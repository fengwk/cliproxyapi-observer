# Development rules

- Keep this an independent, thin CLIProxyAPI v8 native observer. Never change CPA core.
- Observe requests without modifying bodies, headers, model routing, retries or sessions.
- Request content is sensitive: capture is opt-in, bounded and management-authenticated.
- Keep content separate from metadata; enforce expiry on reads, not just background cleanup.
- Never store API keys, authorization headers, failure response bodies or browser credentials.
- Use bounded, nonblocking ingestion. Report dropped observations instead of blocking inference.
- Preserve complete model identities and persist normalized token accounting.
- Use token-only preaggregated statistics, query-time prices and offset pagination; never scan for a page count.
- Quiesce releases storage; reconfigure must reopen after a failed native replacement.
- Keep UI dependency-free, same-origin-frameable and aligned with CPA themes.
- Use fake credentials and local mock upstreams only; never touch production configuration.
- Verify Go/race/vet, Node/browser, native build and real CPA host integration.
- Use isolated workspaces; ordinary feature merges never trigger automatic releases.
