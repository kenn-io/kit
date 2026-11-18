package pgvector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Install creates the managed catalog using the owner role.
func Install(ctx context.Context, db *sql.DB, schema Schema) error {
	if err := schema.validate(); err != nil {
		return err
	}
	if db == nil {
		return errors.New("db is nil")
	}
	_, createErr := db.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS vector`)
	_, err := extensionSchema(ctx, db)
	if err != nil {
		return errors.Join(err, createErr)
	}
	var keyType, revType string
	columnType := func(column string, dst *string) error {
		return db.QueryRowContext(ctx, `SELECT format_type(a.atttypid,a.atttypmod) FROM pg_attribute a WHERE a.attrelid=to_regclass($1) AND a.attname=$2 AND a.attnum>0 AND NOT a.attisdropped`, quote(schema.DocsTable), column).Scan(dst)
	}
	if err := columnType(schema.IDColumn, &keyType); err != nil {
		return fmt.Errorf("document key type: %w", err)
	}
	if err := columnType(schema.RevisionColumn, &revType); err != nil {
		return fmt.Errorf("revision type: %w", err)
	}
	_, err = db.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (id BIGSERIAL PRIMARY KEY, gen_key %s UNIQUE NOT NULL, fingerprint TEXT NOT NULL, dimension INTEGER NOT NULL, state TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS %s (generation_id BIGINT NOT NULL, doc_key %s NOT NULL, revision %s, PRIMARY KEY(generation_id,doc_key))`, quote(schema.Prefix+"_generations"), quote(schema.GenerationKeyType), quote(schema.Prefix+"_stamps"), keyType, revType))
	if err != nil {
		return fmt.Errorf("install vector catalog: %w", err)
	}
	return nil
}
