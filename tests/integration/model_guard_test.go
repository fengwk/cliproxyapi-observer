//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// requireModelSurfaceDenied asserts an intercepted model surface that Observer
// cannot account for is rejected before any upstream dispatch.
func requireModelSurfaceDenied(t *testing.T, status int, body []byte) {
	t.Helper()
	if status != http.StatusForbidden || !bytes.Contains(body, []byte("model_surface_unsupported")) {
		t.Fatalf("unsupported model surface status=%d body=%s, want 403 model_surface_unsupported", status, truncate(body, 300))
	}
}

// Native model execution is guard-only: with zero Observer Keys, supported
// routes are 403 observer_key_required and additional intercepted model
// surfaces are explicitly denied instead of falling back to native execution.
// Non-model routes are covered by the catalog/health/management baseline test.
func TestUnsupportedModelSurfacesRejectNativeExecution(t *testing.T) {
	h := newHarnessWith(t, hostOptions{UnsupportedModels: true})

	// gpt-image/grok model names are host-supported image/audio/video identities;
	// the harness's local openai-compatibility upstream is a fake and never used.
	cases := []struct {
		name string
		path string
		body []byte
	}{
		{"images", "/v1/images/generations", []byte(`{"model":"gpt-image-1.5","prompt":"observer"}`)},
		{"audio speech", "/v1/audio/speech", []byte(`{"model":"grok-tts","input":"observer","voice":"alloy"}`)},
		{"videos", "/v1/videos", []byte(`{"model":"grok-imagine-video","prompt":"observer"}`)},
		{"responses compact", "/v1/responses/compact", []byte(`{"model":"` + modelName + `","input":"observer"}`)},
		{"interactions", "/v1beta/interactions", []byte(`{"model":"` + modelName + `"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := h.mock.callCount()
			status, body, err := h.rawRequest(http.MethodPost, tc.path, tc.body, map[string]string{"Authorization": "Bearer " + clientKey})
			if err != nil {
				t.Fatal(err)
			}
			requireModelSurfaceDenied(t, status, body)
			if h.mock.callCount() != before {
				t.Fatalf("%s reached the upstream", tc.path)
			}
		})
	}

	// No guard path may create Observer Keys, executions or active admissions.
	var health acceptanceHealth
	h.api(t, http.MethodGet, "/health", nil, http.StatusOK, &health)
	if keys := h.keys(t); len(keys) != 0 {
		t.Fatalf("guarded native traffic created Observer keys: %+v", keys)
	}
	if records := h.page(t, "page_size=100"); len(records.Items) != 0 {
		t.Fatalf("guarded native traffic entered Observer accounting: %+v", records.Items)
	}
	if health.ActiveRequests != 0 || health.PendingUsage != 0 {
		t.Fatalf("guarded native traffic changed Observer health: %+v", health)
	}
}

// A Responses WebSocket uses GET on the same path as the supported HTTP POST.
// The intercepted operation whitelist is method-aware, so an Observer Key must
// not acquire a generation identity for the unsupported WS upgrade.
func TestObserverKeyCannotAuthenticateResponsesWebSocket(t *testing.T) {
	h := newHarness(t)
	created := h.createKey(t, validSpec())
	before := h.mock.callCount()
	status, body, err := h.rawRequest(http.MethodGet, "/v1/responses", nil, map[string]string{
		"Authorization":         "Bearer " + created.Secret,
		"Connection":            "Upgrade",
		"Upgrade":               "websocket",
		"Sec-WebSocket-Key":     "b2JzZXJ2ZXItd3MtdGVzdA==",
		"Sec-WebSocket-Version": "13",
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("Observer Key WS upgrade status=%d body=%s, want 401", status, truncate(body, 300))
	}
	if h.mock.callCount() != before {
		t.Fatal("Observer Key WS upgrade reached the upstream")
	}
}

// rawWebSocket dials the host with a minimal RFC 6455 client so the test does
// not add a production or test dependency. It returns the connection and the
// buffered reader positioned at the first frame.
func rawWebSocket(t *testing.T, h *harness, secret string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(h.baseURL, "http://"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("observer-ws-test"))
	request := fmt.Sprintf("GET /v1/responses HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nAuthorization: Bearer %s\r\n\r\n",
		strings.TrimPrefix(h.baseURL, "http://"), key, secret)
	if _, err = io.WriteString(conn, request); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if !strings.Contains(statusLine, " 101 ") {
		conn.Close()
		t.Fatalf("WebSocket handshake status %q, want 101", strings.TrimSpace(statusLine))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	return conn, reader
}

// writeWebSocketText sends one masked client text frame.
func writeWebSocketText(conn net.Conn, payload []byte) error {
	header := []byte{0x81}
	length := len(payload)
	switch {
	case length < 126:
		header = append(header, byte(0x80|length))
	case length <= 0xffff:
		header = append(header, 0x80|126, byte(length>>8), byte(length))
	default:
		header = append(header, 0x80|127)
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(length))
		header = append(header, size[:]...)
	}
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	header = append(header, mask[:]...)
	masked := make([]byte, length)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := conn.Write(header); err != nil {
		return err
	}
	_, err := conn.Write(masked)
	return err
}

// readWebSocketFrame reads one server frame. Fragmented text is assembled by
// the caller via the fin flag.
func readWebSocketFrame(reader *bufio.Reader) (fin bool, opcode byte, payload []byte, err error) {
	header := make([]byte, 2)
	if _, err = io.ReadFull(reader, header); err != nil {
		return false, 0, nil, err
	}
	fin = header[0]&0x80 != 0
	opcode = header[0] & 0x0f
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		var size [2]byte
		if _, err = io.ReadFull(reader, size[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(size[:]))
	case 127:
		var size [8]byte
		if _, err = io.ReadFull(reader, size[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(size[:])
	}
	if header[1]&0x80 != 0 {
		mask := make([]byte, 4)
		if _, err = io.ReadFull(reader, mask); err != nil {
			return false, 0, nil, err
		}
		payload = make([]byte, length)
		if _, err = io.ReadFull(reader, payload); err != nil {
			return false, 0, nil, err
		}
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
		return fin, opcode, payload, nil
	}
	payload = make([]byte, length)
	_, err = io.ReadFull(reader, payload)
	return fin, opcode, payload, err
}

// A native key may complete the Responses WebSocket handshake; the per-turn
// before hook must still deny execution without dispatching to the fake
// upstream. This is execution denial, not a handshake-status guarantee.
func TestNativeResponsesWebSocketExecutionIsDenied(t *testing.T) {
	h := newHarness(t)
	conn, reader := rawWebSocket(t, h, clientKey)
	defer conn.Close()

	if err := writeWebSocketText(conn, []byte(`{"type":"response.create","model":"`+modelName+`","input":[{"role":"user","content":"observer"}]}`)); err != nil {
		t.Fatal(err)
	}
	var messages bytes.Buffer
	closed := false
	sawCode := false
	for i := 0; i < 32; i++ {
		fin, opcode, payload, err := readWebSocketFrame(reader)
		if err != nil {
			// v8.0.30 suppresses a DirectResponse body on WS (403 with a nil
			// Error means "close without an error event"), so EOF after the
			// turn is the observable denial. Never treat it as success.
			closed = true
			break
		}
		switch opcode {
		case 0x1:
			messages.Write(payload)
			if fin && bytes.Contains(payload, []byte("observer_key_required")) {
				sawCode = true
			}
		case 0x8:
			closed = true
		}
		if sawCode || closed {
			break
		}
	}
	if !sawCode && !closed {
		t.Fatalf("WebSocket turn neither received an observer_key_required event nor closed; messages=%s", truncate(messages.Bytes(), 500))
	}
	if calls := h.mock.callCount(); calls != 0 {
		t.Fatalf("native WebSocket execution reached the fake upstream %d times", calls)
	}
	if records := h.page(t, "page_size=100"); len(records.Items) != 0 {
		t.Fatalf("native WebSocket execution entered Observer accounting: %+v", records.Items)
	}
}
