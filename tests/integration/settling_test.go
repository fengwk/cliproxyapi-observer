//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every non-idle snapshot must cause another poll, not a successful return.
// Channel gates on that next poll prove this deterministically for missing
// usage, active completion and the per-Key slot independently.
func TestGracefulRestartWaitsForLedgerAndCallbacks(t *testing.T) {
	states := []struct{ ledger, active, keyActive int }{
		{ledger: 3},
		{ledger: 4, active: 1},
		{ledger: 4, keyActive: 1},
		{ledger: 4},
	}
	type gate struct {
		entered, release chan struct{}
		once             sync.Once
	}
	gates := make([]*gate, len(states)-1)
	for i := range gates {
		gates[i] = &gate{entered: make(chan struct{}), release: make(chan struct{})}
	}
	var polls, phase atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v0/management/plugins/" + pluginID + "/requests":
			index := int(polls.Add(1) - 1)
			if index >= len(states) {
				t.Error("unexpected extra settling poll")
				w.WriteHeader(500)
				return
			}
			if index > 0 {
				g := gates[index-1]
				close(g.entered)
				select {
				case <-g.release:
				case <-r.Context().Done():
					return
				}
			}
			phase.Store(int64(index))
			_ = json.NewEncoder(w).Encode(requestPage{Items: make([]recordView, states[index].ledger)})
		case "/v0/management/plugins/" + pluginID + "/health":
			s := states[phase.Load()]
			_ = json.NewEncoder(w).Encode(acceptanceHealth{ActiveRequests: s.active})
		case "/v0/management/plugins/" + pluginID + "/keys":
			s := states[phase.Load()]
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []keyView{{ID: "fixture", Active: s.keyActive}}})
		default:
			t.Errorf("unexpected settling request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	// Release any blocked handler before httptest.Close during failed assertions.
	t.Cleanup(func() {
		for _, g := range gates {
			g.once.Do(func() { close(g.release) })
		}
	})
	h := &harness{baseURL: server.URL}
	result := make(chan []recordView, 1)
	go func() { result <- h.awaitGracefulRestartReady(t, 4) }()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for i, g := range gates {
		select {
		case <-result:
			t.Fatalf("settling returned early at non-idle state %d: %+v", i, states[i])
		case <-g.entered:
			g.once.Do(func() { close(g.release) })
		case <-deadline.C:
			t.Fatalf("settling did not advance past state %d", i)
		}
	}
	select {
	case records := <-result:
		if len(records) != 4 {
			t.Fatalf("settled ledger count = %d, want 4", len(records))
		}
	case <-deadline.C:
		t.Fatal("settling did not finish after ledger/completion/billing became idle")
	}
}
