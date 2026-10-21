package engine

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// Debug headers are copied for every non-credential name, including multi-value
// and non-canonical keys. Credential variants are omitted before any copy so
// mixed-case names and common token/auth forms never reach the stored envelope.
func TestCapturePersistsDebugHeadersAndOmitsCredentials(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	configureCapture(t, s, CaptureModeHeadersAndBody, nil)

	const referer = "https://user:pass@example.test/debug?token=fake-url-token&ok=1"
	headers := http.Header{
		"x-request-id":        {"req-123"},
		"Traceparent":         {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		"X-Custom":            {"first", "second"},
		"Referer":             {referer},
		"aUtHoRiZaTiOn":       {"fake-authorization"},
		"Proxy-Authorization": {"fake-proxy-authorization"},
		"Cookie":              {"fake-cookie"},
		"sEt-CoOkIe":          {"fake-set-cookie"},
		"x-ApI-kEy":           {"fake-api-key"},
		"X-Api-Key-Id":        {"fake-api-key-id"},
		"Authentication":      {"fake-authentication"},
		"X-Auth":              {"fake-auth"},
		"X-Access-Token":      {"fake-access-token"},
		"X-Token-Expiry":      {"fake-token-expiry"},
		"X-Refresh-Token":     {"fake-refresh-token"},
		"X-Client-Secret":     {"fake-client-secret"},
		"X-Password":          {"fake-password"},
		"X-Credential":        {"fake-credential"},
		"Private-Key":         {"fake-private-key"},
	}
	before := headers.Clone()
	body := []byte(`{"ok":1}`)
	if !s.Capture(CaptureInput{RequestID: "headers", TraceID: "headers", KeyID: k.ID, Headers: headers, Body: body, At: *now}) {
		t.Fatal("capture rejected")
	}
	drainCapture(s)
	if !reflect.DeepEqual(headers, before) {
		t.Fatal("capture mutated caller headers")
	}
	must(t, s.RecordUsage(usageFor(k, "headers-exec", "headers", *now)))
	d, e := s.Detail("headers-exec")
	must(t, e)
	if d.HeadersStatus != captureStatusRedacted || d.BodyStatus != captureStatusAvailable {
		t.Fatal(d)
	}
	if got := d.Headers.Get("X-Request-ID"); got != "req-123" {
		t.Fatalf("case-insensitive debug header lost: %q", got)
	}
	if got := d.Headers.Get("Traceparent"); got != headers.Get("Traceparent") {
		t.Fatalf("traceparent lost: %q", got)
	}
	if got := d.Headers.Values("X-Custom"); !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("multi-value header lost: %#v", got)
	}

	ref, e := url.Parse(d.Headers.Get("Referer"))
	must(t, e)
	if ref.User != nil || ref.Query().Get("token") != "[REDACTED]" || ref.Query().Get("ok") != "1" {
		t.Fatalf("header URL not redacted: %q", d.Headers.Get("Referer"))
	}

	credentialNames := []string{
		"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie", "X-Api-Key",
		"X-Api-Key-Id", "Authentication", "X-Auth", "X-Access-Token", "X-Token-Expiry",
		"X-Refresh-Token", "X-Client-Secret", "X-Password", "X-Credential", "Private-Key",
	}
	for _, name := range credentialNames {
		if _, ok := d.Headers[http.CanonicalHeaderKey(name)]; ok {
			t.Fatalf("credential header persisted: %s", name)
		}
	}
	secrets := []string{
		"fake-authorization", "fake-proxy-authorization", "fake-cookie", "fake-set-cookie",
		"fake-api-key", "fake-api-key-id", "fake-authentication", "fake-auth",
		"fake-access-token", "fake-token-expiry", "fake-refresh-token", "fake-client-secret",
		"fake-password", "fake-credential", "fake-private-key", "fake-url-token", "user:pass",
	}
	for _, values := range d.Headers {
		for _, value := range values {
			for _, secret := range secrets {
				if strings.Contains(value, secret) {
					t.Fatalf("credential value persisted: %q", value)
				}
			}
		}
	}
	c, _ := storedCapture(t, s, k.ID, "headers")
	if c.Headers.Get("X-Custom") == "" || c.Headers.Get("Authorization") != "" || c.Headers.Get("Authentication") != "" {
		t.Fatalf("stored headers mismatch: %#v", c.Headers)
	}
}

// The 16KiB envelope covers header names as well as retained values. A partial
// group would be misleading, so any overflow discards every header while the
// independently validated JSON body remains available.
func TestCaptureHeaderBudgetCountsNamesAndDiscardsWholeGroup(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
	}{
		{
			name: "long empty name",
			headers: http.Header{
				strings.Repeat("X", captureHeaderLimit+1): nil,
				"Accept": {"application/json"},
			},
		},
		{
			name: "name plus value over limit",
			headers: http.Header{
				"X-ABC":  {strings.Repeat("v", captureHeaderLimit-2)},
				"Accept": {"application/json"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, now, _ := fixture(t)
			k, _ := key(t, s, KeySpec{Enabled: true})
			configureCapture(t, s, CaptureModeHeadersAndBody, nil)
			if !s.Capture(CaptureInput{RequestID: tc.name, TraceID: tc.name, KeyID: k.ID, Headers: tc.headers, Body: []byte(`{"ok":1}`), At: *now}) {
				t.Fatal("capture rejected")
			}
			drainCapture(s)
			must(t, s.RecordUsage(usageFor(k, tc.name+"-exec", tc.name, *now)))
			d, e := s.Detail(tc.name + "-exec")
			must(t, e)
			if d.HeadersStatus != captureStatusTooLarge || d.CaptureReason != captureReasonHeadersLarge {
				t.Fatalf("header budget not enforced: %+v", d)
			}
			if len(d.Headers) != 0 {
				t.Fatalf("partial header group persisted: %#v", d.Headers)
			}
			if d.BodyStatus != captureStatusAvailable || string(d.Body) != `{"ok":1}` {
				t.Fatalf("body should remain independent: %+v", d)
			}
		})
	}
}

// Empty values still cost a minimum entry overhead. Otherwise a single header
// with more than 16KiB zero-length values would bypass the budget and keep
// growing the copied slice.
func TestCaptureHeaderBudgetBoundsEmptyValues(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	configureCapture(t, s, CaptureModeHeadersAndBody, nil)
	headers := http.Header{
		"X-Empty": make([]string, captureHeaderLimit+1),
		"Accept":  {"application/json"},
	}
	if !s.Capture(CaptureInput{RequestID: "empty-values", TraceID: "empty-values", KeyID: k.ID, Headers: headers, Body: []byte(`{"ok":1}`), At: *now}) {
		t.Fatal("capture rejected")
	}
	drainCapture(s)
	must(t, s.RecordUsage(usageFor(k, "empty-values-exec", "empty-values", *now)))
	d, e := s.Detail("empty-values-exec")
	must(t, e)
	if d.HeadersStatus != captureStatusTooLarge || d.CaptureReason != captureReasonHeadersLarge || len(d.Headers) != 0 {
		t.Fatalf("empty-value header bypassed budget: %+v", d)
	}
	if d.BodyStatus != captureStatusAvailable || string(d.Body) != `{"ok":1}` {
		t.Fatalf("body should remain independent: %+v", d)
	}
}

// Excluded credential headers are omitted without consuming the debug-header
// budget; otherwise a single large Authorization value could hide all headers.
func TestCaptureSensitiveHeadersDoNotConsumeBudget(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	configureCapture(t, s, CaptureModeHeaders, nil)
	headers := http.Header{
		"Authorization": {strings.Repeat("s", captureHeaderLimit+1)},
		"X-Custom":      {"visible"},
	}
	if !s.Capture(CaptureInput{RequestID: "sensitive", TraceID: "sensitive", KeyID: k.ID, Headers: headers, At: *now}) {
		t.Fatal("capture rejected")
	}
	drainCapture(s)
	must(t, s.RecordUsage(usageFor(k, "sensitive-exec", "sensitive", *now)))
	d, e := s.Detail("sensitive-exec")
	must(t, e)
	if d.HeadersStatus != captureStatusAvailable || d.Headers.Get("X-Custom") != "visible" || d.Headers.Get("Authorization") != "" {
		t.Fatalf("sensitive header affected budget or leaked: %+v", d)
	}
}
