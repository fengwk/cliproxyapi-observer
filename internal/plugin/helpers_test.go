package plugin

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// decodeEnvelope asserts the raw call result is a well-formed RPC envelope.
func decodeEnvelope(t *testing.T, raw []byte) pluginabi.Envelope {
	t.Helper()
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope %s: %v", raw, err)
	}
	return env
}

// mustResult asserts a successful envelope and decodes its result.
func mustResult(t *testing.T, raw []byte, target any) {
	t.Helper()
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("expected ok envelope, got %s", raw)
	}
	if target == nil {
		return
	}
	if err := json.Unmarshal(env.Result, target); err != nil {
		t.Fatalf("decode result %s: %v", env.Result, err)
	}
}

// mustError asserts a failed envelope and returns the error code.
func mustError(t *testing.T, raw []byte) string {
	t.Helper()
	env := decodeEnvelope(t, raw)
	if env.OK || env.Error == nil {
		t.Fatalf("expected error envelope, got %s", raw)
	}
	return env.Error.Code
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
