# Pricing snapshot, preview and quote semantics

For management UI consumers, `Snapshot.Prices` lists baseline manual/automatic
prices for observed model/provider identities, plus unobserved manual entries.
An observed entry keeps its full requested model name and optional `provider`;
sort by model then provider to distinguish provider-specific rates.
`source: "unpriced"` means the cost is zero because no unambiguous price
exists; `reason` and `candidates` explain why and never imply an invented rate.

Snapshots are **not conditional-rule previews**: there is no prompt token count
or execution time in a snapshot. In particular, a rule-only model may appear
unpriced in the baseline even though a matching execution is priced by its rule.
The UI should explain this and show rules separately, not present the snapshot
as the final execution price. Only `Quote` applies ordered rule conditions,
using UTC time and full prompt tokens (`input + cache_read + cache_write`).

## One resolver

`Preview`, `Quote` and `Snapshot` share the same identity resolver. Precedence
is: first matching conditional rule (quote only), complete-model manual price,
administrator mapping, trusted provider plus exact catalog ID, verified public
namespace, same-provider canonical evidence, then provider-less exact identity.
`manual` still wins for every provider; a manual zero is a real override.

`Config.Mappings` has `{model, provider, catalog_model}`. `model` is the complete
public model ID. A mapping affects accounting identity only, never request
routing. A valid mapping that no longer names an exact catalog entry returns
`source: "unpriced"` with `reason: "mapping_missing"`; it does not fall back to a
similar name or another channel.

Only the verified outer namespaces `commandcode/` and `opencode-go/` are
removed, and only when no exact provider identity matched first. The remaining
inner path, letter case, dates, version suffixes and `:free` are preserved.
There is no generic "remove the first slash" or alias normalization. A trusted
provider that disagrees with the namespace produces a `provider_conflict`, not
an arbitrary selection. `owned_by` is not a billing provider.

A trusted provider is a channel boundary: if that channel has neither an exact
entry nor same-channel canonical evidence, another channel's identically named
model is only a candidate, never an automatic price. Provider-less exact
fallback (unique or complete-rate-consistent) is valid only when the request
did not supply a provider. If a known namespace and a different trusted
provider both appear, they conflict even when the full public model exists
under the wrong provider.

A canonical relationship is catalog evidence, not a channel price. It may be
used only inside the same trusted provider and never substitutes a distinct
`-free`/`:free`/`-paid`/`:paid` pricing variant. Without a trusted provider,
canonical aliases are returned as candidates only. Different exact channels
with different complete rates produce `reason: "ambiguous"` and a bounded,
sorted `candidates` list.

## Missing rates and explicit zero

`missing_rates` names rate fields the catalog did not publish; an explicit zero
is not missing and must render as `0`, while a missing value must render as `—`.
If a non-zero token bucket needs a missing rate, the quote becomes
`source: "unpriced"`, `cost_micro_usd: 0`, `reason: "missing_rates"` and lists
the required fields. A missing rate for an unused bucket does not block a quote.
No missing rate is ever invented or replaced by zero.

Automatic rates remain decimal USD per million converted exactly to integer
micro-USD, rounding fractional micro-USD upward once after the whole execution.
Audio rates are outside this API; configure an explicit manual price/rule when
they matter.

## Context tiers

Nested `cost.tiers` are parsed only when every tier is a positive, sorted
`context` threshold with valid rates. The tier object and its metadata use a
strict schema: unknown pricing dimensions (for example audio rates) or unknown
tier metadata invalidate the whole tier table instead of being silently
ignored. Reliable tiers are applied to a quote by the complete prompt size; the
base rates apply through the first threshold.
Quotes never flatten a tiered price into the base rate. If the published tier
shape cannot be represented safely, the entry is `unpriced` with
`reason: "tiers_unsupported"` and requires a manual override. `Preview` and
`Snapshot` show the base tier and mark it with `reason: "context_tiers"` because
they have no invented token context.

## Diagnostics

`Price`, `PreviewPrice` and `Quote` expose optional:

- `reason`: `manual`, `rule`, `auto_disabled`, `catalog_unavailable`,
  `not_found`, `ambiguous`, `provider_conflict`, `mapping_missing`,
  `tiers_unsupported`, `context_tiers`, `missing_rates`, or `variant_only`.
- `match`: `manual`, `rule`, `mapping`, `provider`, `namespace`, `canonical`,
  `exact`, `ambiguous`, or `none`.
- `catalog_provider`, `catalog_model`, `catalog_time`: the catalog evidence
  used (time is the last successful catalog generation).
- `missing_rates` and bounded `candidates`.

Diagnostics describe an immutable quote; later catalog or configuration changes
never reprice a stored quote. A failed refresh keeps the last valid catalog and
reports `Snapshot.Error`; a failed cache write is also visible while the freshly
loaded memory catalog remains usable. Old disk caches without the new optional
fields remain readable.
