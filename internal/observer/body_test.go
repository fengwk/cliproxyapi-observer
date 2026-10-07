package observer

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func captureBody(s *Store, id, body string) bool {
	return s.Capture(pluginapi.RequestInterceptRequest{
		RequestID: id,
		TraceID:   "trace-" + id,
		Body:      []byte(body),
	})
}

func TestCaptureRedactsSensitiveJSONFields(t *testing.T) {
	s := openTestStore(t, func(c *Config) { c.CaptureBodies = true })
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"hello world func main(){}"}],` +
		`"authorization":"Bearer super-secret","metadata":{"api_key":"sk-123","nested":{"password":"top"}},` +
		`"access_token":"tok-9","refresh_token":"ref-9","api-key":"k-1"}`
	if !captureBody(s, "body-1", body) {
		t.Fatalf("capture rejected")
	}
	flushAll(t, s)

	detail, err := s.Body("body-1")
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if !detail.Redacted {
		t.Errorf("Redacted = false")
	}
	for _, secret := range []string{"super-secret", "sk-123", `"top"`, "tok-9", "ref-9", "k-1"} {
		if strings.Contains(detail.Content, secret) {
			t.Errorf("secret %q leaked in body: %s", secret, detail.Content)
		}
	}
	// Normal prompt/code content must be preserved.
	if !strings.Contains(detail.Content, "hello world func main(){}") {
		t.Errorf("prompt content was altered: %s", detail.Content)
	}
	if detail.TraceID != "trace-body-1" {
		t.Errorf("trace id = %q", detail.TraceID)
	}
	// No headers or auth objects are ever stored.
	if detail.RequestID != "body-1" {
		t.Errorf("request id = %q", detail.RequestID)
	}
}

func TestCaptureSkipsOversizeBodyWithoutTruncation(t *testing.T) {
	s := openTestStore(t, func(c *Config) {
		c.CaptureBodies = true
		c.MaxBodyBytes = 64
		c.MaxBodyStorageBytes = 1024
	})
	big := `{"payload":"` + strings.Repeat("x", 200) + `"}`
	if captureBody(s, "big-1", big) {
		t.Fatalf("oversize body accepted")
	}
	flushAll(t, s)
	if got := s.Status().DroppedBodies; got != 1 {
		t.Errorf("DroppedBodies = %d, want 1", got)
	}
	if _, err := s.Body("big-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Body = %v, want ErrNotFound", err)
	}
}

func TestCaptureSkipsInvalidJSON(t *testing.T) {
	s := openTestStore(t, func(c *Config) { c.CaptureBodies = true })
	if !captureBody(s, "bad-1", `{"unterminated": `) {
		t.Fatalf("capture should enqueue valid-length body")
	}
	flushAll(t, s)
	if got := s.Status().DroppedBodies; got != 1 {
		t.Errorf("DroppedBodies = %d, want 1", got)
	}
	if _, err := s.Body("bad-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Body = %v, want ErrNotFound", err)
	}
}

func TestCaptureOncePerExecution(t *testing.T) {
	s := openTestStore(t, func(c *Config) { c.CaptureBodies = true })
	captureBody(s, "once-1", `{"first":true}`)
	captureBody(s, "once-1", `{"second":true}`)
	flushAll(t, s)
	detail, err := s.Body("once-1")
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if strings.Contains(detail.Content, "second") {
		t.Errorf("second capture overwrote the first: %s", detail.Content)
	}
}

func TestBodyStorageCapEvictsOldest(t *testing.T) {
	s := openTestStore(t, func(c *Config) {
		c.CaptureBodies = true
		c.MaxBodyBytes = 200
		c.MaxBodyStorageBytes = 260 // two 100-byte bodies fit
	})
	// Each body is exactly 100 bytes; distinct created times drive eviction.
	body := `{"p":"` + strings.Repeat("x", 92) + `"}`
	if len(body) != 100 {
		t.Fatalf("test body length = %d, want 100", len(body))
	}
	for i := 0; i < 3; i++ {
		id := "cap-" + string(rune('a'+i))
		if !captureBody(s, id, body) {
			t.Fatalf("capture %s rejected", id)
		}
		flushAll(t, s) // distinct capture times, deterministic order
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := s.Body("cap-a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("oldest body should be evicted, got %v", err)
	}
	if _, err := s.Body("cap-c"); err != nil {
		t.Errorf("newest body must survive: %v", err)
	}
}

func TestBodyStrictReadExpiry(t *testing.T) {
	clk := &clock{t: time.Now()}
	s := openTestStore(t, func(c *Config) {
		c.CaptureBodies = true
		c.BodyRetention = 30 * time.Minute
	})
	s.setNow(clk.now)

	at := clk.now()
	s.SubmitUsage(usageRecord("exp-1", "openai", "gpt-5", at, simpleUsage(1, 1)))
	captureBody(s, "exp-1", `{"a":1}`)
	flushAll(t, s)

	if _, err := s.Body("exp-1"); err != nil {
		t.Fatalf("fresh body unreadable: %v", err)
	}

	// Advance past retention without running the sweeper: the read must still
	// enforce expiry.
	clk.advance(31 * time.Minute)
	if _, err := s.Body("exp-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired body read = %v, want ErrNotFound", err)
	}
	page, err := s.Requests(Query{})
	if err != nil {
		t.Fatalf("Requests: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("request metadata should survive body expiry: %d", len(page.Items))
	}
	if page.Items[0].BodyAvailable {
		t.Errorf("BodyAvailable must be false after expiry")
	}
}

func TestInvalidAndEmptyBodyNeverPanics(t *testing.T) {
	s := openTestStore(t, func(c *Config) { c.CaptureBodies = true })
	if captureBody(s, "empty-1", "") {
		t.Errorf("empty body accepted")
	}
	if captureBody(s, "", `{"a":1}`) {
		t.Errorf("empty request id accepted")
	}
	flushAll(t, s)
	if _, err := s.Body(""); !errors.Is(err, ErrNotFound) {
		t.Errorf("Body(\"\") = %v, want ErrNotFound", err)
	}
}
