//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fixtureText is the assistant content the mock upstream returns. The client
// must receive it unchanged, proving the observer never altered the response.
const fixtureText = "observer-fixture-response"

// upstreamPrompt is the user content the client sends and the mock verifies
// arrived unmodified.
const upstreamPrompt = "hello-observer"

// fixtureUsage is a realistic OpenAI usage block: a 10-token prompt of which 4
// tokens are served from cache, and a 5-token completion. The host normalizes
// this to subset accounting: input 10, uncached 6, cache read 4, output 5,
// total 15.
const (
	fixturePromptTokens     = 10
	fixtureCompletionTokens = 5
	fixtureCachedTokens     = 4
	fixtureTotalTokens      = 15
)

type mockCall struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// mockBarrier makes one upstream attempt observable and blocks it until the
// test releases it. entered is closed once the request reaches the mock;
// release is closed by the test. The handler also watches the request context
// so client cancellation cannot leave the mock blocked.
type mockBarrier struct {
	entered     chan struct{}
	canceled    chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

// open releases the blocked request. It is idempotent so tests can also call it
// from cleanup while unwinding a failed assertion.
func (b *mockBarrier) open() {
	b.releaseOnce.Do(func() { close(b.release) })
}

// mockUpstream is an OpenAI-compatible chat endpoint supporting synchronous and
// SSE responses. It verifies the request contract in-line and records
// candidate violations for the test to assert.
type mockUpstream struct {
	server *httptest.Server

	mu            sync.Mutex
	calls         []mockCall
	violations    []string
	wantPrompt    string
	gate          *mockBarrier
	failStatus    int
	failRemaining int
}

func newMockUpstream(t *testing.T) *mockUpstream {
	t.Helper()
	m := &mockUpstream{wantPrompt: upstreamPrompt}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", m.handleChat)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	m.server = httptest.NewServer(mux)
	return m
}

// allowAnyPrompt relaxes the exact-prompt contract for tests that drive
// multiple distinct prompts and assert their own mapping.
func (m *mockUpstream) allowAnyPrompt() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wantPrompt = ""
}

func (m *mockUpstream) baseURL() string { return m.server.URL }

func (m *mockUpstream) Close() { m.server.Close() }

// holdNextRequest installs a one-shot barrier for the next chat request. Tests
// wait on entered instead of sleeping, then close release to let the attempt
// continue. The barrier is consumed before the handler performs the upstream
// response, so the request has already passed observer admission.
func (m *mockUpstream) holdNextRequest() *mockBarrier {
	m.mu.Lock()
	defer m.mu.Unlock()
	gate := &mockBarrier{entered: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{})}
	m.gate = gate
	return gate
}

// failNextRequests scripts the first count chat attempts to fail with status
// before any successful response. It lets a test drive real host retry behavior
// without relying on external failures; the script is consumed atomically.
func (m *mockUpstream) failNextRequests(status, count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failStatus = status
	m.failRemaining = count
}

// callCount reports how many chat attempts reached the mock.
func (m *mockUpstream) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// callsSnapshot copies the recorded calls for assertions.
func (m *mockUpstream) callsSnapshot() []mockCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mockCall, len(m.calls))
	copy(out, m.calls)
	return out
}

func (m *mockUpstream) violationf(format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.violations = append(m.violations, fmt.Sprintf(format, args...))
}

// requireClean fails the test when any request violated the upstream contract.
func (m *mockUpstream) requireClean(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.violations {
		t.Errorf("upstream contract violation: %s", v)
	}
	m.violations = nil
}

func (m *mockUpstream) handleChat(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.calls = append(m.calls, mockCall{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	gate := m.gate
	m.gate = nil
	failStatus := 0
	if m.failRemaining > 0 {
		m.failRemaining--
		failStatus = m.failStatus
	}
	m.mu.Unlock()

	m.validateChat(r, body)

	if gate != nil {
		close(gate.entered)
		select {
		case <-gate.release:
		case <-r.Context().Done():
			close(gate.canceled)
			// The observer must release its admission slot when the client
			// disconnects even though the upstream attempt never finished.
			return
		}
	}
	if failStatus != 0 {
		// A scripted upstream failure lets tests exercise host retry rounds.
		// The body intentionally contains no credentials or usage counters.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(failStatus)
		_, _ = io.WriteString(w, fmt.Sprintf(`{"error":{"message":"scripted upstream failure","type":"server_error","status":%d}}`, failStatus))
		return
	}

	var payload struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &payload)
	if payload.Stream {
		m.writeSSE(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(fmt.Sprintf(`{"id":"chatcmpl-observer","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":%s}`, upstreamModel, fixtureText, usageFixtureJSON())))
}

// usageFixtureJSON renders the realistic OpenAI usage block used by both the
// sync and streaming responses.
func usageFixtureJSON() string {
	return fmt.Sprintf(
		`{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d}}`,
		fixturePromptTokens, fixtureCompletionTokens, fixtureTotalTokens, fixtureCachedTokens,
	)
}

// validateChat asserts the upstream saw exactly what the client sent: the
// mapped native model, the fake upstream key, and the unmodified prompt. Any
// credential the observer must never persist would show up here as a leak.
func (m *mockUpstream) validateChat(r *http.Request, body []byte) {
	if r.Method != http.MethodPost {
		m.violationf("method = %s, want POST", r.Method)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+upstreamKey && got != "Bearer fake-upstream-key-two" {
		m.violationf("Authorization = %q, want the fake upstream key", got)
	}
	if r.Header.Get("x-api-key") != "" {
		m.violationf("unexpected x-api-key header on an OpenAI request")
	}
	var payload struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		m.violationf("malformed chat body: %v", err)
		return
	}
	if payload.Model != upstreamModel {
		m.violationf("model = %q, want %q", payload.Model, upstreamModel)
	}
	m.mu.Lock()
	wantPrompt := m.wantPrompt
	m.mu.Unlock()
	if len(payload.Messages) == 0 {
		m.violationf("no messages in chat body: %s", truncate(body, 200))
	} else if wantPrompt != "" && payload.Messages[0].Content != wantPrompt {
		m.violationf("prompt was altered: %s", truncate(body, 200))
	} else if wantPrompt == "" && strings.TrimSpace(payload.Messages[0].Content) == "" {
		m.violationf("empty prompt in chat body: %s", truncate(body, 200))
	}
	for _, secret := range []string{clientKey, mgmtKey} {
		if strings.Contains(string(body), secret) {
			m.violationf("request body leaked credential %q", secret)
		}
	}
}

// mockSSEPayloads is the exact ordered SSE data payload sequence the mock
// emits. Tests compare the client-visible stream against it to prove the
// observer neither rewrites nor reorders streaming output.
func mockSSEPayloads() []string {
	return []string{
		fmt.Sprintf(`{"id":"chatcmpl-observer","object":"chat.completion.chunk","created":1,"model":%q,"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`, upstreamModel),
		fmt.Sprintf(`{"id":"chatcmpl-observer","object":"chat.completion.chunk","created":1,"model":%q,"choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, upstreamModel, fixtureText),
		fmt.Sprintf(`{"id":"chatcmpl-observer","object":"chat.completion.chunk","created":1,"model":%q,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, upstreamModel),
		fmt.Sprintf(`{"id":"chatcmpl-observer","object":"chat.completion.chunk","created":1,"choices":[],"usage":%s}`, usageFixtureJSON()),
		"[DONE]",
	}
}

func (m *mockUpstream) writeSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, payload := range mockSSEPayloads() {
		_, _ = io.WriteString(w, "data: "+payload+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// chatBody builds a minimal OpenAI chat request.
func chatBody(stream bool) []byte {
	return chatBodyWithPrompt(stream, upstreamPrompt)
}

// chatBodyWithPrompt builds a minimal OpenAI chat request with a custom user
// prompt, used to prove concurrent requests never cross their captured bodies.
func chatBodyWithPrompt(stream bool, prompt string) []byte {
	return []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}],"stream":%t}`, modelName, prompt, stream))
}

// sseContent extracts the concatenated assistant content from an SSE body.
func sseContent(body []byte) string {
	var out strings.Builder
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		for _, choice := range chunk.Choices {
			out.WriteString(choice.Delta.Content)
		}
	}
	return out.String()
}

// sseDataPayloads returns every non-empty data payload in order, including the
// terminating [DONE] marker. It lets a test compare the complete client-visible
// stream, not just concatenated assistant text.
func sseDataPayloads(body []byte) []string {
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data != "" {
			out = append(out, data)
		}
	}
	return out
}

// jsonContent extracts the first assistant message content from a sync body.
func jsonContent(body []byte) string {
	var payload struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	if len(payload.Choices) == 0 {
		return ""
	}
	return payload.Choices[0].Message.Content
}
