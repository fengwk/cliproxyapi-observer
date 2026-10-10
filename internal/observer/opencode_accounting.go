package observer

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The compatibility is deliberately narrow: only the exact upstream provider,
// executor implementation and model namespace below are reinterpreted. Every
// other record keeps whatever accounting the SDK derived for it.
const (
	openCodeGoProvider    = "opencode-go"
	openCodeGoExecutor    = "executorAdapter"
	openCodeGoModelPrefix = "opencode-go/"
)

// isOpenCodeCompatTarget reports whether a usage record belongs to the cohort
// whose canonical accounting the observer may reconstruct from raw counters.
// A bare "opencode-go/" model has no native id and is not a model.
func isOpenCodeCompatTarget(provider, executor, model string) bool {
	return provider == openCodeGoProvider && executor == openCodeGoExecutor &&
		strings.HasPrefix(model, openCodeGoModelPrefix) && len(model) > len(openCodeGoModelPrefix)
}

// recoverOpenCodeBreakdown rebuilds a canonical, mutually exclusive token
// breakdown from the raw callback counters of an opencode-go adapter record.
//
// The host parses this provider's output with OpenAI, OpenAI Responses or
// Anthropic Messages semantics but the observer cannot see which one was used,
// so both candidate normalizations are tried:
//
//   - subset:      cache inside input, reasoning inside output (OpenAI/Responses)
//   - independent: input excludes cache, raw output still contains reasoning
//     (Anthropic Messages)
//
// A candidate is accepted only when it is Valid and complete, and the record is
// recovered only when the raw counters uniquely determine the result: a single
// valid candidate, or several identical ones. The raw callback total stays
// authoritative, so the SDK's unknown-provider lower bound is never used here.
func recoverOpenCodeBreakdown(raw pluginapi.UsageDetail) (usage.TokenBreakdown, bool) {
	// Every raw counter must be non-negative; a negative value is malformed and
	// must never be repaired into fabricated billing.
	for _, v := range []int64{
		raw.InputTokens, raw.OutputTokens, raw.ReasoningTokens, raw.CachedTokens,
		raw.CacheReadTokens, raw.CacheCreationTokens, raw.TotalTokens,
	} {
		if v < 0 {
			return usage.TokenBreakdown{}, false
		}
	}
	// A record with no preserved raw usage has nothing to recover; repairing it
	// could only overwrite whatever the row holds, so leave it alone.
	if raw.InputTokens == 0 && raw.OutputTokens == 0 && raw.ReasoningTokens == 0 &&
		raw.CachedTokens == 0 && raw.CacheReadTokens == 0 && raw.CacheCreationTokens == 0 &&
		raw.TotalTokens == 0 {
		return usage.TokenBreakdown{}, false
	}
	// Reasoning is a subset of the raw output for both registered families.
	if raw.ReasoningTokens > raw.OutputTokens {
		return usage.TokenBreakdown{}, false
	}
	// Non-zero usage must carry a positive authoritative raw total.
	nonZero := raw.InputTokens > 0 || raw.OutputTokens > 0 || raw.ReasoningTokens > 0 ||
		raw.CacheReadTokens > 0 || raw.CacheCreationTokens > 0 || raw.CachedTokens > 0
	if nonZero && raw.TotalTokens <= 0 {
		return usage.TokenBreakdown{}, false
	}
	// CachedTokens aliases an explicit cache component and is never additive:
	// it must equal CacheReadTokens, or CacheCreationTokens when the read is
	// zero. A record carrying only the alias is rejected.
	if raw.CachedTokens > 0 &&
		!(raw.CachedTokens == raw.CacheReadTokens ||
			(raw.CacheReadTokens == 0 && raw.CachedTokens == raw.CacheCreationTokens)) {
		return usage.TokenBreakdown{}, false
	}

	candidates := make([]usage.TokenBreakdown, 0, 2)
	subset := usage.NewSubsetTokenBreakdown(
		raw.InputTokens, raw.CacheReadTokens, raw.CacheCreationTokens,
		raw.OutputTokens, raw.ReasoningTokens, raw.TotalTokens)
	if subset.Valid() && subset.Quality == usage.TokenAccountingQualityComplete {
		candidates = append(candidates, subset)
	}
	independent := usage.NewIndependentTokenBreakdown(
		raw.InputTokens, raw.CacheReadTokens, raw.CacheCreationTokens,
		raw.OutputTokens-raw.ReasoningTokens, raw.ReasoningTokens, raw.TotalTokens)
	if independent.Valid() && independent.Quality == usage.TokenAccountingQualityComplete {
		candidates = append(candidates, independent)
	}
	if len(candidates) == 0 {
		return usage.TokenBreakdown{}, false
	}
	chosen := candidates[0]
	for _, c := range candidates[1:] {
		if c != chosen {
			// Two complete interpretations that disagree cannot be resolved
			// without guessing; keep the record unclassified.
			return usage.TokenBreakdown{}, false
		}
	}
	return chosen, true
}

// storeBreakdown copies the canonical buckets of a complete breakdown onto a
// request. RawUsage and every identity field are left untouched.
func storeBreakdown(req *Request, b usage.TokenBreakdown) {
	req.AccountingQuality = string(b.Quality)
	req.InputTokens = b.Input.TotalTokens
	req.UncachedInputTokens = b.Input.UncachedTokens
	req.OutputTokens = b.Output.TotalTokens
	req.ReasoningTokens = b.Output.ReasoningTokens
	req.CacheReadTokens = b.Input.CacheReadTokens
	req.CacheCreationTokens = b.Input.CacheWriteTokens
	req.TotalTokens = b.TotalTokens
}
