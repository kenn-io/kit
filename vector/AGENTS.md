# vector package invariants

`go.kenn.io/kit/vector` owns the backend-neutral parts of an embedding
pipeline. Preserve these invariants when changing it.

## The storage boundary is the point of this package

- The core `vector` package must not import `database/sql`, a driver, or
  any backend client, and must not construct backend SQL. The `Fill` and
  `Search` flows reach storage only through the `Store[K, G]` interface.
- Persistence is a function of the caller's source system. Backends live
  in their own subpackages (e.g. `vector/sqlitevec`) so a caller wiring
  one backend never pulls another backend's driver. New backends
  (pgvector, duckdb) go in sibling subpackages, not into the core.
- Backends own query construction. The differences between sqlite-vec
  `vec0 MATCH`, pgvector `<=>`, and duckdb `array_distance` belong behind
  `QueryGeneration`, never in the core flows.

## Keys and generations are opaque

- Document identity is the caller's type `K` and generation identity its
  type `G`. msgvault uses `int64`; kata uses UUIDs. Compare them for
  equality only; never assume a type, a single id namespace, or an
  ordering. Backends additionally require `K`/`G` to be types
  `database/sql` can bind and scan.

## Merge semantics

- `Merge` takes per-generation lists in descending preference and keeps
  the earliest list's hit on overlap (prefer the newer generation during
  a migration). Coverage is a union — never drop a document that only one
  generation covers, and never emit duplicates.
- Cross-generation scores are not comparable. Default to
  `MergeNormalizedScore`; raw-score merging is opt-in.

## Generations during migration

- The mid-migration union exists because new documents land only in the
  building generation while the active generation still serves the bulk.
  `Search` must keep querying every generation `LiveGenerations` returns,
  in the order it returns them.
