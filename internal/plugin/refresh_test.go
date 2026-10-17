package plugin

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Synchronize inside the HTTP transport rather than sleeping: lifecycle must
// not acquire the write fence while refresh can still modify its cache file.
func TestRefreshHoldsLifecycleFence(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}
	service, err := pricing.NewService(pricing.DefaultConfig(), filepath.Join(t.TempDir(), "catalog.json"), client)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil)
	m.prices = service
	go func() { defer close(done); m.servePriceRefresh() }()
	<-started
	acquired := m.mu.TryLock()
	if acquired {
		m.mu.Unlock()
	}
	close(release)
	<-done
	if acquired {
		t.Fatal("lifecycle could close while refresh was writing")
	}
	if !m.mu.TryLock() {
		t.Fatal("refresh leaked lifecycle fence")
	}
	m.mu.Unlock()
}
