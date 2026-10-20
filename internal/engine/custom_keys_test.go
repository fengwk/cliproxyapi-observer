package engine

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// Invalid material must never be silently normalized into a different valid
// credential. Boundary cases exercise the actual create and rotate paths.
func TestCustomSecretValidation(t *testing.T) {
	s, _, _ := fixture(t)
	k, secret := key(t, s, KeySpec{Enabled: true})
	for _, candidate := range []string{"", "1234567", strings.Repeat("x", 257),
		" leading", "trailing ", "in side!", "new\nline", "tab\tkey!", "nul\x00key!", "密钥12345678", "delete\x7f!"} {
		if _, revealed, err := s.CreateKeyWithSecret(KeySpec{Enabled: true}, candidate); !errors.Is(err, ErrInvalid) || revealed != "" {
			t.Fatalf("create accepted invalid credential: %v", err)
		}
		if _, revealed, err := s.RotateKeyWithSecret(k.ID, candidate); !errors.Is(err, ErrInvalid) || revealed != "" {
			t.Fatalf("rotation accepted invalid credential: %v", err)
		}
	}
	for _, size := range []int{8, 256} {
		candidate := strings.Repeat("x", size)
		if _, revealed, err := s.CreateKeyWithSecret(KeySpec{Enabled: true}, candidate); err != nil || revealed != candidate {
			t.Fatalf("valid boundary %d: %v", size, err)
		}
	}
	if authenticated, err := s.Authenticate(secret); err != nil || authenticated.ID != k.ID {
		t.Fatal("invalid attempts damaged original authentication")
	}
}

// The hash index, plaintext display material and Key update must commit
// together, including for disabled Keys. Failed rotations must leave both
// authentication, stored plaintext and accounting intact.
func TestCustomSecretAtomicRotationAndPersistence(t *testing.T) {
	s, now, _ := fixture(t)
	first, secret, err := s.CreateKeyWithSecret(KeySpec{Enabled: true, Label: "client", Limits: Limits{Total: 100}}, "first-secret-exact!")
	must(t, err)
	if first.Secret != secret {
		t.Fatal("created key did not expose its current plaintext")
	}
	other, otherSecret, err := s.CreateKeyWithSecret(KeySpec{Enabled: false}, "disabled-secret!")
	must(t, err)
	if other.Secret != otherSecret || other.Enabled {
		t.Fatal("disabled key did not retain its current plaintext")
	}
	must(t, s.RecordUsage(event(first, "custom-record", *now)))
	before, err := s.Keys()
	must(t, err)
	for _, candidate := range []string{secret, "disabled-secret!"} {
		_, revealed, err := s.RotateKeyWithSecret(first.ID, candidate)
		want := ErrConflict
		if candidate == secret {
			want = ErrInvalid
		}
		if !errors.Is(err, want) || revealed != "" {
			t.Fatal("failed rotation revealed material or lost error", err)
		}
	}
	if _, _, err := s.CreateKeyWithSecret(KeySpec{}, "disabled-secret!"); !errors.Is(err, ErrConflict) {
		t.Fatal("disabled credential collision not rejected", other.ID, err)
	}
	after, err := s.Keys()
	must(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed rotation modified Key state or displayed plaintext")
	}
	rotated, next, err := s.RotateKeyWithSecret(first.ID, "replacement-secret-exact!")
	must(t, err)
	if rotated.Secret != next {
		t.Fatal("rotation did not publish the replacement plaintext")
	}
	for _, old := range before {
		if old.ID != first.ID {
			continue
		}
		// Identity, counters, windows and policy are unchanged; only the
		// current plaintext is allowed to differ.
		old.Secret = ""
		comparable := rotated
		comparable.Secret = ""
		if !reflect.DeepEqual(old, comparable) {
			t.Fatal("successful rotation changed identity, counters or windows")
		}
	}
	if _, err = s.Authenticate(secret); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old credential survived rotation", err)
	}
	authenticated, err := s.Authenticate(next)
	if err != nil || authenticated.ID != first.ID || authenticated.Secret != next {
		t.Fatal("replacement rejected or failed to expose current plaintext", err)
	}
	// Inspect the live Bolt value, not a mock: the current plaintext and hash
	// index coexist. Old plaintext may remain in retained bbolt pages, so this
	// test makes no physical-erasure claim.
	path := s.db.Path()
	must(t, s.db.View(func(tx *bolt.Tx) error {
		if string(tx.Bucket([]byte("hashes")).Get([]byte(hash(next)))) != first.ID {
			t.Fatal("hash index missing")
		}
		var stored storedKey
		if e := get(tx.Bucket([]byte("keys")), first.ID, &stored); e != nil {
			return e
		}
		if stored.Secret != next || stored.Hash != hash(next) {
			t.Fatal("live Key value lost current plaintext or hash", stored.Key)
		}
		return nil
	}))
	must(t, s.Close())
	data, err := os.ReadFile(path)
	must(t, err)
	if !bytes.Contains(data, []byte(next)) {
		t.Fatal("current plaintext missing from durable database")
	}
	reopened, err := Open(Options{DataPath: path, Pricer: &fakePrice{}})
	must(t, err)
	defer reopened.Close()
	listed, err := reopened.Keys()
	must(t, err)
	secrets := map[string]string{}
	for _, current := range listed {
		secrets[current.ID] = current.Secret
	}
	if len(listed) != 2 || secrets[first.ID] != next || secrets[other.ID] != otherSecret {
		t.Fatal("stored plaintext lost on restart", listed)
	}
	if authenticated, err := reopened.Authenticate(next); err != nil || authenticated.ID != first.ID {
		t.Fatal("custom credential lost on restart", err)
	}
}

// Concurrent attempts race through the production mutex and Bolt transaction.
// Exactly one may claim a supplied secret; failed callers reveal nothing.
func TestConcurrentCustomSecretCreation(t *testing.T) {
	s, _, _ := fixture(t)
	var success, conflicts atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, revealed, err := s.CreateKeyWithSecret(KeySpec{Enabled: true}, "one-shared-secret!")
			switch {
			case err == nil:
				success.Add(1)
			case errors.Is(err, ErrConflict) && revealed == "":
				conflicts.Add(1)
			default:
				t.Errorf("unexpected result: %v", err)
			}
		}()
	}
	wg.Wait()
	if success.Load() != 1 || conflicts.Load() != 15 {
		t.Fatal("credential uniqueness is not atomic")
	}
}
