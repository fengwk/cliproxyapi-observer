//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"
)

const (
	// executionWait bounds failure detection only; synchronization itself uses
	// the mock barrier and observer state, never fixed sleeps.
	executionWait = 20 * time.Second

	scriptedUpstreamStatus = http.StatusInternalServerError
	clientClosedStatus     = 499
)

// inferResult is returned by the asynchronous client so a goroutine never calls
// testing.T methods outside the test goroutine.
type inferResult struct {
	status int
	body   []byte
	err    error
}

// startInference issues a real client inference with a cancelable context. It
// reports the result over a buffered channel so the caller can synchronize on
// the mock barrier instead of sleeping.
func startInference(ctx context.Context, h *harness, secret string, stream bool) <-chan inferResult {
	result := make(chan inferResult, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+"/v1/chat/completions", bytes.NewReader(chatBody(stream)))
		if err != nil {
			result <- inferResult{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.httpClient().Do(req)
		if err != nil {
			result <- inferResult{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		result <- inferResult{status: resp.StatusCode, body: body, err: err}
	}()
	return result
}

// awaitMockEntered waits until the scripted upstream attempt has been admitted
// and reached the mock. It fails boundedly when the host never forwards it.
func awaitMockEntered(t *testing.T, gate *mockBarrier) {
	t.Helper()
	select {
	case <-gate.entered:
	case <-time.After(executionWait):
		t.Fatal("mock upstream did not receive the scripted request")
	}
}

// awaitInference waits for an asynchronous inference to finish.
func awaitInference(t *testing.T, result <-chan inferResult) inferResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(executionWait):
		t.Fatal("inference did not finish")
		return inferResult{}
	}
}

// TestConcurrentAdmissionRejectsSecondAndRecoversAfterCompletion proves a Key's
// MaxConcurrent slot is held for the whole in-flight request, a second request
// is rejected before it reaches the upstream, and the slot is released after
// completion so a later request succeeds.
func TestConcurrentAdmissionRejectsSecondAndRecoversAfterCompletion(t *testing.T) {
	h := newHarness(t)

	spec := validSpec()
	spec.Enabled = true
	spec.MaxConcurrent = 1
	created := h.createKey(t, spec)

	gate := h.mock.holdNextRequest()
	t.Cleanup(gate.open)

	first := startInference(context.Background(), h, created.Secret, false)
	awaitMockEntered(t, gate)

	h.await(t, "the first inference holds the only concurrency slot", func() bool {
		keys := h.keys(t)
		return len(keys) == 1 && keys[0].ID == created.Key.ID && keys[0].Active == 1
	})

	status, body := h.infer(t, created.Secret, false)
	if status != http.StatusTooManyRequests {
		t.Fatalf("second inference status = %d body %s, want %d for a full concurrency slot", status, truncate(body, 400), http.StatusTooManyRequests)
	}
	if calls := h.mock.callCount(); calls != 1 {
		t.Fatalf("upstream calls after rejected inference = %d, want 1 (rejection must precede upstream dispatch)", calls)
	}

	gate.open()
	completed := awaitInference(t, first)
	if completed.err != nil {
		t.Fatalf("first inference failed: %v", completed.err)
	}
	if completed.status != http.StatusOK {
		t.Fatalf("first inference status = %d body %s, want 200", completed.status, truncate(completed.body, 400))
	}
	if got := jsonContent(completed.body); got != fixtureText {
		t.Errorf("first inference content = %q, want %q", got, fixtureText)
	}

	h.await(t, "completion releases the concurrency slot", func() bool {
		keys := h.keys(t)
		return len(keys) == 1 && keys[0].ID == created.Key.ID && keys[0].Active == 0
	})

	status, body = h.infer(t, created.Secret, false)
	if status != http.StatusOK {
		t.Fatalf("inference after slot release status = %d body %s, want 200", status, truncate(body, 400))
	}
	if got := jsonContent(body); got != fixtureText {
		t.Errorf("inference after slot release content = %q, want %q", got, fixtureText)
	}
	if calls := h.mock.callCount(); calls != 2 {
		t.Errorf("upstream calls after slot recovery = %d, want 2", calls)
	}
	h.mock.requireClean(t)
}

// TestCanceledInferenceReleasesConcurrencySlot proves client cancellation
// unblocks the upstream attempt through context cancellation and releases the
// Key's admission slot, allowing the next request to succeed.
func TestCanceledInferenceReleasesConcurrencySlot(t *testing.T) {
	h := newHarness(t)

	spec := validSpec()
	spec.Enabled = true
	spec.MaxConcurrent = 1
	created := h.createKey(t, spec)

	gate := h.mock.holdNextRequest()
	t.Cleanup(gate.open)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inFlight := startInference(ctx, h, created.Secret, false)
	awaitMockEntered(t, gate)

	h.await(t, "the in-flight inference holds the concurrency slot", func() bool {
		keys := h.keys(t)
		return len(keys) == 1 && keys[0].ID == created.Key.ID && keys[0].Active == 1
	})

	cancel()
	canceled := awaitInference(t, inFlight)
	select {
	case <-gate.canceled:
	case <-time.After(executionWait):
		t.Fatal("client cancellation did not reach the blocked upstream")
	}

	// A canceled request may surface as a transport error or the host's 499
	// response; anything else means cancellation did not propagate.
	if canceled.err == nil && canceled.status != clientClosedStatus {
		t.Fatalf("canceled inference status = %d body %s, want a cancellation error or %d", canceled.status, truncate(canceled.body, 400), clientClosedStatus)
	}
	if canceled.err != nil && !errors.Is(canceled.err, context.Canceled) {
		t.Fatalf("canceled inference error = %v, want context cancellation", canceled.err)
	}

	h.await(t, "cancellation releases the concurrency slot", func() bool {
		keys := h.keys(t)
		return len(keys) == 1 && keys[0].ID == created.Key.ID && keys[0].Active == 0
	})

	status, body := h.infer(t, created.Secret, false)
	if status != http.StatusOK {
		t.Fatalf("inference after cancellation status = %d body %s, want 200", status, truncate(body, 400))
	}
	if got := jsonContent(body); got != fixtureText {
		t.Errorf("inference after cancellation content = %q, want %q", got, fixtureText)
	}
	h.mock.requireClean(t)
}

// TestStreamingResponseUnchanged proves the observer is passive on the SSE
// path relative to an uninstrumented CPA baseline, including normal CPA model
// mapping/usage normalization. Content and the final terminator must survive.
func TestStreamingResponseUnchanged(t *testing.T) {
	baseline := newHarnessWith(t, hostOptions{PluginDisabled: true})
	controlStatus, controlBody := baseline.infer(t, clientKey, true)
	if controlStatus != http.StatusOK {
		t.Fatalf("baseline stream status %d", controlStatus)
	}
	h := newHarness(t)
	created := h.createKey(t, validSpec())

	status, body := h.infer(t, created.Secret, true)
	if status != http.StatusOK {
		t.Fatalf("stream status = %d body %s, want 200", status, truncate(body, 400))
	}
	if got := sseContent(body); got != fixtureText {
		t.Errorf("stream content = %q, want %q", got, fixtureText)
	}

	got := sseDataPayloads(body)
	want := sseDataPayloads(controlBody)
	if len(want) == 0 || want[len(want)-1] != "[DONE]" {
		t.Fatal("baseline stream missing terminator")
	}
	if len(got) != len(want) {
		t.Fatalf("SSE data payload count = %d (%q), want %d (%q)", len(got), got, len(want), want)
	}
	for i := range want {
		if want[i] == "[DONE]" {
			if got[i] != want[i] {
				t.Errorf("SSE payload %d = %q, want terminator %q", i, got[i], want[i])
			}
			continue
		}
		var gotJSON, wantJSON any
		if err := json.Unmarshal([]byte(got[i]), &gotJSON); err != nil {
			t.Errorf("SSE payload %d is not JSON: %v (%q)", i, err, got[i])
			continue
		}
		if err := json.Unmarshal([]byte(want[i]), &wantJSON); err != nil {
			t.Fatalf("fixture SSE payload %d is not JSON: %v", i, err)
		}
		if !reflect.DeepEqual(gotJSON, wantJSON) {
			t.Errorf("SSE payload %d = %s, want %s", i, got[i], want[i])
		}
	}

	calls := h.mock.callsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("upstream stream attempts = %d, want 1", len(calls))
	}
	h.mock.requireClean(t)
}

// TestRetryAttemptsShareTraceAndChargeOnce proves real host retries create
// distinct execution records that share the inbound TraceID, while only the
// execution that produced usage is charged and the failed attempt stays at
// zero. This distinguishes admission IDs from execution IDs and protects
// against double charging a retried logical request.
func TestRetryAttemptsShareTraceAndChargeOnce(t *testing.T) {
	h := newHarness(t)
	created := h.createKey(t, validSpec())
	acceptancePutPricing(t, h, acceptancePricing{Prices: []acceptancePrice{
		{Model: modelName, Rates: acceptanceRatesAll(1_000_000)},
		{Model: upstreamModel, Rates: acceptanceRatesAll(1_000_000)},
	}, Rules: []acceptanceRule{}})

	h.mock.failNextRequests(scriptedUpstreamStatus, 1)

	status, body := h.infer(t, created.Secret, false)
	if status != http.StatusOK {
		t.Fatalf("retried inference status = %d body %s, want 200; host retry must be enabled", status, truncate(body, 400))
	}
	if got := jsonContent(body); got != fixtureText {
		t.Errorf("retried inference content = %q, want %q", got, fixtureText)
	}
	if calls := h.mock.callCount(); calls != 2 {
		t.Fatalf("upstream attempts = %d, want exactly 2 (one scripted failure plus one success)", calls)
	}

	h.await(t, "both retry executions are recorded", func() bool {
		return len(h.page(t, "page_size=100").Items) == 2
	})
	records := h.page(t, "page_size=100").Items
	if len(records) != 2 {
		t.Fatalf("execution records = %d, want 2: %+v", len(records), records)
	}

	var failed, succeeded *recordView
	ids := make(map[string]struct{}, len(records))
	for i := range records {
		record := &records[i]
		if record.ID == "" {
			t.Fatalf("execution record %d has no ID: %+v", i, record)
		}
		if _, duplicate := ids[record.ID]; duplicate {
			t.Fatalf("duplicate execution ID %q; retry attempts must be distinct", record.ID)
		}
		ids[record.ID] = struct{}{}
		if record.KeyID != created.Key.ID {
			t.Errorf("execution %s key = %q, want %q", record.ID, record.KeyID, created.Key.ID)
		}
		if record.Model != modelName && record.Model != upstreamModel {
			t.Errorf("execution %s model = %q, want %q or %q", record.ID, record.Model, modelName, upstreamModel)
		}
		switch {
		case record.Failed:
			failed = record
		default:
			succeeded = record
		}
	}
	if failed == nil || succeeded == nil {
		t.Fatalf("retry records must contain one failed and one successful execution: %+v", records)
	}
	if failed.TraceID == "" || succeeded.TraceID == "" {
		t.Fatalf("execution trace IDs must be present: failed=%q succeeded=%q", failed.TraceID, succeeded.TraceID)
	}
	if failed.TraceID != succeeded.TraceID {
		t.Errorf("retry trace IDs differ: failed=%q succeeded=%q", failed.TraceID, succeeded.TraceID)
	}
	if failed.ID == succeeded.ID {
		t.Errorf("retry attempts reuse execution ID %q, want distinct IDs", failed.ID)
	}
	if failed.CostMicroUSD != 0 {
		t.Errorf("failed retry attempt cost = %d micro-USD, want 0", failed.CostMicroUSD)
	}
	if succeeded.CostMicroUSD != 15 {
		t.Fatalf("successful retry attempt cost = %d micro-USD, want exactly 15", succeeded.CostMicroUSD)
	}

	var summary struct {
		Requests     int64 `json:"requests"`
		Failures     int64 `json:"failures"`
		CostMicroUSD int64 `json:"cost_micro_usd"`
	}
	h.api(t, http.MethodGet, "/summary", nil, http.StatusOK, &summary)
	if summary.Requests != 2 {
		t.Errorf("summary requests = %d, want 2 execution records", summary.Requests)
	}
	if summary.Failures != 1 {
		t.Errorf("summary failures = %d, want 1 failed execution", summary.Failures)
	}
	if summary.CostMicroUSD != failed.CostMicroUSD+succeeded.CostMicroUSD {
		t.Errorf("summary cost = %d micro-USD, want exactly the sum of execution charges %d", summary.CostMicroUSD, failed.CostMicroUSD+succeeded.CostMicroUSD)
	}
	if summary.CostMicroUSD != succeeded.CostMicroUSD {
		t.Errorf("summary cost = %d micro-USD, want %d: the failed attempt must not be charged twice", summary.CostMicroUSD, succeeded.CostMicroUSD)
	}
	h.mock.requireClean(t)
}
