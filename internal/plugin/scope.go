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

// beforeOperation classifies the host interceptor path. The boolean reports the
// Gemini wildcard template, where only the authenticated operation can decide
// between generation and counting.
func beforeOperation(path string) (operation requestOperation, template bool) {
	if path == geminiActionPathTemplate {
		return operationUnknown, true
	}
	return operationFor(http.MethodPost, path), false
}
