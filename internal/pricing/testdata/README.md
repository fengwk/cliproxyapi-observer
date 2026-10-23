# Fixed catalog extract

`models.dev.sample.json` is a hand-extracted subset of the fixed public catalog
used by the design regression sample. Source: `https://models.dev/api.json`,
SHA-256 `8700799debe7b107f438bcc77305847333b6eb232274bb43d4b285b2b0dbca41`
(232 providers, 9103 models at extraction time).

Only the providers/models needed by the ten names in the design are copied,
with their original `cost`, nested `tiers`, `context_over_200k` and
`canonical_model_id` values. The `grok-4.7` evidence includes exact entries
from `xai`, `opencode-go`, `opencode`, `aihubmix`, `302ai` and `requesty`; their
differing complete rates (including `cache_write`) are asserted in the tests. Provider `fixture` contains synthetic entries that
exercise partial rates, explicit zero, an unsupported tier type and the legacy
`context_over_200k` fallback. The multi-megabyte upstream file is not committed.

`models.dev.v2.sample.json` is a hand-extracted subset of
`https://models.dev/experimental/v2.0/api.json`. It keeps the real v2 shapes
needed by the adapter regression tests: `cost.text.input/output`, field-wise
`overrides`, TTL cache writes (`5m`/`1h`), scalar and object `multipliers`,
`canonical_id`, and conditions using `context`, `service_tier`, `date`,
`utc_days` and `utc_hours`. It also includes an unsupported `region` condition
and a `unit: "character"` price that must be rejected rather than treated as a
per-token price. Provider/model names are retained only for this small local
fixture; tests never contact the network.

`models.dev.v2.gemini.json` retains the complete `cost` objects and canonical IDs
of `google/gemini-3.8-flash` and `google-vertex/gemini-3.8-flash` from the v2
catalog inspected on 2026-10-23 (catalog SHA-256
`ed982576921d90863842ff0fda7f611a6a792d6cc21b596b926d7b38f7dc9bff`).
It covers excluded `web_search` fees, scalar `factor.text`, tier/date multipliers,
and the Vertex region condition that cannot safely be evaluated.
