// Package engine implements the clean-slate Observer schema 2 database.
//
// Usage is priced at receipt. Its immutable quote, execution deduplication,
// independent Key counters, compressed raw row and daily aggregate commit in
// one bbolt transaction. Trace IDs are never accounting identities.
//
// Failed writes retain at most 1024 already-quoted events for bounded retries.
// Each Key is unblocked only after its own retained events succeed. Lost or
// irrecoverable usage durably marks that Key as unknown-accounting. Reset and
// rotation never remove this fence. RecoverKey retries pending quotes, requires
// working storage, and requires explicit acknowledgement for unknown usage.
// Recovery changes no counters, history or limits.
//
// Open marks the session running durably. An unclean shutdown conservatively
// fences existing Keys, but never future new Keys. Close always releases the
// handle, draining capture and retrying accounting; unsettled/active principals
// become durable unknown-accounting fences. Closed APIs return ErrClosed even
// if Close itself reported debt. Hosts should stop/drain callbacks before close;
// a callback delivered after detachment, or an unexpected callback panic, must
// call RecordInterruption. Ordinary retryable writes stay in the pending queue.
//
// RecordInterruption uses a constant, atomic 0600 marker at
// <db>.interrupted/<64-lowercase-hex-Key-ID>, without opening bbolt. A stable
// <db>.interrupted.lock flock serializes writers and recovery across independent
// native runtimes on Linux/macOS. Normal Open/accounting/close creates no sidecar.
// Open, admission and Key/health reads observe newly published markers. Close
// never removes them. Recovery requires acknowledgement and no retained target
// debt; it persists a DB fence before removing/fsyncing the marker, then clears
// the fence durably, all under that file lock. A removal/sync failure thus stays
// paused across restart. Marker failures expose only sanitized health text.
//
// Markers only pause: they cannot reconstruct missing usage or a known cost.
// CPA's non-durable queue may never deliver some source events, so this package
// makes no exactly-once source delivery claim. If all storage is unwritable or
// cannot sync, marker persistence cannot be guaranteed: RecordInterruption
// returns an error which the caller must expose. The running flag still protects
// an unclean session, but cannot substitute for delivery or functioning storage.
//
// Completed admission IDs expire after 24 hours, with a hard 4096-entry cap.
// Execution replay protection covers 7 days by the immutable RequestedAt time.
// A durable monotonic cutoff permits deletion of old dedup IDs without making
// old callbacks billable again. Unseen expired, missing or future timestamps
// fail closed for their Key and need acknowledgement, never silent repricing.
// Correlations/collision tombstones expire with raw retention; usage receipt
// extends the correlation lifetime to protect still-readable request rows.
//
// Capture is best effort and nonblocking. A 64-entry queue holds at most 8MiB
// per body, plus bounded headers. CaptureMode selects off, safe headers only, or
// headers plus redacted JSON. Oversized/non-JSON bodies retain independent
// status and never store a partial payload. Diagnostic storage defaults to
// 256MiB (max 1GiB) and is tracked by a transactional compressed-byte counter;
// expired content is removed before the oldest capture is evicted by time.
// Only allowlisted, redacted headers and recursively redacted JSON are stored.
// New captures use one gzip envelope; old double-compressed schema2 rows remain
// readable. Ambiguous same-Key traces hide content; expiration is enforced on
// reads before cleanup, and PurgeCaptures advances a generation so already
// queued writes cannot resurrect cleared content. Cleanup/Purge never touch
// metadata, rollups, costs, quotas or the independent Key counters.
//
// DeleteKey writes a terminal tombstone, clears the authentication hash and
// the stored plaintext credential, and disables the Key in one transaction.
// Active requests and retained accounting events return ErrBusy; deleted Keys
// reject all other mutations while history and counters remain. Revocation
// (empty hash, not deleted) and ordinary disabling remain distinguishable;
// revocation clears plaintext too, while disabling retains it for display.
// Generated secrets are sk- plus 48 unbiased alphanumeric characters; internal
// IDs remain 64 lowercase hex and are never substituted for a missing secret.
//
// Schema stays 2 and observer_features=2 marks additively persisted plaintext
// Key material (feature 1 marked the capture/state buckets). A higher feature
// version is rejected from a read-only preflight before opening a writable
// handle, so older binaries that support only feature 1 cannot overwrite the
// new field. Legacy feature-1/hash-only rows stay authenticatable with an empty
// Secret and are not rewritten with a guessed credential. Other legacy settings
// inherit the former boolean mode and raw retention; the FIFO index and TTL
// state are rebuilt idempotently without rewriting or clearing old content.
//
// Lists expose zero effective rolling usage for expired windows without changing
// anchors. Existing reset timestamps remain in the past so the UI can explain
// that the next admission, not the read, restarts those cycles.
package engine
