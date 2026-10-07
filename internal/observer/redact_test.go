package observer

import (
	"errors"
	"strings"
	"testing"
)

func TestRedactBodyNestedAndDuplicateKeys(t *testing.T) {
	body := `{"messages":[{"content":"keep me","authorization":"Bearer x"}],` +
		`"authorization":"first","authorization":"second","meta":{"password":"p"}}`
	out, redacted, err := redactBody([]byte(body))
	if err != nil {
		t.Fatalf("redactBody: %v", err)
	}
	if !redacted {
		t.Fatalf("expected redaction")
	}
	for _, leaked := range []string{"Bearer x", "first", "second", `"p"`} {
		if strings.Contains(string(out), leaked) {
			t.Errorf("leaked %q in %s", leaked, out)
		}
	}
	if !strings.Contains(string(out), "keep me") {
		t.Errorf("normal content lost: %s", out)
	}
	if got := strings.Count(string(out), redactedPlaceholder); got != 4 {
		t.Errorf("redacted %d fields, want 4: %s", got, out)
	}
}

func TestRedactBodyKeyNamesAreLiteralNotPaths(t *testing.T) {
	// A key literally named "a.b" is not the nested path a -> b, and a backslash
	// key must not be treated as an escape sequence.
	body := `{"a.b":"value-one","a\\b":"value-two","authorization":"secret"}`
	out, redacted, err := redactBody([]byte(body))
	if err != nil {
		t.Fatalf("redactBody: %v", err)
	}
	if !redacted {
		t.Fatalf("expected redaction of authorization")
	}
	if !strings.Contains(string(out), "value-one") || !strings.Contains(string(out), "value-two") {
		t.Errorf("literal dotted/backslash keys were wrongly redacted: %s", out)
	}
	if strings.Contains(string(out), "secret") {
		t.Errorf("authorization leaked: %s", out)
	}
}

func TestRedactBodyPreservesNumbersAndNonHTMLStrings(t *testing.T) {
	body := `{"n":123456789012345678901234567890,"f":1.5e-7,"code":"<div>a & b</div>","authorization":"x"}`
	out, redacted, err := redactBody([]byte(body))
	if err != nil {
		t.Fatalf("redactBody: %v", err)
	}
	if !redacted {
		t.Fatalf("expected redaction")
	}
	if !strings.Contains(string(out), "123456789012345678901234567890") {
		t.Errorf("big integer precision lost: %s", out)
	}
	if !strings.Contains(string(out), "1.5e-7") {
		t.Errorf("number formatting changed: %s", out)
	}
	if !strings.Contains(string(out), "<div>a & b</div>") {
		t.Errorf("unrelated string was HTML-escaped or altered: %s", out)
	}
}

func TestRedactBodyUnchangedWhenNothingSensitive(t *testing.T) {
	body := `{"prompt":"hello world","n":42}`
	out, redacted, err := redactBody([]byte(body))
	if err != nil {
		t.Fatalf("redactBody: %v", err)
	}
	if redacted {
		t.Errorf("unexpected redaction")
	}
	if string(out) != body {
		t.Errorf("body should be returned byte-identical, got %s", out)
	}
}

func TestRedactBodyDepthGuardFailsClosed(t *testing.T) {
	deep := `{"a":` + strings.Repeat("[", maxRedactDepth+10) + `0` +
		strings.Repeat("]", maxRedactDepth+10) + `,"authorization":"x"}`
	if _, _, err := redactBody([]byte(deep)); !errors.Is(err, errRedactTooDeep) {
		t.Fatalf("depth guard err = %v, want errRedactTooDeep", err)
	}
}

func TestCaptureDeeplyNestedBodyIsDropped(t *testing.T) {
	s := openTestStore(t, func(c *Config) { c.CaptureBodies = true })
	deep := `{"a":` + strings.Repeat("[", maxRedactDepth+10) + `0` +
		strings.Repeat("]", maxRedactDepth+10) + `,"authorization":"secret"}`
	if !captureBody(s, "deep-1", deep) {
		t.Fatalf("capture should enqueue a valid-length body")
	}
	flushAll(t, s)
	if _, err := s.Body("deep-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unredactable deep body was stored: %v", err)
	}
	if s.Status().DroppedBodies == 0 {
		t.Errorf("DroppedBodies = 0, want > 0")
	}
}

func TestCaptureRedactsNestedArraysInStore(t *testing.T) {
	s := openTestStore(t, func(c *Config) { c.CaptureBodies = true })
	body := `{"messages":[{"role":"user","content":"hi"},{"x":[{"access_token":"SECRETVALUE"}]}]}`
	captureBody(s, "arr-1", body)
	flushAll(t, s)
	detail, err := s.Body("arr-1")
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if strings.Contains(detail.Content, "SECRETVALUE") {
		t.Errorf("nested access_token leaked: %s", detail.Content)
	}
	if !strings.Contains(detail.Content, `"hi"`) {
		t.Errorf("normal content lost: %s", detail.Content)
	}
}
