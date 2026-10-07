// Package observer owns bounded local observation storage and token accounting.
package observer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	bolt "go.etcd.io/bbolt"
)

var (
	bucketRequests   = []byte("requests")
	bucketStats      = []byte("stats")
	bucketBodies     = []byte("bodies")
	bucketBodyMeta   = []byte("body_meta")
	bucketBodyIndex  = []byte("body_index")
	bucketUsageTrace = []byte("usage_trace")
	bucketTraceBody  = []byte("trace_body")
	allBuckets       = [][]byte{
		bucketRequests, bucketStats, bucketBodies, bucketBodyMeta,
		bucketBodyIndex, bucketUsageTrace, bucketTraceBody,
	}
)

// ambiguousTrace marks a TraceID that observed more than one distinct request
// lifecycle capture. Trace-based body resolution fails closed for such traces
// rather than arbitrarily selecting one body.
var ambiguousTrace = []byte("\x00ambiguous")

const (
	usageQueueCapacity  = 4096
	bodyQueueCapacity   = 128
	writeBatchSize      = 100
	defaultPageLimit    = 50
	maxPageLimit        = 1000
	bodyQueueBudgetMin  = 8 << 20
	bodyQueueBudgetCeil = 64 << 20
)

// pendingBody carries a copied request body from Capture to the writer.
type pendingBody struct {
	requestID  string
	traceID    string
	body       []byte
	capturedAt time.Time
}

// Store is the bounded, nonblocking observation store. A single writer
// goroutine owns every mutation; readers use short bbolt view transactions
// guarded against concurrent Close.
type Store struct {
	cfg    Config
	prices map[string]Price

	db   *bolt.DB
	nowV atomic.Value // stores func() time.Time

	usageCh    chan Request
	bodyCh     chan pendingBody
	flushCh    chan chan error
	cleanupCh  chan chan error
	stopCh     chan struct{}
	writerDone chan struct{}

	cleanupPeriod time.Duration
	bodyBudget    int64

	mu     sync.RWMutex
	closed bool

	closedFlag atomic.Bool
	closeOnce  sync.Once
	closeErr   error

	// enqueueMu coordinates producers with Close so no successful Submit or
	// Capture can enqueue after the writer's final drain. It guards no database
	// access and never blocks on I/O, so capture stays off the inference path.
	enqueueMu sync.RWMutex

	// finalErr records the first writer failure, which Close must report.
	finalErr error

	droppedUsage  atomic.Uint64
	droppedBodies atomic.Uint64
	writeErrors   atomic.Uint64

	queuedBodyBytes atomic.Int64
	pendingCount    atomic.Int64
	pendingMu       sync.Mutex
	pendingBodies   map[string]struct{}

	// bodyBytes is owned exclusively by the writer goroutine.
	bodyBytes int64
}

// Open validates the configuration, opens (creating if needed) the bbolt
// database, performs a startup cleanup and starts the bounded async writer.
func Open(config Config) (*Store, error) {
	cfg, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	path, err := resolveDataPath(cfg.DataPath)
	if err != nil {
		return nil, err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create observer data dir: %w", err)
		}
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open observer database: %w", err)
	}

	s := &Store{
		cfg:           cfg,
		prices:        clonePrices(cfg.Prices),
		db:            db,
		usageCh:       make(chan Request, usageQueueCapacity),
		bodyCh:        make(chan pendingBody, bodyQueueCapacity),
		flushCh:       make(chan chan error, 1),
		cleanupCh:     make(chan chan error, 1),
		stopCh:        make(chan struct{}),
		writerDone:    make(chan struct{}),
		cleanupPeriod: defaultCleanupPeriod,
		bodyBudget:    bodyQueueBudget(cfg),
		pendingBodies: make(map[string]struct{}),
	}
	s.nowV.Store(func() time.Time { return time.Now() })

	if err := s.initBuckets(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.loadBodyBytes(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.cleanupTx(s.now()); err != nil {
		_ = db.Close()
		return nil, err
	}
	go s.writerLoop()
	return s, nil
}

func (s *Store) initBuckets() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, name := range allBuckets {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return fmt.Errorf("create bucket %s: %w", name, err)
			}
		}
		return nil
	})
}

func (s *Store) now() time.Time {
	if fn, ok := s.nowV.Load().(func() time.Time); ok && fn != nil {
		return fn()
	}
	return time.Now()
}

// setNow overrides the clock. It is used by tests; the writer reads the clock
// concurrently, so the value is stored atomically.
func (s *Store) setNow(fn func() time.Time) {
	if fn == nil {
		fn = func() time.Time { return time.Now() }
	}
	s.nowV.Store(fn)
}

func bodyQueueBudget(cfg Config) int64 {
	budget := cfg.MaxBodyStorageBytes / 2
	if budget > bodyQueueBudgetCeil {
		budget = bodyQueueBudgetCeil
	}
	if budget < bodyQueueBudgetMin {
		budget = bodyQueueBudgetMin
	}
	if budget < int64(cfg.MaxBodyBytes) {
		budget = int64(cfg.MaxBodyBytes)
	}
	return budget
}

func (s *Store) loadBodyBytes() error {
	var total int64
	err := s.db.View(func(tx *bolt.Tx) error {
		ib := tx.Bucket(bucketBodyIndex)
		if ib == nil {
			return nil
		}
		return ib.ForEach(func(_, v []byte) error {
			size, ok := decodeBodyIndexSize(v)
			if ok && size > 0 {
				total += size
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	s.bodyBytes = total
	return nil
}

// SubmitUsage normalizes and enqueues a usage record without blocking. It
// returns false when the record is malformed, the bounded queue is full, or the
// store is closing.
func (s *Store) SubmitUsage(record pluginapi.UsageRecord) bool {
	if s.closedFlag.Load() {
		s.droppedUsage.Add(1)
		return false
	}
	record.RequestID = strings.TrimSpace(record.RequestID)
	if record.RequestID == "" {
		s.droppedUsage.Add(1)
		return false
	}
	req := NormalizeUsage(record, s.prices)

	s.enqueueMu.RLock()
	defer s.enqueueMu.RUnlock()
	if s.closedFlag.Load() {
		s.droppedUsage.Add(1)
		return false
	}
	select {
	case s.usageCh <- req:
		return true
	default:
		s.droppedUsage.Add(1)
		return false
	}
}

// Capture copies and enqueues the request body for asynchronous storage. It
// never blocks on the database and returns false when the body is ineligible,
// oversized, the bounded queue/budget is exhausted, or the store is closing.
//
// The queue budget is reserved before the body is copied so a burst of
// concurrent captures cannot each copy first and jointly exceed the cap.
func (s *Store) Capture(req pluginapi.RequestInterceptRequest) bool {
	if !s.cfg.CaptureBodies {
		return false
	}
	if s.closedFlag.Load() {
		s.droppedBodies.Add(1)
		return false
	}
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		s.droppedBodies.Add(1)
		return false
	}
	body := req.Body
	if len(body) == 0 {
		return false
	}
	if len(body) > s.cfg.MaxBodyBytes {
		s.droppedBodies.Add(1)
		return false
	}

	size := int64(len(body))
	if s.queuedBodyBytes.Add(size) > s.bodyBudget {
		s.queuedBodyBytes.Add(-size)
		s.droppedBodies.Add(1)
		return false
	}

	s.enqueueMu.RLock()
	defer s.enqueueMu.RUnlock()
	if s.closedFlag.Load() {
		s.queuedBodyBytes.Add(-size)
		s.droppedBodies.Add(1)
		return false
	}

	// Capture once per execution: drop duplicates before copying.
	s.pendingMu.Lock()
	if _, ok := s.pendingBodies[requestID]; ok {
		s.pendingMu.Unlock()
		s.queuedBodyBytes.Add(-size)
		return true
	}
	s.pendingBodies[requestID] = struct{}{}
	s.pendingMu.Unlock()

	cp := make([]byte, len(body))
	copy(cp, body)
	item := pendingBody{requestID: requestID, traceID: req.TraceID, body: cp, capturedAt: s.now()}
	select {
	case s.bodyCh <- item:
		return true
	default:
		s.queuedBodyBytes.Add(-size)
		s.releasePending(requestID)
		s.droppedBodies.Add(1)
		return false
	}
}

func (s *Store) releasePending(requestID string) {
	s.pendingMu.Lock()
	delete(s.pendingBodies, requestID)
	s.pendingMu.Unlock()
}

// Flush drains the queues, persists pending observations and syncs the
// database. It is the test/synchronization barrier.
func (s *Store) Flush(ctx context.Context) error {
	if s.closedFlag.Load() {
		return ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	reply := make(chan error, 1)
	select {
	case s.flushCh <- reply:
	case <-s.stopCh:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		return err
	case <-s.stopCh:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close quiesces the writer, runs a final cleanup and releases the database.
// It is idempotent, protects concurrent readers from the closed database, and
// reports the first writer/database failure instead of claiming success.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		// Flip the closing flag under the enqueue lock so no in-flight
		// Submit/Capture can enqueue after the writer's final drain.
		s.enqueueMu.Lock()
		s.closedFlag.Store(true)
		s.enqueueMu.Unlock()
		close(s.stopCh)
		<-s.writerDone
		s.mu.Lock()
		s.closed = true
		s.closeErr = s.finalErr
		if err := s.db.Close(); err != nil && s.closeErr == nil {
			s.closeErr = err
		}
		s.mu.Unlock()
	})
	return s.closeErr
}

// Status reports bounded-ingestion health.
func (s *Store) Status() Status {
	return Status{
		DroppedUsage:  s.droppedUsage.Load(),
		DroppedBodies: s.droppedBodies.Load(),
		WriteErrors:   s.writeErrors.Load(),
		Queued:        len(s.usageCh) + len(s.bodyCh) + int(s.pendingCount.Load()),
	}
}

// Requests returns a newest-first cursor page. Provider/model filters narrow
// the scanned range without any exact total scan.
func (s *Store) Requests(query Query) (RequestPage, error) {
	now := s.now()
	to := query.To
	if to.IsZero() {
		to = now
	}
	from := query.From
	if from.IsZero() {
		from = to.Add(-defaultRequestRetention)
	}
	if !from.Before(to) {
		return RequestPage{}, ErrInvalidQuery
	}
	if retainedFrom := now.Add(-s.cfg.RequestRetention); from.Before(retainedFrom) {
		from = retainedFrom
	}
	if !from.Before(to) {
		return RequestPage{Items: []Request{}}, nil
	}

	limit := query.Limit
	if limit <= 0 {
		limit = defaultPageLimit
	}
	if limit > maxPageLimit {
		limit = maxPageLimit
	}

	var cursorKey []byte
	if query.Cursor != "" {
		ts, seq, err := decodeCursor(query.Cursor)
		if err != nil {
			return RequestPage{}, err
		}
		cursorKey = encodeRequestKey(ts, seq)
	}

	page := RequestPage{Items: []Request{}}
	err := s.view(func(tx *bolt.Tx) error {
		rb := tx.Bucket(bucketRequests)
		if rb == nil {
			return nil
		}
		c := rb.Cursor()
		key, value := seekReverse(c, cursorKey, to)
		filtered := query.Provider != "" || query.Model != ""
		for ; key != nil; key, value = c.Prev() {
			ts, ok := requestKeyTime(key)
			if !ok {
				continue
			}
			if !ts.Before(to) {
				continue
			}
			if ts.Before(from) {
				break
			}
			if !filtered && len(page.Items) >= limit {
				page.HasMore = true
				break
			}
			var r Request
			if err := json.Unmarshal(value, &r); err != nil {
				continue
			}
			if !matchesQuery(r, query) {
				continue
			}
			if len(page.Items) >= limit {
				page.HasMore = true
				break
			}
			page.Items = append(page.Items, r)
		}
		s.attachBodyAvailability(tx, page.Items, now)
		return nil
	})
	if err != nil {
		return RequestPage{}, err
	}
	if page.HasMore && len(page.Items) > 0 {
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeCursor(last.Time, last.Sequence)
	}
	return page, nil
}

// seekReverse positions the cursor on the newest key strictly older than the
// cursor key (when paging) or strictly older than `to` (first page).
func seekReverse(c *bolt.Cursor, cursorKey []byte, to time.Time) ([]byte, []byte) {
	if cursorKey != nil {
		key, value := c.Seek(cursorKey)
		if key == nil {
			key, value = c.Last()
			if key != nil && bytes.Equal(key, cursorKey) {
				key, value = c.Prev()
			}
			return key, value
		}
		return c.Prev()
	}
	key, _ := c.Seek(encodeRequestKey(to, 0))
	if key == nil {
		return c.Last()
	}
	return c.Prev()
}

func matchesQuery(r Request, query Query) bool {
	if query.Provider != "" && r.Provider != query.Provider {
		return false
	}
	if query.Model != "" && r.Model != query.Model {
		return false
	}
	return true
}

func (s *Store) attachBodyAvailability(tx *bolt.Tx, items []Request, now time.Time) {
	if !s.cfg.CaptureBodies || len(items) == 0 {
		return
	}
	for i := range items {
		if _, _, ok := resolveBody(tx, items[i].RequestID, now, s.cfg.BodyRetention); ok {
			items[i].BodyAvailable = true
		}
	}
}

// Summary aggregates the selected minute range from preaggregated stats only,
// groups by provider/model and downsamples the series to at most 300 points.
func (s *Store) Summary(query Query) (Summary, error) {
	now := s.now()
	to := query.To
	if to.IsZero() {
		to = now
	}
	from := query.From
	if from.IsZero() {
		from = to.Add(-defaultRequestRetention)
	}
	if !from.Before(to) {
		return Summary{}, ErrInvalidQuery
	}
	if retainedFrom := now.Add(-time.Duration(s.cfg.StatsRetentionDays) * 24 * time.Hour); from.Before(retainedFrom) {
		from = retainedFrom
	}
	if !from.Before(to) {
		return Summary{From: from, To: to, Groups: []Group{}, Series: []Point{}}, nil
	}

	fromMinute := statsMinute(from)
	// Half-open range [from, to): the last included minute is to-1ns.
	toMinute := statsMinute(to.Add(-time.Nanosecond))
	if toMinute < fromMinute {
		toMinute = fromMinute
	}

	var totals Counters
	groups := make(map[string]*Group)
	minutes := make(map[int64]Counters)

	err := s.view(func(tx *bolt.Tx) error {
		sb := tx.Bucket(bucketStats)
		if sb == nil {
			return nil
		}
		c := sb.Cursor()
		for key, value := c.Seek(statsKey(fromMinute, "", "")); key != nil; key, value = c.Next() {
			minute, provider, model, ok := parseStatsKey(key)
			if !ok {
				continue
			}
			if minute > toMinute {
				break
			}
			if query.Provider != "" && provider != query.Provider {
				continue
			}
			if query.Model != "" && model != query.Model {
				continue
			}
			counters, ok := decodeCounters(value)
			if !ok {
				continue
			}
			addCounters(&totals, counters)
			gk := provider + "\x00" + model
			g := groups[gk]
			if g == nil {
				g = &Group{Provider: provider, Model: model}
				groups[gk] = g
			}
			addCounters(&g.Counters, counters)
			mc := minutes[minute]
			addCounters(&mc, counters)
			minutes[minute] = mc
		}
		return nil
	})
	if err != nil {
		return Summary{}, err
	}

	out := Summary{
		From:   from,
		To:     to,
		Totals: totals,
		Groups: sortedGroups(groups),
		Series: buildSeries(fromMinute, int((toMinute-fromMinute)/60)+1, minutes),
	}
	return out, nil
}

func sortedGroups(groups map[string]*Group) []Group {
	out := make([]Group, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	// Deterministic ordering by provider then model.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}

const maxSeriesPoints = 300

// buildSeries downsamples second-aligned minute buckets into at most
// maxSeriesPoints points. fromSeconds is the first bucket boundary and
// totalMinutes the bucket count; every emitted point lies within the range.
func buildSeries(fromSeconds int64, totalMinutes int, minutes map[int64]Counters) []Point {
	if totalMinutes < 1 {
		totalMinutes = 1
	}
	buckets := totalMinutes
	if buckets > maxSeriesPoints {
		buckets = maxSeriesPoints
	}
	width := (totalMinutes + buckets - 1) / buckets
	// Ceil(total/width) can be smaller than buckets; using it keeps the last
	// point inside [from, to) instead of overshooting.
	count := (totalMinutes + width - 1) / width

	series := make([]Point, count)
	for i := range series {
		series[i].Time = time.Unix(fromSeconds+int64(i*width)*60, 0).UTC()
	}
	for minute, counters := range minutes {
		idx := int((minute-fromSeconds)/60) / width
		if idx < 0 {
			idx = 0
		}
		if idx >= count {
			idx = count - 1
		}
		addCounters(&series[idx].Counters, counters)
	}
	return series
}

// Body returns the stored body for a request. It accepts either an execution
// (usage) RequestID, resolved through the persisted RequestID->TraceID and
// TraceID->body indexes, or a direct interception RequestID. Bodies are only
// readable while capture is enabled, and expiry is enforced strictly on read,
// independent of the cleanup sweeper.
func (s *Store) Body(requestID string) (BodyDetail, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return BodyDetail{}, ErrNotFound
	}
	var out BodyDetail
	err := s.view(func(tx *bolt.Tx) error {
		if !s.cfg.CaptureBodies {
			// Capture is disabled: never serve retained bodies from an older
			// run. Kept inside the view so a closed store still reports
			// ErrClosed.
			return ErrNotFound
		}
		mb := tx.Bucket(bucketBodyMeta)
		bb := tx.Bucket(bucketBodies)
		if mb == nil || bb == nil {
			return ErrNotFound
		}
		now := s.now()
		bodyID, meta, ok := resolveBody(tx, requestID, now, s.cfg.BodyRetention)
		if !ok {
			return ErrNotFound
		}
		content := bb.Get([]byte(bodyID))
		if content == nil {
			return ErrNotFound
		}
		cp := make([]byte, len(content))
		copy(cp, content)
		out = BodyDetail{
			RequestID: requestID,
			TraceID:   meta.traceID,
			CreatedAt: meta.createdAt,
			ExpiresAt: meta.expiresAt,
			Content:   string(cp),
			Redacted:  meta.redacted,
			SizeBytes: int64(len(cp)),
		}
		return nil
	})
	if err != nil {
		return BodyDetail{}, err
	}
	return out, nil
}

// resolveBody locates a live body for an identifier. It first tries a direct
// body request ID lookup, then resolves an execution request ID through the
// usage_trace and trace_body indexes. Expired or ambiguous associations are
// treated as absent.
func resolveBody(tx *bolt.Tx, requestID string, now time.Time, retention time.Duration) (string, bodyMeta, bool) {
	mb := tx.Bucket(bucketBodyMeta)
	readMeta := func(id string) (bodyMeta, bool) {
		meta, ok := decodeBodyMeta(mb.Get([]byte(id)))
		if !ok {
			return bodyMeta{}, false
		}
		// Reconfiguration can shorten the read deadline, but must never
		// extend an earlier persisted expiry.
		if deadline := meta.createdAt.Add(retention); deadline.Before(meta.expiresAt) {
			meta.expiresAt = deadline
		}
		return meta, now.Before(meta.expiresAt)
	}
	if meta, ok := readMeta(requestID); ok {
		return requestID, meta, true
	}
	traceID, ok := lookupUsageTrace(tx, requestID)
	if !ok {
		return "", bodyMeta{}, false
	}
	bodyID, ok := lookupTraceBody(tx, traceID)
	if !ok {
		return "", bodyMeta{}, false
	}
	meta, ok := readMeta(bodyID)
	if !ok {
		return "", bodyMeta{}, false
	}
	return bodyID, meta, true
}

func lookupUsageTrace(tx *bolt.Tx, requestID string) (string, bool) {
	utb := tx.Bucket(bucketUsageTrace)
	if utb == nil {
		return "", false
	}
	traceID := utb.Get([]byte(requestID))
	if len(traceID) == 0 {
		return "", false
	}
	return string(traceID), true
}

// lookupTraceBody returns the sole body request ID for a trace, or false when
// the trace is unknown or ambiguous.
func lookupTraceBody(tx *bolt.Tx, traceID string) (string, bool) {
	if traceID == "" {
		return "", false
	}
	tbb := tx.Bucket(bucketTraceBody)
	if tbb == nil {
		return "", false
	}
	value := tbb.Get([]byte(traceID))
	if len(value) == 0 || bytes.Equal(value, ambiguousTrace) {
		return "", false
	}
	return string(value), true
}

func (s *Store) view(fn func(*bolt.Tx) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ErrClosed
	}
	return s.db.View(fn)
}

// runCleanup forces a sweeper pass through the writer and waits for it. It is
// used by tests to make retention deterministic.
func (s *Store) runCleanup() error {
	reply := make(chan error, 1)
	select {
	case s.cleanupCh <- reply:
	case <-s.stopCh:
		return ErrClosed
	}
	select {
	case err := <-reply:
		return err
	case <-s.stopCh:
		return ErrClosed
	}
}

// cleanupTx removes records past their independent retention windows. Request
// metadata, minute aggregates and bodies expire on separate schedules. Body
// byte accounting is applied only after the transaction commits so a rolled
// back cleanup cannot desynchronise s.bodyBytes.
func (s *Store) cleanupTx(now time.Time) error {
	var removedBytes int64
	err := s.db.Update(func(tx *bolt.Tx) error {
		removedBytes = 0
		if rb := tx.Bucket(bucketRequests); rb != nil {
			utb := tx.Bucket(bucketUsageTrace)
			cutoff := now.Add(-s.cfg.RequestRetention)
			c := rb.Cursor()
			for key, value := c.First(); key != nil; key, value = c.Next() {
				ts, ok := requestKeyTime(key)
				if !ok {
					if err := c.Delete(); err != nil {
						return err
					}
					continue
				}
				if !ts.Before(cutoff) {
					break
				}
				if utb != nil {
					var r Request
					if json.Unmarshal(value, &r) == nil && r.RequestID != "" {
						if err := utb.Delete([]byte(r.RequestID)); err != nil {
							return err
						}
					}
				}
				if err := c.Delete(); err != nil {
					return err
				}
			}
		}
		if sb := tx.Bucket(bucketStats); sb != nil {
			cutoffSeconds := now.Add(-time.Duration(s.cfg.StatsRetentionDays) * 24 * time.Hour).Unix()
			c := sb.Cursor()
			for key, _ := c.First(); key != nil; key, _ = c.Next() {
				minute, _, _, ok := parseStatsKey(key)
				if !ok {
					if err := c.Delete(); err != nil {
						return err
					}
					continue
				}
				if minute >= cutoffSeconds {
					break
				}
				if err := c.Delete(); err != nil {
					return err
				}
			}
		}
		var err error
		removedBytes, err = s.cleanupBodiesTx(tx, now)
		return err
	})
	if err != nil {
		return err
	}
	s.removeBodyBytes(removedBytes)
	return nil
}

// cleanupBodiesTx deletes expired/over-cap bodies and returns the bytes removed. The
// caller applies the total to s.bodyBytes only after the transaction commits.
func (s *Store) cleanupBodiesTx(tx *bolt.Tx, now time.Time) (int64, error) {
	ib := tx.Bucket(bucketBodyIndex)
	bb := tx.Bucket(bucketBodies)
	mb := tx.Bucket(bucketBodyMeta)
	tbb := tx.Bucket(bucketTraceBody)
	if ib == nil || bb == nil || mb == nil {
		return 0, nil
	}
	nowNanos := now.UnixNano()
	retentionNS := int64(s.cfg.BodyRetention)
	var removed int64
	c := ib.Cursor()
	for key, value := c.First(); key != nil; key, value = c.Next() {
		createdAt, ok := decodeBodyIndexCreated(key)
		if !ok {
			if err := c.Delete(); err != nil {
				return removed, err
			}
			continue
		}
		if createdAt.UnixNano()+retentionNS > nowNanos {
			break
		}
		requestID := string(key[8:])
		size, _ := decodeBodyIndexSize(value)
		if err := deleteBodyTx(bb, mb, tbb, requestID); err != nil {
			return removed, err
		}
		if err := c.Delete(); err != nil {
			return removed, err
		}
		if size > 0 {
			removed += size
		}
	}
	bodyBytes := s.bodyBytes
	s.removeBodyBytesFrom(&bodyBytes, removed)
	if err := s.evictBodiesTx(bb, mb, ib, tbb, &bodyBytes); err != nil {
		return removed, err
	}
	return s.bodyBytes - bodyBytes, nil
}

func (s *Store) removeBodyBytes(size int64) {
	if size > 0 {
		s.bodyBytes -= size
		if s.bodyBytes < 0 {
			s.bodyBytes = 0
		}
	}
}

// writerLoop is the single mutation goroutine: it batches queued usage and
// bodies, triggers cleanup and serializes all writes.
func (s *Store) writerLoop() {
	defer close(s.writerDone)

	ticker := time.NewTicker(s.cfg.FlushInterval)
	defer ticker.Stop()
	cleanupTicker := time.NewTicker(s.cleanupPeriod)
	defer cleanupTicker.Stop()

	var usage []Request
	var bodies []pendingBody

	flush := func() error {
		if len(usage) == 0 && len(bodies) == 0 {
			return nil
		}
		n := len(usage) + len(bodies)
		var err error
		if err = s.writeBatch(usage, bodies); err != nil {
			// Observation loss is allowed, but it must be reported accurately.
			s.writeErrors.Add(1)
			s.droppedUsage.Add(uint64(len(usage)))
			s.droppedBodies.Add(uint64(len(bodies)))
		}
		s.pendingCount.Add(-int64(n))
		for _, b := range bodies {
			s.queuedBodyBytes.Add(-int64(len(b.body)))
			s.releasePending(b.requestID)
		}
		usage = usage[:0]
		bodies = bodies[:0]
		return err
	}
	// drain takes a bounded snapshot of the queues and flushes in batches, so a
	// busy producer cannot make Flush/Close spin forever or grow memory without
	// bound. The budget equals the total channel capacity, which is also the
	// most that can be pending after producers are quiesced by Close.
	drain := func() error { return s.drainQueues(&usage, &bodies, flush) }
	record := func(err error) {
		if err != nil && s.finalErr == nil {
			s.finalErr = err
		}
	}

	for {
		select {
		case u := <-s.usageCh:
			usage = append(usage, u)
			s.pendingCount.Add(1)
			if len(usage) >= writeBatchSize {
				record(flush())
			}
		case b := <-s.bodyCh:
			bodies = append(bodies, b)
			s.pendingCount.Add(1)
			if len(bodies)+len(usage) >= writeBatchSize {
				record(flush())
			}
		case <-ticker.C:
			record(flush())
		case <-cleanupTicker.C:
			if err := s.cleanupTx(s.now()); err != nil {
				s.writeErrors.Add(1)
				record(err)
			}
		case reply := <-s.cleanupCh:
			err := s.cleanupTx(s.now())
			if err != nil {
				s.writeErrors.Add(1)
			}
			record(err)
			reply <- err
		case reply := <-s.flushCh:
			err := drain()
			if cerr := s.cleanupTx(s.now()); cerr != nil {
				s.writeErrors.Add(1)
				if err == nil {
					err = cerr
				}
			}
			record(err)
			reply <- err
		case <-s.stopCh:
			record(drain())
			if err := s.cleanupTx(s.now()); err != nil {
				s.writeErrors.Add(1)
				record(err)
			}
			// Producers are quiesced (closedFlag set before stopCh), so one
			// more bounded drain flushes anything enqueued before shutdown.
			record(drain())
			return
		}
	}
}

// drainQueues is the flush barrier's bounded write pass. It flushes full
// batches and the remaining tail, returning the first error from this pass.
func (s *Store) drainQueues(usage *[]Request, bodies *[]pendingBody, flush func() error) error {
	var firstErr error
	budget := cap(s.usageCh) + cap(s.bodyCh)
drainLoop:
	for budget > 0 {
		select {
		case u := <-s.usageCh:
			*usage = append(*usage, u)
			s.pendingCount.Add(1)
		case b := <-s.bodyCh:
			*bodies = append(*bodies, b)
			s.pendingCount.Add(1)
		default:
			break drainLoop
		}
		budget--
		if len(*usage)+len(*bodies) >= writeBatchSize {
			if err := flush(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	if err := flush(); firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// writeBatch persists a batch of usage records and bodies in one transaction.
// s.bodyBytes is owned by this goroutine; it is only committed on success.
func (s *Store) writeBatch(usage []Request, bodies []pendingBody) error {
	bodyBytes := s.bodyBytes
	err := s.db.Update(func(tx *bolt.Tx) error {
		rb := tx.Bucket(bucketRequests)
		sb := tx.Bucket(bucketStats)
		bb := tx.Bucket(bucketBodies)
		mb := tx.Bucket(bucketBodyMeta)
		ib := tx.Bucket(bucketBodyIndex)
		utb := tx.Bucket(bucketUsageTrace)
		tbb := tx.Bucket(bucketTraceBody)

		for i := range usage {
			r := usage[i]
			seq, err := rb.NextSequence()
			if err != nil {
				return err
			}
			r.Sequence = seq
			r.BodyAvailable = false
			value, err := json.Marshal(r)
			if err != nil {
				return err
			}
			if err := rb.Put(encodeRequestKey(r.Time, seq), value); err != nil {
				return err
			}
			if err := addStats(sb, r); err != nil {
				return err
			}
			// Persist the execution RequestID -> TraceID association so a body
			// captured under the (different) interception RequestID can later be
			// resolved from the usage record alone.
			if r.RequestID != "" && r.TraceID != "" {
				if err := utb.Put([]byte(r.RequestID), []byte(r.TraceID)); err != nil {
					return err
				}
			}
		}
		for _, item := range bodies {
			bytes, err := s.putBodyTx(bb, mb, ib, tbb, item, &bodyBytes)
			if err != nil {
				return err
			}
			bodyBytes = bytes
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.bodyBytes = bodyBytes
	return nil
}

// putBodyTx stores a redacted body, maintaining the metadata, eviction index
// and trace association, and evicts the oldest bodies when the cap is exceeded.
func (s *Store) putBodyTx(bb, mb, ib, tbb *bolt.Bucket, item pendingBody, bodyBytes *int64) (int64, error) {
	if !json.Valid(item.body) {
		s.droppedBodies.Add(1)
		return *bodyBytes, nil
	}
	content, redacted, err := redactBody(item.body)
	if err != nil {
		// Fail closed: never persist a body we could not fully re-encode.
		s.droppedBodies.Add(1)
		return *bodyBytes, nil
	}
	if len(content) > s.cfg.MaxBodyBytes {
		s.droppedBodies.Add(1)
		return *bodyBytes, nil
	}
	requestID := []byte(item.requestID)
	if mb.Get(requestID) != nil {
		// Already captured; capture once per execution.
		return *bodyBytes, nil
	}

	createdAt := item.capturedAt
	if createdAt.IsZero() {
		createdAt = s.now()
	}
	expiresAt := createdAt.Add(s.cfg.BodyRetention)
	size := int64(len(content))

	if err := bb.Put(requestID, content); err != nil {
		return *bodyBytes, err
	}
	if err := mb.Put(requestID, encodeBodyMeta(bodyMeta{
		createdAt: createdAt,
		expiresAt: expiresAt,
		size:      size,
		redacted:  redacted,
		traceID:   item.traceID,
	})); err != nil {
		return *bodyBytes, err
	}
	if err := ib.Put(bodyIndexKey(createdAt, item.requestID), encodeBodyIndexValue(size)); err != nil {
		return *bodyBytes, err
	}
	if err := recordTraceBody(tbb, item.traceID, item.requestID); err != nil {
		return *bodyBytes, err
	}
	*bodyBytes += size

	if err := s.evictBodiesTx(bb, mb, ib, tbb, bodyBytes); err != nil {
		return *bodyBytes, err
	}
	return *bodyBytes, nil
}

// evictBodiesTx enforces the current cap in oldest-first order. Accounting is
// local to the transaction; the caller publishes it only after commit.
func (s *Store) evictBodiesTx(bb, mb, ib, tbb *bolt.Bucket, bodyBytes *int64) error {
	c := ib.Cursor()
	for key, value := c.First(); key != nil && *bodyBytes > s.cfg.MaxBodyStorageBytes; key, value = c.Next() {
		if _, ok := decodeBodyIndexCreated(key); !ok {
			if err := c.Delete(); err != nil {
				return err
			}
			continue
		}
		id := key[8:]
		evicted, _ := decodeBodyIndexSize(value)
		if err := deleteBodyTx(bb, mb, tbb, string(id)); err != nil {
			return err
		}
		if err := c.Delete(); err != nil {
			return err
		}
		s.removeBodyBytesFrom(bodyBytes, evicted)
	}
	return nil
}

// recordTraceBody maintains the TraceID -> body request ID index. A trace that
// observes more than one distinct capture becomes ambiguous and resolves to no
// body, so trace-level lookup never arbitrarily selects one execution.
func recordTraceBody(tbb *bolt.Bucket, traceID, requestID string) error {
	if traceID == "" {
		return nil
	}
	existing := tbb.Get([]byte(traceID))
	switch {
	case existing == nil:
		return tbb.Put([]byte(traceID), []byte(requestID))
	case string(existing) == requestID:
		return nil
	default:
		return tbb.Put([]byte(traceID), ambiguousTrace)
	}
}

// deleteBodyTx removes a body's content, metadata and trace association.
func deleteBodyTx(bb, mb, tbb *bolt.Bucket, requestID string) error {
	if err := bb.Delete([]byte(requestID)); err != nil {
		return err
	}
	if meta, ok := decodeBodyMeta(mb.Get([]byte(requestID))); ok && meta.traceID != "" {
		if existing := tbb.Get([]byte(meta.traceID)); string(existing) == requestID {
			if err := tbb.Delete([]byte(meta.traceID)); err != nil {
				return err
			}
		}
	}
	return mb.Delete([]byte(requestID))
}

func (s *Store) removeBodyBytesFrom(bodyBytes *int64, size int64) {
	if size <= 0 {
		return
	}
	*bodyBytes -= size
	if *bodyBytes < 0 {
		*bodyBytes = 0
	}
}

func addStats(sb *bolt.Bucket, r Request) error {
	key := statsKey(statsMinute(r.Time), r.Provider, r.Model)
	var counters Counters
	if value := sb.Get(key); value != nil {
		decoded, ok := decodeCounters(value)
		if !ok {
			return fmt.Errorf("corrupt stats record")
		}
		counters = decoded
	}
	addRequestCounters(&counters, r)
	return sb.Put(key, encodeCounters(counters))
}
