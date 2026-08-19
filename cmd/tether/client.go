package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"time"

	"github.com/praneethravuri/tether/internal/id"
	"github.com/praneethravuri/tether/internal/protocol"
)

const (
	// dialTimeout bounds how long we wait for the socket to accept us. A live
	// daemon accepts immediately; anything slower is a hung daemon.
	dialTimeout = 2 * time.Second

	// defaultCallTimeout bounds a normal request/response round trip. Every
	// method except wait answers in milliseconds, so failing fast here is
	// better than hanging a shell forever.
	defaultCallTimeout = 10 * time.Second

	// waitGrace is added to the caller's own timeout for MethodWait, which
	// legitimately blocks for minutes.
	waitGrace = 30 * time.Second
)

// call sends one request to the daemon and decodes one response using the
// default timeout; use doCall directly for wait's longer one, or to skip
// auto-start (doctor, since a health check must never start what it checks).
func call(method string, params, result any) error {
	return doCall(method, params, result, defaultCallTimeout, true)
}

// doCall is call with an explicit deadline (<=0 means none) and control over
// auto-start. A daemon-side failure comes back as *protocol.Error; anything
// else as an *exitError. result may be nil to discard the response body. A
// dead socket auto-starts a daemon and retries once, when autoStart is set.
func doCall(method string, params, result any, timeout time.Duration, autoStart bool) error {
	sock, err := protocol.SocketPath()
	if err != nil {
		return failf(exitGeneral, "cannot work out where the tether socket lives: %v", err)
	}

	conn, err := dial(sock, autoStart)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	reqID, err := id.New()
	if err != nil {
		return failf(exitGeneral, "cannot generate a request id: %v", err)
	}
	req := protocol.Request{ID: reqID, Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return failf(exitGeneral, "cannot encode the %s request: %v", method, err)
		}
		req.Params = raw
	}

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return failf(exitGeneral, "cannot send the %s request to the daemon: %v", method, err)
	}

	counted := &countingReader{r: conn}

	var resp protocol.Response
	if err := json.NewDecoder(counted).Decode(&resp); err != nil {
		return readError(method, timeout, counted.n, err)
	}

	if resp.Error != nil {
		return resp.Error
	}

	if result == nil || len(resp.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Result, result); err != nil {
		return failf(exitGeneral,
			"the daemon sent a %s result this version of tether cannot read: %v", method, err)
	}

	return nil
}

// dial connects to sock. If nothing answers and autoStart is set, it spawns
// a detached daemon and retries once; a spawn failure or a daemon that never
// becomes dialable still exits exitNoDaemon.
func dial(sock string, autoStart bool) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", sock, dialTimeout)
	if err == nil {
		return conn, nil
	}
	if !autoStart {
		return nil, failf(exitNoDaemon,
			"no daemon running (tried socket %s) — start it with `tether start`", sock)
	}

	if startErr := spawnDaemon(sock); startErr != nil {
		return nil, failf(exitNoDaemon,
			"no daemon running (tried socket %s) and could not start one automatically: %v "+
				"— start it manually with `tether start`", sock, startErr)
	}

	conn, err = net.DialTimeout("unix", sock, dialTimeout)
	if err != nil {
		return nil, failf(exitNoDaemon,
			"started a daemon but socket %s is still not reachable: %v", sock, err)
	}
	return conn, nil
}

// countingReader records how many bytes were read from the connection.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// readError turns a decode failure into a message the caller can act on, and
// never into a panic. read is how many bytes arrived before the failure.
func readError(method string, timeout time.Duration, read int64, err error) error {
	switch {
	case read == 0 && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)):
		return failf(exitGeneral,
			"the daemon closed the connection without answering the %s request", method)
	case isTimeout(err):
		return failf(exitGeneral,
			"the daemon did not answer the %s request within %s", method, timeout)
	default:
		return failf(exitGeneral, "the daemon sent a malformed response to %s: %v", method, err)
	}
}

// isTimeout reports whether err is a network timeout.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// waitCallTimeout is the socket deadline to use for a wait of d.
func waitCallTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultCallTimeout
	}
	return d + waitGrace
}

// daemonCode returns the protocol error code carried by err, and whether err
// came from the daemon at all.
func daemonCode(err error) (int, bool) {
	var pe *protocol.Error
	if errors.As(err, &pe) && pe != nil {
		return pe.Code, true
	}
	return 0, false
}

// ensureRegistered silently (re-)registers before a command's real request,
// which is what makes registration implicit. name may be empty, meaning
// "resolve whatever this session already registered, or mint one". It
// returns both the resolved name and the exact session used to register it;
// the caller must use that pair in its immediate real request. A name
// conflict is surfaced immediately; any other failure (usually no daemon) is
// swallowed since the real call right after hits the same failure with a more
// specific message -- in that case the original name and session are returned
// unchanged.
func ensureRegistered(name, workspace string) (string, string, error) {
	cwd, _ := os.Getwd()
	harness, session := currentSessionForAgent(name)

	params := protocol.RegisterParams{
		Name:      name,
		Workspace: workspace,
		Harness:   harness,
		SessionID: session,
		Cwd:       cwd,
		PID:       os.Getppid(), // the shell, not this short-lived CLI process
	}

	var res protocol.RegisterResult
	if err := call(protocol.MethodRegister, params, &res); err != nil {
		if code, ok := daemonCode(err); ok && code == protocol.CodeConflict {
			return "", "", registerError(name, workspace, err)
		}
		return name, session, nil
	}
	if res.Name != "" {
		return res.Name, session, nil
	}
	return name, session, nil
}
