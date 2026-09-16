package a

const schema = `CREATE TABLE jobs (id INTEGER PRIMARY KEY, status TEXT NOT NULL CHECK(status IN ('queued','running','done')) DEFAULT 'queued', retries INTEGER CHECK (retries >= 0));` // want "CHECK constraint hard-codes the allowed values of status"

var migration = "ALTER TABLE jobs ADD COLUMN kind TEXT CHECK (kind = 'a' OR kind = 'b')" // want "CHECK constraint hard-codes the allowed values of kind"

var fine = "CREATE TABLE ok (name TEXT CHECK (length(name) > 0))"

var nullable = "ALTER TABLE jobs ADD COLUMN error_code TEXT CHECK (error_code IS NULL OR error_code IN ('timeout', 'refused'))" // want "CHECK constraint hard-codes the allowed values of error_code"

var pg = `CREATE TYPE job_status AS ENUM ('queued', 'done');` // want "enum type job_status hard-codes its allowed values"

var invariant = "CREATE TABLE runs (state TEXT, finished_at INTEGER, CHECK ((state = 'done' AND finished_at IS NOT NULL) OR (state <> 'done' AND finished_at IS NULL)))"
