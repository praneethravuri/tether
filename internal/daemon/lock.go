package daemon

import (
	"fmt"
	"path/filepath"
	"sync"
	"syscall"
)

// heldLocks closes the same-process gap that advisory file locks deliberately
// do not cover. Production daemons are separate processes, but it also keeps
// Run's in-process contract deterministic for callers and tests.
var heldLocks sync.Map

// acquireLock holds an advisory exclusive lock on the socket directory. The
// operating system releases it if a daemon dies. Locking the directory avoids
// ever opening or mutating a marker pathname that may be a symlink.
func acquireLock(path string) (release func(), err error) {
	lockDir := filepath.Clean(filepath.Dir(path))
	if _, loaded := heldLocks.LoadOrStore(lockDir, struct{}{}); loaded {
		return nil, fmt.Errorf("tether: another daemon is already starting up or running in %s: %w", lockDir, ErrAlreadyRunning)
	}
	defer func() {
		if err != nil {
			heldLocks.Delete(lockDir)
		}
	}()

	fd, err := syscall.Open(lockDir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("tether: open socket directory %s: %w", lockDir, err)
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = syscall.Close(fd)
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, fmt.Errorf("tether: another daemon is already starting up or running in %s: %w", lockDir, ErrAlreadyRunning)
		}
		return nil, fmt.Errorf("tether: lock socket directory %s: %w", lockDir, err)
	}

	return func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = syscall.Close(fd)
		heldLocks.Delete(lockDir)
	}, nil
}
