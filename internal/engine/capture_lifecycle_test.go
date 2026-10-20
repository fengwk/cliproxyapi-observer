package engine

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	bolt "go.etcd.io/bbolt"
)

func configureCapture(t *testing.T, s *Store, mode string, apply func(*Settings)) Settings {
	t.Helper()
	v, e := s.Settings()
	must(t, e)
	v.CaptureMode = mode
	v.CaptureEnabled = mode != CaptureModeOff
	if apply != nil {
		apply(&v)
	}
	v, e = s.UpdateSettings(v)
	must(t, e)
	return v
}

func usageFor(k Key, id, trace string, at time.Time) pluginapi.UsageRecord {
	u := event(k, id, at)
	u.TraceID = trace
	return u
}

func storedCapture(t *testing.T, s *Store, key, trace string) (capture, []byte) {
	t.Helper()
	var c capture
	var raw []byte
	must(t, s.db.View(func(tx *bolt.Tx) error {
		raw = append([]byte(nil), tx.Bucket([]byte("captures")).Get([]byte(captureID(key, trace)))...)
		if len(raw) == 0 {
			return ErrNotFound
		}
		var e error
		c, e = decodeCapture(raw)
		return e
	}))
	return c, raw
}

// New content is one gzip envelope with a version and the redacted JSON body;
// real credentials never reach disk while ordinary max_tokens survives. The
// caller's buffers are never mutated.
func TestCaptureEnvelopeSingleGzipAndRedaction(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	configureCapture(t, s, CaptureModeHeadersAndBody, nil)
	body := []byte(`{"max_tokens":123,"api_key":"fake-secret","url":"https://example.test/v1?token=fake-url-secret&max_tokens=7","nested":{"password":"fake-password"}}`)
	input := append([]byte(nil), body...)
	headers := http.Header{"Authorization": {"fake-bearer"}, "X-Api-Key": {"fake-header-key"}, "Cookie": {"fake-cookie"}, "Content-Type": {"application/json"}, "User-Agent": {"test-agent"}}
	if !s.Capture(CaptureInput{RequestID: "admission", TraceID: "trace", KeyID: k.ID, Body: body, Headers: headers, At: *now}) {
		t.Fatal("capture rejected")
	}
	drainCapture(s)
	if !bytes.Equal(body, input) || headers.Get("Authorization") != "fake-bearer" {
		t.Fatal("capture mutated caller buffers")
	}
	must(t, s.RecordUsage(usageFor(k, "exec", "trace", *now)))
	d, e := s.Detail("exec")
	must(t, e)
	if d.HeadersStatus != captureStatusAvailable || d.BodyStatus != captureStatusRedacted || d.CaptureStatus != captureStatusRedacted {
		t.Fatal(d)
	}
	if d.Headers.Get("Authorization") != "" || d.Headers.Get("Content-Type") != "application/json" {
		t.Fatal("unsafe header persisted", d.Headers)
	}
	for _, secret := range []string{"fake-secret", "fake-url-secret", "fake-password"} {
		if bytes.Contains(d.Body, []byte(secret)) {
			t.Fatal("credential survived redaction", string(d.Body))
		}
	}
	var obj map[string]any
	must(t, json.Unmarshal(d.Body, &obj))
	if obj["max_tokens"] != float64(123) {
		t.Fatal("max_tokens was redacted", obj)
	}
	c, raw := storedCapture(t, s, k.ID, "trace")
	if c.Version != captureEnvelopeVersion || len(c.Gzip) != 0 || len(c.Body) == 0 {
		t.Fatalf("not a single-version envelope: %+v", c)
	}
	if !bytes.HasPrefix(raw, []byte{0x1f, 0x8b}) {
		t.Fatal("capture is not gzip")
	}
	z, e := gzip.NewReader(bytes.NewReader(raw))
	must(t, e)
	plain, e := io.ReadAll(z)
	must(t, e)
	z.Close()
	if bytes.Contains(plain, []byte("fake-secret")) || bytes.Contains(plain, []byte("fake-url-secret")) {
		t.Fatal("credential present in decompressed envelope")
	}
	if !bytes.Contains(plain, []byte(`"Version":1`)) || !bytes.Contains(plain, []byte(`"max_tokens":123`)) {
		t.Fatal("envelope lost version or ordinary parameter", string(plain))
	}
	if d.OriginalBytes != int64(len(input)) || d.CompressedBytes != int64(len(raw)) || d.CapturedAt == nil || d.ExpiresAt == nil {
		t.Fatal("sizes/times unavailable", d)
	}
}

// Old schema2 rows compressed the body separately from the JSON envelope. They
// remain readable after upgrade without rewriting the database.
func TestCaptureLegacyDoubleEnvelopeReadable(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	configureCapture(t, s, CaptureModeHeadersAndBody, nil)
	must(t, s.RecordUsage(usageFor(k, "exec", "trace", *now)))
	innerObj := map[string]any{"ok": float64(1), "message": "legacy"}
	innerJSON, e := json.Marshal(innerObj)
	must(t, e)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, e = zw.Write(innerJSON)
	must(t, e)
	must(t, zw.Close())
	legacy := capture{RequestID: "admission", At: *now, Headers: http.Header{"Content-Type": {"application/json"}}, Gzip: gz.Bytes(), Status: captureStatusRedacted}
	payload, e := encode(legacy)
	must(t, e)
	must(t, s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("captures")).Put([]byte(captureID(k.ID, "trace")), payload)
	}))
	d, e := s.Detail("exec")
	must(t, e)
	if d.CaptureStatus != captureStatusRedacted || !bytes.Contains(d.Body, []byte("legacy")) || string(d.Body) == "null" {
		t.Fatal(d)
	}
}

// Compression pressure evicts the oldest capture by capture time, not by ID
// order, and lowering the budget trims existing content immediately.
func TestCaptureFIFOEvictionAndBudget(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	configureCapture(t, s, CaptureModeHeadersAndBody, nil)
	body := []byte(`{"payload":"` + strings.Repeat("x", 512) + `"}`)
	if !s.Capture(CaptureInput{RequestID: "a", TraceID: "first", KeyID: k.ID, Body: body, At: *now}) {
		t.Fatal("first capture rejected")
	}
	drainCapture(s)
	must(t, s.RecordUsage(usageFor(k, "a-exec", "first", *now)))
	used := s.Health().CaptureBytes
	if used <= 0 {
		t.Fatal("capture bytes not counted")
	}
	configureCapture(t, s, CaptureModeHeadersAndBody, func(v *Settings) { v.MaxBodyStorageBytes = used + 5 })
	*now = now.Add(time.Second)
	if !s.Capture(CaptureInput{RequestID: "b", TraceID: "second", KeyID: k.ID, Body: body, At: *now}) {
		t.Fatal("second capture rejected")
	}
	drainCapture(s)
	must(t, s.RecordUsage(usageFor(k, "b-exec", "second", *now)))
	h := s.Health()
	if h.CaptureBytes > h.CaptureLimitBytes || h.CaptureBytes <= 0 {
		t.Fatal("budget exceeded", h)
	}
	first, e := s.Detail("a-exec")
	must(t, e)
	if first.CaptureStatus != captureStatusEvicted || first.CaptureReason != captureReasonCapacity || string(first.Body) != "null" || h.CaptureDrops["evicted"] < 1 {
		t.Fatal("oldest was not evicted", first, h)
	}
	if first.CapturedAt == nil || first.ExpiresAt == nil || first.OriginalBytes == 0 || first.CompressedBytes == 0 {
		t.Fatal("evicted status lost time/size metadata", first)
	}
	second, e := s.Detail("b-exec")
	must(t, e)
	if second.BodyStatus != captureStatusAvailable {
		t.Fatal("newest missing", second)
	}
	*now = now.Add(time.Second)
	if !s.Capture(CaptureInput{RequestID: "c", TraceID: "third", KeyID: k.ID, Body: body, At: *now}) {
		t.Fatal("third capture rejected")
	}
	drainCapture(s)
	must(t, s.RecordUsage(usageFor(k, "c-exec", "third", *now)))
	second, e = s.Detail("b-exec")
	must(t, e)
	if second.CaptureStatus != captureStatusEvicted {
		t.Fatal("FIFO order ignored", second)
	}
	third, e := s.Detail("c-exec")
	must(t, e)
	if third.BodyStatus != captureStatusAvailable {
		t.Fatal(third)
	}
}

// One compressed object larger than the entire budget is rejected with a
// specific reason and does not disturb the byte counter.
func TestCaptureSingleObjectLargerThanBudget(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	configureCapture(t, s, CaptureModeHeadersAndBody, func(v *Settings) { v.MaxBodyStorageBytes = 1 })
	if !s.Capture(CaptureInput{RequestID: "a", TraceID: "trace", KeyID: k.ID, Body: []byte(`{"ok":1}`), At: *now}) {
		t.Fatal("capture rejected before policy check")
	}
	drainCapture(s)
	must(t, s.RecordUsage(usageFor(k, "exec", "trace", *now)))
	d, e := s.Detail("exec")
	must(t, e)
	if d.CaptureStatus != captureStatusTooLarge || d.BodyStatus != captureStatusTooLarge || d.CaptureReason != captureReasonStorageLimit || string(d.Body) != "null" {
		t.Fatal(d)
	}
	h := s.Health()
	if h.CaptureBytes != 0 || h.CaptureDrops["storage_limit"] != 1 {
		t.Fatal(h)
	}
}

// Expiry is enforced on read without waiting for maintenance, cleanup advances
// only content counters, and a later retention extension cannot revive content
// that already expired under a shorter policy.
func TestCaptureTTLReadMaintenanceAndNoRevive(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	configureCapture(t, s, CaptureModeHeadersAndBody, func(v *Settings) { v.CaptureRetentionSeconds = 60 })
	if !s.Capture(CaptureInput{RequestID: "a", TraceID: "trace", KeyID: k.ID, Body: []byte(`{"ok":1}`), At: *now}) {
		t.Fatal("capture rejected")
	}
	drainCapture(s)
	must(t, s.RecordUsage(usageFor(k, "exec", "trace", *now)))
	*now = now.Add(30 * time.Second)
	d, e := s.Detail("exec")
	must(t, e)
	if d.BodyStatus == captureStatusExpired {
		t.Fatal("expired too early", d)
	}
	*now = now.Add(31 * time.Second)
	d, e = s.Detail("exec")
	must(t, e)
	if d.CaptureStatus != captureStatusExpired || d.CaptureReason != captureReasonExpired || string(d.Body) != "null" {
		t.Fatal("read-time expiry missing", d)
	}
	before := total(t, s)
	result, e := s.CleanupCaptures()
	must(t, e)
	if result.RemovedRecords != 1 || result.RemovedBytes <= 0 || s.Health().CaptureBytes != 0 {
		t.Fatal(result, s.Health())
	}
	if after := total(t, s); after.Used.Total != before.Used.Total {
		t.Fatal("cleanup changed accounting", before, after)
	}
	summary, e := s.Summary(Query{From: day(*now), To: day(*now).Add(24 * time.Hour)})
	must(t, e)
	if summary.Requests != 1 {
		t.Fatal("cleanup changed history", summary)
	}
	configureCapture(t, s, CaptureModeHeadersAndBody, func(v *Settings) { v.CaptureRetentionSeconds = 3600 })
	d, e = s.Detail("exec")
	must(t, e)
	if d.CaptureStatus != captureStatusExpired {
		t.Fatal("policy extension revived expired content", d)
	}

	// A shorter policy tightens stored expiry before cleanup; extending it back
	// still must not make the content readable again.
	configureCapture(t, s, CaptureModeHeadersAndBody, func(v *Settings) { v.CaptureRetentionSeconds = 3600 })
	*now = now.Add(time.Minute)
	if !s.Capture(CaptureInput{RequestID: "b", TraceID: "trace-b", KeyID: k.ID, Body: []byte(`{"ok":2}`), At: *now}) {
		t.Fatal("capture rejected")
	}
	drainCapture(s)
	must(t, s.RecordUsage(usageFor(k, "exec-b", "trace-b", *now)))
	*now = now.Add(2 * time.Minute)
	configureCapture(t, s, CaptureModeHeadersAndBody, func(v *Settings) { v.CaptureRetentionSeconds = 60 })
	d, e = s.Detail("exec-b")
	must(t, e)
	if d.CaptureStatus != captureStatusExpired {
		t.Fatal("shortened policy not enforced", d)
	}
	configureCapture(t, s, CaptureModeHeadersAndBody, func(v *Settings) { v.CaptureRetentionSeconds = 3600 })
	d, e = s.Detail("exec-b")
	must(t, e)
	if d.CaptureStatus != captureStatusExpired {
		t.Fatal("extension revived tightened content", d)
	}
	health := s.Health()
	if health.LastCleanupAt == nil || health.LastCleanupRecords < 1 || health.LastCleanupBytes <= 0 {
		t.Fatal("cleanup result not visible", health)
	}
}

// Purge is transactional, leaves accounting untouched, and an old queued job
// cannot write content back after the generation advanced.
func TestCapturePurgeGenerationAndAccounting(t *testing.T) {
	s, now, p := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	configureCapture(t, s, CaptureModeHeadersAndBody, nil)
	if !s.Capture(CaptureInput{RequestID: "a", TraceID: "trace", KeyID: k.ID, Body: []byte(`{"ok":1}`), At: *now}) {
		t.Fatal("capture rejected")
	}
	drainCapture(s)
	must(t, s.RecordUsage(usageFor(k, "exec", "trace", *now)))
	if s.Health().CaptureBytes == 0 {
		t.Fatal("capture missing")
	}
	before := total(t, s)
	result, e := s.PurgeCaptures()
	must(t, e)
	if result.RemovedRecords != 1 || result.RemovedBytes <= 0 || s.Health().CaptureBytes != 0 {
		t.Fatal(result, s.Health())
	}
	if after := total(t, s); after.Used.Total != before.Used.Total {
		t.Fatal("purge changed accounting")
	}
	d, e := s.Detail("exec")
	must(t, e)
	if d.CaptureStatus != captureStatusPurged || string(d.Body) != "null" {
		t.Fatal("purge status missing", d)
	}
	// Simulate a worker that dequeued before purge and writes afterwards.
	s.mu.Lock()
	oldGeneration := s.captureGeneration - 1
	s.mu.Unlock()
	s.captures <- CaptureInput{RequestID: "a", TraceID: "trace", KeyID: k.ID, Body: []byte(`{"resurrected":true}`), At: *now, mode: CaptureModeHeadersAndBody, generation: oldGeneration}
	drainCapture(s)
	if s.Health().CaptureBytes != 0 {
		t.Fatal("old generation wrote content back")
	}
	d, e = s.Detail("exec")
	must(t, e)
	if d.CaptureStatus != captureStatusPurged {
		t.Fatal("old queue overwrote purge state", d)
	}
	again, e := s.PurgeCaptures()
	must(t, e)
	if again.RemovedRecords != 0 || again.RemovedBytes != 0 {
		t.Fatal("purge not idempotent", again)
	}
	generation := s.captureGeneration
	path := s.path
	must(t, s.Close())
	reopened, e := Open(Options{DataPath: path, Now: func() time.Time { return *now }, Pricer: p})
	must(t, e)
	defer reopened.Close()
	if reopened.captureGeneration != generation {
		t.Fatal("purge generation not durable", reopened.captureGeneration, generation)
	}
	d, e = reopened.Detail("exec")
	must(t, e)
	if d.CaptureStatus != captureStatusPurged || reopened.Health().CaptureBytes != 0 {
		t.Fatal("purged state lost on restart", d, reopened.Health())
	}
}

// Modes independently control headers and body. Active policy is enforced at
// read time: off hides history, headers never returns a previously stored body.
func TestCaptureModesAndIndependentStatuses(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	if s.Capture(CaptureInput{RequestID: "off", TraceID: "off", KeyID: k.ID, Body: []byte(`{"ok":1}`), At: *now}) {
		t.Fatal("off accepted")
	}

	configureCapture(t, s, CaptureModeHeaders, nil)
	if !s.Capture(CaptureInput{RequestID: "h", TraceID: "headers", KeyID: k.ID, Body: []byte(`{"secret":"not-json-body"}`), Headers: http.Header{"Accept": {"application/json"}}, At: *now}) {
		t.Fatal("headers capture rejected")
	}
	drainCapture(s)
	must(t, s.RecordUsage(usageFor(k, "headers-exec", "headers", *now)))
	d, e := s.Detail("headers-exec")
	must(t, e)
	if d.HeadersStatus != captureStatusAvailable || d.BodyStatus != captureStatusDisabled || string(d.Body) != "null" || d.Headers.Get("Accept") == "" {
		t.Fatal(d)
	}
	if _, raw := storedCapture(t, s, k.ID, "headers"); len(raw) == 0 {
		t.Fatal("header content missing")
	}

	configureCapture(t, s, CaptureModeHeadersAndBody, nil)
	if !s.Capture(CaptureInput{RequestID: "b", TraceID: "body", KeyID: k.ID, Body: []byte(`{"ok":1}`), Headers: http.Header{"Accept": {"text/plain"}}, At: *now}) {
		t.Fatal("body capture rejected")
	}
	drainCapture(s)
	must(t, s.RecordUsage(usageFor(k, "body-exec", "body", *now)))
	configureCapture(t, s, CaptureModeHeaders, nil)
	d, e = s.Detail("body-exec")
	must(t, e)
	if d.BodyStatus != captureStatusDisabled || string(d.Body) != "null" || d.HeadersStatus != captureStatusAvailable {
		t.Fatal("headers mode exposed body", d)
	}

	configureCapture(t, s, CaptureModeHeadersAndBody, nil)
	if !s.Capture(CaptureInput{RequestID: "u", TraceID: "unsupported", KeyID: k.ID, Body: []byte("plain text"), Headers: http.Header{"Content-Type": {"text/plain"}}, At: *now}) {
		t.Fatal("unsupported capture rejected")
	}
	drainCapture(s)
	must(t, s.RecordUsage(usageFor(k, "unsupported-exec", "unsupported", *now)))
	d, e = s.Detail("unsupported-exec")
	must(t, e)
	if d.BodyStatus != captureStatusUnsupported || d.CaptureReason != captureReasonUnsupported || string(d.Body) != "null" || d.HeadersStatus != captureStatusAvailable {
		t.Fatal(d)
	}

	configureCapture(t, s, CaptureModeHeadersAndBody, func(v *Settings) { v.MaxBodyBytes = 8 })
	if !s.Capture(CaptureInput{RequestID: "t", TraceID: "too-large", KeyID: k.ID, Body: []byte(`{"too":"large"}`), Headers: http.Header{"Accept": {"*/*"}}, At: *now}) {
		t.Fatal("oversized body queue rejected")
	}
	drainCapture(s)
	must(t, s.RecordUsage(usageFor(k, "too-large-exec", "too-large", *now)))
	d, e = s.Detail("too-large-exec")
	must(t, e)
	if d.BodyStatus != captureStatusTooLarge || d.HeadersStatus != captureStatusAvailable || string(d.Body) != "null" {
		t.Fatal(d)
	}

	configureCapture(t, s, CaptureModeHeadersAndBody, func(v *Settings) { v.MaxBodyBytes = 1 << 20 })
	wide := strings.Repeat("a", captureHeaderLimit+1)
	if !s.Capture(CaptureInput{RequestID: "w", TraceID: "wide", KeyID: k.ID, Body: []byte(`{"ok":1}`), Headers: http.Header{"User-Agent": {wide}}, At: *now}) {
		t.Fatal("wide header capture rejected")
	}
	drainCapture(s)
	must(t, s.RecordUsage(usageFor(k, "wide-exec", "wide", *now)))
	d, e = s.Detail("wide-exec")
	must(t, e)
	if d.HeadersStatus != captureStatusTooLarge || d.BodyStatus != captureStatusAvailable {
		t.Fatal("headers not independent", d)
	}

	configureCapture(t, s, CaptureModeOff, nil)
	d, e = s.Detail("body-exec")
	must(t, e)
	if d.CaptureStatus != captureStatusDisabled || d.HeadersStatus != captureStatusDisabled || d.BodyStatus != captureStatusDisabled || len(d.Headers) != 0 || string(d.Body) != "null" {
		t.Fatal("off exposed historical content", d)
	}
}

// Headers mode is independent of body handling end to end: empty, invalid and
// oversized bodies neither suppress safe headers nor leak body content.
func TestHeadersModeKeepsHeadersForBodyVariants(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	configureCapture(t, s, CaptureModeHeaders, func(v *Settings) { v.MaxBodyBytes = 8 })
	cases := []struct {
		name string
		body []byte
	}{
		{name: "empty", body: nil},
		{name: "invalid", body: []byte("plain text")},
		{name: "oversized", body: []byte(`{"too":"large"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trace := "headers-" + tc.name
			headers := http.Header{"Accept": {"application/json"}, "Content-Type": {"application/json"}, "User-Agent": {"agent-" + tc.name}}
			if !s.Capture(CaptureInput{RequestID: tc.name, TraceID: trace, KeyID: k.ID, Body: tc.body, Headers: headers, At: *now}) {
				t.Fatal("headers capture rejected")
			}
			drainCapture(s)
			must(t, s.RecordUsage(usageFor(k, tc.name+"-exec", trace, *now)))
			d, e := s.Detail(tc.name + "-exec")
			must(t, e)
			if d.CaptureStatus != captureStatusAvailable || d.HeadersStatus != captureStatusAvailable || d.BodyStatus != captureStatusDisabled {
				t.Fatal("body variant changed header status", d)
			}
			if string(d.Body) != "null" || d.Headers.Get("Accept") != "application/json" || d.Headers.Get("Content-Type") != "application/json" || d.Headers.Get("User-Agent") != "agent-"+tc.name {
				t.Fatal("safe headers/body visibility lost", d)
			}
			c, _ := storedCapture(t, s, k.ID, trace)
			if c.BodyStatus != captureStatusDisabled || string(c.Body) != "null" || c.Headers.Get("User-Agent") != "agent-"+tc.name {
				t.Fatal("stored headers/body status mismatch", c)
			}
		})
	}
	h := s.Health()
	if h.CaptureDrops["too_large"] != 0 || h.CaptureDrops["unsupported"] != 0 {
		t.Fatal("headers-only capture counted a skipped body", h.CaptureDrops)
	}
}

// Maintenance gates run before the store mutex so admission and management can
// fail fast while a physical compaction owns the lock.
func TestMaintenanceGateFailsFastWithoutStoreMutex(t *testing.T) {
	s, _, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	s.mu.Lock()
	s.maintenanceActive.Store(true)
	defer func() {
		s.maintenanceActive.Store(false)
		s.mu.Unlock()
	}()
	check := func(name string, call func() error, want error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- call() }()
		select {
		case e := <-done:
			if !errors.Is(e, want) {
				t.Fatalf("%s returned %v, want %v", name, e, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s blocked on the store mutex", name)
		}
	}
	check("admit", func() error { return s.Admit(Admission{RequestID: "during", KeyID: k.ID, Model: "gpt-exact"}) }, ErrMaintenance)
	check("delete", func() error { return s.DeleteKey(k.ID) }, ErrBusy)
	check("compact", s.Compact, ErrBusy)
	check("settings", func() error { _, e := s.UpdateSettings(Settings{}); return e }, ErrBusy)
	check("cleanup", func() error { _, e := s.CleanupCaptures(); return e }, ErrBusy)
	check("purge", func() error { _, e := s.PurgeCaptures(); return e }, ErrBusy)
}

// Maintenance errors are retained in Health rather than being silently lost.
func TestMaintenanceErrorVisible(t *testing.T) {
	s, _, _ := fixture(t)
	s.cleanFault = func() error { return errors.New("injected maintenance failure") }
	if e := s.Maintenance(); e == nil {
		t.Fatal("failure hidden")
	}
	h := s.Health()
	if h.MaintenanceError == "" {
		t.Fatal("maintenance error not visible", h)
	}
	s.cleanFault = nil
	must(t, s.Maintenance())
	if s.Health().MaintenanceError != "" {
		t.Fatal("error not cleared", s.Health())
	}
}

// Automatic physical compaction is opportunistic: it runs after idleness once
// free pages exceed both thresholds, and retries no more often than 15 minutes.
func TestAutomaticCompactionGate(t *testing.T) {
	s, now, _ := fixture(t)
	fill := func() {
		must(t, s.db.Update(func(tx *bolt.Tx) error {
			b, e := tx.CreateBucket([]byte("fixture"))
			if e != nil {
				return e
			}
			payload := bytes.Repeat([]byte("x"), 256<<10)
			for i := 0; i < 128; i++ {
				if e := b.Put([]byte(string(rune(i))), payload); e != nil {
					return e
				}
			}
			return nil
		}))
		must(t, s.db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte("fixture")) }))
	}
	fill()
	must(t, s.Maintenance())
	h := s.Health()
	if h.LastCompactAt == nil || h.LastCompactAfterBytes >= h.LastCompactBeforeBytes {
		t.Fatal("automatic compaction did not run", h)
	}
	first := *h.LastCompactAt
	fill()
	*now = now.Add(time.Minute)
	must(t, s.Maintenance())
	h = s.Health()
	if !h.LastCompactAt.Equal(first) {
		t.Fatal("retry gate ignored", h.LastCompactAt, first)
	}
	*now = now.Add(16 * time.Minute)
	must(t, s.Maintenance())
	h = s.Health()
	if h.LastCompactAt.Equal(first) || h.LastCompactAfterBytes >= h.LastCompactBeforeBytes {
		t.Fatal("gated compaction never retried", h)
	}
}

// A higher feature version is rejected from a read-only inspection before any
// writable bbolt handle or freelist update occurs.
func TestUnknownFeatureVersionReadOnlyRejected(t *testing.T) {
	s, _, p := fixture(t)
	path := s.path
	must(t, s.Close())
	db, e := bolt.Open(path, 0600, nil)
	must(t, e)
	must(t, db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("meta")).Put([]byte("observer_features"), []byte("2"))
	}))
	must(t, db.Close())
	before, e := os.ReadFile(path)
	must(t, e)
	if _, e = Open(Options{DataPath: path, Pricer: p}); !errors.Is(e, ErrUnsupported) {
		t.Fatal(e)
	}
	after, e := os.ReadFile(path)
	must(t, e)
	if !bytes.Equal(before, after) {
		t.Fatal("read-only precheck modified database")
	}
}

// Old schema2 settings inherit the former boolean and raw retention; the one-
// time FIFO index rebuild is idempotent and legacy capture rows stay readable.
func TestLegacySchemaSettingsIndexAndCapture(t *testing.T) {
	s, now, p := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	capturedAt := *now
	configureCapture(t, s, CaptureModeHeadersAndBody, nil)
	must(t, s.RecordUsage(usageFor(k, "exec", "trace", *now)))
	path := s.path
	must(t, s.Close())

	innerJSON, e := json.Marshal(map[string]any{"ok": true})
	must(t, e)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, e = zw.Write(innerJSON)
	must(t, e)
	must(t, zw.Close())
	legacy := capture{RequestID: "admission", At: *now, Headers: http.Header{"Content-Type": {"application/json"}}, Gzip: gz.Bytes(), Status: captureStatusAvailable}
	payload, e := encode(legacy)
	must(t, e)

	db, e := bolt.Open(path, 0600, nil)
	must(t, e)
	must(t, db.Update(func(tx *bolt.Tx) error {
		var raw map[string]any
		must(t, json.Unmarshal(tx.Bucket([]byte("meta")).Get([]byte("settings")), &raw))
		delete(raw, "capture_mode")
		delete(raw, "capture_retention_seconds")
		raw["capture_enabled"] = true
		raw["raw_retention_hours"] = 48
		encoded, e := json.Marshal(raw)
		if e != nil {
			return e
		}
		if e := tx.Bucket([]byte("meta")).Put([]byte("settings"), encoded); e != nil {
			return e
		}
		if e := tx.Bucket([]byte("meta")).Delete([]byte("capture_index_ready")); e != nil {
			return e
		}
		return tx.Bucket([]byte("captures")).Put([]byte(captureID(k.ID, "trace")), payload)
	}))
	must(t, db.Close())

	r, e := Open(Options{DataPath: path, Now: func() time.Time { return *now }, Pricer: p})
	must(t, e)
	settings, e := r.Settings()
	must(t, e)
	if settings.CaptureMode != CaptureModeHeadersAndBody || settings.CaptureRetentionSeconds != 48*3600 || !settings.CaptureEnabled {
		t.Fatal("legacy settings not inherited", settings)
	}
	must(t, r.db.View(func(tx *bolt.Tx) error {
		capb := tx.Bucket([]byte("captures"))
		if capb.Stats().KeyN != 1 || tx.Bucket([]byte("capture_fifo")).Stats().KeyN != 1 {
			t.Fatal("index not rebuilt")
		}
		if captureUsedBytes(tx) != int64(len(payload)) {
			t.Fatal("byte counter not reconciled")
		}
		if string(tx.Bucket([]byte("meta")).Get([]byte("observer_features"))) != "1" {
			t.Fatal("feature marker missing")
		}
		return nil
	}))
	d, e := r.Detail("exec")
	must(t, e)
	if d.CaptureStatus != captureStatusAvailable || !bytes.Contains(d.Body, []byte(`"ok":true`)) {
		t.Fatal("legacy capture unreadable", d)
	}
	must(t, r.Close())
	r, e = Open(Options{DataPath: path, Now: func() time.Time { return *now }, Pricer: p})
	must(t, e)
	defer r.Close()
	must(t, r.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("captures")).Stats().KeyN != 1 || tx.Bucket([]byte("capture_fifo")).Stats().KeyN != 1 {
			t.Fatal("migration not idempotent")
		}
		return nil
	}))
	// Legacy rows inherit the raw retention at migration. Extending the active
	// capture policy later must not revive content past that inherited deadline.
	*now = now.Add(49 * time.Hour)
	v, e := r.Settings()
	must(t, e)
	v.RawRetentionHours = 168
	v.CaptureRetentionSeconds = 72 * 3600
	_, e = r.UpdateSettings(v)
	must(t, e)
	d, e = r.Detail("exec")
	must(t, e)
	wantExpiry := capturedAt.Add(48 * time.Hour)
	if d.CaptureStatus != captureStatusExpired || r.Health().CaptureBytes != 0 || d.ExpiresAt == nil || !d.ExpiresAt.Equal(wantExpiry) {
		t.Fatal("legacy inherited expiry revived", d, r.Health())
	}
}

// A failed write after FIFO eviction rolls the whole transaction back: the old
// content and byte counter survive and the failure is classified separately.
func TestCaptureTransactionRollbackAndWriteFailure(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	configureCapture(t, s, CaptureModeHeadersAndBody, nil)
	body := []byte(`{"payload":"` + strings.Repeat("y", 400) + `"}`)
	if !s.Capture(CaptureInput{RequestID: "a", TraceID: "first", KeyID: k.ID, Body: body, At: *now}) {
		t.Fatal("first capture rejected")
	}
	drainCapture(s)
	must(t, s.RecordUsage(usageFor(k, "a-exec", "first", *now)))
	used := s.Health().CaptureBytes
	configureCapture(t, s, CaptureModeHeadersAndBody, func(v *Settings) { v.MaxBodyStorageBytes = used + 5 })
	s.mu.Lock()
	s.captureFault = func() error { return errors.New("injected capture write failure") }
	s.mu.Unlock()
	*now = now.Add(time.Second)
	if !s.Capture(CaptureInput{RequestID: "b", TraceID: "second", KeyID: k.ID, Body: body, At: *now}) {
		t.Fatal("second capture rejected")
	}
	drainCapture(s)
	h := s.Health()
	if h.CaptureBytes != used || h.CaptureDrops["write_failed"] == 0 || h.CaptureDrops["evicted"] != 0 {
		t.Fatal("failed write escaped rollback", h)
	}
	first, e := s.Detail("a-exec")
	must(t, e)
	if first.BodyStatus != captureStatusAvailable {
		t.Fatal("eviction survived rollback", first)
	}
	s.mu.Lock()
	s.captureFault = nil
	s.mu.Unlock()
	if !s.Capture(CaptureInput{RequestID: "b", TraceID: "second", KeyID: k.ID, Body: body, At: *now}) {
		t.Fatal("retry rejected")
	}
	drainCapture(s)
	must(t, s.RecordUsage(usageFor(k, "b-exec", "second", *now)))
	first, e = s.Detail("a-exec")
	must(t, e)
	if first.CaptureStatus != captureStatusEvicted {
		t.Fatal("retry did not evict oldest", first)
	}
}

// Physical maintenance refuses to start while a request or write is pending,
// and the admission gate rejects work during the short maintenance window.
func TestCompactBusyGuardsAndMaintenanceAdmission(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	must(t, s.Admit(Admission{RequestID: "active", KeyID: k.ID, Model: "gpt-exact"}))
	if e := s.Compact(); !errors.Is(e, ErrBusy) {
		t.Fatal("compacted with an active request", e)
	}
	must(t, s.Complete("active", *now))
	s.captureQueued.Add(1)
	if e := s.Compact(); !errors.Is(e, ErrBusy) {
		t.Fatal("compacted with a queued write", e)
	}
	s.captureQueued.Add(-1)
	s.writeFault = func() error { return ErrBusy }
	if e := s.RecordUsage(event(k, "debt", *now)); !errors.Is(e, ErrAccounting) {
		t.Fatal(e)
	}
	if e := s.Compact(); !errors.Is(e, ErrBusy) {
		t.Fatal("compacted with pending accounting", e)
	}
	s.writeFault = nil
	must(t, s.Maintenance())
	s.maintenanceActive.Store(true)
	if e := s.Admit(Admission{RequestID: "during", KeyID: k.ID, Model: "gpt-exact"}); !errors.Is(e, ErrMaintenance) {
		t.Fatal("admission during maintenance", e)
	}
	if _, e := s.UpdateKey(k.ID, KeySpec{Enabled: true}); !errors.Is(e, ErrBusy) {
		t.Fatal("management write during maintenance", e)
	}
	s.maintenanceActive.Store(false)
	must(t, s.Admit(Admission{RequestID: "after", KeyID: k.ID, Model: "gpt-exact"}))
	must(t, s.Complete("after", *now))
}

// Explicit configuration rejects unsupported modes, sub-minute retention and a
// content window longer than metadata, while the boolean is a projection.
func TestCaptureSettingsValidationAndProjection(t *testing.T) {
	s, _, _ := fixture(t)
	v, e := s.Settings()
	must(t, e)
	v.CaptureMode = "everything"
	if _, e = s.UpdateSettings(v); !errors.Is(e, ErrInvalid) {
		t.Fatal("unknown mode accepted", e)
	}
	v, _ = s.Settings()
	v.CaptureMode = CaptureModeHeaders
	v.CaptureRetentionSeconds = 59
	if _, e = s.UpdateSettings(v); !errors.Is(e, ErrInvalid) {
		t.Fatal("sub-minute retention accepted", e)
	}
	v.CaptureRetentionSeconds = v.RawRetentionHours*3600 + 1
	if _, e = s.UpdateSettings(v); !errors.Is(e, ErrInvalid) {
		t.Fatal("retention beyond metadata accepted", e)
	}
	v.CaptureRetentionSeconds = 60
	v.CaptureEnabled = false
	updated, e := s.UpdateSettings(v)
	must(t, e)
	if updated.CaptureMode != CaptureModeHeaders || !updated.CaptureEnabled {
		t.Fatal("legacy boolean not projected", updated)
	}
	// Old callers that omit the new mode inherit the current narrower scope.
	v, _ = s.Settings()
	v.CaptureMode = ""
	v.CaptureEnabled = true
	updated, e = s.UpdateSettings(v)
	must(t, e)
	if updated.CaptureMode != CaptureModeHeaders {
		t.Fatal("legacy boolean widened headers mode", updated)
	}
	v, _ = s.Settings()
	v.CaptureMode = ""
	v.CaptureEnabled = false
	updated, e = s.UpdateSettings(v)
	must(t, e)
	if updated.CaptureMode != CaptureModeOff {
		t.Fatal("legacy false did not disable", updated)
	}
	v, _ = s.Settings()
	v.CaptureMode = ""
	v.CaptureEnabled = true
	updated, e = s.UpdateSettings(v)
	must(t, e)
	if updated.CaptureMode != CaptureModeHeadersAndBody {
		t.Fatal("legacy enable from off did not select full capture", updated)
	}
}
