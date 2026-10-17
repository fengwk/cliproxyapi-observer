package pricing

import (
	"context"
	"fmt"
	"math"
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

func ptr(v int64) *int64 { return &v }
func service(t *testing.T, c Config) *Service {
	t.Helper()
	s, err := NewService(c, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func quote(t *testing.T, s *Service, model, provider string, tokens Tokens, at time.Time) Quote {
	t.Helper()
	q, err := s.Quote(model, provider, tokens, at)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// Large products and overlapping reasoning must not overflow intermediates or
// double-charge; rounding is once after the entire execution.
func TestMath(t *testing.T) {
	s := service(t, Config{Prices: []Price{{Model: "m", Rates: Rates{Input: 1, Output: 1, CacheRead: 1, CacheWrite: 1}}}})
	q := quote(t, s, "m", "", Tokens{Input: 1, Output: 1, CacheRead: 1, CacheWrite: 1, Reasoning: 1}, time.Now())
	if q.CostMicroUSD != 1 {
		t.Fatal(q)
	}
	c := Config{Prices: []Price{{Model: "m", Rates: Rates{Input: maxSafe, Output: 2, Reasoning: ptr(3)}}}}
	if err := s.Configure(c); err != nil {
		t.Fatal(err)
	}
	q = quote(t, s, "m", "", Tokens{Input: 1000000, Output: 10, Reasoning: 4}, time.Now())
	if q.CostMicroUSD != maxSafe+1 {
		t.Fatal(q)
	}
	for _, tokens := range []Tokens{{Input: -1}, {Reasoning: 1}, {Input: math.MaxInt64}} {
		if _, err := s.Quote("m", "", tokens, time.Now()); err == nil {
			t.Fatal("expected invalid/overflow", tokens)
		}
	}
	q = quote(t, s, "missing", "", Tokens{Input: math.MaxInt64}, time.Now())
	if q.Source != "unpriced" || q.CostMicroUSD != 0 {
		t.Fatal(q)
	}
}

// Rules use UTC, strict greater-than and half-open intervals, including wraps.
func TestRulesAndValidation(t *testing.T) {
	c := Config{Auto: true, Prices: []Price{{Model: "m", Rates: Rates{Input: 9}}}, Rules: []Rule{
		{ID: "night", Model: "m", InputTokensGT: ptr(10), TimeRange: "23:00-01:00", Rates: Rates{Input: 2}},
		{ID: "day", Model: "m", TimeRange: "01:00-23:00", Rates: Rates{Input: 3}},
	}}
	s := service(t, c)
	for _, tc := range []struct {
		at         string
		input      int64
		source, id string
	}{
		{"2026-01-01T23:00:00Z", 11, "rule", "night"},
		{"2026-01-01T00:59:59Z", 11, "rule", "night"},
		{"2026-01-01T01:00:00Z", 11, "rule", "day"},
		{"2026-01-01T23:00:00Z", 10, "manual", ""},
		{"2026-01-01T08:00:00+09:00", 11, "rule", "night"},
	} {
		at, _ := time.Parse(time.RFC3339, tc.at)
		q := quote(t, s, "m", "", Tokens{Input: tc.input}, at)
		if q.Source != tc.source || q.RuleID != tc.id {
			t.Fatal(tc, q)
		}
	}
	for _, v := range []string{"1:00-02:00", "24:00-01:00", "00:60-01:00", "01:00-01:00", "01:00/02:00"} {
		bad := cloneConfig(c)
		bad.Rules[0].TimeRange = v
		if Validate(bad) == nil {
			t.Fatal(v)
		}
		if s.Configure(bad) == nil {
			t.Fatal(v)
		}
	}
	for _, v := range []int64{-1, maxSafe + 1} {
		if Validate(Config{Prices: []Price{{Model: "m", Rates: Rates{Input: v}}}}) == nil {
			t.Fatal(v)
		}
	}
	if Validate(Config{Prices: []Price{{Model: "m"}, {Model: "m"}}}) == nil {
		t.Fatal("duplicate")
	}
	if quote(t, s, "m", "", Tokens{}, time.Now()).Source == "unpriced" {
		t.Fatal("failed configure changed state")
	}
}

// Every outward and inward pointer/slice must be detached from caller mutation.
func TestDeepCopyAndZero(t *testing.T) {
	c := Config{Auto: true, Prices: []Price{{Model: "m", Rates: Rates{Reasoning: ptr(0)}}}, Rules: []Rule{{ID: "r", Model: "other", InputTokensGT: ptr(1)}}}
	s := service(t, c)
	s.entries = []entry{{Model: "m", Provider: "p", Rates: Rates{Input: 100}}}
	*c.Prices[0].Rates.Reasoning = 99
	c.Prices[0].Model = "oops"
	*c.Rules[0].InputTokensGT = 99
	q := quote(t, s, "m", "p", Tokens{Input: 1000000}, time.Now())
	if q.CostMicroUSD != 0 || q.Source != "manual" || *q.Rates.Reasoning != 0 {
		t.Fatal(q)
	}
	*q.Rates.Reasoning = 44
	snap := s.Snapshot()
	*snap.Prices[0].Rates.Reasoning = 33
	if *quote(t, s, "m", "p", Tokens{}, time.Now()).Rates.Reasoning != 0 || *s.config.Rules[0].InputTokensGT != 1 {
		t.Fatal("alias")
	}
}

const catalogFixture = `{"openai":{"id":"openai","models":{"m":{"id":"m","cost":{"input":1.25,"output":2,"cache_read":0.1,"cache_write":0.2}},"dated-2026":{"cost":{"input":5}},"free":{"cost":{"input":0,"output":0}}}},"other":{"models":{"m":{"cost":{"input":4,"output":2}}}}}`

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Refresh evaluates Done only after observing the active flight. This handshake
// makes deduplication tests independent of goroutine scheduler timing.
type waitingContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *waitingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}
func mockClient(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	base := server.Client().Transport
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != catalogURL {
			t.Errorf("non-catalog request %s", r.URL)
		}
		c := r.Clone(r.Context())
		u := *r.URL
		c.URL = &u
		c.URL.Scheme = "https"
		c.URL.Host = strings.TrimPrefix(server.URL, "https://")
		return base.RoundTrip(c)
	})}
}

// Exercise the real nested-provider JSON, provider ambiguity and conservative
// normalization without letting the client ever contact the public network.
func TestCatalogAndNoNetworkQuote(t *testing.T) {
	var calls atomic.Int64
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, catalogFixture) })
	s, err := NewService(DefaultConfig(), "", client)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		s.Observe("m", "openai")
		quote(t, s, "m", "openai", Tokens{}, time.Now())
	}
	if calls.Load() != 0 {
		t.Fatal("quote/observe performed IO")
	}
	if err = s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		model, provider, source string
		rate                    int64
	}{
		{"m", "openai", "auto", 1250000}, {"m", "other", "auto", 4000000},
		{"m", "", "unpriced", 0}, {"openai/m(high)", "openai", "auto", 1250000},
		{"dated", "", "unpriced", 0}, {"free", "", "auto", 0},
	} {
		q := quote(t, s, tc.model, tc.provider, Tokens{}, time.Now())
		if q.Source != tc.source || q.Rates.Input != tc.rate {
			t.Fatal(tc, q)
		}
	}
	s.Observe("missing", "")
	snap := s.Snapshot()
	found := false
	for _, p := range snap.Prices {
		if p.Model == "missing" && p.Source == "unpriced" {
			found = true
		}
	}
	if !found {
		t.Fatal(snap)
	}
	if err := s.Configure(Config{}); err != nil {
		t.Fatal(err)
	}
	if quote(t, s, "m", "openai", Tokens{}, time.Now()).Source != "unpriced" {
		t.Fatal("auto off")
	}
}

// A bad response never replaces last good data; only public catalog entries
// reach the atomic disk cache (not observed model names or manual prices).
func TestPersistenceAndFailure(t *testing.T) {
	var fail atomic.Bool
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "sensitive response", 500)
			return
		}
		fmt.Fprint(w, catalogFixture)
	})
	path := filepath.Join(t.TempDir(), "cache.json")
	s, err := NewService(DefaultConfig(), path, client)
	if err != nil {
		t.Fatal(err)
	}
	s.Observe("private-model", "private-provider")
	if err = s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "private") {
		t.Fatal("observation persisted")
	}
	reopened, err := NewService(DefaultConfig(), path, client)
	if err != nil {
		t.Fatal(err)
	}
	if quote(t, reopened, "m", "openai", Tokens{}, time.Now()).Source != "auto" {
		t.Fatal("cache not loaded")
	}
	last := s.Snapshot().LastSync
	fail.Store(true)
	if err = s.Refresh(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	if s.Snapshot().LastSync != last || strings.Contains(s.Snapshot().Error, "sensitive") {
		t.Fatal(s.Snapshot())
	}
	if quote(t, s, "m", "openai", Tokens{}, time.Now()).Source != "auto" {
		t.Fatal("lastgood lost")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(body) {
		t.Fatal("failed refresh replaced disk cache")
	}
	if err = os.WriteFile(path, []byte(`{"entries":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	bad, _ := NewService(DefaultConfig(), path, client)
	if bad.Snapshot().Error == "" || len(bad.entries) != 0 {
		t.Fatal("corrupt cache accepted")
	}
	// A non-directory parent deterministically fails even under root.
	block := filepath.Join(t.TempDir(), "file")
	os.WriteFile(block, []byte("x"), 0600)
	fail.Store(false)
	unwritable, _ := NewService(DefaultConfig(), filepath.Join(block, "cache"), client)
	if unwritable.Refresh(context.Background()) == nil || unwritable.Snapshot().Error == "" {
		t.Fatal("write error hidden")
	}
}

// Non-success JSON, oversized reads, and hostile redirects all fail safely.
func TestBoundedCatalog(t *testing.T) {
	for _, body := range []string{`{}`, `{"p":{"models":{"m":{"cost":{"input":-1}}}}}`, strings.Repeat(" ", maxCatalogBytes+1)} {
		client := mockClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		s, _ := NewService(DefaultConfig(), "", client)
		if s.Refresh(context.Background()) == nil {
			t.Fatal("bad catalog accepted")
		}
	}
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "https://evil.invalid/catalog", 302) })
	s, _ := NewService(DefaultConfig(), "", client)
	if s.Refresh(context.Background()) == nil {
		t.Fatal("redirect escaped")
	}
}

// Concurrent refresh callers share a single request; canceled waiters do not
// cancel the leader and Run cancellation joins its owned network work.
func TestRefreshDedupAndCancel(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	client := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			fmt.Fprint(w, catalogFixture)
		case <-r.Context().Done():
		}
	})
	s, _ := NewService(DefaultConfig(), "", client)
	done := make(chan error, 1)
	go func() { done <- s.Refresh(context.Background()) }()
	<-started
	waiterCtx := &waitingContext{Context: context.Background(), waiting: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() { waiter <- s.Refresh(waiterCtx) }()
	<-waiterCtx.waiting
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s.Refresh(ctx) == nil {
		t.Fatal("canceled waiter")
	}
	if !s.Snapshot().Refreshing {
		t.Fatal("missing refreshing")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-waiter; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	// New service's worker has no external goroutines to abandon.
	started2 := make(chan struct{})
	c2 := mockClient(t, func(w http.ResponseWriter, r *http.Request) { close(started2); <-r.Context().Done() })
	s2, _ := NewService(DefaultConfig(), "", c2)
	ctx, cancel = context.WithCancel(context.Background())
	worker := make(chan struct{})
	go func() { s2.Run(ctx); close(worker) }()
	<-started2
	cancel()
	select {
	case <-worker:
	case <-time.After(2 * time.Second):
		t.Fatal("worker leaked")
	}
}

// Bound both memory and queued work and exercise public operations under race.
func TestObservationBoundsAndRaces(t *testing.T) {
	s := service(t, DefaultConfig())
	for i := 0; i < maxObserved+50; i++ {
		s.Observe(fmt.Sprint(i), "p")
	}
	if len(s.observed) != maxObserved || len(s.queue) != 1 {
		t.Fatal(len(s.observed), len(s.queue))
	}
	<-s.queue
	for i := 0; i < maxObserved; i++ {
		s.Observe(fmt.Sprint(i), "p")
	}
	if len(s.queue) != 0 {
		t.Fatal("negative lookup storm")
	}
	var wg sync.WaitGroup
	for j := 0; j < 8; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				s.Configure(DefaultConfig())
				s.Observe("m", "p")
				s.Snapshot()
				s.Quote("m", "p", Tokens{}, time.Now())
			}
		}()
	}
	wg.Wait()
}

// Disabled auto mode consumes queued work without making network requests.
func TestAutoOffWorker(t *testing.T) {
	var calls atomic.Int64
	c := mockClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	s, _ := NewService(Config{}, "", c)
	s.Observe("m", "p")
	if len(s.queue) != 0 {
		t.Fatal("auto off queued miss")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Run(ctx)
	if calls.Load() != 0 {
		t.Fatal("auto off fetched")
	}
}
