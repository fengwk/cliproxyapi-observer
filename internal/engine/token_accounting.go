package engine

import (
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// canonicalTokenBreakdown keeps the pinned SDK's provider-aware classification
// and only recovers unknown semantics when every raw counter is explicit and
// conserved by an authoritative total. It never rewrites the raw detail.
func canonicalTokenBreakdown(d pluginapi.UsageDetail, provider, executorType string) usage.TokenBreakdown {
	b := usage.EnsureTokenBreakdownForProvider(usage.Detail{
		InputTokens:         d.InputTokens,
		OutputTokens:        d.OutputTokens,
		ReasoningTokens:     d.ReasoningTokens,
		CachedTokens:        d.CachedTokens,
		CacheReadTokens:     d.CacheReadTokens,
		CacheCreationTokens: d.CacheCreationTokens,
		TotalTokens:         d.TotalTokens,
	}, provider, executorType).TokenBreakdown
	// Complete and inconsistent results are authoritative. Only an unknown
	// semantic partition may be recovered, and never from a synthesized total.
	if b.Quality != usage.TokenAccountingQualityUnclassified {
		return b
	}
	if recovered, ok := recoverUnknownTokenBreakdown(d); ok {
		return recovered
	}
	return b
}

// recoverUnknownTokenBreakdown tries the three canonical partition contracts
// against the same raw counters. Adoption requires a complete, valid breakdown
// whose total equals the explicit raw total; ambiguous differing results are
// rejected rather than selecting one arbitrarily.
func recoverUnknownTokenBreakdown(d pluginapi.UsageDetail) (usage.TokenBreakdown, bool) {
	if d.TotalTokens <= 0 || d.InputTokens <= 0 && d.OutputTokens <= 0 || !nonNegativeRawCounters(d) {
		return usage.TokenBreakdown{}, false
	}
	// CachedTokens is a legacy aggregate without a read/write split. When both
	// canonical cache buckets are zero, interpreting it as either is a guess.
	if d.CachedTokens > 0 && d.CacheReadTokens == 0 && d.CacheCreationTokens == 0 {
		return usage.TokenBreakdown{}, false
	}
	candidates := []usage.TokenBreakdown{
		usage.NewSubsetTokenBreakdown(d.InputTokens, d.CacheReadTokens, d.CacheCreationTokens, d.OutputTokens, d.ReasoningTokens, d.TotalTokens),
		usage.NewIndependentTokenBreakdown(d.InputTokens, d.CacheReadTokens, d.CacheCreationTokens, d.OutputTokens, d.ReasoningTokens, d.TotalTokens),
		usage.NewSeparateReasoningTokenBreakdown(d.InputTokens, d.CacheReadTokens, d.CacheCreationTokens, d.OutputTokens, d.ReasoningTokens, d.TotalTokens),
	}
	var chosen usage.TokenBreakdown
	found := false
	for _, candidate := range candidates {
		if candidate.Quality != usage.TokenAccountingQualityComplete || !candidate.Valid() || candidate.TotalTokens != d.TotalTokens {
			continue
		}
		if found && candidate != chosen {
			return usage.TokenBreakdown{}, false
		}
		chosen, found = candidate, true
	}
	return chosen, found
}

func nonNegativeRawCounters(d pluginapi.UsageDetail) bool {
	return d.InputTokens >= 0 && d.OutputTokens >= 0 && d.ReasoningTokens >= 0 &&
		d.CachedTokens >= 0 && d.CacheReadTokens >= 0 && d.CacheCreationTokens >= 0 && d.TotalTokens >= 0
}
