package daemon

import (
	"errors"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/praneethravuri/tether/internal/store"
)

// Defaults for Config. Anything non-positive in a caller-supplied Config falls
// back to these, so a zero Config is a working Config.
const (
	defaultStaleAfter      = 5 * time.Minute
	defaultDeadAfter       = 24 * time.Hour
	defaultRetainMessages  = 7 * 24 * time.Hour
	defaultSweepInterval   = 5 * time.Minute
	defaultShutdownTimeout = 5 * time.Second
	defaultRequestTimeout  = 30 * time.Second
	defaultMaxRequestBytes = 1 << 20 // 1 MiB
	defaultWaitTimeout     = 60 * time.Second

	// defaultClaimTTL is how long a claim lives without renewal. Short
	// enough that an abandoned claim isn't a permanent lock, long enough to
	// hold across a normal working session.
	defaultClaimTTL = 15 * time.Minute

	// maxWaitPerRequest bounds one held-open wait round trip; WaitResult.Capped
	// tells the CLI to transparently re-issue wait past this internal ceiling.
	maxWaitPerRequest = 5 * time.Minute

	// maxInboxRequestLimit caps a caller's --limit; past it the request is
	// rejected. Unrelated to store.maxInboxDepth, which silently drops the
	// oldest message once a mailbox itself grows too deep.
	maxInboxRequestLimit = 500
	maxClientMsgLen      = 256

	// defaultMaxConns bounds concurrent connections. Generous for a local,
	// single-user daemon -- this guards against runaway reconnect loops, not
	// real traffic.
	defaultMaxConns = 256
	// defaultIdleTimeout closes a connection that sits with no request in
	// flight this long. A production CLI call is one request then close, so
	// this only ever catches an abandoned or misbehaving connection.
	defaultIdleTimeout = 10 * time.Minute
)

// errRequestTooLarge is returned by limitedReader once a single request has
// exceeded Config.MaxRequestBytes.
var errRequestTooLarge = errors.New("request too large")

// Config tunes the daemon. The zero value is valid and yields the defaults.
type Config struct {
	// StaleAfter is how long an agent may go without a heartbeat before its
	// name can be claimed by a different session.
	StaleAfter time.Duration
	// DeadAfter is how old an unacked message may get before the sweeper
	// retires it.
	DeadAfter time.Duration
	// RetainMessages is how long read or dead mail stays in the database
	// before the sweeper deletes it outright, so the file plateaus instead
	// of growing forever. Separate from DeadAfter: unread mail is marked
	// dead first, then deleted once it's also past this window.
	RetainMessages time.Duration
	// SweepInterval is how often the sweeper runs.
	SweepInterval time.Duration
	// ShutdownTimeout bounds how long Serve waits for in-flight connections.
	ShutdownTimeout time.Duration
	// RequestTimeout bounds a single non-wait request. Wait is exempt: it is
	// a long poll by design.
	RequestTimeout time.Duration
	// MaxRequestBytes is the largest single request accepted on a connection.
	MaxRequestBytes int64
	// DefaultWait is the wait timeout used when the caller does not ask for
	// one; MaxWait caps what a caller may ask for.
	DefaultWait time.Duration
	MaxWait     time.Duration
	// ClaimTTL is how long a claim lives without an explicit renewal (a
	// caller re-claiming the same key while still its live owner).
	ClaimTTL time.Duration
	// MaxConns bounds concurrent connections; acceptLoop blocks past this
	// rather than spawning an unbounded goroutine per accept.
	MaxConns int
	// IdleTimeout bounds how long a connection may sit between requests
	// before it's closed. Reset before every read, so it never cuts off a
	// wait already in progress -- only the gap before the next request.
	IdleTimeout time.Duration
	// Logger receives daemon logs. Nil means log.Default().
	Logger *log.Logger
	// LogPath, if set, is checked and rotated in place on every sweep once
	// it grows past logMaxBytes. Empty means no log rotation runs.
	LogPath string
}

// DefaultConfig returns the production configuration.
func DefaultConfig() Config {
	return Config{
		StaleAfter:      defaultStaleAfter,
		DeadAfter:       defaultDeadAfter,
		RetainMessages:  defaultRetainMessages,
		SweepInterval:   defaultSweepInterval,
		ShutdownTimeout: defaultShutdownTimeout,
		RequestTimeout:  defaultRequestTimeout,
		MaxRequestBytes: defaultMaxRequestBytes,
		DefaultWait:     defaultWaitTimeout,
		MaxWait:         maxWaitPerRequest,
		ClaimTTL:        defaultClaimTTL,
		MaxConns:        defaultMaxConns,
		IdleTimeout:     defaultIdleTimeout,
	}
}

// withDefaults fills in every non-positive field.
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.StaleAfter <= 0 {
		c.StaleAfter = d.StaleAfter
	}
	if c.DeadAfter <= 0 {
		c.DeadAfter = d.DeadAfter
	}
	if c.RetainMessages <= 0 {
		c.RetainMessages = d.RetainMessages
	}
	if c.SweepInterval <= 0 {
		c.SweepInterval = d.SweepInterval
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = d.ShutdownTimeout
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = d.RequestTimeout
	}
	if c.MaxRequestBytes <= 0 {
		c.MaxRequestBytes = d.MaxRequestBytes
	}
	if c.DefaultWait <= 0 {
		c.DefaultWait = d.DefaultWait
	}
	if c.MaxWait <= 0 {
		c.MaxWait = d.MaxWait
	}
	if c.DefaultWait > c.MaxWait {
		c.DefaultWait = c.MaxWait
	}
	if c.ClaimTTL <= 0 {
		c.ClaimTTL = d.ClaimTTL
	}
	if c.MaxConns <= 0 {
		c.MaxConns = d.MaxConns
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = d.IdleTimeout
	}
	if c.Logger == nil {
		c.Logger = log.Default()
	}
	return c
}

// Server serves the tether protocol over a unix socket, one goroutine per
// reusable connection. Panics are recovered per request; internal failures
// are logged in full but reported to clients as an opaque reference.
type Server struct {
	store   *store.Store
	waiters *Waiters
	cfg     Config
	log     *log.Logger

	// wg tracks live connection goroutines.
	wg sync.WaitGroup

	// mu guards conns/closed. Tracking connections is what lets shutdown
	// unblock handlers parked in Decode or in a long wait.
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool

	// errSeq numbers internal-error references so a user-visible 500 can be
	// matched to a log line without leaking the underlying error text.
	errSeq atomic.Uint64

	// connSlots bounds concurrent connections: acceptLoop blocks acquiring a
	// slot past MaxConns instead of spawning an unbounded goroutine per accept.
	connSlots chan struct{}
}

// ErrHandlersAbandoned is joined into Serve's returned error when the
// shutdown timeout elapsed with connection handlers still in flight, so a
// caller knows not to close resources (like the store) those handlers might
// still be using.
var ErrHandlersAbandoned = errors.New("daemon: shut down with handlers still in flight")

// NewServer builds a Server. st must be non-nil and stays owned by the caller:
// Serve never closes it.
func NewServer(st *store.Store, cfg Config) *Server {
	c := cfg.withDefaults()
	return &Server{
		store:     st,
		waiters:   NewWaiters(),
		cfg:       c,
		log:       c.Logger,
		conns:     make(map[net.Conn]struct{}),
		connSlots: make(chan struct{}, c.MaxConns),
	}
}

// Config returns the effective configuration, defaults applied.
func (s *Server) Config() Config { return s.cfg }
