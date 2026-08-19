package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/praneethravuri/intern/internal/proc"
	"github.com/praneethravuri/intern/internal/protocol"
	"github.com/praneethravuri/intern/internal/store"
)

// Serve accepts connections until ctx is cancelled or the listener fails. On
// cancellation it closes the listener and every connection, waits up to
// ShutdownTimeout, then returns once all goroutines have stopped.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if ln == nil {
		return errors.New("intern: nil listener")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var bg sync.WaitGroup

	bg.Add(1)
	go func() {
		defer bg.Done()
		s.sweepLoop(ctx)
	}()

	// Closing ln unblocks Accept; closing every conn unblocks parked Decode calls.
	stopped := make(chan struct{})
	bg.Add(1)
	go func() {
		defer bg.Done()
		select {
		case <-ctx.Done():
		case <-stopped:
		}
		_ = ln.Close()
		s.closeConns()
	}()

	err := s.acceptLoop(ctx, ln)

	close(stopped)
	cancel()
	cleanShutdown := s.awaitConns()
	bg.Wait()
	if !cleanShutdown {
		err = errors.Join(err, ErrHandlersAbandoned)
	}
	return err
}

// acceptLoop runs until ctx is cancelled or the listener stops working.
func (s *Server) acceptLoop(ctx context.Context, ln net.Listener) error {
	const maxConsecutiveErrs = 5

	fails := 0
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil // orderly shutdown
			}
			fails++
			s.log.Printf("accept failed (%d/%d): %v", fails, maxConsecutiveErrs, err)
			if fails >= maxConsecutiveErrs {
				return fmt.Errorf("accept: %w", err)
			}
			select { // back off so a wedged listener can't spin a core
			case <-ctx.Done():
				return nil
			case <-time.After(time.Duration(fails) * 50 * time.Millisecond):
			}
			continue
		}
		fails = 0

		select { // bounded concurrency: block rather than spawn unboundedly
		case s.connSlots <- struct{}{}:
		case <-ctx.Done():
			_ = conn.Close()
			return nil
		}

		if !s.trackConn(conn) {
			<-s.connSlots
			_ = conn.Close() // shutting down
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-s.connSlots }()
			s.handleConn(ctx, conn)
		}()
	}
}

// awaitConns waits for connection goroutines, bounded by ShutdownTimeout so a
// wedged client can never hang the daemon's exit. Reports whether every
// handler finished cleanly within that budget.
func (s *Server) awaitConns() bool {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	t := time.NewTimer(s.cfg.ShutdownTimeout)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		s.log.Printf("shutdown timeout: abandoning connections still in flight")
		return false
	}
}

// sweepLoop retires unacked mail and dead agent rows that nobody ever came
// back for. It sweeps once immediately, before the first tick, so a daemon
// that restarts often still prunes something.
func (s *Server) sweepLoop(ctx context.Context) {
	s.sweepOnce(ctx)

	t := time.NewTicker(s.cfg.SweepInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepOnce(ctx)
		}
	}
}

func (s *Server) sweepOnce(ctx context.Context) {
	if n, err := s.store.SweepDead(ctx, s.cfg.DeadAfter); err != nil {
		if ctx.Err() == nil {
			s.log.Printf("sweep dead messages failed: %v", err)
		}
	} else if n > 0 {
		s.log.Printf("swept %d dead message(s)", n)
	}

	if n, err := s.store.PurgeMessages(ctx, s.cfg.RetainMessages); err != nil {
		if ctx.Err() == nil {
			s.log.Printf("purge retired messages failed: %v", err)
		}
	} else if n > 0 {
		s.log.Printf("purged %d message(s)", n)
	}

	s.sweepDeadAgents(ctx)

	if n, err := s.store.SweepExpiredClaims(ctx); err != nil {
		if ctx.Err() == nil {
			s.log.Printf("sweep expired claims failed: %v", err)
		}
	} else if n > 0 {
		s.log.Printf("swept %d expired claim(s)", n)
	}

	if s.cfg.LogPath != "" {
		if err := rotateIfLarge(s.cfg.LogPath); err != nil {
			s.log.Printf("log rotation failed: %v", err)
		}
	}
}

// sweepDeadAgents deletes agent rows both stale past DeadAfter and provably
// dead by pid, replacing the old explicit unregister RPC.
func (s *Server) sweepDeadAgents(ctx context.Context) {
	agents, err := s.store.ListAgents(ctx, "", 0) // every agent; cutoff applied below
	if err != nil {
		if ctx.Err() == nil {
			s.log.Printf("sweep dead agents: list: %v", err)
		}
		return
	}

	cutoff := time.Now().Add(-s.cfg.DeadAfter)
	var dead []store.AgentKey
	for _, a := range agents {
		if a.LastSeen.After(cutoff) {
			continue
		}
		if proc.AliveAt(a.PID, a.PIDStart) {
			continue
		}
		dead = append(dead, store.AgentKey{Workspace: a.Workspace, Name: a.Name})
	}
	if len(dead) == 0 {
		return
	}

	n, err := s.store.DeleteAgents(ctx, dead)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Printf("sweep dead agents: delete: %v", err)
		}
		return
	}
	if n > 0 {
		s.log.Printf("swept %d dead agent(s)", n)
	}
}

func (s *Server) trackConn(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if s.conns == nil {
		s.conns = make(map[net.Conn]struct{})
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *Server) untrackConn(c net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
}

func (s *Server) closeConns() {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.closed = true
	s.mu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}
}

// handleConn serves one connection until EOF, an unrecoverable protocol
// error, or shutdown. No read deadline: wait is a long poll; shutdown closes
// the connection instead.
func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Printf("panic in connection handler: %v\n%s", r, debug.Stack())
		}
		s.untrackConn(conn)
		_ = conn.Close()
	}()

	// Peer pid is advisory logging only, not authentication (the socket
	// directory is); losing it must never lose the connection.
	pid, err := getPeerPID(conn)
	if err != nil {
		s.log.Printf("peer pid unavailable, continuing without it: %v", err)
		pid = 0
	}
	s.log.Printf("connection open (pid %d)", pid)
	defer s.log.Printf("connection closed (pid %d)", pid)

	lr := &limitedReader{r: conn}
	dec := json.NewDecoder(lr)
	enc := json.NewEncoder(conn)

	for {
		lr.reset(s.cfg.MaxRequestBytes) // budget is per request, not per connection

		// Bounds the gap before the *next* request, not any handler already
		// dispatched: nothing here reads from conn again until this Decode
		// returns, so a long wait in dispatch is never cut short by this.
		_ = conn.SetReadDeadline(time.Now().Add(s.cfg.IdleTimeout))

		var req protocol.Request
		if err := dec.Decode(&req); err != nil {
			s.reportDecodeError(conn, enc, err, pid)
			return
		}
		_ = conn.SetReadDeadline(time.Time{})

		resp := s.dispatch(ctx, req, pid)
		if err := enc.Encode(resp); err != nil {
			if !isDisconnect(err) {
				s.log.Printf("write to pid %d failed: %v", pid, err)
			}
			return
		}
	}
}

// reportDecodeError answers a failed Decode as best it can and always ends the
// connection: a JSON stream cannot be resynchronised after a framing error.
func (s *Server) reportDecodeError(conn net.Conn, enc *json.Encoder, err error, pid int) {
	var netErr net.Error
	switch {
	case errors.Is(err, io.EOF), isDisconnect(err):
		return
	case errors.As(err, &netErr) && netErr.Timeout():
		s.log.Printf("closing idle connection from pid %d", pid)
		return
	case errors.Is(err, errRequestTooLarge):
		s.log.Printf("request from pid %d exceeded %d bytes", pid, s.cfg.MaxRequestBytes)
		_ = enc.Encode(protocol.Fail("", protocol.CodeTooLarge, "request too large"))
		drainPeer(conn)
	default:
		s.log.Printf("malformed request from pid %d: %v", pid, err)
		_ = enc.Encode(protocol.Fail("", protocol.CodeBadRequest, "malformed request"))
	}
}

// drainPeer discards leftover bytes from a rejected oversized request so the
// peer (blocked mid-write on a small AF_UNIX buffer) can still read our error
// response instead of getting EPIPE.
func drainPeer(conn net.Conn) {
	const drainCap = 64 << 10
	_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	_, _ = io.CopyN(io.Discard, conn, drainCap)
}

// isDisconnect reports whether err is an ordinary peer hangup rather than a
// fault worth logging.
func isDisconnect(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET)
}

// dispatch routes one request. It never panics: a handler panic becomes a 500.
func (s *Server) dispatch(ctx context.Context, req protocol.Request, pid int) (resp protocol.Response) {
	defer func() {
		if r := recover(); r != nil {
			ref := s.nextRef()
			s.log.Printf("panic handling %q [%s]: %v\n%s", clip(req.Method), ref, r, debug.Stack())
			resp = protocol.Fail(req.ID, protocol.CodeInternal, "internal error (ref "+ref+")")
		}
	}()

	// Wait is a long poll and must not inherit the per-request deadline.
	if req.Method != protocol.MethodWait {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.RequestTimeout)
		defer cancel()
	}

	switch req.Method {
	case protocol.MethodRegister:
		return s.handleRegister(ctx, req, pid)
	case protocol.MethodSend:
		return s.handleSend(ctx, req)
	case protocol.MethodInbox:
		return s.handleInbox(ctx, req)
	case protocol.MethodWait:
		return s.handleWait(ctx, req)
	case protocol.MethodLs:
		return s.handleLs(ctx, req)
	case protocol.MethodClaim:
		return s.handleClaim(ctx, req, pid)
	case protocol.MethodRelease:
		return s.handleRelease(ctx, req)
	case protocol.MethodClaims:
		return s.handleClaims(ctx, req, pid)
	default:
		return protocol.Fail(req.ID, protocol.CodeBadRequest, "unknown method: "+clip(req.Method))
	}
}

// limitedReader caps bytes per request; resettable since one connection
// carries many requests. Used by one goroutine only, so needs no locking.
type limitedReader struct {
	r io.Reader
	n int64
}

func (l *limitedReader) reset(n int64) { l.n = n }

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, errRequestTooLarge
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}
