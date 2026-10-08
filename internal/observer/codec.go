package observer

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"time"
)

// Sentinel errors callers (management layer) can classify.
var (
	// ErrClosed is returned once the store has been closed.
	ErrClosed = errors.New("observer store closed")
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

// Version 1: a version byte, 13 original usage counters and five billable
// counters. No price, monetary cost or price-dependent unpriced count.
const (
	legacyCountersSize = 15 * 8
	countersSize       = 1 + 18*8
)

func encodeCounters(c Counters) []byte {
	b := make([]byte, countersSize)
	b[0] = 1
	put := func(i int, v uint64) { binary.BigEndian.PutUint64(b[1+i*8:], v) }
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
	put(13, c.completeRequests)
	put(14, c.billableInput)
	put(15, c.billableRead)
	put(16, c.billableWrite)
	put(17, c.billableOutput)
	return b
}

func decodeCounters(b []byte) (Counters, bool) {
	legacy := len(b) == legacyCountersSize
	if !legacy && (len(b) != countersSize || b[0] != 1) {
		return Counters{}, false
	}
	if !legacy {
		b = b[1:]
	}
	get := func(i int) uint64 { return binary.BigEndian.Uint64(b[i*8:]) }
	c := Counters{
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
	}
	if legacy {
		c.UnpricedRequests = get(13)
		return c, c.UnpricedRequests <= c.Requests
	}
	c.completeRequests = get(13)
	c.billableInput = get(14)
	c.billableRead = get(15)
	c.billableWrite = get(16)
	c.billableOutput = get(17)
	return c, c.completeRequests <= c.Requests
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
	addBillable(c, r)
}

func addBillable(c *Counters, r Request) {
	if completeTokens(r) {
		c.completeRequests = addSat(c.completeRequests, 1)
		c.billableInput = addSat(c.billableInput, uint64(r.UncachedInputTokens))
		c.billableRead = addSat(c.billableRead, uint64(r.CacheReadTokens))
		c.billableWrite = addSat(c.billableWrite, uint64(r.CacheCreationTokens))
		c.billableOutput = addSat(c.billableOutput, uint64(r.OutputTokens))
	}
}

func priceCounters(c *Counters, model string, prices map[string]Price) {
	c.CostUSD = 0
	c.UnpricedRequests = c.Requests
	if c.completeRequests == 0 || c.completeRequests > c.Requests {
		return
	}
	if cost := tokenCost(c.billableInput, c.billableRead, c.billableWrite, c.billableOutput, model, prices); cost != nil {
		c.CostUSD = *cost
		c.UnpricedRequests = c.Requests - c.completeRequests
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
	dst.completeRequests = addSat(dst.completeRequests, src.completeRequests)
	dst.billableInput = addSat(dst.billableInput, src.billableInput)
	dst.billableRead = addSat(dst.billableRead, src.billableRead)
	dst.billableWrite = addSat(dst.billableWrite, src.billableWrite)
	dst.billableOutput = addSat(dst.billableOutput, src.billableOutput)
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
