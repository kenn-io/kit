package encodingqueue_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/vector/encodingqueue"
)

func TestQueueEnqueueClaimReleaseComplete(t *testing.T) {
	ctx := context.Background()
	require := require.New(t)
	assert := assert.New(t)
	db := openQueueDB(t)
	q, err := encodingqueue.NewQueue(db, encodingqueue.DefaultSchema())
	require.NoError(err)

	require.NoError(q.Enqueue(ctx, []int64{1, 2}, []int64{10, 11, 12}))

	ids, token, err := q.Claim(ctx, 1, 2)
	require.NoError(err)
	require.NotEmpty(token)
	assert.Equal([]int64{10, 11}, ids)
	assert.Equal(1, countAvailable(t, db, 1))

	require.NoError(q.Release(ctx, 1, token, ids))
	assert.Equal(3, countAvailable(t, db, 1))

	more, token2, err := q.Claim(ctx, 1, 10)
	require.NoError(err)
	require.NotEqual(token, token2)
	assert.Equal([]int64{10, 11, 12}, more)

	require.NoError(q.Complete(ctx, 1, token2, more))
	assert.Equal(0, countPending(t, db, 1))
	assert.Equal(3, countPending(t, db, 2))
}

func TestQueueIgnoresDuplicateEnqueue(t *testing.T) {
	ctx := context.Background()
	require := require.New(t)
	assert := assert.New(t)
	db := openQueueDB(t)
	q, err := encodingqueue.NewQueue(db, encodingqueue.DefaultSchema())
	require.NoError(err)

	require.NoError(q.Enqueue(ctx, []int64{1}, []int64{42}))
	require.NoError(q.Enqueue(ctx, []int64{1}, []int64{42, 42}))

	assert.Equal(1, countPending(t, db, 1))
}

func TestQueueEnqueueTxUsesCallerTransaction(t *testing.T) {
	ctx := context.Background()
	require := require.New(t)
	assert := assert.New(t)
	db := openQueueDB(t)
	q, err := encodingqueue.NewQueue(db, encodingqueue.DefaultSchema())
	require.NoError(err)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(err)
	require.NoError(q.EnqueueTx(ctx, tx, []int64{1}, []int64{10, 11}))
	assert.Equal(0, countPending(t, db, 1))
	require.NoError(tx.Commit())

	assert.Equal(2, countPending(t, db, 1))
}

func TestQueueClaimReturnsEmptyForNoWork(t *testing.T) {
	ctx := context.Background()
	require := require.New(t)
	assert := assert.New(t)
	db := openQueueDB(t)
	q, err := encodingqueue.NewQueue(db, encodingqueue.DefaultSchema())
	require.NoError(err)

	ids, token, err := q.Claim(ctx, 1, 10)
	require.NoError(err)

	assert.Empty(ids)
	assert.Empty(token)
}

func TestQueueReclaimStalePreservesNewClaimFromLateComplete(t *testing.T) {
	ctx := context.Background()
	require := require.New(t)
	assert := assert.New(t)
	db := openQueueDB(t)
	q, err := encodingqueue.NewQueue(db, encodingqueue.DefaultSchema())
	require.NoError(err)
	require.NoError(q.Enqueue(ctx, []int64{1}, []int64{1, 2}))

	idsA, tokenA, err := q.Claim(ctx, 1, 2)
	require.NoError(err)
	require.Len(idsA, 2)
	backdateClaims(t, db, 20*time.Minute)

	reclaimed, err := q.ReclaimStale(ctx, 10*time.Minute)
	require.NoError(err)
	assert.Equal(2, reclaimed)

	idsB, tokenB, err := q.Claim(ctx, 1, 2)
	require.NoError(err)
	require.Len(idsB, 2)
	require.NotEqual(tokenA, tokenB)

	require.NoError(q.Complete(ctx, 1, tokenA, idsA))
	assert.Equal(2, countPending(t, db, 1))
	assert.Equal(2, countClaimedByToken(t, db, tokenB))

	require.NoError(q.Complete(ctx, 1, tokenB, idsB))
	assert.Equal(0, countPending(t, db, 1))
}

func TestQueueClaimReturnsIDsAscending(t *testing.T) {
	ctx := context.Background()
	require := require.New(t)
	assert := assert.New(t)
	db := openQueueDB(t)
	q, err := encodingqueue.NewQueue(db, encodingqueue.DefaultSchema())
	require.NoError(err)
	require.NoError(q.Enqueue(ctx, []int64{1}, []int64{9, 3, 7, 1}))

	ids, _, err := q.Claim(ctx, 1, 10)
	require.NoError(err)

	assert.True(sort.SliceIsSorted(ids, func(i, j int) bool { return ids[i] < ids[j] }), "ids: %v", ids)
}

func TestNewQueueRejectsUnsafeIdentifiers(t *testing.T) {
	_, err := encodingqueue.NewQueue(nil, encodingqueue.Schema{
		Table:            "pending_embeddings; DROP TABLE pending_embeddings",
		GroupIDColumn:    "generation_id",
		TaskIDColumn:     "message_id",
		EnqueuedAtColumn: "enqueued_at",
		ClaimedAtColumn:  "claimed_at",
		ClaimTokenColumn: "claim_token",
	})

	require.Error(t, err)
}

func openQueueDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "queue.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.Exec(`
CREATE TABLE pending_embeddings (
    generation_id INTEGER NOT NULL,
    message_id    INTEGER NOT NULL,
    enqueued_at   INTEGER NOT NULL,
    claimed_at    INTEGER,
    claim_token   TEXT,
    PRIMARY KEY (generation_id, message_id)
);
CREATE INDEX idx_pending_available
    ON pending_embeddings(generation_id, message_id) WHERE claimed_at IS NULL;
CREATE INDEX idx_pending_claims
    ON pending_embeddings(claimed_at) WHERE claimed_at IS NOT NULL;`)
	require.NoError(t, err)
	return db
}

func countPending(t *testing.T, db *sql.DB, generationID int64) int {
	t.Helper()
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ?`,
		generationID,
	).Scan(&n)
	require.NoError(t, err)
	return n
}

func countAvailable(t *testing.T, db *sql.DB, generationID int64) int {
	t.Helper()
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM pending_embeddings WHERE generation_id = ? AND claimed_at IS NULL`,
		generationID,
	).Scan(&n)
	require.NoError(t, err)
	return n
}

func countClaimedByToken(t *testing.T, db *sql.DB, token string) int {
	t.Helper()
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM pending_embeddings WHERE claim_token = ?`, token).Scan(&n)
	require.NoError(t, err)
	return n
}

func backdateClaims(t *testing.T, db *sql.DB, age time.Duration) {
	t.Helper()
	_, err := db.Exec(
		`UPDATE pending_embeddings SET claimed_at = ? WHERE claimed_at IS NOT NULL`,
		time.Now().Add(-age).Unix(),
	)
	require.NoError(t, err)
}
