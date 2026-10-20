package engine

import (
	"bytes"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// A live content row is deliberately made undecodable. Cleanup and policy
// changes must only read lightweight time/size indexes, never scan/decompress
// all bodies on every write. Expiring diagnostics must not touch the ledger.
func TestCaptureExpiryUsesMetadataWithoutDecodingPayloads(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	configureCapture(t, s, CaptureModeHeadersAndBody, func(v *Settings) {
		v.CaptureRetentionSeconds = 120
	})
	if !s.Capture(CaptureInput{KeyID: k.ID, TraceID: "indexed", RequestID: "admission", Body: []byte(`{"ok":true}`), At: *now}) {
		t.Fatal("capture rejected")
	}
	drainCapture(s)
	must(t, s.RecordUsage(usageFor(k, "execution", "indexed", *now)))
	cid := captureID(k.ID, "indexed")
	var size int64
	must(t, s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("captures"))
		size = int64(len(b.Get([]byte(cid))))
		if size == 0 || tx.Bucket([]byte("capture_expiry")).Stats().KeyN != 1 {
			t.Fatal("expiry index not written atomically")
		}
		return b.Put([]byte(cid), bytes.Repeat([]byte("!"), int(size)))
	}))
	clean, err := s.CleanupCaptures()
	must(t, err)
	if clean.RemovedRecords != 0 {
		t.Fatal("live content expired early")
	}
	configureCapture(t, s, CaptureModeHeadersAndBody, func(v *Settings) { v.CaptureRetentionSeconds = 60 })
	*now = now.Add(time.Minute)
	clean, err = s.CleanupCaptures()
	must(t, err)
	if clean.RemovedRecords != 1 || clean.RemovedBytes != size || s.Health().CaptureBytes != 0 {
		t.Fatalf("indexed expiry or byte count: %+v", clean)
	}
	detail, err := s.Detail("execution")
	must(t, err)
	if detail.CaptureStatus != captureStatusExpired || detail.Record.CostMicroUSD != 7 {
		t.Fatal("cleanup changed accounting or capture status", detail)
	}
	must(t, s.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("capture_expiry")).Stats().KeyN != 0 || tx.Bucket([]byte("capture_fifo")).Stats().KeyN != 0 {
			t.Fatal("dangling content indexes")
		}
		return nil
	}))
}

// A queued job retains its accepted expiry even when the administrator later
// extends TTL. Delayed work must not store an already expired prompt.
func TestDelayedCaptureCannotExtendAcceptedExpiry(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	configureCapture(t, s, CaptureModeHeadersAndBody, func(v *Settings) { v.CaptureRetentionSeconds = 60 })
	v := CaptureInput{RequestID: "admission", TraceID: "delayed", KeyID: k.ID, At: *now,
		Body: []byte(`{"prompt":"expired"}`), mode: CaptureModeHeadersAndBody,
		expiresAt: now.Add(time.Minute), generation: s.captureGeneration}
	must(t, s.RecordUsage(usageFor(k, "execution", "delayed", *now)))
	*now = now.Add(61 * time.Second)
	configureCapture(t, s, CaptureModeHeadersAndBody, func(v *Settings) { v.CaptureRetentionSeconds = 120 })
	content, _ := buildCapture(v)
	s.writeCapture(v, content)
	d, err := s.Detail("execution")
	must(t, err)
	if d.BodyStatus != captureStatusExpired || string(d.Body) != "null" || s.Health().CaptureBytes != 0 {
		t.Fatal("delayed work revived expired content", d)
	}
	must(t, s.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("captures")).Stats().KeyN != 0 ||
			tx.Bucket([]byte("capture_expiry")).Stats().KeyN != 0 {
			t.Fatal("expired queue payload was stored")
		}
		return nil
	}))
}
