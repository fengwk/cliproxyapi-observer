//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// Keep both names Claude-prefixed so the host uses its native Claude executor,
// while an alias still exposes any loss of the complete execution identity.
const (
	claudeModelName     = "claude-observer-model"
	claudeUpstreamModel = "claude-observer-upstream"
	claudeUpstreamKey   = "fake-claude-upstream-key"
	claudeProvider      = "claude"
	claudeThinking      = "observer-fixture-thinking"
	claudeInput         = int64(2)
	claudeCacheRead     = int64(44225)
	claudeCacheWrite    = int64(831)
	claudeOutput        = int64(244) // includes the 40 thinking tokens
	claudeReasoning     = int64(40)
	claudeTotal         = int64(45302)
)

// This is the provider's wire usage, not the Observer's normalized buckets.
type claudeUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	CacheReadTokens   int64 `json:"cache_read_input_tokens"`
	CacheWriteTokens  int64 `json:"cache_creation_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	OutputTokenDetail struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

type claudeRawUsage struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}

type claudeMock struct {
	server *httptest.Server
	stream bool
	mu     sync.Mutex
	calls  int
	errors []string
}

func newClaudeMock(t *testing.T, stream bool) *claudeMock {
	t.Helper()
	m := &claudeMock{stream: stream}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	// Registered before the host cleanup, so the host stops first.
	t.Cleanup(m.server.Close)
	return m
}

func (m *claudeMock) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	var payload struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Thinking struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		} `json:"thinking"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	decodeErr := json.Unmarshal(body, &payload)
	m.mu.Lock()
	m.calls++
	if err != nil || decodeErr != nil {
		m.errors = append(m.errors, fmt.Sprintf("invalid request: read=%v JSON=%v", err, decodeErr))
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		m.errors = append(m.errors, fmt.Sprintf("request = %s %s, want POST /v1/messages", r.Method, r.URL.Path))
	}
	if r.Header.Get("Authorization") != "Bearer "+claudeUpstreamKey {
		m.errors = append(m.errors, "upstream did not receive the fake Claude credential")
	}
	if payload.Model != claudeUpstreamModel || payload.Stream != m.stream {
		m.errors = append(m.errors, fmt.Sprintf("model/stream = %q/%t, want %q/%t", payload.Model, payload.Stream, claudeUpstreamModel, m.stream))
	}
	if payload.Thinking.Type != "enabled" || payload.Thinking.BudgetTokens != 1024 {
		m.errors = append(m.errors, fmt.Sprintf("upstream thinking = %+v, want enabled with a 1024 token budget", payload.Thinking))
	}
	if len(payload.Messages) != 1 || payload.Messages[0].Role != "user" || len(payload.Messages[0].Content) != 1 ||
		payload.Messages[0].Content[0].Type != "text" || payload.Messages[0].Content[0].Text != upstreamPrompt {
		m.errors = append(m.errors, fmt.Sprintf("unexpected messages in request: %s", truncate(body, 400)))
	}
	m.mu.Unlock()

	if !m.stream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"msg_observer","type":"message","role":"assistant","model":%q,"content":[{"type":"thinking","thinking":%q,"signature":"fixture-signature"},{"type":"text","text":%q}],"stop_reason":"end_turn","usage":{"input_tokens":2,"cache_read_input_tokens":44225,"cache_creation_input_tokens":831,"output_tokens":244,"output_tokens_details":{"thinking_tokens":40}}}`, claudeUpstreamModel, claudeThinking, fixtureText)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	// Anthropic sends input/cache in message_start and final output/thinking in
	// message_delta. The initial output count differs to prove later replacement.
	events := []string{
		fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_observer","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"usage":{"input_tokens":2,"cache_read_input_tokens":44225,"cache_creation_input_tokens":831,"output_tokens":1}}}`, claudeUpstreamModel),
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}}`, claudeThinking),
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"fixture-signature"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		fmt.Sprintf(`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":%q}}`, fixtureText),
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":244,"output_tokens_details":{"thinking_tokens":40}}}`,
		`{"type":"message_stop"}`,
	}
	for _, data := range events {
		var event struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(data), &event)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, data)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

func (m *claudeMock) requireCalls(t *testing.T, want int) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.calls != want {
		t.Errorf("Claude upstream calls = %d, want %d", m.calls, want)
	}
	for _, err := range m.errors {
		t.Errorf("Claude upstream contract: %s", err)
	}
}

func claudeRequest(t *testing.T, h *harness, secret string, stream bool) (int, []byte) {
	t.Helper()
	request := []byte(fmt.Sprintf(`{"model":%q,"max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":[{"type":"text","text":%q}]}],"stream":%t}`, claudeModelName, upstreamPrompt, stream))
	status, body, err := h.rawRequest(http.MethodPost, "/v1/messages", request,
		map[string]string{"x-api-key": secret, "anthropic-version": "2023-06-01"})
	if err != nil {
		t.Fatal(err)
	}
	return status, body
}

func requireClaudeResponse(t *testing.T, body []byte, stream bool) {
	t.Helper()
	var start, final claudeUsage
	var text, thinking string
	if !stream {
		var response struct {
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"content"`
			Usage claudeUsage `json:"usage"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			t.Fatalf("decode Claude response: %v; body=%s", err, truncate(body, 500))
		}
		for _, part := range response.Content {
			if part.Type == "text" {
				text += part.Text
			}
			if part.Type == "thinking" {
				thinking += part.Thinking
			}
		}
		start, final = response.Usage, response.Usage
	} else {
		var starts, deltas, stops int
		for _, payload := range sseDataPayloads(body) {
			var event struct {
				Type    string `json:"type"`
				Message struct {
					Usage claudeUsage `json:"usage"`
				} `json:"message"`
				Delta struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					Thinking string `json:"thinking"`
				} `json:"delta"`
				Usage claudeUsage `json:"usage"`
			}
			if err := json.Unmarshal([]byte(payload), &event); err != nil {
				t.Fatalf("decode Claude SSE payload %q: %v", payload, err)
			}
			switch event.Type {
			case "message_start":
				starts++
				start = event.Message.Usage
			case "content_block_delta":
				if event.Delta.Type == "text_delta" {
					text += event.Delta.Text
				}
				if event.Delta.Type == "thinking_delta" {
					thinking += event.Delta.Thinking
				}
			case "message_delta":
				deltas++
				final = event.Usage
			case "message_stop":
				stops++
			}
		}
		if starts != 1 || deltas != 1 || stops != 1 {
			t.Errorf("Claude SSE start/delta/stop = %d/%d/%d, want 1/1/1: %s", starts, deltas, stops, truncate(body, 1000))
		}
		if start.OutputTokens != 1 || start.OutputTokenDetail.ThinkingTokens != 0 {
			t.Errorf("Claude SSE initial output usage = %+v, want output=1 and no thinking", start)
		}
		if final.InputTokens != 0 || final.CacheReadTokens != 0 || final.CacheWriteTokens != 0 {
			t.Errorf("Claude SSE final event must not repeat input/cache: %+v", final)
		}
	}
	if text != fixtureText {
		t.Errorf("Claude response text = %q, want %q", text, fixtureText)
	}
	if thinking != claudeThinking {
		t.Errorf("Claude response thinking = %q, want %q", thinking, claudeThinking)
	}
	if start.InputTokens != claudeInput || start.CacheReadTokens != claudeCacheRead || start.CacheWriteTokens != claudeCacheWrite ||
		final.OutputTokens != claudeOutput || final.OutputTokenDetail.ThinkingTokens != claudeReasoning {
		t.Errorf("Claude client usage start=%+v final=%+v", start, final)
	}
}

// A native Claude output includes thinking, whereas input/cache are independent.
// These two host executions catch double-counting and SSE usage merge regressions
// across the real CPA -> native plugin -> Observer accounting boundary.
func TestClaudeThinkingUsageAccounting(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "sync"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			mock := newClaudeMock(t, stream)
			h := newHarnessWith(t, hostOptions{ClaudeURL: mock.server.URL})
			spec := validSpec()
			spec.AllowedModels = []string{claudeModelName}
			spec.Limits.Total = claudeTotal
			created := h.createKey(t, spec)

			rate := int64(1_000_000)
			prices := []acceptancePrice{
				acceptanceManual(claudeModelName, claudeProvider, rate),
				acceptanceManual(claudeUpstreamModel, claudeProvider, rate),
			}
			for i := range prices {
				prices[i].Definition.Base.Reasoning = &rate
			}
			acceptancePutPricing(t, h, acceptancePricing{Auto: false, Source: "models_dev_v2", Prices: prices})

			status, body := claudeRequest(t, h, created.Secret, stream)
			if status != http.StatusOK {
				t.Fatalf("Claude inference status = %d body %s, want 200", status, truncate(body, 500))
			}
			requireClaudeResponse(t, body, stream)
			record := acceptanceAwaitNewRecord(t, h, map[string]struct{}{})

			var detail struct {
				Record struct {
					RawUsage          claudeRawUsage `json:"raw_usage"`
					AccountingQuality string         `json:"accounting_quality"`
				} `json:"record"`
			}
			h.api(t, http.MethodGet, "/requests/detail?id="+url.QueryEscape(record.ID), nil, http.StatusOK, &detail)
			wantRaw := claudeRawUsage{
				InputTokens: claudeInput, OutputTokens: claudeOutput, ReasoningTokens: claudeReasoning,
				CachedTokens: claudeCacheRead, CacheReadTokens: claudeCacheRead,
				CacheCreationTokens: claudeCacheWrite, TotalTokens: claudeTotal,
			}
			if detail.Record.RawUsage != wantRaw {
				t.Errorf("raw usage = %+v, want %+v", detail.Record.RawUsage, wantRaw)
			}
			if detail.Record.AccountingQuality != "complete" {
				t.Errorf("accounting quality = %q, want complete", detail.Record.AccountingQuality)
			}
			wantTokens := tokensView{Input: claudeInput, Output: claudeOutput, CacheRead: claudeCacheRead, CacheWrite: claudeCacheWrite, Reasoning: claudeReasoning, Total: claudeTotal}
			if record.Tokens != wantTokens {
				t.Errorf("normalized tokens = %+v, want %+v", record.Tokens, wantTokens)
			}
			if record.Tokens.Input+record.Tokens.CacheRead+record.Tokens.CacheWrite+record.Tokens.Output != record.Tokens.Total ||
				record.Tokens.Reasoning > record.Tokens.Output || record.Tokens.Output-record.Tokens.Reasoning != 204 {
				t.Errorf("normalized buckets do not conserve total with reasoning as an output subset: %+v", record.Tokens)
			}
			if record.Model != claudeUpstreamModel || record.Provider != claudeProvider || record.KeyID != created.Key.ID || record.Failed || record.Stream != stream {
				t.Errorf("execution identity/state = %+v, want model=%q provider=%q key=%q failed=false stream=%t", record, claudeUpstreamModel, claudeProvider, created.Key.ID, stream)
			}
			acceptanceAssertCost(t, record, claudeTotal, "manual")

			// The record and its quota accumulators are committed transactionally;
			// wait only for callback completion, never for an expected price.
			h.await(t, "Claude callback releases the Key", func() bool {
				keys := h.keys(t)
				return len(keys) == 1 && keys[0].Active == 0
			})
			keys := h.keys(t)
			wantUsed := limitsView{FiveHour: claudeTotal, Week: claudeTotal, Month: claudeTotal, Total: claudeTotal}
			if len(keys) != 1 || keys[0].ID != created.Key.ID || keys[0].Limits.Total != claudeTotal || keys[0].Used != wantUsed {
				t.Errorf("Key quota/accumulators = %+v, want total limit %d and used %+v", keys, claudeTotal, wantUsed)
			}
			var summary struct {
				Requests         int64 `json:"requests"`
				Failures         int64 `json:"failures"`
				Tokens           int64 `json:"tokens"`
				InputTokens      int64 `json:"input_tokens"`
				OutputTokens     int64 `json:"output_tokens"`
				CacheReadTokens  int64 `json:"cache_read_tokens"`
				CacheWriteTokens int64 `json:"cache_write_tokens"`
				ReasoningTokens  int64 `json:"reasoning_tokens"`
				CompleteRequests int64 `json:"complete_requests"`
				CostMicroUSD     int64 `json:"cost_micro_usd"`
			}
			h.api(t, http.MethodGet, "/summary?key_id="+url.QueryEscape(created.Key.ID), nil, http.StatusOK, &summary)
			if summary.Requests != 1 || summary.Failures != 0 || summary.Tokens != claudeTotal || summary.InputTokens != claudeInput ||
				summary.OutputTokens != claudeOutput || summary.CacheReadTokens != claudeCacheRead || summary.CacheWriteTokens != claudeCacheWrite ||
				summary.ReasoningTokens != claudeReasoning || summary.CompleteRequests != 1 || summary.CostMicroUSD != claudeTotal {
				t.Errorf("Claude summary = %+v, want one complete request and all normalized buckets/cost", summary)
			}
			status, body = claudeRequest(t, h, created.Secret, stream)
			if status != http.StatusTooManyRequests {
				t.Errorf("exhausted Claude Key status = %d body %s, want 429", status, truncate(body, 400))
			}
			mock.requireCalls(t, 1)
			if calls := h.mock.callCount(); calls != 0 {
				t.Errorf("Claude requests reached OpenAI mock %d times", calls)
			}
		})
	}
}
