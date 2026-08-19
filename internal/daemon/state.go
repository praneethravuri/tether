package daemon

import (
	"fmt"
	"time"

	"github.com/praneethravuri/tether/internal/proc"
	"github.com/praneethravuri/tether/internal/store"
)

// stateReport is the computed, never-persisted answer for "what is this agent doing?"
type stateReport struct {
	State  string        // gone | blocked | working | quiet | unknown
	Source string        // pid | wait | heartbeat | registration
	Age    time.Duration // age of the evidence, not of the agent itself
	Detail string        // evidence included in the agent JSON result
}

// workingWindow is how recently an agent must have run a tether command to
// read as working rather than quiet.
const workingWindow = 60 * time.Second

// computeState answers in strict priority: gone > blocked > working > quiet > unknown.
// blocked comes from the live wait registry, not agents.last_seen, since a
// parked wait can't outlive the process holding it.
func computeState(a store.Agent, blocked bool, now time.Time) stateReport {
	if a.PID > 0 && !proc.AliveAt(a.PID, a.PIDStart) {
		return stateReport{State: "gone", Source: "pid", Age: now.Sub(a.LastSeen),
			Detail: fmt.Sprintf("pid %d is gone", a.PID)}
	}
	if blocked {
		return stateReport{State: "blocked", Source: "wait", Age: 0,
			Detail: "parked in tether wait"}
	}
	if a.LastKind != "" {
		age := now.Sub(a.LastSeen)
		state, verb := "working", "ran"
		if age >= workingWindow {
			state, verb = "quiet", "last ran"
		}
		detail := fmt.Sprintf("%s tether %s", verb, a.LastKind)
		return stateReport{State: state, Source: "heartbeat", Age: age, Detail: detail}
	}
	return stateReport{State: "unknown", Source: "registration", Age: now.Sub(a.RegisteredAt),
		Detail: "registered; nothing observed since"}
}

// claimStatus answers "is this claim still in force?" in strict priority:
// gone (owner process no longer alive) beats expired (TTL elapsed) beats
// held. Mirrors computeState's gone check via the same proc.AliveAt
// primitive rather than a second liveness concept.
func claimStatus(c store.Claim, now time.Time) string {
	if !proc.AliveAt(c.OwnerPID, c.OwnerPIDStart) {
		return "gone"
	}
	if !c.ExpiresAt.After(now) {
		return "expired"
	}
	return "held"
}
