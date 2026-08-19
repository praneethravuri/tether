// Package daemon runs the tether daemon: the socket listener, SQLite-backed
// message bus, and background sweep that cmd/tether serves in the foreground
// when invoked with no arguments.
package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/praneethravuri/tether/internal/protocol"
	"github.com/praneethravuri/tether/internal/store"
)

// ErrAlreadyRunning marks every "a daemon already holds this socket" failure
// (lock held or live), so a caller can map it to a conflict rather than a
// general error regardless of which of the two checks caught it.
var ErrAlreadyRunning = errors.New("a daemon is already running")

// Run resolves the socket and database paths, guards against a second
// daemon, and serves until interrupted or terminated.
func Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sockPath, err := protocol.SocketPath()
	if err != nil {
		return err
	}
	dbPath, err := protocol.DBPath()
	if err != nil {
		return err
	}

	// Closes a race daemonIsLive alone can't: Listen unlinks any existing
	// socket, so two racing daemons could both pass the liveness check below
	// before either binds.
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return err
	}
	release, err := acquireLock(sockPath)
	if err != nil {
		return err
	}
	defer release()

	// Second, independent check: catches a live daemon not holding this lock.
	if daemonIsLive(sockPath) {
		return &startupError{"socket " + sockPath + " is already served by a running daemon"}
	}

	logPath, err := protocol.LogPath()
	if err != nil {
		return err
	}
	logFile, err := openLog(logPath)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()
	logger := newJSONLogger(logFile)

	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	st.Logger = logger

	ln, err := protocol.Listen(sockPath)
	if err != nil {
		_ = st.Close()
		return err
	}

	logger.Printf("listening on %s", sockPath)
	logger.Printf("database %s", dbPath)

	cfg := DefaultConfig()
	cfg.Logger = logger
	cfg.LogPath = logPath
	serveErr := NewServer(st, cfg).Serve(ctx, ln)
	logger.Printf("stopped")

	// A handler still using the store when Serve gave up on it must not
	// race a close out from under it -- leave the store open; the process
	// exiting reclaims it regardless.
	if errors.Is(serveErr, ErrHandlersAbandoned) {
		logger.Printf("handlers were still in flight at shutdown; leaving the store open rather than closing it under them")
	} else if closeErr := st.Close(); closeErr != nil {
		logger.Printf("closing store: %v", closeErr)
	}
	return serveErr
}

// daemonIsLive reports whether something is currently accepting on path. A
// leftover socket file from a crashed daemon refuses connections and is
// treated as free.
func daemonIsLive(path string) bool {
	conn, err := net.DialTimeout("unix", path, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// startupError keeps Run's failure messages free of Go error decoration.
type startupError struct{ msg string }

func (e *startupError) Error() string { return e.msg }
func (e *startupError) Unwrap() error { return ErrAlreadyRunning }
