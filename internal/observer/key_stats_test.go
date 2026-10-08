package observer

import (
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Rollback cannot mark a partial copy complete or alter authoritative totals.
func TestKeyStatsMigrationRollback(t *testing.T) {
	cfg := testConfig(t, nil)
	db, err := bolt.Open(cfg.DataPath, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Store{db: db}
	if err := s.initBuckets(); err != nil {
		t.Fatal(err)
	}
	minute := statsMinute(time.Now())
	good := encodeCounters(Counters{Requests: 1})
	if err := db.Update(func(tx *bolt.Tx) error {
		sb := tx.Bucket(bucketStats)
		if err := sb.Put(statsKey(minute, "a", "m"), good); err != nil {
			return err
		}
		return sb.Put(statsKey(minute, "b", "m"), []byte("corrupt"))
	}); err != nil {
		t.Fatal(err)
	}
	if s.migrateKeyStats() == nil {
		t.Fatal("corrupt migration succeeded")
	}
	if err := db.View(func(tx *bolt.Tx) error {
		kb := tx.Bucket(bucketKeyStats)
		if kb.Sequence() != 0 || kb.Stats().KeyN != 0 {
			t.Fatal("partial migration committed")
		}
		if string(tx.Bucket(bucketStats).Get(statsKey(minute, "b", "m"))) != "corrupt" {
			t.Fatal("migration rewrote source")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketStats).Put(statsKey(minute, "b", "m"), good)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateKeyStats(); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *bolt.Tx) error {
		kb := tx.Bucket(bucketKeyStats)
		if kb.Sequence() != 1 || kb.Stats().KeyN != 2 {
			t.Fatal("migration retry did not complete")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
