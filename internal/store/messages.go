package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/praneethravuri/tether/internal/id"
)

// DefaultInboxLimit is used when a caller passes a non-positive limit.
// Exported so the daemon's own request-side default stays the same value by
// reference, not by a second, easy-to-forget copy of the number 50.
const DefaultInboxLimit = 50

// maxInboxDepth bounds one agent's pending mail; past it the oldest message
// is retired first and agents.dropped is incremented. Unrelated to the
// daemon's maxInboxRequestLimit, despite the similar name and value: this
// one silently drops the oldest message, that one rejects the request.
const maxInboxDepth = 500

// scanMessage reads one messages row in msgCols order.
func scanMessage(sc rowScanner) (Message, error) {
	var (
		m         Message
		created   int64
		delivered sql.NullInt64
		acked     sql.NullInt64
	)
	err := sc.Scan(
		&m.ID, &m.ThreadID, &m.ReplyTo,
		&m.FromName, &m.FromWS, &m.ToName, &m.ToWS,
		&m.Kind, &m.Body, &created, &delivered, &acked,
	)
	if err != nil {
		return Message{}, err
	}
	m.CreatedAt = fromMS(created)
	m.DeliveredAt = msPtr(delivered.Int64, delivered.Valid)
	m.AckedAt = msPtr(acked.Int64, acked.Valid)
	return m, nil
}

// Send queues a message for the recipient and returns the new message id.
// The recipient must exist but need not be fresh (mail queues for a
// mid-restart agent).
func (s *Store) Send(ctx context.Context, m Message) (string, error) {
	if m.ToWS == "" || m.ToName == "" {
		return "", fmt.Errorf("%w: message needs a recipient", ErrBadAddress)
	}
	if m.FromWS == "" || m.FromName == "" {
		return "", fmt.Errorf("%w: message needs a sender", ErrBadAddress)
	}
	if strings.TrimSpace(m.Body) == "" {
		return "", ErrEmptyBody
	}
	if len(m.Body) > s.maxBody() {
		return "", fmt.Errorf("%w: %d bytes (max %d)", ErrBodyTooLarge, len(m.Body), s.maxBody())
	}
	if m.Kind == "" {
		m.Kind = KindNote
	}
	if !ValidKind(m.Kind) {
		return "", fmt.Errorf("store: invalid kind %q", m.Kind)
	}

	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("store: send: %w", err)
	}
	defer s.rollback(tx)

	var exists int
	err = tx.QueryRowContext(ctx, qAgentExists, m.ToWS, m.ToName).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrNoSuchAgent, m.To())
	}
	if err != nil {
		return "", fmt.Errorf("store: send: lookup recipient: %w", err)
	}

	newID, err := id.New()
	if err != nil {
		return "", fmt.Errorf("store: send: %w", err)
	}

	// A reply joins its parent's thread; a fresh message starts its own.
	threadID := newID
	if m.ReplyTo != "" {
		err = tx.QueryRowContext(ctx, qThreadOf, m.ReplyTo).Scan(&threadID)
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: reply_to %s", ErrNoSuchMessage, m.ReplyTo)
		}
		if err != nil {
			return "", fmt.Errorf("store: send: lookup thread: %w", err)
		}
	}

	created := s.nowMS()
	if _, err := tx.ExecContext(ctx, qInsertMessage,
		newID, threadID, m.ReplyTo,
		m.FromName, m.FromWS, m.ToName, m.ToWS,
		m.Kind, m.Body, created,
	); err != nil {
		return "", fmt.Errorf("store: send: insert: %w", err)
	}

	if err := s.enforceInboxDepth(ctx, tx, m.ToWS, m.ToName, newID); err != nil {
		return "", fmt.Errorf("store: send: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("store: send: commit: %w", err)
	}
	return newID, nil
}

// enforceInboxDepth marks the oldest excess over maxInboxDepth dead and
// bumps agents.dropped. keepID is the message just inserted, excluded from
// eviction so it can never be the thing it displaces.
func (s *Store) enforceInboxDepth(ctx context.Context, tx *sql.Tx, ws, name, keepID string) error {
	var count int
	if err := tx.QueryRowContext(ctx, qPendingCount, ws, name).Scan(&count); err != nil {
		return fmt.Errorf("count pending: %w", err)
	}
	excess := count - maxInboxDepth
	if excess <= 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, qDropOldest, ws, name, keepID, excess); err != nil {
		return fmt.Errorf("drop oldest: %w", err)
	}
	if _, err := tx.ExecContext(ctx, qIncrementDropped, excess, ws, name); err != nil {
		return fmt.Errorf("increment dropped: %w", err)
	}
	return nil
}

// queryer is satisfied by *sql.DB, *sql.Tx, and the read pool.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// fetchPending runs qInbox and scans every row, the read half Inbox and
// Drain otherwise duplicated between them.
func fetchPending(ctx context.Context, q queryer, ws, name string, limit int) ([]Message, error) {
	rows, err := q.QueryContext(ctx, qInbox, ws, name, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]Message, 0, limit)
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// stampDelivered marks every id delivered in one round trip. ids must be
// non-empty; callers already know that from building the slice.
func stampDelivered(ctx context.Context, ex execer, nowMS int64, ids []string) error {
	placeholders := "(" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"
	args := make([]any, 0, len(ids)+1)
	args = append(args, nowMS)
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := ex.ExecContext(ctx, qStampDeliveredPrefix+placeholders, args...)
	return err
}

// Inbox returns the recipient's pending mail, oldest first, stamping
// delivered_at on first delivery. Not destructive: unacked mail is returned
// again on every call. Reads from the pool, not the single-connection
// writer -- only the delivered stamp, if anything needs it, touches the
// writer, and as one batched statement rather than one per message.
func (s *Store) Inbox(ctx context.Context, ws, name string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = DefaultInboxLimit
	}

	out, err := fetchPending(ctx, s.r, ws, name, limit)
	if err != nil {
		return nil, fmt.Errorf("store: inbox: %w", err)
	}

	now := s.now()
	nowMS := now.UnixMilli()
	var undelivered []string
	for i := range out {
		if out[i].DeliveredAt != nil {
			continue
		}
		undelivered = append(undelivered, out[i].ID)
		t := now
		out[i].DeliveredAt = &t
	}
	if len(undelivered) > 0 {
		if err := stampDelivered(ctx, s.w, nowMS, undelivered); err != nil {
			return nil, fmt.Errorf("store: inbox: stamp delivered: %w", err)
		}
	}
	return out, nil
}

// Replay returns the recipient's most recent acked messages, oldest first,
// going back at most window (<= 0 means no limit) since each message was
// acked -- not since it was sent, which is a different message entirely.
func (s *Store) Replay(ctx context.Context, ws, name string, limit int, window time.Duration) ([]Message, error) {
	if limit <= 0 {
		limit = DefaultInboxLimit
	}
	var ackedSince int64
	if window > 0 {
		ackedSince = s.nowMS() - window.Milliseconds()
	}

	rows, err := s.r.QueryContext(ctx, qReplay, ws, name, ackedSince, limit)
	if err != nil {
		return nil, fmt.Errorf("store: replay: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Message, 0, limit)
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("store: replay: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: replay: %w", err)
	}

	// qReplay is newest-first; reverse to ascending.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// execer lets ackIDs run against either the write pool or an existing
// transaction (Drain).
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ackIDs runs the ack UPDATE and reports how many rows changed; a repeat ack
// or an id owned by another agent counts as 0.
func ackIDs(ctx context.Context, ex execer, now int64, ws, name string, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	// Placeholders only; ids are always bound, never spliced into the SQL.
	placeholders := "(" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"
	query := qAckPrefix + placeholders

	args := make([]any, 0, len(ids)+3)
	args = append(args, now, ws, name)
	for _, i := range ids {
		args = append(args, i)
	}

	res, err := ex.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// Drain returns the recipient's pending mail, oldest first, acking it in the
// same transaction so a message is handed over exactly once. dropped reports
// and resets agents.dropped since the last drain.
func (s *Store) Drain(ctx context.Context, ws, name string, limit int) (msgs []Message, dropped int, err error) {
	if limit <= 0 {
		limit = DefaultInboxLimit
	}

	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("store: drain: %w", err)
	}
	defer s.rollback(tx)

	out, err := fetchPending(ctx, tx, ws, name, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("store: drain: %w", err)
	}

	now := s.now()
	nowMS := now.UnixMilli()
	ids := make([]string, len(out))
	var undelivered []string
	for i := range out {
		ids[i] = out[i].ID
		if out[i].DeliveredAt == nil {
			undelivered = append(undelivered, out[i].ID)
			t := now
			out[i].DeliveredAt = &t
		}
	}
	if len(undelivered) > 0 {
		if err := stampDelivered(ctx, tx, nowMS, undelivered); err != nil {
			return nil, 0, fmt.Errorf("store: drain: stamp delivered: %w", err)
		}
	}

	if len(ids) > 0 {
		if _, err := ackIDs(ctx, tx, nowMS, ws, name, ids); err != nil {
			return nil, 0, fmt.Errorf("store: drain: ack: %w", err)
		}
		for i := range out {
			t := now
			out[i].AckedAt = &t
		}
	}

	if err := tx.QueryRowContext(ctx, qGetDropped, ws, name).Scan(&dropped); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, 0, fmt.Errorf("store: drain: get dropped: %w", err)
	}
	if dropped > 0 {
		if _, err := tx.ExecContext(ctx, qResetDropped, ws, name); err != nil {
			return nil, 0, fmt.Errorf("store: drain: reset dropped: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, 0, fmt.Errorf("store: drain: commit: %w", err)
	}
	return out, dropped, nil
}

// PendingCount reports how many unacked, non-dead messages await a recipient.
func (s *Store) PendingCount(ctx context.Context, ws, name string) (int, error) {
	var n int
	if err := s.r.QueryRowContext(ctx, qPendingCount, ws, name).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: pending count %s@%s: %w", name, ws, err)
	}
	return n, nil
}

// SweepDead marks unacked messages older than olderThan as dead; they stay
// in the table for forensics.
func (s *Store) SweepDead(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := s.nowMS()
	if olderThan > 0 {
		cutoff -= olderThan.Milliseconds()
	}

	res, err := s.w.ExecContext(ctx, qSweepDead, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: sweep dead: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: sweep dead: %w", err)
	}
	return int(n), nil
}

// PurgeMessages deletes read or dead mail older than olderThan, so the
// database plateaus instead of growing forever. Pending mail is never
// touched regardless of age.
func (s *Store) PurgeMessages(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := s.nowMS()
	if olderThan > 0 {
		cutoff -= olderThan.Milliseconds()
	}

	res, err := s.w.ExecContext(ctx, qPurgeMessages, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: purge messages: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: purge messages: %w", err)
	}
	return int(n), nil
}

// PendingByWorkspace returns pending message counts per recipient name in a workspace.
func (s *Store) PendingByWorkspace(ctx context.Context, ws string) (map[string]int, error) {
	rows, err := s.r.QueryContext(ctx, qPendingByWorkspace, ws)
	if err != nil {
		return nil, fmt.Errorf("store: pending by workspace %s: %w", ws, err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]int)
	for rows.Next() {
		var (
			name string
			n    int
		)
		if err := rows.Scan(&name, &n); err != nil {
			return nil, fmt.Errorf("store: pending by workspace %s: %w", ws, err)
		}
		out[name] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: pending by workspace %s: %w", ws, err)
	}
	return out, nil
}
