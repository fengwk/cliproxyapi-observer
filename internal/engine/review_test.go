package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	bolt "go.etcd.io/bbolt"
	"testing"
	"time"
)

func drainCapture(s *Store) {
	close(s.captures)
	s.worker.Wait()
	s.captures = make(chan CaptureInput, 64)
	s.worker.Add(1)
	go s.captureWorker()
}

// An abrupt termination keeps every committed fact. Both used and never-used
// Keys work right after restart, prior totals, limits and history survive,
// and new usage increments the same counters.
func TestCrashRestartKeepsKeysUsable(t *testing.T) {
	s, now, p := fixture(t)
	k, secret := key(t, s, KeySpec{Enabled: true, Limits: Limits{Total: 100}})
	unused, unusedSecret := key(t, s, KeySpec{Enabled: true, Limits: Limits{Total: 21}, MaxConcurrent: 1, AllowedModels: []string{"gpt-exact"}})
	must(t, s.RecordUsage(event(k, "bill", *now)))
	close(s.captures)
	s.worker.Wait()
	must(t, s.db.Close())
	s.closed = true
	r, e := Open(Options{DataPath: s.path, Now: func() time.Time { return *now }, Pricer: p})
	must(t, e)
	defer r.Close()
	// No acknowledgement, reset or rotation is needed before the Key serves
	// again: an interrupted process never fences an existing principal.
	must(t, r.Admit(Admission{RequestID: "old", KeyID: k.ID}))
	must(t, r.Complete("old", *now))
	view, e := r.Authenticate(secret)
	must(t, e)
	if view.ID != k.ID || view.Used.Total != 7 || view.Limits.Total != 100 {
		t.Fatal(view)
	}
	// A Key that never produced usage must not inherit another Key's loss.
	untouched, e := r.Authenticate(unusedSecret)
	must(t, e)
	if untouched.ID != unused.ID || untouched.Used != (Limits{}) || untouched.Limits.Total != 21 {
		t.Fatal(untouched)
	}
	must(t, r.Admit(Admission{RequestID: "first-use", KeyID: unused.ID, Model: "gpt-exact"}))
	must(t, r.Complete("first-use", *now))
	d, e := r.Detail("bill")
	must(t, e)
	if d.Record.CostMicroUSD != 7 {
		t.Fatal(d)
	}
	// New usage increments the retained counters.
	must(t, r.RecordUsage(event(k, "after", *now)))
	view, e = r.Authenticate(secret)
	must(t, e)
	if view.Used.Total != 14 {
		t.Fatal(view)
	}
}

// The effective view reports an expired window as zero without rewriting the
// stored anchor; only a real charge restarts the cycle.
func TestEffectiveViewExpiresWindowsWithoutRestartingAnchor(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	must(t, s.RecordUsage(event(k, "usage", *now)))
	*now = now.Add(6 * time.Hour)
	view := total(t, s)
	if view.Used.FiveHour != 0 || view.Used.Week != 7 || !view.Starts.FiveHour.Equal(k.Starts.FiveHour) {
		t.Fatal(view)
	}
	must(t, s.db.View(func(tx *bolt.Tx) error {
		var stored storedKey
		if e := get(tx.Bucket([]byte("keys")), k.ID, &stored); e != nil {
			return e
		}
		if stored.Used.FiveHour != 7 || !stored.Starts.FiveHour.Equal(k.Starts.FiveHour) {
			t.Fatal("read mutated cycle", stored)
		}
		return nil
	}))
}

// A failed write is a visible error, not a fence. Nothing is charged, the
// affected Key keeps serving, an unrelated Key is unaffected and the next
// valid usage is accounted exactly once.
func TestFailedWriteLeavesNoChargeAndNoAdmissionBlock(t *testing.T) {
	s, now, p := fixture(t)
	a, _ := key(t, s, KeySpec{Enabled: true})
	b, _ := key(t, s, KeySpec{Enabled: true})
	s.writeFault = func() error { return ErrBusy }
	if e := s.RecordUsage(event(a, "a", *now)); !errors.Is(e, ErrAccounting) {
		t.Fatal("missing failure", e)
	}
	// The failed transaction stored no record and charged no window.
	if _, e := s.Detail("a"); !errors.Is(e, ErrNotFound) {
		t.Fatal("failed write left a record", e)
	}
	ks, e := s.Keys()
	must(t, e)
	for _, v := range ks {
		if v.Used.Total != 0 {
			t.Fatal("failed write charged", v)
		}
	}
	// The failure gates neither the affected nor an unrelated Key.
	must(t, s.Admit(Admission{RequestID: "a-live", KeyID: a.ID}))
	must(t, s.Complete("a-live", *now))
	must(t, s.Admit(Admission{RequestID: "b-live", KeyID: b.ID}))
	must(t, s.Complete("b-live", *now))
	// The next valid usage is accounted normally and deduplicated.
	s.writeFault = nil
	p.cost = 7
	must(t, s.RecordUsage(event(a, "a-after", *now)))
	must(t, s.RecordUsage(event(a, "a-after", *now)))
	d, e := s.Detail("a-after")
	must(t, e)
	if d.Record.CostMicroUSD != 7 {
		t.Fatal(d)
	}
	ks, e = s.Keys()
	must(t, e)
	used := map[string]int64{}
	for _, v := range ks {
		used[v.ID] = v.Used.Total
	}
	if used[a.ID] != 7 || used[b.ID] != 0 {
		t.Fatal(used)
	}
}

// A failed accounting write is lost with the process instead of being fenced
// into the database: the shutdown is clean, committed totals survive the
// restart and the missing execution simply stays missing.
func TestCloseKeepsCommittedDataAndDropsMissingExecutions(t *testing.T) {
	s, now, p := fixture(t)
	a, _ := key(t, s, KeySpec{Enabled: true})
	b, _ := key(t, s, KeySpec{Enabled: true})
	must(t, s.RecordUsage(event(a, "settled", *now)))
	s.writeFault = func() error { return ErrBusy }
	if e := s.RecordUsage(event(a, "unsettled", *now)); !errors.Is(e, ErrAccounting) {
		t.Fatal("failure hidden", e)
	}
	// A clean shutdown no longer reports retained accounting debt.
	if e := s.Close(); e != nil {
		t.Fatal("close failed", e)
	}
	r, e := Open(Options{DataPath: s.path, Now: func() time.Time { return *now }, Pricer: p})
	must(t, e)
	defer r.Close()
	// The committed execution is retained and the missing one stays missing.
	d, e := r.Detail("settled")
	must(t, e)
	if d.Record.CostMicroUSD != 7 {
		t.Fatal(d)
	}
	if _, e = r.Detail("unsettled"); !errors.Is(e, ErrNotFound) {
		t.Fatal("missing execution was charged", e)
	}
	// Both Keys stay usable; the unrelated one is unaffected.
	must(t, r.Admit(Admission{RequestID: "other", KeyID: b.ID}))
	must(t, r.Complete("other", *now))
	must(t, r.Admit(Admission{RequestID: "a-live", KeyID: a.ID}))
	must(t, r.Complete("a-live", *now))
	ks, e := r.Keys()
	must(t, e)
	if len(ks) != 2 {
		t.Fatal(ks)
	}
}

// Quota and concurrency limits stay independent of accounting failures: a
// failed write neither relaxes a configured dollar limit nor a concurrent
// limit, and both keep rejecting work afterwards.
func TestQuotaAndConcurrencyStayIndependentOfAccountingFailure(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true, Limits: Limits{Total: 7}})
	s.writeFault = func() error { return ErrBusy }
	if e := s.RecordUsage(event(k, "debt", *now)); !errors.Is(e, ErrAccounting) {
		t.Fatal(e)
	}
	s.writeFault = nil
	// The configured dollar limit is still enforced after the failure.
	must(t, s.RecordUsage(event(k, "charged", *now)))
	if e := s.Admit(Admission{RequestID: "over-quota", KeyID: k.ID}); !errors.Is(e, ErrQuota) {
		t.Fatal("quota lost after the accounting failure", e)
	}
	// The concurrent limit is enforced independently of the dollar limit.
	b, _ := key(t, s, KeySpec{Enabled: true, MaxConcurrent: 1})
	must(t, s.Admit(Admission{RequestID: "first", KeyID: b.ID}))
	if e := s.Admit(Admission{RequestID: "second", KeyID: b.ID}); !errors.Is(e, ErrConcurrency) {
		t.Fatal("concurrency limit lost", e)
	}
	must(t, s.Complete("first", *now))
}

// Durable monotonic cutoff prevents an expired execution from charging again,
// including clock rollback/restart. An old or unknown timing is rejected for
// that execution only and never blocks the Key.
func TestReplayHorizonAndMonotonicCutoff(t *testing.T) {
	s, now, p := fixture(t)
	a, _ := key(t, s, KeySpec{Enabled: true})
	b, _ := key(t, s, KeySpec{Enabled: true})
	original := *now
	u := event(a, "old", *now)
	must(t, s.RecordUsage(u))
	must(t, s.RecordUsage(u))
	p.cost = 0
	for i := 0; i < 20; i++ {
		extra := u
		extra.RequestID = random()
		must(t, s.RecordUsage(extra))
	}
	p.cost = 7
	*now = now.Add(8 * 24 * time.Hour)
	must(t, s.Maintenance())
	must(t, s.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("dedup")).Stats().KeyN != 0 {
			t.Fatal("dedup not pruned")
		}
		return nil
	}))
	cutoff := s.cutoff
	must(t, s.Close())
	*now = original
	r, e := Open(Options{DataPath: s.path, Now: func() time.Time { return *now }, Pricer: p})
	must(t, e)
	defer r.Close()
	if e = r.RecordUsage(u); !errors.Is(e, ErrAccounting) {
		t.Fatal("old replay accepted", e)
	}
	if !r.cutoff.Equal(cutoff) {
		t.Fatal("cutoff rolled backward")
	}
	must(t, r.Admit(Admission{RequestID: "other", KeyID: b.ID}))
	must(t, r.Complete("other", *now))
	ks, e := r.Keys()
	must(t, e)
	for _, k := range ks {
		if k.ID == a.ID && k.Used.Total != 7 {
			t.Fatal("double charge", k)
		}
	}
	unknown := event(b, "unknown", time.Time{})
	if e = r.RecordUsage(unknown); !errors.Is(e, ErrAccounting) {
		t.Fatal("unknown time accepted", e)
	}
	// The rejected executions block neither Key.
	must(t, r.Admit(Admission{RequestID: "after-invalid", KeyID: b.ID}))
	must(t, r.Complete("after-invalid", *now))
	must(t, r.Admit(Admission{RequestID: "after-invalid-a", KeyID: a.ID}))
	must(t, r.Complete("after-invalid-a", *now))
}

// Completion identities have both a hard bound and TTL; trace collision
// tombstones expire with raw retention rather than leaking forever.
func TestIdentityStateBounds(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	for i := 0; i < maxCompleted+10; i++ {
		id := random()
		s.completed[id] = Admission{RequestID: id, At: *now}
	}
	s.pruneCompletedLocked()
	if len(s.completed) != maxCompleted {
		t.Fatal(len(s.completed))
	}
	must(t, s.Admit(Admission{RequestID: "a", TraceID: "trace", KeyID: k.ID}))
	must(t, s.Complete("a", *now))
	must(t, s.Admit(Admission{RequestID: "b", TraceID: "trace", KeyID: k.ID}))
	must(t, s.Complete("b", *now))
	*now = now.Add(25 * time.Hour)
	must(t, s.Maintenance())
	if len(s.completed) != 0 {
		t.Fatal("TTL leak")
	}
	must(t, s.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("correlations")).Stats().KeyN != 0 {
			t.Fatal("correlation leak")
		}
		return nil
	}))
}

// Recent raw ranges yield hourly zero-filled trends, exact half-open bounds,
// and never double count full-day rollups into the same hourly chart.
func TestHourlyTrendBoundaries(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	start := day(*now).Add(time.Hour)
	for i, at := range []time.Time{start, start.Add(2 * time.Hour), start.Add(3 * time.Hour)} {
		u := event(k, string(rune('a'+i)), at)
		u.Failed = i == 1
		must(t, s.RecordUsage(u))
	}
	summary, e := s.Summary(Query{From: start, To: start.Add(3 * time.Hour)})
	must(t, e)
	if summary.Granularity != "hour" || summary.Requests != 2 || summary.Failures != 1 || len(summary.Buckets) != 3 {
		t.Fatal(summary)
	}
	if summary.Buckets[0].Requests != 1 || summary.Buckets[1].Requests != 0 || summary.Buckets[2].Failures != 1 {
		t.Fatal(summary)
	}
	whole, e := s.Summary(Query{From: day(*now), To: day(*now).Add(24 * time.Hour)})
	must(t, e)
	if whole.Granularity != "hour" || whole.Requests != 3 || len(whole.Buckets) != 24 || whole.Buckets[0].Requests != 0 || whole.Buckets[1].Requests != 1 || whole.Buckets[3].Failures != 1 || whole.Buckets[4].Requests != 1 {
		t.Fatal("daily rollup leaked into hourly full-day range", whole)
	}
}

// Tiny legitimate budgets remain accepted, but the queue cannot be configured
// above 8MiB per body and finite 1GiB compressed diagnostic storage.
func TestCaptureConfigCaps(t *testing.T) {
	s, _, _ := fixture(t)
	v, _ := s.Settings()
	for _, size := range []int{1, maxCaptureBody} {
		v.MaxBodyBytes = size
		v.MaxBodyStorageBytes = 1
		_, e := s.UpdateSettings(v)
		must(t, e)
	}
	v.MaxBodyBytes = maxCaptureBody + 1
	if _, e := s.UpdateSettings(v); !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
	v.MaxBodyBytes = 1
	v.MaxBodyStorageBytes = maxCaptureStorage + 1
	if _, e := s.UpdateSettings(v); !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
}

// Actual metadata bytes are gzip and much smaller, while oversized input keeps
// headers but never copies a partial body into the queue.
func TestCompressedRecordsAndTruncatedSentinel(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	v, _ := s.Settings()
	v.CaptureEnabled = true
	v.MaxBodyBytes = 1
	_, e := s.UpdateSettings(v)
	must(t, e)
	if !s.Capture(CaptureInput{RequestID: "a", TraceID: "trace", KeyID: k.ID, Body: bytes.Repeat([]byte("x"), 1<<20)}) {
		t.Fatal("oversized body queue rejected")
	}
	drainCapture(s)
	u := event(k, "compressed", *now)
	u.Model = string(bytes.Repeat([]byte("model-"), 1000))
	must(t, s.RecordUsage(u))
	must(t, s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte("records")).Get([]byte(u.RequestID))
		if !bytes.HasPrefix(raw, []byte{0x1f, 0x8b}) {
			t.Fatal("metadata not gzip")
		}
		var data recordData
		must(t, decode(raw, &data))
		plain, _ := json.Marshal(data)
		if len(raw) >= len(plain)/4 {
			t.Fatal("no actual shrink")
		}
		return nil
	}))
	d, e := s.Detail(u.RequestID)
	must(t, e)
	if d.CaptureStatus != captureStatusTooLarge || d.BodyStatus != captureStatusTooLarge || d.HeadersStatus != captureStatusAvailable || string(d.Body) != "null" {
		t.Fatal(d)
	}
}

// Safe money maximum stays exact. An unsafe quote or an accumulator overflow
// rolls the whole transaction back, charges no window partially, gates no
// admission and leaves the next valid usage accountable.
func TestSafeMoneyOverflowScoped(t *testing.T) {
	s, now, p := fixture(t)
	a, _ := key(t, s, KeySpec{Enabled: true})
	b, _ := key(t, s, KeySpec{Enabled: true})
	used := func(id string) int64 {
		t.Helper()
		ks, e := s.Keys()
		must(t, e)
		for _, v := range ks {
			if v.ID == id {
				return v.Used.Total
			}
		}
		t.Fatalf("key %s missing", id)
		return 0
	}
	// The exact safe maximum is still an exact charge.
	p.cost = maxSafe
	must(t, s.RecordUsage(event(a, "max", *now)))
	p.cost = 1
	if e := s.RecordUsage(event(a, "overflow", *now)); !errors.Is(e, ErrAccounting) {
		t.Fatal(e)
	}
	d, e := s.Detail("max")
	must(t, e)
	if d.Record.CostMicroUSD != maxSafe {
		t.Fatal(d)
	}
	if _, e = s.Detail("overflow"); !errors.Is(e, ErrNotFound) {
		t.Fatal("partial transaction", e)
	}
	// The failed transaction charged nothing extra and gated no admission.
	if used(a.ID) != maxSafe {
		t.Fatal("partial charge", used(a.ID))
	}
	must(t, s.Admit(Admission{RequestID: "live-after-overflow", KeyID: a.ID}))
	must(t, s.Complete("live-after-overflow", *now))
	// An unsafe quote is rejected the same way and is never retained.
	p.cost = maxSafe + 1
	if e = s.RecordUsage(event(b, "unsafe-quote", *now)); !errors.Is(e, ErrAccounting) {
		t.Fatal(e)
	}
	if _, e = s.Detail("unsafe-quote"); !errors.Is(e, ErrNotFound) {
		t.Fatal("unsafe quote was stored", e)
	}
	if used(b.ID) != 0 {
		t.Fatal("unsafe quote charged", used(b.ID))
	}
	// The next valid usage is accounted normally.
	p.cost = 7
	must(t, s.RecordUsage(event(b, "valid", *now)))
	d, e = s.Detail("valid")
	must(t, e)
	if d.Record.CostMicroUSD != 7 {
		t.Fatal(d)
	}
	// The affected Key accepts the next valid usage once its windows and the
	// daily aggregates start fresh.
	*now = now.Add(24 * time.Hour)
	_, e = s.ResetKey(a.ID, []string{"five_hour", "week", "month", "total"})
	must(t, e)
	must(t, s.RecordUsage(event(a, "valid-after-overflow", *now)))
	d, e = s.Detail("valid-after-overflow")
	must(t, e)
	if d.Record.CostMicroUSD != 7 {
		t.Fatal(d)
	}
	if used(a.ID) != 7 {
		t.Fatal("post-overflow usage not charged", used(a.ID))
	}
}
