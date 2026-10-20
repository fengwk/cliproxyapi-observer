package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	bolt "go.etcd.io/bbolt"
)

type fakePrice struct {
	cost  int64
	calls int
}

func (p *fakePrice) Quote(string, string, pricing.Tokens, time.Time) (pricing.Quote, error) {
	p.calls++
	return pricing.Quote{CostMicroUSD: p.cost, Source: "manual"}, nil
}
func (*fakePrice) Observe(string, string)         {}
func (*fakePrice) Configure(pricing.Config) error { return nil }
func (*fakePrice) Snapshot() pricing.Snapshot     { return pricing.Snapshot{} }
func (*fakePrice) Refresh(context.Context) error  { return nil }
func fixture(t *testing.T) (*Store, *time.Time, *fakePrice) {
	t.Helper()
	now := time.Date(2024, 1, 31, 12, 0, 0, 0, time.UTC)
	p := &fakePrice{cost: 7}
	s, e := Open(Options{DataPath: filepath.Join(t.TempDir(), "engine.db"), Now: func() time.Time { return now }, Pricer: p})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if !s.closed {
			if e := s.Close(); e != nil {
				t.Error(e)
			}
		}
	})
	return s, &now, p
}
func key(t *testing.T, s *Store, v KeySpec) (Key, string) {
	t.Helper()
	k, x, e := s.CreateKey(v)
	if e != nil {
		t.Fatal(e)
	}
	return k, x
}
func event(k Key, id string, at time.Time) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{RequestID: id, TraceID: "trace", APIKey: "observer:" + k.ID, Generate: true, Model: "gpt-exact", Provider: "openai", RequestedAt: at, Detail: pluginapi.UsageDetail{InputTokens: 8, OutputTokens: 2, TotalTokens: 10}}
}
func must(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}
func total(t *testing.T, s *Store) Key {
	t.Helper()
	ks, e := s.Keys()
	must(t, e)
	if len(ks) != 1 {
		t.Fatal(ks)
	}
	return ks[0]
}

// Plaintext is revealed only once; rotation/revocation invalidate old material,
// while expiry and model matching are exact rather than prefix authorization.
func TestKeySecurity(t *testing.T) {
	s, now, _ := fixture(t)
	expiry := now.Add(time.Hour)
	k, secret := key(t, s, KeySpec{Enabled: true, AllowedModels: []string{"gpt-exact"}, ExpiresAt: &expiry})
	_, e := s.Authenticate(secret)
	must(t, e)
	if !errors.Is(s.Authorize(k.ID, "gpt-exact-v2"), ErrForbidden) {
		t.Fatal("prefix accepted")
	}
	_, next, e := s.RotateKey(k.ID)
	must(t, e)
	if _, e = s.Authenticate(secret); !errors.Is(e, ErrUnauthorized) {
		t.Fatal(e)
	}
	_, e = s.Authenticate(next)
	must(t, e)
	raw, e := os.ReadFile(s.path)
	must(t, e)
	if bytes.Contains(raw, []byte(secret)) || bytes.Contains(raw, []byte(next)) {
		t.Fatal("plaintext secret on disk")
	}
	*now = expiry
	if _, e = s.Authenticate(next); !errors.Is(e, ErrExpired) {
		t.Fatal(e)
	}
	must(t, s.RevokeKey(k.ID))
	if _, e = s.Authenticate(next); !errors.Is(e, ErrUnauthorized) {
		t.Fatal(e)
	}
}

// All rolling anchors are UTC, month-end clamps including leap years, idle
// windows restart at admission, and explicit reset never resets total implicitly.
func TestCycles(t *testing.T) {
	for _, tc := range []struct{ at, want string }{{"2024-01-31T12:00:00Z", "2024-02-29T12:00:00Z"}, {"2023-01-31T12:00:00Z", "2023-02-28T12:00:00Z"}, {"2024-03-10T01:30:00-05:00", "2024-04-10T06:30:00Z"}} {
		a, _ := time.Parse(time.RFC3339, tc.at)
		w, _ := time.Parse(time.RFC3339, tc.want)
		if !month(a).Equal(w) {
			t.Fatal(month(a), w)
		}
	}
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	must(t, s.RecordUsage(event(k, "one", *now)))
	*now = now.AddDate(0, 2, 0)
	must(t, s.Admit(Admission{RequestID: "admit", KeyID: k.ID, Model: "m"}))
	must(t, s.Complete("admit", *now))
	k = total(t, s)
	if k.Used.FiveHour != 0 || k.Used.Week != 0 || k.Used.Month != 0 || k.Used.Total != 7 {
		t.Fatal(k)
	}
	if !k.Starts.Month.Equal(*now) || !k.Resets.Week.Equal(now.Add(7*24*time.Hour)) {
		t.Fatal(k)
	}
	k, e := s.ResetKey(k.ID, []string{"total"})
	must(t, e)
	if k.Used.Total != 0 || !k.Starts.Total.Equal(*now) {
		t.Fatal(k)
	}
}

// Distinct execution IDs sharing an admission trace each bill once; replay,
// count-tokens, and subsequent price changes cannot rewrite fixed old costs.
func TestExecutionsAndQueries(t *testing.T) {
	s, now, p := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	u := event(k, "exec-1", *now)
	must(t, s.RecordUsage(u))
	p.cost = 13
	must(t, s.RecordUsage(u))
	u.RequestID = "exec-2"
	must(t, s.RecordUsage(u))
	u.Generate = false
	u.RequestID = "count"
	must(t, s.RecordUsage(u))
	if total(t, s).Used.Total != 20 || p.calls != 2 {
		t.Fatal(total(t, s), p.calls)
	}
	q := Query{From: now.Add(-time.Minute), To: now.Add(time.Minute), PageSize: 1}
	page, e := s.Requests(q)
	must(t, e)
	if len(page.Items) != 1 || !page.HasMore {
		t.Fatal(page)
	}
	q.Page = 2
	page, e = s.Requests(q)
	must(t, e)
	if len(page.Items) != 1 || page.HasMore {
		t.Fatal(page)
	}
	summary, e := s.Summary(q)
	must(t, e)
	if summary.Requests != 2 || summary.CostMicroUSD != 20 {
		t.Fatal(summary)
	}
	q.To = *now
	page, e = s.Requests(q)
	must(t, e)
	if len(page.Items) != 0 {
		t.Fatal("upper bound not exclusive")
	}
}

// Failed writes block admission; reset cannot clear the gate, and retry keeps
// the original quote even when manual prices change before recovery.
func TestFailedWriteRetry(t *testing.T) {
	s, now, p := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	s.writeFault = func() error { return errors.New("injected write failure") }
	if !errors.Is(s.RecordUsage(event(k, "debt", *now)), ErrAccounting) {
		t.Fatal("failure hidden")
	}
	if !errors.Is(s.Admit(Admission{RequestID: "x", KeyID: k.ID}), ErrAccounting) {
		t.Fatal("unblocked")
	}
	s.writeFault = nil
	var e error
	_, e = s.ResetKey(k.ID, []string{"five_hour"})
	must(t, e)
	if !errors.Is(s.Admit(Admission{RequestID: "x", KeyID: k.ID}), ErrAccounting) {
		t.Fatal("reset removed gate")
	}
	p.cost = 99
	must(t, s.Maintenance())
	if total(t, s).Used.Total != 7 || total(t, s).Used.FiveHour != 0 || p.calls != 1 {
		t.Fatal(total(t, s), p.calls)
	}
}

// Simulated abrupt termination leaves a durable running marker. On restart a
// reset/rotate cannot authorize new inference against unknown unsettled debt.
func TestUncleanRestart(t *testing.T) {
	s, _, p := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	close(s.captures)
	s.worker.Wait()
	must(t, s.db.Close())
	s.closed = true
	reopened, e := Open(Options{DataPath: s.path, Pricer: p})
	must(t, e)
	_, e = reopened.ResetKey(k.ID, []string{"total"})
	must(t, e)
	if !errors.Is(reopened.Admit(Admission{RequestID: "new", KeyID: k.ID}), ErrAccounting) {
		t.Fatal("unsafe restart")
	}
	must(t, reopened.Close())
}

// Concurrent admissions serialize the limit check, idempotent completion frees
// exactly one slot, and duplicate admission never reserves another slot.
func TestConcurrency(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true, MaxConcurrent: 1})
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := []string{}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprint(i)
			e := s.Admit(Admission{RequestID: id, KeyID: k.ID, Model: "m"})
			if e == nil {
				mu.Lock()
				winners = append(winners, id)
				mu.Unlock()
			} else if !errors.Is(e, ErrConcurrency) {
				t.Error(e)
			}
		}(i)
	}
	wg.Wait()
	if len(winners) != 1 {
		t.Fatal(winners)
	}
	id := winners[0]
	must(t, s.Admit(Admission{RequestID: id, KeyID: k.ID, Model: "m"}))
	must(t, s.Complete(id, *now))
	must(t, s.Complete(id, *now))
	must(t, s.Admit(Admission{RequestID: "new", KeyID: k.ID}))
	must(t, s.Complete("new", *now))
}

// Drain synchronization (not sleeps) proves gzip capture, recursive redaction,
// cross-principal isolation and expiry enforcement before maintenance runs.
func TestCaptureAndExpiry(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	v, _ := s.Settings()
	v.CaptureEnabled = true
	_, e := s.UpdateSettings(v)
	must(t, e)
	if !s.Capture(CaptureInput{RequestID: "admission", TraceID: "trace", KeyID: k.ID, Body: []byte(`{"nested":{"api_key":"fake-secret"},"ok":1}`), Headers: http.Header{"Authorization": {"fake-bearer"}, "Content-Type": {"application/json"}}}) {
		t.Fatal("capture rejected")
	}
	close(s.captures)
	s.worker.Wait()
	s.captures = make(chan CaptureInput, 64)
	s.worker.Add(1)
	go s.captureWorker()
	must(t, s.RecordUsage(event(k, "exec", *now)))
	d, e := s.Detail("exec")
	must(t, e)
	if bytes.Contains(d.Body, []byte("fake-secret")) || d.Headers.Get("Authorization") != "" || d.CaptureStatus != "redacted" {
		t.Fatal(d)
	}
	var obj any
	must(t, json.Unmarshal(d.Body, &obj))
	*now = now.Add(25 * time.Hour)
	if _, e = s.Detail("exec"); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
}

// Retention removes raw rows and rollups, never independent quota counters;
// historical partial-day queries fail rather than silently approximating.
func TestRetention(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	must(t, s.RecordUsage(event(k, "old", *now)))
	original := *now
	*now = now.Add(48 * time.Hour)
	must(t, s.Maintenance())
	if total(t, s).Used.Total != 7 {
		t.Fatal("usage pruned")
	}
	summary, e := s.Summary(Query{From: day(original), To: day(original).Add(24 * time.Hour)})
	must(t, e)
	if summary.Requests != 1 {
		t.Fatal(summary)
	}
	_, e = s.Summary(Query{From: original, To: original.Add(time.Hour)})
	if !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
	*now = original.AddDate(2, 0, 0)
	must(t, s.Maintenance())
	if total(t, s).Used.Total != 7 {
		t.Fatal("daily pruning erased independent totals")
	}
	summary, e = s.Summary(Query{From: day(original), To: day(original).Add(24 * time.Hour)})
	must(t, e)
	if summary.Requests != 0 {
		t.Fatal("daily row not pruned")
	}
}

// Foreign databases must remain byte-identical: no schema/freelist writes.
func TestForeignDatabasePreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreign.db")
	db, e := bolt.Open(path, 0600, nil)
	must(t, e)
	must(t, db.Update(func(tx *bolt.Tx) error { _, e := tx.CreateBucket([]byte("legacy")); return e }))
	must(t, db.Close())
	before, e := os.ReadFile(path)
	must(t, e)
	_, e = Open(Options{DataPath: path, Pricer: &fakePrice{}})
	if !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
	after, e := os.ReadFile(path)
	must(t, e)
	if !bytes.Equal(before, after) {
		t.Fatal("foreign DB modified")
	}
}

// An actual large bbolt fixture verifies physical shrink and continued writes
// through the swapped handle, not merely logical deletion or free-page counts.
func TestPhysicalCompaction(t *testing.T) {
	s, _, _ := fixture(t)
	must(t, s.db.Update(func(tx *bolt.Tx) error {
		b, e := tx.CreateBucket([]byte("fixture"))
		if e != nil {
			return e
		}
		for i := 0; i < 128; i++ {
			if e = b.Put([]byte(fmt.Sprint(i)), bytes.Repeat([]byte("x"), 128<<10)); e != nil {
				return e
			}
		}
		return nil
	}))
	must(t, s.db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte("fixture")) }))
	before, e := os.Stat(s.path)
	must(t, e)
	must(t, s.Compact())
	after, e := os.Stat(s.path)
	must(t, e)
	if after.Size() >= before.Size()/2 {
		t.Fatal(before.Size(), after.Size())
	}
	key(t, s, KeySpec{Enabled: true})
	_, e = s.Keys()
	must(t, e)
}

// Every finite cycle rejects at equality, while automatic rollover affects
// only its own accumulator. Total never rolls, even after long idle periods.
func TestQuotaBoundaries(t *testing.T) {
	for _, window := range []string{"five_hour", "week", "month", "total"} {
		t.Run(window, func(t *testing.T) {
			s, now, _ := fixture(t)
			limits := Limits{}
			switch window {
			case "five_hour":
				limits.FiveHour = 7
			case "week":
				limits.Week = 7
			case "month":
				limits.Month = 7
			case "total":
				limits.Total = 7
			}
			k, _ := key(t, s, KeySpec{Enabled: true, Limits: limits})
			must(t, s.RecordUsage(event(k, "at-limit", *now)))
			if !errors.Is(s.Admit(Admission{RequestID: "blocked", KeyID: k.ID}), ErrQuota) {
				t.Fatal("quota equality admitted")
			}
			*now = now.AddDate(1, 0, 0)
			e := s.Admit(Admission{RequestID: "later", KeyID: k.ID})
			if window == "total" {
				if !errors.Is(e, ErrQuota) {
					t.Fatal(e)
				}
			} else {
				must(t, e)
				must(t, s.Complete("later", *now))
			}
		})
	}
}

// A retained old quote does not refill an explicitly reset window; the new
// execution still charges using current receipt time, not RequestedAt.
func TestResetDoesNotRefillOldWindow(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	s.writeFault = func() error { return ErrBusy }
	if !errors.Is(s.RecordUsage(event(k, "before", *now)), ErrAccounting) {
		t.Fatal("write succeeded")
	}
	*now = now.Add(time.Second)
	_, e := s.ResetKey(k.ID, []string{"five_hour", "week", "month", "total"})
	must(t, e)
	s.writeFault = nil
	must(t, s.Maintenance())
	if total(t, s).Used.Total != 0 {
		t.Fatal("old event refilled reset")
	}
	must(t, s.RecordUsage(event(k, "late-historical", now.Add(-6*24*time.Hour))))
	if total(t, s).Used.Total != 7 {
		t.Fatal("late execution not billed at receipt")
	}
	if e = s.RecordUsage(event(k, "unsupported-90-day-callback", now.Add(-90*24*time.Hour))); !errors.Is(e, ErrAccounting) {
		t.Fatal("callback outside the documented replay horizon accepted", e)
	}
	if total(t, s).Used.Total != 7 {
		t.Fatal("rejected callback charged")
	}
}

// The failure queue has a hard bound. Losing the ability to retain another
// event must create an irreversible fence rather than silently unblocking.
func TestPendingBoundAndCloseDebt(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	s.writeFault = func() error { return ErrBusy }
	for i := 0; i < 1025; i++ {
		if !errors.Is(s.RecordUsage(event(k, fmt.Sprint(i), *now)), ErrAccounting) {
			t.Fatal(i)
		}
	}
	if s.Health().PendingUsage != 1024 || !s.unknown[k.ID] {
		t.Fatal(s.Health())
	}
	if !errors.Is(s.Close(), ErrAccounting) {
		t.Fatal("close discarded debt")
	}
}

// Unknown pricing is a valid zero-cost quote, not an inference gate. Unknown
// provider token overlap must never invent billable canonical input buckets.
func TestUnknownPricingAndTokens(t *testing.T) {
	s, now, p := fixture(t)
	p.cost = 0
	k, _ := key(t, s, KeySpec{Enabled: true})
	u := event(k, "unknown", *now)
	u.Provider = "unknown-provider"
	u.Detail.CacheReadTokens = 4
	must(t, s.RecordUsage(u))
	d, e := s.Detail(u.RequestID)
	must(t, e)
	if d.Record.CostMicroUSD != 0 || d.Record.Tokens.Input != 0 {
		t.Fatal(d)
	}
	must(t, s.Admit(Admission{RequestID: "allowed", KeyID: k.ID}))
	must(t, s.Complete("allowed", *now))
}

// Bounded capture fails immediately under lock contention, keeps headers for an
// oversized body, and reports body size, storage and lock drops separately.
func TestCaptureBounds(t *testing.T) {
	s, _, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	settings, _ := s.Settings()
	settings.CaptureEnabled = true
	settings.MaxBodyBytes = 16
	settings.MaxBodyStorageBytes = 1
	_, e := s.UpdateSettings(settings)
	must(t, e)
	s.mu.Lock()
	accepted := s.Capture(CaptureInput{RequestID: "admission", KeyID: k.ID, TraceID: "t", Body: []byte("{}")})
	s.mu.Unlock()
	if accepted {
		t.Fatal("contended capture accepted")
	}
	if !s.Capture(CaptureInput{RequestID: "admission", KeyID: k.ID, TraceID: "t", Body: bytes.Repeat([]byte("x"), 17)}) {
		t.Fatal("oversized body queue rejected")
	}
	drainCapture(s) // nonblocking Capture may legitimately drop during worker I/O
	if !s.Capture(CaptureInput{RequestID: "admission", KeyID: k.ID, TraceID: "t", Body: []byte("{}")}) {
		t.Fatal("bounded input rejected")
	}
	close(s.captures)
	s.worker.Wait()
	s.captures = make(chan CaptureInput, 64)
	s.worker.Add(1)
	go s.captureWorker()
	health := s.Health()
	if health.DroppedCaptures != 4 || health.CaptureDrops["lock_contention"] != 1 || health.CaptureDrops["too_large"] != 1 || health.CaptureDrops["storage_limit"] != 2 {
		t.Fatal(health)
	}
}

// Clean shutdown persists counters without introducing a restart fence and is
// idempotent; APIs and diagnostic enqueueing reject use after close.
func TestCleanCloseRestart(t *testing.T) {
	s, now, p := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	must(t, s.RecordUsage(event(k, "saved", *now)))
	must(t, s.Close())
	must(t, s.Close())
	if _, e := s.Keys(); !errors.Is(e, ErrClosed) {
		t.Fatal(e)
	}
	if s.Capture(CaptureInput{RequestID: "admission", KeyID: k.ID, TraceID: "t"}) {
		t.Fatal("capture after close")
	}
	reopened, e := Open(Options{DataPath: s.path, Pricer: p})
	must(t, e)
	defer reopened.Close()
	if total(t, reopened).Used.Total != 7 {
		t.Fatal("lost usage")
	}
	must(t, reopened.Admit(Admission{RequestID: "new", KeyID: k.ID}))
	must(t, reopened.Complete("new", *now))
}

// A shared short trace can correlate multiple executions of one admission,
// but never discloses another Key's content or an ambiguous second admission.
func TestCapturePrincipalAndTraceCollision(t *testing.T) {
	s, now, _ := fixture(t)
	a, _ := key(t, s, KeySpec{Enabled: true})
	b, _ := key(t, s, KeySpec{Enabled: true})
	settings, _ := s.Settings()
	settings.CaptureEnabled = true
	_, e := s.UpdateSettings(settings)
	must(t, e)
	must(t, s.Admit(Admission{RequestID: "admission-a", TraceID: "trace", KeyID: a.ID}))
	if !s.Capture(CaptureInput{RequestID: "admission-a", TraceID: "trace", KeyID: a.ID, Body: []byte(`{"message":"private-a"}`)}) {
		t.Fatal("capture rejected")
	}
	close(s.captures)
	s.worker.Wait()
	s.captures = make(chan CaptureInput, 64)
	s.worker.Add(1)
	go s.captureWorker()
	must(t, s.RecordUsage(event(a, "a-exec", *now)))
	must(t, s.RecordUsage(event(b, "b-exec", *now)))
	d, e := s.Detail("b-exec")
	must(t, e)
	if d.CaptureStatus != "missing" || bytes.Contains(d.Body, []byte("private-a")) {
		t.Fatal(d)
	}
	d, e = s.Detail("a-exec")
	must(t, e)
	if !bytes.Contains(d.Body, []byte("private-a")) {
		t.Fatal(d)
	}
	must(t, s.Admit(Admission{RequestID: "admission-collision", TraceID: "trace", KeyID: a.ID}))
	d, e = s.Detail("a-exec")
	must(t, e)
	if d.CaptureStatus != "missing" {
		t.Fatal("collision trusted", d)
	}
	must(t, s.Complete("admission-a", *now))
	must(t, s.Complete("admission-collision", *now))
}

// A failed atomic replacement leaves the primary handle usable; operations
// never revert to or reuse an invalid database pointer.
func TestCompactFailurePreservesHandle(t *testing.T) {
	s, _, _ := fixture(t)
	key(t, s, KeySpec{Enabled: true})
	path := s.path
	old := s.db
	s.path = t.TempDir() // rename of a file over a directory must fail
	if e := s.Compact(); e == nil {
		t.Fatal("replacement unexpectedly succeeded")
	}
	s.path = path
	if s.db != old {
		t.Fatal("handle replaced on failure")
	}
	if _, e := s.Keys(); e != nil {
		t.Fatal("primary lost", e)
	}
}

// Concurrent shutdown callers all wait for the same diagnostic drain and see
// the same result, instead of returning before the database is released.
func TestConcurrentClose(t *testing.T) {
	s, _, _ := fixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := s.Close(); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
}

// A recent exact query may start after the moving raw cutoff even if midnight
// precedes it; whole days with expired rollups can still use retained raw data.
func TestSummaryRawCoverage(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	settings, _ := s.Settings()
	settings.RawRetentionHours = 168
	settings.DailyRetentionDays = 1
	_, e := s.UpdateSettings(settings)
	must(t, e)
	original := *now
	must(t, s.RecordUsage(event(k, "old", original)))
	*now = now.Add(3 * 24 * time.Hour)
	must(t, s.Maintenance())
	summary, e := s.Summary(Query{From: day(original), To: day(original).Add(24 * time.Hour)})
	must(t, e)
	if summary.Requests != 1 {
		t.Fatal(summary)
	}
	settings.RawRetentionHours = 24
	_, e = s.UpdateSettings(settings)
	must(t, e)
	cut := now.Add(-24 * time.Hour)
	must(t, s.RecordUsage(event(k, "recent", cut.Add(time.Hour))))
	summary, e = s.Summary(Query{From: cut.Add(time.Minute), To: cut.Add(2 * time.Hour)})
	must(t, e)
	if summary.Requests != 1 {
		t.Fatal(summary)
	}
}

// A recently arriving usage row must not revive old captured content. Expiry
// is enforced on read even when maintenance has not deleted the gzip payload.
func TestCaptureExpiresBeforeCleanup(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	settings, _ := s.Settings()
	settings.CaptureEnabled = true
	_, e := s.UpdateSettings(settings)
	must(t, e)
	if !s.Capture(CaptureInput{RequestID: "old-admission", TraceID: "trace", KeyID: k.ID, At: now.Add(-25 * time.Hour), Body: []byte(`{"safe":1}`)}) {
		t.Fatal("capture rejected")
	}
	close(s.captures)
	s.worker.Wait()
	s.captures = make(chan CaptureInput, 64)
	s.worker.Add(1)
	go s.captureWorker()
	must(t, s.RecordUsage(event(k, "new-row", *now)))
	d, e := s.Detail("new-row")
	must(t, e)
	if d.CaptureStatus != "expired" || string(d.Body) != "null" {
		t.Fatal(d)
	}
}

// Request and daily queries must apply the same full dimension predicate.
// Upstream failure bodies never enter persisted metadata or diagnostic content.
func TestFilterDimensionsAndFailureSafety(t *testing.T) {
	s, now, _ := fixture(t)
	a, _ := key(t, s, KeySpec{Enabled: true})
	b, _ := key(t, s, KeySpec{Enabled: true})
	u := event(a, "selected", *now)
	u.AuthIndex = "auth"
	u.Failed = true
	u.Failure = pluginapi.UsageFailure{StatusCode: 502, Body: "fake-private-upstream-failure"}
	must(t, s.RecordUsage(u))
	u = event(b, "other", *now)
	must(t, s.RecordUsage(u))
	q := Query{From: day(*now), To: day(*now).Add(24 * time.Hour), KeyID: a.ID, Model: "gpt-exact", Provider: "openai", AuthIndex: "auth", Status: "failed"}
	page, e := s.Requests(q)
	must(t, e)
	if len(page.Items) != 1 || page.Items[0].ID != "selected" {
		t.Fatal(page)
	}
	summary, e := s.Summary(q)
	must(t, e)
	if summary.Requests != 1 || summary.Failures != 1 || summary.CostMicroUSD != 7 {
		t.Fatal(summary)
	}
	raw, e := os.ReadFile(s.path)
	must(t, e)
	if bytes.Contains(raw, []byte("fake-private-upstream-failure")) {
		t.Fatal("failure body persisted")
	}
}

// Known SDK semantics produce disjoint uncached/cache buckets while retaining
// reasoning as an output subset and preserving the original raw SDK counters.
func TestCanonicalSDKTokens(t *testing.T) {
	u := pluginapi.UsageRecord{Provider: "openai", Detail: pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 3, ReasoningTokens: 1, CacheReadTokens: 4, TotalTokens: 13}}
	r := normalize(u)
	if r.Tokens.Input != 6 || r.Tokens.CacheRead != 4 || r.Tokens.Output != 3 || r.Tokens.Reasoning != 1 || r.Tokens.Total != 13 || r.RawUsage != u.Detail {
		t.Fatal(r)
	}
}
