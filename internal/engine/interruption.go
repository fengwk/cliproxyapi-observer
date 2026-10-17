package engine

import (
	"os"
	"path/filepath"
	"strings"
)

const interruptionReason = "accounting interruption: explicit acknowledgement required"
const interruptionIOReason = "accounting interruption storage unavailable"

func validKeyID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func interruptionPath(dataPath, keyID string) (string, error) {
	if !validKeyID(keyID) || dataPath == "" || strings.ContainsRune(dataPath, 0) || strings.HasSuffix(dataPath, string(os.PathSeparator)) {
		return "", ErrInvalid
	}
	base := filepath.Base(dataPath)
	if base == "." || base == ".." {
		return "", ErrInvalid
	}
	path, e := filepath.Abs(dataPath)
	if e != nil {
		return "", e
	}
	if st, e := os.Lstat(path); e == nil {
		if !st.Mode().IsRegular() {
			return "", ErrInvalid
		}
	} else if !os.IsNotExist(e) {
		return "", e
	}
	return filepath.Join(path+".interrupted", keyID), nil
}
func markerPresent(dataPath, id string) (bool, error) {
	path, e := interruptionPath(dataPath, id)
	if e != nil {
		return false, e
	}
	// Never follow a substituted marker directory. Any I/O error fails closed.
	st, e := os.Lstat(filepath.Dir(path))
	if os.IsNotExist(e) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return false, ErrInvalid
	}
	_, e = os.Lstat(path)
	if os.IsNotExist(e) {
		return false, nil
	}
	return e == nil, e
}
func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}

// RecordInterruption durably pauses a Key without needing an open bbolt handle.
// It stores only a constant marker, not a receipt or a recoverable price. Calls
// from independent native runtimes serialize with RecoverKey via flock.
// If storage cannot accept/sync this marker, the returned error MUST be exposed
// by the caller; no software can guarantee persistence on wholly unwritable
// storage. The running marker still protects an unclean Store session.
func RecordInterruption(dataPath, keyID string) error {
	path, e := interruptionPath(dataPath, keyID)
	if e != nil {
		return e
	}
	return withInterruptionLock(path, func() error {
		dir := filepath.Dir(path)
		if e := os.Mkdir(dir, 0700); e != nil && !os.IsExist(e) {
			return e
		}
		st, e := os.Lstat(dir)
		if e != nil {
			return e
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return ErrInvalid
		}
		if e := syncDir(filepath.Dir(dir)); e != nil {
			return e
		}
		// One fixed stage per DB bounds crash leftovers independently of Key count;
		// the shared lock serializes writers. Never unlink the lock file (which
		// would split the lock domain).
		temp := filepath.Join(dir, ".pending")
		f, e := openPrivateFile(temp)
		if e != nil {
			return e
		}
		defer os.Remove(temp)
		if e = f.Truncate(0); e == nil {
			_, e = f.WriteString("unknown accounting\n")
		}
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
		if e = os.Rename(temp, path); e != nil {
			return e
		}
		return syncDir(dir)
	})
}
func removeInterruption(path string) error {
	if e := os.Remove(path); e != nil && !os.IsNotExist(e) {
		return e
	}
	return syncDir(filepath.Dir(path))
}

// Called with Store.mu held; no DB writes here, including inside read txs.
func (s *Store) observeInterruption(id string) bool {
	if !validKeyID(id) {
		return false
	}
	present, e := markerPresent(s.path, id)
	if present || e != nil {
		s.unknown[id] = true
		s.lastError = interruptionReason
		if e != nil {
			s.lastError = interruptionIOReason
		}
	}
	return present || e != nil
}
