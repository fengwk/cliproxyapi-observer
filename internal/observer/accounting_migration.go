package observer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	bolt "go.etcd.io/bbolt"
)

// bucketAccountingMeta is a dedicated, tiny metadata bucket whose sequence is
// the completion marker for the accounting repair migration. Keeping it
// separate leaves the existing stats/key-stats sequence markers untouched.
var bucketAccountingMeta = []byte("accounting_meta")

// accountingMigrationSequence is the completion marker value. bbolt starts a
// bucket sequence at 0, so 1 unambiguously means "already repaired once".
const accountingMigrationSequence = 1

// persistedCounterWords is the number of 64-bit words stored by encodeCounters:
// the 13 original usage/latency metrics plus the complete-accounting count and
// the four billable buckets. Cost and unpriced counts are never persisted.
const persistedCounterWords = 18

// migrateAccounting performs the one-time historical repair of retained usage
// that was persisted as unclassified before the protocol-aware compatibility
// existed. It runs after the legacy stats/key-stats migrations and before the
// writer starts, and commits everything in a single transaction.
//
// Only records that are exactly opencode-go / executorAdapter / opencode-go/*,
// still carry their raw usage and were unclassified are considered. When the
// raw counters no longer determine a canonical breakdown, the record is left
// unclassified; only genuinely corrupt records fail and roll the transaction
// back.
func (s *Store) migrateAccounting() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		am, err := tx.CreateBucketIfNotExists(bucketAccountingMeta)
		if err != nil {
			return fmt.Errorf("create accounting metadata bucket: %w", err)
		}
		if am.Sequence() == accountingMigrationSequence {
			return nil
		}
		rb := tx.Bucket(bucketRequests)
		if rb == nil {
			return am.SetSequence(accountingMigrationSequence)
		}

		type repair struct {
			key  []byte
			old  Request
			next Request
		}
		var repairs []repair
		// Collect candidates in a read-only pass so the request bucket is never
		// mutated while it is being iterated.
		if err := rb.ForEach(func(key, value []byte) error {
			if len(key) != requestKeyLen {
				return fmt.Errorf("corrupt retained request key during accounting migration")
			}
			var r Request
			if err := json.Unmarshal(value, &r); err != nil {
				return fmt.Errorf("corrupt retained request during accounting migration")
			}
			if !bytes.Equal(key, encodeRequestKey(r.Time, r.Sequence)) {
				return fmt.Errorf("retained request key mismatch during accounting migration")
			}
			if r.AccountingQuality != string(usage.TokenAccountingQualityUnclassified) ||
				!isOpenCodeCompatTarget(r.Provider, r.Executor, r.Model) {
				return nil
			}
			recovered, ok := recoverOpenCodeBreakdown(r.RawUsage)
			if !ok {
				return nil
			}
			next := r
			storeBreakdown(&next, recovered)
			next.GenerationNS = generationNS(next.LatencyNS, next.TTFTNS)
			next.TPS = tpsFor(recovered, next.GenerationNS)
			// Bodies and derived cost stay off the retained record.
			next.CostUSD = nil
			next.BodyAvailable = false
			repairs = append(repairs, repair{key: append([]byte(nil), key...), old: r, next: next})
			return nil
		}); err != nil {
			return err
		}

		for _, rep := range repairs {
			if err := replaceAccountingContribution(tx, rep.key, rep.old, rep.next); err != nil {
				return err
			}
		}
		return am.SetSequence(accountingMigrationSequence)
	})
}

// replaceAccountingContribution reconciles the global minute row and the
// identity/input-length key row with a request whose accounting was just
// repaired. The pre- and post-repair contributions differ only in the input
// total (Anthropic moves a request to a longer input bucket) and in the
// complete/billable partitions, so the aggregates are adjusted by that exact
// difference instead of being rebuilt.
//
// Both the precise global row and the precise old key row must exist and be
// safely decrementable; otherwise the request is conservatively skipped and
// stays unclassified. Malformed bytes fail the transaction.
func replaceAccountingContribution(tx *bolt.Tx, key []byte, old, next Request) error {
	sb := tx.Bucket(bucketStats)
	kb := tx.Bucket(bucketKeyStats)
	if sb == nil || kb == nil {
		return nil
	}
	oldContribution := requestContribution(old)
	newContribution := requestContribution(next)
	minute := statsMinute(old.Time)

	globalKey := statsKey(minute, old.Provider, old.Model)
	globalValue := sb.Get(globalKey)
	if globalValue == nil {
		return nil
	}
	global, ok := decodeCounters(globalValue)
	if !ok {
		return fmt.Errorf("corrupt stats record during accounting migration")
	}
	updatedGlobal, ok := applyContributionDelta(global, oldContribution, newContribution)
	if !ok || updatedGlobal.completeRequests > updatedGlobal.Requests {
		return nil
	}

	// The exact pre-repair input-length row must exist; a legacy unknown-input
	// (-1) row is never treated as this request's contribution.
	oldRowKey := keyStatsKey(minute, old.InputTokens, old.ClientKeyID, old.AuthIndex, old.Provider, old.Model)
	oldRowValue := kb.Get(oldRowKey)
	if oldRowValue == nil {
		return nil
	}
	oldRow, ok := decodeCounters(oldRowValue)
	if !ok {
		return fmt.Errorf("corrupt key stats record during accounting migration")
	}
	newRowKey := keyStatsKey(minute, next.InputTokens, next.ClientKeyID, next.AuthIndex, next.Provider, next.Model)

	if bytes.Equal(oldRowKey, newRowKey) {
		updatedRow, ok := applyContributionDelta(oldRow, oldContribution, newContribution)
		if !ok || updatedRow.completeRequests > updatedRow.Requests {
			return nil
		}
		if err := kb.Put(oldRowKey, encodeCounters(updatedRow)); err != nil {
			return err
		}
	} else {
		// The normalized input length changed: move the whole contribution to
		// the new input-length bucket.
		reducedRow, ok := subtractContribution(oldRow, oldContribution)
		if !ok || reducedRow.completeRequests > reducedRow.Requests {
			return nil
		}
		var newRow Counters
		if value := kb.Get(newRowKey); value != nil {
			decoded, ok := decodeCounters(value)
			if !ok {
				return fmt.Errorf("corrupt key stats record during accounting migration")
			}
			newRow = decoded
		}
		mergedRow, ok := addContribution(newRow, newContribution)
		if !ok || mergedRow.completeRequests > mergedRow.Requests {
			return nil
		}
		if err := kb.Put(newRowKey, encodeCounters(mergedRow)); err != nil {
			return err
		}
		if err := kb.Put(oldRowKey, encodeCounters(reducedRow)); err != nil {
			return err
		}
	}

	value, err := marshalStoredRequest(next)
	if err != nil {
		return err
	}
	if err := tx.Bucket(bucketRequests).Put(key, value); err != nil {
		return err
	}
	return sb.Put(globalKey, encodeCounters(updatedGlobal))
}

// requestContribution is the exact counters row a single request contributes.
func requestContribution(r Request) Counters {
	var c Counters
	addRequestCounters(&c, r)
	return c
}

// persistedWords extracts the stored counters in encodeCounters order so the
// replacement helper can handle all 18 words uniformly.
func persistedWords(c Counters) [persistedCounterWords]uint64 {
	return [persistedCounterWords]uint64{
		c.Requests, c.FailedRequests, c.InputTokens, c.OutputTokens,
		c.ReasoningTokens, c.CacheReadTokens, c.CacheCreationTokens, c.TotalTokens,
		c.CacheHits, c.LatencyNS, c.LatencySamples, c.TTFTNS, c.TTFTSamples,
		c.completeRequests, c.billableInput, c.billableRead, c.billableWrite, c.billableOutput,
	}
}

func withPersistedWords(c Counters, w [persistedCounterWords]uint64) Counters {
	c.Requests = w[0]
	c.FailedRequests = w[1]
	c.InputTokens = w[2]
	c.OutputTokens = w[3]
	c.ReasoningTokens = w[4]
	c.CacheReadTokens = w[5]
	c.CacheCreationTokens = w[6]
	c.TotalTokens = w[7]
	c.CacheHits = w[8]
	c.LatencyNS = w[9]
	c.LatencySamples = w[10]
	c.TTFTNS = w[11]
	c.TTFTSamples = w[12]
	c.completeRequests = w[13]
	c.billableInput = w[14]
	c.billableRead = w[15]
	c.billableWrite = w[16]
	c.billableOutput = w[17]
	return c
}

// applyContributionDelta replaces old by next inside an existing aggregate row.
// Only fields that actually change are touched, so a saturated but unchanged
// counter never blocks an otherwise safe repair. A negative difference that
// would underflow a saturated or too-small counter is rejected.
func applyContributionDelta(row, old, next Counters) (Counters, bool) {
	rw := persistedWords(row)
	ow := persistedWords(old)
	nw := persistedWords(next)
	for i := range rw {
		switch {
		case nw[i] == ow[i]:
		case nw[i] > ow[i]:
			delta := nw[i] - ow[i]
			if rw[i] > math.MaxUint64-delta {
				return Counters{}, false
			}
			rw[i] += delta
		default:
			delta := ow[i] - nw[i]
			if rw[i] == math.MaxUint64 || rw[i] < delta {
				return Counters{}, false
			}
			rw[i] -= delta
		}
	}
	return withPersistedWords(row, rw), true
}

// subtractContribution removes a whole contribution from an aggregate row. It
// refuses to subtract from a saturated value or one that does not contain the
// contribution.
func subtractContribution(row, contrib Counters) (Counters, bool) {
	rw := persistedWords(row)
	cw := persistedWords(contrib)
	for i, v := range cw {
		if v == 0 {
			continue
		}
		if rw[i] == math.MaxUint64 || rw[i] < v {
			return Counters{}, false
		}
		rw[i] -= v
	}
	return withPersistedWords(row, rw), true
}

// addContribution adds a whole contribution to an aggregate row, refusing to
// wrap a counter past its maximum.
func addContribution(row, contrib Counters) (Counters, bool) {
	rw := persistedWords(row)
	cw := persistedWords(contrib)
	for i, v := range cw {
		if v == 0 {
			continue
		}
		if rw[i] > math.MaxUint64-v {
			return Counters{}, false
		}
		rw[i] += v
	}
	return withPersistedWords(row, rw), true
}

// marshalStoredRequest serializes a request for disk exactly like the writer:
// the derived CostUSD field is shadowed and never persisted.
func marshalStoredRequest(r Request) ([]byte, error) {
	return json.Marshal(struct {
		Request
		CostUSD *float64 `json:"cost_usd,omitempty"`
	}{Request: r})
}
