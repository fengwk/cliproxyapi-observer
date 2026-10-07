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

type mockCall struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// mockUpstream is an OpenAI-compatible chat endpoint supporting synchronous and
// SSE responses. It verifies the request contract in-line and records
// candidate violations for the test to assert.
type mockUpstream struct {
	server *httptest.Server

	mu         sync.Mutex
	calls      []mockCall
	violations []string
}

func newMockUpstream(t *testing.T) *mockUpstream {
	t.Helper()
	m := &mockUpstream{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", m.handleChat)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	m.server = httptest.NewServer(mux)
	return m
}

func (m *mockUpstream) baseURL() string { return m.server.URL }

func (m *mockUpstream) Close() { m.server.Close() }

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
	m.mu.Unlock()

	m.validateChat(r, body)

	var payload struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &payload)
	if payload.Stream {
		m.writeSSE(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(fmt.Sprintf(`{"id":"chatcmpl-observer","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`, upstreamModel, fixtureText)))
}

// validateChat asserts the upstream saw exactly what the client sent: the
// mapped native model, the fake upstream key, and the unmodified prompt. Any
// credential the observer must never persist would show up here as a leak.
func (m *mockUpstream) validateChat(r *http.Request, body []byte) {
	if r.Method != http.MethodPost {
		m.violationf("method = %s, want POST", r.Method)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+upstreamKey {
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
	if len(payload.Messages) == 0 || payload.Messages[0].Content != upstreamPrompt {
		m.violationf("prompt was altered: %s", truncate(body, 200))
	}
	for _, secret := range []string{clientKey, mgmtKey} {
		if strings.Contains(string(body), secret) {
			m.violationf("request body leaked credential %q", secret)
		}
	}
}

func (m *mockUpstream) writeSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	write := func(chunk string) {
		_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	write(fmt.Sprintf(`{"id":"chatcmpl-observer","object":"chat.completion.chunk","created":1,"model":%q,"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`, upstreamModel))
	write(fmt.Sprintf(`{"id":"chatcmpl-observer","object":"chat.completion.chunk","created":1,"model":%q,"choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, upstreamModel, fixtureText))
	write(fmt.Sprintf(`{"id":"chatcmpl-observer","object":"chat.completion.chunk","created":1,"model":%q,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, upstreamModel))
	write(`{"id":"chatcmpl-observer","object":"chat.completion.chunk","created":1,"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// chatBody builds a minimal OpenAI chat request.
func chatBody(stream bool) []byte {
	return []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}],"stream":%t}`, modelName, upstreamPrompt, stream))
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
