# Development rules

- Keep this an independent CLIProxyAPI v8 native plugin. Never change CPA core.
- Own client Key authentication and admission only. Never modify bodies, headers, model routing, retries or sessions.
- Request content is sensitive: capture is opt-in, bounded and management-authenticated.
- Keep content separate from metadata; enforce expiry on reads, not just background cleanup.
- Store only hashes of generated or administrator-supplied client Keys. Never store authorization headers, upstream secrets, failure response bodies or browser credentials.
- Diagnostic capture is bounded and nonblocking. Accounting is transactional; a failed usage write pauses that Key's subsequent admissions, not an in-flight response.
- Preserve complete model identities and persist normalized token accounting.
- Fix integer micro-USD costs at ingestion; keep independent Key accumulators, daily rollups and short-lived request details. No legacy migration or query-time repricing.
- Quiesce releases storage; reconfigure must reopen after a failed native replacement.
- Keep UI runtime self-contained, same-origin-frameable and aligned with CPA's actual theme tokens and embedded shell. Build-time React/TypeScript tooling is allowed; no runtime CDN.
- Use fake credentials and local mock upstreams only; never touch production configuration.
- Verify Go/race/vet, Node/browser, native build and real CPA host integration.
- Use isolated workspaces; release only when explicitly authorized and verified.
