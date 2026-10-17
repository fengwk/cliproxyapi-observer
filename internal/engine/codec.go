package engine

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
)

// The same bounded decoder is used by get, scans, pruning and detail reads.
// Only recordData and capture values are compressed; small indexes stay plain.
const maxDecoded = 64 << 20

func encode(v any) ([]byte, error) {
	raw, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	switch v.(type) {
	case recordData, capture:
		if len(raw) > maxDecoded {
			return nil, ErrInvalid
		}
		var b bytes.Buffer
		z := gzip.NewWriter(&b)
		if _, e = z.Write(raw); e != nil {
			return nil, e
		}
		if e = z.Close(); e != nil {
			return nil, e
		}
		return b.Bytes(), nil
	default:
		return raw, nil
	}
}
func decode(b []byte, v any) error {
	if len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b {
		z, e := gzip.NewReader(bytes.NewReader(b))
		if e != nil {
			return e
		}
		defer z.Close()
		b, e = io.ReadAll(io.LimitReader(z, maxDecoded+1))
		if e != nil {
			return e
		}
		if len(b) > maxDecoded {
			return ErrInvalid
		}
	}
	return json.Unmarshal(b, v)
}
