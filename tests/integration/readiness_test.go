//go:build integration

package integration

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A healthy inference server is not enough: startup must wait for the observer,
// while persistent unavailability still fails within the original deadline.
func TestAwaitReadyRequiresObserver(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		name := "transient"
		if permanent {
			name = "permanent"
		}
		t.Run(name, func(t *testing.T) {
			var probes atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/healthz", "/v1/models":
					w.WriteHeader(http.StatusOK)
				case "/v0/management/plugins/" + pluginID + "/health":
					if r.Header.Get("Authorization") != "Bearer "+mgmtKey {
						t.Error("observer readiness did not use management authentication")
					}
					if probes.Add(1) < 3 || permanent {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					w.WriteHeader(http.StatusOK)
				default:
					t.Errorf("unexpected readiness URL %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			h := &harness{baseURL: server.URL, done: make(chan struct{})}
			err := h.awaitReady(250 * time.Millisecond)
			if permanent {
				if err == nil || !strings.Contains(err.Error(), "observer did not become ready") {
					t.Fatalf("persistent unavailable observer must fail readiness: %v", err)
				}
			} else if err != nil || probes.Load() != 3 {
				t.Fatalf("transient readiness err=%v probes=%d, want success after 3 probes", err, probes.Load())
			}
		})
	}
}
