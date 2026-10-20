package engine

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// A real feature-1/hash-only row must remain authenticatable after upgrade.
// The new plaintext field is additive, so an unavailable historical secret stays
// empty (never the internal ID) until an explicit rotation supplies new material.
func TestFeatureOneHashOnlyKeyUpgradeKeepsIdentityAndAuthentication(t *testing.T) {
	s, now, p := fixture(t)
	k, secret := key(t, s, KeySpec{Enabled: true, Limits: Limits{Total: 100}})
	must(t, s.RecordUsage(event(k, "legacy-record", *now)))
	path := s.path
	must(t, s.Close())

	db, e := bolt.Open(path, 0600, nil)
	must(t, e)
	must(t, db.Update(func(tx *bolt.Tx) error {
		keys := tx.Bucket([]byte("keys"))
		var fields map[string]json.RawMessage
		if e := json.Unmarshal(keys.Get([]byte(k.ID)), &fields); e != nil {
			return e
		}
		delete(fields, "secret")
		encoded, e := json.Marshal(fields)
		if e != nil {
			return e
		}
		if e := keys.Put([]byte(k.ID), encoded); e != nil {
			return e
		}
		return tx.Bucket([]byte("meta")).Put([]byte("observer_features"), []byte("1"))
	}))
	must(t, db.Close())

	r, e := Open(Options{DataPath: path, Now: func() time.Time { return *now }, Pricer: p})
	must(t, e)
	defer r.Close()

	listed, e := r.Keys()
	must(t, e)
	if len(listed) != 1 || listed[0].ID != k.ID || listed[0].Secret != "" || listed[0].Used.Total != 7 {
		t.Fatal("legacy key lost identity, counters or invented plaintext", listed)
	}
	if _, e := r.Authenticate(secret); e != nil {
		t.Fatal("legacy hash-only key stopped authenticating", e)
	}
	updated, e := r.UpdateKey(k.ID, KeySpec{Enabled: true, Limits: Limits{Total: 100}})
	must(t, e)
	if updated.ID != k.ID || updated.Secret != "" {
		t.Fatal("missing plaintext was substituted or identity changed", updated)
	}
	rotated, next, e := r.RotateKey(k.ID)
	must(t, e)
	if rotated.ID != k.ID || rotated.Secret != next || next == secret || next == k.ID {
		t.Fatal("rotation did not supply distinct plaintext or changed identity", rotated)
	}
	if _, e := r.Authenticate(secret); !errors.Is(e, ErrUnauthorized) {
		t.Fatal("rotation left the legacy credential authenticatable", e)
	}
	if authenticated, e := r.Authenticate(next); e != nil || authenticated.Secret != next {
		t.Fatal("rotated credential or plaintext unavailable", e)
	}
	must(t, r.db.View(func(tx *bolt.Tx) error {
		if string(tx.Bucket([]byte("meta")).Get([]byte("observer_features"))) != "2" {
			t.Fatal("upgraded writer did not publish feature 2")
		}
		var stored storedKey
		if e := get(tx.Bucket([]byte("keys")), k.ID, &stored); e != nil {
			return e
		}
		if stored.Secret != next {
			t.Fatal("rotated plaintext not stored transactionally")
		}
		return nil
	}))
}
