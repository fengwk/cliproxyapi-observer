package observer

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The compatibility is deliberately narrow: only these exact upstream providers,
// the shared native executor implementation and their own model namespaces are
// reinterpreted. Every other record keeps whatever accounting the SDK derived.
const (
	// adapterExecutor is the single executor implementation shared by the native
	// plugin adapters whose raw callback counters the observer may repair.
	adapterExecutor = "executorAdapter"

	openCodeGoProvider    = "opencode-go"
	openCodeGoExecutor    = adapterExecutor
	openCodeGoModelPrefix = "opencode-go/"

	commandCodeProvider    = "commandcode"
	commandCodeModelPrefix = "commandcode/"
)

// isAdapterCompatTarget reports whether a usage record belongs to one of the
// exact adapter cohorts whose canonical accounting the observer may reconstruct
// from raw counters. The executor must be the shared adapter implementation and
// the model must carry a non-empty native id inside the cohort's own namespace;
// a bare "<provider>/" prefix has no native id and is not a model.
func isAdapterCompatTarget(provider, executor, model string) bool {
	if executor != adapterExecutor {
		return false
	}
	switch provider {
	case openCodeGoProvider:
		return hasNativeModelID(model, openCodeGoModelPrefix)
	case commandCodeProvider:
		return hasNativeModelID(model, commandCodeModelPrefix)
	default:
		return false
	}
}

// hasNativeModelID reports whether model lives inside prefix and carries a
// non-empty native id after it.
func hasNativeModelID(model, prefix string) bool {
	return strings.HasPrefix(model, prefix) && len(model) > len(prefix)
}

// recoverAdapterBreakdown rebuilds a canonical, mutually exclusive token
// breakdown from the raw callback counters of a native adapter record.
//
// The host parses these providers' output with OpenAI, OpenAI Responses or
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
func recoverAdapterBreakdown(raw pluginapi.UsageDetail) (usage.TokenBreakdown, bool) {
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
