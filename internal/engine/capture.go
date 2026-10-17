package engine

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

type capture struct {
	RequestID string
	At        time.Time
	Headers   http.Header
	Gzip      []byte
	Status    string
}

func captureBytes(b *bolt.Bucket) int64 {
	var size int64
	b.ForEach(func(k, v []byte) error { size += int64(len(k) + len(v)); return nil })
	return size
}
func trimCaptures(b *bolt.Bucket, limit int64) error {
	size := captureBytes(b)
	c := b.Cursor()
	for k, v := c.First(); k != nil && size > limit; k, v = c.Next() {
		size -= int64(len(k) + len(v))
		if e := c.Delete(); e != nil {
			return e
		}
	}
	return nil
}

func captureID(key, trace string) string {
	v, _ := json.Marshal([]string{key, trace})
	return string(v)
}

// correlate never treats an execution ID as an admission identity.
func correlate(tx *bolt.Tx, key, trace, request string) error {
	if trace == "" || request == "" {
		return nil
	}
	b := tx.Bucket([]byte("correlations"))
	id := captureID(key, trace)
	old := b.Get([]byte(id))
	if old != nil && string(old) != request {
		if e := tx.Bucket([]byte("captures")).Delete([]byte(id)); e != nil {
			return e
		}
		return b.Put([]byte(id), []byte{0})
	}
	return b.Put([]byte(id), []byte(request))
}
func secretField(k string) bool {
	k = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(k, "-", ""), "_", ""))
	return strings.Contains(k, "authorization") || strings.Contains(k, "apikey") || strings.Contains(k, "token") || strings.Contains(k, "secret") || strings.Contains(k, "password") || strings.Contains(k, "cookie") || k == "key"
}
func redactURL(text string) (string, bool) {
	u, e := url.Parse(text)
	if e != nil || u.Scheme == "" || u.Host == "" {
		return text, false
	}
	changed := false
	if u.User != nil {
		u.User = nil
		changed = true
	}
	q := u.Query()
	for k := range q {
		if secretField(k) {
			q.Set(k, "[REDACTED]")
			changed = true
		}
	}
	if changed {
		u.RawQuery = q.Encode()
		return u.String(), true
	}
	return text, false
}
func redactValue(v any) (any, bool) {
	switch x := v.(type) {
	case string:
		return redactURL(x)
	case map[string]any:
		changed := false
		for k, a := range x {
			if secretField(k) {
				x[k] = "[REDACTED]"
				changed = true
			} else if b, ok := redactValue(a); ok {
				x[k] = b
				changed = true
			}
		}
		return x, changed
	case []any:
		changed := false
		for i, a := range x {
			if b, ok := redactValue(a); ok {
				x[i] = b
				changed = true
			}
		}
		return x, changed
	}
	return v, false
}
func (s *Store) Capture(v CaptureInput) bool {
	if !s.mu.TryLock() {
		s.dropped.Add(1)
		return false
	}
	defer s.mu.Unlock()
	if s.closed || !s.settings.CaptureEnabled || v.KeyID == "" || v.TraceID == "" || v.RequestID == "" {
		return false
	}
	if len(v.Body) > s.settings.MaxBodyBytes {
		s.dropped.Add(1)
		return false
	}
	v.Body = bytes.Clone(v.Body)
	headers := http.Header{}
	size := 0
	for _, k := range []string{"Content-Type", "Accept", "User-Agent"} {
		for _, value := range v.Headers.Values(k) {
			size += len(value)
			if size > 16<<10 {
				s.dropped.Add(1)
				return false
			}
			value, _ = redactURL(value)
			headers.Add(k, value)
		}
	}
	v.Headers = headers
	if v.At.IsZero() {
		v.At = s.now().UTC()
	}
	select {
	case s.captures <- v:
		return true
	default:
		s.dropped.Add(1)
		return false
	}
}
func (s *Store) captureWorker() {
	defer s.worker.Done()
	for v := range s.captures {
		c := capture{RequestID: v.RequestID, At: v.At.UTC(), Headers: http.Header{}, Status: "available"}
		// Only an explicit safe header allowlist: unknown URL/query-bearing headers
		// and upstream credential headers are never persisted.
		for _, k := range []string{"Content-Type", "Accept", "User-Agent"} {
			if a := v.Headers.Values(k); len(a) > 0 {
				c.Headers[k] = a
			}
		}
		var body any
		dec := json.NewDecoder(bytes.NewReader(v.Body))
		dec.UseNumber()
		if e := dec.Decode(&body); e != nil {
			c.Status = "redacted"
			body = nil
		} else {
			var extra any
			if dec.Decode(&extra) != io.EOF {
				body = nil
				c.Status = "redacted"
			} else if redacted, changed := redactValue(body); changed {
				body = redacted
				c.Status = "redacted"
			}
		}
		raw, _ := json.Marshal(body)
		var buf bytes.Buffer
		z := gzip.NewWriter(&buf)
		z.Write(raw)
		z.Close()
		c.Gzip = buf.Bytes()
		s.mu.Lock()
		if !s.settings.CaptureEnabled || len(v.Body) > s.settings.MaxBodyBytes {
			s.dropped.Add(1)
			s.mu.Unlock()
			continue
		}
		e := s.db.Update(func(tx *bolt.Tx) error {
			var key storedKey
			if e := get(tx.Bucket([]byte("keys")), v.KeyID, &key); e != nil {
				return e
			}
			if e := correlate(tx, v.KeyID, v.TraceID, v.RequestID); e != nil {
				return e
			}
			b := tx.Bucket([]byte("captures"))
			id := captureID(v.KeyID, v.TraceID)
			if string(tx.Bucket([]byte("correlations")).Get([]byte(id))) == "\x00" {
				return nil
			}
			if old := b.Get([]byte(id)); old != nil {
				var previous capture
				if e := decode(old, &previous); e != nil {
					return e
				}
				if previous.RequestID == v.RequestID && v.RequestID != "" {
					return nil
				}
				previous.Headers = http.Header{}
				previous.Gzip = nil
				previous.Status = "missing"
				return put(b, id, previous)
			}
			p, e := json.Marshal(c)
			if e != nil {
				return e
			}
			size := captureBytes(b)
			if size+int64(len(id)+len(p)) > s.settings.MaxBodyStorageBytes {
				return ErrBusy
			}
			return b.Put([]byte(id), p)
		})
		if e != nil {
			s.dropped.Add(1)
		}
		s.mu.Unlock()
	}
}
func (s *Store) Detail(id string) (Detail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := Detail{Headers: http.Header{}, Body: json.RawMessage("null"), CaptureStatus: "missing"}
	if e := s.check(); e != nil {
		return d, e
	}
	e := s.db.View(func(tx *bolt.Tx) error {
		var stored recordData
		if e := get(tx.Bucket([]byte("records")), id, &stored); e != nil {
			return e
		}
		d.Record = stored.Record
		cut := s.now().Add(-time.Duration(s.settings.RawRetentionHours) * time.Hour)
		if d.Record.Time.Before(cut) {
			return ErrNotFound
		}
		if !s.settings.CaptureEnabled || !stored.CaptureEnabled {
			d.CaptureStatus = "disabled"
			return nil
		}
		var c capture
		if e := get(tx.Bucket([]byte("captures")), captureID(d.Record.KeyID, d.Record.TraceID), &c); e != nil {
			if e == ErrNotFound {
				return nil
			}
			return e
		}
		if c.At.Before(cut) {
			d.CaptureStatus = "expired"
			return nil
		}
		if c.Status == "missing" {
			return nil
		}
		z, e := gzip.NewReader(bytes.NewReader(c.Gzip))
		if e != nil {
			return e
		}
		defer z.Close()
		raw, e := io.ReadAll(io.LimitReader(z, int64(s.settings.MaxBodyBytes)+1))
		if e != nil {
			return e
		}
		if len(raw) > s.settings.MaxBodyBytes {
			d.CaptureStatus = "truncated"
			return nil
		}
		d.Headers = c.Headers
		d.Body = raw
		d.CaptureStatus = c.Status
		return nil
	})
	return d, e
}
