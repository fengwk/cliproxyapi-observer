package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
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
	// Schema 7 is the first host lifecycle contract that reauthenticates each WS round.
	if out := call(t, m, "plugin.reconfigure", lifecycleRequest{[]byte("db: " + m.cfg.DB), 7}); len(out) == 0 {
		t.Fatal("schema 7 reconfigure returned no result")
	}
	s := m.store
	k, secret, err := s.CreateKey(engine.KeySpec{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	const path = "/v1/responses"
	auth := authenticateTest(t, m, pluginapi.FrontendAuthRequest{
		Method: http.MethodGet, Path: path, Headers: http.Header{"Authorization": {"Bearer " + secret}, "Upgrade": {"websocket"}, "Connection": {"Upgrade"}},
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
				coreexecutor.CallerScopeMetadataKey:      session.CallerScope(auth.Principal),
				coreexecutor.RequestPathMetadataKey:      path,
				coreexecutor.ExecutionSessionMetadataKey: "trusted-session-" + mode,
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

// The host lifecycle schema, not plugin registration schema, gates WS auth and health reporting.
func TestResponsesWebsocketHostSchemaCapability(t *testing.T) {
	for _, schema := range []uint32{6, 7} {
		t.Run(map[uint32]string{6: "schema-6", 7: "schema-7"}[schema], func(t *testing.T) {
			m := NewManager(nil)
			call(t, m, "plugin.register", lifecycleRequest{[]byte("db: " + t.TempDir() + "/observer.db"), schema})
			t.Cleanup(func() { call(t, m, "plugin.shutdown", nil) })
			_, secret, err := m.store.CreateKey(engine.KeySpec{Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			handshake := http.Header{"Authorization": {"Bearer " + secret}, "Upgrade": {"websocket"}, "Connection": {"Upgrade"}}
			auth := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: http.MethodGet, Path: "/v1/responses", Headers: handshake})
			if auth.Authenticated != (schema >= responsesWebsocketSchemaVersion) {
				t.Fatalf("schema %d handshake authenticated=%v", schema, auth.Authenticated)
			}
			ordinary := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: http.MethodGet, Path: "/v1/responses", Headers: http.Header{"Authorization": {"Bearer " + secret}}})
			if ordinary.Authenticated {
				t.Fatalf("schema %d ordinary GET was claimed", schema)
			}
			if schema == 6 {
				// Even an already-owned scope from a valid HTTP auth must
				// fail closed if the old host sends a WebSocket execution.
				httpAuth := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: http.MethodPost, Path: "/v1/responses", Headers: handshake})
				if !httpAuth.Authenticated {
					t.Fatal("schema 6 HTTP auth was not preserved")
				}
				raw := call(t, m, "request.intercept_before", pluginapi.RequestInterceptRequest{
					RequestID: "old-host-ws", TraceID: "old-host-ws", RequestedModel: "model",
					Metadata: map[string]any{coreexecutor.CallerScopeMetadataKey: session.CallerScope(httpAuth.Principal), coreexecutor.RequestPathMetadataKey: "/v1/responses", coreexecutor.ExecutionSessionMetadataKey: "trusted-session"},
				})
				var before pluginapi.RequestInterceptResponse
				if json.Unmarshal(raw, &before) != nil || !before.Terminate || before.StatusCode != http.StatusBadRequest || m.store.Health().ActiveRequests != 0 {
					t.Fatalf("schema 6 owned WebSocket execution not denied: %s", raw)
				}
			}
			var health healthResponse
			response := m.serveHealth()
			if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &health) != nil ||
				health.ResponsesWebsocket != (schema >= responsesWebsocketSchemaVersion) || health.SchemaVersion != managementSchemaVersion {
				t.Fatalf("schema %d health: %s (%+v)", schema, response.Body, health)
			}
		})
	}
}

// A fake Upgrade header cannot hide HTTP capture; a trusted WS follow-up is rejected before a second admission.
func TestWebsocketRequiresHostSignalAndRejectsFollowup(t *testing.T) {
	m := liveManager(t)
	if result := call(t, m, "plugin.reconfigure", lifecycleRequest{[]byte("db: " + m.cfg.DB), 7}); len(result) == 0 {
		t.Fatal("schema 7 lifecycle reconfigure failed")
	}
	s := m.store
	_, secret, err := s.CreateKey(engine.KeySpec{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := s.Settings()
	settings.CaptureMode, settings.CaptureEnabled, settings.MaxBodyBytes = engine.CaptureModeHeadersAndBody, true, 1
	if _, err := s.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	const path = "/v1/responses"
	auth := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: http.MethodPost, Path: path, Headers: http.Header{"Authorization": {"Bearer " + secret}}})
	fake := call(t, m, "request.intercept_before", pluginapi.RequestInterceptRequest{
		RequestID: "fake-upgrade", TraceID: "fake-upgrade", RequestedModel: "model", Body: []byte(`{"payload":"oversized"}`),
		Headers:  http.Header{"Upgrade": {"websocket"}, "Connection": {"Upgrade"}},
		Metadata: map[string]any{coreexecutor.CallerScopeMetadataKey: session.CallerScope(auth.Principal), coreexecutor.RequestPathMetadataKey: path},
	})
	var fakeResponse pluginapi.RequestInterceptResponse
	if json.Unmarshal(fake, &fakeResponse) != nil || fakeResponse.Terminate {
		t.Fatalf("fake Upgrade HTTP request was not handled: %s", fake)
	}
	call(t, m, "request.complete", pluginapi.RequestCompletion{RequestID: "fake-upgrade", CompletedAt: time.Now()})

	wsAuth := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: http.MethodGet, Path: path, Headers: http.Header{"Authorization": {"Bearer " + secret}, "Upgrade": {"websocket"}, "Connection": {"Upgrade"}}})
	if !wsAuth.Authenticated {
		t.Fatal("schema 7 WebSocket handshake was not authenticated")
	}
	first := call(t, m, "request.intercept_before", pluginapi.RequestInterceptRequest{
		RequestID: "ws-session", TraceID: "ws-session", RequestedModel: "model", Body: []byte(`{"input":"first"}`),
		Headers:  http.Header{"Upgrade": {"websocket"}, "Connection": {"Upgrade"}},
		Metadata: map[string]any{coreexecutor.CallerScopeMetadataKey: session.CallerScope(wsAuth.Principal), coreexecutor.RequestPathMetadataKey: path, coreexecutor.ExecutionSessionMetadataKey: "trusted-session"},
	})
	var admitted pluginapi.RequestInterceptResponse
	if json.Unmarshal(first, &admitted) != nil || admitted.Terminate || s.Health().ActiveRequests != 1 {
		t.Fatalf("initial WS admission: %s health=%+v", first, s.Health())
	}
	followup := call(t, m, "request.intercept_before", pluginapi.RequestInterceptRequest{
		RequestID: "ws-session", TraceID: "ws-session", RequestedModel: "model", Body: []byte(`{"type":"response.append"}`),
		Headers:  http.Header{"Upgrade": {"websocket"}, "Connection": {"Upgrade"}},
		Metadata: map[string]any{coreexecutor.CallerScopeMetadataKey: session.CallerScope(wsAuth.Principal), coreexecutor.RequestPathMetadataKey: path, coreexecutor.ExecutionSessionMetadataKey: "trusted-session", coreexecutor.SelectedAuthMetadataKey: "synthetic-upstream"},
	})
	var followupResponse pluginapi.RequestInterceptResponse
	if json.Unmarshal(followup, &followupResponse) != nil || !followupResponse.Terminate || followupResponse.StatusCode != http.StatusBadRequest ||
		!strings.Contains(string(followupResponse.ResponseBody), "unsupported_websocket_followup") || s.Health().ActiveRequests != 1 {
		t.Fatalf("WS follow-up was not safely rejected: %s health=%+v", followup, s.Health())
	}
	call(t, m, "request.complete", pluginapi.RequestCompletion{RequestID: "ws-session", CompletedAt: time.Now()})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.Health().DroppedCaptures != 1 {
		t.Fatalf("capture drop count after HTTP only: %+v", s.Health())
	}
}
