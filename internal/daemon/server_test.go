package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/praneethravuri/tether/internal/protocol"
	"github.com/praneethravuri/tether/internal/store"
)

// fakeClock is a mutex-guarded fake clock so tests can fast-forward past
// staleness thresholds without sleeping. It mirrors internal/store's own
// test clock (store_test.go's clock), which this package cannot import
// (it is unexported and lives in a _test.go file).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Now()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// -- harness ----------------------------------------------------------------

// testServer is a real Server on a real unix socket, driven over the wire.
// Nothing here imports cmd/tether: the daemon is exercised exactly as an
// arbitrary client would exercise it.
type testServer struct {
	t      *testing.T
	srv    *Server
	store  *store.Store
	sock   string
	cancel context.CancelFunc
	done   chan error
}

// shortTempDir returns a short-lived directory with a short path. t.TempDir()
// embeds the test name, and a unix socket path is capped at ~104 bytes on
// darwin, so long test names would break the bind.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "td")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func newTestServer(t *testing.T, tweak func(*Config)) *testServer {
	t.Helper()
	return newTestServerWithClock(t, tweak, nil)
}

// newTestServerWithClock is newTestServer, additionally setting the store's
// clock before Serve starts. sweepLoop now sweeps once at startup, so a test
// that fast-forwards time by assigning store.Now after construction would
// race with that first sweep; setting it here, before the background
// goroutine exists, avoids the race entirely.
func newTestServerWithClock(t *testing.T, tweak func(*Config), now func() time.Time) *testServer {
	t.Helper()
	return newTestServerFull(t, tweak, now, nil)
}

// newTestServerFull is newTestServerWithClock, additionally running seed
// against the store before Serve starts. A test that needs data to exist
// before the startup sweep can observe it (sweepLoop's first sweep runs the
// instant the background goroutine starts) has no other race-free way to
// seed it -- inserting after construction races the same way store.Now
// assignment used to.
func newTestServerFull(t *testing.T, tweak func(*Config), now func() time.Time, seed func(*store.Store)) *testServer {
	t.Helper()

	dir := shortTempDir(t)
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if now != nil {
		st.Now = now
	}
	if seed != nil {
		seed(st)
	}

	cfg := DefaultConfig()
	cfg.Logger = log.New(io.Discard, "", 0)
	cfg.SweepInterval = 50 * time.Millisecond // exercise the sweeper in every test
	if tweak != nil {
		tweak(&cfg)
	}

	sock := filepath.Join(dir, "s")
	ln, err := protocol.Listen(sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ts := &testServer{
		t:      t,
		srv:    NewServer(st, cfg),
		store:  st,
		sock:   sock,
		cancel: cancel,
		done:   make(chan error, 1),
	}
	go func() { ts.done <- ts.srv.Serve(ctx, ln) }()

	t.Cleanup(func() { ts.stop() })
	return ts
}

// stop shuts the server down and fails the test if it does not come back.
func (ts *testServer) stop() {
	ts.t.Helper()
	ts.cancel()
	select {
	case err := <-ts.done:
		if err != nil {
			ts.t.Errorf("Serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		ts.t.Error("Serve did not return after cancellation")
	}
	if err := ts.store.Close(); err != nil && !strings.Contains(err.Error(), "closed") {
		ts.t.Errorf("close store: %v", err)
	}
}

// client is a raw newline-delimited JSON client.
type client struct {
	t    *testing.T
	conn net.Conn
	enc  *json.Encoder
	dec  *json.Decoder
	seq  int
	// sessions maps a registered name to the session it was registered
	// with, so the send/inbox/wait helpers below can authenticate as
	// themselves without every call site repeating it.
	sessions map[string]string
}

func (ts *testServer) dial() *client {
	ts.t.Helper()
	conn, err := net.Dial("unix", ts.sock)
	if err != nil {
		ts.t.Fatalf("dial: %v", err)
	}
	c := &client{t: ts.t, conn: conn, enc: json.NewEncoder(conn), dec: json.NewDecoder(conn)}
	ts.t.Cleanup(c.close)
	return c
}

func (c *client) close() { _ = c.conn.Close() }

// call sends one request and returns the response.
func (c *client) call(method string, params any) protocol.Response {
	c.t.Helper()
	c.seq++
	id := fmt.Sprintf("r%d", c.seq)

	req := protocol.Request{ID: id, Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			c.t.Fatalf("marshal params: %v", err)
		}
		req.Params = raw
	}
	if err := c.enc.Encode(req); err != nil {
		c.t.Fatalf("write %s: %v", method, err)
	}

	var resp protocol.Response
	if err := c.dec.Decode(&resp); err != nil {
		c.t.Fatalf("read %s: %v", method, err)
	}
	if resp.ID != id {
		c.t.Fatalf("response id = %q, want %q", resp.ID, id)
	}
	return resp
}

// mustCall fails unless the call succeeded, and decodes the result.
func (c *client) mustCall(method string, params, out any) {
	c.t.Helper()
	resp := c.call(method, params)
	if resp.Error != nil {
		c.t.Fatalf("%s failed: %v", method, resp.Error)
	}
	if out != nil {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			c.t.Fatalf("decode %s result: %v", method, err)
		}
	}
}

// mustFail fails unless the call returned the given code.
func (c *client) mustFail(method string, params any, code int) *protocol.Error {
	c.t.Helper()
	resp := c.call(method, params)
	if resp.Error == nil {
		c.t.Fatalf("%s: expected error %d, got result %s", method, code, resp.Result)
	}
	if resp.Error.Code != code {
		c.t.Fatalf("%s: error code = %d (%s), want %d", method, resp.Error.Code, resp.Error.Message, code)
	}
	return resp.Error
}

func (c *client) register(name, ws, session string) protocol.RegisterResult {
	c.t.Helper()
	var out protocol.RegisterResult
	c.mustCall(protocol.MethodRegister, protocol.RegisterParams{
		Name: name, Workspace: ws, Harness: "test", SessionID: session, Cwd: "/tmp", PID: os.Getpid(),
	}, &out)
	if c.sessions == nil {
		c.sessions = map[string]string{}
	}
	c.sessions[out.Name] = session
	return out
}

func (c *client) send(body string) string {
	c.t.Helper()
	var out protocol.SendResult
	c.mustCall(protocol.MethodSend, protocol.SendParams{
		FromName: "alice", FromWorkspace: "proj", FromSession: c.sessions["alice"],
		ToName: "bob", ToWorkspace: "proj", Body: body,
	}, &out)
	return out.MessageID
}

// inbox drains by default now (replay=false means "drain", not "peek"). See
// inboxPeek for a non-destructive read.
func (c *client) inbox(name string, replay bool) []protocol.MessageView {
	c.t.Helper()
	var out protocol.InboxResult
	c.mustCall(protocol.MethodInbox, protocol.InboxParams{
		Name: name, Workspace: "proj", Session: c.sessions[name], Limit: 500, Replay: replay,
	}, &out)
	return out.Messages
}

func (c *client) inboxPeek(name string) []protocol.MessageView {
	c.t.Helper()
	var out protocol.InboxResult
	c.mustCall(protocol.MethodInbox, protocol.InboxParams{
		Name: name, Workspace: "proj", Session: c.sessions[name], Limit: 500, Peek: true,
	}, &out)
	return out.Messages
}

// -- registration -----------------------------------------------------------

func TestRegister(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	got := c.register("alice", "proj", "sess-1")
	want := protocol.RegisterResult{Address: "alice@proj", Name: "alice", Harness: "test", Created: true}
	if got != want {
		t.Fatalf("register result = %+v, want %+v", got, want)
	}

	// The same session re-registering is a supported no-op, not a conflict,
	// and is a refresh of the existing row rather than a fresh creation.
	got = c.register("alice", "proj", "sess-1")
	if got.Created {
		t.Fatalf("re-register reported Created=true, want false (the row already existed)")
	}
}

func TestRegister_DuplicateNameFromAnotherSessionConflicts(t *testing.T) {
	ts := newTestServer(t, nil)

	a := ts.dial()
	a.register("alice", "proj", "sess-1")

	b := ts.dial()
	_ = b.mustFail(protocol.MethodRegister, protocol.RegisterParams{
		Name: "alice", Workspace: "proj", Harness: "test", SessionID: "sess-2",
	}, protocol.CodeConflict)
}

func TestRegister_MissingFields(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	// An empty Name is valid now -- it means "resolve or mint one" -- so only
	// workspace and a malformed explicit name are still rejected.
	_ = c.mustFail(protocol.MethodRegister, protocol.RegisterParams{Name: "alice"}, protocol.CodeBadRequest)
	_ = c.mustFail(protocol.MethodRegister, protocol.RegisterParams{Name: "a@b", Workspace: "proj"}, protocol.CodeBadRequest)
	// Absent params block at all.
	_ = c.mustFail(protocol.MethodRegister, nil, protocol.CodeBadRequest)
}

func TestRegister_NameTooLongIsRejected(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	longName := strings.Repeat("a", 33)
	_ = c.mustFail(protocol.MethodRegister, protocol.RegisterParams{
		Name: longName, Workspace: "proj", SessionID: "sess-1",
	}, protocol.CodeBadRequest)
}

// TestRegister_EmptyNameMints proves an empty Name with no existing session
// registration mints "<harness>-<hex4>" instead of failing.
func TestRegister_EmptyNameMints(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	got := c.register("", "proj", "sess-1")
	if got.Name == "" {
		t.Fatal("empty Name register did not mint a name")
	}
	if !strings.HasPrefix(got.Name, "test-") {
		t.Fatalf("minted name = %q, want a test-<hex4> shape", got.Name)
	}
	if !got.Created {
		t.Fatal("first-ever registration for this session reported Created=false")
	}
	if got.Address != got.Name+"@proj" {
		t.Fatalf("address = %q, want %s@proj", got.Address, got.Name)
	}
}

// TestRegister_EmptyNameResolvesExistingSession proves a session that
// already registered resolves back to its own name on a later empty-Name
// register, rather than minting a second identity.
func TestRegister_EmptyNameResolvesExistingSession(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	c.register("alice", "proj", "sess-1")

	got := c.register("", "proj", "sess-1")
	if got.Name != "alice" {
		t.Fatalf("empty-Name register resolved to %q, want alice", got.Name)
	}
	if got.Created {
		t.Fatal("resolving an existing session reported Created=true")
	}
	if got.Renamed {
		t.Fatal("resolving to the same name should not report Renamed")
	}
}

// TestRegister_ExplicitNameRenamesAndMovesMail is the headline rename
// scenario: registering a different explicit name from a session that
// already holds one renames in place and carries pending mail along, rather
// than leaving an orphaned second identity.
func TestRegister_ExplicitNameRenamesAndMovesMail(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	c.register("alice", "proj", "sess-1")
	c.register("bob", "proj", "sess-2")
	c.mustCall(protocol.MethodSend, protocol.SendParams{
		FromName: "bob", FromWorkspace: "proj", FromSession: "sess-2",
		ToName: "alice", ToWorkspace: "proj", Body: "hi",
	}, nil)

	got := c.register("frontend", "proj", "sess-1")
	if !got.Renamed {
		t.Fatal("registering a different name from an existing session did not report Renamed")
	}
	if got.Created {
		t.Fatal("a rename reported Created=true")
	}
	if got.Name != "frontend" {
		t.Fatalf("Name = %q, want frontend", got.Name)
	}

	_ = c.mustFail(protocol.MethodLs,
		protocol.LsParams{Name: "alice", Workspace: "proj"}, protocol.CodeNotFound)

	var st protocol.LsResult
	c.mustCall(protocol.MethodLs, protocol.LsParams{Name: "frontend", Workspace: "proj"}, &st)
	if st.Agents[0].Pending != 1 {
		t.Fatalf("frontend pending = %d, want 1 (mail must follow the rename)", st.Agents[0].Pending)
	}
}

// TestRegister_RenameOntoALiveNameConflictsWithSuggestion proves a rename
// that collides with a different live session's name 409s with a free
// alternative suggested in the message.
func TestRegister_RenameOntoALiveNameConflictsWithSuggestion(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	c.register("alice", "proj", "sess-1")
	c.register("bob", "proj", "sess-2")

	e := c.mustFail(protocol.MethodRegister, protocol.RegisterParams{
		Name: "bob", Workspace: "proj", SessionID: "sess-1",
	}, protocol.CodeConflict)
	if !strings.Contains(e.Message, "bob-2") {
		t.Fatalf("conflict message %q does not suggest a free alternative", e.Message)
	}
}

// TestRegister_DuplicateNameConflictSuggestsAlternative is the same
// suggestion behaviour for a plain (non-rename) conflict.
func TestRegister_DuplicateNameConflictSuggestsAlternative(t *testing.T) {
	ts := newTestServer(t, nil)

	a := ts.dial()
	a.register("alice", "proj", "sess-1")

	b := ts.dial()
	e := b.mustFail(protocol.MethodRegister, protocol.RegisterParams{
		Name: "alice", Workspace: "proj", Harness: "test", SessionID: "sess-2",
	}, protocol.CodeConflict)
	if !strings.Contains(e.Message, "alice-2") {
		t.Fatalf("conflict message %q does not suggest a free alternative", e.Message)
	}
}

// implausiblePID is large enough that no supported platform will ever hand
// it to a real process (see internal/proc/proc_test.go's identical
// reasoning), so registering with it is a safe, non-flaky way to exercise
// the "session pid is not alive" rejection.
const implausiblePID = 1 << 30

// TestRegister_DeadPidRejected is P1: handleRegister must reject a session
// pid that is not alive, rather than accepting whatever a client claims.
func TestRegister_DeadPidRejected(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	e := c.mustFail(protocol.MethodRegister, protocol.RegisterParams{
		Name: "alice", Workspace: "proj", Harness: "test", SessionID: "s1", PID: implausiblePID,
	}, protocol.CodeBadRequest)
	if !strings.Contains(e.Message, "1073741824") {
		t.Fatalf("error message %q does not name the dead pid", e.Message)
	}
}

// TestRegister_LivePidRecordsPidStart proves the daemon computes pid_start
// itself (never trusting a client-supplied value) for a genuinely live pid,
// by round-tripping through status and confirming the agent is reachable --
// the store-level round trip of PIDStart itself is covered directly in
// internal/store/store_test.go.
func TestRegister_LivePidRecordsPidStart(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	c.register("alice", "proj", "s1") // client.register uses os.Getpid(), which is alive

	var st protocol.LsResult
	c.mustCall(protocol.MethodLs, protocol.LsParams{Name: "alice", Workspace: "proj"}, &st)
	if st.Agents[0].PID != os.Getpid() {
		t.Fatalf("stored pid = %d, want %d", st.Agents[0].PID, os.Getpid())
	}
}

// TestRegister_DeadIncumbentReclaimedImmediately is the headline fix: a name
// held by a session whose pid is now provably dead must be reclaimable right
// away, not after StaleAfter elapses.
//
// The incumbent cannot be seeded through the register RPC itself: handleRegister
// rejects a dead pid on every registration, first-time or not (see
// TestRegister_DeadPidRejected), so a session's pid can only go from alive to
// dead the way it would in production -- the process that registered it
// exits sometime later while the row stays in the store. This test fast-forwards
// past that by writing the incumbent row directly through the store, exactly
// as if it had registered validly with a pid that has since exited.
func TestRegister_DeadIncumbentReclaimedImmediately(t *testing.T) {
	// A long StaleAfter proves the reclaim is NOT happening via the
	// time-based path.
	ts := newTestServer(t, func(c *Config) { c.StaleAfter = time.Hour })
	c := ts.dial()

	if err := ts.store.Register(context.Background(), store.Agent{
		Workspace: "proj", Name: "alice", Harness: "test", SessionID: "sess-dead",
		PID: implausiblePID, Cwd: "/tmp",
	}, time.Now()); err != nil {
		t.Fatalf("seed incumbent: %v", err)
	}

	got := c.register("alice", "proj", "sess-new")
	if got.Created {
		t.Fatalf("reclaim reported Created=true, want false (the row already existed)")
	}

	var st protocol.LsResult
	c.mustCall(protocol.MethodLs, protocol.LsParams{Name: "alice", Workspace: "proj"}, &st)
	if st.Agents[0].PID != os.Getpid() {
		t.Fatalf("reclaim did not update the stored pid: %+v", st.Agents[0])
	}
}

// TestRegister_LiveIncumbentStillConflicts is the mirror of the reclaim
// test: a name held by a session whose pid IS alive must still 409 for a
// different session, exactly like today.
func TestRegister_LiveIncumbentStillConflicts(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.StaleAfter = time.Hour })

	a := ts.dial()
	a.register("alice", "proj", "sess-1") // peer pid is this live test process

	b := ts.dial()
	_ = b.mustFail(protocol.MethodRegister, protocol.RegisterParams{
		Name: "alice", Workspace: "proj", Harness: "test", SessionID: "sess-2", PID: os.Getpid(),
	}, protocol.CodeConflict)
}

// TestRegister_PIDInADifferentSessionIsRejected is 6.3: a claimed session pid
// that is alive but demonstrably not this connection and not in its session
// (a process detached into its own session, the shape PID 1 or a stolen
// victim pid would also take) must be rejected rather than trusted outright.
func TestRegister_PIDInADifferentSessionIsRejected(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	detached := exec.Command("sleep", "5")
	detached.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := detached.Start(); err != nil {
		t.Skipf("cannot start a detached process in this environment: %v", err)
	}
	t.Cleanup(func() { _ = detached.Process.Kill(); _ = detached.Wait() })

	e := c.mustFail(protocol.MethodRegister, protocol.RegisterParams{
		Name: "alice", Workspace: "proj", Harness: "test", SessionID: "s1", PID: detached.Process.Pid,
	}, protocol.CodeBadRequest)
	if !strings.Contains(e.Message, fmt.Sprint(detached.Process.Pid)) {
		t.Fatalf("error message %q does not name the rejected pid", e.Message)
	}
}

// TestRegister_PIDSharingTheConnectionsSessionIsAccepted is the positive
// mirror: a pid that is not the connection's own peer pid, but was launched
// by the same shell (an ordinary child process, no setsid), must still be
// accepted -- ancestry is not required, only a shared session.
func TestRegister_PIDSharingTheConnectionsSessionIsAccepted(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	sibling := exec.Command("sleep", "5")
	if err := sibling.Start(); err != nil {
		t.Skipf("cannot start a child process in this environment: %v", err)
	}
	t.Cleanup(func() { _ = sibling.Process.Kill(); _ = sibling.Wait() })

	c.mustCall(protocol.MethodRegister, protocol.RegisterParams{
		Name: "bob", Workspace: "proj", Harness: "test", SessionID: "s2", PID: sibling.Process.Pid,
	}, &protocol.RegisterResult{})
}

// -- authentication -----------------------------------------------------------

// TestSend_SessionMismatchIsConflict is the other P1 fix: a sender claiming
// a session that does not match the live session actually holding
// from_name must be rejected, closing the forgery this phase exists to fix.
func TestSend_SessionMismatchIsConflict(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()
	c.register("alice", "proj", "sess-1")
	c.register("bob", "proj", "sess-2")

	_ = c.mustFail(protocol.MethodSend, protocol.SendParams{
		FromName: "alice", FromWorkspace: "proj", FromSession: "sess-intruder",
		ToName: "bob", ToWorkspace: "proj", Body: "forged",
	}, protocol.CodeConflict)

	// The genuine session still works.
	c.mustCall(protocol.MethodSend, protocol.SendParams{
		FromName: "alice", FromWorkspace: "proj", FromSession: "sess-1",
		ToName: "bob", ToWorkspace: "proj", Body: "genuine",
	}, &protocol.SendResult{})
}

// TestSend_EmptyStoredSessionStillAllowed covers the legacy/no-session row: a
// sender whose stored session_id is empty (an unrecognised harness that could
// not synthesise one) can still act as itself as long as it likewise claims
// no session -- but an empty stored session is not a wildcard: claiming any
// session against it must still be rejected, which is the exact bypass this
// phase closes (an omitted session used to authenticate as anyone).
func TestSend_EmptyStoredSessionStillAllowed(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()
	c.mustCall(protocol.MethodRegister, protocol.RegisterParams{
		Name: "alice", Workspace: "proj", Harness: "test", // no SessionID
	}, &protocol.RegisterResult{})
	c.register("bob", "proj", "sess-2")

	c.mustCall(protocol.MethodSend, protocol.SendParams{
		FromName: "alice", FromWorkspace: "proj", // no FromSession, matching the empty stored one
		ToName: "bob", ToWorkspace: "proj", Body: "hi",
	}, &protocol.SendResult{})

	_ = c.mustFail(protocol.MethodSend, protocol.SendParams{
		FromName: "alice", FromWorkspace: "proj", FromSession: "whatever-i-like",
		ToName: "bob", ToWorkspace: "proj", Body: "forged",
	}, protocol.CodeConflict)
}

// TestSend_ReportsRecipientState proves a unicast send's response tells the
// sender what the recipient looks like right now, so a message landing in a
// queue nobody's watching isn't silent. blocked outranks a fresh heartbeat,
// per computeState's own priority order.
func TestSend_ReportsRecipientState(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()
	c.register("alice", "proj", "s1")
	c.register("bob", "proj", "s2")

	var res protocol.SendResult
	c.mustCall(protocol.MethodSend, protocol.SendParams{
		FromName: "alice", FromWorkspace: "proj", FromSession: c.sessions["alice"],
		ToName: "bob", ToWorkspace: "proj", Body: "hi",
	}, &res)
	if res.RecipientState != "working" {
		t.Fatalf("RecipientState = %q, want %q (bob just registered)", res.RecipientState, "working")
	}

	// Drain bob's inbox first -- wait returns immediately (never parking)
	// when mail is already pending, which would make the next block a
	// false pass rather than an actual test of the blocked state.
	c.mustCall(protocol.MethodInbox,
		protocol.InboxParams{Name: "bob", Workspace: "proj", Session: c.sessions["bob"], Limit: 10}, &protocol.InboxResult{})

	// bob is parked in wait: blocked outranks the heartbeat-derived state.
	bob := ts.dial()
	waitDone := make(chan protocol.WaitResult, 1)
	go func() {
		var out protocol.WaitResult
		bob.mustCall(protocol.MethodWait, protocol.WaitParams{
			Name: "bob", Workspace: "proj", Session: c.sessions["bob"], TimeoutMS: 5000,
		}, &out)
		waitDone <- out
	}()

	// Give the waiter time to actually park before sending.
	deadline := time.Now().Add(5 * time.Second)
	for ts.srv.waiters.Count("bob@proj") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("waiter never parked")
		}
		time.Sleep(10 * time.Millisecond)
	}

	c.mustCall(protocol.MethodSend, protocol.SendParams{
		FromName: "alice", FromWorkspace: "proj", FromSession: c.sessions["alice"],
		ToName: "bob", ToWorkspace: "proj", Body: "hi again",
	}, &res)
	if res.RecipientState != "blocked" {
		t.Fatalf("RecipientState = %q, want %q (bob is parked in wait)", res.RecipientState, "blocked")
	}
	<-waitDone
}

// TestSend_BroadcastLeavesRecipientStateEmpty proves a broadcast, which has
// no single recipient, doesn't try to report one.
func TestSend_BroadcastLeavesRecipientStateEmpty(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()
	c.register("alice", "proj", "s1")
	c.register("bob", "proj", "s2")

	var res protocol.SendResult
	c.mustCall(protocol.MethodSend, protocol.SendParams{
		FromName: "alice", FromWorkspace: "proj", FromSession: c.sessions["alice"],
		ToName: "*", ToWorkspace: "proj", Body: "hi all",
	}, &res)
	if res.RecipientState != "" {
		t.Fatalf("broadcast RecipientState = %q, want empty", res.RecipientState)
	}
}

// -- activity keeps an agent alive --------------------------------------------

// TestActivityKeepsAgentAliveAndNameUnstealable is the core regression test
// for this phase's P1 bug: before implicit registration, nothing ever
// refreshed last_seen after the initial register, so every agent silently
// vanished from `who` after StaleAfter and its name became stealable even
// though the agent was still alive and working. With a very short
// StaleAfter and a fake clock advanced well past it, an agent that has made
// ANY authenticated call since registering (here, a second inbox call) must
// still appear in `who` AND must not be stealable by a different session.
func TestActivityKeepsAgentAliveAndNameUnstealable(t *testing.T) {
	const staleAfter = 50 * time.Millisecond
	clk := newFakeClock()

	ts := newTestServerWithClock(t, func(c *Config) { c.StaleAfter = staleAfter }, clk.Now)

	c := ts.dial()
	c.register("alice", "proj", "sess-1")

	clk.advance(staleAfter * 10)

	// Any activity since register -- an authenticated inbox call -- must
	// refresh last_seen.
	c.mustCall(protocol.MethodInbox, protocol.InboxParams{
		Name: "alice", Workspace: "proj", Session: "sess-1",
	}, &protocol.InboxResult{})

	var who protocol.LsResult
	c.mustCall(protocol.MethodLs, protocol.LsParams{Workspace: "proj"}, &who)
	found := false
	for _, a := range who.Agents {
		if a.Name == "alice" {
			found = true
		}
	}
	if !found {
		t.Fatalf("alice missing from who after activity within StaleAfter of the read: %+v", who.Agents)
	}

	// And the name must not be stealable by a different session: alice's
	// pid (this live test process) is also still alive, so this is doubly
	// protected, but the point under test is specifically that activity
	// alone -- not just a live pid -- keeps last_seen fresh enough that the
	// time-based staleness guard in store.Register would refuse a steal too.
	b := ts.dial()
	_ = b.mustFail(protocol.MethodRegister, protocol.RegisterParams{
		Name: "alice", Workspace: "proj", Harness: "test", SessionID: "sess-intruder",
	}, protocol.CodeConflict)
}

// TestUnregisterAndHeartbeatAreGone confirms the retired RPCs are not merely
// absent from the CLI surface but actively rejected by the wire: a raw
// "unregister" or "heartbeat" request now falls through dispatch's default
// case like any other unknown method.
func TestUnregisterAndHeartbeatAreGone(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()
	c.register("alice", "proj", "s1")

	for _, method := range []string{"unregister", "heartbeat"} {
		e := c.mustFail(method, nil, protocol.CodeBadRequest)
		if !strings.Contains(e.Message, method) {
			t.Fatalf("%s error message %q does not name the method", method, e.Message)
		}
	}

	// The connection survives both.
	c.register("bob", "proj", "s2")
}

// -- mail -------------------------------------------------------------------

// TestInboxDrainsPeekReplay is Phase 5's headline server-side contract:
// inbox drains by default (read + ack in one call), --peek leaves mail in
// place, and a message only shows up in --replay's history once a real
// drain has acked it.
func TestInboxDrainsPeekReplay(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()
	c.register("alice", "proj", "s1")
	c.register("bob", "proj", "s2")

	id := c.send("hello bob")

	// A peek does not clear: two peeks return the same message, unacked.
	first := c.inboxPeek("bob")
	if len(first) != 1 {
		t.Fatalf("peek #1 = %d messages, want 1", len(first))
	}
	m := first[0]
	if m.ID != id || m.From != "alice@proj" || m.To != "bob@proj" || m.Body != "hello bob" {
		t.Fatalf("unexpected message %+v", m)
	}
	if m.Kind != store.KindNote {
		t.Fatalf("kind = %q, want %q", m.Kind, store.KindNote)
	}
	if m.ThreadID == "" {
		t.Fatal("thread_id is empty")
	}
	if _, err := time.Parse(time.RFC3339, m.CreatedAt); err != nil {
		t.Fatalf("created_at %q is not RFC3339: %v", m.CreatedAt, err)
	}
	if m.DeliveredAt == nil {
		t.Fatal("delivered_at not stamped on first read")
	}
	if m.AckedAt != nil {
		t.Fatalf("acked_at set after a peek: %v", *m.AckedAt)
	}

	second := c.inboxPeek("bob")
	if len(second) != 1 || second[0].ID != id {
		t.Fatalf("peek #2 = %+v, want the same one message again", second)
	}

	// The default drains: the message comes back once, marked acked, and is
	// gone on every subsequent read (peek or drain).
	drained := c.inbox("bob", false)
	if len(drained) != 1 || drained[0].ID != id {
		t.Fatalf("drain = %+v, want the one message", drained)
	}
	if drained[0].AckedAt == nil {
		t.Fatal("drained message has no acked_at")
	}

	if again := c.inbox("bob", false); len(again) != 0 {
		t.Fatalf("second drain = %+v, want empty", again)
	}
	if again := c.inboxPeek("bob"); len(again) != 0 {
		t.Fatalf("peek after drain = %+v, want empty", again)
	}

	replay := c.inbox("bob", true)
	if len(replay) != 1 || replay[0].ID != id {
		t.Fatalf("replay = %+v, want the drained message", replay)
	}
	if replay[0].AckedAt == nil {
		t.Fatal("replayed message has no acked_at")
	}
}

func TestSend_UnknownRecipient(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()
	c.register("alice", "proj", "s1")

	_ = c.mustFail(protocol.MethodSend, protocol.SendParams{
		FromName: "alice", FromWorkspace: "proj", FromSession: c.sessions["alice"],
		ToName: "ghost", ToWorkspace: "proj", Body: "anyone there",
	}, protocol.CodeNotFound)
}

// TestSend_UnknownRecipientTypoSuggestsTheCloseMatch is the did-you-mean
// half of the 404 path: a recipient name close to a real, registered one
// gets a suggestion appended to the error message. The wire code stays 404
// -- this only enriches the message, it does not invent a new error kind.
func TestSend_UnknownRecipientTypoSuggestsTheCloseMatch(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()
	c.register("alice", "proj", "s1")
	c.register("backend", "proj", "s2")

	e := c.mustFail(protocol.MethodSend, protocol.SendParams{
		FromName: "alice", FromWorkspace: "proj", FromSession: c.sessions["alice"],
		ToName: "back", ToWorkspace: "proj", Body: "hi",
	}, protocol.CodeNotFound)
	if !strings.Contains(e.Message, "did you mean") || !strings.Contains(e.Message, "backend@proj") {
		t.Fatalf("error message = %q, want a did-you-mean pointing at backend@proj", e.Message)
	}
}

// TestSend_UnknownRecipientWithNoCloseMatchSuggestsNothing is the other
// half: when nothing registered is close enough, the message stays plain.
func TestSend_UnknownRecipientWithNoCloseMatchSuggestsNothing(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()
	c.register("alice", "proj", "s1")
	c.register("backend", "proj", "s2")

	e := c.mustFail(protocol.MethodSend, protocol.SendParams{
		FromName: "alice", FromWorkspace: "proj", FromSession: c.sessions["alice"],
		ToName: "zzzzzzzzzz", ToWorkspace: "proj", Body: "hi",
	}, protocol.CodeNotFound)
	if strings.Contains(e.Message, "did you mean") {
		t.Fatalf("error message = %q, want no did-you-mean suggestion", e.Message)
	}
}

// -- broadcast ('*' / 'all') --------------------------------------------------

// TestSend_BroadcastReachesEveryoneButTheSender is the headline broadcast
// contract: with a sender plus two other agents registered, a send to '*'
// lands in exactly the two others' inboxes and never the sender's own --
// the entire loop-prevention mechanism for this phase is that exclusion.
func TestSend_BroadcastReachesEveryoneButTheSender(t *testing.T) {
	for _, marker := range []string{"*", "all"} {
		t.Run(marker, func(t *testing.T) {
			ts := newTestServer(t, nil)
			c := ts.dial()
			c.register("alice", "proj", "s1")
			c.register("bob", "proj", "s2")
			c.register("carol", "proj", "s3")

			var out protocol.SendResult
			c.mustCall(protocol.MethodSend, protocol.SendParams{
				FromName: "alice", FromWorkspace: "proj", FromSession: c.sessions["alice"],
				ToName: marker, ToWorkspace: "proj", Body: "heads up",
			}, &out)

			if out.Delivered != 2 {
				t.Fatalf("delivered = %d, want 2", out.Delivered)
			}
			got := map[string]bool{}
			for _, a := range out.Recipients {
				got[a] = true
			}
			if len(got) != 2 || !got["bob@proj"] || !got["carol@proj"] {
				t.Fatalf("recipients = %v, want exactly bob@proj and carol@proj", out.Recipients)
			}

			if msgs := c.inboxPeek("bob"); len(msgs) != 1 || msgs[0].Body != "heads up" {
				t.Fatalf("bob's inbox = %+v, want the broadcast message", msgs)
			}
			if msgs := c.inboxPeek("carol"); len(msgs) != 1 || msgs[0].Body != "heads up" {
				t.Fatalf("carol's inbox = %+v, want the broadcast message", msgs)
			}
			if msgs := c.inboxPeek("alice"); len(msgs) != 0 {
				t.Fatalf("alice's (the sender's) inbox = %+v, want empty", msgs)
			}
		})
	}
}

// TestSend_BroadcastWithOnlyTheSenderIsSuccessWithZeroDelivered covers "a
// lone agent broadcasting to an empty room": that must succeed, not fail,
// and report Delivered: 0.
func TestSend_BroadcastWithOnlyTheSenderIsSuccessWithZeroDelivered(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()
	c.register("alice", "proj", "s1")

	var out protocol.SendResult
	c.mustCall(protocol.MethodSend, protocol.SendParams{
		FromName: "alice", FromWorkspace: "proj", FromSession: c.sessions["alice"],
		ToName: "*", ToWorkspace: "proj", Body: "anyone?",
	}, &out)

	if out.Delivered != 0 {
		t.Fatalf("delivered = %d, want 0", out.Delivered)
	}
	if len(out.Recipients) != 0 {
		t.Fatalf("recipients = %v, want none", out.Recipients)
	}
}

// TestSend_BroadcastEntirelyFailingIsAnError is drift item 6.12: a
// broadcast with a non-empty eligible recipient set that every delivery
// fails against must not read as success with Delivered: 0, which is
// indistinguishable from an empty workspace. An oversized body fails every
// recipient uniformly and deterministically, without needing to mock the
// store.
func TestSend_BroadcastEntirelyFailingIsAnError(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()
	c.register("alice", "proj", "s1")
	c.register("bob", "proj", "s2")
	c.register("carol", "proj", "s3")

	huge := strings.Repeat("x", (64<<10)+1)
	e := c.mustFail(protocol.MethodSend, protocol.SendParams{
		FromName: "alice", FromWorkspace: "proj", FromSession: c.sessions["alice"],
		ToName: "*", ToWorkspace: "proj", Body: huge,
	}, protocol.CodeBadRequest)
	if !strings.Contains(e.Message, "2") {
		t.Fatalf("error message %q does not name the failure count", e.Message)
	}

	// Neither recipient actually received anything.
	if msgs := c.inboxPeek("bob"); len(msgs) != 0 {
		t.Fatalf("bob's inbox = %+v, want empty", msgs)
	}
	if msgs := c.inboxPeek("carol"); len(msgs) != 0 {
		t.Fatalf("carol's inbox = %+v, want empty", msgs)
	}
}

// TestRegister_ReservedBroadcastNamesAreRejected is the trust-boundary half
// of broadcast addressing: no agent may ever register as "*" or "all",
// since those are reserved recipient markers, not real names.
func TestRegister_ReservedBroadcastNamesAreRejected(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	for _, name := range []string{"*", "all"} {
		_ = c.mustFail(protocol.MethodRegister, protocol.RegisterParams{
			Name: name, Workspace: "proj", Harness: "test", PID: os.Getpid(),
		}, protocol.CodeBadRequest)
	}
}

func TestSend_Validation(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()
	c.register("alice", "proj", "s1")
	c.register("bob", "proj", "s2")

	base := func() protocol.SendParams {
		return protocol.SendParams{
			FromName: "alice", FromWorkspace: "proj", FromSession: c.sessions["alice"],
			ToName: "bob", ToWorkspace: "proj", Body: "hi",
		}
	}

	empty := base()
	empty.Body = "   "
	_ = c.mustFail(protocol.MethodSend, empty, protocol.CodeBadRequest)

	badKind := base()
	badKind.Kind = "shout"
	_ = c.mustFail(protocol.MethodSend, badKind, protocol.CodeBadRequest)

	noFrom := base()
	noFrom.FromName = ""
	_ = c.mustFail(protocol.MethodSend, noFrom, protocol.CodeBadRequest)

	huge := base()
	huge.Body = strings.Repeat("x", (64<<10)+1)
	_ = c.mustFail(protocol.MethodSend, huge, protocol.CodeTooLarge)

	// A bad --reply-to is a malformed request, not "nobody was there" --
	// it must not share a recipient-missing's exit code (drift item 4).
	badReply := base()
	badReply.ReplyTo = "nosuchmessage"
	_ = c.mustFail(protocol.MethodSend, badReply, protocol.CodeBadRequest)

	// The daemon is still healthy after all of that.
	c.send("still working")
}

// TestInbox_LimitIsClampedOrRejected covers both halves of the P1 fix:
// a non-positive limit quietly becomes the default of 50 (never "no
// limit"), and anything past maxInboxRequestLimit is now a hard 400 naming both
// numbers instead of a silent clamp down to the max.
func TestInbox_LimitIsClampedOrRejected(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()
	c.register("alice", "proj", "s1")
	c.register("bob", "proj", "s2")

	// More than store.DefaultInboxLimit so the cap is actually provable, not just
	// "returned everything there was".
	const sent = store.DefaultInboxLimit + 10
	for i := 0; i < sent; i++ {
		c.send(fmt.Sprintf("msg %d", i))
	}

	var out protocol.InboxResult
	peek := func(limit int) int {
		t.Helper()
		c.mustCall(protocol.MethodInbox,
			protocol.InboxParams{Name: "bob", Workspace: "proj", Session: c.sessions["bob"], Limit: limit, Peek: true}, &out)
		return len(out.Messages)
	}

	// A negative limit falls back to the default rather than reaching SQL.
	if got := peek(-5); got != store.DefaultInboxLimit {
		t.Fatalf("inbox with negative limit = %d messages, want %d", got, store.DefaultInboxLimit)
	}
	// limit 0 means the same thing: the honest default, not "no limit".
	if got := peek(0); got != store.DefaultInboxLimit {
		t.Fatalf("inbox with limit 0 = %d messages, want %d", got, store.DefaultInboxLimit)
	}

	// Anything past maxInboxRequestLimit is rejected outright, not silently
	// truncated down to it.
	e := c.mustFail(protocol.MethodInbox,
		protocol.InboxParams{Name: "bob", Workspace: "proj", Session: c.sessions["bob"], Limit: maxInboxRequestLimit + 1, Peek: true},
		protocol.CodeBadRequest)
	if !strings.Contains(e.Message, fmt.Sprint(maxInboxRequestLimit+1)) || !strings.Contains(e.Message, fmt.Sprint(maxInboxRequestLimit)) {
		t.Fatalf("error message %q does not name both the limit and the max", e.Message)
	}

	if got, err := clampLimit(0); err != nil || got != store.DefaultInboxLimit {
		t.Fatalf("clampLimit(0) = (%d, %v), want (%d, nil)", got, err, store.DefaultInboxLimit)
	}
	if _, err := clampLimit(maxInboxRequestLimit + 1); err == nil {
		t.Fatal("clampLimit(maxInboxRequestLimit+1) succeeded, want an error")
	}
}

// TestAckMethodIsGone confirms the retired "ack" RPC is rejected exactly
// like any other unknown method now that draining is the only way to
// acknowledge mail (see TestUnregisterAndHeartbeatAreGone for the same
// pattern applied to the other retired RPCs).
func TestAckMethodIsGone(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()
	c.register("bob", "proj", "s1")

	e := c.mustFail("ack", nil, protocol.CodeBadRequest)
	if !strings.Contains(e.Message, "ack") {
		t.Fatalf("ack error message %q does not name the method", e.Message)
	}
}

// -- presence ---------------------------------------------------------------

// TestLs_ShowsAgentPastStaleAfter is drift item 5.2: ls must not hide an
// agent once it goes stale -- gone is only reachable if the row is still
// listed at all.
func TestLs_ShowsAgentPastStaleAfter(t *testing.T) {
	const staleAfter = 50 * time.Millisecond
	clk := newFakeClock()
	ts := newTestServerWithClock(t, func(c *Config) { c.StaleAfter = staleAfter }, clk.Now)

	c := ts.dial()
	c.register("alice", "proj", "s1")
	clk.advance(staleAfter * 10)

	var who protocol.LsResult
	c.mustCall(protocol.MethodLs, protocol.LsParams{Workspace: "proj"}, &who)
	found := false
	for _, a := range who.Agents {
		if a.Name == "alice" {
			found = true
		}
	}
	if !found {
		t.Fatalf("alice missing from ls once stale: %+v", who.Agents)
	}
}

// TestBroadcast_ReachesAgentIdlePastStaleAfter is drift item 5.3: an idle
// registered agent is still "everyone else in the workspace" for broadcast,
// not silently skipped and counted absent.
func TestBroadcast_ReachesAgentIdlePastStaleAfter(t *testing.T) {
	const staleAfter = 50 * time.Millisecond
	clk := newFakeClock()
	ts := newTestServerWithClock(t, func(c *Config) { c.StaleAfter = staleAfter }, clk.Now)

	c := ts.dial()
	c.register("alice", "proj", "s1")
	c.register("bob", "proj", "s2")
	clk.advance(staleAfter * 10)

	var out protocol.SendResult
	c.mustCall(protocol.MethodSend, protocol.SendParams{
		FromName: "alice", FromWorkspace: "proj", FromSession: "s1",
		ToName: "*", ToWorkspace: "proj", Body: "still here?",
	}, &out)

	if out.Delivered != 1 || len(out.Recipients) != 1 || out.Recipients[0] != "bob@proj" {
		t.Fatalf("broadcast result = %+v, want delivery to the idle bob@proj too", out)
	}
}

// TestReplay_WindowExpiresFromAckedTime is drift item 5.6: --replay's window
// is measured from when a message was acked (drained), and is now actually
// enforced instead of being emergent from the purge sweep's own schedule.
func TestReplay_WindowExpiresFromAckedTime(t *testing.T) {
	const window = 50 * time.Millisecond
	clk := newFakeClock()
	ts := newTestServerWithClock(t, func(c *Config) { c.RetainMessages = window }, clk.Now)

	c := ts.dial()
	c.register("alice", "proj", "s1")
	c.register("bob", "proj", "s2")
	c.mustCall(protocol.MethodSend, protocol.SendParams{
		FromName: "alice", FromWorkspace: "proj", FromSession: "s1",
		ToName: "bob", ToWorkspace: "proj", Body: "hi",
	}, &protocol.SendResult{})

	if drained := c.inbox("bob", false); len(drained) != 1 {
		t.Fatalf("drain = %+v, want 1 message", drained)
	}

	if replay := c.inbox("bob", true); len(replay) != 1 {
		t.Fatalf("replay within the window = %+v, want the drained message", replay)
	}

	clk.advance(window * 2)
	if replay := c.inbox("bob", true); len(replay) != 0 {
		t.Fatalf("replay past the window = %+v, want empty", replay)
	}
}

func TestWhoStatus(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()
	c.register("alice", "proj", "s1")
	c.register("bob", "other", "s2")

	var who protocol.LsResult
	c.mustCall(protocol.MethodLs, protocol.LsParams{Workspace: "proj"}, &who)
	if len(who.Agents) != 1 || who.Agents[0].Address != "alice@proj" {
		t.Fatalf("who(proj) = %+v, want just alice@proj", who.Agents)
	}

	c.mustCall(protocol.MethodLs, protocol.LsParams{}, &who)
	if len(who.Agents) != 2 {
		t.Fatalf("who(all workspaces) = %d agents, want 2", len(who.Agents))
	}

	var st protocol.LsResult
	c.mustCall(protocol.MethodLs, protocol.LsParams{Name: "alice", Workspace: "proj"}, &st)
	if st.Agents[0].Address != "alice@proj" {
		t.Fatalf("status agent = %+v", st.Agents[0])
	}
	if _, err := time.Parse(time.RFC3339, st.Agents[0].LastSeen); err != nil {
		t.Fatalf("last_seen %q is not RFC3339: %v", st.Agents[0].LastSeen, err)
	}
	_ = c.mustFail(protocol.MethodLs, protocol.LsParams{Name: "ghost", Workspace: "proj"}, protocol.CodeNotFound)
}

// TestWho_BlockedStateClearsOnRelease proves blocked is read from the live
// wait registry, not a durable log: an agent shows blocked only while it is
// actually parked in `wait`, and stops the instant that call returns
// (Waiters.Release runs).
func TestWho_BlockedStateClearsOnRelease(t *testing.T) {
	ts := newTestServer(t, nil)
	setup := ts.dial()
	setup.register("alice", "proj", "s1")
	setup.register("bob", "proj", "s2")

	waiter := ts.dial()
	done := make(chan protocol.WaitResult, 1)
	go func() {
		var out protocol.WaitResult
		waiter.mustCall(protocol.MethodWait, protocol.WaitParams{
			Name: "bob", Workspace: "proj", Session: setup.sessions["bob"], TimeoutMS: 30000,
		}, &out)
		done <- out
	}()

	// Give the waiter time to actually park before checking `who`.
	deadline := time.Now().Add(5 * time.Second)
	for ts.srv.waiters.Count("bob@proj") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("waiter never parked")
		}
		time.Sleep(10 * time.Millisecond)
	}

	var who protocol.LsResult
	setup.mustCall(protocol.MethodLs, protocol.LsParams{Workspace: "proj"}, &who)
	bobState := ""
	for _, a := range who.Agents {
		if a.Name == "bob" {
			bobState = a.State
		}
	}
	if bobState != "blocked" {
		t.Fatalf("bob's state while parked = %q, want blocked (%+v)", bobState, who.Agents)
	}

	setup.send("wake up") // alice -> bob, releases the parked wait

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not wake after send")
	}

	setup.mustCall(protocol.MethodLs, protocol.LsParams{Workspace: "proj"}, &who)
	for _, a := range who.Agents {
		if a.Name == "bob" && a.State == "blocked" {
			t.Fatalf("bob still reads as blocked after the wait returned: %+v", who.Agents)
		}
	}
}

// -- protocol version (6.5) --------------------------------------------------

// TestRequest_MissingOrWrongVersionIsRejected proves a request that omits or
// misstates V never reaches a handler at all -- the version check runs
// before dispatch, so a version-skewed peer gets a loud, distinct error
// instead of Params silently decoding with whatever fields it recognises.
func TestDecodeParams_UnknownFieldIsRejected(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()
	c.register("bob", "proj", "s1")

	raw := json.RawMessage(`{"name":"bob","workspace":"proj","session":"s1","peek":true,"from_the_future":42}`)
	req := protocol.Request{ID: "u", Method: protocol.MethodInbox, Params: raw}
	if err := c.enc.Encode(req); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var resp protocol.Response
	if err := c.dec.Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != protocol.CodeBadRequest {
		t.Fatalf("response = %+v, want a %d", resp, protocol.CodeBadRequest)
	}
	if !strings.Contains(resp.Error.Message, "from_the_future") {
		t.Fatalf("error message %q does not name the unrecognised field", resp.Error.Message)
	}

	// Rejected before any side effect: nothing was drained.
	if got := c.inboxPeek("bob"); len(got) != 0 {
		t.Fatalf("inboxPeek after a rejected request = %+v, want empty (nothing sent yet)", got)
	}
}

func TestUnknownMethod(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	e := c.mustFail("teleport", nil, protocol.CodeBadRequest)
	if !strings.Contains(e.Message, "teleport") {
		t.Fatalf("error message %q does not name the method", e.Message)
	}
	// The connection survives an unknown method.
	c.register("alice", "proj", "s1")
}

func TestMalformedJSONClosesConnectionButNotDaemon(t *testing.T) {
	ts := newTestServer(t, nil)

	bad := ts.dial()
	if _, err := bad.conn.Write([]byte("{\"id\": \"1\", this is not json}\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	var resp protocol.Response
	if err := bad.dec.Decode(&resp); err != nil {
		t.Fatalf("expected an error response before close, got %v", err)
	}
	if resp.Error == nil || resp.Error.Code != protocol.CodeBadRequest {
		t.Fatalf("response = %+v, want a 400", resp)
	}

	// The connection is then closed cleanly: EOF, not a reset mid-stream.
	if err := bad.dec.Decode(&resp); err != io.EOF && !strings.Contains(fmt.Sprint(err), "closed") {
		t.Fatalf("second decode = %v, want EOF", err)
	}

	// A brand new connection still works.
	good := ts.dial()
	good.register("alice", "proj", "s1")
}

func TestOversizedRequestIsRejected(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.MaxRequestBytes = 4 << 10 })

	c := ts.dial()
	// Comfortably over the cap but well under the socket buffer, so the write
	// completes even though the daemon stops reading.
	body := strings.Repeat("x", 16<<10)
	req := protocol.Request{ID: "big", Method: protocol.MethodSend}
	req.Params, _ = json.Marshal(protocol.SendParams{
		FromName: "a", FromWorkspace: "proj", ToName: "b", ToWorkspace: "proj", Body: body,
	})
	if err := c.enc.Encode(req); err != nil {
		t.Fatalf("write: %v", err)
	}

	var resp protocol.Response
	if err := c.dec.Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != protocol.CodeTooLarge {
		t.Fatalf("response = %+v, want a 413", resp)
	}

	// The daemon survived and serves new clients.
	good := ts.dial()
	good.register("alice", "proj", "s1")
}

func TestConnectionIsReusable(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	c.register("alice", "proj", "s1")
	c.register("bob", "proj", "s2")
	id := c.send("three requests, one dial")

	msgs := c.inbox("bob", false)
	if len(msgs) != 1 || msgs[0].ID != id {
		t.Fatalf("inbox = %+v, want the one message", msgs)
	}
	if c.seq < 3 {
		t.Fatalf("only %d requests were made on the connection", c.seq)
	}
}

func TestConcurrentClients(t *testing.T) {
	ts := newTestServer(t, nil)

	const (
		clients      = 20
		perClient    = 5
		totalWanted  = clients * perClient
		workspace    = "proj"
		sinkName     = "sink"
		sinkSessions = "sink-session"
	)

	setup := ts.dial()
	setup.register(sinkName, workspace, sinkSessions)

	var wg sync.WaitGroup
	errs := make(chan error, clients)

	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			conn, err := net.Dial("unix", ts.sock)
			if err != nil {
				errs <- fmt.Errorf("client %d dial: %w", i, err)
				return
			}
			defer func() { _ = conn.Close() }()

			enc := json.NewEncoder(conn)
			dec := json.NewDecoder(conn)
			call := func(method string, params any) error {
				raw, err := json.Marshal(params)
				if err != nil {
					return err
				}
				if err := enc.Encode(protocol.Request{ID: "x", Method: method, Params: raw}); err != nil {
					return err
				}
				var resp protocol.Response
				if err := dec.Decode(&resp); err != nil {
					return err
				}
				if resp.Error != nil {
					return fmt.Errorf("%s: %w", method, resp.Error)
				}
				return nil
			}

			name := fmt.Sprintf("agent-%02d", i)
			if err := call(protocol.MethodRegister, protocol.RegisterParams{
				Name: name, Workspace: workspace, Harness: "test", SessionID: name,
			}); err != nil {
				errs <- fmt.Errorf("client %d: %w", i, err)
				return
			}

			for j := 0; j < perClient; j++ {
				if err := call(protocol.MethodSend, protocol.SendParams{
					FromName: name, FromWorkspace: workspace, FromSession: name,
					ToName: sinkName, ToWorkspace: workspace,
					Body: fmt.Sprintf("%s/%d", name, j),
				}); err != nil {
					errs <- fmt.Errorf("client %d msg %d: %w", i, j, err)
					return
				}
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}

	msgs := setup.inbox(sinkName, false)
	if len(msgs) != totalWanted {
		t.Fatalf("sink received %d messages, want %d", len(msgs), totalWanted)
	}
	seen := make(map[string]bool, len(msgs))
	for _, m := range msgs {
		if seen[m.Body] {
			t.Errorf("duplicate message body %q", m.Body)
		}
		seen[m.Body] = true
	}
	for i := 0; i < clients; i++ {
		for j := 0; j < perClient; j++ {
			want := fmt.Sprintf("agent-%02d/%d", i, j)
			if !seen[want] {
				t.Errorf("lost message %q", want)
			}
		}
	}

	var who protocol.LsResult
	setup.mustCall(protocol.MethodLs, protocol.LsParams{Workspace: workspace}, &who)
	if len(who.Agents) != clients+1 {
		t.Fatalf("who = %d agents, want %d", len(who.Agents), clients+1)
	}
}

// -- wait -------------------------------------------------------------------

func TestWait_ReturnsImmediatelyWhenMailExists(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()
	c.register("alice", "proj", "s1")
	c.register("bob", "proj", "s2")
	c.send("already here")

	start := time.Now()
	var out protocol.WaitResult
	c.mustCall(protocol.MethodWait,
		protocol.WaitParams{Name: "bob", Workspace: "proj", Session: c.sessions["bob"], TimeoutMS: 30000}, &out)
	elapsed := time.Since(start)

	if out.TimedOut {
		t.Fatal("wait timed out even though mail was already pending")
	}
	if out.Pending != 1 {
		t.Fatalf("pending = %d, want 1", out.Pending)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("wait took %v with mail already pending", elapsed)
	}
}

func TestWait_WakesOnSendFromAnotherConnection(t *testing.T) {
	ts := newTestServer(t, nil)

	setup := ts.dial()
	setup.register("alice", "proj", "s1")
	setup.register("bob", "proj", "s2")

	waiter := ts.dial()
	type result struct {
		res     protocol.WaitResult
		elapsed time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		var out protocol.WaitResult
		waiter.mustCall(protocol.MethodWait, protocol.WaitParams{
			Name: "bob", Workspace: "proj", Session: setup.sessions["bob"], TimeoutMS: 30000,
		}, &out)
		done <- result{out, time.Since(start)}
	}()

	// Give the waiter time to actually park in the select rather than take the
	// pending-count shortcut, so the wake path is what is under test.
	time.Sleep(150 * time.Millisecond)
	setup.send("wake up")

	select {
	case got := <-done:
		if got.res.TimedOut {
			t.Fatal("wait reported a timeout after a send")
		}
		if got.res.Pending != 1 {
			t.Fatalf("pending = %d, want 1", got.res.Pending)
		}
		if got.elapsed > 5*time.Second {
			t.Fatalf("wait took %v, expected to wake promptly", got.elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("wait did not wake within 10s of the send")
	}
}

func TestWait_TimesOutCleanly(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()
	c.register("bob", "proj", "s1")

	start := time.Now()
	var out protocol.WaitResult
	c.mustCall(protocol.MethodWait,
		protocol.WaitParams{Name: "bob", Workspace: "proj", Session: c.sessions["bob"], TimeoutMS: 150}, &out)
	elapsed := time.Since(start)

	if !out.TimedOut {
		t.Fatalf("wait result = %+v, want timed_out", out)
	}
	if out.Pending != 0 {
		t.Fatalf("pending = %d, want 0", out.Pending)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("wait returned after %v, want at least the requested 150ms", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("wait overshot its timeout: %v", elapsed)
	}

	// The waiter released its slot, so nothing is left behind.
	if n := ts.srv.waiters.Len(); n != 0 {
		t.Fatalf("waiters map holds %d entries after a timeout, want 0", n)
	}

	// The connection is reusable straight after a timeout.
	c.mustCall(protocol.MethodWait,
		protocol.WaitParams{Name: "bob", Workspace: "proj", Session: c.sessions["bob"], TimeoutMS: 50}, &out)
	if !out.TimedOut {
		t.Fatal("second wait did not time out")
	}
}

func TestWait_TimeoutIsCapped(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.MaxWait = 120 * time.Millisecond })
	c := ts.dial()
	c.register("bob", "proj", "s1")

	start := time.Now()
	var out protocol.WaitResult
	// Ask for an hour; the daemon must impose its own ceiling.
	c.mustCall(protocol.MethodWait,
		protocol.WaitParams{Name: "bob", Workspace: "proj", Session: c.sessions["bob"], TimeoutMS: 3600000}, &out)

	if !out.TimedOut {
		t.Fatalf("wait result = %+v, want timed_out", out)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("wait honoured the client's timeout (%v) instead of the cap", elapsed)
	}
	// H2: a timeout produced by the server's own ceiling, not the caller's
	// requested duration, must be flagged as such so the CLI can tell it
	// apart from a genuine "nothing arrived within what you asked for".
	if !out.Capped {
		t.Fatalf("wait result = %+v, want capped=true since MaxWait was the binding limit", out)
	}
}

// TestWait_NotCappedWhenTheRequestFitsUnderTheCeiling is the mirror of
// TestWait_TimeoutIsCapped: when the caller's own requested timeout is what
// actually elapses (it is under the server's ceiling), Capped must be false,
// or the CLI's re-issue loop (cmd_wait.go's waitUpTo) would spin forever
// re-asking for a timeout that will never arrive any sooner.
func TestWait_NotCappedWhenTheRequestFitsUnderTheCeiling(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.MaxWait = time.Minute })
	c := ts.dial()
	c.register("bob", "proj", "s1")

	var out protocol.WaitResult
	c.mustCall(protocol.MethodWait,
		protocol.WaitParams{Name: "bob", Workspace: "proj", Session: c.sessions["bob"], TimeoutMS: 100}, &out)

	if !out.TimedOut {
		t.Fatalf("wait result = %+v, want timed_out", out)
	}
	if out.Capped {
		t.Fatalf("wait result = %+v, want capped=false: the request's own timeout is what elapsed", out)
	}
}

func TestWait_ConcurrentWaitersAllWake(t *testing.T) {
	ts := newTestServer(t, nil)

	setup := ts.dial()
	setup.register("alice", "proj", "s1")
	setup.register("bob", "proj", "s2")

	const waiters = 10
	done := make(chan protocol.WaitResult, waiters)
	for i := 0; i < waiters; i++ {
		c := ts.dial()
		go func() {
			var out protocol.WaitResult
			c.mustCall(protocol.MethodWait, protocol.WaitParams{
				Name: "bob", Workspace: "proj", Session: setup.sessions["bob"], TimeoutMS: 30000,
			}, &out)
			done <- out
		}()
	}

	time.Sleep(200 * time.Millisecond)
	setup.send("broadcast")

	for i := 0; i < waiters; i++ {
		select {
		case out := <-done:
			if out.TimedOut {
				t.Fatalf("waiter %d timed out", i)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d waiters woke", i, waiters)
		}
	}
	if n := ts.srv.waiters.Len(); n != 0 {
		t.Fatalf("waiters map holds %d entries, want 0", n)
	}
}

// TestHandleWait_PanicStillReleasesTheWaiter proves the defer ordering
// handleWait relies on: `ch := s.waiters.Wait(addr)` is followed immediately
// by `defer s.waiters.Release(addr)`, specifically so that anything panicking
// later in the same call -- including a downstream store failure nobody
// anticipated -- still gives the slot back rather than leaking it forever, a
// leak that would matter because dispatch's own recover (a level up) turns
// the panic into an ordinary 500 and the daemon keeps running.
//
// A nil store is what actually panics here: NewServer accepts a nil *Store
// (see TestServe_NilListener), and store.PendingCount dereferences a nil
// receiver's connection pool field, which is a real, unremarkable way for a
// handler to panic in production -- not a contrived one.
func TestHandleWait_PanicStillReleasesTheWaiter(t *testing.T) {
	srv := NewServer(nil, Config{Logger: log.New(io.Discard, "", 0)})

	req := protocol.Request{ID: "1", Method: protocol.MethodWait}
	req.Params, _ = json.Marshal(protocol.WaitParams{Name: "bob", Workspace: "proj", TimeoutMS: 1000})

	resp := srv.dispatch(context.Background(), req, 0)
	if resp.Error == nil || resp.Error.Code != protocol.CodeInternal {
		t.Fatalf("dispatch response = %+v, want a 500 (the panic recovered into an internal error)", resp)
	}
	if n := srv.waiters.Len(); n != 0 {
		t.Fatalf("waiters map holds %d entries after a panic in handleWait, want 0", n)
	}

	// The server is still usable: a fresh wait on the same address blocks
	// normally rather than tripping over a leftover entry.
	req2 := protocol.Request{ID: "2", Method: protocol.MethodLs}
	req2.Params, _ = json.Marshal(protocol.LsParams{Workspace: "proj"})
	resp2 := srv.dispatch(context.Background(), req2, 0)
	if resp2.Error == nil || resp2.Error.Code != protocol.CodeInternal {
		t.Fatalf("who against a nil store = %+v, want a 500 too (sanity: the server is still dispatching)", resp2)
	}
}

// -- shutdown ---------------------------------------------------------------

// TestAwaitConns_ReportsAbandonedHandlers is 6.9: awaitConns must tell its
// caller when it gave up on a still-running handler, so Run knows not to
// close the store out from under it.
func TestAwaitConns_ReportsAbandonedHandlers(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.ShutdownTimeout = 20 * time.Millisecond })

	ts.srv.wg.Add(1)
	stuck := make(chan struct{})
	go func() {
		defer ts.srv.wg.Done()
		<-stuck // simulates a handler that outlives the shutdown budget
	}()
	t.Cleanup(func() { close(stuck) })

	if clean := ts.srv.awaitConns(); clean {
		t.Fatal("awaitConns() = true, want false: a handler was still in flight")
	}
}

// TestAwaitConns_ReportsCleanShutdown is the control case.
func TestAwaitConns_ReportsCleanShutdown(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.ShutdownTimeout = time.Second })

	ts.srv.wg.Add(1)
	go ts.srv.wg.Done()

	if clean := ts.srv.awaitConns(); !clean {
		t.Fatal("awaitConns() = false, want true: nothing was left running")
	}
}

// TestIdleConnectionIsClosedAfterIdleTimeout is 6.8: a connection that never
// sends a request must be closed rather than pinning a goroutine forever.
func TestIdleConnectionIsClosedAfterIdleTimeout(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.IdleTimeout = 50 * time.Millisecond })
	c := ts.dial()

	// Say nothing. The server must close the connection on its own.
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	_, err := c.conn.Read(buf)
	if err == nil {
		t.Fatal("read succeeded on an idle connection the server should have closed")
	}
}

// TestConnectionsAreBoundedByMaxConns is 6.8's other half: acceptLoop blocks
// past MaxConns rather than spawning an unbounded goroutine per accept. Two
// connections hold their slots parked in wait; a third connection's request
// must go unanswered until one of the first two releases its slot -- a raw
// dial alone would succeed regardless (the kernel backlog accepts a
// connection independent of this process ever calling Accept), so the test
// asserts on the request being served, not on the dial.
func TestConnectionsAreBoundedByMaxConns(t *testing.T) {
	ts := newTestServer(t, func(c *Config) { c.MaxConns = 2 })

	parked := make([]*client, 2)
	for i := range 2 {
		parked[i] = ts.dial()
		name := fmt.Sprintf("waiter%d", i)
		parked[i].register(name, "proj", fmt.Sprintf("s%d", i))
		go func() {
			parked[i].call(protocol.MethodWait, protocol.WaitParams{Name: name, Workspace: "proj", TimeoutMS: 30000})
		}()
	}

	deadline := time.Now().Add(5 * time.Second)
	for len(ts.srv.connSlots) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("both parked connections never occupied their slots")
		}
		time.Sleep(10 * time.Millisecond)
	}

	third := ts.dial()
	regDone := make(chan protocol.Response, 1)
	go func() {
		regDone <- third.call(protocol.MethodRegister, protocol.RegisterParams{Name: "late", Workspace: "proj"})
	}()

	select {
	case <-regDone:
		t.Fatal("third connection was served while both MaxConns slots were full")
	case <-time.After(300 * time.Millisecond):
		// expected: acceptLoop hasn't even called Accept on it yet
	}

	// Free a slot the direct way: close one of the parked connections. A
	// completed wait alone would not free it -- the connection stays open
	// and handleConn keeps its slot until the connection itself closes.
	parked[0].close()

	select {
	case resp := <-regDone:
		if resp.Error != nil {
			t.Fatalf("third connection's register failed once a slot freed: %v", resp.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("third connection was never served after a slot freed")
	}
}

func TestGracefulShutdownUnblocksEverything(t *testing.T) {
	dir := shortTempDir(t)
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	cfg := DefaultConfig()
	cfg.Logger = log.New(io.Discard, "", 0)
	cfg.SweepInterval = 20 * time.Millisecond
	cfg.ShutdownTimeout = 2 * time.Second

	sock := filepath.Join(dir, "s")
	ln, err := protocol.Listen(sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := NewServer(st, cfg)
	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	var serveErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		serveErr = srv.Serve(ctx, ln)
	}()

	// One idle connection and one parked in a long wait: both must be released
	// by shutdown, not by their own timeouts.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	waitConn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = waitConn.Close() }()

	enc := json.NewEncoder(waitConn)
	dec := json.NewDecoder(waitConn)
	reg, _ := json.Marshal(protocol.RegisterParams{Name: "bob", Workspace: "proj"})
	if err := enc.Encode(protocol.Request{ID: "1", Method: protocol.MethodRegister, Params: reg}); err != nil {
		t.Fatalf("register: %v", err)
	}
	var resp protocol.Response
	if err := dec.Decode(&resp); err != nil || resp.Error != nil {
		t.Fatalf("register: %v %+v", err, resp.Error)
	}

	waitParams, _ := json.Marshal(protocol.WaitParams{Name: "bob", Workspace: "proj", TimeoutMS: 300000})
	if err := enc.Encode(protocol.Request{ID: "2", Method: protocol.MethodWait, Params: waitParams}); err != nil {
		t.Fatalf("wait: %v", err)
	}
	time.Sleep(150 * time.Millisecond) // let it park

	start := time.Now()
	cancel()

	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
	if serveErr != nil {
		t.Fatalf("Serve returned %v", serveErr)
	}
	if elapsed := time.Since(start); elapsed > cfg.ShutdownTimeout+2*time.Second {
		t.Fatalf("shutdown took %v", elapsed)
	}

	// The parked wait was released rather than left hanging: either it answered
	// on the way out or the connection was closed under it. Both are clean.
	_ = waitConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := dec.Decode(&resp); err == nil && resp.Error != nil {
		t.Fatalf("parked wait failed: %v", resp.Error)
	}

	// The socket no longer accepts.
	if c, err := net.DialTimeout("unix", sock, 500*time.Millisecond); err == nil {
		_ = c.Close()
		t.Fatal("listener still accepting after shutdown")
	}
}

func TestServe_NilListener(t *testing.T) {
	srv := NewServer(nil, Config{Logger: log.New(io.Discard, "", 0)})
	if err := srv.Serve(context.Background(), nil); err == nil {
		t.Fatal("Serve(nil listener) = nil, want an error")
	}
}

// -- unit-level helpers -----------------------------------------------------

func TestConfigDefaults(t *testing.T) {
	c := Config{}.withDefaults()
	if c.StaleAfter <= 0 || c.DeadAfter <= 0 || c.SweepInterval <= 0 ||
		c.ShutdownTimeout <= 0 || c.RequestTimeout <= 0 || c.MaxRequestBytes <= 0 ||
		c.DefaultWait <= 0 || c.MaxWait <= 0 || c.Logger == nil {
		t.Fatalf("zero Config did not fill in: %+v", c)
	}
	// A caller-supplied default longer than the cap is pulled back to the cap.
	c = Config{DefaultWait: time.Hour, MaxWait: time.Minute}.withDefaults()
	if c.DefaultWait != time.Minute {
		t.Fatalf("DefaultWait = %v, want %v", c.DefaultWait, time.Minute)
	}
}

func TestClipBoundsAndSanitises(t *testing.T) {
	long := strings.Repeat("a", maxClientMsgLen*2)
	if got := clip(long); len(got) != maxClientMsgLen+3 {
		t.Fatalf("clip length = %d, want %d", len(got), maxClientMsgLen+3)
	}
	if got := clip("bad\x1b[31mescape\n"); strings.ContainsAny(got, "\x1b\n") {
		t.Fatalf("clip left control characters in %q", got)
	}
}

func TestPublicMessageHidesStorePrefix(t *testing.T) {
	if got := publicMessage(store.ErrNoSuchAgent); strings.HasPrefix(got, "store:") {
		t.Fatalf("publicMessage = %q, still carries the package prefix", got)
	}
}

func TestFailHidesInternalDetail(t *testing.T) {
	srv := NewServer(nil, Config{Logger: log.New(io.Discard, "", 0)})
	resp := srv.fail("1", fmt.Errorf("sqlite: disk image is malformed at /home/user/secret.db"), "inbox")
	if resp.Error == nil || resp.Error.Code != protocol.CodeInternal {
		t.Fatalf("resp = %+v, want a 500", resp)
	}
	if strings.Contains(resp.Error.Message, "secret.db") || strings.Contains(resp.Error.Message, "sqlite") {
		t.Fatalf("500 leaked internal detail: %q", resp.Error.Message)
	}
	if !strings.Contains(resp.Error.Message, "ref ") {
		t.Fatalf("500 has no log reference: %q", resp.Error.Message)
	}
}

func TestLimitedReaderIsPerRequest(t *testing.T) {
	lr := &limitedReader{r: strings.NewReader(strings.Repeat("x", 100))}
	lr.reset(10)

	buf := make([]byte, 64)
	n, err := lr.Read(buf)
	if err != nil || n != 10 {
		t.Fatalf("first read = (%d, %v), want (10, nil)", n, err)
	}
	if _, err := lr.Read(buf); err != errRequestTooLarge {
		t.Fatalf("read past budget = %v, want errRequestTooLarge", err)
	}

	// Resetting restores the budget: one connection, many requests.
	lr.reset(10)
	if n, err := lr.Read(buf); err != nil || n != 10 {
		t.Fatalf("read after reset = (%d, %v), want (10, nil)", n, err)
	}
}

func TestMessageViewConversion(t *testing.T) {
	delivered := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	m := store.Message{
		ID: "01", ThreadID: "01", FromName: "alice", FromWS: "proj",
		ToName: "bob", ToWS: "proj", Kind: store.KindNote, Body: "hi",
		CreatedAt: delivered, DeliveredAt: &delivered,
	}
	v := messageView(m)
	if v.From != "alice@proj" || v.To != "bob@proj" {
		t.Fatalf("addresses = %s -> %s", v.From, v.To)
	}
	if v.CreatedAt != "2026-07-28T12:00:00Z" {
		t.Fatalf("created_at = %q", v.CreatedAt)
	}
	if v.DeliveredAt == nil || *v.DeliveredAt != "2026-07-28T12:00:00Z" {
		t.Fatalf("delivered_at = %v", v.DeliveredAt)
	}
	if v.AckedAt != nil {
		t.Fatalf("acked_at = %v, want nil", v.AckedAt)
	}

	// A zero time renders as empty, never as year 1.
	if got := formatTime(time.Time{}); got != "" {
		t.Fatalf("formatTime(zero) = %q, want empty", got)
	}
}

func TestRequireAddress(t *testing.T) {
	if _, _, err := requireAddress("  alice  ", " proj "); err != nil {
		t.Fatalf("trimmed address rejected: %v", err)
	}
	for _, tc := range [][2]string{{"", "proj"}, {"alice", ""}, {"a@b", "proj"}, {"alice", "p@j"}} {
		if _, _, err := requireAddress(tc[0], tc[1]); err == nil {
			t.Fatalf("requireAddress(%q, %q) = nil, want an error", tc[0], tc[1])
		}
	}
}

// TestRequireAddressRejectsControlBytes is H1: name and workspace are
// rendered verbatim in every other agent's terminal, so a control byte --
// not just "@" -- must be refused at the door rather than merely stripped
// downstream.
func TestRequireAddressRejectsControlBytes(t *testing.T) {
	// TrimSpace runs first and legitimately eats leading/trailing whitespace
	// (which includes \r, \n and \t), so every control byte here sits in the
	// middle of the string where trimming cannot remove it.
	hostile := [][2]string{
		{"alice\x1b[31m-x", "proj"},         // ESC, the start of every ANSI escape
		{"ali\rce", "proj"},                 // embedded carriage return
		{"ali\nce", "proj"},                 // embedded newline
		{"ali\x07ce", "proj"},               // BEL
		{"ali\x7fce", "proj"},               // DEL
		{"alice", "proj\x1b]0;pwned\x07-x"}, // control bytes in the workspace half too
	}
	for _, tc := range hostile {
		if _, _, err := requireAddress(tc[0], tc[1]); err == nil {
			t.Fatalf("requireAddress(%q, %q) = nil, want a control-byte rejection", tc[0], tc[1])
		}
	}
}

func TestHasControlBytes(t *testing.T) {
	for _, s := range []string{"", "plain", "with space", "unicode: café"} {
		if hasControlBytes(s) {
			t.Errorf("hasControlBytes(%q) = true, want false", s)
		}
	}
	for _, s := range []string{"\x1b", "\r", "\n", "\x00", "\x7f", "prefix\x1bsuffix"} {
		if !hasControlBytes(s) {
			t.Errorf("hasControlBytes(%q) = false, want true", s)
		}
	}
}

func TestStripControlLeavesLengthAndOrdinaryTextAlone(t *testing.T) {
	if got := stripControl("plain text, nothing to strip"); got != "plain text, nothing to strip" {
		t.Fatalf("stripControl altered ordinary text: %q", got)
	}
	got := stripControl("a\x1bb\rc\nd\x7fe")
	if strings.ContainsAny(got, "\x1b\r\n\x7f") {
		t.Fatalf("stripControl left control bytes in %q", got)
	}
	if len(got) != len("a\x1bb\rc\nd\x7fe") {
		t.Fatalf("stripControl changed the length: %d vs %d", len(got), len("a\x1bb\rc\nd\x7fe"))
	}
}

// TestRegisterSanitisesMetadata is H1: harness, cwd and session_id are
// client-controlled strings that reach every `tether who`/`status`/`doctor`
// call for that agent, so a control byte in any of them must be neutralised
// before it is stored, not merely at render time.
func TestRegisterSanitisesMetadata(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	c.mustCall(protocol.MethodRegister, protocol.RegisterParams{
		Name: "alice", Workspace: "proj",
		Harness:   "evil\x1b[31mharness",
		SessionID: "sess\x07-1",
		Cwd:       "/tmp/\x1b]0;pwned\x07repo",
		PID:       os.Getpid(),
	}, &protocol.RegisterResult{})

	var who protocol.LsResult
	c.mustCall(protocol.MethodLs, protocol.LsParams{Workspace: "proj"}, &who)
	if len(who.Agents) != 1 {
		t.Fatalf("who = %d agents, want 1", len(who.Agents))
	}
	a := who.Agents[0]
	for _, field := range []string{a.Harness, a.Cwd} {
		if hasControlBytes(field) {
			t.Fatalf("stored agent field %q still has a control byte", field)
		}
	}

	var st protocol.LsResult
	c.mustCall(protocol.MethodLs, protocol.LsParams{Name: "alice", Workspace: "proj"}, &st)

	// A same-session re-register must still be recognised as idempotent even
	// though the session id went through stripControl -- sanitising must not
	// itself corrupt the value used for the upsert guard, only the bytes a
	// terminal could act on.
	c.mustCall(protocol.MethodRegister, protocol.RegisterParams{
		Name: "alice", Workspace: "proj",
		Harness: "evil\x1b[31mharness", SessionID: "sess\x07-1", Cwd: "/tmp/repo", PID: os.Getpid(),
	}, &protocol.RegisterResult{})
}
