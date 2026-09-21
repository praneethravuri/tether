package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/praneethravuri/tether/internal/protocol"
	"github.com/praneethravuri/tether/internal/store"
)

func TestDaemonIsLive_NoSocketAtAll(t *testing.T) {
	dir := shortTempDir(t)
	if daemonIsLive(filepath.Join(dir, "does-not-exist")) {
		t.Fatal("daemonIsLive on a missing path = true, want false")
	}
}

func TestDaemonIsLive_StaleSocketFile(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "s")

	// A crashed daemon leaves a socket file behind that nothing is bound to.
	// It must not be mistaken for a running daemon, or the daemon could
	// never restart after a hard kill.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if daemonIsLive(path) {
		t.Fatal("daemonIsLive on a stale file = true, want false")
	}
}

func TestDaemonIsLive_BoundSocket(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "s")

	ln, err := protocol.Listen(path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	if !daemonIsLive(path) {
		t.Fatal("daemonIsLive on a bound socket = false, want true")
	}

	// Once the listener goes away the path is free again.
	_ = ln.Close()
	if daemonIsLive(path) {
		t.Fatal("daemonIsLive after close = true, want false")
	}
}

func TestStartupErrorMessageIsPlain(t *testing.T) {
	err := &startupError{"socket /tmp/sock is already served by a running daemon"}
	if err.Error() != "socket /tmp/sock is already served by a running daemon" {
		t.Fatalf("Error() = %q", err.Error())
	}
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatal("startupError does not unwrap to ErrAlreadyRunning")
	}
}

// TestRun_DoubleStartReturnsErrAlreadyRunning is drift item 9: Run must
// fail with an error identifiable as ErrAlreadyRunning when another daemon
// already holds the lock (this process's own pid, which is of course
// alive), so the CLI can map it to exitConflict instead of a bare general
// failure.
func TestRun_DoubleStartReturnsErrAlreadyRunning(t *testing.T) {
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "sock")

	release, err := acquireLock(sockPath)
	if err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	defer release()

	t.Setenv("TETHER_SOCK", sockPath)
	t.Setenv("TETHER_DB", filepath.Join(dir, "tether.db"))

	if err := Run(); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("Run() against an already-held lock = %v, want ErrAlreadyRunning", err)
	}
}

// TestEndToEndOverRealSocket wires the daemon up the way Run does -- open the
// store, bind the socket, serve, cancel -- and drives a full agent lifecycle
// across it. It is the smoke test that the wiring in Run is correct even
// though Run itself resolves paths from the environment.
func TestEndToEndOverRealSocket(t *testing.T) {
	dir := shortTempDir(t)

	st, err := store.Open(context.Background(), filepath.Join(dir, "tether.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	sock := filepath.Join(dir, "sock")
	ln, err := protocol.Listen(sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("socket mode = %o, want no group/other access", perm)
	}

	cfg := DefaultConfig()
	cfg.Logger = log.New(io.Discard, "", 0)
	srv := NewServer(st, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)

	call := func(id, method string, params any) protocol.Response {
		t.Helper()
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := enc.Encode(protocol.Request{ID: id, Method: method, Params: raw}); err != nil {
			t.Fatalf("write %s: %v", method, err)
		}
		var resp protocol.Response
		if err := dec.Decode(&resp); err != nil {
			t.Fatalf("read %s: %v", method, err)
		}
		if resp.Error != nil {
			t.Fatalf("%s: %v", method, resp.Error)
		}
		return resp
	}

	// No session on either register: none of the calls below claim one
	// either, and this test is about the wiring, not about authentication.
	call("1", protocol.MethodRegister, protocol.RegisterParams{Name: "alice", Workspace: "proj"})
	call("2", protocol.MethodRegister, protocol.RegisterParams{Name: "bob", Workspace: "proj"})
	call("3", protocol.MethodSend, protocol.SendParams{
		FromName: "alice", FromWorkspace: "proj",
		ToName: "bob", ToWorkspace: "proj",
		Kind: store.KindQuestion, Body: "ship it?",
	})

	var inbox protocol.InboxResult
	resp := call("4", protocol.MethodInbox, protocol.InboxParams{Name: "bob", Workspace: "proj"})
	if err := json.Unmarshal(resp.Result, &inbox); err != nil {
		t.Fatalf("decode inbox: %v", err)
	}
	if len(inbox.Messages) != 1 || inbox.Messages[0].Kind != store.KindQuestion {
		t.Fatalf("inbox = %+v", inbox.Messages)
	}

	// A reply joins the question's thread.
	resp = call("5", protocol.MethodSend, protocol.SendParams{
		FromName: "bob", FromWorkspace: "proj",
		ToName: "alice", ToWorkspace: "proj",
		Kind: store.KindAnswer, Body: "ship it", ReplyTo: inbox.Messages[0].ID,
	})
	var sent protocol.SendResult
	if err := json.Unmarshal(resp.Result, &sent); err != nil {
		t.Fatalf("decode send: %v", err)
	}
	resp = call("6", protocol.MethodInbox, protocol.InboxParams{Name: "alice", Workspace: "proj"})
	if err := json.Unmarshal(resp.Result, &inbox); err != nil {
		t.Fatalf("decode inbox: %v", err)
	}
	if len(inbox.Messages) != 1 {
		t.Fatalf("alice inbox = %d messages, want 1", len(inbox.Messages))
	}
	if inbox.Messages[0].ThreadID == inbox.Messages[0].ID {
		t.Fatal("reply started its own thread instead of joining the question's")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
}
