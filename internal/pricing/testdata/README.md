# Fixed catalog extract

`models.dev.sample.json` is a hand-extracted subset of the fixed public catalog
used by the design regression sample. Source: `https://models.dev/api.json`,
SHA-256 `8700799debe7b107f438bcc77305847333b6eb232274bb43d4b285b2b0dbca41`
(232 providers, 9103 models at extraction time).

Only the providers/models needed by the ten names in the design are copied,
with their original `cost`, nested `tiers`, `context_over_200k` and
`canonical_model_id` values. Provider `fixture` contains synthetic entries that
exercise partial rates, explicit zero, an unsupported tier type and the legacy
`context_over_200k` fallback. The multi-megabyte upstream file is not committed.
