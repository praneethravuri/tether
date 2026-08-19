package main

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/praneethravuri/tether/internal/protocol"
)

// shortSockDir is a temp dir short enough for a unix socket path -- unlike
// t.TempDir(), which embeds the (long) test name and can exceed the
// platform's ~104-byte sun_path limit.
func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tw")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestDialAutoStartsOnDialFailure proves dial() calls spawnDaemon and retries
// once when nothing answers the socket, without ever exec'ing a real process
// -- the stub here simulates a successful spawn by binding the socket itself.
func TestDialAutoStartsOnDialFailure(t *testing.T) {
	sock := filepath.Join(shortSockDir(t), "sock")

	called := false
	restoreSpawn(t, func(s string) error {
		called = true
		ln, err := net.Listen("unix", s)
		if err != nil {
			return err
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				_ = conn.Close()
			}
		}()
		return nil
	})

	conn, err := dial(sock, true)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()

	if !called {
		t.Fatal("dial did not call spawnDaemon when nothing was listening")
	}
}

func TestDialWithoutAutoStartFailsImmediately(t *testing.T) {
	sock := filepath.Join(shortSockDir(t), "sock")

	restoreSpawn(t, func(string) error {
		t.Fatal("spawnDaemon must not be called when autoStart is false")
		return nil
	})

	_, err := dial(sock, false)
	if err == nil {
		t.Fatal("dial succeeded with nothing listening and autoStart disabled")
	}
	requireContains(t, err.Error(), "no daemon running", "error")
	if got := exitCodeFor(err); got != exitNoDaemon {
		t.Fatalf("exit code = %d, want %d", got, exitNoDaemon)
	}
}

func TestDialReportsASpawnThatFails(t *testing.T) {
	sock := filepath.Join(shortSockDir(t), "sock")

	restoreSpawn(t, func(string) error {
		return errors.New("boom")
	})

	_, err := dial(sock, true)
	if err == nil {
		t.Fatal("dial succeeded even though spawnDaemon failed")
	}
	requireContains(t, err.Error(), "boom", "error")
	requireContains(t, err.Error(), "tether", "error")
	if got := exitCodeFor(err); got != exitNoDaemon {
		t.Fatalf("exit code = %d, want %d", got, exitNoDaemon)
	}
}

// restoreSpawn overrides spawnDaemon for the duration of one test.
func restoreSpawn(t *testing.T, fn func(string) error) {
	t.Helper()
	prev := spawnDaemon
	spawnDaemon = fn
	t.Cleanup(func() { spawnDaemon = prev })
}

// TestWaitReconnectsAfterATransportFailure proves waitUpTo does not give up
// the first time a connection drops mid-wait without answering (e.g. the
// daemon was killed while this call was parked) -- it retries rather than
// failing the whole wait, as long as the retry is itself a transport failure
// (not a daemon-side error) and time remains.
func TestWaitReconnectsAfterATransportFailure(t *testing.T) {
	sock := filepath.Join(shortSockDir(t), "sock")
	t.Setenv("TETHER_SOCK", sock)

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	var seen int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			n := atomic.AddInt32(&seen, 1)
			go handleOneWaitConn(conn, n)
		}
	}()

	res, err := waitUpTo("frontend", "storefront", "", 2*time.Second)
	if err != nil {
		t.Fatalf("waitUpTo did not reconnect past the dropped connection: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("res = %+v, want a genuine timeout from the connection that finally answered", res)
	}
	if atomic.LoadInt32(&seen) < 2 {
		t.Fatalf("only %d connection(s) were made; the drop never happened", seen)
	}
}

// handleOneWaitConn answers a MethodWait request normally, except the first
// connection, which is closed unanswered -- simulating a daemon killed
// mid-request.
func handleOneWaitConn(conn net.Conn, n int32) {
	defer func() { _ = conn.Close() }()

	var req protocol.Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	if n == 1 {
		return // drop it, unanswered
	}
	_ = json.NewEncoder(conn).Encode(protocol.OK(req.ID, protocol.WaitResult{Pending: 0, TimedOut: true}))
}
