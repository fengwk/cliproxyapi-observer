package plugin

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/engine"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// WebSocket upgrade detection must match protocol tokens, not substrings or header casing.
func TestWebsocketUpgrade(t *testing.T) {
	for _, test := range []struct {
		name    string
		headers http.Header
		want    bool
	}{
		{"standard", http.Header{"Upgrade": {"websocket"}, "Connection": {"Upgrade"}}, true},
		{"tokens", http.Header{"Upgrade": {"h2c, WebSocket"}, "Connection": {"keep-alive, upgrade"}}, true},
		{"multiple values", http.Header{"Upgrade": {"h2c", "websocket"}, "Connection": {"keep-alive", "upgrade"}}, true},
		{"substring", http.Header{"Upgrade": {"notwebsocket"}, "Connection": {"upgrade"}}, false},
		{"missing connection", http.Header{"Upgrade": {"websocket"}}, false},
		{"missing upgrade", http.Header{"Connection": {"upgrade"}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := websocketUpgrade(test.headers); got != test.want {
				t.Fatalf("websocketUpgrade() = %v, want %v", got, test.want)
			}
		})
	}
}

// Quota rejection must preserve the standard API error envelope while other denials remain unchanged.
func TestDeniedForQuotaBody(t *testing.T) {
	got := deniedFor(engine.ErrQuota)
	if !got.Terminate || got.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("quota response: %+v", got)
	}
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(got.ResponseBody, &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != "insufficient_quota" || body.Error.Code != "insufficient_quota" || body.Error.Message != "Observer key quota exceeded" {
		t.Fatalf("quota error body: %s", got.ResponseBody)
	}
	other := deniedFor(engine.ErrConcurrency)
	if string(other.ResponseBody) != `{"error":{"code":"admission_denied","message":"request not admitted"}}` {
		t.Fatalf("non-quota denial changed: %s", other.ResponseBody)
	}
}

// Run the before and usage hooks with capture off and on. An oversized body
// would increment drops if capture were attempted; close drains the queue before checking.
func TestWebsocketAdmissionDoesNotCapture(t *testing.T) {
	m := liveManager(t)
	s := m.store
	k, secret, err := s.CreateKey(engine.KeySpec{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	const path = "/v1/responses"
	auth := authenticateTest(t, m, pluginapi.FrontendAuthRequest{
		Method: http.MethodGet, Path: path, Headers: http.Header{"Authorization": {"Bearer " + secret}},
	})
	for _, mode := range []string{engine.CaptureModeOff, engine.CaptureModeHeadersAndBody} {
		settings, err := s.Settings()
		if err != nil {
			t.Fatal(err)
		}
		settings.CaptureMode, settings.CaptureEnabled, settings.MaxBodyBytes = mode, mode != engine.CaptureModeOff, 1
		if _, err = s.UpdateSettings(settings); err != nil {
			t.Fatal(err)
		}
		var before pluginapi.RequestInterceptResponse
		result := call(t, m, "request.intercept_before", pluginapi.RequestInterceptRequest{
			RequestID: "admission-" + mode, TraceID: mode, RequestedModel: "model", Body: []byte(`{"input":"hello"}`),
			Headers: http.Header{"Upgrade": {"websocket"}, "Connection": {"Upgrade"}},
			Metadata: map[string]any{
				coreexecutor.CallerScopeMetadataKey: session.CallerScope(auth.Principal),
				coreexecutor.RequestPathMetadataKey: path,
			},
		})
		if err := json.Unmarshal(result, &before); err != nil || before.Terminate {
			t.Fatalf("websocket admission (%s): %s (%v)", mode, result, err)
		}
		call(t, m, "request.complete", pluginapi.RequestCompletion{RequestID: "admission-" + mode, CompletedAt: time.Now()})
		call(t, m, "usage.handle", pluginapi.UsageRecord{
			RequestID: "usage-" + mode, TraceID: mode, APIKey: "observer:" + k.ID, AccessProvider: AccessProvider,
			Generate: true, Model: "model", Provider: "openai", RequestedAt: time.Now(),
		})
		d, err := s.Detail("usage-" + mode)
		if err != nil || !d.Record.Websocket || d.CaptureStatus != "unsupported" || d.HeadersStatus != "unsupported" ||
			d.BodyStatus != "unsupported" || d.CaptureReason != "websocket" || len(d.Headers) != 0 || string(d.Body) != "null" {
			t.Fatalf("websocket detail (%s): %+v (%v)", mode, d, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if h := s.Health(); h.DroppedCaptures != 0 {
		t.Fatalf("websocket admission attempted capture: %v", h.CaptureDrops)
	}
}
