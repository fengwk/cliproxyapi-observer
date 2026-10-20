package plugin

import (
	"net/http"
	"strings"
)

// Gemini's RequestInterceptor metadata uses Gin's FullPath template, so the
// exact action (generateContent vs countTokens) is not recoverable there. The
// authenticated operation is authoritative for this template.
const geminiActionPathTemplate = "/v1beta/models/*action"

// requestOperation is derived from the host's original HTTP method and path,
// never from body fields or the Generate usage flag (count_tokens also sets it).
type requestOperation uint8

const (
	operationUnknown requestOperation = iota
	operationGeneration
	operationCount
	operationDirectory
)

// modelSurface is the finite set of model execution surfaces that can reach
// the request interceptor. It is deliberately separate from the supported
// billable operations: a surface may be intercepted yet not safely accounted.
type modelSurface uint8

const (
	surfaceNone modelSurface = iota
	surfaceGeneration
	surfaceCount
	surfaceGeminiWildcard
	surfaceUnsupported
)

// unsupportedModelSurfaces are model execution routes that, when they reach
// request.intercept_before, have no safe Observer accounting. They are denied
// explicitly instead of silently falling back to native Keys. Classification
// is not coverage: v8.0.30's alpha-search/live/realtime host entrypoints do not
// call the interceptor, so listing them here only prevents a future hooked path
// from failing open. They remain a documented all-route coverage gap.
var unsupportedModelSurfaces = map[string]struct{}{
	"/v1/images/generations":               {},
	"/v1/images/edits":                     {},
	"/v1/videos":                           {},
	"/v1/videos/generations":               {},
	"/v1/videos/edits":                     {},
	"/v1/videos/extensions":                {},
	"/openai/v1/videos":                    {},
	"/v1/audio/speech":                     {},
	"/v1/tts":                              {},
	"/v1/responses/compact":                {},
	"/backend-api/codex/responses/compact": {},
	"/v1/alpha/search":                     {},
	"/backend-api/codex/alpha/search":      {},
	"/v1beta/interactions":                 {},
	"/v1/live":                             {},
	"/v1/live/:call_id":                    {},
	"/v1/realtime":                         {},
	"/v1/realtime/calls":                   {},
	"/v1/realtime/calls/:call_id":          {},
	"/v1/realtime/translations":            {},
}

// surfaceFor classifies only exact, known interceptor paths. Unknown and
// non-model paths return surfaceNone so the host's own behavior is unchanged.
// Gemini's Gin FullPath cannot reveal the action or HTTP method, so the whole
// wildcard family is one surface governed by the authenticated operation.
func surfaceFor(path string) modelSurface {
	if path == geminiActionPathTemplate {
		return surfaceGeminiWildcard
	}
	switch operationFor(http.MethodPost, path) {
	case operationGeneration:
		return surfaceGeneration
	case operationCount:
		return surfaceCount
	}
	if _, ok := unsupportedModelSurfaces[path]; ok {
		return surfaceUnsupported
	}
	return surfaceNone
}

// operationFor recognizes only the v8.0.30 routes whose model execution and
// accounting semantics Observer actually implements. Everything else stays
// unhandled so the host's own auth and routing remain authoritative.
func operationFor(method, path string) requestOperation {
	switch method {
	case http.MethodPost:
		switch path {
		case "/v1/chat/completions", "/v1/completions", "/v1/messages", "/v1/responses", "/backend-api/codex/responses":
			return operationGeneration
		case "/v1/messages/count_tokens":
			return operationCount
		}
		switch geminiAction(path) {
		case "generateContent", "streamGenerateContent":
			return operationGeneration
		case "countTokens":
			return operationCount
		}
	case http.MethodGet:
		if directoryPath(path) {
			return operationDirectory
		}
	}
	return operationUnknown
}

// geminiAction returns the action from an exact /v1beta/models/{model}:{action}
// path. Model names with separators or control characters are not accepted.
func geminiAction(path string) string {
	const prefix = "/v1beta/models/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	separator := strings.LastIndexByte(rest, ':')
	if separator <= 0 || separator == len(rest)-1 {
		return ""
	}
	model := rest[:separator]
	if model == "." || model == ".." || strings.ContainsAny(model, "/\\:%\x00\r\n \t") {
		return ""
	}
	return rest[separator+1:]
}

// safeModelDetail keeps directory auth inside the documented list/detail
// surface. Traversal segments and control characters are not details.
func safeModelDetail(detail string) bool {
	if detail == "" || strings.ContainsAny(detail, "\\\x00\r\n\t") {
		return false
	}
	for _, segment := range strings.Split(detail, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// directoryPath covers model listing and detail reads only. Empty detail paths
// and Gemini action paths are deliberately left unhandled.
func directoryPath(path string) bool {
	if path == "/v1/models" || path == "/v1beta/models" {
		return true
	}
	if strings.HasPrefix(path, "/v1/models/") {
		return safeModelDetail(strings.TrimPrefix(path, "/v1/models/"))
	}
	if strings.HasPrefix(path, "/v1beta/models/") {
		detail := strings.TrimPrefix(path, "/v1beta/models/")
		return safeModelDetail(detail) && !strings.Contains(detail, ":")
	}
	return false
}

// countTokenPath is kept as the narrow operation predicate used by tests; it
// intentionally shares the same exact-path rules as authentication.
func countTokenPath(path string) bool {
	return operationFor(http.MethodPost, path) == operationCount
}
