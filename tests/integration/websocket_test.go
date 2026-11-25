//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func websocketHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.waitForLog(t, "full client load complete", 20*time.Second)
	return h
}

func websocketDial(t *testing.T, h *harness, path, secret string) (*websocket.Conn, error) {
	t.Helper()
	cfg, err := websocket.NewConfig(fmt.Sprintf("ws://127.0.0.1:%d%s", h.port, path), h.baseURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Header = http.Header{"Authorization": {"Bearer " + secret}}
	cfg.Dialer = &net.Dialer{Timeout: 5 * time.Second}
	return websocket.DialConfig(cfg)
}

func websocketTurn(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	payload := fmt.Sprintf(`{"type":"response.create","model":%q,"speed":"fast","input":[{"role":"user","content":[{"type":"input_text","text":%q}]}]}`, modelName, upstreamPrompt)
	if err := websocket.Message.Send(conn, payload); err != nil {
		t.Fatalf("send response.create: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		var frame string
		if err := websocket.Message.Receive(conn, &frame); err != nil {
			t.Fatalf("receive response event: %v", err)
		}
		var event struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(frame), &event); err != nil {
			t.Fatalf("decode response event %q: %v", frame, err)
		}
		switch event.Type {
		case "response.completed":
			return
		case "response.failed", "error":
			t.Fatalf("response turn failed: %s", frame)
		}
	}
}

func TestObserverResponsesWebsocketRounds(t *testing.T) {
	for _, capture := range []bool{false, true} {
		name := "capture_off"
		if capture {
			name = "capture_on"
		}
		t.Run(name, func(t *testing.T) {
			h := websocketHarness(t)
			settings := acceptanceGetSettings(t, h)
			if capture {
				settings["capture_enabled"], settings["capture_mode"] = true, "headers_and_body"
			}
			settings["pricing"] = acceptancePricing{Prices: []acceptancePrice{{
				Model: upstreamModel, Provider: acceptanceProvider,
				Definition: acceptanceDefinitionAll(1_000_000),
			}}}
			price := settings["pricing"].(acceptancePricing).Prices[0]
			price.Definition.Overrides = []acceptanceOverride{{
				ID:    "fast-request-body",
				When:  acceptanceCondition{RequestBody: &acceptanceRequestBody{Path: "/speed", Equals: "fast"}},
				Rates: acceptanceRatePatch{Input: acceptanceInt64(2_000_000)},
			}}
			settings["pricing"] = acceptancePricing{Prices: []acceptancePrice{price}}
			var updated acceptanceSettings
			h.api(t, http.MethodPut, "/settings", settings, http.StatusOK, &updated)
			key := h.createKey(t, validSpec())
			conn, err := websocketDial(t, h, "/v1/responses", key.Secret)
			if err != nil {
				t.Fatalf("Observer Key websocket handshake: %v", err)
			}
			defer conn.Close()
			for range 2 {
				websocketTurn(t, conn)
			}

			var records []recordView
			h.await(t, "two websocket usage records and idle key", func() bool {
				page := h.page(t, "page_size=100&key_id="+url.QueryEscape(key.Key.ID))
				keys := h.keys(t)
				if len(page.Items) != 2 || len(keys) != 1 || keys[0].Active != 0 {
					return false
				}
				records = page.Items
				return true
			})
			if records[0].ID == records[1].ID || records[0].TraceID == "" || records[0].TraceID != records[1].TraceID {
				t.Fatalf("websocket record identity/trace mismatch: %+v", records)
			}
			for _, record := range records {
				if !record.Websocket || record.CostMicroUSD != 21 || record.PriceSource != "manual" || record.Failed {
					t.Errorf("unexpected websocket record: %+v", record)
				}
				var detail struct {
					CaptureStatus string          `json:"capture_status"`
					HeadersStatus string          `json:"headers_status"`
					BodyStatus    string          `json:"body_status"`
					CaptureReason string          `json:"capture_reason"`
					Headers       http.Header     `json:"headers"`
					Body          json.RawMessage `json:"body"`
				}
				h.api(t, http.MethodGet, "/requests/detail?id="+url.QueryEscape(record.ID), nil, http.StatusOK, &detail)
				if detail.CaptureStatus != "unsupported" || detail.HeadersStatus != "unsupported" || detail.BodyStatus != "unsupported" || detail.CaptureReason != "websocket" {
					t.Errorf("websocket detail statuses = %+v", detail)
				}
				if len(detail.Headers) != 0 || len(detail.Body) != 0 && string(detail.Body) != "null" {
					t.Errorf("websocket detail unexpectedly includes captured content: headers=%v body=%s", detail.Headers, detail.Body)
				}
			}
			if calls := h.mock.callCount(); calls != 2 {
				t.Errorf("upstream calls = %d, want 2", calls)
			}
		})
	}
}

func TestObserverResponsesWebsocketQuotaAndUnsupportedRoute(t *testing.T) {
	h := websocketHarness(t)
	h.api(t, http.MethodPut, "/settings", func() map[string]any {
		settings := acceptanceGetSettings(t, h)
		settings["pricing"] = acceptancePricing{Prices: []acceptancePrice{acceptanceManual(upstreamModel, acceptanceProvider, 1_000_000)}}
		return settings
	}(), http.StatusOK, nil)
	keySpec := validSpec()
	keySpec.Limits.Total = 1
	key := h.createKey(t, keySpec)
	conn, err := websocketDial(t, h, "/v1/responses", key.Secret)
	if err != nil {
		t.Fatalf("Observer Key websocket handshake: %v", err)
	}
	websocketTurn(t, conn)
	h.await(t, "first websocket usage is charged and concurrency released", func() bool {
		keys := h.keys(t)
		return len(keys) == 1 && keys[0].Used.Total >= 15 && keys[0].Active == 0 && len(h.page(t, "page_size=100").Items) == 1
	})
	calls := h.mock.callCount()
	if err := websocket.Message.Send(conn, `{"type":"response.create","model":"observer-mock-model","input":[{"role":"user","content":[{"type":"input_text","text":"hello-observer"}]}]}`); err != nil {
		t.Fatalf("send quota-rejected turn: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	completed := false
	for {
		var frame string
		if err := websocket.Message.Receive(conn, &frame); err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatalf("quota-rejected websocket did not close: %v", err)
			}
			break // Host versions differ: quota error frame followed by close, or immediate close.
		}
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(frame), &event) == nil && event.Type == "response.completed" {
			completed = true
		}
	}
	_ = conn.Close()
	if completed {
		t.Error("quota-rejected websocket turn completed")
	}
	h.await(t, "quota rejection leaves key idle", func() bool {
		keys := h.keys(t)
		return len(keys) == 1 && keys[0].Active == 0
	})
	if got := h.mock.callCount(); got != calls {
		t.Errorf("quota-rejected websocket turn reached upstream: calls %d -> %d", calls, got)
	}
	if got := len(h.page(t, "page_size=100").Items); got != 1 {
		t.Errorf("quota-rejected websocket turn added usage records: %d", got)
	}

	unsupported, err := websocketDial(t, h, "/backend-api/codex/responses", key.Secret)
	if err == nil {
		_ = unsupported.Close()
		t.Fatal("unsupported codex websocket route unexpectedly upgraded")
	}
}
