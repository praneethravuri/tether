package main

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/praneethravuri/tether/internal/protocol"
)

func TestCallSendsTheRequestAndDecodesTheResult(t *testing.T) {
	d := newFakeDaemon(t, func(req protocol.Request) protocol.Response {
		if req.ID == "" {
			return protocol.Fail(req.ID, protocol.CodeBadRequest, "empty request id")
		}
		return protocol.OK(req.ID, protocol.SendResult{MessageID: "01KROUTED"})
	})

	var res protocol.SendResult
	if err := call(protocol.MethodSend,
		protocol.SendParams{FromName: "frontend", FromWorkspace: "storefront", ToName: "backend", Body: "hi"},
		&res); err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.MessageID != "01KROUTED" {
		t.Fatalf("MessageID = %q, want 01KROUTED", res.MessageID)
	}

	got := d.only(t, protocol.MethodSend)
	params := decodeParams[protocol.SendParams](t, got)
	if params.FromName != "frontend" || params.ToName != "backend" {
		t.Fatalf("params = %+v, want from frontend to backend", params)
	}
}

func TestCallWithoutADaemonExplainsHowToStartOne(t *testing.T) {
	noDaemon(t)

	err := call(protocol.MethodLs, protocol.LsParams{}, nil)
	if err == nil {
		t.Fatal("call succeeded with no daemon listening")
	}
	if got := exitCodeFor(err); got != exitNoDaemon {
		t.Fatalf("exit code = %d, want %d", got, exitNoDaemon)
	}
	requireContains(t, err.Error(), "no daemon running", "error")
	requireContains(t, err.Error(), "tether", "error")
}

func TestCallSurfacesDaemonErrorsWithTheirCode(t *testing.T) {
	newFakeDaemon(t, errHandler(protocol.CodeNotFound, "no agent named ghost"))

	err := call(protocol.MethodLs, protocol.LsParams{Name: "ghost"}, nil)
	if err == nil {
		t.Fatal("call succeeded against an erroring daemon")
	}

	var pe *protocol.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error %v (%T) is not a *protocol.Error", err, err)
	}
	if pe.Code != protocol.CodeNotFound {
		t.Fatalf("code = %d, want %d", pe.Code, protocol.CodeNotFound)
	}
	if got := exitCodeFor(err); got != exitGeneral {
		t.Fatalf("exit code = %d, want %d", got, exitGeneral)
	}
}

func TestCallMapsConflictToExitFive(t *testing.T) {
	newFakeDaemon(t, errHandler(protocol.CodeConflict, "name taken"))

	err := call(protocol.MethodRegister, protocol.RegisterParams{}, nil)
	if got := exitCodeFor(err); got != exitConflict {
		t.Fatalf("exit code = %d, want %d", got, exitConflict)
	}
}

// A daemon that speaks nonsense must produce an explanation, not a panic and
// not a stack trace.
func TestCallOnMalformedResponses(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		want string
	}{
		{name: "garbage", raw: []byte("this is not json\n"), want: "malformed response"},
		{name: "truncated json", raw: []byte(`{"id":"x","result":`), want: "malformed response"},
		{name: "html error page", raw: []byte("<html><body>502</body></html>\n"), want: "malformed response"},
		{name: "empty line", raw: []byte("\n"), want: "malformed response"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			newRawDaemon(t, tc.raw)

			var res protocol.LsResult
			err := call(protocol.MethodLs, protocol.LsParams{}, &res)
			if err == nil {
				t.Fatal("call succeeded against a daemon speaking garbage")
			}
			requireContains(t, err.Error(), tc.want, "error")
			if got := exitCodeFor(err); got != exitGeneral {
				t.Fatalf("exit code = %d, want %d", got, exitGeneral)
			}
		})
	}
}

func TestCallWhenTheDaemonHangsUp(t *testing.T) {
	newSilentDaemon(t)

	err := call(protocol.MethodLs, protocol.LsParams{}, nil)
	if err == nil {
		t.Fatal("call succeeded against a daemon that never answered")
	}
	requireContains(t, err.Error(), "closed the connection", "error")
	if got := exitCodeFor(err); got != exitGeneral {
		t.Fatalf("exit code = %d, want %d", got, exitGeneral)
	}
}

// A result whose shape this CLI cannot read is reported, not silently ignored.
func TestCallOnAnUnreadableResult(t *testing.T) {
	newFakeDaemon(t, func(req protocol.Request) protocol.Response {
		return protocol.Response{ID: req.ID, Result: json.RawMessage(`"a string, not an object"`)}
	})

	var res protocol.LsResult
	err := call(protocol.MethodLs, protocol.LsParams{}, &res)
	if err == nil {
		t.Fatal("call accepted a result of the wrong shape")
	}
	requireContains(t, err.Error(), "cannot read", "error")
}

// A nil result argument, and an empty result body, are both fine.
func TestCallIsNilSafe(t *testing.T) {
	newFakeDaemon(t, func(req protocol.Request) protocol.Response {
		return protocol.Response{ID: req.ID}
	})

	if err := call(protocol.MethodLs, nil, nil); err != nil {
		t.Fatalf("call with nil params and nil result: %v", err)
	}

	var res protocol.LsResult
	if err := call(protocol.MethodLs, protocol.LsParams{}, &res); err != nil {
		t.Fatalf("call with an empty result body: %v", err)
	}
	if len(res.Agents) != 0 {
		t.Fatalf("agents = %+v, want none", res.Agents)
	}
}

// -- ensureRegistered ---------------------------------------------------------

func TestEnsureRegisteredFiresARegisterCall(t *testing.T) {
	d := newFakeDaemon(t, okHandler(protocol.RegisterResult{Name: "frontend", Created: true}))

	got, session, err := ensureRegistered("frontend", "storefront")
	if err != nil {
		t.Fatalf("ensureRegistered: %v", err)
	}
	if got != "frontend" {
		t.Fatalf("ensureRegistered returned %q, want frontend", got)
	}
	if session == "" {
		t.Fatal("ensureRegistered returned an empty session")
	}

	params := decodeParams[protocol.RegisterParams](t, d.only(t, protocol.MethodRegister))
	if params.Name != "frontend" || params.Workspace != "storefront" {
		t.Fatalf("registered %s@%s, want frontend@storefront", params.Name, params.Workspace)
	}
}

// TestEnsureRegisteredResolvesAnEmptyName proves an empty name is sent as-is
// (letting the daemon resolve or mint one) and the daemon's answer is what
// the caller gets back.
func TestEnsureRegisteredResolvesAnEmptyName(t *testing.T) {
	d := newFakeDaemon(t, okHandler(protocol.RegisterResult{Name: "claude-code-3f1a", Created: true}))

	got, session, err := ensureRegistered("", "storefront")
	if err != nil {
		t.Fatalf("ensureRegistered: %v", err)
	}
	if got != "claude-code-3f1a" {
		t.Fatalf("ensureRegistered returned %q, want the daemon-resolved name", got)
	}
	if session == "" {
		t.Fatal("ensureRegistered returned an empty session")
	}

	params := decodeParams[protocol.RegisterParams](t, d.only(t, protocol.MethodRegister))
	if params.Name != "" {
		t.Fatalf("register params carried Name %q, want empty (resolve-or-mint)", params.Name)
	}
}

// TestEnsureRegisteredSurfacesConflict is P1: a name conflict discovered at
// this implicit registration step must not be silently swallowed like every
// other failure here is -- continuing would mean the caller's real request
// is about to be authenticated against a name it does not hold.
func TestEnsureRegisteredSurfacesConflict(t *testing.T) {
	newFakeDaemon(t, errHandler(protocol.CodeConflict, "taken"))

	_, _, err := ensureRegistered("frontend", "storefront")
	if err == nil {
		t.Fatal("ensureRegistered succeeded despite a conflict")
	}
	if got := exitCodeFor(err); got != exitConflict {
		t.Fatalf("exit code = %d, want %d", got, exitConflict)
	}
}

// TestEnsureRegisteredSwallowsNonConflictFailures is the flip side: a
// failure that is not a name conflict (here, no daemon at all) must not
// surface here -- the caller's real request, made immediately afterwards,
// hits the exact same failure and reports it with a message specific to
// what was actually asked for.
func TestEnsureRegisteredSwallowsNonConflictFailures(t *testing.T) {
	noDaemon(t)
	got, session, err := ensureRegistered("frontend", "storefront")
	if err != nil {
		t.Fatalf("ensureRegistered should swallow a no-daemon failure, got: %v", err)
	}
	if got != "frontend" {
		t.Fatalf("ensureRegistered returned %q, want the original name unchanged", got)
	}
	if session == "" {
		t.Fatal("ensureRegistered returned an empty session")
	}
}

func TestWaitCallTimeoutLeavesRoomForTheWait(t *testing.T) {
	if got := waitCallTimeout(5 * time.Minute); got <= 5*time.Minute {
		t.Fatalf("waitCallTimeout(5m) = %s, want more than the wait itself", got)
	}
	if got := waitCallTimeout(0); got != defaultCallTimeout {
		t.Fatalf("waitCallTimeout(0) = %s, want %s", got, defaultCallTimeout)
	}
}
