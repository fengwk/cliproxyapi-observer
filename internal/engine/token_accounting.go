package engine

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// canonicalTokenBreakdown adapts native Claude's raw output subset, then keeps
// the SDK's provider-aware classification and conservative unknown recovery.
// Adaptation and recovery never rewrite raw counters or invent a reported total.
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
	// CPA's Claude parser keeps thinking inside raw OutputTokens, but the
	// plugin bridge drops its TokenBreakdown. The SDK's legacy independent
	// constructor then adds thinking again. Recover only the native contract
	// proven by a positive explicit total; keep complete legacy partitions.
	if b.Quality == usage.TokenAccountingQualityInconsistent {
		if adapted, ok := nativeClaudeOutputSubset(d, provider, executorType); ok {
			return adapted
		}
	}
	// Other inconsistent results stay authoritative. Unknown semantics may
	// only be recovered against an explicit total, never a synthesized one.
	if b.Quality != usage.TokenAccountingQualityUnclassified {
		return b
	}
	if recovered, ok := recoverUnknownTokenBreakdown(d); ok {
		return recovered
	}
	return b
}

func nativeClaudeOutputSubset(d pluginapi.UsageDetail, provider, executorType string) (usage.TokenBreakdown, bool) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	executorType = strings.ToLower(strings.TrimSpace(executorType))
	// Do not reinterpret custom providers or OpenAI compatibility executors,
	// even when their names contain a Claude/Anthropic marker.
	if provider != "claude" || executorType != "" && executorType != "claudeexecutor" {
		return usage.TokenBreakdown{}, false
	}
	if d.TotalTokens <= 0 || d.ReasoningTokens <= 0 || d.ReasoningTokens > d.OutputTokens || !nonNegativeRawCounters(d) {
		return usage.TokenBreakdown{}, false
	}
	if d.CachedTokens > 0 && d.CacheReadTokens == 0 && d.CacheCreationTokens == 0 {
		return usage.TokenBreakdown{}, false
	}
	b := usage.NewIndependentTokenBreakdown(d.InputTokens, d.CacheReadTokens, d.CacheCreationTokens,
		d.OutputTokens-d.ReasoningTokens, d.ReasoningTokens, d.TotalTokens)
	return b, b.Quality == usage.TokenAccountingQualityComplete && b.Valid() && b.TotalTokens == d.TotalTokens
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
