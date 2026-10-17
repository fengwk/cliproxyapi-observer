# Pricing snapshot and quote semantics

For management UI consumers, `Snapshot.Prices` lists baseline manual/automatic
prices for observed model/provider identities, plus unobserved manual entries.
An observed entry keeps its full requested model name and optional `provider`;
sort by model then provider to distinguish provider-specific rates.
`source: "unpriced"` explicitly means zero because no unambiguous price exists.

Snapshots are **not conditional-rule previews**: there is no prompt token count
or execution time in a snapshot. In particular, a rule-only model may appear
unpriced in the baseline even though a matching execution is priced by its rule.
The UI should explain this and show rules separately, not present the snapshot
as the final execution price. Only `Quote` applies ordered rule conditions,
using UTC time and full prompt tokens (`input + cache_read + cache_write`).

Manual config entries are model-wide. They must omit `provider` (or use an empty
string), and `source` must be empty or `"manual"`; other values are rejected.
A manual zero price still overrides all automatic provider prices.

Automatic catalog rates use the five flat cost fields represented by `Rates`.
models.dev additionally publishes audio rates and nested context tiers; these
are not represented in this API. Configure explicit rules for context-tier
overrides. Automatic base rates should not be presented as a full provider
invoice including unsupported dimensions.
