package plugin

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-observer/internal/observer"
)

// fakeStore is a faithful in-memory stand-in for *observer.Store. It records
// every call so tests can assert what the plugin enqueued, and it can inject
// failures to exercise sanitized error paths.
type fakeStore struct {
	mu       sync.Mutex
	usage    []pluginapi.UsageRecord
	captures []pluginapi.RequestInterceptRequest
	queries  []observer.Query
	closed   bool

	submitOK  bool
	captureOK bool

	// submitEntered/submitGate let a test hold SubmitUsage open to prove the
	// manager never blocks a lifecycle transition on an in-flight observation.
	submitEntered chan struct{}
	submitGate    chan struct{}
	submitOnce    sync.Once

	summary     observer.Summary
	summaryErr  error
	page        observer.RequestPage
	requestsErr error
	body        observer.BodyDetail
	bodyErr     error
	status      observer.Status
}

func newFakeStore() *fakeStore {
	return &fakeStore{submitOK: true, captureOK: true}
}

func (s *fakeStore) SubmitUsage(record pluginapi.UsageRecord) bool {
	if s.submitEntered != nil {
		s.submitOnce.Do(func() { close(s.submitEntered) })
	}
	if s.submitGate != nil {
		<-s.submitGate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage = append(s.usage, record)
	return s.submitOK
}

func (s *fakeStore) Capture(req pluginapi.RequestInterceptRequest) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captures = append(s.captures, req)
	return s.captureOK
}

func (s *fakeStore) Flush(context.Context) error { return nil }

func (s *fakeStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *fakeStore) Requests(query observer.Query) (observer.RequestPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries = append(s.queries, query)
	if s.closed {
		return observer.RequestPage{}, observer.ErrClosed
	}
	return s.page, s.requestsErr
}

func (s *fakeStore) Summary(query observer.Query) (observer.Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries = append(s.queries, query)
	if s.closed {
		return observer.Summary{}, observer.ErrClosed
	}
	return s.summary, s.summaryErr
}

func (s *fakeStore) Body(string) (observer.BodyDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return observer.BodyDetail{}, observer.ErrClosed
	}
	return s.body, s.bodyErr
}

func (s *fakeStore) Status() observer.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *fakeStore) usageCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.usage)
}

func (s *fakeStore) captureCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.captures)
}

func (s *fakeStore) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *fakeStore) lastQuery() (observer.Query, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queries) == 0 {
		return observer.Query{}, false
	}
	return s.queries[len(s.queries)-1], true
}

// fakeOpener parses raw config into a fixed Config whose DataPath is the raw
// payload, and opens a new fakeStore unless openErrFor says otherwise. This
// lets a test distinguish the live configuration from a failing candidate.
type fakeOpener struct {
	cfg        observer.Config
	parseErr   error
	openErrFor func(cfg observer.Config) error

	mu     sync.Mutex
	stores []*fakeStore
}

func (o *fakeOpener) Parse(raw []byte) (observer.Config, error) {
	if o.parseErr != nil {
		return observer.Config{}, o.parseErr
	}
	cfg := o.cfg
	cfg.DataPath = strings.TrimSpace(string(raw))
	return cfg, nil
}

func (o *fakeOpener) Open(cfg observer.Config) (Store, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.openErrFor != nil {
		if err := o.openErrFor(cfg); err != nil {
			return nil, err
		}
	}
	store := newFakeStore()
	o.stores = append(o.stores, store)
	return store, nil
}

func (o *fakeOpener) opened() []*fakeStore {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]*fakeStore, len(o.stores))
	copy(out, o.stores)
	return out
}

func (o *fakeOpener) last() *fakeStore {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.stores) == 0 {
		return nil
	}
	return o.stores[len(o.stores)-1]
}

// errStoreFailure is an internal error used to prove messages are sanitized.
var errStoreFailure = errors.New("open /secret/path/data.db: permission denied")
