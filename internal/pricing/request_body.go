package pricing

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxRequestBodyPathBytes    = 512
	maxRequestBodySegments     = 16
	maxRequestBodyStringBytes  = 1024
	maxRequestBodyNumberDigits = 64
	maxRequestBodyNumberScale  = 18
	maxRequestJSONBytes        = 32 << 20
	maxRequestJSONDepth        = 64
	maxRequestConditions       = 128
)

const (
	scalarString  = "string"
	scalarNumber  = "number"
	scalarBoolean = "boolean"
	scalarNull    = "null"
)

var jsonNumberPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

// JSONScalar is one typed JSON scalar accepted by a requestBody condition.
// Number text is stored in plain normalized decimal form, never float64.
type JSONScalar struct {
	kind string
	text string
}

func JSONString(v string) JSONScalar { return JSONScalar{kind: scalarString, text: v} }
func JSONBool(v bool) JSONScalar {
	if v {
		return JSONScalar{kind: scalarBoolean, text: "true"}
	}
	return JSONScalar{kind: scalarBoolean, text: "false"}
}
func JSONNull() JSONScalar { return JSONScalar{kind: scalarNull} }

// JSONNumber validates and canonicalizes a JSON number without binary floats.
func JSONNumber(v string) (JSONScalar, error) {
	n, ok := normalizeExpectedNumber(v)
	if !ok {
		return JSONScalar{}, errInvalid
	}
	return JSONScalar{kind: scalarNumber, text: n}, nil
}

func (v JSONScalar) Kind() string { return v.kind }
func (v JSONScalar) Text() string { return v.text }

func (v JSONScalar) valid() bool {
	switch v.kind {
	case scalarString:
		return utf8.ValidString(v.text) && len(v.text) <= maxRequestBodyStringBytes
	case scalarNumber:
		n, ok := normalizeExpectedNumber(v.text)
		return ok && n == v.text
	case scalarBoolean:
		return v.text == "true" || v.text == "false"
	case scalarNull:
		return v.text == ""
	default:
		return false
	}
}

func (v JSONScalar) MarshalJSON() ([]byte, error) {
	if !v.valid() {
		return nil, errInvalid
	}
	switch v.kind {
	case scalarString:
		return json.Marshal(v.text)
	case scalarNumber:
		return []byte(v.text), nil
	case scalarBoolean:
		return []byte(v.text), nil
	default:
		return []byte("null"), nil
	}
}

func (v *JSONScalar) UnmarshalJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return errInvalid
	}
	switch trimmed[0] {
	case '"':
		var s string
		if json.Unmarshal(trimmed, &s) != nil || !utf8.ValidString(s) || len(s) > maxRequestBodyStringBytes {
			return errInvalid
		}
		*v = JSONString(s)
	case 't':
		if !bytes.Equal(trimmed, []byte("true")) {
			return errInvalid
		}
		*v = JSONBool(true)
	case 'f':
		if !bytes.Equal(trimmed, []byte("false")) {
			return errInvalid
		}
		*v = JSONBool(false)
	case 'n':
		if !bytes.Equal(trimmed, []byte("null")) {
			return errInvalid
		}
		*v = JSONNull()
	default:
		var n json.Number
		if json.Unmarshal(trimmed, &n) != nil || !jsonNumberPattern.MatchString(string(n)) {
			return errInvalid
		}
		normalized, ok := normalizeExpectedNumber(string(n))
		if !ok {
			return errInvalid
		}
		*v = JSONScalar{kind: scalarNumber, text: normalized}
	}
	return nil
}

// RequestBodyCondition matches one scalar value at an RFC 6901 JSON Pointer.
// Both fields are required; an explicit null expected value is valid.
type RequestBodyCondition struct {
	Path   string     `json:"path"`
	Equals JSONScalar `json:"equals"`
}

func (c RequestBodyCondition) valid() bool {
	_, ok := decodePointer(c.Path)
	return ok && c.Equals.valid()
}

func (c *RequestBodyCondition) UnmarshalJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errInvalid
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(trimmed, &fields) != nil || len(fields) != 2 {
		return errInvalid
	}
	pathRaw, pathOK := fields["path"]
	equalsRaw, equalsOK := fields["equals"]
	if !pathOK || !equalsOK {
		return errInvalid
	}
	var path string
	if json.Unmarshal(pathRaw, &path) != nil {
		return errInvalid
	}
	var equals JSONScalar
	if equals.UnmarshalJSON(equalsRaw) != nil {
		return errInvalid
	}
	next := RequestBodyCondition{Path: path, Equals: equals}
	if !next.valid() {
		return errInvalid
	}
	*c = next
	return nil
}

// Fingerprint is the lowercase SHA256 over path, scalar type and canonical
// expected value. The scalar value is never stored with a request snapshot.
func (c RequestBodyCondition) Fingerprint() string {
	sum := sha256.Sum256([]byte(c.Path + "\x00" + c.Equals.kind + "\x00" + c.Equals.text))
	return hex.EncodeToString(sum[:])
}

func decodePointer(path string) ([]string, bool) {
	if path == "" || path[0] != '/' || len(path) > maxRequestBodyPathBytes || !utf8.ValidString(path) {
		return nil, false
	}
	parts := strings.Split(path[1:], "/")
	if len(parts) > maxRequestBodySegments {
		return nil, false
	}
	out := make([]string, len(parts))
	for i, part := range parts {
		var b strings.Builder
		for j := 0; j < len(part); j++ {
			if part[j] != '~' {
				b.WriteByte(part[j])
				continue
			}
			if j+1 >= len(part) || part[j+1] != '0' && part[j+1] != '1' {
				return nil, false
			}
			j++
			if part[j] == '0' {
				b.WriteByte('~')
			} else {
				b.WriteByte('/')
			}
		}
		out[i] = b.String()
	}
	return out, true
}

func arrayIndex(segment string) (int64, bool) {
	if segment == "" || len(segment) > 18 || len(segment) > 1 && segment[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(segment); i++ {
		if segment[i] < '0' || segment[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(segment, 10, 64)
	return n, err == nil
}

type decimalForm struct {
	negative bool
	digits   string
	exp      int64
	zero     bool
}

func parseDecimal(text string) (decimalForm, bool) {
	if !jsonNumberPattern.MatchString(text) {
		return decimalForm{}, false
	}
	negative := strings.HasPrefix(text, "-")
	if negative {
		text = text[1:]
	}
	mantissa := text
	exp := int64(0)
	if i := strings.IndexAny(mantissa, "eE"); i >= 0 {
		expText := mantissa[i+1:]
		mantissa = mantissa[:i]
		sign := int64(1)
		if expText[0] == '+' || expText[0] == '-' {
			if expText[0] == '-' {
				sign = -1
			}
			expText = expText[1:]
		}
		expText = strings.TrimLeft(expText, "0")
		if expText == "" {
			exp = 0
		} else if len(expText) > 10 {
			exp = sign * (1 << 30)
		} else {
			n, err := strconv.ParseInt(expText, 10, 64)
			if err != nil {
				exp = sign * (1 << 30)
			} else {
				exp = sign * n
			}
		}
	}
	frac := int64(0)
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		frac = int64(len(mantissa) - i - 1)
		mantissa = mantissa[:i] + mantissa[i+1:]
	}
	start := 0
	for start < len(mantissa) && mantissa[start] == '0' {
		start++
	}
	if start == len(mantissa) {
		return decimalForm{zero: true}, true
	}
	digits := mantissa[start:]
	end := len(digits)
	for end > 0 && digits[end-1] == '0' {
		end--
	}
	trailing := int64(len(digits) - end)
	digits = digits[:end]
	exp = exp - frac + trailing
	return decimalForm{negative: negative, digits: digits, exp: exp}, true
}

func normalizeExpectedNumber(text string) (string, bool) {
	f, ok := parseDecimal(text)
	if !ok {
		return "", false
	}
	if f.zero {
		return "0", true
	}
	if len(f.digits) > maxRequestBodyNumberDigits || f.exp < -maxRequestBodyNumberScale || f.exp > maxRequestBodyNumberScale {
		return "", false
	}
	var b strings.Builder
	if f.negative {
		b.WriteByte('-')
	}
	if f.exp >= 0 {
		b.WriteString(f.digits)
		for i := int64(0); i < f.exp; i++ {
			b.WriteByte('0')
		}
		return b.String(), true
	}
	point := int64(len(f.digits)) + f.exp
	if point > 0 {
		b.WriteString(f.digits[:point])
		b.WriteByte('.')
		b.WriteString(f.digits[point:])
		return b.String(), true
	}
	b.WriteString("0.")
	for i := point; i < 0; i++ {
		b.WriteByte('0')
	}
	b.WriteString(f.digits)
	return b.String(), true
}

func numbersEqual(expected, actual string) bool {
	e, eok := parseDecimal(expected)
	a, aok := parseDecimal(actual)
	if !eok || !aok || e.zero || a.zero {
		return eok && aok && e.zero && a.zero
	}
	return e.negative == a.negative && e.digits == a.digits && e.exp == a.exp
}

type requestLeaf struct {
	fingerprint string
	equals      JSONScalar
}

type requestNode struct {
	children map[string]*requestNode
	indices  map[int64]*requestNode
	leaves   []requestLeaf
}

func (n *requestNode) childKey(key string) *requestNode {
	if n == nil {
		return nil
	}
	return n.children[key]
}

func (n *requestNode) childIndex(index int64) *requestNode {
	if n == nil {
		return nil
	}
	return n.indices[index]
}

// addRequestConditionEdges builds both object-key and canonical array-index edges so
// object keys such as "01" remain distinct from array index 1.
func addRequestConditionEdges(root *requestNode, c RequestBodyCondition) {
	segments, ok := decodePointer(c.Path)
	if !ok {
		return
	}
	node := root
	for _, segment := range segments {
		if node.children == nil {
			node.children = map[string]*requestNode{}
		}
		child := node.children[segment]
		if child == nil {
			child = &requestNode{}
			node.children[segment] = child
		}
		if index, ok := arrayIndex(segment); ok {
			if node.indices == nil {
				node.indices = map[int64]*requestNode{}
			}
			if node.indices[index] == nil {
				node.indices[index] = child
			}
		}
		node = child
	}
	node.leaves = append(node.leaves, requestLeaf{fingerprint: c.Fingerprint(), equals: c.Equals})
}

func snapshotRequestBody(body []byte, conditions []RequestBodyCondition) map[string]bool {
	if len(conditions) == 0 || len(body) > maxRequestJSONBytes {
		return nil
	}
	results := make(map[string]bool, len(conditions))
	root := &requestNode{}
	for _, condition := range conditions {
		if !condition.valid() {
			continue
		}
		results[condition.Fingerprint()] = false
		addRequestConditionEdges(root, condition)
	}
	if len(results) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	w := requestWalker{decoder: decoder, results: results}
	if err := w.scan(root, 0); err != nil {
		return nil
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil
	}
	return results
}

type requestWalker struct {
	decoder *json.Decoder
	results map[string]bool
}

func (w *requestWalker) scan(node *requestNode, depth int) error {
	token, err := w.decoder.Token()
	if err != nil {
		return err
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			if depth+1 > maxRequestJSONDepth {
				return errInvalid
			}
			seen := map[string]bool{}
			for w.decoder.More() {
				rawKey, err := w.decoder.Token()
				if err != nil {
					return err
				}
				key, ok := rawKey.(string)
				if !ok || seen[key] {
					return errInvalid
				}
				seen[key] = true
				if err := w.scan(node.childKey(key), depth+1); err != nil {
					return err
				}
			}
			end, err := w.decoder.Token()
			if err != nil || end != json.Delim('}') {
				return errInvalid
			}
		case '[':
			if depth+1 > maxRequestJSONDepth {
				return errInvalid
			}
			var index int64
			for w.decoder.More() {
				if err := w.scan(node.childIndex(index), depth+1); err != nil {
					return err
				}
				index++
			}
			end, err := w.decoder.Token()
			if err != nil || end != json.Delim(']') {
				return errInvalid
			}
		default:
			return errInvalid
		}
	case string:
		w.match(node, JSONString(value))
	case json.Number:
		w.matchNumber(node, value.String())
	case bool:
		w.match(node, JSONBool(value))
	case nil:
		w.match(node, JSONNull())
	default:
		return errInvalid
	}
	return nil
}

func (w *requestWalker) matchNumber(node *requestNode, actual string) {
	if node == nil {
		return
	}
	for _, leaf := range node.leaves {
		if leaf.equals.kind == scalarNumber && numbersEqual(leaf.equals.text, actual) {
			w.results[leaf.fingerprint] = true
		}
	}
}

func (w *requestWalker) match(node *requestNode, actual JSONScalar) {
	if node == nil {
		return
	}
	for _, leaf := range node.leaves {
		if leaf.equals.kind == actual.kind && leaf.equals.text == actual.text {
			w.results[leaf.fingerprint] = true
		}
	}
}

// manualRequestBodyConditions returns only the manual-price requestBody leaves,
// deduplicated by fingerprint, in deterministic definition order.
func manualRequestBodyConditions(c Config) []RequestBodyCondition {
	out := make([]RequestBodyCondition, 0)
	seen := map[string]bool{}
	var walk func(Condition)
	walk = func(condition Condition) {
		if len(out) >= maxRequestConditions {
			return
		}
		if condition.RequestBody != nil && condition.RequestBody.valid() {
			fingerprint := condition.RequestBody.Fingerprint()
			if !seen[fingerprint] {
				seen[fingerprint] = true
				out = append(out, *condition.RequestBody)
			}
		}
		for _, child := range condition.All {
			walk(child)
		}
		for _, child := range condition.Any {
			walk(child)
		}
		if condition.Not != nil {
			walk(*condition.Not)
		}
	}
	for _, price := range c.Prices {
		for _, override := range price.Definition.Overrides {
			walk(override.When)
			if len(out) >= maxRequestConditions {
				return out
			}
		}
		for _, multiplier := range price.Definition.Multipliers {
			walk(multiplier.When)
			if len(out) >= maxRequestConditions {
				return out
			}
		}
	}
	return out
}

// SnapshotConditions evaluates configured manual requestBody leaves against one
// transient request body. The result contains fingerprints only; no request
// value or body is retained. Invalid/oversized bodies and missing conditions
// produce no entry, which callers treat as unknown.
func (s *Service) SnapshotConditions(body []byte) map[string]bool {
	s.mu.Lock()
	conditions := append([]RequestBodyCondition(nil), s.requestConditions...)
	s.mu.Unlock()
	return snapshotRequestBody(body, conditions)
}
