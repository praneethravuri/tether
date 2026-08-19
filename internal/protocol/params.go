package protocol

// RegisterParams is the payload for MethodRegister.
type RegisterParams struct {
	Name      string `json:"name"`
	Workspace string `json:"workspace"`
	Harness   string `json:"harness"`
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	// PID is the session process (shell/harness), not the short-lived CLI call.
	PID int `json:"pid"`
}

// RegisterResult is the result for MethodRegister.
type RegisterResult struct {
	Address string `json:"address"`
	// Name is the resolved name: what was asked for, what the session
	// already held (an empty-Name register resolves to this), or a minted
	// name (a register with no session history and no chosen name).
	Name    string `json:"name"`
	Harness string `json:"harness"`
	// Created is false when this call refreshed, renamed, or reclaimed an
	// existing agent.
	Created bool `json:"created"`
	// Renamed is true when this call changed the session's existing name
	// to Name, moving its pending mail along with it.
	Renamed bool `json:"renamed,omitempty"`
}

// SendParams is the payload for MethodSend.
type SendParams struct {
	FromName      string `json:"from_name"`
	FromWorkspace string `json:"from_workspace"`
	// FromSession authenticates the sender against the name it registered with.
	FromSession string `json:"from_session"`
	ToName      string `json:"to_name"`
	ToWorkspace string `json:"to_workspace"`
	Kind        string `json:"kind"`
	Body        string `json:"body"`
	ReplyTo     string `json:"reply_to"`
}

// SendResult is the result for MethodSend. A unicast send sets MessageID and
// RecipientState; a broadcast sets Recipients and Delivered instead -- there
// is no single recipient state to report for a broadcast.
type SendResult struct {
	MessageID      string   `json:"message_id,omitempty"`
	RecipientState string   `json:"recipient_state,omitempty"`
	Recipients     []string `json:"recipients,omitempty"`
	Delivered      int      `json:"delivered,omitempty"`
	// Failed is how many of a broadcast's eligible recipients could not be
	// delivered to. A broadcast with every eligible recipient failing does
	// not reach this field -- it fails the request outright instead of
	// reporting an indistinguishable-from-empty Delivered: 0.
	Failed int `json:"failed,omitempty"`
}

// InboxParams is the payload for MethodInbox.
type InboxParams struct {
	Name      string `json:"name"`
	Workspace string `json:"workspace"`
	Session   string `json:"session"`
	Limit     int    `json:"limit"`
	// Peek is non-destructive; Replay shows already-delivered history. Mutually
	// exclusive at the CLI validation layer.
	Peek   bool `json:"peek"`
	Replay bool `json:"replay"`
}

// MessageView is the JSON envelope an agent sees. Times are RFC3339 strings.
type MessageView struct {
	ID          string  `json:"id"`
	ThreadID    string  `json:"thread_id"`
	ReplyTo     string  `json:"reply_to"`
	From        string  `json:"from"`
	To          string  `json:"to"`
	Kind        string  `json:"kind"`
	Body        string  `json:"body"`
	CreatedAt   string  `json:"created_at"`
	DeliveredAt *string `json:"delivered_at,omitempty"`
	AckedAt     *string `json:"acked_at,omitempty"`
}

// InboxResult is the result for MethodInbox. Cleared and Dropped are always
// 0 for a peek or replay; only a real drain acks or resets the counter.
type InboxResult struct {
	Messages []MessageView `json:"messages"`
	Cleared  int           `json:"cleared"`
	Pending  int           `json:"pending"`
	Dropped  int           `json:"dropped"`
}

// WaitParams is the payload for MethodWait.
type WaitParams struct {
	Name      string `json:"name"`
	Workspace string `json:"workspace"`
	Session   string `json:"session"`
	TimeoutMS int    `json:"timeout_ms"`
}

// WaitResult is the result for MethodWait. Capped means the daemon's own
// wait ceiling elapsed, not a genuine timeout; the CLI re-issues wait
// transparently when it sees this.
type WaitResult struct {
	Pending  int  `json:"pending"`
	TimedOut bool `json:"timed_out"`
	Capped   bool `json:"capped,omitempty"`
}

// LsParams is the payload for MethodLs. Workspace scopes the result; an empty
// workspace returns agents from every workspace. Name is reserved for an
// internal single-agent lookup.
type LsParams struct {
	Workspace string `json:"workspace"`
	Name      string `json:"name,omitempty"`
}

// AgentView describes a registered agent. State/StateSource/StateAgeMS/
// StateDetail are computed fresh on every query, never persisted.
type AgentView struct {
	Address      string `json:"address"`
	Name         string `json:"name"`
	Workspace    string `json:"workspace"`
	Harness      string `json:"harness"`
	State        string `json:"state"`
	StateSource  string `json:"state_source"`
	StateAgeMS   int64  `json:"state_age_ms"`
	StateDetail  string `json:"state_detail"`
	Cwd          string `json:"cwd"`
	PID          int    `json:"pid"`
	Pending      int    `json:"pending"`
	Dropped      int    `json:"dropped"`
	RegisteredAt string `json:"registered_at"`
	LastSeen     string `json:"last_seen"`
}

// LsResult is the result for MethodLs.
type LsResult struct {
	Agents []AgentView `json:"agents"`
}

// ClaimParams is the payload for MethodClaim. Acquiring a free key, renewing
// the caller's own still-live claim, and reclaiming one whose owner is
// provably dead are all the same request -- the daemon decides which
// happened and reports it on the result.
type ClaimParams struct {
	Workspace string `json:"workspace"`
	Key       string `json:"key"`
	// OwnerPID is the live process holding this claim (the session, not the
	// short-lived CLI call), paired server-side with its start time so a
	// recycled pid is never mistaken for the original holder.
	OwnerPID int `json:"owner_pid"`
	// Holder is a free-text label shown by tether claims. It is purely
	// diagnostic and never checked by release's logic.
	Holder string `json:"holder,omitempty"`
}

// ClaimResult is the result for MethodClaim. LeaseID is freshly minted on
// every call, including a renewal of the caller's own claim; pass it to
// MethodRelease's LeaseID to release it. Workspace/Key are not echoed back --
// unlike RegisterResult's Name, they never differ from what was requested.
type ClaimResult struct {
	LeaseID   string `json:"lease_id"`
	Holder    string `json:"holder,omitempty"`
	ExpiresAt string `json:"expires_at"`
	// Renewed is true when this call extended the caller's own still-live claim.
	Renewed bool `json:"renewed,omitempty"`
	// Reclaimed is true when this call took over a claim whose previous
	// owner process is provably dead.
	Reclaimed bool `json:"reclaimed,omitempty"`
}

// ReleaseParams is the payload for MethodRelease. LeaseID must match the
// claim's current lease exactly; a stale id from an earlier acquisition of
// the same key is rejected, not silently ignored.
type ReleaseParams struct {
	Workspace string `json:"workspace"`
	Key       string `json:"key"`
	LeaseID   string `json:"lease_id"`
}

// ReleaseResult is the result for MethodRelease: an empty object acking
// success, matching WaitResult/InboxResult's lead of not echoing back
// request-supplied identity that never changes.
type ReleaseResult struct{}

// ClaimsParams is the payload for MethodClaims. An empty Workspace lists
// every workspace, matching LsParams.
type ClaimsParams struct {
	Workspace string `json:"workspace"`
}

// ClaimView describes one claim. Status is computed fresh on every query,
// never persisted, mirroring AgentView's State. LeaseID is returned so the
// holder can release it; empty for claims the caller does not own.
type ClaimView struct {
	Workspace string `json:"workspace"`
	Key       string `json:"key"`
	OwnerPID  int    `json:"owner_pid"`
	Holder    string `json:"holder,omitempty"`
	LeaseID   string `json:"lease_id,omitempty"`
	// Status is one of held (owner alive, TTL not elapsed), expired (TTL
	// elapsed), or gone (owner process no longer alive).
	Status    string `json:"status"`
	LeasedAt  string `json:"leased_at"`
	ExpiresAt string `json:"expires_at"`
}

// ClaimsResult is the result for MethodClaims.
type ClaimsResult struct {
	Claims []ClaimView `json:"claims"`
}
