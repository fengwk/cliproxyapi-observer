package observer

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// redactedPlaceholder replaces the value of a sensitive JSON field. Only the
// field value is replaced; unrelated prompt/code strings are preserved.
const redactedPlaceholder = "[REDACTED]"

// maxRedactDepth bounds recursion so pathological nesting cannot exhaust the
// stack; deeper documents fail closed instead of being stored unredacted.
const maxRedactDepth = 64

var errRedactTooDeep = errors.New("json document nesting too deep")

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

// redactBody re-encodes a valid JSON document replacing every sensitive object
// field value with a placeholder. It streams tokens so it preserves object key
// order, duplicate keys and numeric fidelity (json.Number), and it operates on
// decoded key names (so nested "a.b" or backslash keys are handled literally,
// never as gjson-style paths). When nothing is sensitive the original bytes are
// returned untouched. Any decoding/encoding problem fails closed: the caller
// must skip the body rather than persist unredacted content.
func redactBody(body []byte) ([]byte, bool, error) {
	var out bytes.Buffer
	r := redactor{buf: &out}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := r.value(dec, 0); err != nil {
		return nil, false, err
	}
	if !r.redacted {
		return body, false, nil
	}
	return out.Bytes(), true, nil
}

type redactor struct {
	buf      *bytes.Buffer
	redacted bool
}

func (r *redactor) value(dec *json.Decoder, depth int) error {
	if depth > maxRedactDepth {
		return errRedactTooDeep
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return r.object(dec, depth)
		case '[':
			return r.array(dec, depth)
		default:
			return errors.New("unexpected json delimiter")
		}
	case string:
		writeJSONString(r.buf, t)
		return nil
	case json.Number:
		r.buf.WriteString(t.String())
		return nil
	case bool:
		if t {
			r.buf.WriteString("true")
		} else {
			r.buf.WriteString("false")
		}
		return nil
	case nil:
		r.buf.WriteString("null")
		return nil
	default:
		return errors.New("unexpected json token")
	}
}

func (r *redactor) object(dec *json.Decoder, depth int) error {
	r.buf.WriteByte('{')
	first := true
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := keyTok.(string)
		if !ok {
			return errors.New("non-string json object key")
		}
		if !first {
			r.buf.WriteByte(',')
		}
		first = false
		writeJSONString(r.buf, key)
		r.buf.WriteByte(':')
		if isSensitiveField(key) {
			r.redacted = true
			if err := r.skipValue(dec, depth+1); err != nil {
				return err
			}
			writeJSONString(r.buf, redactedPlaceholder)
			continue
		}
		if err := r.value(dec, depth+1); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil { // consume closing '}'
		return err
	}
	r.buf.WriteByte('}')
	return nil
}

func (r *redactor) array(dec *json.Decoder, depth int) error {
	r.buf.WriteByte('[')
	first := true
	for dec.More() {
		if !first {
			r.buf.WriteByte(',')
		}
		first = false
		if err := r.value(dec, depth+1); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil { // consume closing ']'
		return err
	}
	r.buf.WriteByte(']')
	return nil
}

// skipValue consumes a complete JSON value without emitting it.
func (r *redactor) skipValue(dec *json.Decoder, depth int) error {
	if depth > maxRedactDepth {
		return errRedactTooDeep
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		for dec.More() {
			if _, err := dec.Token(); err != nil { // key
				return err
			}
			if err := r.skipValue(dec, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := r.skipValue(dec, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected json delimiter")
	}
	_, err = dec.Token() // consume closing delimiter
	return err
}

const jsonHexDigits = "0123456789abcdef"

// writeJSONString writes s as a JSON string without HTML escaping. The decoder
// already normalised the input to valid UTF-8, so multi-byte runes pass through.
func writeJSONString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		buf.WriteString(s[start:i])
		switch c {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		default:
			buf.WriteString(`\u00`)
			buf.WriteByte(jsonHexDigits[c>>4])
			buf.WriteByte(jsonHexDigits[c&0x0f])
		}
		start = i + 1
	}
	buf.WriteString(s[start:])
	buf.WriteByte('"')
}
