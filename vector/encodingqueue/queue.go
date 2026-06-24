package encodingqueue

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"time"
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Schema names the pending-task table and columns used by Queue.
//
// Identifiers are interpolated into SQL after validation, so every field
// must be a simple SQL identifier rather than arbitrary SQL text.
type Schema struct {
	Table            string
	GroupIDColumn    string
	TaskIDColumn     string
	EnqueuedAtColumn string
	ClaimedAtColumn  string
	ClaimTokenColumn string
}

// DefaultSchema returns the table shape used by msgvault's vector
// embedding queue.
func DefaultSchema() Schema {
	return Schema{
		Table:            "pending_embeddings",
		GroupIDColumn:    "generation_id",
		TaskIDColumn:     "message_id",
		EnqueuedAtColumn: "enqueued_at",
		ClaimedAtColumn:  "claimed_at",
		ClaimTokenColumn: "claim_token",
	}
}

// Queue wraps a SQL pending-task table with crash-safe claim, complete,
// release, and stale-claim reclamation operations.
type Queue struct {
	db     *sql.DB
	schema Schema
}

type execContext interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// NewQueue returns a Queue bound to db. The caller retains ownership of
// db; Queue does not close it.
func NewQueue(db *sql.DB, schema Schema) (*Queue, error) {
	if err := schema.validate(); err != nil {
		return nil, err
	}
	if db == nil {
		return nil, fmt.Errorf("db is nil")
	}
	return &Queue{db: db, schema: schema}, nil
}

// Enqueue inserts every task ID once for every group ID. Duplicate
// (group, task) pairs are ignored by the pending table's uniqueness
// constraint.
func (q *Queue) Enqueue(ctx context.Context, groupIDs []int64, taskIDs []int64) error {
	if len(groupIDs) == 0 || len(taskIDs) == 0 {
		return nil
	}
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin enqueue tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := q.EnqueueTx(ctx, tx, groupIDs, taskIDs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit enqueue: %w", err)
	}
	return nil
}

// EnqueueTx inserts every task ID once for every group ID using tx. The
// caller is responsible for committing or rolling back tx.
func (q *Queue) EnqueueTx(ctx context.Context, tx *sql.Tx, groupIDs []int64, taskIDs []int64) error {
	if tx == nil {
		return fmt.Errorf("tx is nil")
	}
	return q.enqueueWith(ctx, tx, groupIDs, taskIDs)
}

// Claim marks up to batch available rows for groupID as claimed by a
// fresh token, returning task IDs in ascending order with the token to
// pass to Complete or Release.
func (q *Queue) Claim(ctx context.Context, groupID int64, batch int) ([]int64, string, error) {
	if batch <= 0 {
		return nil, "", nil
	}
	token, err := newToken()
	if err != nil {
		return nil, "", fmt.Errorf("new token: %w", err)
	}
	now := time.Now().Unix()

	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", fmt.Errorf("begin claim tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, q.claimSQL(), now, token, groupID, batch)
	if err != nil {
		return nil, "", fmt.Errorf("claim query: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, "", fmt.Errorf("scan claimed task id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, "", fmt.Errorf("claim rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, "", fmt.Errorf("close claim rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, "", fmt.Errorf("commit claim: %w", err)
	}
	if len(ids) == 0 {
		return nil, "", nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, token, nil
}

// Complete deletes claimed rows whose claim token still matches token.
func (q *Queue) Complete(ctx context.Context, groupID int64, token string, taskIDs []int64) error {
	if len(taskIDs) == 0 {
		return nil
	}
	return q.updateClaimedIDs(ctx, "delete pending tasks", q.completeSQL(), groupID, token, taskIDs)
}

// Release clears matching claims so tasks can be retried by another
// worker.
func (q *Queue) Release(ctx context.Context, groupID int64, token string, taskIDs []int64) error {
	if len(taskIDs) == 0 {
		return nil
	}
	return q.updateClaimedIDs(ctx, "release pending tasks", q.releaseSQL(), groupID, token, taskIDs)
}

// ReclaimStale clears claims older than olderThan and returns the number
// of rows reclaimed.
func (q *Queue) ReclaimStale(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan).Unix()
	res, err := q.db.ExecContext(ctx, q.reclaimSQL(), cutoff)
	if err != nil {
		return 0, fmt.Errorf("reclaim stale: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return int(n), nil
}

func (q *Queue) updateClaimedIDs(ctx context.Context, op, query string, groupID int64, token string, taskIDs []int64) error {
	blob, err := json.Marshal(taskIDs)
	if err != nil {
		return fmt.Errorf("encode task ids: %w", err)
	}
	if _, err := q.db.ExecContext(ctx, query, groupID, token, string(blob)); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

func (q *Queue) enqueueWith(ctx context.Context, execer execContext, groupIDs []int64, taskIDs []int64) error {
	if len(groupIDs) == 0 || len(taskIDs) == 0 {
		return nil
	}
	blob, err := json.Marshal(taskIDs)
	if err != nil {
		return fmt.Errorf("encode task ids: %w", err)
	}
	now := time.Now().Unix()
	for _, groupID := range groupIDs {
		if _, err := execer.ExecContext(ctx, q.enqueueSQL(), groupID, now, string(blob)); err != nil {
			return fmt.Errorf("insert pending tasks (group=%d): %w", groupID, err)
		}
	}
	return nil
}

func (q *Queue) enqueueSQL() string {
	s := q.schema
	return fmt.Sprintf(`
		INSERT OR IGNORE INTO %s (%s, %s, %s)
		SELECT ?, value, ? FROM json_each(?)`,
		s.Table, s.GroupIDColumn, s.TaskIDColumn, s.EnqueuedAtColumn)
}

func (q *Queue) claimSQL() string {
	s := q.schema
	return fmt.Sprintf(`
		UPDATE %s
		   SET %s = ?, %s = ?
		 WHERE (%s, %s) IN (
		       SELECT %s, %s
		         FROM %s
		        WHERE %s = ?
		          AND %s IS NULL
		        ORDER BY %s
		        LIMIT ?)
		RETURNING %s`,
		s.Table,
		s.ClaimedAtColumn, s.ClaimTokenColumn,
		s.GroupIDColumn, s.TaskIDColumn,
		s.GroupIDColumn, s.TaskIDColumn,
		s.Table,
		s.GroupIDColumn,
		s.ClaimedAtColumn,
		s.TaskIDColumn,
		s.TaskIDColumn)
}

func (q *Queue) completeSQL() string {
	s := q.schema
	return fmt.Sprintf(`
		DELETE FROM %s
		 WHERE %s = ?
		   AND %s = ?
		   AND %s IN (SELECT value FROM json_each(?))`,
		s.Table, s.GroupIDColumn, s.ClaimTokenColumn, s.TaskIDColumn)
}

func (q *Queue) releaseSQL() string {
	s := q.schema
	return fmt.Sprintf(`
		UPDATE %s
		   SET %s = NULL, %s = NULL
		 WHERE %s = ?
		   AND %s = ?
		   AND %s IN (SELECT value FROM json_each(?))`,
		s.Table,
		s.ClaimedAtColumn, s.ClaimTokenColumn,
		s.GroupIDColumn,
		s.ClaimTokenColumn,
		s.TaskIDColumn)
}

func (q *Queue) reclaimSQL() string {
	s := q.schema
	return fmt.Sprintf(`
		UPDATE %s
		   SET %s = NULL, %s = NULL
		 WHERE %s IS NOT NULL AND %s < ?`,
		s.Table,
		s.ClaimedAtColumn, s.ClaimTokenColumn,
		s.ClaimedAtColumn, s.ClaimedAtColumn)
}

func (s Schema) validate() error {
	fields := map[string]string{
		"table":              s.Table,
		"group id column":    s.GroupIDColumn,
		"task id column":     s.TaskIDColumn,
		"enqueued at column": s.EnqueuedAtColumn,
		"claimed at column":  s.ClaimedAtColumn,
		"claim token column": s.ClaimTokenColumn,
	}
	for name, value := range fields {
		if !identifierPattern.MatchString(value) {
			return fmt.Errorf("invalid %s %q", name, value)
		}
	}
	return nil
}

func newToken() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(b), nil
}
