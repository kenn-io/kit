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

## Encoded vectors must be usable for cosine distance

- `EncodeBatched` rejects encoder output that has the right vector count
  but cannot participate in cosine distance — a non-finite component or a
  zero-norm vector — with an error wrapping `*InvalidVectorError`. `Fill`
  and `Search` route every encode through it, so faulty endpoint output
  never reaches a `Store`. Do not weaken this to a skip or a warning: a
  stamped invalid vector looks complete forever and silently poisons
  search rankings.

## Fill batches without losing document boundaries

- A positive `FillOptions.Batch.BatchSize` packs chunks across documents in
  one scan page; values less than or equal to zero preserve the legacy
  per-document encode unit. `BatchSize` remains the maximum texts in one
  `EncodeFunc` call.
- Vectors from a shared encode batch must be scattered back to their exact
  document and chunk indexes before `SaveVectors`. Saves and `OnEncodeError`
  remain serialized and per document.
- If a shared encode batch fails, isolate its document slices on the error
  path. Skip only documents that fail in isolation; if every isolated request
  succeeds, keep the original unattributed batch error fatal instead of hiding
  a request-shape or transport failure.
- With one fill worker, finish the current bounded encode window and its saves
  before starting another window. A save failure must not launch later encode
  work.

## Keys and generations are opaque

- Document identity is the caller's type `K` and generation identity its
  type `G`. msgvault uses `int64`; kata uses UUIDs. Compare them for
  equality only; never assume a type, a single id namespace, or an
  ordering. Backends additionally require `K`/`G` to be types
  `database/sql` can bind and scan.

## The stamp is conditional

- `SaveVectors` receives the `Pending.Revision` token read with the
  content. A store that tracks revisions must persist nothing and return
  an error wrapping `ErrStale` when the document's revision has changed
  since the scan — never stamp stale vectors over a concurrent edit. The
  document stays pending and the next fill re-reads it.
- `Fill` treats `ErrStale` as "leave it for the next run", not a failure:
  the document is excluded for the rest of the run so an actively edited
  document cannot starve the loop, and the scan limit is widened past
  excluded documents so fresh work stays visible.
- An empty vectors slice is a stamp-only save: the document is marked
  handled for the generation without storing vectors. Fill relies on this
  both for empty content and for documents `OnEncodeError` elects to
  skip after a permanent encode failure — without it, one poison document
  would wedge every future fill.

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

## Hits come from live, current documents

- Backends must not return hits whose source row no longer exists; the
  caller may delete documents without telling the store, so
  `QueryGeneration` joins back to the documents table.
- Hits must also be current for the searched generation: stale vectors
  for an edited or invalidated document must never surface between the
  edit and the next fill, or a search can leak removed or redacted text.
  In sqlitevec, searchable and pending are exact complements of one
  shared freshness predicate (`coveredPredicate`) used by
  `PendingForGeneration`, `QueryGeneration`, and invalidation checks —
  never fork a new freshness expression for one read path.
- Freshness is per generation (sqlitevec: the stamps table), never
  `embed_gen = searched generation`. The embed-gen column records just
  the newest generation, so comparing it to the searched generation
  would wrongly hide the active generation's valid hits while
  generations overlap and break the union.
- Filtering hides stale and orphan hits but the vectors still occupy
  KNN slots; replacement happens at the next `SaveVectors`, and deletion
  cleanup (`sqlitevec.DeleteVectors`) is the caller's responsibility
  when removing documents.
