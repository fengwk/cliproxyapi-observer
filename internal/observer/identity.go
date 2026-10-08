package observer

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketIdentity  = []byte("identity")
	clientSecretKey = []byte("client-key-hmac-v1")
)

// Keep the random secret with the database so reopening, backup and compaction
// preserve identities. Only fingerprints, never access keys, enter request rows.
func (s *Store) initClientKeySecret() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketIdentity)
		if err != nil {
			return err
		}
		secret := b.Get(clientSecretKey)
		if secret == nil {
			if _, err := rand.Read(s.clientKeySecret[:]); err != nil {
				return errors.New("generate observer identity secret")
			}
			return b.Put(clientSecretKey, s.clientKeySecret[:])
		}
		if len(secret) != len(s.clientKeySecret) {
			return errors.New("invalid observer identity secret")
		}
		copy(s.clientKeySecret[:], secret)
		return nil
	})
}

func (s *Store) clientKeyID(principal string) string {
	if principal == "" {
		return ""
	}
	mac := hmac.New(sha256.New, s.clientKeySecret[:])
	_, _ = mac.Write([]byte("observer-client-key-v1\x00"))
	_, _ = mac.Write([]byte(principal))
	return hex.EncodeToString(mac.Sum(nil))
}

func validHexID(id string, size int) bool {
	if len(id) != size {
		return false
	}
	for _, ch := range id {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

// ValidClientKeyFilter accepts a full fingerprint or the legacy/unattributed bucket.
func ValidClientKeyFilter(id string) bool {
	return id == "" || id == "unknown" || validHexID(id, 64)
}

// ValidAuthFilter accepts CPA's nonsecret credential index or an unknown bucket.
func ValidAuthFilter(id string) bool {
	return id == "" || id == "unknown" || validHexID(id, 16)
}

func matchesIdentity(id, filter string) bool {
	return filter == "" || filter == "unknown" && id == "" || filter == id
}
