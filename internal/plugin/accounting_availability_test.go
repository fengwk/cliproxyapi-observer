package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/engine"
	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// callEnvelope performs one plugin call and decodes its envelope without
// requiring a successful result. Accounting failures must stay observable, so
// the helper must not fail the test on a reported error.
func callEnvelope(t *testing.T, m *Manager, method string, value any) envelopeResult {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	out, errCall := m.HandleCall(method, raw)
	if errCall != nil {
		t.Fatal(errCall)
	}
	var env envelopeResult
	if json.Unmarshal(out, &env) != nil {
		t.Fatalf("%s: decode envelope: %s", method, out)
	}
	return env
}

// An accounting failure must be a genuine, visible error and must never
// create a durable or in-memory admission block. The affected Key stays
// usable, no interruption sidecar is written next to the database and the next
// valid callback is accounted normally.
func TestAccountingFailureIsVisibleAndLeavesNoAdmissionBlock(t *testing.T) {
	m := liveManager(t)
	settings, err := m.store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.Pricing = pricing.Config{Prices: []pricing.ManualPrice{{Model: "model", Provider: "openai", Definition: pricing.PricingDefinition{Base: pricing.RateSet{Input: 1_000_000}}}}}
	if _, err = m.store.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	key, secret, err := m.store.CreateKey(engine.KeySpec{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	usage := func(requestID string) pluginapi.UsageRecord {
		return pluginapi.UsageRecord{RequestID: requestID, APIKey: "observer:" + key.ID, AccessProvider: AccessProvider, Generate: true, Provider: "openai", Model: "model", RequestedAt: time.Now(), Detail: pluginapi.UsageDetail{InputTokens: 1, TotalTokens: 1}}
	}
	// A valid callback is charged exactly once.
	if env := callEnvelope(t, m, "usage.handle", usage("first")); !env.OK {
		t.Fatalf("valid usage rejected: %+v", env.Error)
	}
	keys, err := m.store.Keys()
	if err != nil || len(keys) != 1 || keys[0].Used.Total != 1 {
		t.Fatalf("first usage not charged: %+v %v", keys, err)
	}
	// Authenticate before the outage so the owned scope survives it.
	if a := authenticateTest(t, m, pluginapi.FrontendAuthRequest{Method: http.MethodPost, Path: "/v1/chat/completions", Headers: http.Header{"Authorization": {"Bearer " + secret}}}); !a.Authenticated {
		t.Fatal("authentication failed")
	}
	before := func(requestID string) pluginapi.RequestInterceptResponse {
		var r pluginapi.RequestInterceptResponse
		_ = json.Unmarshal(call(t, m, "request.intercept_before", pluginapi.RequestInterceptRequest{RequestID: requestID, RequestedModel: "model", Metadata: map[string]any{
			coreexecutor.CallerScopeMetadataKey: session.CallerScope("observer:" + key.ID),
			coreexecutor.RequestPathMetadataKey: "/v1/chat/completions",
		}}), &r)
		return r
	}
	// Storage becomes genuinely unavailable. The failure is reported instead of
	// being swallowed or turned into a silent $0 charge.
	call(t, m, "plugin.quiesce", nil)
	env := callEnvelope(t, m, "usage.handle", usage("second"))
	if env.OK || env.Error == nil || env.Error.Code != "accounting_unavailable" {
		t.Fatalf("accounting failure not reported: %+v", env)
	}
	for _, internal := range []string{m.cfg.DB, secret, key.ID} {
		if internal != "" && strings.Contains(env.Error.Message, internal) {
			t.Fatalf("accounting error leaked internals: %s", env.Error.Message)
		}
	}
	// A genuinely inaccessible database is not a valid admission lookup.
	if r := before("during-outage"); !r.Terminate || r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unavailable storage did not fail closed: %+v", r)
	}
	// The removed fence mechanism must not leave a sidecar behind.
	if _, errStat := os.Stat(m.cfg.DB + ".interrupted"); !os.IsNotExist(errStat) {
		t.Fatalf("interruption sidecar written: %v", errStat)
	}
	// The same configuration reopens and the Key works again immediately.
	call(t, m, "plugin.reconfigure", lifecycleRequest{[]byte("db: " + m.cfg.DB), 6})
	if r := before("after-outage"); r.Terminate {
		t.Fatalf("accounting failure latched the Key: %+v", r)
	}
	call(t, m, "request.complete", pluginapi.RequestCompletion{RequestID: "after-outage", CompletedAt: time.Now()})
	if env = callEnvelope(t, m, "usage.handle", usage("third")); !env.OK {
		t.Fatalf("valid usage after the outage rejected: %+v", env.Error)
	}
	keys, err = m.store.Keys()
	if err != nil || len(keys) != 1 || keys[0].Used.Total != 2 {
		t.Fatalf("usage after the outage not charged: %+v %v", keys, err)
	}
}

// A usage record for an unknown Key must never become an invisible charge and
// must never leak engine internals into the host envelope.
func TestUnknownKeyUsageIsNotAnInvisibleCharge(t *testing.T) {
	m := liveManager(t)
	unknown := "observer:" + strings.Repeat("a", 64)
	env := callEnvelope(t, m, "usage.handle", pluginapi.UsageRecord{RequestID: "unknown", APIKey: unknown, AccessProvider: AccessProvider, Generate: true, Provider: "openai", Model: "model", RequestedAt: time.Now(), Detail: pluginapi.UsageDetail{InputTokens: 1, TotalTokens: 1}})
	if env.OK || env.Error == nil || env.Error.Code != "unknown_key" {
		t.Fatalf("unknown key usage not reported: %+v", env)
	}
	for _, internal := range []string{m.cfg.DB, "observer:" + strings.Repeat("a", 64)} {
		if strings.Contains(env.Error.Message, internal) {
			t.Fatalf("usage error leaked internals: %s", env.Error.Message)
		}
	}
	keys, err := m.store.Keys()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if key.Used.Total != 0 {
			t.Fatalf("unknown Key was charged: %+v", key)
		}
	}
}

// A panic in a usage callback is reported to the host but leaves no marker or
// persistent gate; a subsequent real callback still commits normally.
func TestUsagePanicDoesNotRestrictFutureAdmissions(t *testing.T) {
	m := NewManager(nil)
	var panicClock atomic.Bool
	now := time.Now().UTC()
	m.now = func() time.Time {
		if panicClock.Load() {
			panic("injected clock failure")
		}
		return now
	}
	db := filepath.Join(t.TempDir(), "observer.db")
	call(t, m, "plugin.register", lifecycleRequest{[]byte("db: " + db), 6})
	t.Cleanup(func() { call(t, m, "plugin.shutdown", nil) })
	settings, err := m.store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.Pricing = pricing.Config{Prices: []pricing.ManualPrice{{Model: "model", Provider: "openai", Definition: pricing.PricingDefinition{Base: pricing.RateSet{Input: 1_000_000}}}}}
	if _, err = m.store.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	key, _, err := m.store.CreateKey(engine.KeySpec{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	u := pluginapi.UsageRecord{RequestID: "panic", APIKey: "observer:" + key.ID, AccessProvider: AccessProvider, Generate: true, Provider: "openai", Model: "model", RequestedAt: now, Detail: pluginapi.UsageDetail{InputTokens: 1, TotalTokens: 1}}
	panicClock.Store(true)
	env := callEnvelope(t, m, "usage.handle", u)
	panicClock.Store(false)
	if env.OK || env.Error == nil || env.Error.Code != "plugin_error" {
		t.Fatalf("panic was not reported: %+v", env)
	}
	if _, err = os.Stat(db + ".interrupted"); !os.IsNotExist(err) {
		t.Fatalf("panic wrote a sidecar: %v", err)
	}
	if err = m.store.Admit(engine.Admission{RequestID: "after-panic", KeyID: key.ID}); err != nil {
		t.Fatal("panic blocked admission", err)
	}
	if err = m.store.Complete("after-panic", now); err != nil {
		t.Fatal(err)
	}
	u.RequestID = "valid"
	if env = callEnvelope(t, m, "usage.handle", u); !env.OK {
		t.Fatalf("subsequent usage failed: %+v", env.Error)
	}
	keys, err := m.store.Keys()
	if err != nil || len(keys) != 1 || keys[0].Used.Total != 1 {
		t.Fatalf("subsequent accounting: %+v %v", keys, err)
	}
}

// The removed recovery route owns no registration, no handler and no request
// body contract. It answers as a missing route, never as an alias and never
// as a silent no-op, and it never touches the Key it names.
func TestRemovedRecoveryRouteIsGone(t *testing.T) {
	m := liveManager(t)
	created := dispatchForTest(t, m, http.MethodPost, managementBasePath+"/keys", nil, `{"label":"removed-route","enabled":true}`)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create key: %d %s", created.StatusCode, created.Body)
	}
	var key keySecretResponse
	if json.Unmarshal(created.Body, &key) != nil || key.Key.ID == "" {
		t.Fatalf("decode key: %s", created.Body)
	}
	for _, route := range managementRegistration().Routes {
		if strings.Contains(route.Path, "recover") {
			t.Fatalf("removed route still registered: %s", route.Path)
		}
	}
	removed := managementBasePath + "/keys/recover"
	cases := []struct {
		name   string
		method string
		query  url.Values
		body   string
	}{
		{"post without body", http.MethodPost, nil, ""},
		{"post with the removed body", http.MethodPost, url.Values{"id": {key.Key.ID}}, `{"acknowledge":true}`},
		{"get", http.MethodGet, url.Values{"id": {key.Key.ID}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := dispatchForTest(t, m, tc.method, removed, tc.query, tc.body)
			if r.StatusCode != http.StatusNotFound {
				t.Fatalf("removed route answered %d %s, want 404", r.StatusCode, r.Body)
			}
			if strings.Contains(string(r.Body), "acknowledge") || strings.Contains(string(r.Body), key.Secret) {
				t.Fatalf("removed route echoed its old contract: %s", r.Body)
			}
		})
	}
	// Without a body the removed path is a plain missing route.
	if r := dispatchForTest(t, m, http.MethodPost, removed, nil, ""); r.StatusCode != http.StatusNotFound {
		t.Fatalf("removed route status %d %s, want 404", r.StatusCode, r.Body)
	}
	// The rejected calls must not have touched the Key or its history.
	keys, err := m.store.Keys()
	if err != nil || len(keys) != 1 || keys[0].ID != key.Key.ID || keys[0].Used.Total != 0 {
		t.Fatalf("keys after the removed route = %+v %v", keys, err)
	}
	page, err := m.store.Requests(engine.Query{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour), Page: 1, PageSize: 25})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("ledger after the removed route = %+v %v", page.Items, err)
	}
}
