// Package protocol defines the wire format and socket transport for tether:
// newline-delimited JSON Request/Response objects over a unix socket.
package protocol

import (
	"encoding/json"
	"fmt"
)

// Error codes carried in Error.Code. They mirror the HTTP status codes with
// the closest meaning so they read the same on both sides of the socket.
const (
	// CodeBadRequest means the request was malformed or failed validation.
	CodeBadRequest = 400
	// CodeNotFound means the addressed agent or message does not exist.
	CodeNotFound = 404
	// CodeConflict means the request collided with existing state, such as
	// registering a name that is already taken in the workspace.
	CodeConflict = 409
	// CodeTooLarge means the request or message body exceeded the limit.
	CodeTooLarge = 413
	// CodeInternal means the daemon failed for a reason the caller cannot fix.
	CodeInternal = 500
)

// Method names. No unregister or heartbeat: every command implicitly
// re-registers, and a dead-agent sweep replaces explicit unregister.
const (
	MethodRegister = "register"
	MethodSend     = "send"
	MethodInbox    = "inbox"
	MethodWait     = "wait"
	MethodLs       = "ls"

	// MethodClaim acquires, renews, or reclaims a workspace/key claim in one
	// request; the daemon decides which happened.
	MethodClaim   = "claim"
	MethodRelease = "release"
	MethodClaims  = "claims"
)

// Request represents an incoming command from an agent. Params stays raw so
// the daemon can dispatch on Method before decoding the payload.
type Request struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Error maps closely to the CLI exit codes.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Error implements the error interface so an *Error carried back over the wire
// can be handed to callers as a normal Go error.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%d: %s", e.Code, e.Message)
}

// Response is sent back to the agent. At most one of Result or Error is set.
type Response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// OK builds a success Response carrying the marshalled result. If result
// cannot be marshalled, OK degrades to a CodeInternal failure rather than
// panicking or silently dropping the response.
func OK(id string, result any) Response {
	if result == nil {
		return Response{ID: id}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return Fail(id, CodeInternal, fmt.Sprintf("marshal result: %v", err))
	}
	return Response{ID: id, Result: raw}
}

// Fail builds an error Response. Result is left empty.
func Fail(id string, code int, msg string) Response {
	return Response{ID: id, Error: &Error{Code: code, Message: msg}}
}
