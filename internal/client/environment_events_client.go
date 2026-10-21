package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/hollis-labs/tether/internal/environment"
)

// EnvironmentStreamQuery selects events from GET /environment/events. The
// filter fields mirror environmentstream.Filter and combine the same way (AND
// across fields, OR within one).
type EnvironmentStreamQuery struct {
	// AfterSeq is the resume cursor; the server requires it.
	AfterSeq    int64
	Kinds       []string
	Categories  []string
	Subjects    []string
	Apps        []string
	SeverityMin string
	// Scope maps a scope dimension (task_id, project_id, ...) to wanted values.
	Scope map[string][]string
}

// EnvironmentFrame is one SSE frame of the environment stream. Kind is the
// event kind, or one of the control frames "synchronized" and "gap". Data is the
// frame's JSON body: the complete mesh envelope for an event.
type EnvironmentFrame struct {
	ID   int64
	Kind string
	Data json.RawMessage
}

// Values encodes the query as the server's filter parameters.
func (q EnvironmentStreamQuery) Values() url.Values {
	v := url.Values{}
	v.Set("after_seq", strconv.FormatInt(q.AfterSeq, 10))
	add := func(key string, vals []string) {
		for _, s := range vals {
			if strings.TrimSpace(s) != "" {
				v.Add(key, strings.TrimSpace(s))
			}
		}
	}
	add("kind", q.Kinds)
	add("category", q.Categories)
	add("subject", q.Subjects)
	add("app", q.Apps)
	if q.SeverityMin != "" {
		v.Set("severity_min", q.SeverityMin)
	}
	for dim, vals := range q.Scope {
		add("scope."+dim, vals)
	}
	return v
}

// StreamEnvironmentEvents opens GET /environment/events. The channels close when
// the stream ends; a read error is sent on the error channel first.
func (c *Client) StreamEnvironmentEvents(ctx context.Context, q EnvironmentStreamQuery) (<-chan EnvironmentFrame, <-chan error, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/environment/events?"+q.Values().Encode(), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set(environment.ProtocolHeader, strconv.Itoa(environment.Protocol))
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, wrapIfUnreachable(err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, nil, readError(resp)
	}
	frames := make(chan EnvironmentFrame, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(frames)
		defer close(errCh)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var f EnvironmentFrame
		var data strings.Builder
		flush := func() bool {
			if f.Kind == "" && data.Len() == 0 {
				return true
			}
			f.Data = json.RawMessage(data.String())
			select {
			case frames <- f:
			case <-ctx.Done():
				return false
			}
			f, data = EnvironmentFrame{}, strings.Builder{}
			return true
		}
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				if !flush() {
					return
				}
			case strings.HasPrefix(line, ":"):
			case strings.HasPrefix(line, "id:"):
				if n, perr := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "id:")), 10, 64); perr == nil {
					f.ID = n
				}
			case strings.HasPrefix(line, "event:"):
				f.Kind = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			errCh <- fmt.Errorf("read environment stream: %w", err)
		}
	}()
	return frames, errCh, nil
}
