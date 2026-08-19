package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/praneethravuri/tether/internal/protocol"
	"github.com/praneethravuri/tether/internal/sanitize"
	"github.com/praneethravuri/tether/internal/store"
)

// stripControl replaces C0 control bytes and DEL with a space, so
// client-controlled metadata can't carry a terminal escape into another
// agent's screen. Deliberately unbounded in length, unlike clip.
func stripControl(s string) string {
	return sanitize.Replace(s, ' ', false)
}

// clip bounds and sanitises a client-visible string.
func clip(s string) string {
	s = stripControl(s)
	if len(s) > maxClientMsgLen {
		return s[:maxClientMsgLen] + "..."
	}
	return s
}

// hasControlBytes reports whether s contains a C0 control code or DEL.
func hasControlBytes(s string) bool {
	return sanitize.HasControlBytes(s)
}

// badRequest builds a 400 as an error value so handlers can return it through
// the same fail() path as store errors.
func badRequest(format string, args ...any) error {
	return &protocol.Error{Code: protocol.CodeBadRequest, Message: fmt.Sprintf(format, args...)}
}

// decodeParams decodes req.Params into dst, rejecting any unrecognised field
// rather than silently dropping it. An absent params block is valid; the
// handler's own validation rejects an empty dst.
func decodeParams(req protocol.Request, dst any) error {
	if len(req.Params) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(req.Params))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return badRequest("invalid params for %s: %v", clip(req.Method), err)
	}
	return nil
}

// requireAddress validates and normalises a name/workspace pair.
func requireAddress(name, ws string) (string, string, error) {
	name, err := requireName(name)
	if err != nil {
		return "", "", err
	}
	ws, err = requireWorkspace(ws)
	if err != nil {
		return "", "", err
	}
	return name, ws, nil
}

// MaxNameLength keeps a name short enough to stay readable in the ls table
// and in a name@workspace address.
const MaxNameLength = 32

// requireName validates and normalises one agent name: no "@" (ambiguous
// with "name@workspace"), no control bytes, not too long, and "*"/"all" are
// reserved for broadcast addressing.
func requireName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", badRequest("name is required")
	}
	if strings.ContainsRune(name, '@') {
		return "", badRequest("name must not contain '@'")
	}
	if hasControlBytes(name) {
		return "", badRequest("name must not contain control characters")
	}
	if len(name) > MaxNameLength {
		return "", badRequest("name must be at most %d characters", MaxNameLength)
	}
	if name == broadcastStar || name == broadcastAll {
		return "", badRequest("%q is a reserved name and cannot be registered", name)
	}
	return name, nil
}

// requireWorkspace validates and normalises one workspace name.
func requireWorkspace(ws string) (string, error) {
	ws = strings.TrimSpace(ws)
	if ws == "" {
		return "", badRequest("workspace is required")
	}
	if strings.ContainsRune(ws, '@') {
		return "", badRequest("workspace must not contain '@'")
	}
	if hasControlBytes(ws) {
		return "", badRequest("workspace must not contain control characters")
	}
	return ws, nil
}

// MaxClaimKeyLength keeps a claim key well clear of practical limits while
// still fitting a deep file path.
const MaxClaimKeyLength = 1024

// requireClaimKey validates and normalises a claim key. Unlike requireName,
// this is not a path parser and imposes no shape beyond "non-empty, no
// control bytes, not absurdly long" -- most callers pass a file path, but
// nothing here assumes that.
func requireClaimKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", badRequest("key is required")
	}
	if hasControlBytes(key) {
		return "", badRequest("key must not contain control characters")
	}
	if len(key) > MaxClaimKeyLength {
		return "", badRequest("key must be at most %d characters", MaxClaimKeyLength)
	}
	return key, nil
}

// clampLimit turns a non-positive limit into the default and rejects
// anything past maxInboxRequestLimit with a 400, rather than silently truncating.
func clampLimit(limit int) (int, error) {
	if limit <= 0 {
		return store.DefaultInboxLimit, nil
	}
	if limit > maxInboxRequestLimit {
		return 0, badRequest("limit %d exceeds the maximum of %d", limit, maxInboxRequestLimit)
	}
	return limit, nil
}
