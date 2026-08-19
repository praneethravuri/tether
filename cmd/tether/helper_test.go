package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/praneethravuri/tether/internal/protocol"
)

// The CLI cannot import internal/daemon, so these tests stand a fake daemon up on
// a unix socket and point the CLI at it with TETHER_SOCK. That exercises the
// real dial/encode/decode path while letting a test say exactly what comes
// back, including responses no real daemon would ever send.

// TestMain disables auto-start for the whole package: without this, any
// "no daemon" test would exec the go test binary itself as if it were
// tether. A test that wants to exercise auto-start overrides spawnDaemon
// locally via restoreSpawn.
func TestMain(m *testing.M) {
	spawnDaemon = func(string) error {
		return errors.New("auto-start disabled in tests")
	}
	os.Exit(m.Run())
}

// recorded is one request the fake daemon received.
type recorded struct {
	Method string
	Params json.RawMessage
}

// handlerFunc builds the response for a request. It runs on the daemon's own
// goroutine, so it must not call t.Fatal; assert on requests() afterwards
// instead.
type handlerFunc func(req protocol.Request) protocol.Response

type fakeDaemon struct {
	path string
	ln   net.Listener
	done chan struct{}

	// raw, when non-nil, is written to the connection instead of an encoded
	// response, so a test can send garbage.
	raw []byte
	// hangUp closes the connection without answering.
	hangUp bool

	handler handlerFunc

	mu  sync.Mutex
	got []recorded
}

// newFakeDaemon starts a fake daemon, points TETHER_SOCK at it, and stops it
// when the test ends.
func newFakeDaemon(t *testing.T, h handlerFunc) *fakeDaemon {
	t.Helper()

	// os.MkdirTemp rather than t.TempDir: a unix socket path is limited to
	// about a hundred bytes and t.TempDir embeds the (long) test name.
	dir, err := os.MkdirTemp("", "tether-cli")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}

	path := filepath.Join(dir, "sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen on %s: %v", path, err)
	}

	d := &fakeDaemon{path: path, ln: ln, done: make(chan struct{}), handler: h}
	go d.serve()

	t.Cleanup(func() {
		_ = ln.Close()
		<-d.done
		_ = os.RemoveAll(dir)
	})

	t.Setenv("TETHER_SOCK", path)
	return d
}

// newRawDaemon starts a fake daemon that always answers with these exact
// bytes, which is how the malformed-response tests are written.
func newRawDaemon(t *testing.T, raw []byte) {
	t.Helper()
	d := newFakeDaemon(t, nil)
	d.raw = raw
}

// newSilentDaemon starts a fake daemon that accepts and then hangs up without
// answering.
func newSilentDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	d := newFakeDaemon(t, nil)
	d.hangUp = true
	return d
}

func (d *fakeDaemon) serve() {
	defer close(d.done)
	for {
		conn, err := d.ln.Accept()
		if err != nil {
			return
		}
		d.handle(conn)
	}
}

func (d *fakeDaemon) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	var req protocol.Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}

	d.mu.Lock()
	d.got = append(d.got, recorded{Method: req.Method, Params: req.Params})
	d.mu.Unlock()

	switch {
	case d.hangUp:
		return
	case d.raw != nil:
		_, _ = conn.Write(d.raw)
	default:
		resp := d.handler(req)
		if resp.ID == "" {
			resp.ID = req.ID
		}
		_ = json.NewEncoder(conn).Encode(resp)
	}
}

// requests returns a copy of everything the daemon has been asked so far.
func (d *fakeDaemon) requests() []recorded {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]recorded(nil), d.got...)
}

// countMethod returns how many requests received so far used method. Tests
// that count retries of one particular RPC (cmd_wait.go's re-issue loop) use
// this instead of len(requests()) so an unrelated implicit "register" call
// does not inflate the count they actually care about.

// only asserts exactly one request arrived, that it used the expected method,
// and returns it.
func (d *fakeDaemon) only(t *testing.T, method string) recorded {
	t.Helper()
	got := d.requests()
	if len(got) != 1 {
		t.Fatalf("daemon received %d requests, want exactly 1: %+v", len(got), got)
	}
	if got[0].Method != method {
		t.Fatalf("daemon received method %q, want %q", got[0].Method, method)
	}
	return got[0]
}

// registerThen asserts the daemon received exactly two requests: an
// implicit "register" call first (see client.go's ensureRegistered), then
// exactly one call to method, and returns the second. This is the shape
// every command that acts as an agent -- send, inbox, wait, and status's
// self path -- now produces, since each registers itself before its real
// request.
func (d *fakeDaemon) registerThen(t *testing.T, method string) recorded {
	t.Helper()
	got := d.requests()
	if len(got) != 2 {
		t.Fatalf("daemon received %d requests, want exactly 2 (register then %s): %+v",
			len(got), method, got)
	}
	if got[0].Method != protocol.MethodRegister {
		t.Fatalf("first request method = %q, want %q (implicit registration)",
			got[0].Method, protocol.MethodRegister)
	}
	if got[1].Method != method {
		t.Fatalf("second request method = %q, want %q", got[1].Method, method)
	}
	return got[1]
}

// okHandler answers every request with result.
func okHandler(result any) handlerFunc {
	return func(req protocol.Request) protocol.Response {
		return protocol.OK(req.ID, result)
	}
}

// errHandler answers every request with a daemon-side failure.
func errHandler(code int, msg string) handlerFunc {
	return func(req protocol.Request) protocol.Response {
		return protocol.Fail(req.ID, code, msg)
	}
}

// decodeParams unmarshals the params of a recorded request.
func decodeParams[T any](t *testing.T, r recorded) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(r.Params, &v); err != nil {
		t.Fatalf("decode %s params %s: %v", r.Method, r.Params, err)
	}
	return v
}

// testAsIdentity is what setIdentity configures; run/mustRun apply it as
// --as on any command that has the flag, so the ~100 existing call sites
// that predate per-session identity resolution don't all need --as added
// to their args now that $TETHER_NAME is gone.
var testAsIdentity string

// setIdentity makes name and workspace resolution deterministic, so tests do
// not depend on where the checkout lives or on the developer's environment.
func setIdentity(t *testing.T, name, workspace string) {
	t.Helper()
	prev := testAsIdentity
	testAsIdentity = name
	t.Cleanup(func() { testAsIdentity = prev })
	t.Setenv("TETHER_WORKSPACE", workspace)
}

// runOut is the outcome of executing one command.
type runOut struct {
	stdout string
	stderr string
	err    error
}

// exitCode is the process exit code this outcome would produce.
func (r runOut) exitCode() int { return exitCodeFor(r.err) }

// run executes cmd with args and captured streams. stdin is what the command
// reads from, which matters for `send --body-file -`. If setIdentity
// configured a name and cmd has an --as flag, it is applied here -- an
// explicit --as in args still wins, since Execute parses args afterward.
//
//nolint:unparam // stdin param kept for API symmetry with mustRun
func run(t *testing.T, cmd *cobra.Command, stdin string, args ...string) runOut {
	t.Helper()

	if testAsIdentity != "" {
		if f := cmd.Flags().Lookup("as"); f != nil {
			_ = f.Value.Set(testAsIdentity)
		}
	}

	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)

	err := cmd.Execute()
	return runOut{stdout: out.String(), stderr: errOut.String(), err: err}
}

// mustRun executes a command and fails the test if it returned an error.
func mustRun(t *testing.T, cmd *cobra.Command, _ string, args ...string) runOut {
	t.Helper()
	r := run(t, cmd, "", args...)
	if r.err != nil {
		t.Fatalf("command failed: %v\nstdout:\n%s", r.err, r.stdout)
	}
	return r
}

// unmarshalJSON decodes a command result, failing the test if it is not valid
// JSON for that result's shape.
func unmarshalJSON(t *testing.T, out string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(out), v); err != nil {
		t.Fatalf("command output is not valid %T: %v\n%s", v, err, out)
	}
}

// requireContains fails unless got contains want.
func requireContains(t *testing.T, got, want, what string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("%s does not contain %q:\n%s", what, want, got)
	}
}

// requireNotContains fails when got contains unwanted.
func requireNotContains(t *testing.T, got, unwanted, what string) {
	t.Helper()
	if strings.Contains(got, unwanted) {
		t.Fatalf("%s unexpectedly contains %q:\n%s", what, unwanted, got)
	}
}

// noDaemon points the CLI at a socket path nothing is listening on.
func noDaemon(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("", "tether-nodaemon")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("TETHER_SOCK", filepath.Join(dir, "sock"))
}
