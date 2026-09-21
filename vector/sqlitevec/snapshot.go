package sqlitevec

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"

	"go.kenn.io/kit/vector"
)

// GenerationInfo describes one registered generation.
type GenerationInfo[G comparable] struct {
	Key         G
	Fingerprint string
	Dimension   int
	State       State
}

// Generations lists every registered generation in creation order,
// whatever its state. LiveGenerations remains the search-time view.
func (s *Store[K, G]) Generations(ctx context.Context) ([]GenerationInfo[G], error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT gen_key, fingerprint, dimension, state FROM %s ORDER BY ordinal`,
		s.generationsTable()))
	if err != nil {
		return nil, fmt.Errorf("list generations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var gens []GenerationInfo[G]
	for rows.Next() {
		var g GenerationInfo[G]
		var state string
		if err := rows.Scan(&g.Key, &g.Fingerprint, &g.Dimension, &state); err != nil {
			return nil, fmt.Errorf("scan generation: %w", err)
		}
		g.State = State(state)
		gens = append(gens, g)
	}
	return gens, rows.Err()
}

// Snapshot is one read transaction pinned to a generation. It lets a
// caller export the generation's covered documents and their vectors as
// they stood at one instant, for replication to another backend. Close it
// when done; every method fails after Close.
type Snapshot[K, G comparable] struct {
	store   *Store[K, G]
	tx      *sql.Tx
	gen     GenerationInfo[G]
	ordinal int64

	mu     sync.Mutex
	closed bool
}

// Snapshot opens a read transaction over gen. The returned Snapshot holds
// the transaction until Close.
func (s *Store[K, G]) Snapshot(ctx context.Context, gen G) (*Snapshot[K, G], error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin snapshot: %w", err)
	}
	var info GenerationInfo[G]
	var ordinal int64
	var state string
	err = tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT ordinal, gen_key, fingerprint, dimension, state FROM %s WHERE gen_key = ?`,
		s.generationsTable()), gen).Scan(&ordinal, &info.Key, &info.Fingerprint, &info.Dimension, &state)
	if err != nil {
		_ = tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("generation %v not ensured", gen)
		}
		return nil, fmt.Errorf("lookup generation %v: %w", gen, err)
	}
	info.State = State(state)
	return &Snapshot[K, G]{store: s, tx: tx, gen: info, ordinal: ordinal}, nil
}

// Generation returns the generation the snapshot is pinned to.
func (sn *Snapshot[K, G]) Generation() GenerationInfo[G] { return sn.gen }

// Close ends the read transaction. Calling it again is a no-op.
func (sn *Snapshot[K, G]) Close() error {
	sn.mu.Lock()
	defer sn.mu.Unlock()
	if sn.closed {
		return nil
	}
	sn.closed = true
	return sn.tx.Rollback()
}

func (sn *Snapshot[K, G]) ensureOpen() error {
	sn.mu.Lock()
	defer sn.mu.Unlock()
	if sn.closed {
		return errors.New("snapshot is closed")
	}
	return nil
}

// DocQuery narrows which documents a Snapshot streams and which columns of
// the caller's documents table accompany each key. Columns and OrderBy are
// bare identifiers of that table; Where is a predicate over it aliased as d,
// with ? placeholders bound from Args in order.
type DocQuery struct {
	Columns []string
	Where   string
	Args    []any
	OrderBy []string
}

func (q DocQuery) validate() error {
	for _, c := range q.Columns {
		if !identifierPattern.MatchString(c) {
			return fmt.Errorf("invalid column %q", c)
		}
	}
	for _, c := range q.OrderBy {
		if !identifierPattern.MatchString(c) {
			return fmt.Errorf("invalid order by column %q", c)
		}
	}
	return nil
}

// CoveredDocs streams the documents the generation currently covers: the
// generation has stamped them and, with a revision column, the stamped
// revision still matches. This is the same freshness rule QueryGeneration
// applies, so an export never carries a stale or unstamped document. Each
// row holds the document key followed by q.Columns in order, sorted by
// q.OrderBy or, when empty, by key. Close the rows before the Snapshot.
func (sn *Snapshot[K, G]) CoveredDocs(ctx context.Context, q DocQuery) (*sql.Rows, error) {
	if err := sn.ensureOpen(); err != nil {
		return nil, err
	}
	if err := q.validate(); err != nil {
		return nil, err
	}
	schema := sn.store.schema
	columns := make([]string, 0, len(q.Columns)+1)
	columns = append(columns, "d."+schema.IDColumn)
	for _, c := range q.Columns {
		columns = append(columns, "d."+c)
	}
	orderBy := q.OrderBy
	if len(orderBy) == 0 {
		orderBy = []string{schema.IDColumn}
	}
	order := make([]string, len(orderBy))
	for i, c := range orderBy {
		order[i] = "d." + c
	}
	where := sn.store.coveredPredicate("d", "stamp")
	if q.Where != "" {
		where += " AND (" + q.Where + ")"
	}
	query := fmt.Sprintf(`
SELECT %s
  FROM %s d
  LEFT JOIN %s stamp ON stamp.ordinal = ? AND stamp.doc_key = d.%s
 WHERE %s
 ORDER BY %s`,
		strings.Join(columns, ", "), schema.DocsTable, sn.store.stampsTable(), schema.IDColumn,
		where, strings.Join(order, ", "))
	rows, err := sn.tx.QueryContext(ctx, query, append([]any{sn.ordinal}, q.Args...)...)
	if err != nil {
		return nil, fmt.Errorf("covered documents: %w", err)
	}
	return rows, nil
}

// UncoveredCount counts the documents matching where (a predicate over the
// documents table aliased as d, with ? placeholders bound from args) that
// the generation does not currently cover. An empty where counts every
// uncovered document. A caller checks it before exporting so a generation
// still being filled is not replicated as complete.
func (sn *Snapshot[K, G]) UncoveredCount(ctx context.Context, where string, args ...any) (int64, error) {
	if err := sn.ensureOpen(); err != nil {
		return 0, err
	}
	schema := sn.store.schema
	predicate := "NOT " + sn.store.coveredPredicate("d", "stamp")
	if where != "" {
		predicate += " AND (" + where + ")"
	}
	query := fmt.Sprintf(`
SELECT COUNT(*)
  FROM %s d
  LEFT JOIN %s stamp ON stamp.ordinal = ? AND stamp.doc_key = d.%s
 WHERE %s`,
		schema.DocsTable, sn.store.stampsTable(), schema.IDColumn, predicate)
	var n int64
	if err := sn.tx.QueryRowContext(ctx, query, append([]any{sn.ordinal}, args...)...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count uncovered documents: %w", err)
	}
	return n, nil
}

// Chunks returns doc's stored vectors for the generation in chunk order,
// decoded to float32. It reads whatever the generation holds for doc;
// pair it with CoveredDocs from the same Snapshot so only current
// documents are exported.
func (sn *Snapshot[K, G]) Chunks(ctx context.Context, doc K) ([]vector.ChunkVector, error) {
	if err := sn.ensureOpen(); err != nil {
		return nil, err
	}
	rows, err := sn.tx.QueryContext(ctx, fmt.Sprintf(`
SELECT c.chunk_index, v.embedding
  FROM %s c
  JOIN %s v ON v.rowid = c.vec_rowid
 WHERE c.ordinal = ? AND c.doc_key = ?
 ORDER BY c.chunk_index`, sn.store.chunksTable(), sn.store.vecTable(sn.ordinal)), sn.ordinal, doc)
	if err != nil {
		return nil, fmt.Errorf("read chunks for %v: %w", doc, err)
	}
	defer func() { _ = rows.Close() }()

	var chunks []vector.ChunkVector
	for rows.Next() {
		var cv vector.ChunkVector
		var blob []byte
		if err := rows.Scan(&cv.ChunkIndex, &blob); err != nil {
			return nil, fmt.Errorf("scan chunk for %v: %w", doc, err)
		}
		cv.Vector, err = decodeVector(blob, sn.gen.Dimension)
		if err != nil {
			return nil, fmt.Errorf("chunk %d of %v: %w", cv.ChunkIndex, doc, err)
		}
		chunks = append(chunks, cv)
	}
	return chunks, rows.Err()
}

// decodeVector decodes sqlite-vec's little-endian float32 blob and checks
// it against the generation's dimension.
func decodeVector(blob []byte, dimension int) (vector.Vector, error) {
	if len(blob) != dimension*4 {
		return nil, fmt.Errorf("embedding blob is %d bytes, expected %d for %d dimensions",
			len(blob), dimension*4, dimension)
	}
	out := make(vector.Vector, dimension)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return out, nil
}
