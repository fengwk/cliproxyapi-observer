package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mockClient(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	base := server.Client().Transport
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		c := r.Clone(r.Context())
		u := *r.URL
		c.URL = &u
		c.URL.Scheme = "https"
		c.URL.Host = strings.TrimPrefix(server.URL, "https://")
		return base.RoundTrip(c)
	})}
}

func v2Body(model string, input int) string {
	return fmt.Sprintf(`{"p":{"models":{%q:{"id":%q,"canonical_id":"p/%s","cost":{"text":{"input":{"price":%d},"output":{"price":2}}}}}}}`, model, model, model, input)
}

func v1Body(model string, input int) string {
	return fmt.Sprintf(`{"p":{"models":{%q:{"id":%q,"cost":{"input":%d,"output":2}}}}}`, model, model, input)
}

// A source switch immediately changes the selected state. A late refresh for
// the old source must be discarded and must never fill the new source's gap.
func TestSourceSwitchDiscardsLateResponse(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/experimental/v2.0/api.json" {
			started <- struct{}{}
			<-release
			fmt.Fprint(w, v2Body("only-v2", 7))
			return
		}
		fmt.Fprint(w, v1Body("only-v1", 3))
	})
	s, err := NewService(DefaultConfig(), filepath.Join(t.TempDir(), "cache"), client)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- s.Refresh(context.Background()) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("v2 refresh did not start")
	}
	v1 := DefaultConfig()
	v1.Source = SourceV1
	if err = s.Configure(v1); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err = <-result; err == nil {
		t.Fatal("stale refresh reported success")
	}
	snap := s.Snapshot()
	if snap.Source != SourceV1 || snap.CatalogEntries != 0 || !snap.LastSync.IsZero() {
		t.Fatalf("late source result leaked: %+v", snap)
	}
	if q := quote(t, s, "only-v2", "p", Tokens{Input: 1}, time.Now()); q.Source != "zero" {
		t.Fatal("v1 selection used v2 data", q)
	}
	if err = s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if q := quote(t, s, "only-v1", "p", Tokens{Input: 1}, time.Now()); q.CostMicroUSD != 3 {
		t.Fatal(q)
	}
	if q := quote(t, s, "only-v2", "p", Tokens{Input: 1}, time.Now()); q.Source != "zero" {
		t.Fatal("v1 selection borrowed v2 price", q)
	}
	// The discarded late v2 result is not available when switching back.
	if err = s.Configure(DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	if snap = s.Snapshot(); snap.Source != SourceV2 || snap.CatalogEntries != 0 {
		t.Fatal("stale result was retained", snap)
	}
}

// A failed refresh keeps the previous successful catalog and its real
// fetchedAt. Only a later success advances the snapshot.
func TestRefreshFailureKeepsOldSnapshot(t *testing.T) {
	var mode atomic.Int32
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 0:
			fmt.Fprint(w, v2Body("m", 2))
		case 1:
			http.Error(w, "no", http.StatusInternalServerError)
		default:
			fmt.Fprint(w, v2Body("m", 3))
		}
	})
	s, err := NewService(DefaultConfig(), "", client)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := s.Snapshot()
	mode.Store(1)
	if err = s.Refresh(context.Background()); err == nil {
		t.Fatal("failure hidden")
	}
	failed := s.Snapshot()
	if failed.Error == "" || !failed.LastSync.Equal(first.LastSync) || failed.CatalogEntries != first.CatalogEntries {
		t.Fatal("failure replaced old snapshot", failed)
	}
	if q := quote(t, s, "m", "p", Tokens{Input: 1}, time.Now()); q.CostMicroUSD != 2 {
		t.Fatal("old snapshot unavailable", q)
	}
	mode.Store(2)
	if err = s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	final := s.Snapshot()
	if final.Error != "" || final.LastSync.Before(first.LastSync) || final.CatalogEntries != 1 {
		t.Fatal(final)
	}
	if q := quote(t, s, "m", "p", Tokens{Input: 1}, time.Now()); q.CostMicroUSD != 3 {
		t.Fatal("new snapshot not active", q)
	}
}

// A disk cache write failure is reported but the successfully fetched
// in-memory snapshot remains usable; a later success clears the error.
func TestCacheWriteFailureKeepsFetchedSnapshot(t *testing.T) {
	var input atomic.Int32
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, v2Body("m", int(input.Add(1))))
	})
	base := filepath.Join(t.TempDir(), "cache")
	cacheFile := base + "." + SourceV2
	if err := os.Mkdir(cacheFile, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := NewService(DefaultConfig(), base, client)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Refresh(context.Background()); err == nil || s.Snapshot().Error != "catalog cache write failed" {
		t.Fatalf("cache failure not reported: %v %+v", err, s.Snapshot())
	}
	snap := s.Snapshot()
	if snap.LastSync.IsZero() || snap.CatalogEntries != 1 {
		t.Fatal("fetched snapshot not published", snap)
	}
	if q := quote(t, s, "m", "p", Tokens{Input: 1}, time.Now()); q.CostMicroUSD != 1 {
		t.Fatal("fetched snapshot unusable", q)
	}
	if err = os.Remove(cacheFile); err != nil {
		t.Fatal(err)
	}
	if err = s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snap = s.Snapshot(); snap.Error != "" || snap.CatalogEntries != 1 {
		t.Fatal("successful retry did not clear error", snap)
	}
	if q := quote(t, s, "m", "p", Tokens{Input: 1}, time.Now()); q.CostMicroUSD != 2 {
		t.Fatal("successful retry did not publish", q)
	}
}

// Concurrent refreshes for the same source/generation share one network call.
func TestRefreshSingleFlightPerSource(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		fmt.Fprint(w, v2Body("m", 1))
	})
	s, err := NewService(DefaultConfig(), "", client)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	go func() { results <- s.Refresh(context.Background()) }()
	<-started
	go func() { results <- s.Refresh(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("concurrent refresh not coalesced: %d", calls.Load())
	}
	close(release)
	for range 2 {
		if err = <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("second request issued")
	}
}

// Non-JSON bodies and a cache belonging to another source are rejected rather
// than being partially interpreted or mixed.
func TestHTMLAndWrongSourceCacheRejected(t *testing.T) {
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<!doctype html><html>not a catalog</html>")
	})
	s, err := NewService(DefaultConfig(), "", client)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Refresh(context.Background()); err == nil || s.Snapshot().Error == "" {
		t.Fatal("HTML accepted as catalog")
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "catalog")
	body, err := json.Marshal(diskCache{Version: diskCacheVersion, Source: SourceV1, LastSync: time.Now(),
		Entries: []entry{{Model: "m", Provider: "p", Supported: true, Definition: definition(RateSet{Input: 1}, nil, nil)}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(base+"."+SourceV2, body, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := NewService(DefaultConfig(), base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snap := loaded.Snapshot(); snap.Error != "catalog cache invalid" || snap.CatalogEntries != 0 {
		t.Fatal("cross-source cache accepted", snap)
	}
}

// The selected source state is further isolated from a prior failure flag.
func TestSourceErrorDoesNotLeak(t *testing.T) {
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/experimental/v2.0/api.json" {
			http.Error(w, "fail", 500)
			return
		}
		fmt.Fprint(w, v1Body("m", 1))
	})
	s, err := NewService(DefaultConfig(), "", client)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Refresh(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	v1 := DefaultConfig()
	v1.Source = SourceV1
	if err = s.Configure(v1); err != nil {
		t.Fatal(err)
	}
	if snap := s.Snapshot(); snap.Error != "" {
		t.Fatal("v2 error leaked into v1", snap)
	}
	if err = s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snap := s.Snapshot(); snap.Source != SourceV1 || snap.Error != "" || snap.CatalogEntries != 1 {
		t.Fatal(snap)
	}
}

// Cached snapshots are source-specific and survive a new service instance.
func TestDiskCacheLoadsOnlySelectedSource(t *testing.T) {
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/experimental/v2.0/api.json" {
			fmt.Fprint(w, v2Body("m", 4))
			return
		}
		fmt.Fprint(w, v1Body("m", 5))
	})
	base := filepath.Join(t.TempDir(), "catalog")
	s, err := NewService(DefaultConfig(), base, client)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	loaded, err := NewService(DefaultConfig(), base, client)
	if err != nil {
		t.Fatal(err)
	}
	if q := quote(t, loaded, "m", "p", Tokens{Input: 1}, time.Now()); q.CostMicroUSD != 4 {
		t.Fatal("v2 cache not loaded", q)
	}
	v1 := DefaultConfig()
	v1.Source = SourceV1
	if err = loaded.Configure(v1); err != nil {
		t.Fatal(err)
	}
	if snap := loaded.Snapshot(); snap.CatalogEntries != 0 {
		t.Fatal("v1 selection loaded v2 cache", snap)
	}
}

func TestRefreshReadConfigureRaces(t *testing.T) {
	var body atomic.Value
	body.Store(v2Body("m", 1))
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body.Load().(string)) })
	s, err := NewService(DefaultConfig(), "", client)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for j := 0; j < 6; j++ {
		wg.Add(1)
		go func(j int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				switch j % 3 {
				case 0:
					if err := s.Refresh(context.Background()); err != nil && err != errSourceChanged {
						t.Error(err)
					}
				case 1:
					c := DefaultConfig()
					c.Prices = []ManualPrice{{Model: "manual", Definition: definition(RateSet{Reasoning: ptr(int64(i))}, nil, nil)}}
					if err := s.Configure(c); err != nil {
						t.Error(err)
					}
				case 2:
					s.Observe("m", "p")
					s.Snapshot()
					if _, err := s.Quote("m", "p", Tokens{Input: 10}, time.Now(), "priority"); err != nil {
						t.Error(err)
					}
				}
			}
		}(j)
	}
	wg.Wait()
}
