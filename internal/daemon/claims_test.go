package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/praneethravuri/tether/internal/protocol"
	"github.com/praneethravuri/tether/internal/store"
)

// spawnSleeper starts a short-lived real process and returns its pid, so a
// test can exercise a genuinely different, genuinely live owner -- claim
// ownership is pid-identity, not session-identity, so two goroutines in this
// same test binary sharing os.Getpid() can never simulate two owners.
func spawnSleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a real subprocess in this environment: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

// claim acts in workspace "proj" on key "src/main.go", matching send/inbox's
// own hardcoded workspace and recipient.
func (c *client) claim(holder string) protocol.ClaimResult {
	c.t.Helper()
	var out protocol.ClaimResult
	c.mustCall(protocol.MethodClaim, protocol.ClaimParams{
		Workspace: "proj", Key: "src/main.go", OwnerPID: os.Getpid(), Holder: holder,
	}, &out)
	return out
}

func TestClaim_FreshKeySucceeds(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	got := c.claim("alice")
	if got.LeaseID == "" {
		t.Fatal("Claim returned an empty lease id")
	}
	if got.Renewed || got.Reclaimed {
		t.Fatalf("fresh claim reported Renewed=%v Reclaimed=%v, want both false", got.Renewed, got.Reclaimed)
	}
	if got.Holder != "alice" {
		t.Fatalf("Holder = %q, want %q", got.Holder, "alice")
	}
}

func TestClaim_ReclaimingOwnClaimRenewsWithAFreshLeaseID(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	first := c.claim("alice")
	second := c.claim("alice")

	if !second.Renewed || second.Reclaimed {
		t.Fatalf("same-owner reclaim reported Renewed=%v Reclaimed=%v, want Renewed=true Reclaimed=false",
			second.Renewed, second.Reclaimed)
	}
	if second.LeaseID == first.LeaseID {
		t.Fatal("renewal reused the previous lease id")
	}
}

func TestClaim_HeldByLiveOwnerConflicts(t *testing.T) {
	ts := newTestServer(t, nil)
	a := ts.dial()
	a.claim("alice") // owner is this live test process

	otherPID := spawnSleeper(t) // a second, genuinely different live owner
	b := ts.dial()
	_ = b.mustFail(protocol.MethodClaim, protocol.ClaimParams{
		Workspace: "proj", Key: "src/main.go", OwnerPID: otherPID, Holder: "bob",
	}, protocol.CodeConflict)
}

// TestClaim_DeadIncumbentReclaimedImmediately mirrors
// TestRegister_DeadIncumbentReclaimedImmediately: a claim held by a provably
// dead owner is reclaimable right away, not just after its TTL elapses --
// proven with a TTL long enough that the time-based path could not explain it.
func TestClaim_DeadIncumbentReclaimedImmediately(t *testing.T) {
	ts := newTestServer(t, func(cfg *Config) { cfg.ClaimTTL = time.Hour })

	if _, err := ts.store.Claim(context.Background(), "proj", "src/main.go", implausiblePID, 0, "ghost", time.Hour); err != nil {
		t.Fatalf("seed dead incumbent: %v", err)
	}

	c := ts.dial()
	got := c.claim("alice")
	if !got.Reclaimed || got.Renewed {
		t.Fatalf("reclaim reported Renewed=%v Reclaimed=%v, want Renewed=false Reclaimed=true",
			got.Renewed, got.Reclaimed)
	}

	claim, err := ts.store.GetClaim(context.Background(), "proj", "src/main.go")
	if err != nil || claim.OwnerPID != os.Getpid() {
		t.Fatalf("reclaim did not update the stored owner: %+v, err=%v", claim, err)
	}
}

func TestClaim_MissingWorkspaceOrKeyIsBadRequest(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	_ = c.mustFail(protocol.MethodClaim, protocol.ClaimParams{
		Key: "src/main.go", OwnerPID: os.Getpid(),
	}, protocol.CodeBadRequest)
	_ = c.mustFail(protocol.MethodClaim, protocol.ClaimParams{
		Workspace: "proj", OwnerPID: os.Getpid(),
	}, protocol.CodeBadRequest)
}

func TestClaim_DeadOwnerPIDIsRejected(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	_ = c.mustFail(protocol.MethodClaim, protocol.ClaimParams{
		Workspace: "proj", Key: "src/main.go", OwnerPID: implausiblePID,
	}, protocol.CodeBadRequest)
}

// TestClaim_OwnerPIDInADifferentSessionIsRejected mirrors
// TestRegister_PIDInADifferentSessionIsRejected (finding 6.3): claim's
// OwnerPID is the same kind of trust boundary as register's session pid --
// a live pid demonstrably not this connection and not in its session (the
// shape a stolen victim pid would take) must be rejected, not trusted
// outright just because it happens to be alive.
func TestClaim_OwnerPIDInADifferentSessionIsRejected(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	detached := exec.Command("sleep", "5")
	detached.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := detached.Start(); err != nil {
		t.Skipf("cannot start a detached process in this environment: %v", err)
	}
	t.Cleanup(func() { _ = detached.Process.Kill(); _ = detached.Wait() })

	e := c.mustFail(protocol.MethodClaim, protocol.ClaimParams{
		Workspace: "proj", Key: "src/main.go", OwnerPID: detached.Process.Pid, Holder: "eve",
	}, protocol.CodeBadRequest)
	if !strings.Contains(e.Message, fmt.Sprint(detached.Process.Pid)) {
		t.Fatalf("error message %q does not name the rejected pid", e.Message)
	}
}

func TestRelease_WithCorrectLeaseIDSucceeds(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	claimed := c.claim("alice")
	var out protocol.ReleaseResult
	c.mustCall(protocol.MethodRelease, protocol.ReleaseParams{
		Workspace: "proj", Key: "src/main.go", LeaseID: claimed.LeaseID,
	}, &out)

	if _, err := ts.store.GetClaim(context.Background(), "proj", "src/main.go"); !errors.Is(err, store.ErrNoSuchClaim) {
		t.Fatalf("claim still exists after release: %v", err)
	}
}

func TestRelease_UnknownClaimIsNotFound(t *testing.T) {
	ts := newTestServer(t, nil)
	c := ts.dial()

	_ = c.mustFail(protocol.MethodRelease, protocol.ReleaseParams{
		Workspace: "proj", Key: "ghost", LeaseID: "deadbeef",
	}, protocol.CodeNotFound)
}

// TestRelease_StaleLeaseIDCannotAffectANewerAcquisition is the daemon-level
// ABA test: a lease id from an old acquisition of a key must not release a
// newer, unrelated acquisition of that same key -- the CAS must live inside
// Store.Release, not a check-then-act round trip the CLI could race.
func TestRelease_StaleLeaseIDCannotAffectANewerAcquisition(t *testing.T) {
	ts := newTestServer(t, nil)
	a := ts.dial()
	stale := a.claim("alice")

	var out protocol.ReleaseResult
	a.mustCall(protocol.MethodRelease, protocol.ReleaseParams{
		Workspace: "proj", Key: "src/main.go", LeaseID: stale.LeaseID,
	}, &out)

	b := ts.dial()
	fresh := b.claim("bob")

	_ = a.mustFail(protocol.MethodRelease, protocol.ReleaseParams{
		Workspace: "proj", Key: "src/main.go", LeaseID: stale.LeaseID,
	}, protocol.CodeConflict)

	claim, err := ts.store.GetClaim(context.Background(), "proj", "src/main.go")
	if err != nil || claim.LeaseID != fresh.LeaseID {
		t.Fatalf("a stale release must not have touched the fresh claim: %+v, err=%v", claim, err)
	}
}

func TestClaims_ListsAcrossWorkspacesWithStatus(t *testing.T) {
	ts := newTestServer(t, func(cfg *Config) { cfg.ClaimTTL = time.Hour })
	a := ts.dial()
	a.claim("alice")

	if _, err := ts.store.Claim(context.Background(), "other", "readme.md", implausiblePID, 0, "ghost", time.Hour); err != nil {
		t.Fatalf("seed dead claim: %v", err)
	}

	var scoped protocol.ClaimsResult
	a.mustCall(protocol.MethodClaims, protocol.ClaimsParams{Workspace: "proj"}, &scoped)
	if len(scoped.Claims) != 1 || scoped.Claims[0].Status != "held" {
		t.Fatalf("scoped claims = %+v, want exactly one held claim", scoped.Claims)
	}

	var all protocol.ClaimsResult
	a.mustCall(protocol.MethodClaims, protocol.ClaimsParams{}, &all)
	if len(all.Claims) != 2 {
		t.Fatalf("all claims = %d, want 2", len(all.Claims))
	}
	var sawGone bool
	for _, cl := range all.Claims {
		if cl.Workspace == "other" && cl.Status == "gone" {
			sawGone = true
		}
	}
	if !sawGone {
		t.Fatalf("expected the dead-owner claim to report status=gone: %+v", all.Claims)
	}
}

// TestClaim_ConcurrentClaimsOnTheSameKeyExactlyOneWins is the daemon-level
// concurrency test: distinct real, live owner processes race MethodClaim
// for the same fresh key over real connections, and exactly one must win.
// Claim ownership is pid identity, so each racer needs a genuinely distinct
// live pid, not just a distinct goroutine -- see spawnSleeper.
func TestClaim_ConcurrentClaimsOnTheSameKeyExactlyOneWins(t *testing.T) {
	ts := newTestServer(t, nil)

	const racers = 10
	pids := make([]int, racers)
	for i := range pids {
		pids[i] = spawnSleeper(t)
	}

	var wins atomic.Int32
	var wg sync.WaitGroup
	wg.Add(racers)
	for _, pid := range pids {
		go func(pid int) {
			defer wg.Done()
			c := ts.dial()
			resp := c.call(protocol.MethodClaim, protocol.ClaimParams{
				Workspace: "proj", Key: "contended", OwnerPID: pid, Holder: "racer",
			})
			if resp.Error == nil {
				wins.Add(1)
			} else if resp.Error.Code != protocol.CodeConflict {
				t.Errorf("unexpected error: %v", resp.Error)
			}
		}(pid)
	}
	wg.Wait()

	if got := wins.Load(); got != 1 {
		t.Fatalf("%d racers won the claim, want exactly 1", got)
	}
}
