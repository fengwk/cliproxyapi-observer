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

// The hash index and Key update must commit together, including for disabled
// Keys. Failed rotations must leave both authentication and accounting intact.
func TestCustomSecretAtomicRotationAndPersistence(t *testing.T) {
	s, now, _ := fixture(t)
	first, secret, err := s.CreateKeyWithSecret(KeySpec{Enabled: true, Label: "client", Limits: Limits{Total: 100}}, "first-secret-exact!")
	must(t, err)
	other, _, err := s.CreateKeyWithSecret(KeySpec{Enabled: false}, "disabled-secret!")
	must(t, err)
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
		t.Fatal("failed rotation modified Key state")
	}
	rotated, next, err := s.RotateKeyWithSecret(first.ID, "replacement-secret-exact!")
	must(t, err)
	for _, old := range before {
		if old.ID == first.ID && !reflect.DeepEqual(old, rotated) {
			t.Fatal("successful rotation changed identity, counters or windows")
		}
	}
	if _, err = s.Authenticate(secret); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old credential survived rotation", err)
	}
	if authenticated, err := s.Authenticate(next); err != nil || authenticated.ID != first.ID {
		t.Fatal("replacement rejected", err)
	}
	// Inspect real Bolt storage, not a mock, for both plaintext exclusion and
	// hash lookup integrity; then reopen the same schema2 file.
	path := s.db.Path()
	must(t, s.db.View(func(tx *bolt.Tx) error {
		if string(tx.Bucket([]byte("hashes")).Get([]byte(hash(next)))) != first.ID {
			t.Fatal("hash index missing")
		}
		return nil
	}))
	must(t, s.Close())
	data, err := os.ReadFile(path)
	must(t, err)
	if bytes.Contains(data, []byte(secret)) || bytes.Contains(data, []byte(next)) {
		t.Fatal("plaintext credential persisted")
	}
	reopened, err := Open(Options{DataPath: path, Pricer: &fakePrice{}})
	must(t, err)
	defer reopened.Close()
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
