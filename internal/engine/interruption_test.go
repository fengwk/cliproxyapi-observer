//go:build linux || darwin

package engine

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// A detached native runtime may report a lost callback after clean Close.
// The next independent Store sees the marker and pauses only its principal.
func TestInterruptionAcrossStoreLifetimes(t *testing.T) {
	s, now, p := fixture(t)
	a, _ := key(t, s, KeySpec{Enabled: true, Limits: Limits{Total: 100}})
	b, _ := key(t, s, KeySpec{Enabled: true})
	must(t, s.RecordUsage(event(a, "charged", *now)))
	must(t, s.Close())
	for i := 0; i < 3; i++ {
		must(t, RecordInterruption(s.path, a.ID))
	}
	path, e := interruptionPath(s.path, a.ID)
	must(t, e)
	entries, e := os.ReadDir(filepath.Dir(path))
	must(t, e)
	if len(entries) != 1 || entries[0].Name() != a.ID {
		t.Fatal(entries)
	}
	st, e := os.Stat(path)
	must(t, e)
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
	body, e := os.ReadFile(path)
	must(t, e)
	if string(body) != "unknown accounting\n" {
		t.Fatal("unexpected marker payload")
	}
	r, e := Open(Options{DataPath: s.path, Now: func() time.Time { return *now }, Pricer: p})
	must(t, e)
	if r.Health().LastError == "" {
		t.Fatal("marker absent from health")
	}
	if e = r.Admit(Admission{RequestID: "blocked", KeyID: a.ID}); !errors.Is(e, ErrAccounting) {
		t.Fatal(e)
	}
	must(t, r.Admit(Admission{RequestID: "other", KeyID: b.ID}))
	must(t, r.Complete("other", *now))
	fresh, _ := key(t, r, KeySpec{Enabled: true})
	must(t, r.Admit(Admission{RequestID: "fresh", KeyID: fresh.ID}))
	must(t, r.Complete("fresh", *now))
	if _, e = r.RecoverKey(a.ID, false); !errors.Is(e, ErrAccounting) {
		t.Fatal("implicit acknowledgement", e)
	}
	// Close does not consume a marker, even after persisting a DB fence.
	must(t, r.Close())
	if _, e = os.Stat(path); e != nil {
		t.Fatal("Close removed marker", e)
	}
	r, e = Open(Options{DataPath: s.path, Now: func() time.Time { return *now }, Pricer: p})
	must(t, e)
	defer r.Close()
	k, e := r.RecoverKey(a.ID, true)
	must(t, e)
	if k.Used.Total != 7 || k.Limits.Total != 100 || !k.Starts.Total.Equal(a.Starts.Total) || k.Resets != a.Resets || k.BlockedReason != "" {
		t.Fatal("recovery mutated accounting", k)
	}
	d, e := r.Detail("charged")
	must(t, e)
	if d.Record.CostMicroUSD != 7 {
		t.Fatal(d)
	}
	if _, e = os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("marker not removed", e)
	}
	must(t, r.Admit(Admission{RequestID: "resumed", KeyID: a.ID}))
	must(t, r.Complete("resumed", *now))
}

// A late callback may arrive after Open. Every admission (even a duplicate)
// and list/health observation must notice the newly published atomic marker.
func TestInterruptionAfterOpen(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	must(t, s.Admit(Admission{RequestID: "active", KeyID: k.ID}))
	must(t, RecordInterruption(s.path, k.ID))
	if s.Health().LastError == "" {
		t.Fatal("late fault not exposed")
	}
	keys, e := s.Keys()
	must(t, e)
	if keys[0].BlockedReason == "" {
		t.Fatal(keys)
	}
	if e = s.Admit(Admission{RequestID: "active", KeyID: k.ID}); !errors.Is(e, ErrAccounting) {
		t.Fatal("duplicate bypassed marker", e)
	}
	_, _, e = s.RotateKey(k.ID)
	must(t, e)
	_, e = s.ResetKey(k.ID, []string{"total"})
	must(t, e)
	if e = s.Admit(Admission{RequestID: "next", KeyID: k.ID}); !errors.Is(e, ErrAccounting) {
		t.Fatal("reset/rotation cleared marker", e)
	}
	must(t, s.Complete("active", *now))
	if _, e = s.RecoverKey(k.ID, false); !errors.Is(e, ErrAccounting) {
		t.Fatal(e)
	}
}

// New interruptions serialize after acknowledgement rather than being erased
// by it. A separate descriptor's nonblocking flock proves an OS-level lock,
// not a Go mutex shared only within one native runtime. No timed sleeps.
func TestInterruptionRecoveryRace(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	must(t, RecordInterruption(s.path, k.ID))
	held := make(chan struct{})
	release := make(chan struct{})
	releaseRecovery := sync.OnceFunc(func() { close(release) })
	defer releaseRecovery()
	s.writeFault = func() error { close(held); <-release; return nil }
	recovered := make(chan error, 1)
	go func() { _, e := s.RecoverKey(k.ID, true); recovered <- e }()
	<-held
	path, e := interruptionPath(s.path, k.ID)
	must(t, e)
	f, e := openPrivateFile(filepath.Dir(path) + ".lock")
	must(t, e)
	e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if e != syscall.EWOULDBLOCK && e != syscall.EAGAIN {
		f.Close()
		releaseRecovery()
		t.Fatal("RecoverKey does not hold file lock", e)
	}
	must(t, f.Close())
	started := make(chan struct{})
	marked := make(chan error, 1)
	go func() { close(started); marked <- RecordInterruption(s.path, k.ID) }()
	<-started
	releaseRecovery()
	must(t, <-recovered)
	must(t, <-marked)
	s.writeFault = nil
	if e = s.Admit(Admission{RequestID: "late", KeyID: k.ID}); !errors.Is(e, ErrAccounting) {
		t.Fatal("newer interruption erased", e)
	}
	_, e = s.RecoverKey(k.ID, true)
	must(t, e)
	must(t, s.Admit(Admission{RequestID: "resumed", KeyID: k.ID}))
	must(t, s.Complete("resumed", *now))
}

// Ordinary retained write failures create no marker and keep their quote.
// A separate interruption cannot be acknowledged until that pending debt settles.
func TestInterruptionPendingCannotBeAcknowledged(t *testing.T) {
	s, now, p := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	s.writeFault = func() error { return ErrBusy }
	if e := s.RecordUsage(event(k, "pending", *now)); !errors.Is(e, ErrAccounting) {
		t.Fatal(e)
	}
	sidecars, e := filepath.Glob(s.path + ".interrupted*")
	must(t, e)
	if len(sidecars) != 0 {
		t.Fatal("retryable write created marker", sidecars)
	}
	must(t, RecordInterruption(s.path, k.ID))
	if _, e = s.RecoverKey(k.ID, true); !errors.Is(e, ErrAccounting) {
		t.Fatal("pending was forgiven", e)
	}
	s.writeFault = nil
	p.cost = 123
	if _, e = s.RecoverKey(k.ID, false); !errors.Is(e, ErrAccounting) {
		t.Fatal("retry implicitly acknowledged interruption", e)
	}
	if len(s.pending) != 0 || total(t, s).Used.Total != 7 {
		t.Fatal("quote was lost or repriced")
	}
	k, e = s.RecoverKey(k.ID, true)
	must(t, e)
	if k.Used.Total != 7 {
		t.Fatal(k)
	}
}

// This helper runs in a second OS process/runtime, proving that the lock
// domain remains shared when no Go globals, mutexes or bbolt handles are shared.
func TestInterruptionSubprocess(t *testing.T) {
	if os.Getenv("OBSERVER_INTERRUPTION_CHILD") != "1" {
		return
	}
	path, id := os.Getenv("OBSERVER_INTERRUPTION_PATH"), os.Getenv("OBSERVER_INTERRUPTION_KEY")
	marker, e := interruptionPath(path, id)
	must(t, e)
	f, e := openPrivateFile(filepath.Dir(marker) + ".lock")
	must(t, e)
	e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if e != syscall.EWOULDBLOCK && e != syscall.EAGAIN {
		t.Fatal("parent runtime lock not visible", e)
	}
	must(t, f.Close())
	fmt.Println("locked")
	must(t, RecordInterruption(path, id))
}
func TestInterruptionIndependentRuntimeLock(t *testing.T) {
	s, _, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	marker, e := interruptionPath(s.path, k.ID)
	must(t, e)
	cmd := exec.Command(os.Args[0], "-test.run=^TestInterruptionSubprocess$")
	cmd.Env = append(os.Environ(), "OBSERVER_INTERRUPTION_CHILD=1", "OBSERVER_INTERRUPTION_PATH="+s.path, "OBSERVER_INTERRUPTION_KEY="+k.ID)
	output, e := cmd.StdoutPipe()
	must(t, e)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	e = withInterruptionLock(marker, func() error {
		if e := cmd.Start(); e != nil {
			return e
		}
		line, e := bufio.NewReader(output).ReadString('\n')
		if e != nil {
			return e
		}
		if strings.TrimSpace(line) != "locked" {
			return fmt.Errorf("unexpected child signal: %q", line)
		}
		return nil
	})
	must(t, e)
	if e = cmd.Wait(); e != nil {
		t.Fatal(e, stderr.String())
	}
	present, e := markerPresent(s.path, k.ID)
	must(t, e)
	if !present {
		t.Fatal("child marker missing")
	}
}

// An actual failed unlink (nonempty directory) must leave a durable DB fence.
// Even loss of the marker afterward cannot silently unpause on a clean restart.
func TestInterruptionRemoveFailure(t *testing.T) {
	s, now, p := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	must(t, RecordInterruption(s.path, k.ID))
	path, e := interruptionPath(s.path, k.ID)
	must(t, e)
	must(t, os.Remove(path))
	must(t, os.Mkdir(path, 0700))
	must(t, os.WriteFile(filepath.Join(path, "block-remove"), []byte("test"), 0600))
	if _, e = s.RecoverKey(k.ID, true); !errors.Is(e, ErrAccounting) {
		t.Fatal("remove error hidden", e)
	}
	must(t, s.db.View(func(tx *bolt.Tx) error {
		var stored storedKey
		if e := get(tx.Bucket([]byte("keys")), k.ID, &stored); e != nil {
			return e
		}
		if stored.BlockedReason == "" {
			t.Fatal("no durable safety fence")
		}
		return nil
	}))
	if e = s.Admit(Admission{RequestID: "bad", KeyID: k.ID}); !errors.Is(e, ErrAccounting) {
		t.Fatal(e)
	}
	must(t, os.RemoveAll(path))
	must(t, s.Close())
	r, e := Open(Options{DataPath: s.path, Now: func() time.Time { return *now }, Pricer: p})
	must(t, e)
	defer r.Close()
	if _, e = r.RecoverKey(k.ID, false); !errors.Is(e, ErrAccounting) {
		t.Fatal("failed removal silently recovered", e)
	}
	_, e = r.RecoverKey(k.ID, true)
	must(t, e)
}

// Reject traversal, unsupported IDs, invalid DB paths and substituted symlink
// directories. Ordinary accounting/list/admission/close never creates sidecars.
func TestInterruptionValidationAndNoNormalSideEffects(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	for _, id := range []string{"", "../escape", strings.Repeat("A", 64), strings.Repeat("g", 64), k.ID + "/x", strings.Repeat("a", 63)} {
		if !errors.Is(RecordInterruption(s.path, id), ErrInvalid) {
			t.Fatal("invalid ID accepted", id)
		}
	}
	for _, path := range []string{"", ".", "..", "/", "bad\x00path", s.path + "/", filepath.Dir(s.path)} {
		if e := RecordInterruption(path, k.ID); !errors.Is(e, ErrInvalid) {
			t.Fatal("invalid path accepted", path, e)
		}
	}
	must(t, s.RecordUsage(event(k, "normal", *now)))
	must(t, s.Admit(Admission{RequestID: "normal", KeyID: k.ID}))
	must(t, s.Complete("normal", *now))
	_, e := s.Keys()
	must(t, e)
	s.Health()
	must(t, s.Close())
	sidecars, e := filepath.Glob(s.path + ".interrupted*")
	must(t, e)
	if len(sidecars) != 0 {
		t.Fatal("normal instance wrote sidecars", sidecars)
	}
	target := t.TempDir()
	must(t, os.Symlink(target, s.path+".interrupted"))
	if e = RecordInterruption(s.path, k.ID); e == nil {
		t.Fatal("symlink marker directory followed")
	}
	entries, e := os.ReadDir(target)
	must(t, e)
	if len(entries) != 0 {
		t.Fatal("wrote outside marker directory")
	}
}

// When the sidecar storage cannot be written, the API must return a failure,
// never claim it persisted a pause. This physical boundary is documented.
func TestInterruptionWriteFailureReported(t *testing.T) {
	s, _, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true})
	must(t, os.Mkdir(s.path+".interrupted.lock", 0700)) // actual EISDIR, even as root
	if e := RecordInterruption(s.path, k.ID); e == nil {
		t.Fatal("unwritable marker falsely reported success")
	}
	present, e := markerPresent(s.path, k.ID)
	must(t, e)
	if present {
		t.Fatal("unexpected marker")
	}
}
