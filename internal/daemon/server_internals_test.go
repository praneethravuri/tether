package daemon

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/praneethravuri/tether/internal/store"
)

func TestAuthenticate_SessionMismatchIsRejected(t *testing.T) {
	ts := newTestServer(t, nil)
	ctx := context.Background()

	if err := ts.store.Register(ctx, store.Agent{Workspace: "ws", Name: "alice", SessionID: "s1", Cwd: "/a"}, time.Time{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := ts.srv.authenticate(ctx, "ws", "alice", "s2"); err == nil {
		t.Fatal("authenticate with a mismatched session: want error, got nil")
	}
	if err := ts.srv.authenticate(ctx, "ws", "alice", "s1"); err != nil {
		t.Fatalf("authenticate with the matching session: %v", err)
	}
	// alice's stored session is non-empty ("s1"), so an empty claim must not
	// pass -- that is exactly the bypass this check exists to close.
	if err := ts.srv.authenticate(ctx, "ws", "alice", ""); err == nil {
		t.Fatal("authenticate with an empty claim against a non-empty stored session: want error, got nil")
	}
	// An unregistered agent is never authenticated as itself: there is no
	// stored session to have matched.
	if err := ts.srv.authenticate(ctx, "ws", "ghost", "anything"); err == nil {
		t.Fatal("authenticate against an unregistered agent: want error, got nil")
	}
}

// TestAuthenticate_BothSessionsEmptyIsAllowed covers the one legitimate empty
// case: an agent registered with no session recorded (an unrecognised
// harness that could not synthesise one) can still act as itself as long as
// every caller likewise claims no session.
func TestAuthenticate_BothSessionsEmptyIsAllowed(t *testing.T) {
	ts := newTestServer(t, nil)
	ctx := context.Background()

	if err := ts.store.Register(ctx, store.Agent{Workspace: "ws", Name: "alice", SessionID: "", Cwd: "/a"}, time.Time{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := ts.srv.authenticate(ctx, "ws", "alice", ""); err != nil {
		t.Fatalf("authenticate with both sessions empty: %v", err)
	}
	if err := ts.srv.authenticate(ctx, "ws", "alice", "s1"); err == nil {
		t.Fatal("authenticate claiming a session against an empty stored one: want error, got nil")
	}
}

func TestAuthenticate_StoreErrorPropagates(t *testing.T) {
	ts := newTestServer(t, nil)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	if err := ts.srv.authenticate(canceled, "ws", "alice", "s1"); err == nil {
		t.Fatal("authenticate with a canceled context: want error, got nil")
	}
}

func TestTouch_LogsHeartbeatFailuresWithoutPropagating(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	ts := newTestServer(t, func(c *Config) { c.Logger = log.New(&lockedWriter{mu: &mu, w: &buf}, "", 0) })

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	ts.srv.touch(canceled, "ws", "alice", "register")

	mu.Lock()
	got := buf.String()
	mu.Unlock()
	if !strings.Contains(got, "touch:") {
		t.Fatalf("touch did not log its failures: %q", got)
	}
}

func TestSweepOnce_LogsStoreFailuresWithoutPanicking(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	// A long SweepInterval keeps the background sweepLoop from firing again
	// during the test and racing this direct call on the shared log buffer.
	ts := newTestServer(t, func(c *Config) {
		c.Logger = log.New(&lockedWriter{mu: &mu, w: &buf}, "", 0)
		c.SweepInterval = time.Hour
	})

	if err := ts.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	ts.srv.sweepOnce(context.Background())

	mu.Lock()
	got := buf.String()
	mu.Unlock()
	if !strings.Contains(got, "failed") {
		t.Fatalf("sweepOnce did not log the store failure: %q", got)
	}
}

func TestSweepOnce_SweepsDeadMessages(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	clk := newFakeClock()
	// A long SweepInterval keeps the background sweepLoop from firing again
	// during the test and racing this direct call on the shared log buffer.
	ts := newTestServerWithClock(t, func(c *Config) {
		c.Logger = log.New(&lockedWriter{mu: &mu, w: &buf}, "", 0)
		c.DeadAfter = time.Minute
		c.SweepInterval = time.Hour
	}, clk.Now)
	ctx := context.Background()

	if err := ts.store.Register(ctx, store.Agent{Workspace: "ws", Name: "alice", Cwd: "/a"}, time.Time{}); err != nil {
		t.Fatalf("register alice: %v", err)
	}
	if err := ts.store.Register(ctx, store.Agent{Workspace: "ws", Name: "bob", Cwd: "/b"}, time.Time{}); err != nil {
		t.Fatalf("register bob: %v", err)
	}
	if _, err := ts.store.Send(ctx, store.Message{FromName: "alice", FromWS: "ws", ToName: "bob", ToWS: "ws", Kind: store.KindNote, Body: "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	clk.advance(2 * time.Minute)
	ts.srv.sweepOnce(ctx)

	mu.Lock()
	got := buf.String()
	mu.Unlock()
	if !strings.Contains(got, "swept 1 dead message") {
		t.Errorf("sweepOnce did not report sweeping the dead message: %q", got)
	}
}

// TestSweepOnce_RotatesAnOversizedLog is 6.18's "periodic size check in the
// daemon loop" half: the log doesn't wait for the next daemon restart to
// rotate, the existing sweep cycle does it.
func TestSweepOnce_RotatesAnOversizedLog(t *testing.T) {
	dir := shortTempDir(t)
	logPath := filepath.Join(dir, "daemon.log")
	if err := os.WriteFile(logPath, make([]byte, logMaxBytes+1000), 0o600); err != nil {
		t.Fatalf("seed oversized log: %v", err)
	}

	ts := newTestServer(t, func(c *Config) { c.LogPath = logPath })
	ts.srv.sweepOnce(context.Background())

	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() > logMaxBytes {
		t.Fatalf("log is %d bytes after sweepOnce, want at most %d", info.Size(), logMaxBytes)
	}
}

// TestSweepOnce_PurgesRetiredMessages proves the sweep also deletes read/dead
// mail past RetainMessages, separately from the 24h DeadAfter marking pass.
func TestSweepOnce_PurgesRetiredMessages(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	clk := newFakeClock()
	// A long SweepInterval keeps the background sweepLoop from firing again
	// during the test and racing this direct call on the shared log buffer.
	ts := newTestServerWithClock(t, func(c *Config) {
		c.Logger = log.New(&lockedWriter{mu: &mu, w: &buf}, "", 0)
		c.RetainMessages = time.Hour
		c.SweepInterval = time.Hour
	}, clk.Now)
	ctx := context.Background()

	if err := ts.store.Register(ctx, store.Agent{Workspace: "ws", Name: "alice", Cwd: "/a"}, time.Time{}); err != nil {
		t.Fatalf("register alice: %v", err)
	}
	if err := ts.store.Register(ctx, store.Agent{Workspace: "ws", Name: "bob", Cwd: "/b"}, time.Time{}); err != nil {
		t.Fatalf("register bob: %v", err)
	}
	if _, err := ts.store.Send(ctx, store.Message{FromName: "alice", FromWS: "ws", ToName: "bob", ToWS: "ws", Kind: store.KindNote, Body: "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, _, err := ts.store.Drain(ctx, "ws", "bob", 10); err != nil {
		t.Fatalf("drain: %v", err)
	}

	clk.advance(2 * time.Hour)
	ts.srv.sweepOnce(ctx)

	mu.Lock()
	got := buf.String()
	mu.Unlock()
	if !strings.Contains(got, "purged 1 message") {
		t.Errorf("sweepOnce did not report purging the retired message: %q", got)
	}
	replay, err := ts.store.Replay(ctx, "ws", "bob", 10, 0)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(replay) != 0 {
		t.Fatalf("Replay after purge = %d, want 0", len(replay))
	}
}

// TestSweepLoop_SweepsOnceAtStartup proves a freshly started daemon sweeps
// immediately instead of waiting a full SweepInterval for its first tick --
// a daemon that restarts often would otherwise never prune anything. Uses a
// deliberately long interval so an early sweep is the only way the log line
// can appear within the short poll window below.
//
// sweepDeadAgents' cutoff is the real wall clock (time.Now(), not the
// store's overridable one), so the seeded agent's LastSeen is set an hour
// in the past for a comfortable, scheduling-jitter-proof margin -- unlike
// age-based message/observation sweeping, there is no millisecond-precision
// race to lose here.
func TestSweepLoop_SweepsOnceAtStartup(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	newTestServerFull(t, func(c *Config) {
		c.Logger = log.New(&lockedWriter{mu: &mu, w: &buf}, "", 0)
		c.SweepInterval = time.Hour
		c.DeadAfter = time.Minute
	}, func() time.Time { return time.Now().Add(-time.Hour) }, func(st *store.Store) {
		// Seeded before Serve starts: sweepLoop's first sweep runs the instant
		// its goroutine is scheduled, so inserting after construction would
		// race it -- the agent might not exist yet when that sweep runs, and
		// the next one is an hour away.
		if err := st.Register(context.Background(), store.Agent{
			Workspace: "ws", Name: "gone", Cwd: "/g", PID: implausiblePID,
		}, time.Time{}); err != nil {
			t.Fatalf("register: %v", err)
		}
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := buf.String()
		mu.Unlock()
		if strings.Contains(got, "dead agent") {
			return // the startup sweep ran well before the 1-hour interval ever could
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("sweepLoop did not sweep at startup within 2s")
}

// lockedWriter guards w with mu, since the sweep loop's goroutine and this
// test's polling goroutine both touch buf.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestSweepDeadAgents_RemovesOnlyStaleAndDeadRows(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	// sweepDeadAgents' cutoff is time.Now().Add(-DeadAfter), using the real
	// wall clock rather than the store's overridable one, so LastSeen has to
	// predate it by actually being written far in the past. A long
	// SweepInterval also keeps the background sweepLoop from firing again
	// during the test and racing this direct call on the shared log buffer.
	ts := newTestServerWithClock(t, func(c *Config) {
		c.Logger = log.New(&lockedWriter{mu: &mu, w: &buf}, "", 0)
		c.DeadAfter = time.Minute
		c.SweepInterval = time.Hour
	}, func() time.Time { return time.Now().Add(-time.Hour) })
	ctx := context.Background()

	// Dead: stale and its pid is not alive.
	if err := ts.store.Register(ctx, store.Agent{Workspace: "ws", Name: "gone", Cwd: "/g", PID: 1 << 30}, time.Time{}); err != nil {
		t.Fatalf("register gone: %v", err)
	}
	// Stale by time but still alive: must survive the sweep.
	if err := ts.store.Register(ctx, store.Agent{Workspace: "ws", Name: "alive", Cwd: "/a", PID: os.Getpid()}, time.Time{}); err != nil {
		t.Fatalf("register alive: %v", err)
	}

	ts.srv.sweepDeadAgents(ctx)

	if _, err := ts.store.GetAgent(ctx, "ws", "gone"); err == nil {
		t.Error("dead agent survived the sweep")
	}
	if _, err := ts.store.GetAgent(ctx, "ws", "alive"); err != nil {
		t.Errorf("live agent was swept away: %v", err)
	}
	mu.Lock()
	got := buf.String()
	mu.Unlock()
	if !strings.Contains(got, "swept 1 dead agent") {
		t.Errorf("sweepDeadAgents did not report the removal: %q", got)
	}
}

func TestSweepDeadAgents_LogsListFailureWithoutPanicking(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	// A long SweepInterval keeps the background sweepLoop from firing again
	// during the test and racing this direct call on the shared log buffer.
	ts := newTestServer(t, func(c *Config) {
		c.Logger = log.New(&lockedWriter{mu: &mu, w: &buf}, "", 0)
		c.SweepInterval = time.Hour
	})

	if err := ts.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	ts.srv.sweepDeadAgents(context.Background())

	mu.Lock()
	got := buf.String()
	mu.Unlock()
	if !strings.Contains(got, "sweep dead agents") {
		t.Fatalf("sweepDeadAgents did not log the list failure: %q", got)
	}
}
