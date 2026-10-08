package observer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"

	bolt "go.etcd.io/bbolt"
)

var bucketKeyStats = []byte("key_stats_v1")

// Minute/input-length/client/auth/provider/model preserves query-time token tiers
// after request expiry. Input=-1 marks legacy aggregates with unknown lengths.
func keyStatsKey(minute, input int64, client, auth, provider, model string) []byte {
	key := make([]byte, 16+len(client)+len(auth)+len(provider)+len(model)+3)
	binary.BigEndian.PutUint64(key, uint64(minute))
	binary.BigEndian.PutUint64(key[8:], uint64(input))
	copy(key[16:], client+"\x00"+auth+"\x00"+provider+"\x00"+model)
	return key
}

func parseKeyStatsKey(key []byte) (minute, input int64, client, auth, provider, model string, ok bool) {
	if len(key) < 19 {
		return
	}
	parts := bytes.SplitN(key[16:], []byte{0}, 4)
	if len(parts) != 4 {
		return
	}
	client, auth, provider, model = string(parts[0]), string(parts[1]), string(parts[2]), string(parts[3])
	if (client != "" && !validHexID(client, 64)) || (auth != "" && !validHexID(auth, 16)) {
		return
	}
	return int64(binary.BigEndian.Uint64(key)), int64(binary.BigEndian.Uint64(key[8:])), client, auth, provider, model, true
}

// Legacy totals have no attribution. Copy them atomically once into the unknown
// bucket, without scanning short-lived request rows or inventing identities.
func (s *Store) migrateKeyStats() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		kb, err := tx.CreateBucketIfNotExists(bucketKeyStats)
		if err != nil {
			return err
		}
		if kb.Sequence() == 1 {
			return nil
		}
		if err := tx.Bucket(bucketStats).ForEach(func(key, value []byte) error {
			minute, provider, model, ok := parseStatsKey(key)
			if !ok {
				return fmt.Errorf("invalid stats key during identity migration")
			}
			if _, ok := decodeCounters(value); !ok {
				return fmt.Errorf("corrupt stats during identity migration")
			}
			return kb.Put(keyStatsKey(minute, -1, "", "", provider, model), value)
		}); err != nil {
			return err
		}
		return kb.SetSequence(1)
	})
}

func visitKeyStats(tx *bolt.Tx, query Query, from, to int64, visit func(int64, int64, string, string, string, string, Counters)) error {
	kb := tx.Bucket(bucketKeyStats)
	if kb == nil {
		return nil
	}
	c := kb.Cursor()
	prefix := make([]byte, 8)
	binary.BigEndian.PutUint64(prefix, uint64(from))
	for key, value := c.Seek(prefix); key != nil; key, value = c.Next() {
		minute, input, client, auth, provider, model, ok := parseKeyStatsKey(key)
		if !ok {
			return fmt.Errorf("invalid key stats record")
		}
		if minute > to {
			break
		}
		if !matchesQuery(Request{Provider: provider, Model: model, ClientKeyID: client, AuthIndex: auth}, query) {
			continue
		}
		counters, ok := decodeCounters(value)
		if !ok {
			return fmt.Errorf("corrupt key stats record")
		}
		visit(minute, input, client, auth, provider, model, counters)
	}
	return nil
}

func sortedKeyGroups(groups map[string]Counters) []KeyGroup {
	out := make([]KeyGroup, 0, len(groups))
	for id, counters := range groups {
		out = append(out, KeyGroup{ID: id, Counters: counters})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
