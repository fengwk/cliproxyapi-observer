package plugin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/engine"
	"github.com/fengwk/cliproxyapi-observer/internal/pricing"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Real manager.before -> engine admission -> asynchronous usage.handle uses
// different admission/usage IDs, capture off, and completion before usage.
// Only the typed condition result survives; the request body does not.
func TestRequestBodyPricingManagerFlow(t *testing.T) {
	m := liveManager(t)
	base := int64(1_000_000)
	fast := int64(10_000_000)
	settings, err := m.store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.Pricing = pricing.Config{Auto: false, Source: pricing.SourceV2, Prices: []pricing.ManualPrice{{
		Model: "priced", Provider: "openai",
		Definition: pricing.PricingDefinition{
			Base: pricing.RateSet{Input: base},
			Overrides: []pricing.Override{{
				ID: "fast", When: pricing.Condition{RequestBody: &pricing.RequestBodyCondition{
					Path: "/speed", Equals: pricing.JSONString("fast"),
				}},
				Rates: pricing.RatePatch{Input: &fast},
			}},
		},
	}}}
	if _, err = m.store.UpdateSettings(settings); err != nil {
		t.Fatal(err)
	}
	k, secret, err := m.store.CreateKey(engine.KeySpec{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	auth := authenticateTest(t, m, pluginapi.FrontendAuthRequest{
		Method: http.MethodPost, Path: "/v1/chat/completions",
		Headers: http.Header{"Authorization": {"Bearer " + secret}},
	})
	metadata := map[string]any{
		coreexecutor.CallerScopeMetadataKey: session.CallerScope(auth.Principal),
		coreexecutor.RequestPathMetadataKey: "/v1/chat/completions",
	}
	before := func(requestID, trace, body string) {
		t.Helper()
		var response pluginapi.RequestInterceptResponse
		_ = json.Unmarshal(call(t, m, "request.intercept_before", pluginapi.RequestInterceptRequest{
			RequestID: requestID, TraceID: trace, RequestedModel: "priced",
			Body: []byte(body), Metadata: metadata,
		}), &response)
		if response.Terminate {
			t.Fatalf("admission denied: %+v", response)
		}
	}
	before("admission-fast", "trace-fast", `{"speed":"fast"}`)
	call(t, m, "request.complete", pluginapi.RequestCompletion{RequestID: "admission-fast", CompletedAt: time.Now()})

	usage := func(id, trace string) engine.Detail {
		t.Helper()
		call(t, m, "usage.handle", pluginapi.UsageRecord{
			RequestID: id, TraceID: trace, APIKey: "observer:" + k.ID,
			AccessProvider: AccessProvider, Generate: true, Provider: "openai", Model: "priced",
			RequestedAt: time.Now(), Detail: pluginapi.UsageDetail{InputTokens: 1, TotalTokens: 1},
		})
		d, err := m.store.Detail(id)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	fastDetail := usage("usage-fast", "trace-fast")
	if fastDetail.Record.CostMicroUSD != 10 || fastDetail.PriceQuote.Rates.Input != fast {
		t.Fatalf("request-body override not applied: %+v", fastDetail)
	}
	if fastDetail.CaptureStatus != "disabled" || string(fastDetail.Body) != "null" ||
		bytes.Contains(fastDetail.Body, []byte("fast")) {
		t.Fatalf("capture off retained request content: %+v", fastDetail)
	}

	before("admission-slow", "trace-slow", `{"speed":"slow"}`)
	call(t, m, "request.complete", pluginapi.RequestCompletion{RequestID: "admission-slow", CompletedAt: time.Now()})
	slowDetail := usage("usage-slow", "trace-slow")
	if slowDetail.Record.CostMicroUSD != 1 || slowDetail.PriceQuote.Rates.Input != base {
		t.Fatalf("non-matching body charged override: %+v", slowDetail)
	}

	// A usage record without a matching admission is unknown, not an invented
	// match and not a Key gate.
	missing := usage("usage-missing", "trace-missing")
	if missing.Record.CostMicroUSD != 0 || missing.PriceQuote.Reason != "missing_facts" {
		t.Fatalf("missing correlation = %+v", missing)
	}
}
