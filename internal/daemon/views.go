package daemon

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/praneethravuri/tether/internal/protocol"
	"github.com/praneethravuri/tether/internal/store"
)

// fail turns any error into a Response. Store sentinels map to protocol
// codes; anything else is logged in full and reported as an opaque error.
func (s *Server) fail(id string, err error, op string) protocol.Response {
	var pe *protocol.Error
	switch {
	case errors.As(err, &pe):
		return protocol.Fail(id, pe.Code, clip(pe.Message))
	case errors.Is(err, store.ErrNameTaken):
		return protocol.Fail(id, protocol.CodeConflict, publicMessage(err))
	case errors.Is(err, store.ErrNoSuchAgent):
		return protocol.Fail(id, protocol.CodeNotFound, publicMessage(err))
	case errors.Is(err, store.ErrNoSuchMessage):
		// Deliberately not CodeNotFound: a bad --reply-to means the request
		// was malformed, not that "nobody was there" the way a missing
		// recipient does, and the two must not share an exit code.
		return protocol.Fail(id, protocol.CodeBadRequest, publicMessage(err))
	case errors.Is(err, store.ErrBodyTooLarge):
		return protocol.Fail(id, protocol.CodeTooLarge, publicMessage(err))
	case errors.Is(err, store.ErrEmptyBody), errors.Is(err, store.ErrBadAddress):
		return protocol.Fail(id, protocol.CodeBadRequest, publicMessage(err))
	case errors.Is(err, store.ErrClaimHeld), errors.Is(err, store.ErrClaimMismatch):
		return protocol.Fail(id, protocol.CodeConflict, publicMessage(err))
	case errors.Is(err, store.ErrNoSuchClaim):
		return protocol.Fail(id, protocol.CodeNotFound, publicMessage(err))
	default:
		ref := s.nextRef()
		s.log.Printf("%s failed [%s]: %v", op, ref, err)
		return protocol.Fail(id, protocol.CodeInternal, "internal error (ref "+ref+")")
	}
}

// nextRef mints a short reference shared between the log line and the client.
func (s *Server) nextRef() string {
	return fmt.Sprintf("e%04d", s.errSeq.Add(1))
}

// publicMessage renders a sentinel error for a client, stripping the "store: " prefix.
func publicMessage(err error) string {
	return clip(strings.TrimPrefix(err.Error(), "store: "))
}

// formatTime renders a store time as RFC3339 in UTC. The zero time renders as
// the empty string rather than year 1.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := formatTime(*t)
	return &s
}

func messageView(m store.Message) protocol.MessageView {
	return protocol.MessageView{
		ID:          m.ID,
		ThreadID:    m.ThreadID,
		ReplyTo:     m.ReplyTo,
		From:        m.From(),
		To:          m.To(),
		Kind:        m.Kind,
		Body:        m.Body,
		CreatedAt:   formatTime(m.CreatedAt),
		DeliveredAt: formatTimePtr(m.DeliveredAt),
		AckedAt:     formatTimePtr(m.AckedAt),
	}
}

// agentView renders one agent plus its freshly computed state for the wire.
func agentView(a store.Agent, sr stateReport, pending int) protocol.AgentView {
	return protocol.AgentView{
		Address:      a.Address(),
		Name:         a.Name,
		Workspace:    a.Workspace,
		Harness:      a.Harness,
		State:        sr.State,
		StateSource:  sr.Source,
		StateAgeMS:   sr.Age.Milliseconds(),
		StateDetail:  sr.Detail,
		Cwd:          a.Cwd,
		PID:          a.PID,
		Pending:      pending,
		Dropped:      a.Dropped,
		RegisteredAt: formatTime(a.RegisteredAt),
		LastSeen:     formatTime(a.LastSeen),
	}
}

// claimView renders one claim plus its freshly computed status for the wire.
// LeaseID is included only when the caller is the claim's owner (same pid),
// since a lease id is a release capability.
func claimView(c store.Claim, now time.Time, callerPID int) protocol.ClaimView {
	v := protocol.ClaimView{
		Workspace: c.Workspace,
		Key:       c.Key,
		OwnerPID:  c.OwnerPID,
		Holder:    c.LeaseHolder,
		Status:    claimStatus(c, now),
		LeasedAt:  formatTime(c.LeasedAt),
		ExpiresAt: formatTime(c.ExpiresAt),
	}
	if callerPID > 0 && callerPID == c.OwnerPID {
		v.LeaseID = c.LeaseID
	}
	return v
}
