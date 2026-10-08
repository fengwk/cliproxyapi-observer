package observer

import (
	"math"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const millionTokens = 1_000_000.0

// NormalizeUsage converts a host usage record into the persisted canonical
// request accounting. Normalization runs exactly once, at record time, so the
// stored quality/TPS are independent of prices. Raw counters are always
// preserved in RawUsage; monetary costs are derived only at query time.
func NormalizeUsage(record pluginapi.UsageRecord) Request {
	detail := usage.Detail{
		InputTokens:         record.Detail.InputTokens,
		OutputTokens:        record.Detail.OutputTokens,
		ReasoningTokens:     record.Detail.ReasoningTokens,
		CachedTokens:        record.Detail.CachedTokens,
		CacheReadTokens:     record.Detail.CacheReadTokens,
		CacheCreationTokens: record.Detail.CacheCreationTokens,
		TotalTokens:         record.Detail.TotalTokens,
	}
	// The public CPA helper owns provider semantics; the observer never guesses
	// a protocol from the model name. Known providers yield mutually exclusive
	// buckets, unknown providers stay unclassified with an authoritative total.
	ensured := usage.EnsureTokenBreakdownForProvider(detail, record.Provider, record.ExecutorType)
	breakdown := ensured.TokenBreakdown

	req := Request{
		RequestID:         record.RequestID,
		TraceID:           record.TraceID,
		Time:              record.RequestedAt,
		Provider:          record.Provider,
		Model:             record.Model,
		Alias:             record.Alias,
		Executor:          record.ExecutorType,
		AuthType:          record.AuthType,
		ServiceTier:       firstNonEmpty(record.ResponseServiceTier, record.ServiceTier),
		ReasoningEffort:   record.ReasoningEffort,
		Stream:            record.Stream,
		Failed:            record.Failed,
		FailureStatus:     record.Failure.StatusCode,
		AccountingQuality: string(breakdown.Quality),
		RawUsage:          record.Detail,
		LatencyNS:         int64(record.Latency),
		TTFTNS:            int64(record.TTFT),
		CacheHit:          record.Detail.CacheReadTokens > 0,
	}
	if breakdown.Quality == usage.TokenAccountingQualityComplete {
		// Canonical, mutually exclusive buckets.
		req.InputTokens = breakdown.Input.TotalTokens
		req.UncachedInputTokens = breakdown.Input.UncachedTokens
		req.OutputTokens = breakdown.Output.TotalTokens
		req.ReasoningTokens = breakdown.Output.ReasoningTokens
		req.CacheReadTokens = breakdown.Input.CacheReadTokens
		req.CacheCreationTokens = breakdown.Input.CacheWriteTokens
		req.TotalTokens = breakdown.TotalTokens
	} else {
		// Unknown/ambiguous semantics: keep the authoritative total but do not
		// guess how reasoning overlaps output. Expose the raw positive counters
		// for display and leave the uncertain uncached bucket at zero.
		d := record.Detail
		req.InputTokens = positive(d.InputTokens)
		req.OutputTokens = positive(d.OutputTokens)
		req.ReasoningTokens = positive(d.ReasoningTokens)
		req.CacheReadTokens = positive(d.CacheReadTokens)
		req.CacheCreationTokens = positive(d.CacheCreationTokens)
		req.UncachedInputTokens = 0
		req.TotalTokens = breakdown.TotalTokens
	}
	if req.Time.IsZero() {
		req.Time = time.Now()
	}
	req.GenerationNS = generationNS(req.LatencyNS, req.TTFTNS)
	req.TPS = tpsFor(breakdown, req.GenerationNS)
	return req
}

// positive exposes a raw counter for display without inventing values for
// unknown semantics; negative counters become zero.
func positive(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// generationNS is the post-first-token generation window. It is only known for
// streaming requests that reported a TTFT shorter than the total latency.
func generationNS(latencyNS, ttftNS int64) int64 {
	if ttftNS <= 0 || latencyNS <= ttftNS {
		return 0
	}
	return latencyNS - ttftNS
}

func tpsFor(breakdown usage.TokenBreakdown, genNS int64) *float64 {
	if breakdown.Quality != usage.TokenAccountingQualityComplete {
		return nil
	}
	if genNS <= 0 || breakdown.Output.TotalTokens <= 0 {
		return nil
	}
	v := float64(breakdown.Output.TotalTokens) / (float64(genNS) / float64(time.Second))
	return &v
}

// RequestCost ignores any legacy stored cost and prices the normalized,
// mutually exclusive buckets using the current exact full model identity.
func RequestCost(r Request, prices map[string]Price) *float64 {
	if !completeTokens(r) {
		return nil
	}
	return tokenCost(uint64(r.UncachedInputTokens), uint64(r.CacheReadTokens),
		uint64(r.CacheCreationTokens), uint64(r.OutputTokens), r.Model, prices)
}

func completeTokens(r Request) bool {
	return r.AccountingQuality == string(usage.TokenAccountingQualityComplete) &&
		r.UncachedInputTokens >= 0 && r.CacheReadTokens >= 0 &&
		r.CacheCreationTokens >= 0 && r.OutputTokens >= 0 &&
		r.InputTokens >= r.CacheReadTokens &&
		r.InputTokens-r.CacheReadTokens >= r.CacheCreationTokens &&
		r.InputTokens-r.CacheReadTokens-r.CacheCreationTokens == r.UncachedInputTokens &&
		r.ReasoningTokens >= 0 && r.ReasoningTokens <= r.OutputTokens
}

func tokenCost(uncached, read, write, output uint64, model string, prices map[string]Price) *float64 {
	price, ok := prices[model]
	if !ok {
		return nil
	}
	for _, p := range []float64{price.Input, price.CacheRead, price.CacheCreation, price.Output} {
		if p < 0 || math.IsNaN(p) || math.IsInf(p, 0) {
			return nil
		}
	}
	cost := (float64(uncached)*price.Input + float64(read)*price.CacheRead +
		float64(write)*price.CacheCreation + float64(output)*price.Output) / millionTokens
	if math.IsNaN(cost) || math.IsInf(cost, 0) {
		// A pathological price/token product must not poison summaries.
		return nil
	}
	return &cost
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
