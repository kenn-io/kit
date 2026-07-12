# Format-v1 streaming pack I/O contract

Status: **approved** for issue `6j6h` on 2026-07-11. Implementation issues must
preserve this contract; changes to its public API or authority-safety choices
require an explicit contract amendment before dependent code changes.

## Scope and fixed compatibility boundary

This design adds bounded-memory streaming for plain format-v1 entries across
`pack`, `packstore`, and `backup`. It does not change the pack header, frame
bytes, footer entry fields, footer checksum, trailer, index, manifest, or
publication rules described in [backup/FORMAT.md](../backup/FORMAT.md).

The existing buffered APIs remain source-compatible and retain their current
contracts:

- `pack.Writer.Append` and `AppendEncoded` accept complete slices.
- `pack.Reader.ReadBlob` returns a complete verified slice.
- `packstore.Store.Open` returns seekable content and therefore continues to
  buffer packed content before returning it.
- `packstore.Store.ReadBounded` and `backup.Repo.ReadBlob` return complete
  verified slices.
- encrypted v1 frames continue to use complete-frame AEAD. Streaming APIs
  reject them with the typed unsupported result below; they never decrypt a
  prefix or rewrite it without encryption.

Format v2, chunked authenticated encryption, objects whose raw length exceeds
`pack.MaxRawLen` (4 GiB), and changing application defaults are out of scope.
The 4 GiB value is a representation ceiling, not a recommended policy limit.

## Terminology and invariants

Consumer streaming may deliver a prefix before the stored CRC32C, decoded raw
length, and SHA-256 identity are known to be valid. Only a terminal `io.EOF`
from `Read`, a nil result from `Verify`, or `Verified() == true` proves complete
verification. A nil result from opening a stream proves only that footer and
policy preflight succeeded.

Authority-safe copying consumes and verifies the complete source into a
caller-owned private staging file. The caller must then sync and close that
file, publish it without replacement, sync its parent directory where the
platform supports that operation, and only then change catalog or restored
database authority. A stream prefix is never authoritative.

All byte counters use `uint64` at the format boundary and checked `int64` at
filesystem/application boundaries. No untrusted format value is converted to
`int`, passed to `make`, or added to an offset before a checked bound.

## Public API contract

Names may change during review, but implementations must preserve these
ownership and terminal-state semantics.

### `pack`

```go
var (
	ErrStreamUnsupported      = errors.New("pack: streaming unsupported")
	ErrStreamLimit            = errors.New("pack: streaming limit exceeded")
	ErrVerificationIncomplete = errors.New("pack: verification incomplete")
	ErrStreamsActive          = errors.New("pack: blob streams active")
)

type StreamFeature string

const StreamEncryptedV1 StreamFeature = "encrypted_v1"

type UnsupportedStreamError struct {
	Feature StreamFeature
}

func (*UnsupportedStreamError) Error() string
func (*UnsupportedStreamError) Unwrap() error // ErrStreamUnsupported

type StreamLimitError struct {
	Dimension string
	Actual    uint64
	Limit     uint64
}

func (*StreamLimitError) Error() string
func (*StreamLimitError) Unwrap() error // ErrStreamLimit

type ReaderLimits struct {
	ContainerBytes uint64
	FooterBytes    uint64
	Entries        uint64
	RawBytes       uint64
	StoredBytes    uint64
}

type ReaderOptions struct {
	Limits ReaderLimits
}

func OpenReaderWithOptions(
	path string, crypter *Crypter, opts ReaderOptions,
) (*Reader, error)
func NewReaderFromFileWithOptions(
	f *os.File, id string, crypter *Crypter, opts ReaderOptions,
) (*Reader, error)

type AppendStreamOptions struct {
	ExpectedID   *BlobID
	ScratchDir   string
	ScratchBytes uint64
}

type PreparedBlob struct { /* unexported files and immutable metadata */ }

func PrepareBlob(
	ctx context.Context,
	src io.Reader,
	rawLen uint64,
	zstdLevel int,
	opts AppendStreamOptions,
) (*PreparedBlob, error)
func (p *PreparedBlob) ID() BlobID
func (p *PreparedBlob) RawLen() uint64
func (p *PreparedBlob) StoredLen() uint64
func (p *PreparedBlob) ScratchBytes() uint64
func (p *PreparedBlob) Close() error

func (w *Writer) AppendStream(
	ctx context.Context,
	src io.Reader,
	rawLen uint64,
	opts AppendStreamOptions,
) (Entry, error)
func (w *Writer) AppendPrepared(
	ctx context.Context, prepared *PreparedBlob,
) (Entry, error)

type BlobReader struct { /* unexported state */ }

func (r *Reader) OpenBlob(ctx context.Context, e Entry) (*BlobReader, error)
func (r *BlobReader) Read(p []byte) (int, error)
func (r *BlobReader) Verify() error
func (r *BlobReader) Verified() bool
func (r *BlobReader) Close() error
```

`PrepareBlob` owns neither `src` nor a caller-supplied scratch directory. It
reads exactly `rawLen` bytes, probes for one trailing byte, computes SHA-256
incrementally, and enforces `ExpectedID` when non-nil. On success it transfers
ownership of every exact scratch file to `PreparedBlob`; `Close` removes those
files exactly and is idempotent. The prepared value exposes metadata but not
paths or mutable files. On failure `PrepareBlob` cleans its own artifacts.

`AppendPrepared` consumes ownership of the prepared value on every return. It
accepts only a complete plain prepared frame, copies it into the pack, removes
its scratch, and returns an entry matching its immutable metadata. Callers use
`PreparedBlob.Close` only to discard a value they never append. This permits
concurrent preparation with single, ordered writer access. `AppendStream` is
the convenient composition of `PrepareBlob` and `AppendPrepared`; it never
closes `src`. Cancellation is checked while reading, encoding, copying scratch
into the pack, and cleaning up.

The writer staging directory is the default scratch directory. A non-empty
`ScratchDir` allows callers to place temporary amplification on a different
volume. `ScratchBytes == 0` means no application-specific scratch ceiling
beyond checked format and filesystem limits; higher layers must always pass a
finite policy value for automatic work. Exceeding it returns a typed limit
error before pack bytes are appended.

`OpenBlob` validates entry bounds before returning. Plain raw entries stream
from an `io.SectionReader`; plain compressed entries use a streaming zstd
decoder whose window and decoded total are bounded by the entry and policy.
The reader incrementally verifies stored CRC32C, exact stored length, exact raw
length, and SHA-256. It returns `io.EOF` only after all four checks pass.

`Verify` drains to the terminal state without exposing more bytes and returns
nil only after verified EOF. `Close` releases resources. If called before a
verified terminal state it returns `ErrVerificationIncomplete`, joined with any
close error; it does not drain implicitly. `Verified` is monotonic. After a
terminal integrity, cancellation, or decode error, every later `Read` and
`Verify` returns that same error. A read may return both a final prefix and a
non-nil terminal error, following `io.Reader` rules.

An entry with `BlobEncrypted`, or any entry inside an encrypted pack, makes
`OpenBlob` return `UnsupportedStreamError{StreamEncryptedV1}`. Existing
`ReadBlob` remains the authenticated bounded-buffer path.

Zero reader limits select the format maxima. `OpenReader` and
`NewReaderFromFile` delegate to the option-bearing constructors with zero
limits. The constructors enforce container, footer, entry-count, stored, and
raw limits before allocation or exposure, and retain ownership of the supplied
descriptor on every return. This lets `packstore` use its stable no-follow
descriptor without maintaining a second frame/footer implementation.

`Reader.Close` must not race a live `BlobReader`. The low-level contract is
that closing the parent before all children is misuse and returns
`ErrStreamsActive` without closing the descriptor. Higher-level
caches provide retirement and leasing rather than exposing this condition.

### Trial compression and writer poisoning

Streaming trial compression preserves zstd level, single-segment downgrade
compatibility, the minimum zstd window rule, and the current threshold of at
least `ceil(rawLen*3/100)` bytes saved (minimum one byte). It proceeds as
follows:

1. Preflight `rawLen`, the worst-case stored bound, and scratch budget.
2. Create exact-owned random raw and compressed scratch files with mode 0600
   using create-exclusive semantics. Names contain the writer pack ID and end
   in `.raw.staging` and `.zstd.staging`.
3. Read the source once while hashing and writing raw scratch; feed the same
   bytes to a streaming zstd encoder, initialized with the declared content
   size, when `rawLen >= zstd.MinWindowSize`.
4. Sync is not required for disposable scratch. Close both files and choose
   compressed only when the exact stored length meets the 3% rule.
5. Rewind the chosen scratch, copy exactly its length into the pack while
   computing CRC32C, require it to equal the CRC recorded during preparation,
   and append the footer entry only after the copy succeeds. Preparation keeps
   the exact descriptors and checks their identity and length before copying.
6. Close and remove both exact scratch paths. Cleanup errors are joined to the
   primary error.

Source, compression, scratch write/read/close, size/hash, cancellation, and
scratch-cap failures occur before pack mutation and do not poison the writer.
Once copying to the pack begins, any short write or write error poisons the
writer because its descriptor offset may no longer equal `w.off`. Footer write
failure retains the existing poison rule. Sync, close, publication, and parent
directory sync retain the existing `Seal` state distinctions. A scratch
cleanup error after a successful pack copy also poisons the writer so callers
cannot retry an operation whose entry was already appended; `Abort` remains
the only valid next operation.

Streaming is allowed to produce a different valid zstd byte sequence from the
buffered encoder. It must remain readable by the frozen v0.7.0 reader and must
preserve every format-v1 field meaning.

### `packstore`

```go
type VerifiedReadCloser interface {
	io.ReadCloser
	Verify() error
	Verified() bool
}

func (s *Store) OpenStream(
	ctx context.Context, hash Hash,
) (VerifiedReadCloser, int64, error)

func (s *Store) CopyVerified(
	ctx context.Context, hash Hash, dst io.Writer,
) (int64, error)
```

The concrete return type may become a small package-local wrapper so loose and
packed sources share the same methods. Callers must see `Read`, `Verify`,
`Verified`, and `Close` with the `pack.BlobReader` terminal contract.

`OpenStream` applies catalog membership before physical access and retains the
single re-resolution on `fs.ErrNotExist` already implemented by
`resolveBlob`. Loose streams use stable no-follow descriptors and incrementally
verify stat length and canonical SHA-256. Packed streams compare catalog index
metadata with the authoritative footer before returning.

Each cached pack slot has a retired flag and a lease count. Opening a packed
stream acquires a lease under the cache mutex and releases the mutex before any
content I/O. Eviction and `Store.Close` remove the slot from future lookup;
they close its descriptor immediately only at lease count zero. The last
stream close closes a retired descriptor. No decode holds the global cache
mutex.

`RetirePack` first retires the cache slot, then attempts exact canonical-file
removal. Unix may unlink while leased readers finish on the old descriptor.
Windows removal may return a sharing violation while a lease is live; that is
a typed retryable physical-cleanup error and never rolls catalog authority
back. Unsupported platforms retain the existing fail-closed no-follow policy.

`CopyVerified` writes only to `dst`, does not close or sync it, and succeeds
only after verified EOF. A destination write error cancels/drains no source;
the source closes incomplete and the destination remains private caller-owned
staging. The returned count is verified raw bytes. Publication is deliberately
not part of this app-neutral API.

Existing `Limits` keep their meanings. The implementation adds explicit
scratch and per-run limits rather than overloading `BlobBytes`:

```go
type WorkLimits struct {
	Limits
	ScratchBytes uint64
	RunRawBytes  uint64
}
```

The final location of these fields may be operation options to preserve source
compatibility. Limits distinguish raw object, stored entry, pack container,
footer, entry count, scratch bytes, and committed run bytes. Policy errors use
typed dimensions; corrupt metadata never masquerades as a policy deferral.

### `backup`

`backup.Repo` adds a streaming read that resolves the existing index, opens
the named pack, finds and cross-checks the footer entry, and transfers pack
reader ownership to the returned stream. `Repo.ReadBlob` remains buffered.

`PackAppender` adds an ordered `AddStream` equivalent to `AppendStream` and
preserves first-seen deduplication. Plain capture workers produce bounded
prepared scratch results, not `[]byte` frames. The in-order collector owns each
result after receipt, appends it, and removes its exact scratch files. Admission
uses both worker count and total admitted scratch bytes. Cancellation wakes
workers and the collector and cleans results that completed out of order.

Encrypted backup continues through the existing buffered `Add`/`AddEncoded`
path under the configured object limit. Asking the new streaming path to write
or read encrypted v1 returns `pack.UnsupportedStreamError`; it never selects
plain output as a fallback.

## Ordered state and crash contracts

### Append

| State | Durable/authoritative effect | Recovery |
| --- | --- | --- |
| scratch created or filled | none | remove exact owned scratch names |
| chosen scratch complete | none | remove scratch; writer remains usable |
| frame copy started | pack staging is non-authoritative | any failure poisons writer; abort exact pack staging |
| entry recorded in memory | pack staging is non-authoritative | later append or seal; abort discards all |
| footer written, file synced and closed | complete private pack | seal continues publication |
| no-clobber publication succeeded | immutable pack exists, not catalog authority | directory-sync error reports published state |
| catalog/manifest later commits | pack becomes authoritative | higher-layer transaction owns recovery |

### Consumer read and verified copy

| State | Consumer stream | Authority-safe copy |
| --- | --- | --- |
| preflight/open | no content verified | private staging only |
| prefix delivered/written | terminal error still possible | staging must not publish |
| verified EOF | caller may trust full content | sync and close staging |
| no-clobber publish + directory sync | not applicable | bytes durable, still uncataloged |
| catalog transaction | not applicable | authority may reference destination |

### Repack

| Order | State/effect | Failure or crash result |
| --- | --- | --- |
| 1 | resolve current authority and lease exact source descriptor | no mutation |
| 2 | prepare and verify source with expected ID/length | private scratch only; clean it |
| 3 | append prepared frame to replacement staging | abort poisoned replacement on failure |
| 4 | record proposed move in memory | no catalog authority change |
| 5 | seal, sync, publish, and reopen-validate replacement packs | durable uncataloged packs may remain |
| 6 | exact-set compare-and-swap all moves for the source set | authority changes atomically or not at all |
| 7 | retire old cache readers and exact files | retryable physical orphan may remain, never stale authority |
| 8 | count committed progress | uncommitted work is excluded |

A source failure aborts only replacement packs derived from that source group;
other independently selected source packs follow the operation's existing
explicit/automatic isolation policy.

### Unpack

| Order | State/effect | Failure or crash result |
| --- | --- | --- |
| 1 | preflight complete requested live set and limits | no mutation |
| 2 | `CopyVerified` one entry to exact private loose staging | incomplete staging is removable and non-authoritative |
| 3 | sync and close staging | complete private bytes, still non-authoritative |
| 4 | publish without replacement and sync parent | durable loose orphan may remain |
| 5 | repeat until every required destination is durable | catalog still names packs |
| 6 | atomically clear exact packed mappings | loose destinations become authoritative |
| 7 | retire empty packs | retryable physical orphan may remain |

Retries accept an existing destination only after its full identity verifies.

### Restore

| Order | State/effect | Failure or crash result |
| --- | --- | --- |
| 1 | copy/import packs into private staging and verify every selected entry | private staging only |
| 2 | sync/close and no-clobber publish packs | durable uncataloged packs may remain |
| 3 | materialize loose fallbacks through verified staging and durable publication | durable loose orphans may remain |
| 4 | replace catalog records in unpublished staged database | visible database unchanged |
| 5 | close SQLite and remove exact sidecars | staged database remains private |
| 6 | verify identity, sync, and run integrity/stats proof | proof failure leaves visible database unchanged |
| 7 | publish database last and sync target directory | restored authority becomes visible only here |

This preserves `backup/FORMAT.md`. A prefix, complete but unsynced file, or
uncataloged pack cannot make the visible database authoritative.

## Existing buffering call-site disposition

Line anchors refer to the baseline at commit `a4913c4`.

| Current site | Disposition |
| --- | --- |
| `pack/writer.go:89`, `pack/writer.go:112` | retain buffered compatibility; add `AppendStream` |
| `pack/reader.go:149`, `pack/reader.go:171` | retain buffered reads; build them over or alongside `OpenBlob` without changing all-or-error return |
| `packstore/store.go:79`, `packstore/store.go:263` | retain `ReadBounded`/seekable `Open`; add streaming resolution and cache leases |
| `packstore/preflight.go:102` | retain buffered compatibility; add bounded streaming entry open |
| `packstore/pack.go:306-373` | replace candidate slice/frame path with stable-descriptor streaming append |
| `packstore/repack.go:278-306` | replace `ReadBounded` + `EncodeFrame` + `AppendEncoded` with leased stream append |
| `packstore/unpack.go:104` | replace whole blob with `CopyVerified` to private loose staging |
| `packstore/import.go:106` | replace selected-entry whole read with streaming verify; retain bounded footer/index data |
| `backup/attachments.go:435-478` | replace worker content/frame slices with byte-admitted scratch results |
| `backup/attachments.go:482-545` | replace source/file `ReadAll` with stable stream into prepared append |
| `backup/attachments.go:591` | retain buffered attachment-list metadata decode; list blobs remain policy-bounded metadata |
| `backup/create.go:499-527` | retain buffered page blobs: their size is independently bounded by page-plan policy; account them in worker admission |
| `backup/packer.go:46-85` | retain `Add`/`AddEncoded`; add streaming appender path |
| `backup/packer.go:186-205` | retain `Repo.ReadBlob`; add streaming repository read |
| `backup/verify.go:376`, `backup/verify.go:574` | consume streams fully and require verified EOF |
| `backup/restore.go:562`, `backup/restore.go:837`, `backup/restore.go:1055` | retain small structural blob reads where bounded; stream canonical content and loose fallback publication |
| `backup/lock.go:373,517,600`, `backup/cache.go:20`, `backup/index.go:170`, `backup/manifest.go:131` | deliberately retain bounded control-plane JSON/index/cache reads |

Implementation commits must refresh anchors changed by earlier child work and
add any newly discovered complete-content allocation to this table.

## Error classification and observability

Errors remain inspectable with `errors.Is`/`errors.As` and fall into: caller or
context cancellation; typed policy limit (with dimension, actual, limit);
typed unsupported encrypted streaming; source I/O; scratch I/O; corruption
(footer, CRC, zstd, length, hash); destination I/O/durability; publication
collision; catalog/authority transaction; and retryable physical retirement.
No corruption is converted to a policy skip, and no durability failure is
reported as successful publication.

Progress counts raw bytes as they are read for informational progress, scratch
and stored bytes as separate metrics, and committed raw/stored bytes only after
the operation's authority commit. Required metrics are raw bytes, stored bytes,
scratch bytes created and peak live, bytes copied, entries started/verified/
committed, compression decision, verification failures by class, active pack
leases, retirement retries, and elapsed encode/decode/copy time.

## Measurable approval and implementation gates

- Frozen v0.7.0 readers read new raw and compressed plain packs; new readers
  read frozen v0.7.0 fixtures. Buffered APIs remain source- and behavior-
  compatible.
- Real raw, compressed, empty, tiny, boundary, incompressible, and highly
  compressible entries cover truncation, trailing source bytes, CRC, hash,
  length, zstd, cancellation, disk-full, short-write, sync, close, collision,
  and cleanup faults.
- Allocation tests process content above 64 MiB with peak live heap bounded by
  documented codec buffers plus configured concurrency, not object length.
- Scratch tests prove the configured cap, exact cleanup, worst-case two input-
  sized scratch files per admitted encoder, and retry after interruption.
- Cache race tests cover shared streams, early close, ignored terminal error,
  eviction, store close, retirement, and one-time authority re-resolution.
- Native Windows tests cover open-handle sharing violations and retry. Unix
  tests cover unlink with a pinned descriptor. Unsupported targets compile and
  fail closed.
- End-to-end pack, mixed read, repack, unpack, backup, verify, packed restore,
  loose restore, and final verify use real files/codecs rather than stubs.
- Benchmarks record peak heap/RSS, throughput, allocations, scratch bytes, and
  copy amplification for small objects and large raw/compressible objects,
  including a 1 GiB-class manual run when host resources permit.

Approval of this design does not approve raising `DefaultLimits().BlobBytes`.
That remains an explicit application policy decision after the verification
gate.
