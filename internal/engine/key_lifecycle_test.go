package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Generated credentials are sk- plus 48 unbiased alphanumeric characters while
// internal IDs remain the existing 64-hex identity used by accounting.
func TestGeneratedKeySecretFormat(t *testing.T) {
	s, _, _ := fixture(t)
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		k, secret, e := s.CreateKey(KeySpec{Enabled: true})
		must(t, e)
		if !strings.HasPrefix(secret, "sk-") || len(secret) != 51 || len(k.ID) != 64 {
			t.Fatalf("unexpected generated shape: %q %q", secret, k.ID)
		}
		for _, c := range secret[3:] {
			if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
				t.Fatalf("non-alphanumeric secret character %q", c)
			}
		}
		for _, c := range k.ID {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Fatalf("ID left hex format: %q", k.ID)
			}
		}
		if seen[secret] {
			t.Fatal("duplicate generated secret")
		}
		seen[secret] = true
	}
	// Custom material is preserved exactly, not trimmed into another credential.
	if _, secret, e := s.CreateKeyWithSecret(KeySpec{Enabled: true}, "Custom-Secret!"); e != nil || secret != "Custom-Secret!" {
		t.Fatal(e, secret)
	}
}

// An active request makes delete busy; a completed key becomes a tombstone
// that rejects every mutation while preserving history and counters.
func TestDeleteKeyLifecycleBusyAuthAndHistory(t *testing.T) {
	s, now, p := fixture(t)
	k, secret := key(t, s, KeySpec{Enabled: true, Limits: Limits{Total: 100}})
	must(t, s.RecordUsage(event(k, "bill", *now)))
	must(t, s.Admit(Admission{RequestID: "active", KeyID: k.ID, Model: "gpt-exact"}))
	if e := s.DeleteKey(k.ID); !errors.Is(e, ErrBusy) {
		t.Fatal("deleted an active key", e)
	}
	must(t, s.Complete("active", *now))
	if e := s.DeleteKey(k.ID); e != nil {
		t.Fatal(e)
	}
	if e := s.DeleteKey(k.ID); e != nil {
		t.Fatal("delete is not idempotent", e)
	}
	if _, e := s.Authenticate(secret); !errors.Is(e, ErrUnauthorized) {
		t.Fatal("deleted credential still authenticates", e)
	}
	must(t, s.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("hashes")).Get([]byte(hash(secret))) != nil {
			t.Fatal("authentication hash not cleared")
		}
		var stored storedKey
		if e := get(tx.Bucket([]byte("keys")), k.ID, &stored); e != nil {
			return e
		}
		if stored.Hash != "" || stored.Secret != "" || stored.Enabled || stored.DeletedAt == nil {
			t.Fatal("tombstone incomplete", stored)
		}
		return nil
	}))
	list, e := s.Keys()
	must(t, e)
	if len(list) != 0 {
		t.Fatal("default list includes tombstone", list)
	}
	all, e := s.KeysIncludingDeleted()
	must(t, e)
	if len(all) != 1 || all[0].DeletedAt == nil || all[0].Revoked || all[0].Used.Total != 7 || all[0].Secret != "" {
		t.Fatal("tombstone/history lost", all)
	}
	if _, e = s.UpdateKey(k.ID, KeySpec{Enabled: false}); !errors.Is(e, ErrDeleted) {
		t.Fatal("update accepted", e)
	}
	if _, _, e = s.RotateKey(k.ID); !errors.Is(e, ErrDeleted) {
		t.Fatal("rotate accepted", e)
	}
	if _, _, e = s.RotateKeyWithSecret(k.ID, "another-secret!"); !errors.Is(e, ErrDeleted) {
		t.Fatal("custom rotate accepted", e)
	}
	if e = s.RevokeKey(k.ID); !errors.Is(e, ErrDeleted) {
		t.Fatal("revoke accepted", e)
	}
	if _, e = s.ResetKey(k.ID, []string{"total"}); !errors.Is(e, ErrDeleted) {
		t.Fatal("reset accepted", e)
	}
	d, e := s.Detail("bill")
	must(t, e)
	if d.Record.CostMicroUSD != 7 {
		t.Fatal("history removed", d.Record)
	}
	if _, _, e = s.CreateKeyWithSecret(KeySpec{Enabled: true}, secret); e != nil {
		t.Fatal("cleared hash was not reusable", e)
	}
	must(t, s.Close())

	r, e := Open(Options{DataPath: s.path, Now: func() time.Time { return *now }, Pricer: p})
	must(t, e)
	defer r.Close()
	all, e = r.KeysIncludingDeleted()
	must(t, e)
	deleted := 0
	for _, v := range all {
		if v.DeletedAt != nil {
			deleted++
		}
	}
	if len(all) != 2 || deleted != 1 {
		t.Fatal("tombstone not durable", all)
	}
	if _, e = r.Authenticate(secret); e != nil {
		t.Fatal("replacement key lost on restart", e)
	}
	if _, e = r.Detail("bill"); e != nil {
		t.Fatal("history lost on restart", e)
	}
}

// A failed accounting write does not bar deletion: the Key is tombstoned and
// keeps its history, while an active request still keeps the Key alive.
func TestDeleteKeyIgnoresFailedAccountingButKeepsActiveGuard(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	s.writeFault = func() error { return ErrBusy }
	if e := s.RecordUsage(event(k, "debt", *now)); !errors.Is(e, ErrAccounting) {
		t.Fatal(e)
	}
	// The failed write left neither a record nor a charge behind.
	if _, e := s.Detail("debt"); !errors.Is(e, ErrNotFound) {
		t.Fatal("failed write left a record", e)
	}
	// An active request still blocks the delete.
	must(t, s.Admit(Admission{RequestID: "active", KeyID: k.ID}))
	if e := s.DeleteKey(k.ID); !errors.Is(e, ErrBusy) {
		t.Fatal("deleted an active key", e)
	}
	must(t, s.Complete("active", *now))
	// The accounting failure does not bar the delete.
	if e := s.DeleteKey(k.ID); e != nil {
		t.Fatal(e)
	}
	all, e := s.KeysIncludingDeleted()
	must(t, e)
	if len(all) != 1 || all[0].DeletedAt == nil || all[0].Used.Total != 0 || all[0].Secret != "" {
		t.Fatal("tombstone/history lost", all)
	}
}

// Revocation clears the hash and plaintext and disables the key but is not a
// tombstone. Enabling requires rotation; a plain disable keeps both the hash and
// current plaintext and is reversible.
func TestRevokedVersusDisabled(t *testing.T) {
	s, _, _ := fixture(t)
	k, old := key(t, s, KeySpec{Enabled: true})
	disabled, e := s.UpdateKey(k.ID, KeySpec{Enabled: false})
	must(t, e)
	if disabled.Revoked || disabled.Enabled || disabled.DeletedAt != nil || disabled.Secret != old {
		t.Fatal("disabled misclassified or lost plaintext", disabled)
	}
	if _, e = s.Authenticate(old); !errors.Is(e, ErrUnauthorized) {
		t.Fatal("disabled authenticates", e)
	}
	enabled, e := s.UpdateKey(k.ID, KeySpec{Enabled: true})
	must(t, e)
	if enabled.Revoked || enabled.Secret != old {
		t.Fatal("enabled key marked revoked or lost plaintext", enabled)
	}
	if _, e = s.Authenticate(old); e != nil {
		t.Fatal("enable after disable lost hash", e)
	}
	must(t, s.RevokeKey(k.ID))
	revoked := total(t, s)
	if !revoked.Revoked || revoked.Enabled || revoked.DeletedAt != nil || revoked.Secret != "" {
		t.Fatal("revocation not distinct or retained plaintext", revoked)
	}
	if _, e = s.Authenticate(old); !errors.Is(e, ErrUnauthorized) {
		t.Fatal("revoked credential valid")
	}
	if _, e = s.UpdateKey(k.ID, KeySpec{Enabled: true}); !errors.Is(e, ErrConflict) {
		t.Fatal("revoked key enabled without rotation", e)
	}
	rotated, next, e := s.RotateKey(k.ID)
	must(t, e)
	if rotated.Revoked || rotated.Enabled || rotated.Secret != next {
		t.Fatal("rotation did not leave key disabled or lost plaintext", rotated)
	}
	if _, e = s.Authenticate(next); !errors.Is(e, ErrUnauthorized) {
		t.Fatal("rotation activated disabled key", e)
	}
	if _, e = s.UpdateKey(k.ID, KeySpec{Enabled: true}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Authenticate(next); e != nil {
		t.Fatal("explicit enable after rotation failed", e)
	}
}
