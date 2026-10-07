package observer

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// redactedPlaceholder replaces the value of a sensitive JSON field. Only the
// field value is replaced; unrelated prompt/code strings are preserved.
const redactedPlaceholder = "[REDACTED]"

// sensitiveJSONFields are matched case-insensitively against object keys.
var sensitiveJSONFields = map[string]struct{}{
	"authorization": {},
	"api_key":       {},
	"api-key":       {},
	"password":      {},
	"secret":        {},
	"access_token":  {},
	"refresh_token": {},
}

func isSensitiveField(key string) bool {
	_, ok := sensitiveJSONFields[strings.ToLower(strings.TrimSpace(key))]
	return ok
}

// redactBody replaces sensitive object field values in a JSON document. It
// returns the (possibly unchanged) body and whether anything was redacted. The
// caller guarantees the input is valid JSON.
func redactBody(body []byte) ([]byte, bool) {
	paths := sensitivePaths(gjson.ParseBytes(body), "")
	if len(paths) == 0 {
		return body, false
	}
	out := body
	for _, path := range paths {
		updated, err := sjson.SetBytes(out, path, redactedPlaceholder)
		if err != nil {
			// A path gjson accepted but sjson rejected should not corrupt the
			// body; skip only that field.
			continue
		}
		out = updated
	}
	return out, true
}

func sensitivePaths(result gjson.Result, prefix string) []string {
	if !result.IsObject() && !result.IsArray() {
		return nil
	}
	var paths []string
	result.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		if result.IsObject() && isSensitiveField(name) {
			paths = append(paths, path)
			return true
		}
		if value.IsObject() || value.IsArray() {
			paths = append(paths, sensitivePaths(value, path)...)
		}
		return true
	})
	return paths
}
