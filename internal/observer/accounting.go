package observer

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const millionTokens = 1_000_000.0

// NormalizeUsage converts a host usage record into the persisted canonical
// request accounting. Normalization runs exactly once, at record time, so the
// stored quality/TPS/cost reflect the prices snapshot configured when the
// observation was captured. Raw counters are always preserved in RawUsage.
func NormalizeUsage(record pluginapi.UsageRecord, prices map[string]Price) Request {
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
		RequestID:           record.RequestID,
		TraceID:             record.TraceID,
		Time:                record.RequestedAt,
		Provider:            record.Provider,
		Model:               record.Model,
		Alias:               record.Alias,
		Executor:            record.ExecutorType,
		AuthType:            record.AuthType,
		ServiceTier:         firstNonEmpty(record.ResponseServiceTier, record.ServiceTier),
		ReasoningEffort:     record.ReasoningEffort,
		Stream:              record.Stream,
		Failed:              record.Failed,
		FailureStatus:       record.Failure.StatusCode,
		InputTokens:         breakdown.Input.TotalTokens,
		UncachedInputTokens: breakdown.Input.UncachedTokens,
		OutputTokens:        breakdown.Output.TotalTokens,
		ReasoningTokens:     breakdown.Output.ReasoningTokens,
		CacheReadTokens:     breakdown.Input.CacheReadTokens,
		CacheCreationTokens: breakdown.Input.CacheWriteTokens,
		TotalTokens:         breakdown.TotalTokens,
		AccountingQuality:   string(breakdown.Quality),
		RawUsage:            record.Detail,
		LatencyNS:           int64(record.Latency),
		TTFTNS:              int64(record.TTFT),
		CacheHit:            breakdown.Input.CacheReadTokens > 0,
	}
	if req.Time.IsZero() {
		req.Time = time.Now()
	}
	req.GenerationNS = generationNS(req.LatencyNS, req.TTFTNS)
	req.TPS = tpsFor(breakdown, req.GenerationNS)
	req.CostUSD = costFor(breakdown, req.Model, prices)
	return req
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

// costFor prices complete accounting only, using the exact full model id and
// the mutually exclusive buckets. Ambiguous or unpriced requests return nil.
func costFor(breakdown usage.TokenBreakdown, model string, prices map[string]Price) *float64 {
	if breakdown.Quality != usage.TokenAccountingQualityComplete {
		return nil
	}
	price, ok := prices[model]
	if !ok {
		return nil
	}
	cost := (float64(breakdown.Input.UncachedTokens)*price.Input +
		float64(breakdown.Input.CacheReadTokens)*price.CacheRead +
		float64(breakdown.Input.CacheWriteTokens)*price.CacheCreation +
		float64(breakdown.Output.TotalTokens)*price.Output) / millionTokens
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
