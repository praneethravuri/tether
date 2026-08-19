package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------- helpers --

// newStore opens a throwaway database on disk (":memory:" would give each pool a separate DB).
func newStore(t *testing.T) *Store {
	t.Helper()
	return openAt(t, filepath.Join(t.TempDir(), "tether.db"))
}

func openAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// clock is a mutex-guarded fake clock so tests can jump past staleness
// thresholds without sleeping. It is race-safe on purpose.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func mustRegister(t *testing.T, s *Store, a Agent) {
	t.Helper()
	if err := s.Register(context.Background(), a, s.now().Add(-time.Minute)); err != nil {
		t.Fatalf("Register(%s): %v", a.Address(), err)
	}
}

func mustSend(t *testing.T, s *Store, m Message) string {
	t.Helper()
	id, err := s.Send(context.Background(), m)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	return id
}

// note builds a minimal valid message from alice to bob in workspace ws.
func note(body string) Message {
	return Message{
		FromName: "alice", FromWS: "ws",
		ToName: "bob", ToWS: "ws",
		Kind: KindNote, Body: body,
	}
}

// ------------------------------------------------------------------ open ---

func TestOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tether.db")

	s1, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	mustRegister(t, s1, Agent{Workspace: "ws", Name: "alice", Cwd: "/tmp"})
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening must re-apply the schema without error and see prior data.
	s2 := openAt(t, path)
	if _, err := s2.GetAgent(ctx, "ws", "alice"); err != nil {
		t.Fatalf("agent did not survive reopen: %v", err)
	}

	// And a third open on the same path is still fine.
	s3 := openAt(t, path)
	if _, err := s3.ListAgents(ctx, "", 0); err != nil {
		t.Fatalf("ListAgents after third open: %v", err)
	}
}

// failingRollback lets tests force a rollback failure without a real connection.
type failingRollback struct{ err error }

func (f failingRollback) Rollback() error { return f.err }

func TestRollbackLogsGenuineFailures(t *testing.T) {
	var buf bytes.Buffer
	s := &Store{Logger: log.New(&buf, "", 0)}

	s.rollback(failingRollback{err: errors.New("disk I/O error")})
	if !strings.Contains(buf.String(), "disk I/O error") {
		t.Fatalf("a genuine rollback failure was not logged: %q", buf.String())
	}

	buf.Reset()
	s.rollback(failingRollback{err: sql.ErrTxDone})
	if buf.Len() != 0 {
		t.Fatalf("sql.ErrTxDone should not be logged (it means the tx was already finished): %q", buf.String())
	}

	buf.Reset()
	s.rollback(failingRollback{err: nil})
	if buf.Len() != 0 {
		t.Fatalf("a successful rollback should not be logged: %q", buf.String())
	}

	// No Logger configured: must not panic.
	var silent Store
	silent.rollback(failingRollback{err: errors.New("boom")})
}

func TestOpenDefaults(t *testing.T) {
	s := newStore(t)
	if s.Now == nil {
		t.Error("Now should default to time.Now")
	}
	if s.MaxBodyBytes != 64<<10 {
		t.Errorf("MaxBodyBytes = %d, want %d", s.MaxBodyBytes, 64<<10)
	}
}

// ---------------------------------------------------------------- agents ---

func TestRegisterGetAgentRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	clk := newClock()
	s.Now = clk.Now

	want := Agent{
		Workspace: "myws",
		Name:      "alice",
		Harness:   "claude-code",
		SessionID: "sess-1",
		Cwd:       "/home/u/proj",
		PID:       4242,
		PIDStart:  99999,
	}
	mustRegister(t, s, want)

	got, err := s.GetAgent(ctx, "myws", "alice")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}

	if got.Workspace != want.Workspace || got.Name != want.Name ||
		got.Harness != want.Harness || got.SessionID != want.SessionID ||
		got.Cwd != want.Cwd || got.PID != want.PID || got.PIDStart != want.PIDStart {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	if got.RegisteredAt.UnixMilli() != clk.Now().UnixMilli() {
		t.Errorf("RegisteredAt = %v, want %v", got.RegisteredAt, clk.Now())
	}
	if got.LastSeen.UnixMilli() != clk.Now().UnixMilli() {
		t.Errorf("LastSeen = %v, want %v", got.LastSeen, clk.Now())
	}
	if got.Address() != "alice@myws" {
		t.Errorf("Address() = %q", got.Address())
	}
}

func TestRegisterAppliesDefaults(t *testing.T) {
	s := newStore(t)
	mustRegister(t, s, Agent{Workspace: "ws", Name: "bare", Cwd: "/tmp"})

	got, err := s.GetAgent(context.Background(), "ws", "bare")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.Harness != "unknown" {
		t.Errorf("defaults not applied: %+v", got)
	}
}

func TestRegisterSameSessionSucceeds(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	clk := newClock()
	s.Now = clk.Now

	a := Agent{Workspace: "ws", Name: "alice", SessionID: "sess-1", Cwd: "/a"}
	mustRegister(t, s, a)

	// Same session re-registering (daemon restarted, agent reconnects) must be
	// allowed even though the incumbent row is perfectly fresh.
	a.Cwd = "/a2"
	if err := s.Register(ctx, a, clk.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("same-session re-register: %v", err)
	}

	got, err := s.GetAgent(ctx, "ws", "alice")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.Cwd != "/a2" {
		t.Errorf("re-register did not refresh fields: %+v", got)
	}
	if got.RegisteredAt.UnixMilli() != clk.Now().UnixMilli() {
		t.Errorf("same session should keep original RegisteredAt")
	}
}

func TestRegisterNameTakenThenStaleTakeover(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	clk := newClock()
	s.Now = clk.Now

	const staleAfter = time.Minute

	incumbent := Agent{Workspace: "ws", Name: "alice", SessionID: "sess-1", Cwd: "/a"}
	if err := s.Register(ctx, incumbent, clk.Now().Add(-staleAfter)); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	// A different session while the holder is fresh must be rejected.
	intruder := Agent{Workspace: "ws", Name: "alice", SessionID: "sess-2", Cwd: "/b"}
	err := s.Register(ctx, intruder, clk.Now().Add(-staleAfter))
	if !errors.Is(err, ErrNameTaken) {
		t.Fatalf("Register by other live session = %v, want ErrNameTaken", err)
	}

	// The incumbent still owns the row.
	got, _ := s.GetAgent(ctx, "ws", "alice")
	if got.SessionID != "sess-1" || got.Cwd != "/a" {
		t.Fatalf("failed register mutated the row: %+v", got)
	}

	// Fast-forward past the staleness window: takeover now succeeds.
	clk.advance(2 * time.Minute)
	if err := s.Register(ctx, intruder, clk.Now().Add(-staleAfter)); err != nil {
		t.Fatalf("takeover of stale name: %v", err)
	}
	got, err = s.GetAgent(ctx, "ws", "alice")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.SessionID != "sess-2" || got.Cwd != "/b" {
		t.Errorf("takeover did not replace holder: %+v", got)
	}
	if got.RegisteredAt.UnixMilli() != clk.Now().UnixMilli() {
		t.Errorf("takeover should reset RegisteredAt, got %v", got.RegisteredAt)
	}
}

// TestReclaimAgentSucceedsOnMatchingIncumbent is defect C5: reclaiming a
// name via ReclaimAgent overwrites the row when it still matches the exact
// pid/pid_start the caller observed.
func TestReclaimAgentSucceedsOnMatchingIncumbent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	mustRegister(t, s, Agent{Workspace: "ws", Name: "alice", SessionID: "sess-1", Cwd: "/a", PID: 111, PIDStart: 222})

	claimant := Agent{Workspace: "ws", Name: "alice", SessionID: "sess-2", Cwd: "/b", PID: 333, PIDStart: 444}
	ok, err := s.ReclaimAgent(ctx, claimant, 111, 222)
	if err != nil {
		t.Fatalf("ReclaimAgent: %v", err)
	}
	if !ok {
		t.Fatal("ReclaimAgent = false, want true (pid/pid_start matched)")
	}
	got, err := s.GetAgent(ctx, "ws", "alice")
	if err != nil || got.SessionID != "sess-2" {
		t.Fatalf("reclaim did not take effect: %+v, err=%v", got, err)
	}
}

// TestReclaimAgentFailsWhenTheRowMoved is the TOCTOU close: if the
// incumbent's pid/pid_start no longer matches what the caller observed
// (someone else already reclaimed it, or it came back alive under a new
// identity), ReclaimAgent must not blindly overwrite it.
func TestReclaimAgentFailsWhenTheRowMoved(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	mustRegister(t, s, Agent{Workspace: "ws", Name: "alice", SessionID: "sess-1", Cwd: "/a", PID: 111, PIDStart: 222})

	claimant := Agent{Workspace: "ws", Name: "alice", SessionID: "sess-2", Cwd: "/b", PID: 333, PIDStart: 444}
	ok, err := s.ReclaimAgent(ctx, claimant, 999, 888) // stale observation: wrong pid/pid_start
	if err != nil {
		t.Fatalf("ReclaimAgent: %v", err)
	}
	if ok {
		t.Fatal("ReclaimAgent = true, want false (observed identity did not match the current row)")
	}
	got, err := s.GetAgent(ctx, "ws", "alice")
	if err != nil || got.SessionID != "sess-1" {
		t.Fatalf("a failed reclaim mutated the row: %+v, err=%v", got, err)
	}
}

func TestGetAgentUnknown(t *testing.T) {
	s := newStore(t)
	if _, err := s.GetAgent(context.Background(), "ws", "ghost"); !errors.Is(err, ErrNoSuchAgent) {
		t.Fatalf("GetAgent(ghost) = %v, want ErrNoSuchAgent", err)
	}
}

func TestFindNameBySession(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	mustRegister(t, s, Agent{Workspace: "ws", Name: "alice", SessionID: "sess-1", Cwd: "/a"})

	got, err := s.FindNameBySession(ctx, "ws", "sess-1")
	if err != nil {
		t.Fatalf("FindNameBySession: %v", err)
	}
	if got != "alice" {
		t.Fatalf("FindNameBySession = %q, want alice", got)
	}
}

func TestFindNameBySessionUnknown(t *testing.T) {
	s := newStore(t)
	if _, err := s.FindNameBySession(context.Background(), "ws", "sess-nope"); !errors.Is(err, ErrNoSuchAgent) {
		t.Fatalf("FindNameBySession(unknown session) = %v, want ErrNoSuchAgent", err)
	}
}

func TestRenameMovesNameAndPendingMail(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	mustRegister(t, s, Agent{Workspace: "ws", Name: "frontend", SessionID: "sess-1", Cwd: "/a"})
	mustSend(t, s, Message{FromName: "backend", FromWS: "ws", ToName: "frontend", ToWS: "ws", Body: "hi"})

	oldName, err := s.Rename(ctx, Agent{Workspace: "ws", Name: "frontend2", SessionID: "sess-1", Cwd: "/a2"}, time.Time{})
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if oldName != "frontend" {
		t.Fatalf("Rename returned old name %q, want frontend", oldName)
	}

	if _, err := s.GetAgent(ctx, "ws", "frontend"); !errors.Is(err, ErrNoSuchAgent) {
		t.Fatalf("old name still exists after rename: %v", err)
	}
	got, err := s.GetAgent(ctx, "ws", "frontend2")
	if err != nil {
		t.Fatalf("GetAgent(new name): %v", err)
	}
	if got.Cwd != "/a2" {
		t.Fatalf("rename did not refresh other fields: %+v", got)
	}

	msgs, _, err := s.Drain(ctx, "ws", "frontend2", 0)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Body != "hi" {
		t.Fatalf("pending mail did not follow the rename: %+v", msgs)
	}
}

func TestRenameOntoALiveNameConflicts(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	mustRegister(t, s, Agent{Workspace: "ws", Name: "frontend", SessionID: "sess-1", Cwd: "/a"})
	mustRegister(t, s, Agent{Workspace: "ws", Name: "backend", SessionID: "sess-2", Cwd: "/b"})

	_, err := s.Rename(ctx, Agent{Workspace: "ws", Name: "backend", SessionID: "sess-1", Cwd: "/a"}, time.Time{})
	if !errors.Is(err, ErrNameTaken) {
		t.Fatalf("Rename onto a live name = %v, want ErrNameTaken", err)
	}

	// Neither row was touched by the rejected rename.
	if _, err := s.GetAgent(ctx, "ws", "frontend"); err != nil {
		t.Fatalf("frontend disappeared after a rejected rename: %v", err)
	}
	got, err := s.GetAgent(ctx, "ws", "backend")
	if err != nil || got.SessionID != "sess-2" {
		t.Fatalf("backend was mutated by a rejected rename: %+v, err=%v", got, err)
	}
}

// TestRenameOntoADeadNameReclaimsWithAPastCutoff is drift item 17: a target
// name held by an agent whose last_seen is before staleCutoff is renamable
// into, the same escape hatch Register already has for a dead incumbent.
func TestRenameOntoADeadNameReclaimsWithAPastCutoff(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	mustRegister(t, s, Agent{Workspace: "ws", Name: "frontend", SessionID: "sess-1", Cwd: "/a"})
	mustRegister(t, s, Agent{Workspace: "ws", Name: "backend", SessionID: "sess-2", Cwd: "/b"})

	// A cutoff in the future makes every real last_seen count as stale.
	futureCutoff := time.Now().Add(time.Hour)
	oldName, err := s.Rename(ctx,
		Agent{Workspace: "ws", Name: "backend", SessionID: "sess-1", Cwd: "/a"}, futureCutoff)
	if err != nil {
		t.Fatalf("Rename onto a name past the cutoff: %v", err)
	}
	if oldName != "frontend" {
		t.Fatalf("oldName = %q, want frontend", oldName)
	}
	got, err := s.GetAgent(ctx, "ws", "backend")
	if err != nil || got.SessionID != "sess-1" {
		t.Fatalf("backend was not reclaimed by sess-1: %+v, err=%v", got, err)
	}
}

func TestReclaimRenameLeavesChangedTargetUntouched(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	mustRegister(t, s, Agent{Workspace: "ws", Name: "frontend", SessionID: "sess-1", Cwd: "/a", PID: 111, PIDStart: 222})
	mustRegister(t, s, Agent{Workspace: "ws", Name: "backend", SessionID: "sess-2", Cwd: "/b", PID: 333, PIDStart: 444})

	observed, err := s.GetAgent(ctx, "ws", "backend")
	if err != nil {
		t.Fatalf("GetAgent(target): %v", err)
	}
	if ok, err := s.ReclaimAgent(ctx,
		Agent{Workspace: "ws", Name: "backend", SessionID: "sess-3", Cwd: "/c", PID: 555, PIDStart: 666},
		observed.PID, observed.PIDStart); err != nil || !ok {
		t.Fatalf("replace observed target: ok=%v, err=%v", ok, err)
	}

	_, reclaimed, err := s.ReclaimRename(ctx,
		Agent{Workspace: "ws", Name: "backend", SessionID: "sess-1", Cwd: "/a2", PID: 111, PIDStart: 222}, observed)
	if err != nil {
		t.Fatalf("ReclaimRename: %v", err)
	}
	if reclaimed {
		t.Fatal("ReclaimRename succeeded after the target identity changed")
	}

	target, err := s.GetAgent(ctx, "ws", "backend")
	if err != nil || target.SessionID != "sess-3" {
		t.Fatalf("changed target was overwritten: %+v, err=%v", target, err)
	}
	if _, err := s.GetAgent(ctx, "ws", "frontend"); err != nil {
		t.Fatalf("source disappeared after rejected reclaiming rename: %v", err)
	}
}

func TestReclaimRenameSucceedsForObservedTarget(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	mustRegister(t, s, Agent{Workspace: "ws", Name: "frontend", SessionID: "sess-1", Cwd: "/a", PID: 111, PIDStart: 222})
	mustRegister(t, s, Agent{Workspace: "ws", Name: "backend", SessionID: "sess-2", Cwd: "/b", PID: 333, PIDStart: 444})
	observed, err := s.GetAgent(ctx, "ws", "backend")
	if err != nil {
		t.Fatalf("GetAgent(target): %v", err)
	}

	oldName, reclaimed, err := s.ReclaimRename(ctx,
		Agent{Workspace: "ws", Name: "backend", SessionID: "sess-1", Cwd: "/a2", PID: 111, PIDStart: 222}, observed)
	if err != nil {
		t.Fatalf("ReclaimRename: %v", err)
	}
	if !reclaimed || oldName != "frontend" {
		t.Fatalf("ReclaimRename = (%q, %v), want (frontend, true)", oldName, reclaimed)
	}
	target, err := s.GetAgent(ctx, "ws", "backend")
	if err != nil || target.SessionID != "sess-1" {
		t.Fatalf("target was not reclaimed by source session: %+v, err=%v", target, err)
	}
}

func TestRenameNoExistingSessionIsNoSuchAgent(t *testing.T) {
	s := newStore(t)
	_, err := s.Rename(context.Background(), Agent{Workspace: "ws", Name: "frontend", SessionID: "sess-ghost", Cwd: "/a"}, time.Time{})
	if !errors.Is(err, ErrNoSuchAgent) {
		t.Fatalf("Rename with no existing session = %v, want ErrNoSuchAgent", err)
	}
}

func TestRenameToTheSameNameIsANoOpRefresh(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	mustRegister(t, s, Agent{Workspace: "ws", Name: "frontend", SessionID: "sess-1", Cwd: "/a"})

	oldName, err := s.Rename(ctx, Agent{Workspace: "ws", Name: "frontend", SessionID: "sess-1", Cwd: "/a2"}, time.Time{})
	if err != nil {
		t.Fatalf("Rename to the same name: %v", err)
	}
	if oldName != "frontend" {
		t.Fatalf("oldName = %q, want frontend", oldName)
	}
	got, err := s.GetAgent(ctx, "ws", "frontend")
	if err != nil || got.Cwd != "/a2" {
		t.Fatalf("same-name rename did not refresh fields: %+v, err=%v", got, err)
	}
}

func TestListAgentsFiltersByWorkspaceAndStaleness(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	clk := newClock()
	s.Now = clk.Now

	mustRegister(t, s, Agent{Workspace: "ws1", Name: "alice", Cwd: "/a"})
	mustRegister(t, s, Agent{Workspace: "ws1", Name: "bob", Cwd: "/b"})
	mustRegister(t, s, Agent{Workspace: "ws2", Name: "carol", Cwd: "/c"})

	all, err := s.ListAgents(ctx, "", 0)
	if err != nil {
		t.Fatalf("ListAgents(all): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListAgents(\"\") = %d agents, want 3", len(all))
	}
	// Ordered by workspace, then name.
	if all[0].Name != "alice" || all[1].Name != "bob" || all[2].Name != "carol" {
		t.Errorf("unexpected order: %v %v %v", all[0].Name, all[1].Name, all[2].Name)
	}

	ws1, err := s.ListAgents(ctx, "ws1", 0)
	if err != nil {
		t.Fatalf("ListAgents(ws1): %v", err)
	}
	if len(ws1) != 2 {
		t.Fatalf("ListAgents(ws1) = %d agents, want 2", len(ws1))
	}

	// Move time forward; only alice heartbeats, so only alice stays fresh.
	clk.advance(5 * time.Minute)
	if _, err := s.Heartbeat(ctx, "ws1", "alice", "send"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	fresh, err := s.ListAgents(ctx, "", time.Minute)
	if err != nil {
		t.Fatalf("ListAgents(fresh): %v", err)
	}
	if len(fresh) != 1 || fresh[0].Name != "alice" {
		t.Fatalf("staleness filter returned %+v, want only alice", fresh)
	}

	// Staleness filter still respects the workspace filter.
	none, err := s.ListAgents(ctx, "ws2", time.Minute)
	if err != nil {
		t.Fatalf("ListAgents(ws2, fresh): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("ListAgents(ws2, fresh) = %d, want 0", len(none))
	}
}

func TestHeartbeat(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	clk := newClock()
	s.Now = clk.Now

	mustRegister(t, s, Agent{Workspace: "ws", Name: "alice", Cwd: "/a"})
	mustRegister(t, s, Agent{Workspace: "ws", Name: "bob", Cwd: "/b"})

	mustSend(t, s, note("one"))
	mustSend(t, s, note("two"))

	pending, err := s.Heartbeat(ctx, "ws", "bob", "inbox")
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if pending != 2 {
		t.Errorf("Heartbeat pending = %d, want 2", pending)
	}

	clk.advance(time.Second)
	before, _ := s.GetAgent(ctx, "ws", "bob")
	if _, err := s.Heartbeat(ctx, "ws", "bob", "send"); err != nil {
		t.Fatalf("Heartbeat(again): %v", err)
	}
	after, _ := s.GetAgent(ctx, "ws", "bob")
	if !after.LastSeen.After(before.LastSeen) {
		t.Errorf("Heartbeat did not advance last_seen: before %v, after %v", before.LastSeen, after.LastSeen)
	}
	if after.LastKind != "send" {
		t.Errorf("last_kind = %q, want %q", after.LastKind, "send")
	}

	if _, err := s.Heartbeat(ctx, "ws", "ghost", "send"); !errors.Is(err, ErrNoSuchAgent) {
		t.Fatalf("Heartbeat(ghost) = %v, want ErrNoSuchAgent", err)
	}
}

func TestDeleteAgents(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	mustRegister(t, s, Agent{Workspace: "ws", Name: "alice", Cwd: "/a"})
	mustRegister(t, s, Agent{Workspace: "ws", Name: "bob", Cwd: "/b"})
	mustRegister(t, s, Agent{Workspace: "ws", Name: "carol", Cwd: "/c"})

	n, err := s.DeleteAgents(ctx, []AgentKey{{Workspace: "ws", Name: "alice"}, {Workspace: "ws", Name: "bob"}})
	if err != nil {
		t.Fatalf("DeleteAgents: %v", err)
	}
	if n != 2 {
		t.Fatalf("DeleteAgents removed %d rows, want 2", n)
	}

	if _, err := s.GetAgent(ctx, "ws", "alice"); !errors.Is(err, ErrNoSuchAgent) {
		t.Fatalf("alice still present after DeleteAgents: %v", err)
	}
	if _, err := s.GetAgent(ctx, "ws", "bob"); !errors.Is(err, ErrNoSuchAgent) {
		t.Fatalf("bob still present after DeleteAgents: %v", err)
	}
	if _, err := s.GetAgent(ctx, "ws", "carol"); err != nil {
		t.Fatalf("carol was removed too: %v", err)
	}

	// A repeated or empty call is a harmless no-op, not an error.
	if n, err := s.DeleteAgents(ctx, []AgentKey{{Workspace: "ws", Name: "alice"}}); err != nil || n != 0 {
		t.Fatalf("DeleteAgents(already gone) = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := s.DeleteAgents(ctx, nil); err != nil || n != 0 {
		t.Fatalf("DeleteAgents(nil) = (%d, %v), want (0, nil)", n, err)
	}
}

// TestDeleteAgents_ChunksPastTheBoundParamLimit proves a batch larger than
// one chunk still deletes everything, not just the first chunk's worth --
// unlike ackIDs' ids (bounded by the 500-message inbox cap upstream), a dead
// agent sweep's key count has no such ceiling.
func TestDeleteAgents_ChunksPastTheBoundParamLimit(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	const total = deleteAgentsChunk + 50
	keys := make([]AgentKey, 0, total)
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("agent-%d", i)
		mustRegister(t, s, Agent{Workspace: "ws", Name: name, Cwd: "/a"})
		keys = append(keys, AgentKey{Workspace: "ws", Name: name})
	}

	n, err := s.DeleteAgents(ctx, keys)
	if err != nil {
		t.Fatalf("DeleteAgents: %v", err)
	}
	if n != total {
		t.Fatalf("DeleteAgents removed %d rows, want %d", n, total)
	}

	remaining, err := s.ListAgents(ctx, "ws", 0)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("%d agents remain after deleting a multi-chunk batch, want 0", len(remaining))
	}
}
