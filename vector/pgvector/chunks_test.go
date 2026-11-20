package pgvector_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/pgvector"
)

var (
	cancelChunkScan      context.CancelFunc
	errCanceledChunkScan = errors.New("canceled during scan")
)

type cancelingChunkKey string

func (k *cancelingChunkKey) Scan(value any) error {
	switch value := value.(type) {
	case string:
		*k = cancelingChunkKey(value)
	case []byte:
		*k = cancelingChunkKey(value)
	default:
		return fmt.Errorf("unexpected key type %T", value)
	}
	if cancelChunkScan != nil {
		cancelChunkScan()
		return errCanceledChunkScan
	}
	return nil
}

func TestChunks(t *testing.T) {
	setupChunks := func(t *testing.T) (*sql.DB, *pgvector.Chunks[string]) {
		t.Helper()
		db := setupDB(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `CREATE TABLE "Mixed chunks" (doc_key text, chunk_index integer, embedding public.halfvec(2), PRIMARY KEY(doc_key,chunk_index))`)
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		require.NoError(t, err)
		chunks, err := pgvector.AttachChunks[string](ctx, tx, "Mixed chunks")
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		return db, chunks
	}
	sequence := func(key string, n int) iter.Seq2[pgvector.Chunk[string], error] {
		return func(yield func(pgvector.Chunk[string], error) bool) {
			for i := range n {
				if !yield(pgvector.Chunk[string]{Doc: key, ChunkIndex: i, Vector: vector.Vector{2, 1}}, nil) {
					return
				}
			}
		}
	}

	t.Run("nil transaction", func(t *testing.T) {
		_, chunks := setupChunks(t)
		consumed := false
		assert.NotPanics(t, func() {
			_, err := chunks.ReplaceTx(t.Context(), nil, []string{"key"}, func(func(pgvector.Chunk[string], error) bool) { consumed = true })
			assert.ErrorContains(t, err, "transaction")
		})
		assert.False(t, consumed)
		assert.NotPanics(t, func() {
			_, err := chunks.SearchTx(t.Context(), nil, vector.Vector{1, 0}, 1)
			assert.ErrorContains(t, err, "transaction")
		})
	})

	t.Run("trigger changes selected document", func(t *testing.T) {
		db := setupDB(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `CREATE TABLE chunks (doc_key text NOT NULL, chunk_index int NOT NULL, embedding public.halfvec(2)); CREATE FUNCTION change_key() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.doc_key := lower(NEW.doc_key); RETURN NEW; END $$; CREATE TRIGGER change_key BEFORE INSERT ON chunks FOR EACH ROW EXECUTE FUNCTION change_key()`)
		require.NoError(t, err)
		chunks, err := pgvector.AttachChunks[string](ctx, db, "chunks")
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = chunks.ReplaceTx(ctx, tx, []string{"A", "a"}, sequence("A", 1))
		require.ErrorContains(t, err, "outside the selected keys")
	})

	t.Run("search excludes unusable rows", func(t *testing.T) {
		db := setupDB(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `CREATE TABLE chunks (doc_key text NOT NULL, chunk_index int NOT NULL, embedding public.halfvec(2)); INSERT INTO chunks VALUES ('null', 0, NULL), ('zero', 0, '[0,0]'), ('valid', 2, '[1,1]'), ('valid2', 3, '[0,1]')`)
		require.NoError(t, err)
		chunks, err := pgvector.AttachChunks[string](ctx, db, "chunks")
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		page, err := chunks.SearchTx(ctx, tx, vector.Vector{1, 0}, 1)
		require.NoError(t, err)
		require.Len(t, page.Hits, 1)
		assert.Equal(t, "valid", page.Hits[0].Doc)
		assert.Equal(t, 2, page.Hits[0].ChunkIndex)
		page, err = chunks.SearchTx(ctx, tx, vector.Vector{1, 0}, 10)
		require.NoError(t, err)
		require.Len(t, page.Hits, 2)
		assert.Equal(t, []string{"valid", "valid2"}, []string{page.Hits[0].Doc, page.Hits[1].Doc})
	})

	t.Run("unsupported key leaves rows intact", func(t *testing.T) {
		db := setupDB(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `CREATE TYPE document_id AS ENUM ('key'); CREATE TABLE chunks (doc_key document_id NOT NULL, chunk_index int NOT NULL, embedding public.halfvec(2)); INSERT INTO chunks VALUES ('key', 0, '[1,0]')`)
		require.NoError(t, err)
		_, err = pgvector.AttachChunks[string](ctx, db, "chunks")
		require.ErrorContains(t, err, "doc_key")
		var count int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM chunks`).Scan(&count))
		assert.Equal(t, 1, count)
	})

	seed := func(t *testing.T, db *sql.DB, chunks *pgvector.Chunks[string], keys ...string) {
		t.Helper()
		ctx := t.Context()
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		n, err := chunks.ReplaceTx(ctx, tx, keys, func(yield func(pgvector.Chunk[string], error) bool) {
			for _, key := range keys {
				if !yield(pgvector.Chunk[string]{Doc: key, Vector: vector.Vector{2, 1}}, nil) {
					return
				}
			}
		})
		require.NoError(t, err)
		assert.Equal(t, len(keys), n)
		require.NoError(t, tx.Commit())
	}

	t.Run("replace batching", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		seed(t, db, chunks, "replace", "covered", "other")
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		n, err := chunks.ReplaceTx(ctx, tx, []string{"replace", "covered"}, func(yield func(pgvector.Chunk[string], error) bool) {
			for chunk, err := range sequence("replace", 201) {
				if chunk.ChunkIndex == 199 || chunk.ChunkIndex == 200 {
					var flushed int
					require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM "Mixed chunks" WHERE doc_key='replace'`).Scan(&flushed))
					if chunk.ChunkIndex == 199 {
						assert.Zero(t, flushed)
					} else {
						assert.Equal(t, 200, flushed)
					}
				}
				if !yield(chunk, err) {
					return
				}
			}
		})
		require.NoError(t, err)
		assert.Equal(t, 201, n)
		require.NoError(t, tx.Commit())
		var count int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM "Mixed chunks" WHERE doc_key='replace'`).Scan(&count))
		assert.Equal(t, 201, count)
	})

	t.Run("search", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `INSERT INTO "Mixed chunks" VALUES ('covered', 0, '[2,1.001]'), ('other', 0, '[2,1]')`)
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		require.NoError(t, err)
		page, err := chunks.SearchTx(ctx, tx, vector.Vector{1, 0}, 60)
		require.NoError(t, err)
		require.Len(t, page.Hits, 2)
		assert.Equal(t, []string{"other", "covered"}, []string{page.Hits[0].Doc, page.Hits[1].Doc})
		assert.InDelta(t, 0.8944272, page.Hits[0].Score, 1e-6)
		assert.InDelta(t, 0.8942525, page.Hits[1].Score, 1e-6)
		assert.Equal(t, 60, page.ScanCap)
		page, err = chunks.SearchTx(ctx, tx, vector.Vector{1, 0}, -1)
		require.NoError(t, err)
		assert.Empty(t, page.Hits)
		assert.Zero(t, page.ScanCap)
		require.NoError(t, tx.Rollback())
	})

	t.Run("search existing halfvec ranking", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `INSERT INTO "Mixed chunks" VALUES ('a', 0, '[0.08,0.9968]'), ('b', 0, '[0,1]')`)
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		page, err := chunks.SearchTx(ctx, tx, vector.Vector{1e-8, 1e-7}, 2)
		require.NoError(t, err)
		require.Len(t, page.Hits, 2)
		assert.Equal(t, "b", page.Hits[0].Doc)
	})

	t.Run("reject vectors rounded to zero", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = chunks.ReplaceTx(ctx, tx, []string{"selected"}, func(yield func(pgvector.Chunk[string], error) bool) {
			yield(pgvector.Chunk[string]{Doc: "selected", Vector: vector.Vector{1e-9, 0}}, nil)
		})
		require.ErrorContains(t, err, "zero norm")
		require.NoError(t, tx.Rollback())
		tx, err = db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = chunks.SearchTx(ctx, tx, vector.Vector{1e-9, 0}, 10)
		require.ErrorContains(t, err, "zero norm")
		require.NoError(t, tx.Rollback())
	})

	t.Run("search orders tied chunks", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `INSERT INTO "Mixed chunks" VALUES ('b', 2, '[1,0]'), ('b', 0, '[1,0]'), ('a', 1, '[1,0]'), ('a', 0, '[1,0]')`)
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		page, err := chunks.SearchTx(ctx, tx, vector.Vector{1, 0}, 10)
		require.NoError(t, err)
		require.Len(t, page.Hits, 4)
		assert.Equal(t, []string{"a", "a", "b", "b"}, []string{page.Hits[0].Doc, page.Hits[1].Doc, page.Hits[2].Doc, page.Hits[3].Doc})
		assert.Equal(t, []int{0, 1, 0, 2}, []int{page.Hits[0].ChunkIndex, page.Hits[1].ChunkIndex, page.Hits[2].ChunkIndex, page.Hits[3].ChunkIndex})
	})

	t.Run("search dimension error", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		seed(t, db, chunks, "selected")
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = chunks.SearchTx(ctx, tx, vector.Vector{1, 0, 0}, 1200)
		require.ErrorContains(t, err, "different halfvec dimensions")
		assert.NotContains(t, err.Error(), "restore hnsw")
	})

	t.Run("large limit search", func(t *testing.T) {
		db := setupDB(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `CREATE TABLE chunks (doc_key text NOT NULL, chunk_index integer NOT NULL, embedding public.halfvec(3))`)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO chunks SELECT i::text, 0, format('[%s,%s,%s]', sin(i*0.91), cos(i*1.17), sin(i*0.23))::public.halfvec FROM generate_series(1,1600) i`)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `CREATE INDEX chunks_hnsw_idx ON chunks USING hnsw (embedding public.halfvec_cosine_ops)`)
		require.NoError(t, err)
		chunks, err := pgvector.AttachChunks[string](ctx, db, "chunks")
		require.NoError(t, err)
		// Search on a fresh connection where pgvector has not yet been loaded.
		db.SetMaxIdleConns(0)
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx, `SET LOCAL enable_seqscan=off`)
		require.NoError(t, err)
		page, err := chunks.SearchTx(ctx, tx, vector.Vector{1, 0.3, 0.1}, 1200)
		require.NoError(t, err)
		require.Len(t, page.Hits, 1200)
		assert.Equal(t, 1200, page.ScanCap)
		for i := 1; i < len(page.Hits); i++ {
			assert.GreaterOrEqual(t, page.Hits[i-1].Score, page.Hits[i].Score)
		}
		_, err = tx.ExecContext(ctx, `SELECT pg_stat_force_next_flush()`)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		var scans int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT pg_stat_get_numscans('chunks_hnsw_idx'::regclass)`).Scan(&scans))
		assert.Positive(t, scans)
	})

	t.Run("replace reports stream and vector errors", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		wantErr := errors.New("stream failed")
		_, err = chunks.ReplaceTx(ctx, tx, []string{"replace"}, func(yield func(pgvector.Chunk[string], error) bool) {
			if !yield(pgvector.Chunk[string]{Doc: "replace", Vector: vector.Vector{2, 1}}, nil) {
				return
			}
			yield(pgvector.Chunk[string]{}, wantErr)
		})
		require.ErrorIs(t, err, wantErr)
		_, err = chunks.ReplaceTx(ctx, tx, []string{"valid"}, func(yield func(pgvector.Chunk[string], error) bool) {
			yield(pgvector.Chunk[string]{Doc: "valid", ChunkIndex: 7, Vector: vector.Vector{0, 0}}, nil)
		})
		require.ErrorContains(t, err, "document valid chunk 7")
		require.NoError(t, tx.Rollback())
	})
	t.Run("clear selected keys", func(t *testing.T) {
		db, chunks := setupChunks(t)
		ctx := t.Context()
		seed(t, db, chunks, "selected", "other")
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		n, err := chunks.ReplaceTx(ctx, tx, []string{"selected"}, nil)
		require.NoError(t, err)
		assert.Zero(t, n)
		require.NoError(t, tx.Commit())
		var count int
		var remaining string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*), min(doc_key) FROM "Mixed chunks"`).Scan(&count, &remaining))
		assert.Equal(t, 1, count)
		assert.Equal(t, "other", remaining)
	})

	t.Run("attachment columns", func(t *testing.T) {
		db := setupDB(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `CREATE TABLE missing_position (doc_key text NOT NULL, embedding public.halfvec(2)); CREATE TABLE missing_key (chunk_index int NOT NULL, embedding public.halfvec(2))`)
		require.NoError(t, err)
		_, err = pgvector.AttachChunks[string](ctx, db, "missing_position")
		require.ErrorContains(t, err, "fixed-dimension halfvec")
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		schema := uniqueName(t, "fake_halfvec")
		_, err = tx.ExecContext(ctx, `CREATE SCHEMA `+schema+`; CREATE DOMAIN `+schema+`.halfvec AS text; CREATE TABLE fake_chunks (doc_key text NOT NULL, chunk_index int NOT NULL, embedding `+schema+`.halfvec)`)
		require.NoError(t, err)
		_, err = pgvector.AttachChunks[string](ctx, tx, "fake_chunks")
		require.ErrorContains(t, err, "fixed-dimension halfvec")
		_, err = pgvector.AttachChunks[string](ctx, db, "missing_key")
		require.ErrorContains(t, err, "fixed-dimension halfvec")
	})

	t.Run("attachment requires supported columns", func(t *testing.T) {
		for _, tc := range []struct {
			name, columns string
			valid         bool
		}{
			{name: "text integer", columns: `doc_key text NOT NULL, chunk_index integer NOT NULL, embedding public.halfvec(2)`, valid: true},
			{name: "text bigint", columns: `doc_key text NOT NULL, chunk_index bigint NOT NULL, embedding public.halfvec(2)`, valid: true},
			{name: "bigint integer", columns: `doc_key bigint NOT NULL, chunk_index integer NOT NULL, embedding public.halfvec(2)`, valid: true},
			{name: "bigint bigint", columns: `doc_key bigint NOT NULL, chunk_index bigint NOT NULL, embedding public.halfvec(2)`, valid: true},
			{name: "uuid key", columns: `doc_key uuid NOT NULL, chunk_index integer NOT NULL, embedding public.halfvec(2)`},
			{name: "varchar key", columns: `doc_key varchar(8) NOT NULL, chunk_index integer NOT NULL, embedding public.halfvec(2)`},
			{name: "domain key", columns: `doc_key docid NOT NULL, chunk_index integer NOT NULL, embedding public.halfvec(2)`},
			{name: "nullable key", columns: `doc_key text, chunk_index integer NOT NULL, embedding public.halfvec(2)`},
			{name: "nullable position", columns: `doc_key text NOT NULL, chunk_index integer, embedding public.halfvec(2)`},
			{name: "text position", columns: `doc_key text NOT NULL, chunk_index text NOT NULL, embedding public.halfvec(2)`},
			{name: "smallint position", columns: `doc_key text NOT NULL, chunk_index smallint NOT NULL, embedding public.halfvec(2)`},
			{name: "domain position", columns: `doc_key text NOT NULL, chunk_index position_type NOT NULL, embedding public.halfvec(2)`},
			{name: "variable dimensions", columns: `doc_key text NOT NULL, chunk_index integer NOT NULL, embedding public.halfvec`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				db := setupDB(t)
				ctx := t.Context()
				_, err := db.ExecContext(ctx, `CREATE DOMAIN docid AS text; CREATE DOMAIN position_type AS integer; CREATE TABLE chunks (`+tc.columns+`)`)
				require.NoError(t, err)
				_, err = pgvector.AttachChunks[any](ctx, db, "chunks")
				if tc.valid {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			})
		}
	})

	t.Run("null selected key", func(t *testing.T) {
		db := setupDB(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `CREATE TABLE chunks (doc_key text NOT NULL, chunk_index int NOT NULL, embedding public.halfvec(2))`)
		require.NoError(t, err)
		chunks, err := pgvector.AttachChunks[sql.NullString](ctx, db, "chunks")
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = chunks.ReplaceTx(ctx, tx, []sql.NullString{{Valid: false}}, nil)
		require.ErrorContains(t, err, "null")
	})

	t.Run("search path", func(t *testing.T) {
		t.Run("catalog functions", func(t *testing.T) {
			db := setupDB(t)
			ctx := t.Context()
			_, err := db.ExecContext(ctx, `CREATE TABLE chunks (doc_key text NOT NULL, chunk_index int NOT NULL, embedding public.halfvec(2)); INSERT INTO chunks VALUES ('key', 0, '[1,0]'); CREATE TABLE calls (n int); CREATE FUNCTION unnest(text[]) RETURNS SETOF text LANGUAGE plpgsql AS $$ BEGIN INSERT INTO calls VALUES (1); RETURN; END $$; CREATE FUNCTION format(text, name, name) RETURNS text LANGUAGE sql AS 'SELECT ''wrong''::text'`)
			require.NoError(t, err)
			tx, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			_, err = tx.ExecContext(ctx, `SELECT set_config('search_path', current_schema() || ', pg_catalog', true)`)
			require.NoError(t, err)
			chunks, err := pgvector.AttachChunks[string](ctx, tx, "chunks")
			require.NoError(t, err)
			_, err = chunks.ReplaceTx(ctx, tx, []string{"key"}, nil)
			require.NoError(t, err)
			var calls, rows int
			require.NoError(t, tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM calls), (SELECT count(*) FROM chunks)`).Scan(&calls, &rows))
			assert.Zero(t, calls)
			assert.Zero(t, rows)
		})
		t.Run("attachment follows resolved table", func(t *testing.T) {
			db, chunks := setupChunks(t)
			ctx := t.Context()
			seed(t, db, chunks, "original")
			tx, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			_, err = tx.ExecContext(ctx, `CREATE TEMP TABLE "Mixed chunks" (doc_key text NOT NULL, chunk_index int NOT NULL, embedding public.halfvec(2)); INSERT INTO "Mixed chunks" VALUES ('shadow',0,'[1,0]'); SET LOCAL search_path=pg_temp`)
			require.NoError(t, err)
			page, err := chunks.SearchTx(ctx, tx, vector.Vector{1, 0}, 10)
			require.NoError(t, err)
			require.Len(t, page.Hits, 1)
			assert.Equal(t, "original", page.Hits[0].Doc)
			_, err = chunks.ReplaceTx(ctx, tx, []string{"original"}, sequence("original", 1))
			require.NoError(t, err)
			var shadowCount int
			require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM "Mixed chunks"`).Scan(&shadowCount))
			assert.Equal(t, 1, shadowCount)
		})
		t.Run("key equality follows the column", func(t *testing.T) {
			db := setupDB(t)
			ctx := t.Context()
			t.Run("bigint", func(t *testing.T) {
				_, err := db.ExecContext(ctx, `CREATE TABLE bigint_chunks (doc_key bigint NOT NULL, chunk_index bigint NOT NULL, embedding public.halfvec(2))`)
				require.NoError(t, err)
				chunks, err := pgvector.AttachChunks[int64](ctx, db, "bigint_chunks")
				require.NoError(t, err)
				tx, err := db.BeginTx(ctx, nil)
				require.NoError(t, err)
				defer func() { _ = tx.Rollback() }()
				n, err := chunks.ReplaceTx(ctx, tx, []int64{12}, func(yield func(pgvector.Chunk[int64], error) bool) {
					yield(pgvector.Chunk[int64]{Doc: 12, ChunkIndex: 7, Vector: vector.Vector{1, 0}}, nil)
				})
				require.NoError(t, err)
				assert.Equal(t, 1, n)
				page, err := chunks.SearchTx(ctx, tx, vector.Vector{1, 0}, 1)
				require.NoError(t, err)
				require.Len(t, page.Hits, 1)
				assert.Equal(t, int64(12), page.Hits[0].Doc)
				assert.Equal(t, 7, page.Hits[0].ChunkIndex)
			})
			t.Run("collation", func(t *testing.T) {
				_, err := db.ExecContext(ctx, `CREATE COLLATION case_insensitive (provider = icu, locale = 'und-u-ks-level2', deterministic = false); CREATE TABLE collated_chunks (doc_key text COLLATE case_insensitive NOT NULL, chunk_index int NOT NULL, embedding public.halfvec(2))`)
				require.NoError(t, err)
				chunks, err := pgvector.AttachChunks[string](ctx, db, "collated_chunks")
				require.NoError(t, err)
				tx, err := db.BeginTx(ctx, nil)
				require.NoError(t, err)
				defer func() { _ = tx.Rollback() }()
				n, err := chunks.ReplaceTx(ctx, tx, []string{"KEY"}, sequence("key", 1))
				require.NoError(t, err)
				assert.Equal(t, 1, n)
				require.NoError(t, tx.Rollback())
			})

			t.Run("citext", func(t *testing.T) {
				_, err := db.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS citext WITH SCHEMA public; CREATE TABLE chunks (doc_key public.citext NOT NULL, chunk_index int NOT NULL, embedding public.halfvec(2))`)
				require.NoError(t, err)
				_, err = pgvector.AttachChunks[string](ctx, db, "chunks")
				require.ErrorContains(t, err, "doc_key")
			})
		})
	})

	t.Run("timestamp keys are unsupported", func(t *testing.T) {
		for _, tc := range []struct{ name, ddl string }{
			{name: "column", ddl: `CREATE TABLE timed_chunks (doc_key timestamptz(0) NOT NULL, chunk_index int NOT NULL, embedding public.halfvec(2))`},
			{name: "domain", ddl: `CREATE DOMAIN seconds AS timestamptz(0); CREATE TABLE timed_chunks (doc_key seconds NOT NULL, chunk_index int NOT NULL, embedding public.halfvec(2))`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				db := setupDB(t)
				_, err := db.ExecContext(t.Context(), tc.ddl)
				require.NoError(t, err)
				_, err = pgvector.AttachChunks[any](t.Context(), db, "timed_chunks")
				require.ErrorContains(t, err, "doc_key")
			})
		}
	})

	t.Run("selected keys are stable during streaming", func(t *testing.T) {
		db := setupDB(t)
		ctx := t.Context()
		_, err := db.ExecContext(ctx, `CREATE TABLE binary_keys (doc_key text NOT NULL, chunk_index bigint NOT NULL, embedding public.halfvec(2))`)
		require.NoError(t, err)
		chunks, err := pgvector.AttachChunks[any](ctx, db, "binary_keys")
		require.NoError(t, err)
		for _, overwrite := range []bool{false, true} {
			tx, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			key := []byte("a")
			keys := []any{key}
			_, err = chunks.ReplaceTx(ctx, tx, keys, func(yield func(pgvector.Chunk[any], error) bool) {
				if overwrite {
					keys[0] = []byte("b")
				} else {
					key[0] = 'b'
				}
				yield(pgvector.Chunk[any]{Doc: []byte("b"), Vector: vector.Vector{1, 0}}, nil)
			})
			require.ErrorContains(t, err, "outside the selected keys")
			require.NoError(t, tx.Rollback())
		}
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		key := []byte("a")
		_, err = chunks.ReplaceTx(ctx, tx, []any{[]byte("a"), []byte("b")}, func(yield func(pgvector.Chunk[any], error) bool) {
			if !yield(pgvector.Chunk[any]{Doc: key, ChunkIndex: 0, Vector: vector.Vector{1, 0}}, nil) {
				return
			}
			key[0] = 'b'
			yield(pgvector.Chunk[any]{Doc: key, ChunkIndex: 1, Vector: vector.Vector{1, 0}}, nil)
		})
		require.NoError(t, err)
		rows, err := tx.QueryContext(ctx, `SELECT doc_key FROM binary_keys ORDER BY chunk_index`)
		require.NoError(t, err)
		defer rows.Close()
		var keys []string
		for rows.Next() {
			var key []byte
			require.NoError(t, rows.Scan(&key))
			keys = append(keys, string(key))
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		assert.Equal(t, []string{"a", "b"}, keys)
		require.NoError(t, tx.Rollback())
	})

	t.Run("search restores settings", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			cancel bool
		}{
			{name: "success"},
			{name: "canceled scan", cancel: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				db := setupDB(t)
				ctx := t.Context()
				_, err := db.ExecContext(ctx, `CREATE TABLE chunks (doc_key text NOT NULL, chunk_index int NOT NULL, embedding public.halfvec(2)); INSERT INTO chunks VALUES ('key', 0, '[1,0]')`)
				require.NoError(t, err)
				chunks, err := pgvector.AttachChunks[cancelingChunkKey](ctx, db, "chunks")
				require.NoError(t, err)
				tx, err := db.BeginTx(ctx, nil)
				require.NoError(t, err)
				defer func() { _ = tx.Rollback() }()
				_, err = tx.ExecContext(ctx, `SET LOCAL hnsw.ef_search=77; SET LOCAL hnsw.iterative_scan='strict_order'`)
				require.NoError(t, err)
				searchCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				if tc.cancel {
					cancelChunkScan = cancel
					defer func() { cancelChunkScan = nil }()
				}
				_, err = chunks.SearchTx(searchCtx, tx, vector.Vector{1, 0}, 1200)
				if tc.cancel {
					require.ErrorIs(t, err, errCanceledChunkScan)
					require.ErrorIs(t, searchCtx.Err(), context.Canceled)
				} else {
					require.NoError(t, err)
				}
				var ef, iterative string
				require.NoError(t, tx.QueryRowContext(ctx, `SELECT current_setting('hnsw.ef_search'), current_setting('hnsw.iterative_scan')`).Scan(&ef, &iterative))
				assert.Equal(t, "77", ef)
				assert.Equal(t, "strict_order", iterative)
			})
		}
	})
}
