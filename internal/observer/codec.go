package observer

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math"
	"time"
)

// Sentinel errors callers (management layer) can classify.
var (
	// ErrClosed is returned once the store has been closed.
	ErrClosed = errors.New("observer store closed")
	// ErrInvalidCursor marks a malformed or unverifiable pagination cursor.
	ErrInvalidCursor = errors.New("invalid cursor")
	// ErrInvalidQuery marks an incoherent query range.
	ErrInvalidQuery = errors.New("invalid query")
)

// Request keys are fixed-width timestamp+sequence tuples so bbolt orders them
// chronologically and every execution keeps a stable total order.
const requestKeyLen = 16

func encodeRequestKey(t time.Time, seq uint64) []byte {
	b := make([]byte, requestKeyLen)
	binary.BigEndian.PutUint64(b, uint64(t.UnixNano()))
	binary.BigEndian.PutUint64(b[8:], seq)
	return b
}

func requestKeyTime(key []byte) (time.Time, bool) {
	if len(key) < 8 {
		return time.Time{}, false
	}
	return time.Unix(0, int64(binary.BigEndian.Uint64(key))).UTC(), true
}

func encodeCursor(t time.Time, seq uint64) string {
	return base64.RawURLEncoding.EncodeToString(encodeRequestKey(t, seq))
}

func decodeCursor(raw string) (time.Time, uint64, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) != requestKeyLen {
		return time.Time{}, 0, ErrInvalidCursor
	}
	return time.Unix(0, int64(binary.BigEndian.Uint64(decoded))).UTC(),
		binary.BigEndian.Uint64(decoded[8:]), nil
}

// Stats keys are minute(8) + provider + 0x00 + model. The fixed minute prefix
// keeps the bucket ordered by time, so Summary seeks the selected range.
func statsKey(minute int64, provider, model string) []byte {
	b := make([]byte, 8+len(provider)+1+len(model))
	binary.BigEndian.PutUint64(b, uint64(minute))
	copy(b[8:], provider)
	b[8+len(provider)] = 0
	copy(b[9+len(provider):], model)
	return b
}

func parseStatsKey(key []byte) (int64, string, string, bool) {
	if len(key) < 9 {
		return 0, "", "", false
	}
	minute := int64(binary.BigEndian.Uint64(key))
	rest := key[8:]
	idx := bytes.IndexByte(rest, 0)
	if idx < 0 {
		return 0, "", "", false
	}
	return minute, string(rest[:idx]), string(rest[idx+1:]), true
}

func statsMinute(t time.Time) int64 {
	return t.Truncate(time.Minute).Unix()
}

// Counter records use a fixed 15x8 byte layout (14 uint64 + one float64) so
// aggregation never needs JSON decoding.
const countersSize = 15 * 8

func encodeCounters(c Counters) []byte {
	b := make([]byte, countersSize)
	put := func(i int, v uint64) { binary.BigEndian.PutUint64(b[i*8:], v) }
	put(0, c.Requests)
	put(1, c.FailedRequests)
	put(2, c.InputTokens)
	put(3, c.OutputTokens)
	put(4, c.ReasoningTokens)
	put(5, c.CacheReadTokens)
	put(6, c.CacheCreationTokens)
	put(7, c.TotalTokens)
	put(8, c.CacheHits)
	put(9, c.LatencyNS)
	put(10, c.LatencySamples)
	put(11, c.TTFTNS)
	put(12, c.TTFTSamples)
	put(13, c.UnpricedRequests)
	binary.BigEndian.PutUint64(b[14*8:], math.Float64bits(c.CostUSD))
	return b
}

func decodeCounters(b []byte) (Counters, bool) {
	if len(b) != countersSize {
		return Counters{}, false
	}
	get := func(i int) uint64 { return binary.BigEndian.Uint64(b[i*8:]) }
	return Counters{
		Requests:            get(0),
		FailedRequests:      get(1),
		InputTokens:         get(2),
		OutputTokens:        get(3),
		ReasoningTokens:     get(4),
		CacheReadTokens:     get(5),
		CacheCreationTokens: get(6),
		TotalTokens:         get(7),
		CacheHits:           get(8),
		LatencyNS:           get(9),
		LatencySamples:      get(10),
		TTFTNS:              get(11),
		TTFTSamples:         get(12),
		UnpricedRequests:    get(13),
		CostUSD:             math.Float64frombits(get(14)),
	}, true
}

func addRequestCounters(c *Counters, r Request) {
	c.Requests = addSat(c.Requests, 1)
	if r.Failed {
		c.FailedRequests = addSat(c.FailedRequests, 1)
	}
	addNonNegative := func(dst *uint64, v int64) {
		if v > 0 {
			*dst = addSat(*dst, uint64(v))
		}
	}
	addNonNegative(&c.InputTokens, r.InputTokens)
	addNonNegative(&c.OutputTokens, r.OutputTokens)
	addNonNegative(&c.ReasoningTokens, r.ReasoningTokens)
	addNonNegative(&c.CacheReadTokens, r.CacheReadTokens)
	addNonNegative(&c.CacheCreationTokens, r.CacheCreationTokens)
	addNonNegative(&c.TotalTokens, r.TotalTokens)
	if r.CacheHit {
		c.CacheHits = addSat(c.CacheHits, 1)
	}
	if r.LatencyNS > 0 {
		c.LatencyNS = addSat(c.LatencyNS, uint64(r.LatencyNS))
		c.LatencySamples = addSat(c.LatencySamples, 1)
	}
	if r.TTFTNS > 0 {
		c.TTFTNS = addSat(c.TTFTNS, uint64(r.TTFTNS))
		c.TTFTSamples = addSat(c.TTFTSamples, 1)
	}
	if r.CostUSD != nil {
		c.CostUSD = addCostSat(c.CostUSD, *r.CostUSD)
	} else {
		c.UnpricedRequests = addSat(c.UnpricedRequests, 1)
	}
}

func addCounters(dst *Counters, src Counters) {
	dst.Requests = addSat(dst.Requests, src.Requests)
	dst.FailedRequests = addSat(dst.FailedRequests, src.FailedRequests)
	dst.InputTokens = addSat(dst.InputTokens, src.InputTokens)
	dst.OutputTokens = addSat(dst.OutputTokens, src.OutputTokens)
	dst.ReasoningTokens = addSat(dst.ReasoningTokens, src.ReasoningTokens)
	dst.CacheReadTokens = addSat(dst.CacheReadTokens, src.CacheReadTokens)
	dst.CacheCreationTokens = addSat(dst.CacheCreationTokens, src.CacheCreationTokens)
	dst.TotalTokens = addSat(dst.TotalTokens, src.TotalTokens)
	dst.CacheHits = addSat(dst.CacheHits, src.CacheHits)
	dst.LatencyNS = addSat(dst.LatencyNS, src.LatencyNS)
	dst.LatencySamples = addSat(dst.LatencySamples, src.LatencySamples)
	dst.TTFTNS = addSat(dst.TTFTNS, src.TTFTNS)
	dst.TTFTSamples = addSat(dst.TTFTSamples, src.TTFTSamples)
	dst.CostUSD = addCostSat(dst.CostUSD, src.CostUSD)
	dst.UnpricedRequests = addSat(dst.UnpricedRequests, src.UnpricedRequests)
}

// addSat adds two uint64 counters, saturating at MaxUint64 instead of wrapping.
func addSat(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

// addCostSat adds costs, ensuring a non-finite operand cannot poison
// aggregates: the finite operand wins, and two non-finite operands clamp.
func addCostSat(a, b float64) float64 {
	aFinite := !math.IsNaN(a) && !math.IsInf(a, 0)
	bFinite := !math.IsNaN(b) && !math.IsInf(b, 0)
	if !aFinite && !bFinite {
		return math.MaxFloat64
	}
	if !aFinite {
		return b
	}
	if !bFinite {
		return a
	}
	sum := a + b
	if math.IsInf(sum, 0) {
		return math.MaxFloat64
	}
	return sum
}

// Body metadata is kept separate from the content bucket so BodyAvailable can
// be computed without reading body bytes.
const bodyMetaFixed = 8 + 8 + 8 + 1 + 2 // created, expires, size, flags, traceLen

type bodyMeta struct {
	createdAt time.Time
	expiresAt time.Time
	size      int64
	redacted  bool
	traceID   string
}

func encodeBodyMeta(m bodyMeta) []byte {
	b := make([]byte, bodyMetaFixed+len(m.traceID))
	binary.BigEndian.PutUint64(b, uint64(m.createdAt.UnixNano()))
	binary.BigEndian.PutUint64(b[8:], uint64(m.expiresAt.UnixNano()))
	binary.BigEndian.PutUint64(b[16:], uint64(m.size))
	if m.redacted {
		b[24] = 1
	}
	binary.BigEndian.PutUint16(b[25:], uint16(len(m.traceID)))
	copy(b[bodyMetaFixed:], m.traceID)
	return b
}

func decodeBodyMeta(b []byte) (bodyMeta, bool) {
	if len(b) < bodyMetaFixed {
		return bodyMeta{}, false
	}
	traceLen := int(binary.BigEndian.Uint16(b[25:]))
	if len(b) != bodyMetaFixed+traceLen {
		return bodyMeta{}, false
	}
	return bodyMeta{
		createdAt: time.Unix(0, int64(binary.BigEndian.Uint64(b))).UTC(),
		expiresAt: time.Unix(0, int64(binary.BigEndian.Uint64(b[8:]))).UTC(),
		size:      int64(binary.BigEndian.Uint64(b[16:])),
		redacted:  b[24]&1 == 1,
		traceID:   string(b[bodyMetaFixed:]),
	}, true
}

func encodeBodyIndexValue(size int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(size))
	return b
}

func bodyIndexKey(createdAt time.Time, requestID string) []byte {
	b := make([]byte, 8+len(requestID))
	binary.BigEndian.PutUint64(b, uint64(createdAt.UnixNano()))
	copy(b[8:], requestID)
	return b
}

func decodeBodyIndexCreated(key []byte) (time.Time, bool) {
	if len(key) < 8 {
		return time.Time{}, false
	}
	return time.Unix(0, int64(binary.BigEndian.Uint64(key))).UTC(), true
}

func decodeBodyIndexSize(value []byte) (int64, bool) {
	if len(value) != 8 {
		return 0, false
	}
	return int64(binary.BigEndian.Uint64(value)), true
}
