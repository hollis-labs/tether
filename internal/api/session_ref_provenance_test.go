package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestSessionRefSourceUsesVerifiedPrincipal(t *testing.T) {
	for _, tc := range []struct {
		name      string
		principal identity.Principal
		want      string
	}{
		{"anonymous", identity.Principal{}, store.SourceAPI},
		{"unverified session", identity.Principal{Kind: "session", SessionID: "s"}, store.SourceAPI},
		{"unverified operator", identity.Principal{Kind: "operator", ID: identity.OperatorID}, store.SourceAPI},
		{"verified session", identity.Principal{Verified: true, Kind: "session", SessionID: "s"}, store.SourceSession},
		{"bound agent", identity.Principal{Verified: true, Kind: "agent", SessionID: "s"}, store.SourceSession},
		{"operator", identity.Principal{Verified: true, Kind: "operator", ID: identity.OperatorID}, store.SourceOperator},
		{"operator minted narrow token", identity.Principal{Verified: true, Kind: "operator", ID: identity.OperatorID, TokenID: "issued"}, store.SourceAPI},
		{"device", identity.Principal{Verified: true, Kind: "device"}, store.SourceAPI},
		{"service named proxy", identity.Principal{Verified: true, Kind: "service", ID: "proxy", Scopes: []string{"*", "tether.refs.proxy"}}, store.SourceAPI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, source := range []string{"", "agent", "operator", "session", "proxy_claimed", "vibes", "proxy"} {
				rows := &recordingRefStore{}
				h := NewHandler(Deps{Service: &fakeLaunchService{}, SessionRefs: rows})
				body := fmt.Sprintf(`{"kind":"torque_task","ref_id":"CW-1","source":%q}`, source)
				req := httptest.NewRequest(http.MethodPost, "/sessions/s/refs", strings.NewReader(body))
				req = req.WithContext(identity.WithPrincipal(req.Context(), tc.principal))
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if source == "proxy" {
					if w.Code != 403 || len(rows.got) != 0 || !strings.Contains(w.Body.String(), `"code":"source_asserted"`) {
						t.Fatalf("spoof status=%d body=%s rows=%v", w.Code, w.Body.String(), rows.got)
					}
					continue
				}
				if w.Code != 200 || len(rows.got) != 1 || rows.got[0].Source != tc.want {
					t.Fatalf("asserted=%q status=%d rows=%v want=%s", source, w.Code, rows.got, tc.want)
				}
			}
		})
	}
}

func TestSessionRefSourceAssertionCounter(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	counter, err := provider.Meter("test").Int64Counter("tether.refs.source_asserted")
	if err != nil {
		t.Fatal(err)
	}
	rows := &recordingRefStore{}
	s := &Server{SessionRefs: rows, refSourceAssertions: counter}
	for _, source := range []string{"proxy", "proxy", "agent"} {
		r := httptest.NewRequest(http.MethodPost, "/sessions/s/refs", strings.NewReader(fmt.Sprintf(`{"kind":"torque_task","ref_id":"CW-1","source":%q}`, source)))
		s.handleAttachSessionRef(httptest.NewRecorder(), r, "s")
	}
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	var rejected int64
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != "tether.refs.source_asserted" {
				continue
			}
			for _, point := range metric.Data.(metricdata.Sum[int64]).DataPoints {
				if point.Attributes.Len() != 0 {
					t.Fatal("request data became metric labels")
				}
				rejected += point.Value
			}
		}
	}
	if rejected != 2 || len(rows.got) != 1 {
		t.Fatalf("rejected=%d writes=%d", rejected, len(rows.got))
	}
}
