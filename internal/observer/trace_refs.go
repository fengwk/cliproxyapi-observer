package observer

import (
	"encoding/binary"
	"fmt"
	"math"

	bolt "go.etcd.io/bbolt"
)

func encodeTraceBodyRefs(count uint64) []byte {
	value := make([]byte, 8)
	binary.BigEndian.PutUint64(value, count)
	return value
}

func traceBodyRefCount(refs *bolt.Bucket, key []byte, allowMissing bool) (uint64, error) {
	if refs == nil {
		return 0, fmt.Errorf("missing trace body reference bucket")
	}
	value := refs.Get(key)
	if value == nil && allowMissing {
		return 0, nil
	}
	if len(value) != 8 {
		return 0, fmt.Errorf("corrupt trace body reference count")
	}
	count := binary.BigEndian.Uint64(value)
	if count == 0 {
		return 0, fmt.Errorf("trace body reference count underflow")
	}
	return count, nil
}

func incrementTraceBodyRef(refs *bolt.Bucket, key []byte, allowMissing bool) (uint64, error) {
	count, err := traceBodyRefCount(refs, key, allowMissing)
	if err != nil {
		return 0, err
	}
	if count == math.MaxUint64 {
		return 0, fmt.Errorf("trace body reference count overflow")
	}
	count++
	return count, refs.Put(key, encodeTraceBodyRefs(count))
}

// Rebuild once per Open: older writers may have removed bodies without
// maintaining refs. Recreate counts atomically from the existing metadata,
// then retire orphan trace mappings, including legacy ambiguous markers.
// Live ambiguity is never promoted to a surviving unique body.
func (s *Store) rebuildTraceBodyRefs() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket(bucketTraceBodyRefs); err != nil {
			return err
		}
		refs, err := tx.CreateBucket(bucketTraceBodyRefs)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketBodyMeta).ForEach(func(_, value []byte) error {
			meta, ok := decodeBodyMeta(value)
			if !ok {
				return fmt.Errorf("corrupt body metadata")
			}
			if meta.traceID == "" {
				return nil
			}
			_, err := incrementTraceBodyRef(refs, []byte(meta.traceID), true)
			return err
		}); err != nil {
			return err
		}
		c := tx.Bucket(bucketTraceBody).Cursor()
		for key, _ := c.First(); key != nil; key, _ = c.Next() {
			if refs.Get(key) == nil {
				if err := c.Delete(); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
