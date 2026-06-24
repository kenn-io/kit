// Package encodingqueue provides SQLite-compatible queue helpers for
// vector encoding tasks.
//
// The package expects one pending-task table with a compound primary key
// over generation and task IDs, an enqueue timestamp, and nullable claim
// fields. Callers own schema creation and database lifecycle.
package encodingqueue
