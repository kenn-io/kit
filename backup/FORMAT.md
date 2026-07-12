# Backup Repository Format

On-disk format reference for repositories created by the `backup` and `pack`
packages — layout, object encodings, versioning, crash-consistency, and the
freeze protocol. It documents the invariants an implementation must preserve,
in enough detail to audit a repository by hand or reimplement a reader.

The engine is application-agnostic: an application supplies its own database
filename, content-directory name, and schema-specific stats/content-path
logic through the `App` interface (`app.go`). The engine treats the
manifest's application version and stats payload as opaque bytes — it
records them at create and byte-compares them at restore. Everything below
applies uniformly to every application built on this engine.

## Design Goals

- **Local-first, tool-agnostic.** A repository is a plain directory of write-once files. Any file-sync tool can replicate it; no server or database is required to read it.
- **Content-addressed and deduplicated.** Every stored object is a blob named by the SHA-256 of its (uncompressed, unencrypted) content. Identical content is stored once, across snapshots and across data types.
- **Crash-safe by construction.** A snapshot exists if and only if its manifest file exists, and the manifest is written last. There is no repair step and no journal.
- **Verifiable.** Every container and metadata object carries integrity checksums; full verification re-derives every referenced blob's identity from its bytes.
- **Versioned at every level.** Readers refuse what they cannot safely interpret rather than guessing.

## Repository Layout

```
<repo>/
  config.toml              # repo identity + format versions (plain TOML)
  snapshots/
    <snapshot-id>.mvmanifest   # JSON manifest, written LAST
  packs/
    <aa>/<ulid><ext>       # sealed blob containers (~32 MB target), aa = first ID byte;
                           # ext is the application-chosen extension (App.PackFileExtension)
  indexes/
    <ulid>.mvidx           # immutable blob -> (pack, offset) indexes
  locks/                   # exclusive.json / shared-<ulid>.json
  staging/                 # temp files; atomically renamed into place
```

All multi-byte integers in every binary object are **little-endian**. All timestamps are UTC.

## Versioning Model

Compatibility is enforced at three levels, all of which must pass:

1. **Repository level.** `config.toml` records `repo_id` (a lowercase-hex UUID; readers refuse any other shape, because the ID is embedded verbatim in local cache filenames), `format_version` (what wrote it), and `min_reader_version` (the oldest format a reader must understand). `Open` refuses a repository whose `min_reader_version` exceeds the reader's supported version, with an explicit error telling the caller to upgrade the reader. A future format change that old readers can safely ignore bumps only `format_version`; a change they cannot safely ignore also bumps `min_reader_version`.
2. **Object level.** Every binary object begins with a 4-byte magic and a version field, and every decoder rejects an unknown magic or version. A reader can therefore never misparse an object from a future format as if it were current.
3. **Snapshot level.** Each manifest records its own `format_version`, `min_reader_version`, and the application version string that wrote it (wire key `msgvault_version`, frozen for compatibility across every application built on this engine), so compatibility can evolve per-snapshot within one repository (for example, when a future version introduces encrypted snapshots alongside existing plaintext ones). Version 2 marks snapshots whose attachment population records storage paths beyond the canonical `<aa>/<hash>` derivation: version-1 readers placed every restored attachment at the canonical path and would materialize a database pointing at files that do not exist, so they must refuse these snapshots. Snapshots whose recorded paths are all canonical keep version 1. A manifest whose `min_reader_version` a reader accepts must contain only fields that reader knows: the content-derived ID covers only known fields, so an unknown field would otherwise ride along in an authenticated manifest, and readers refuse it as forged rather than ignore it.

Integrity is separate from versioning: every metadata object ends with a SHA-256 trailer over everything before it, checked before any field is interpreted, and pack entries carry CRC32-C over the stored bytes.

## Blob Identity

```
BlobID = SHA-256(plaintext content)
```

The ID is always computed over the raw content — before compression, before any future encryption. Compression and encryption are storage transforms recorded per-entry in the pack; they never change identity. This is what makes deduplication stable across compression-level changes and future format evolution.

## Pack Files

Blobs are appended into pack files sealed at a ~32 MB target. A sealed pack is never modified.

The pack file format is identified by its `MVPK` header magic, not by the file's name: the
file extension is application-chosen (`App.PackFileExtension`), and `.kpack` is the
recommended convention. An application must keep its chosen extension fixed for the life of a
repository — packs are located by `<packID><ext>`, so changing it strands previously written
packs — and, for encrypted repositories, renaming a pack file also breaks it: the pack ID
derived from the filename (basename minus extension) participates in the footer's AAD.

```
header:   "MVPK" | version u8 (=1) | flags u8
frames:   one frame per blob, concatenated
footer:   entry table | footer trailer ("KPVM" magic, SHA-256 over footer region)
```

Each footer entry records the blob ID, offset, stored length, raw length, CRC32-C of the stored bytes, and per-blob flags (`compressed`, `encrypted`). Each frame is either the raw content or a zstd frame: compression (level 3 by default, `zstd_level` configurable 1–19) is kept only if it saves at least 3%, so already-compressed content (most media attachments) is stored raw rather than burning CPU for nothing. Raw blob size is capped at 4 GiB (`maxRawLen`), and readers reject stored lengths beyond that bound plus a small overhead allowance before allocating.

Reading a blob verifies, in order: the footer trailer hash (at open), the entry CRC over stored bytes, then — after decompression — that SHA-256 of the result equals the blob ID.

Plain format-v1 frames may be consumed incrementally without changing their
encoding. In that mode, bytes read before terminal EOF are an unverified
prefix; the CRC, decoded length, trailing-data check, and blob identity become
authoritative only when the stream reaches its successful terminal result.
Closing early is not success. Encrypted format-v1 frames retain whole-entry
authentication and therefore remain buffered; safely streaming encrypted
prefixes requires a future chunk-authenticated format.

## Index Objects (`.mvidx`, magic `MVIX`)

Immutable mappings from blob ID to pack location, written once per `create` after its packs are sealed:

```
"MVIX" | version u16 (=1) | entry_count u32 |
entries: blob_id [32] | pack_ulid [16] | offset u64 | stored_len u64 | flags u8   (65 bytes each)
SHA-256 trailer
```

Entries are strictly sorted by blob ID; decoders reject unsorted or duplicate entries. Readers load the union of all index files. An index file orphaned by an interrupted backup (index written, manifest never written) is safe by construction: indexes are only ever written after their packs are durably sealed, so an orphan references real, valid blobs and deduplicating against it is correct.

## Page-Hash Objects (magic `MVHK` keyframe / `MVHD` delta)

The incremental-capture state: the truncated SHA-256 (first 16 bytes) of every 4 KB database page.

```
keyframe: "MVHK" | version u16 | page_size u32 | page_count u64 | hashes (page_count x 16) | trailer
delta:    "MVHD" | version u16 | page_size u32 | new page_count u64 | entry_count u32 |
          pages (u64 each, strictly ascending) | hashes (entry_count x 16) | trailer
```

Applying a delta resizes to the new page count (growth zero-fills, shrinking truncates) and patches the listed pages. All count and size fields are validated overflow-safely against the actual body length before any allocation.

## Page-Map Objects (magic `MVMK` keyframe / `MVMD` delta)

Where each database page's content lives, as sorted, non-overlapping runs:

```
"MVMK"/"MVMD" | version u16 | page_size u32 | page_count u64 | blob_count u32 |
blob table (32-byte blob IDs) | run_count u32 |
runs: start_page u64 | page_count u32 | blob_index u32 | blob_offset u64   (24 bytes each)
SHA-256 trailer
```

A keyframe must cover `[0, page_count)` with no gaps; deltas are sparse. Delta application unions the blob tables, subtracts the delta's intervals from the base runs (splitting runs with byte-exact offset adjustment), and merges — a linear sweep over both sorted run lists. Materializing a snapshot's map and concatenating the referenced page ranges reproduces the database file byte-for-byte; the end-to-end test asserts exactly that.

**Capture grouping:** contiguous dirty ranges of ≥ 256 pages become dedicated blobs, split at 1024 pages (4 MiB); smaller scattered ranges are grouped into shared blobs of at most 1024 pages.

**Keyframe cadence:** a snapshot writes fresh keyframes (instead of deltas) when the chain would exceed 30 deltas or when the accumulated deltas' stored size exceeds the previous keyframe's, bounding both chain-walk depth and wasted space. Chain walks independently enforce cycle detection and the depth bound, so corrupted parent links fail deterministically.

## Attachment Lists (magic `MVAL`)

```
"MVAL" | version u16 (=1) | entry_count u32 |
entries: content SHA-256 [32] | size u64   (40 bytes, first-seen order)
SHA-256 trailer
```

A snapshot's manifest references one or more list blobs whose union is exactly the attachment population of that snapshot. In the common append-only case, a snapshot inherits its parent's list blobs and adds one new segment; when the live set has shrunk (attachments were deleted), the snapshot writes one fresh full list instead, so the union invariant holds in both directions. Attachment content is re-read and re-hashed at every capture — from the attachments directory, or from an application-supplied `ContentSource` when `CreateOptions.ContentSource` is set — and content whose bytes no longer match the recorded hash fails the backup rather than being stored wrong. The wire format is identical either way; the source only changes where capture acquires bytes.

## Snapshot Manifests

A manifest is indented JSON with a fixed field set: format versions, `snapshot_id`, `parent_id`, `created_at` (RFC 3339 UTC), capture options, database geometry and page-map/hash-map chain heads, attachment lists and totals, extras tree, exclusions, stats, the packs and index added, duration, and bytes added.

**Snapshot ID derivation:**

```
snapshot_id = <UTC yyyymmddTHHMMSSZ> + "-" + first 32 hex chars (128 bits) of
              SHA-256(compact JSON of the manifest with snapshot_id = "")
```

The ID is content-derived: identical content at the same second produces the same ID, and any change to the manifest changes it. Readers recompute the ID from the manifest body on load and refuse a mismatch with the filename or embedded `snapshot_id`, so a renamed, corrupted, or forged manifest is rejected; the 128-bit digest keeps crafting a different manifest with the same ID computationally infeasible. `create` additionally enforces **strictly monotonic timestamps** per repository (bumping past the parent's second when two snapshots land within one second), so lexicographic ID order is chronological order and parent selection is deterministic.

**Trust model.** The content-derived ID is tamper-evidence, not cryptographic authentication: it is an unkeyed hash binding a manifest's content to its filename, so corruption, renames, and serving different content under a known ID are all detected — but an actor who can write to the repository can add a wholly new, internally consistent snapshot (packs, index, and manifest with a correctly derived ID) that readers accept, and an empty `SnapshotID` restores whatever snapshot sorts latest. Restoring from storage that other principals can write therefore requires pinning the expected snapshot ID through a trusted channel (`RestoreOptions.SnapshotID`); repository encryption (planned, below) closes this fully by binding every object to a key the repository does not store.

Manifests hash Go's canonical struct-order JSON encoding, and the manifest contains no map-typed fields, so serialization is fully deterministic. This is a deliberate reason the format uses JSON rather than a schema-compiled encoding such as protobuf: protobuf serialization is not canonical across implementations or library versions, which would break content-derived IDs, and its silently-ignore-unknown-fields evolution model is the opposite of what a backup format wants — unknown data must be refused via explicit versioning, not skipped. JSON manifests are also inspectable with nothing but `cat` and `jq`, which matters when debugging a decade-old repository.

## Crash Consistency

Every repository file is published atomically: written to `staging/`, fsynced, renamed into place, parent directory synced. Pack publication additionally refuses to replace an existing file (sealed packs are immutable and located by ID, so a name collision must fail the seal, never overwrite a pack existing snapshots reference). Within one `create`, the write order is:

1. Pack files sealed (durable),
2. Index object written,
3. Manifest written **last**.

A crash at any point leaves either a complete snapshot or no snapshot — never a manifest referencing missing data. Data orphaned before the manifest write (sealed packs, an index) is unreferenced garbage: harmless, deduplicated against by later runs, and reclaimable by a future prune command.

## Locking

`create` holds an exclusive lock; `verify` holds a shared lock (concurrent verifies allowed). Locks are JSON files under `locks/` recording hostname, PID, operation, and acquisition time; holders refresh the file mtime every 30 seconds after re-verifying they still own the file, and locks older than 30 minutes are reaped as stale. Acquisition uses a plant-then-recheck handshake on both sides to close the create/verify race window, and release re-reads the file and removes it only if every field still matches the holder's own record.

## Freeze Protocol

To capture a transactionally consistent database image while the application's database-owning process (for example, a daemon) keeps running:

1. `OpenFrozenSession` calls `FreezeCoordinator.Begin`, which the application implements to pause conflicting writes against the live database — for example, an authenticated same-host call into a daemon's serial operation gate — and returns once the gate is held. The application is expected to bound this with its own watchdog so a crashed capture cannot wedge the gate forever.
2. It opens its own SQLite connection, runs `PRAGMA wal_checkpoint(TRUNCATE)` (with bounded retries) until the WAL is empty, then pins a read transaction — from this point the main database file bytes cannot change under it.
3. It immediately calls `FreezeCoordinator.End`. The gate is released and normal writes resume; the pinned transaction alone keeps the file image stable for the page scan. Database geometry, statistics, and content locators are all read inside the pinned transaction (`App.FrozenView`).

The freeze window is therefore milliseconds-to-seconds regardless of archive size. `Create` refuses to run unfrozen against a live database owner: an application whose `FreezeCoordinator.Begin` cannot resolve the owner should fail rather than risk a torn read.

## Restore

`Restore` materializes one snapshot into a target directory as a usable copy of the application's data: the database written run-by-run at `page × page_size` from the materialized page map, content files at the storage paths the restored database records for each hash (applications may namespace paths beyond the loose `<hash[:2]>/<hash>` layout; paths are re-validated as local before writing), and captured extras at their recorded relative paths and file modes (tree entry paths are re-validated as local and traversal-free before writing). It refuses a non-empty target unless `Overwrite` is set.

Restore is destructive only after the source has proven itself, in two layers. A preflight runs before the target is touched: the snapshot's map chains must materialize, every referenced blob (pages, attachments, extras) must resolve through the index, and the extras tree's paths must pass restore's locality, reserved-name, and collision rules. Failures the index cannot reveal — unreadable or corrupt pack bytes — are covered by ordering instead: the database is materialized and page-verified in a staging temp, attachments are then read (each re-deriving its SHA-256) and written to their content-addressed paths, extras are read and staged as temp siblings of their final paths, and the full restore proof (integrity_check plus the stats comparison, below) runs against the staging temp. Only after all of that do the staged extras get renamed over their live counterparts and the database published: the target's stale SQLite sidecars are set aside (renamed, so a failed publish puts them back rather than stranding the old database without its WAL), the temp is renamed over the existing database, and the asides are removed. An `--overwrite` target's live database and extras files therefore survive any content or proof failure up to that final swap; partial attachment writes are benign because the paths are content-addressed — a write to a path the live tree already uses is byte-identical, and a write to a new path is an orphan the live database never references. One caveat: that argument holds only for the canonical `<hash[:2]>/<hash>` layout. An application that namespaces attachment paths (reader version 2) can record different content at the same path across snapshots, so a failed overwrite restore may leave such a path already rewritten; no current application does this, and an application adopting namespaced paths onto live overwrite targets should derive the path from the content hash to stay in the benign case. Content-path derivation and the proof both read the staging temp, never the not-yet-replaced database.

Restore is self-proving, in layers. During materialization every blob read re-derives its SHA-256 identity (the pack reader's normal contract) and every database page is additionally checked against the snapshot's page-hash map before it is written — so a page-map bug cannot silently place correct bytes at the wrong offset. After materialization the restored database must pass `PRAGMA integrity_check` and reproduce the manifest's recorded stats (via `App.RestoredStats`) through exactly the queries capture ran inside the freeze window; the end-to-end test further proves the restored file is byte-identical to the live database as it existed at capture time, including for parent snapshots restored from an incremental chain. All files, and the directory entries naming them, are fsynced before Restore reports success. Pack reads are grouped by pack with a `Jobs` worker bound (1 = strictly serial for spinning-disk repositories); serial and parallel restores produce byte-identical trees. Restoring an old snapshot for use with a newer application version is expected to go through the application's normal schema migration at first open, the same path as any upgrade.

### Optional packed-content restore

Repository attachment membership is representation-neutral: the snapshot's
attachment lists and the content paths derived from the restored database are
the liveness authority whether bytes are restored loose or in packs. A pack
file or footer entry alone never grants application read authority. Without a
`PackedContentTarget`, restore follows the fully-loose path described above.

With a packed-content target, restore may copy compatible plain version-1
repository packs once into the target store's sharded `packs/<aa>/<id>.mvpack`
layout. The source pack remains immutable and in place. Before a copied pack
can receive catalog authority, restore validates the whole container, footer,
entry count, version, flags, and encoding settings against the target's
configured `packstore.Limits`. It verifies each snapshot-selected entry against
the footer metadata, decodes and CRC-checks the stored frame, and checks its raw
size and SHA-256 identity. The application catalog records the immutable totals
for the whole footer, but authorizes only the selected hashes that are both live
in this snapshot and eligible for packed storage. Imported pack `CreatedAt` is
the restore time, so age-based maintenance does not immediately churn freshly
restored packs.

Compatibility is deliberately per pack and per selected entry. A pack falls
back to loose restore when its container, footer, or entry count exceeds the
configured target limits, when its otherwise recognizable version or encoding
settings are unsupported, or when the target filesystem cannot atomically
publish an immutable pack. A selected entry larger than the configured
`BlobBytes` ceiling falls back independently while eligible siblings in the
same copied pack can remain packed. An unsupported application pack extension
keeps all content loose and commits an empty packed-authority replacement.
Unsupported pack settings keep the affected packs loose; when no pack is
compatible, the same replacement contains no pack records or hash mappings.
Whole-pack limit fallback verification has separate conservative hard ceilings
of 8 MiB of footer data and 100,000 entries; a larger footer fails restore
before scanning or scratch-file creation instead of doing unbounded verifier
work. These checks protect availability for pathological or damaged inputs and
do not add per-entry bookkeeping to ordinary verification.
These are compatibility outcomes, not integrity waivers: every declined hash
must still pass the ordinary encoding-aware loose read, size check, and SHA-256
verification before restore can succeed. Corruption, selection/footer metadata
mismatch, or a same-ID destination with different bytes is a hard failure, not
a fallback.

Before packed attachment restoration starts, the application supplies a live
mutation lease from the same process-local `packstore.Coordinator` used by
every maintainer that can adopt, repack, or remove target-store content. The
application must acquire its broader operation gates before that lease; it
transfers sole ownership of the lease to Restore, which validates it and holds
it across pack publication, loose fallbacks, extras, proof, database
publication, and the final durability sync. Restore releases the lease on every
success and failure path, joining a release failure with the primary restore
error. `OpenRestoreCatalog` and `ReplaceRestoredPacks` run under the existing
lease and must not reacquire or otherwise reenter that Coordinator. A
restore without a `PackedContentTarget` does not acquire a packed-store lease.

Publication and authority follow one crash-safe order:

1. Stream the source pack to a private target staging file, sync it, and close
   every handle.
2. Publish without replacement by hard-linking the staging file to its final
   sharded name, remove the staging link, and sync the containing directory.
3. Reopen and verify the final file. An existing byte-identical final file is
   safe to reuse on retry; validating that collision may require hashing the
   whole container.
4. Materialize and durably sync every loose fallback.
5. In one application-owned transaction against the unpublished staged
   database, replace all packed catalog records and selected mappings.
6. Close the SQLite staged-catalog connection and remove its exact `-wal`,
   `-shm`, and `-journal` sidecars.
7. Reopen the main staged database file read-write, verify that the opened
   handle still names the inspected regular file, sync it, re-check that the
   staged path still names that same regular file, and close the handle.
8. Run the database integrity and stats proof, then publish the database last.

The staged catalog mutation must leave the database structurally valid and
must not change the application's `RestoredStats` payload. An overwrite
target's old visible database therefore survives any failure through catalog
replacement and proof. A crash before catalog replacement can leave a valid
uncataloged pack, but it grants no read authority; maintenance may adopt,
remove, or retain it according to application liveness. Retry never depends on
that orphan surviving: it either reuses a byte-identical final pack or fails
closed on a collision, then replaces authority idempotently. A crash after the
catalog transaction but before database publication changes only the staged
database, not the visible one.

The first path component `packs` is reserved inside the content directory only
when packed restore is enabled, preventing restored loose paths from colliding
with the production pack subtree. In a fully-loose restore it remains an
ordinary application path.

This path primarily removes per-blob file creation and associated filesystem
or antivirus scanning for the packed subset. It does not eliminate read I/O:
restore still streams each copied container and hashes every selected blob,
and an existing-final retry can hash the entire container. A mixed restore also
pays a complete content-directory durability traversal after loose fallback
materialization and before packed authority is committed.

On Windows, durability follows Kit's established policy: regular files are
flushed, handles are closed before publication and reopening, and directory
sync is a documented no-op. Atomic no-clobber publication uses a hard link on
both Windows and Unix. If a filesystem cannot provide that operation, a new
pack falls back loose instead of using a replacement rename with a race window.
`RestoreResult` exposes packed and loose blob counts, imported pack count, and
structured fallback reasons so callers can report both the achieved layout and
why content stayed loose.

## Verification Model

`verify` enumerates every blob a manifest can reach — page-map chains and their blob tables, hash-map chains, attachment lists and every listed content hash, the extras tree and its entries — and checks each against the index and packs. Quick mode proves structure (references resolve, objects decode, packs exist); full mode additionally reads every referenced blob and re-derives its SHA-256 identity, compares each attachment list and extras tree entry's recorded size against the blob's actual content length (restore refuses a mismatch, so verify must flag it), confirms materialized page/hash maps match the manifest's recorded geometry with full coverage, and checks every page-map run against its blob's actual bytes — the run must fit inside the blob and each mapped page must hash to the page-hash map's entry, exactly as restore's materialization requires. Extras tree paths are held to restore's rules in both modes: escaping or reserved-overlapping paths and case-folded path collisions are Problems, not restore-time surprises. Capture enforces the same rules, so a snapshot with such paths is never written in the first place; verify's check exists for trees written by other tools or tampered after the fact. Problems are collected, not fail-fast, and each names the snapshot, blob, and pack involved.

## Current Limitations

- Repository encryption and retention (forgetting and pruning snapshots) are not yet implemented; the format reserves flags and fields for them (`encryption` in the repo config, the `encrypted` blob flag, crypter parameters threaded through the code as nil).
- The application database must be SQLite; other database engines are not supported.
- The application's write gate is held only through the freeze protocol (checkpoint plus read-transaction pin), not through content capture. An operation that deletes content files while a backup is still capturing can therefore delete a file the frozen database still references; the backup then fails loudly with a read or hash error and can be retried after the deletion completes. This is a deliberate trade: holding the gate — and with it every write — for the full capture window would be far more disruptive than a rare retryable backup failure. A snapshot that completed is unaffected: it captured every file it references.

## Roadmap (settled design, not yet implemented)

The following behaviors were designed alongside the shipped format — the format hooks for them already exist — and are recorded here as the binding intent for the follow-up phases.

A restore-check verification mode performing the full restore materialization proof against scratch space, without writing a target, remains planned.

**Encryption.** Initializing a repository with encryption enabled generates a random 256-bit repository key; every blob, footer, index, and manifest is encrypted with XChaCha20-Poly1305, with the AAD binding each ciphertext to its identity (blob ID, or object role plus ID). The repository key is wrapped with [age](https://age-encryption.org) to one or more recipients (scrypt passphrase and/or X25519 identities) in `keys/master.age`; adding, removing, or rotating recipients rewraps the key without rewriting objects. `config.toml` stays plaintext by necessity; tampering yields detectable failures, not silent corruption. Key loss is unrecoverable by design. Blob IDs remain plaintext-content hashes but appear only inside encrypted metadata.

**Retention.** Forgetting a snapshot deletes its manifest file (refusing to drop the last snapshot without an explicit force); pruning takes the exclusive lock, walks the remaining manifests to collect the live blob set, deletes fully-dead packs, repacks packs below 50% live content, and writes merged indexes — with new packs and indexes durable before anything is deleted, so a crash mid-prune never breaks reference closure. Until these ship, content purged from the application's live store persists in historical snapshots.

**Performance follow-ups.** Two accepted deferrals from review: detecting a same-page-size `VACUUM` by delta-ratio anomaly (warn that a keyframe would be cheaper), and a streaming page-map merge for memory-constrained hosts. Further out: an export mode (one self-contained archive file), WAL shipping for point-in-time recovery, native remote backends, and application-scheduled backups.
