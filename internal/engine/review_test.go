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

// Crash fences existing principals durably, never future Keys. Explicit recovery
// preserves counters/limits/history and cannot be replaced by reset or rotation.
func TestRecoverCrashScopedAndPersistent(t *testing.T) {
	s, now, p := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true, Limits: Limits{Total: 100}})
	must(t, s.RecordUsage(event(k, "bill", *now)))
	close(s.captures)
	s.worker.Wait()
	must(t, s.db.Close())
	s.closed = true
	r, e := Open(Options{DataPath: s.path, Now: func() time.Time { return *now }, Pricer: p})
	must(t, e)
	fresh, _ := key(t, r, KeySpec{Enabled: true})
	must(t, r.Admit(Admission{RequestID: "fresh", KeyID: fresh.ID}))
	must(t, r.Complete("fresh", *now))
	_, _, e = r.RotateKey(k.ID)
	must(t, e)
	_, e = r.ResetKey(k.ID, []string{"five_hour"})
	must(t, e)
	if _, e = r.RecoverKey(k.ID, false); !errors.Is(e, ErrAccounting) {
		t.Fatal("implicit acknowledgement", e)
	}
	must(t, r.Close())
	r, e = Open(Options{DataPath: s.path, Now: func() time.Time { return *now }, Pricer: p})
	must(t, e)
	defer r.Close()
	if e = r.Admit(Admission{RequestID: "old", KeyID: k.ID}); !errors.Is(e, ErrAccounting) {
		t.Fatal("fence lost", e)
	}
	recovered, e := r.RecoverKey(k.ID, true)
	must(t, e)
	if recovered.Used.Total != 7 || recovered.Limits.Total != 100 || recovered.BlockedReason != "" {
		t.Fatal(recovered)
	}
	d, e := r.Detail("bill")
	must(t, e)
	if d.Record.CostMicroUSD != 7 {
		t.Fatal(d)
	}
	must(t, r.Admit(Admission{RequestID: "old", KeyID: k.ID}))
	must(t, r.Complete("old", *now))
	must(t, r.Close())
	r, e = Open(Options{DataPath: s.path, Now: func() time.Time { return *now }, Pricer: p})
	must(t, e)
	defer r.Close()
	must(t, r.Admit(Admission{RequestID: "after-recover-restart", KeyID: k.ID}))
	must(t, r.Complete("after-recover-restart", *now))
}

// Durable acknowledgement itself requires writable storage even if no retry
// remains, and an expired display view must not restart the stored anchor.
func TestRecoveryStorageAndEffectiveView(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	must(t, s.RecordUsage(event(k, "usage", *now)))
	s.fenceLocked(k.ID)
	s.writeFault = func() error { return ErrBusy }
	if _, e := s.RecoverKey(k.ID, true); !errors.Is(e, ErrAccounting) {
		t.Fatal(e)
	}
	s.writeFault = nil
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
	recovered, e := s.RecoverKey(k.ID, true)
	must(t, e)
	if recovered.Used.Total != 7 {
		t.Fatal(recovered)
	}
}

// A failing Key cannot starve unrelated retry/recovery. Pending events are never
// forgiven, and successful retries use their original quote, not current price.
func TestRetryKeyIsolation(t *testing.T) {
	s, now, p := fixture(t)
	a, _ := key(t, s, KeySpec{Enabled: true})
	b, _ := key(t, s, KeySpec{Enabled: true})
	s.writeFault = func() error { return ErrBusy }
	if s.RecordUsage(event(a, "a", *now)) != ErrAccounting || s.RecordUsage(event(b, "b", *now)) != ErrAccounting {
		t.Fatal("missing failures")
	}
	if _, e := s.RecoverKey(a.ID, true); !errors.Is(e, ErrAccounting) {
		t.Fatal(e)
	}
	s.writeFault = nil
	s.usageFault = func(p pending) error {
		if p.Record.KeyID == a.ID {
			return ErrBusy
		}
		return nil
	}
	p.cost = 999
	if e := s.Maintenance(); !errors.Is(e, ErrAccounting) {
		t.Fatal(e)
	}
	if s.blocked[b.ID] || !s.blocked[a.ID] || len(s.pending) != 1 {
		t.Fatal(s.blocked, s.pending)
	}
	_, e := s.RecoverKey(b.ID, false)
	must(t, e)
	if _, e = s.RecoverKey(a.ID, true); !errors.Is(e, ErrAccounting) {
		t.Fatal(e)
	}
	s.usageFault = nil
	must(t, s.Maintenance())
	d, e := s.Detail("b")
	must(t, e)
	if d.Record.CostMicroUSD != 7 || p.calls != 2 {
		t.Fatal(d, p.calls)
	}
}

// Failed Close releases the handle and durably marks only lost-debt principals;
// reopening cannot silently unpause them or poison an unrelated existing Key.
func TestCloseDebtScopedRestart(t *testing.T) {
	s, now, p := fixture(t)
	a, _ := key(t, s, KeySpec{Enabled: true})
	b, _ := key(t, s, KeySpec{Enabled: true})
	s.writeFault = func() error { return ErrBusy }
	if s.RecordUsage(event(a, "unsettled", *now)) != ErrAccounting {
		t.Fatal("failure hidden")
	}
	if !errors.Is(s.Close(), ErrAccounting) {
		t.Fatal("lost close error")
	}
	if _, e := s.Keys(); !errors.Is(e, ErrClosed) {
		t.Fatal("closed handle not explicit", e)
	}
	r, e := Open(Options{DataPath: s.path, Now: func() time.Time { return *now }, Pricer: p})
	must(t, e)
	defer r.Close()
	must(t, r.Admit(Admission{RequestID: "other", KeyID: b.ID}))
	must(t, r.Complete("other", *now))
	if _, e = r.RecoverKey(a.ID, false); !errors.Is(e, ErrAccounting) {
		t.Fatal("silent unpause", e)
	}
	k, e := r.RecoverKey(a.ID, true)
	must(t, e)
	if k.Used.Total != 0 {
		t.Fatal("recovery invented charges", k)
	}
}

// Durable monotonic cutoff prevents an expired execution from charging again,
// including clock rollback/restart. Old or unknown timing blocks only its Key.
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
	if r.RecordUsage(unknown) != ErrAccounting {
		t.Fatal("unknown time accepted")
	}
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

// Safe money maximum stays exact; unsafe quotes or accumulator sums atomically
// roll back and require per-Key acknowledgement, never a database-global fence.
func TestSafeMoneyOverflowScoped(t *testing.T) {
	s, now, p := fixture(t)
	a, _ := key(t, s, KeySpec{Enabled: true})
	b, _ := key(t, s, KeySpec{Enabled: true})
	p.cost = maxSafe
	must(t, s.RecordUsage(event(a, "max", *now)))
	p.cost = 1
	if e := s.RecordUsage(event(a, "overflow", *now)); !errors.Is(e, ErrAccounting) {
		t.Fatal(e)
	}
	if !s.unknown[a.ID] || s.unknown[b.ID] {
		t.Fatal(s.unknown)
	}
	d, e := s.Detail("max")
	must(t, e)
	if d.Record.CostMicroUSD != maxSafe {
		t.Fatal(d)
	}
	if _, e = s.Detail("overflow"); !errors.Is(e, ErrNotFound) {
		t.Fatal("partial transaction", e)
	}
	p.cost = maxSafe + 1
	if e = s.RecordUsage(event(b, "unsafe-quote", *now)); !errors.Is(e, ErrAccounting) {
		t.Fatal(e)
	}
	if len(s.pending) != 0 {
		t.Fatal("irrecoverable overflow retained")
	}
}
