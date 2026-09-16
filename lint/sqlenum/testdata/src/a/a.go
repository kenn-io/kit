package a

const schema = `CREATE TABLE jobs (id INTEGER PRIMARY KEY, status TEXT NOT NULL CHECK(status IN ('queued','running','done')) DEFAULT 'queued', retries INTEGER CHECK (retries >= 0));` // want "CHECK constraint hard-codes the allowed values of status"

var migration = "ALTER TABLE jobs ADD COLUMN kind TEXT CHECK (kind = 'a' OR kind = 'b')" // want "CHECK constraint hard-codes the allowed values of kind"

var fine = "CREATE TABLE ok (name TEXT CHECK (length(name) > 0))"
