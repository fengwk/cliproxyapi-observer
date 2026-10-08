package observer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"

	bolt "go.etcd.io/bbolt"
)

// The stats bucket sequence is a startup migration marker, not a request
// sequence. Once upgraded, reopening does not scan requests or stats again.
func (s *Store) migrateStats() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		sb := tx.Bucket(bucketStats)
		if sb.Sequence() == 1 {
			return nil
		}
		legacy := make(map[string]Counters)
		recoverRows := make(map[string]bool)
		if err := sb.ForEach(func(key, value []byte) error {
			if _, _, _, ok := parseStatsKey(key); !ok {
				return fmt.Errorf("malformed legacy stats key")
			}
			c, ok := decodeCounters(value)
			if !ok {
				return fmt.Errorf("corrupt stats record during migration")
			}
			if len(value) != legacyCountersSize {
				return nil
			}
			// Only old fully priced buckets prove that every row was complete.
			// Saturated totals cannot prove a valid exclusive subtraction.
			full := c.UnpricedRequests == 0 &&
				c.Requests != math.MaxUint64 && c.InputTokens != math.MaxUint64 &&
				c.CacheReadTokens != math.MaxUint64 && c.CacheCreationTokens != math.MaxUint64 &&
				c.OutputTokens != math.MaxUint64 &&
				c.InputTokens >= c.CacheReadTokens &&
				c.InputTokens-c.CacheReadTokens >= c.CacheCreationTokens
			if full {
				c.completeRequests = c.Requests
				c.billableInput = c.InputTokens - c.CacheReadTokens - c.CacheCreationTokens
				c.billableRead = c.CacheReadTokens
				c.billableWrite = c.CacheCreationTokens
				c.billableOutput = c.OutputTokens
			} else {
				recoverRows[string(key)] = true
			}
			c.UnpricedRequests = 0
			legacy[string(key)] = c
			return nil
		}); err != nil {
			return err
		}
		if len(recoverRows) > 0 {
			matched := make(map[string]uint64)
			if err := tx.Bucket(bucketRequests).ForEach(func(key, value []byte) error {
				var r Request
				if len(key) != requestKeyLen || json.Unmarshal(value, &r) != nil {
					return fmt.Errorf("corrupt retained request during migration")
				}
				if !bytes.Equal(key, encodeRequestKey(r.Time, r.Sequence)) {
					return fmt.Errorf("retained request key mismatch during migration")
				}
				sk := string(statsKey(statsMinute(r.Time), r.Provider, r.Model))
				if !recoverRows[sk] {
					return nil
				}
				c := legacy[sk]
				if matched[sk] >= c.Requests {
					return fmt.Errorf("retained request count exceeds legacy stats")
				}
				matched[sk]++
				if r.AccountingQuality == "complete" && !completeTokens(r) {
					return fmt.Errorf("invalid complete token buckets during migration")
				}
				addBillable(&c, r)
				legacy[sk] = c
				return nil
			}); err != nil {
				return err
			}
		}
		for key, c := range legacy {
			if err := sb.Put([]byte(key), encodeCounters(c)); err != nil {
				return err
			}
		}
		// Avoid using a metadata framework for a single fixed schema upgrade.
		return sb.SetSequence(1)
	})
}
