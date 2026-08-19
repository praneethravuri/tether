package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/praneethravuri/intern/internal/proc"
	"github.com/praneethravuri/intern/internal/protocol"
	"github.com/praneethravuri/intern/internal/store"
)

// handleRegister claims or refreshes a workspace/name for the caller.
func (s *Server) handleRegister(ctx context.Context, req protocol.Request, peerPID int) protocol.Response {
	var p protocol.RegisterParams
	if err := decodeParams(req, &p); err != nil {
		return s.fail(req.ID, err, "register")
	}

	ws, err := requireWorkspace(p.Workspace)
	if err != nil {
		return s.fail(req.ID, err, "register")
	}

	harness := stripControl(strings.TrimSpace(p.Harness))
	if harness == "" {
		harness = "unknown"
	}
	session := stripControl(strings.TrimSpace(p.SessionID))

	// A session that already holds a name in ws: resolving an empty Name
	// falls back to it, and an explicit different Name renames it in place.
	var existingName string
	if session != "" {
		if n, findErr := s.store.FindNameBySession(ctx, ws, session); findErr == nil {
			existingName = n
		} else if !errors.Is(findErr, store.ErrNoSuchAgent) {
			return s.fail(req.ID, findErr, "register")
		}
	}

	name := strings.TrimSpace(p.Name)
	switch {
	case name == "" && existingName != "":
		name = existingName
	case name == "":
		name, err = s.mintFreeName(ctx, ws, harness, session)
		if err != nil {
			return s.fail(req.ID, err, "register")
		}
	default:
		name, err = requireName(name)
		if err != nil {
			return s.fail(req.ID, err, "register")
		}
	}

	// peerPID is the short-lived intern CLI itself, not the shell pid a
	// client claims -- so what must hold is a shared session, not equality.
	sessionPID := p.PID
	switch {
	case peerPID <= 0:
		// no signal to check against; fall through unchanged
	case sessionPID <= 0:
		sessionPID = peerPID
	case sessionPID == peerPID, proc.SameSession(sessionPID, peerPID):
		// claimed pid is the connecting process, or shares its session
	default:
		return s.fail(req.ID, badRequest(
			"pid %d is not this connection's process or in its session", sessionPID), "register")
	}

	if sessionPID > 0 && !proc.Alive(sessionPID) {
		return s.fail(req.ID, badRequest("session pid %d is not alive", sessionPID), "register")
	}
	var pidStart int64
	if sessionPID > 0 {
		pidStart, _ = proc.StartTime(sessionPID) // 0 on failure is proc.AliveAt's "unknown"
	}

	a := store.Agent{
		Workspace: ws,
		Name:      name,
		Harness:   harness,
		SessionID: session,
		Cwd:       stripControl(p.Cwd),
		PID:       sessionPID,
		PIDStart:  pidStart,
	}

	var created, renamed bool
	if existingName != "" && existingName != name {
		if err := s.renameOrReclaim(ctx, a); err != nil {
			if errors.Is(err, store.ErrNameTaken) {
				err = s.withNameSuggestion(ctx, err, ws, name)
			}
			return s.fail(req.ID, err, "register")
		}
		renamed = true
	} else {
		created, err = s.registerOrReclaim(ctx, a)
		if err != nil {
			if errors.Is(err, store.ErrNameTaken) {
				err = s.withNameSuggestion(ctx, err, ws, name)
			}
			return s.fail(req.ID, err, "register")
		}
	}

	s.touch(ctx, ws, name, "register")
	s.log.Printf("registered %s (harness=%s pid=%d created=%v renamed=%v)",
		a.Address(), harness, sessionPID, created, renamed)
	return protocol.OK(req.ID, protocol.RegisterResult{
		Address: a.Address(),
		Name:    name,
		Harness: harness,
		Created: created,
		Renamed: renamed,
	})
}

// mintFreeName synthesises a name from harness+session and finds a free
// variant, for a register with no chosen name and no existing registration
// to resolve to.
func (s *Server) mintFreeName(ctx context.Context, ws, harness, session string) (string, error) {
	base := mintName(harness, session)
	if free, err := s.isNameFree(ctx, ws, base); err != nil {
		return "", err
	} else if free {
		return base, nil
	}
	if alt := s.firstFreeSuffixed(ctx, ws, base); alt != "" {
		return alt, nil
	}
	return "", fmt.Errorf("could not find a free name derived from %s", base)
}

// withNameSuggestion enriches a name-conflict error with a free alternative
// (target-2, target-3, ...), so a rejected register has something concrete
// to try next.
func (s *Server) withNameSuggestion(ctx context.Context, err error, ws, target string) error {
	if alt := s.firstFreeSuffixed(ctx, ws, target); alt != "" {
		return fmt.Errorf("%w — try %s@%s", err, alt, ws)
	}
	return err
}

// isNameFree reports whether no agent in ws is named name.
func (s *Server) isNameFree(ctx context.Context, ws, name string) (bool, error) {
	if _, err := s.store.GetAgent(ctx, ws, name); errors.Is(err, store.ErrNoSuchAgent) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	return false, nil
}

// firstFreeSuffixed returns base-2, base-3, ... up to a small bound --
// whichever names no agent in ws yet -- or "" if none of them do.
func (s *Server) firstFreeSuffixed(ctx context.Context, ws, base string) string {
	for i := 2; i <= 20; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if free, err := s.isNameFree(ctx, ws, candidate); err == nil && free {
			return candidate
		}
	}
	return ""
}

// registerOrReclaim performs the guarded register, and when the name is held
// by a provably dead session, reclaims it via a compare-and-swap on the
// incumbent's exact pid/pid_start rather than a blind forced overwrite --
// closing the race where a third party reclaims or revives the row between
// the deadness check and the write (defect C5).
//
// The "did this row already exist" check is a separate read before the
// guarded upsert, so Created can race and misreport under a same-instant
// collision; it is cosmetic (only affects what the CLI prints), so this is
// an accepted ceiling — the claim itself stays a single guarded statement.
func (s *Server) registerOrReclaim(ctx context.Context, a store.Agent) (created bool, err error) {
	existedBefore := true
	if _, getErr := s.store.GetAgent(ctx, a.Workspace, a.Name); errors.Is(getErr, store.ErrNoSuchAgent) {
		existedBefore = false
	}

	staleCutoff := time.Now().Add(-s.cfg.StaleAfter)
	err = s.store.Register(ctx, a, staleCutoff)
	if err == nil {
		return !existedBefore, nil
	}
	if !errors.Is(err, store.ErrNameTaken) {
		return false, err
	}

	incumbent, getErr := s.store.GetAgent(ctx, a.Workspace, a.Name)
	if getErr != nil {
		return false, err // could not confirm the incumbent; report the original conflict
	}
	if proc.AliveAt(incumbent.PID, incumbent.PIDStart) {
		return false, err // still alive: today's behaviour, a genuine conflict
	}

	reclaimed, err := s.store.ReclaimAgent(ctx, a, incumbent.PID, incumbent.PIDStart)
	if err != nil {
		return false, err
	}
	if !reclaimed {
		// The row moved between our checks: someone else raced in. Report
		// the original conflict rather than silently doing nothing.
		return false, fmt.Errorf("%w: %s", store.ErrNameTaken, a.Address())
	}
	return false, nil // reclaiming an existing row, not creating one
}

// renameOrReclaim is Rename with the same dead-holder escape hatch as
// registerOrReclaim. The second step is a compare-and-swap against the exact
// dead identity observed, so an intervening live claimant is never evicted.
func (s *Server) renameOrReclaim(ctx context.Context, a store.Agent) error {
	staleCutoff := time.Now().Add(-s.cfg.StaleAfter)
	_, err := s.store.Rename(ctx, a, staleCutoff)
	if err == nil || !errors.Is(err, store.ErrNameTaken) {
		return err
	}

	incumbent, getErr := s.store.GetAgent(ctx, a.Workspace, a.Name)
	if getErr != nil {
		return err // could not confirm the incumbent; report the original conflict
	}
	if proc.AliveAt(incumbent.PID, incumbent.PIDStart) {
		return err // still alive: a genuine conflict
	}

	_, reclaimed, reclaimErr := s.store.ReclaimRename(ctx, a, incumbent)
	if reclaimErr != nil {
		return reclaimErr
	}
	if !reclaimed {
		return err
	}
	return nil
}

// authenticate checks that session matches ws/name's stored session exactly
// (both empty counts as a match); a missing agent is never authenticated.
//
// Same-uid forgery of the session id is still possible; the 0700 socket
// directory is the real trust boundary, not this comparison.
func (s *Server) authenticate(ctx context.Context, ws, name, session string) error {
	a, err := s.store.GetAgent(ctx, ws, name)
	if err != nil {
		return err
	}
	if a.SessionID == session {
		return nil
	}
	return fmt.Errorf("%w: acting as %s but a different session holds that name",
		store.ErrNameTaken, addr(name, ws))
}

// addr renders the canonical "name@workspace" form from separate strings.
func addr(name, ws string) string { return name + "@" + ws }

// touch refreshes last_seen and last_kind; errors are logged, not
// propagated, since the handler's real work already succeeded.
func (s *Server) touch(ctx context.Context, ws, name, kind string) {
	if _, err := s.store.Heartbeat(ctx, ws, name, kind); err != nil {
		s.log.Printf("touch: heartbeat %s@%s: %v", name, ws, err)
	}
}

// broadcastStar and broadcastAll mean "every other agent in the workspace".
// Both exist because a bare "*" glob-expands in the caller's shell; requireName
// refuses to let a real agent register under either.
const (
	broadcastStar = "*"
	broadcastAll  = "all"
)

func (s *Server) handleSend(ctx context.Context, req protocol.Request) protocol.Response {
	var p protocol.SendParams
	if err := decodeParams(req, &p); err != nil {
		return s.fail(req.ID, err, "send")
	}

	fromName, fromWS, err := requireAddress(p.FromName, p.FromWorkspace)
	if err != nil {
		return s.fail(req.ID, err, "send")
	}

	// Authenticate the sender, not the recipient: the recipient only receives.
	if err := s.authenticate(ctx, fromWS, fromName, p.FromSession); err != nil {
		return s.fail(req.ID, err, "send")
	}

	kind := strings.TrimSpace(p.Kind)
	if kind == "" {
		kind = store.KindNote
	}
	if !store.ValidKind(kind) {
		return protocol.Fail(req.ID, protocol.CodeBadRequest, "unknown kind: "+clip(kind))
	}
	replyTo := strings.TrimSpace(p.ReplyTo)

	toWS, err := requireWorkspace(p.ToWorkspace)
	if err != nil {
		return s.fail(req.ID, err, "send")
	}

	toNameRaw := strings.TrimSpace(p.ToName)
	if toNameRaw == broadcastStar || toNameRaw == broadcastAll {
		return s.handleBroadcastSend(ctx, req, fromName, fromWS, toWS, kind, p.Body, replyTo)
	}

	toName, err := requireName(p.ToName)
	if err != nil {
		return s.fail(req.ID, err, "send")
	}

	// Snapshot before sendOne, not after: sendOne's Notify releases a parked
	// wait as a side effect, and blocked is the single most useful state to
	// report -- computed after delivery, it would never be reachable.
	recipientState := s.recipientState(ctx, toWS, toName)

	id, err := s.sendOne(ctx, fromName, fromWS, toName, toWS, kind, p.Body, replyTo)
	if err != nil {
		if errors.Is(err, store.ErrNoSuchAgent) {
			err = s.withSuggestion(ctx, err, toWS, toName)
		}
		return s.fail(req.ID, err, "send")
	}

	s.touch(ctx, fromWS, fromName, "send")
	s.log.Printf("send %s -> %s (%s, id=%s)", addr(fromName, fromWS), addr(toName, toWS), kind, id)
	return protocol.OK(req.ID, protocol.SendResult{MessageID: id, RecipientState: recipientState})
}

// recipientState reports the recipient's computed state. A lookup failure
// (e.g. the recipient doesn't exist, which sendOne is about to reject too)
// just degrades to an empty state rather than failing the send.
func (s *Server) recipientState(ctx context.Context, ws, name string) string {
	a, err := s.store.GetAgent(ctx, ws, name)
	if err != nil {
		return ""
	}
	blocked := s.waiters.Count(a.Address()) > 0
	return computeState(a, blocked, time.Now()).State
}

// sendOne delivers one message and wakes any waiter parked on the recipient;
// broadcast is a loop over this, not a second implementation.
func (s *Server) sendOne(ctx context.Context, fromName, fromWS, toName, toWS, kind, body, replyTo string) (string, error) {
	m := store.Message{
		ReplyTo:  replyTo,
		FromName: fromName,
		FromWS:   fromWS,
		ToName:   toName,
		ToWS:     toWS,
		Kind:     kind,
		Body:     body,
	}
	id, err := s.store.Send(ctx, m)
	if err != nil {
		return "", err
	}
	s.waiters.Notify(m.To()) // only after a durable commit
	return id, nil
}

// handleBroadcastSend delivers to every other agent registered in toWS,
// dropped by name+workspace comparison. Zero recipients is success, not an error.
func (s *Server) handleBroadcastSend(ctx context.Context, req protocol.Request, fromName, fromWS, toWS, kind, body, replyTo string) protocol.Response {
	// Unfiltered: an idle-but-registered agent is still "everyone else in
	// the workspace" and must not be silently skipped, nor counted absent.
	agents, err := s.store.ListAgents(ctx, toWS, 0)
	if err != nil {
		return s.fail(req.ID, err, "send")
	}

	addrs := make([]string, 0, len(agents))
	eligible, failed := 0, 0
	for _, a := range agents {
		if a.Name == fromName && a.Workspace == fromWS {
			continue // never deliver to the sender
		}
		eligible++
		if _, err := s.sendOne(ctx, fromName, fromWS, a.Name, toWS, kind, body, replyTo); err != nil {
			s.log.Printf("broadcast send %s@%s -> %s@%s failed: %v", fromName, fromWS, a.Name, toWS, err)
			failed++
			continue
		}
		addrs = append(addrs, addr(a.Name, toWS))
	}

	// A non-empty recipient set that entirely failed must not read like an
	// empty workspace: Delivered: 0 means both today, indistinguishably.
	if eligible > 0 && len(addrs) == 0 {
		return s.fail(req.ID,
			badRequest("broadcast to %d recipient(s) in %s failed entirely", eligible, toWS), "send")
	}

	s.touch(ctx, fromWS, fromName, "send")
	s.log.Printf("broadcast send %s@%s -> */%s (%s, delivered=%d, failed=%d)",
		fromName, fromWS, toWS, kind, len(addrs), failed)
	return protocol.OK(req.ID, protocol.SendResult{Recipients: addrs, Delivered: len(addrs), Failed: failed})
}

// withSuggestion enriches a "no such agent" error with a did-you-mean hint.
func (s *Server) withSuggestion(ctx context.Context, err error, ws, target string) error {
	agents, listErr := s.store.ListAgents(ctx, ws, 0)
	if listErr != nil || len(agents) == 0 {
		return err
	}
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		names = append(names, a.Name)
	}
	if hint := suggest(target, names); hint != "" {
		return fmt.Errorf("%w — did you mean %s@%s?", err, hint, ws)
	}
	return err
}

// handleInbox serves Replay, Peek, and the default drain over one method.
func (s *Server) handleInbox(ctx context.Context, req protocol.Request) protocol.Response {
	var p protocol.InboxParams
	if err := decodeParams(req, &p); err != nil {
		return s.fail(req.ID, err, "inbox")
	}
	name, ws, err := requireAddress(p.Name, p.Workspace)
	if err != nil {
		return s.fail(req.ID, err, "inbox")
	}
	if err := s.authenticate(ctx, ws, name, p.Session); err != nil {
		return s.fail(req.ID, err, "inbox")
	}
	limit, err := clampLimit(p.Limit)
	if err != nil {
		return s.fail(req.ID, err, "inbox")
	}

	var (
		msgs    []store.Message
		cleared int
		dropped int
	)
	switch {
	case p.Replay:
		msgs, err = s.store.Replay(ctx, ws, name, limit, s.cfg.RetainMessages)
	case p.Peek:
		msgs, err = s.store.Inbox(ctx, ws, name, limit)
	default:
		msgs, dropped, err = s.store.Drain(ctx, ws, name, limit)
		cleared = len(msgs)
	}
	if err != nil {
		return s.fail(req.ID, err, "inbox")
	}

	pending, err := s.store.PendingCount(ctx, ws, name)
	if err != nil {
		return s.fail(req.ID, err, "inbox")
	}

	views := make([]protocol.MessageView, 0, len(msgs))
	for _, m := range msgs {
		views = append(views, messageView(m))
	}
	s.touch(ctx, ws, name, "inbox")
	return protocol.OK(req.ID, protocol.InboxResult{
		Messages: views,
		Cleared:  cleared,
		Pending:  pending,
		Dropped:  dropped,
	})
}

// handleWait subscribes before counting pending mail, closing the race where
// a send commits between the two.
func (s *Server) handleWait(ctx context.Context, req protocol.Request) protocol.Response {
	var p protocol.WaitParams
	if err := decodeParams(req, &p); err != nil {
		return s.fail(req.ID, err, "wait")
	}
	name, ws, err := requireAddress(p.Name, p.Workspace)
	if err != nil {
		return s.fail(req.ID, err, "wait")
	}
	if err := s.authenticate(ctx, ws, name, p.Session); err != nil {
		return s.fail(req.ID, err, "wait")
	}
	s.touch(ctx, ws, name, "wait")

	requested := s.cfg.DefaultWait
	if p.TimeoutMS > 0 {
		requested = time.Duration(p.TimeoutMS) * time.Millisecond
	}
	// capped marks a timeout bounded by the daemon's own ceiling, not a real
	// "nothing arrived" answer; the CLI re-issues wait when it sees this.
	timeout := requested
	capped := false
	if timeout > s.cfg.MaxWait {
		timeout = s.cfg.MaxWait
		capped = true
	}

	address := addr(name, ws)
	ch := s.waiters.Wait(address)
	defer s.waiters.Release(address)

	n, err := s.store.PendingCount(ctx, ws, name)
	if err != nil {
		return s.fail(req.ID, err, "wait")
	}
	if n > 0 {
		return protocol.OK(req.ID, protocol.WaitResult{Pending: n, TimedOut: false})
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ch:
		n, err = s.store.PendingCount(ctx, ws, name) // re-read; another connection may have acked it
		if err != nil {
			return s.fail(req.ID, err, "wait")
		}
		return protocol.OK(req.ID, protocol.WaitResult{Pending: n, TimedOut: false})
	case <-timer.C:
		return protocol.OK(req.ID, protocol.WaitResult{Pending: 0, TimedOut: true, Capped: capped})
	case <-ctx.Done():
		return protocol.OK(req.ID, protocol.WaitResult{Pending: 0, TimedOut: true, Capped: capped})
	}
}

// handleLs lists agents plus their computed state. ws is used exactly as
// sent; --all and --workspace compose rather than one overriding the other.
func (s *Server) handleLs(ctx context.Context, req protocol.Request) protocol.Response {
	var p protocol.LsParams
	if err := decodeParams(req, &p); err != nil {
		return s.fail(req.ID, err, "ls")
	}

	ws := strings.TrimSpace(p.Workspace)

	// An internal caller can request one agent by name.
	if p.Name != "" {
		name := strings.TrimSpace(p.Name)
		a, err := s.store.GetAgent(ctx, ws, name)
		if err != nil {
			return s.fail(req.ID, err, "ls")
		}
		pending, err := s.store.PendingCount(ctx, ws, name)
		if err != nil {
			return s.fail(req.ID, err, "ls")
		}
		blocked := s.waiters.Count(a.Address()) > 0
		sr := computeState(a, blocked, time.Now())
		return protocol.OK(req.ID, protocol.LsResult{Agents: []protocol.AgentView{agentView(a, sr, pending)}})
	}

	// Unfiltered: an agent stops appearing only when the
	// sweeper deletes it, not after StaleAfter -- otherwise gone (the state
	// you most want to see) becomes unreachable a few minutes after death.
	agents, err := s.store.ListAgents(ctx, ws, 0)
	if err != nil {
		return s.fail(req.ID, err, "ls")
	}

	pending, err := s.fleetPending(ctx, agents)
	if err != nil {
		return s.fail(req.ID, err, "ls")
	}

	now := time.Now()
	views := make([]protocol.AgentView, 0, len(agents))
	for _, a := range agents {
		blocked := s.waiters.Count(a.Address()) > 0
		sr := computeState(a, blocked, now)
		views = append(views, agentView(a, sr, pending[a.Address()]))
	}
	return protocol.OK(req.ID, protocol.LsResult{Agents: views})
}

// fleetPending gathers pending mail counts for agents, one query per
// distinct workspace, keyed by full address.
func (s *Server) fleetPending(ctx context.Context, agents []store.Agent) (map[string]int, error) {
	pending := make(map[string]int, len(agents))

	seen := make(map[string]bool)
	for _, a := range agents {
		if seen[a.Workspace] {
			continue
		}
		seen[a.Workspace] = true

		wsPending, err := s.store.PendingByWorkspace(ctx, a.Workspace)
		if err != nil {
			return nil, err
		}
		for name, n := range wsPending {
			pending[addr(name, a.Workspace)] = n
		}
	}
	return pending, nil
}

// handleClaim acquires, renews, or reclaims a workspace/key claim for the
// calling process.
func (s *Server) handleClaim(ctx context.Context, req protocol.Request, peerPID int) protocol.Response {
	var p protocol.ClaimParams
	if err := decodeParams(req, &p); err != nil {
		return s.fail(req.ID, err, "claim")
	}
	ws, err := requireWorkspace(p.Workspace)
	if err != nil {
		return s.fail(req.ID, err, "claim")
	}
	key, err := requireClaimKey(p.Key)
	if err != nil {
		return s.fail(req.ID, err, "claim")
	}

	// ownerPID is the claim's real trust boundary -- who this daemon will
	// later treat as "the live process to reclaim from once it dies" -- so
	// it gets the same peer-pid cross-check register's session pid does
	// (finding 6.3): a claimed pid unrelated to this connection would let
	// one process CAS-lock a key against a pid it does not actually control.
	ownerPID := p.OwnerPID
	switch {
	case peerPID <= 0:
		// no signal to check against; fall through unchanged
	case ownerPID <= 0:
		ownerPID = peerPID
	case ownerPID == peerPID, proc.SameSession(ownerPID, peerPID):
		// claimed pid is the connecting process, or shares its session
	default:
		return s.fail(req.ID, badRequest(
			"pid %d is not this connection's process or in its session", ownerPID), "claim")
	}

	if ownerPID <= 0 || !proc.Alive(ownerPID) {
		return s.fail(req.ID, badRequest("owner pid %d is not alive", ownerPID), "claim")
	}
	pidStart, _ := proc.StartTime(ownerPID) // 0 on failure is proc.AliveAt's "unknown"
	holder := clip(strings.TrimSpace(p.Holder))

	c, renewed, reclaimed, err := s.claimOrReclaim(ctx, ws, key, ownerPID, pidStart, holder)
	if err != nil {
		return s.fail(req.ID, err, "claim")
	}

	s.log.Printf("claim %s/%s (pid=%d renewed=%v reclaimed=%v)", ws, key, ownerPID, renewed, reclaimed)
	return protocol.OK(req.ID, protocol.ClaimResult{
		LeaseID: c.LeaseID, Holder: c.LeaseHolder,
		ExpiresAt: formatTime(c.ExpiresAt), Renewed: renewed, Reclaimed: reclaimed,
	})
}

// claimOrReclaim performs the guarded claim, and when the key is held by a
// provably dead owner, reclaims it via a compare-and-swap on the incumbent's
// exact owner_pid/owner_pid_start rather than a blind overwrite -- the same
// shape registerOrReclaim uses for agent names. renewed reports whether this
// call extended the caller's own still-live claim, a cosmetic best-effort
// read like registerOrReclaim's "created" (it can race under a same-instant
// collision; it only affects what the CLI prints).
func (s *Server) claimOrReclaim(ctx context.Context, ws, key string, ownerPID int, ownerPIDStart int64, holder string) (c store.Claim, renewed, reclaimed bool, err error) {
	incumbent, getErr := s.store.GetClaim(ctx, ws, key)
	existed := getErr == nil

	c, err = s.store.Claim(ctx, ws, key, ownerPID, ownerPIDStart, holder, s.cfg.ClaimTTL)
	if err == nil {
		renewed = existed && incumbent.OwnerPID == ownerPID && incumbent.OwnerPIDStart == ownerPIDStart
		return c, renewed, false, nil
	}
	if !errors.Is(err, store.ErrClaimHeld) {
		return store.Claim{}, false, false, err
	}

	// Re-read: the conflict above may be a claim that expired between our
	// first read and the guarded claim attempt.
	incumbent, getErr = s.store.GetClaim(ctx, ws, key)
	if getErr != nil {
		return store.Claim{}, false, false, err // could not confirm the incumbent; report the original conflict
	}
	if proc.AliveAt(incumbent.OwnerPID, incumbent.OwnerPIDStart) {
		return store.Claim{}, false, false, err // still alive: a genuine conflict
	}

	c, ok, rErr := s.store.ReclaimClaim(ctx, ws, key,
		incumbent.OwnerPID, incumbent.OwnerPIDStart, ownerPID, ownerPIDStart, holder, s.cfg.ClaimTTL)
	if rErr != nil {
		return store.Claim{}, false, false, rErr
	}
	if !ok {
		// The row moved between our checks: someone else raced in. Report
		// the original conflict rather than silently doing nothing.
		return store.Claim{}, false, false, fmt.Errorf("%w: %s/%s", store.ErrClaimHeld, ws, key)
	}
	return c, false, true, nil
}

// handleRelease releases a claim the caller holds, verified by lease id in
// one compare-and-swap statement inside Store.Release -- not a check then a
// separate act.
func (s *Server) handleRelease(ctx context.Context, req protocol.Request) protocol.Response {
	var p protocol.ReleaseParams
	if err := decodeParams(req, &p); err != nil {
		return s.fail(req.ID, err, "release")
	}
	ws, err := requireWorkspace(p.Workspace)
	if err != nil {
		return s.fail(req.ID, err, "release")
	}
	key, err := requireClaimKey(p.Key)
	if err != nil {
		return s.fail(req.ID, err, "release")
	}
	leaseID := strings.TrimSpace(p.LeaseID)
	if leaseID == "" {
		return s.fail(req.ID, badRequest("lease id is required"), "release")
	}

	if err := s.store.Release(ctx, ws, key, leaseID); err != nil {
		return s.fail(req.ID, err, "release")
	}
	s.log.Printf("release %s/%s", ws, key)
	return protocol.OK(req.ID, protocol.ReleaseResult{})
}

// handleClaims lists claims, each with a freshly computed status. The caller
// sees the lease id for claims it owns (same pid) so it can release them.
func (s *Server) handleClaims(ctx context.Context, req protocol.Request, peerPID int) protocol.Response {
	var p protocol.ClaimsParams
	if err := decodeParams(req, &p); err != nil {
		return s.fail(req.ID, err, "claims")
	}
	ws := strings.TrimSpace(p.Workspace)

	claims, err := s.store.ListClaims(ctx, ws)
	if err != nil {
		return s.fail(req.ID, err, "claims")
	}

	now := time.Now()
	views := make([]protocol.ClaimView, 0, len(claims))
	for _, c := range claims {
		views = append(views, claimView(c, now, peerPID))
	}
	return protocol.OK(req.ID, protocol.ClaimsResult{Claims: views})
}
