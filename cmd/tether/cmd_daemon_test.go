package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/praneethravuri/tether/internal/daemon"
)

func TestDaemonBanner(t *testing.T) {
	got := daemonBanner("/tmp/sock", "/tmp/tether.db")
	for _, want := range []string{
		"running the daemon in the foreground",
		"/tmp/sock",
		"/tmp/tether.db",
		"tether ls",
	} {
		requireContains(t, got, want, "banner")
	}
}

// TestDaemonRunErr_AlreadyRunningIsConflict is drift item 9: a double-start
// must exit 5, not the general 1 exitConflict = 5 was defined for exactly
// this and previously went unused for it.
func TestDaemonRunErr_AlreadyRunningIsConflict(t *testing.T) {
	err := daemonRunErr(errors.New("wrap: " + daemon.ErrAlreadyRunning.Error()))
	if exitCodeFor(err) != exitGeneral {
		t.Fatalf("sanity check: a plain string error should stay general, got %d", exitCodeFor(err))
	}

	wrapped := fmt.Errorf("socket already served: %w", daemon.ErrAlreadyRunning)
	if got := exitCodeFor(daemonRunErr(wrapped)); got != exitConflict {
		t.Fatalf("exit code = %d, want %d", got, exitConflict)
	}
}

func TestDaemonRunErr_PassesThroughOtherErrorsAndNil(t *testing.T) {
	sentinel := errors.New("boom")
	if got := daemonRunErr(sentinel); got != sentinel {
		t.Fatalf("daemonRunErr changed an unrelated error: %v", got)
	}
	if daemonRunErr(nil) != nil {
		t.Fatal("daemonRunErr(nil) != nil")
	}
}
