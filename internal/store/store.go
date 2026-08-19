// Package store is tether's persistence layer: a SQLite-backed registry of
// live agents and a durable, ack-based mailbox.
//
// Reading an inbox never removes anything; a message leaves only via an
// explicit ack or the dead sweep.
package store

import (
	"context"
	"database/sql"
	_ "embed" // for //go:embed schema.sql
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	// modernc.org/sqlite is a cgo-free SQLite driver. It registers itself
	// under the name "sqlite" (not "sqlite3").
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// defaultMaxBodyBytes caps a single message body at 64 KiB.
const defaultMaxBodyBytes = 64 << 10

// Store owns the database handles. It is safe for concurrent use. The write
// pool is pinned to one connection so writes queue instead of fighting
// SQLITE_BUSY; reads use a separate, larger pool.
type Store struct {
	// Now supplies the current time; tests override it to skip past
	// staleness thresholds.
	Now func() time.Time

	// MaxBodyBytes is the largest accepted message body.
	MaxBodyBytes int

	// Logger receives diagnostics the Store cannot return as an error (a
	// rollback failure). Nil means silent.
	Logger *log.Logger

	w *sql.DB // writes: SetMaxOpenConns(1)
	r *sql.DB // reads: normal pool
}

// dsn builds the connection string; pragmas travel here because a PRAGMA via
// Exec would land on one arbitrary pooled connection.
func dsn(path string) string {
	return "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)"
}

// Open opens (creating if needed) the database at path and applies the schema.
// It is idempotent: reopening an existing database is a supported no-op.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: create db dir: %w", err)
		}
	}

	conn := dsn(path)

	w, err := sql.Open("sqlite", conn)
	if err != nil {
		return nil, fmt.Errorf("store: open writer: %w", err)
	}
	// One connection == one serialized write queue.
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)

	// Ping forces a real connection so a bad path fails here, not later.
	if err := w.PingContext(ctx); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("store: ping writer: %w", err)
	}

	r, err := sql.Open("sqlite", conn)
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("store: open reader: %w", err)
	}
	readers := runtime.NumCPU()
	if readers < 4 {
		readers = 4
	}
	r.SetMaxOpenConns(readers)
	r.SetMaxIdleConns(readers)
	r.SetConnMaxLifetime(0)

	if err := r.PingContext(ctx); err != nil {
		_ = w.Close()
		_ = r.Close()
		return nil, fmt.Errorf("store: ping reader: %w", err)
	}

	s := &Store{
		Now:          time.Now,
		MaxBodyBytes: defaultMaxBodyBytes,
		w:            w,
		r:            r,
	}
	if err := s.migrate(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// Close releases both pools. It is safe to call on a partially built Store.
func (s *Store) Close() error {
	var errs []error
	if s.w != nil {
		if err := s.w.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.r != nil {
		if err := s.r.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// currentSchemaVersion is the highest schema version this binary
// understands. Open refuses a database stamped with a newer one rather than
// silently continuing against a shape it doesn't know.
const currentSchemaVersion = 1

// migrate applies the full schema in one statement (modernc.org/sqlite
// executes a multi-statement script directly, so no hand-rolled splitter is
// needed), then reconciles a database created under an older schema, all in
// one transaction: a failure partway through leaves the database exactly as
// it was, not half-migrated with no record of how far it got. Safe to
// re-run on every Open.
func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	defer s.rollback(tx)

	version, err := readSchemaVersion(ctx, tx)
	if err != nil {
		return fmt.Errorf("store: migrate: read schema version: %w", err)
	}
	if version > currentSchemaVersion {
		return fmt.Errorf(
			"store: database schema v%d is newer than this binary understands (v%d) -- upgrade tether",
			version, currentSchemaVersion)
	}

	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("store: migrate: apply schema: %w", err)
	}

	if _, err := tx.ExecContext(ctx, qSetSchemaVersion, currentSchemaVersion); err != nil {
		return fmt.Errorf("store: migrate: record schema version: %w", err)
	}
	return tx.Commit()
}

// readSchemaVersion returns 0 for a database with no meta row yet -- a
// brand-new database (the common case: meta doesn't exist until schemaSQL
// runs, right after this check), or one from before schema_version existed
// at all -- which is exactly "run every migration from scratch".
func readSchemaVersion(ctx context.Context, tx *sql.Tx) (int, error) {
	var metaExists int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'meta'`,
	).Scan(&metaExists); err != nil {
		return 0, err
	}
	if metaExists == 0 {
		return 0, nil
	}

	var raw string
	err := tx.QueryRowContext(ctx, qGetSchemaVersion).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("malformed schema_version %q: %w", raw, err)
	}
	return n, nil
}

// rollbacker is satisfied by *sql.Tx, and by a fake in tests.
// table_info. table and col must be compile-time constants from this
// package -- PRAGMA does not accept bound parameters and no SQL here is
// ever built from a runtime string.

// nowMS is the current store time in Unix milliseconds.
func (s *Store) nowMS() int64 {
	return s.now().UnixMilli()
}

// now reads Now defensively so a zero-valued Store still works.
func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// maxBody returns the effective body cap.
func (s *Store) maxBody() int {
	if s.MaxBodyBytes > 0 {
		return s.MaxBodyBytes
	}
	return defaultMaxBodyBytes
}

// rollbacker is satisfied by *sql.Tx, and by a fake in tests.
type rollbacker interface {
	Rollback() error
}

// rollback discards a transaction, logging anything but the expected
// already-finished case.
func (s *Store) rollback(tx rollbacker) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		if s.Logger != nil {
			s.Logger.Printf("store: rollback failed: %v", err)
		}
	}
}
