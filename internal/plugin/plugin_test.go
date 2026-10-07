package plugin

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-observer/internal/observer"
)

func newTestManager() (*Manager, *fakeOpener) {
	opener := &fakeOpener{cfg: observerConfigFixture()}
	return NewManager(opener, nil), opener
}

func observerConfigFixture() observer.Config {
	return observer.Config{
		StatsRetentionDays:  365,
		RequestRetention:    24 * time.Hour,
		BodyRetention:       24 * time.Hour,
		CaptureBodies:       false,
		MaxBodyBytes:        1048576,
		MaxBodyStorageBytes: 268435456,
		FlushInterval:       time.Second,
	}
}

func TestRegisterAdvertisesExactCapabilities(t *testing.T) {
	m, _ := newTestManager()
	raw, err := m.HandleCall(pluginabi.MethodPluginRegister, mustJSON(t, lifecycleRequest{ConfigYAML: []byte("live"), SchemaVersion: pluginabi.SchemaVersion}))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var reg registration
	mustResult(t, raw, &reg)
	if reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Errorf("schema = %d, want %d", reg.SchemaVersion, pluginabi.SchemaVersion)
	}
	if reg.Metadata.Name != "Observer" || reg.Metadata.Version != Version || reg.Metadata.GitHubRepository == "" || reg.Metadata.Author == "" {
		t.Errorf("metadata incomplete: %+v", reg.Metadata)
	}
	if !reg.Capabilities.UsagePlugin || !reg.Capabilities.RequestInterceptor || !reg.Capabilities.ManagementAPI {
		t.Errorf("missing required capability: %+v", reg.Capabilities)
	}
	// The host must never be able to route inference through the observer.
	env := decodeEnvelope(t, raw)
	caps := string(mustResultField(t, env, "capabilities"))
	for _, forbidden := range []string{"executor", "model_provider", "model_router", "response_interceptor", "stream", "auth_provider", "scheduler"} {
		if strings.Contains(caps, forbidden) {
			t.Errorf("capabilities unexpectedly declare %q: %s", forbidden, caps)
		}
	}
	if got := strings.Count(caps, "true"); got != 3 {
		t.Errorf("expected exactly 3 enabled capabilities, got %d: %s", got, caps)
	}
}

func TestSchemaNegotiationCapsAtHostAndSDK(t *testing.T) {
	cases := []struct {
		host uint32
		want uint32
	}{
		{host: 0, want: pluginabi.SchemaVersion},
		{host: 6, want: 6},
		{host: 5, want: 5},
		{host: 3, want: 3},
		{host: 99, want: pluginabi.SchemaVersion},
	}
	for _, tc := range cases {
		m, _ := newTestManager()
		raw, err := m.HandleCall(pluginabi.MethodPluginReconfigure, mustJSON(t, lifecycleRequest{ConfigYAML: []byte("live"), SchemaVersion: tc.host}))
		if err != nil {
			t.Fatalf("host %d: %v", tc.host, err)
		}
		var reg registration
		mustResult(t, raw, &reg)
		if reg.SchemaVersion != tc.want {
			t.Errorf("host schema %d negotiated %d, want %d", tc.host, reg.SchemaVersion, tc.want)
		}
	}
}

func TestInterceptNeverModifiesRequest(t *testing.T) {
	m, opener := newTestManager()
	if _, err := m.HandleCall(pluginabi.MethodPluginRegister, mustJSON(t, lifecycleRequest{ConfigYAML: []byte("live"), SchemaVersion: pluginabi.SchemaVersion})); err != nil {
		t.Fatalf("register: %v", err)
	}
	req := pluginapi.RequestInterceptRequest{
		RequestID: "req-1",
		TraceID:   "trace-1",
		Model:     "claude-3",
		Headers:   map[string][]string{"Authorization": {"Bearer secret"}},
		Body:      []byte(`{"prompt":"hello"}`),
	}
	raw, err := m.HandleCall(pluginabi.MethodRequestInterceptBefore, mustJSON(t, req))
	if err != nil {
		t.Fatalf("intercept before: %v", err)
	}
	var resp pluginapi.RequestInterceptResponse
	mustResult(t, raw, &resp)
	if resp.Path != "" || len(resp.Headers) != 0 || len(resp.Body) != 0 || len(resp.ClearHeaders) != 0 || resp.Terminate {
		t.Errorf("before-auth interceptor modified the request: %+v", resp)
	}
	if store := opener.last(); store == nil || store.captureCount() != 1 {
		t.Fatalf("request was not enqueued for capture")
	}

	raw, err = m.HandleCall(pluginabi.MethodRequestInterceptAfter, mustJSON(t, req))
	if err != nil {
		t.Fatalf("intercept after: %v", err)
	}
	mustResult(t, raw, &pluginapi.RequestInterceptResponse{})
}

func TestObservationIsFailOpenWithoutStore(t *testing.T) {
	m, _ := newTestManager() // never registered: no live store
	for _, method := range []string{pluginabi.MethodUsageHandle, pluginabi.MethodRequestInterceptBefore} {
		raw, err := m.HandleCall(method, []byte("not-json-at-all"))
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if env := decodeEnvelope(t, raw); !env.OK {
			t.Errorf("%s must acknowledge without a store, got %s", method, raw)
		}
	}

	// Storage that fails to open must still not fail observation hooks.
	m2 := NewManager(&fakeOpener{parseErr: errStoreFailure}, nil)
	raw, err := m2.HandleCall(pluginabi.MethodPluginRegister, mustJSON(t, lifecycleRequest{ConfigYAML: []byte("live")}))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if code := mustError(t, raw); code != "invalid_config" {
		t.Errorf("register with bad config code = %q, want invalid_config", code)
	}
	raw, err = m2.HandleCall(pluginabi.MethodUsageHandle, mustJSON(t, pluginapi.UsageRecord{RequestID: "req-2"}))
	if err != nil || !decodeEnvelope(t, raw).OK {
		t.Fatalf("usage after failed register must be acked, got %s err %v", raw, err)
	}
}

func TestUnknownMethodReturnsError(t *testing.T) {
	m, _ := newTestManager()
	raw, err := m.HandleCall("nonsense.method", nil)
	if err != nil {
		t.Fatalf("unknown method: %v", err)
	}
	if code := mustError(t, raw); code != "unknown_method" {
		t.Errorf("code = %q, want unknown_method", code)
	}
}

// mustResultField returns a single field from an envelope result object.
func mustResultField(t *testing.T, env pluginabi.Envelope, field string) []byte {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(env.Result, &obj); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	value, ok := obj[field]
	if !ok {
		t.Fatalf("result missing field %q: %s", field, env.Result)
	}
	return value
}
