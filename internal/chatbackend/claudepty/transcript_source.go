package claudepty

import (
	"context"
	"time"

	"github.com/hollis-labs/tether/internal/chatbackend/claudetranscript"
	"github.com/hollis-labs/tether/internal/chatbackend/jsonltail"
)

// SessionTranscript follows the transcript of whatever Claude session id sessionID reports (Backend.ClaudeSessionID),
// under projectsDir (claudetranscript.ProjectsDir). It waits until the id is known and the file exists, and switches
// files when the id changes (/clear starts a new transcript).
//
// A transcript that already exists when it is first found is followed from its end, so attaching to a long-running
// session does not replay its history as runs. A transcript that appears later, or one switched to, is read from the
// start.
func SessionTranscript(projectsDir string, sessionID func() string) LineSource {
	return &sessionTranscript{dir: projectsDir, sessionID: sessionID, poll: jsonltail.DefaultPoll}
}

type sessionTranscript struct {
	dir       string
	sessionID func() string
	poll      time.Duration
}

func (s *sessionTranscript) Run(ctx context.Context, emit func([]byte)) error {
	first := true
	for {
		id := s.sessionID()
		path := ""
		if id != "" {
			if p, err := claudetranscript.Locate(s.dir, id); err == nil {
				path = p
			}
		}
		if path == "" {
			first = false // whatever appears from here on is new
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(s.poll):
			}
			continue
		}
		tctx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		t := &jsonltail.Tail{Path: path, Poll: s.poll, FromEnd: first}
		go func() { done <- t.Run(tctx, emit) }()
		first = false
	watch:
		for {
			select {
			case <-ctx.Done():
				cancel()
				<-done
				return ctx.Err()
			case err := <-done:
				cancel()
				return err
			case <-time.After(s.poll):
				if s.sessionID() != id {
					cancel()
					<-done
					break watch
				}
			}
		}
	}
}
