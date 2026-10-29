# Development rules

- Keep this an independent CLIProxyAPI v8 native plugin. Never change CPA core.
- Use the existing nonexclusive FrontendAuthProvider: native CPA credentials stay native; manage only supported model operations authenticated by Observer. Enforce exact model permissions, quota and concurrency on owned calls. Leave unsupported Observer routes unhandled instead of blanket-authenticating them. Preserve CPA-issued ephemeral tokens and the Observer catalog token's narrow scope. Never use global exclusive auth, take over routing, or modify bodies, headers, retries or sessions.
- Request content is sensitive: capture is opt-in, bounded and management-authenticated.
- Keep content separate from metadata; enforce expiry on reads, not just background cleanup.
- The user explicitly authorized persistent plaintext Observer client Keys for authenticated internal management display/copy. Persist the current secret transactionally with its authentication hash; never relabel internal IDs as secrets. Revoke/delete clear both; disable retains them. Never store authorization headers, host/upstream secrets or failure response bodies. Browser persistence is still limited to explicit "remember management login"; do not put client secrets in browser storage/URLs/logs or read host credentials.
- Diagnostic capture is bounded and nonblocking. Accounting is transactional but availability takes priority: failed/missing usage, callback errors and unclean restarts must never create persistent Key blocks or recovery requirements. Lost records may remain lost; never invent charges or rewrite historical costs. Preserve explicit authentication, model permissions, quotas and concurrency limits.
- Preserve complete model identities and persist normalized token accounting.
- Fix integer micro-USD costs at ingestion; keep independent Key accumulators, daily rollups and short-lived request details. No legacy migration or query-time repricing.
- Quiesce releases storage; reconfigure must reopen after a failed native replacement.
- Keep UI runtime self-contained, same-origin-frameable and aligned with CPA's actual theme tokens and embedded shell. Build-time React/TypeScript tooling is allowed; no runtime CDN.
- Use fake credentials and local mock upstreams only; never touch production configuration.
- Verify Go/race/vet, Node/browser, native build and real CPA host integration.
- Use isolated workspaces; release only when explicitly authorized and verified.
