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
	bucketRequests  = []byte("requests")
	bucketStats     = []byte("stats")
	bucketBodies    = []byte("bodies")
	bucketBodyMeta  = []byte("body_meta")
	bucketBodyIndex = []byte("body_index")
	allBuckets      = [][]byte{bucketRequests, bucketStats, bucketBodies, bucketBodyMeta, bucketBodyIndex}
)

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
// returns false when the record is malformed or the bounded queue is full.
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
	select {
	case s.usageCh <- req:
		return true
	default:
		s.droppedUsage.Add(1)
		return false
	}
}

// Capture copies and enqueues the request body for asynchronous storage. It
// never blocks and returns false when the body is ineligible, oversized or the
// bounded queue/budget is exhausted.
func (s *Store) Capture(req pluginapi.RequestInterceptRequest) bool {
	if s.closedFlag.Load() {
		s.droppedBodies.Add(1)
		return false
	}
	if !s.cfg.CaptureBodies {
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

	// Capture once per execution: drop duplicates before they consume memory.
	s.pendingMu.Lock()
	if _, ok := s.pendingBodies[requestID]; ok {
		s.pendingMu.Unlock()
		return true
	}
	s.pendingBodies[requestID] = struct{}{}
	s.pendingMu.Unlock()

	size := int64(len(body))
	cp := make([]byte, len(body))
	copy(cp, body)
	if s.queuedBodyBytes.Add(size) > s.bodyBudget {
		s.queuedBodyBytes.Add(-size)
		s.releasePending(requestID)
		s.droppedBodies.Add(1)
		return false
	}
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
// It is idempotent and protects concurrent readers from the closed database.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closedFlag.Store(true)
		close(s.stopCh)
		<-s.writerDone
		s.mu.Lock()
		s.closed = true
		s.closeErr = s.db.Close()
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
	mb := tx.Bucket(bucketBodyMeta)
	if mb == nil {
		return
	}
	for i := range items {
		meta, ok := decodeBodyMeta(mb.Get([]byte(items[i].RequestID)))
		if !ok {
			continue
		}
		if now.Before(meta.expiresAt) {
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
// maxSeriesPoints points. fromSeconds is the first bucket boundary.
func buildSeries(fromSeconds int64, totalMinutes int, minutes map[int64]Counters) []Point {
	if totalMinutes < 1 {
		totalMinutes = 1
	}
	buckets := totalMinutes
	if buckets > maxSeriesPoints {
		buckets = maxSeriesPoints
	}
	width := (totalMinutes + buckets - 1) / buckets

	series := make([]Point, buckets)
	for i := range series {
		series[i].Time = time.Unix(fromSeconds+int64(i*width)*60, 0).UTC()
	}
	for minute, counters := range minutes {
		idx := int((minute-fromSeconds)/60) / width
		if idx < 0 {
			idx = 0
		}
		if idx >= buckets {
			idx = buckets - 1
		}
		addCounters(&series[idx].Counters, counters)
	}
	return series
}

// Body returns the stored body for a request. Expiry is enforced strictly on
// read, independent of the cleanup sweeper.
func (s *Store) Body(requestID string) (BodyDetail, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return BodyDetail{}, ErrNotFound
	}
	var out BodyDetail
	err := s.view(func(tx *bolt.Tx) error {
		mb := tx.Bucket(bucketBodyMeta)
		bb := tx.Bucket(bucketBodies)
		if mb == nil || bb == nil {
			return ErrNotFound
		}
		meta, ok := decodeBodyMeta(mb.Get([]byte(requestID)))
		if !ok || !s.now().Before(meta.expiresAt) {
			return ErrNotFound
		}
		content := bb.Get([]byte(requestID))
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
// metadata, minute aggregates and bodies expire on separate schedules.
func (s *Store) cleanupTx(now time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if rb := tx.Bucket(bucketRequests); rb != nil {
			cutoff := now.Add(-s.cfg.RequestRetention)
			c := rb.Cursor()
			for key, _ := c.First(); key != nil; key, _ = c.Next() {
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
		return s.cleanupBodiesTx(tx, now)
	})
}

func (s *Store) cleanupBodiesTx(tx *bolt.Tx, now time.Time) error {
	ib := tx.Bucket(bucketBodyIndex)
	bb := tx.Bucket(bucketBodies)
	mb := tx.Bucket(bucketBodyMeta)
	if ib == nil || bb == nil || mb == nil {
		return nil
	}
	nowNanos := now.UnixNano()
	retentionNS := int64(s.cfg.BodyRetention)
	c := ib.Cursor()
	for key, value := c.First(); key != nil; key, value = c.Next() {
		createdAt, ok := decodeBodyIndexCreated(key)
		if !ok {
			if err := c.Delete(); err != nil {
				return err
			}
			continue
		}
		if createdAt.UnixNano()+retentionNS > nowNanos {
			break
		}
		requestID := key[8:]
		size, _ := decodeBodyIndexSize(value)
		if err := bb.Delete(requestID); err != nil {
			return err
		}
		if err := mb.Delete(requestID); err != nil {
			return err
		}
		if err := c.Delete(); err != nil {
			return err
		}
		s.removeBodyBytes(size)
	}
	return nil
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

	flush := func() {
		if len(usage) == 0 && len(bodies) == 0 {
			return
		}
		if err := s.writeBatch(usage, bodies); err != nil {
			s.writeErrors.Add(1)
		}
		s.pendingCount.Add(-int64(len(usage) + len(bodies)))
		for _, b := range bodies {
			s.queuedBodyBytes.Add(-int64(len(b.body)))
			s.releasePending(b.requestID)
		}
		usage = usage[:0]
		bodies = bodies[:0]
	}
	collect := func() {
		for {
			select {
			case u := <-s.usageCh:
				usage = append(usage, u)
				s.pendingCount.Add(1)
			case b := <-s.bodyCh:
				bodies = append(bodies, b)
				s.pendingCount.Add(1)
			default:
				return
			}
		}
	}

	for {
		select {
		case u := <-s.usageCh:
			usage = append(usage, u)
			s.pendingCount.Add(1)
			if len(usage) >= writeBatchSize {
				flush()
			}
		case b := <-s.bodyCh:
			bodies = append(bodies, b)
			s.pendingCount.Add(1)
			if len(bodies)+len(usage) >= writeBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-cleanupTicker.C:
			if err := s.cleanupTx(s.now()); err != nil {
				s.writeErrors.Add(1)
			}
		case reply := <-s.cleanupCh:
			reply <- s.cleanupTx(s.now())
		case reply := <-s.flushCh:
			collect()
			flush()
			if err := s.cleanupTx(s.now()); err != nil {
				s.writeErrors.Add(1)
			}
			reply <- nil
		case <-s.stopCh:
			collect()
			flush()
			if err := s.cleanupTx(s.now()); err != nil {
				s.writeErrors.Add(1)
			}
			flush()
			return
		}
	}
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
		}
		for _, item := range bodies {
			bytes, err := s.putBodyTx(bb, mb, ib, item, &bodyBytes)
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

// putBodyTx stores a redacted body, maintaining the metadata and index
// buckets, and evicts the oldest bodies when the storage cap is exceeded.
func (s *Store) putBodyTx(bb, mb, ib *bolt.Bucket, item pendingBody, bodyBytes *int64) (int64, error) {
	if !json.Valid(item.body) {
		s.droppedBodies.Add(1)
		return *bodyBytes, nil
	}
	content, redacted := redactBody(item.body)
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
	*bodyBytes += size

	c := ib.Cursor()
	for key, value := c.First(); key != nil && *bodyBytes > s.cfg.MaxBodyStorageBytes; key, value = c.Next() {
		id := key[8:]
		evicted, _ := decodeBodyIndexSize(value)
		if err := bb.Delete(id); err != nil {
			return *bodyBytes, err
		}
		if err := mb.Delete(id); err != nil {
			return *bodyBytes, err
		}
		if err := c.Delete(); err != nil {
			return *bodyBytes, err
		}
		s.removeBodyBytesFrom(bodyBytes, evicted)
	}
	return *bodyBytes, nil
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
