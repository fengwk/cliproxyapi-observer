// Package engine implements the clean-slate Observer schema 2 database.
//
// Usage is priced at receipt. Its immutable quote, execution deduplication,
// independent Key counters, raw row and daily aggregate commit in one bbolt
// transaction. Trace IDs are not accounting identities.
//
// Failed writes retain at most 1024 already-quoted events in memory. The Key
// remains blocked until Maintenance or Close durably retries every retained
// event. Reset and rotation do not clear this gate. Overflow or a quote failure
// permanently fences this database. Health reports the failure without storing
// upstream error messages or response bodies.
//
// Crash recovery is deliberately conservative: Open durably marks the session
// running before accepting work. Any unclean shutdown fences all admissions
// after restart, even if no money was actually lost. Close drains capture,
// retries usage, and clears the running marker only with no active admissions,
// pending usage or irreversible failure. A fence is not automatically cleared:
// reconciliation is an operator responsibility outside this API. An operator
// must not delete the database to bypass unsettled debt.
//
// Admission/completion idempotency lasts for this process lifetime; after a
// crash admission is fenced. Execution deduplication is retained independently
// of raw/daily retention, so a late replay cannot charge twice. These small
// deduplication identities intentionally remain on disk.
//
// Capture is best effort: a busy mutex, full queue, oversized input or full
// diagnostic storage drops capture without delaying inference. Only allowlisted
// headers and valid, recursively redacted JSON are stored, gzip compressed.
// Ambiguous same-Key trace collisions discard diagnostic content. Raw detail
// and capture expiry are enforced on reads before background pruning.
package engine
