# Streaming pack I/O

Kit reads and writes format-v1 packs without requiring memory proportional to
the largest object. This document describes the contracts and ownership rules
that keep the implementation bounded, verifiable, and safe during concurrent
maintenance.

The on-disk representation remains the format described by the
[backup repository and pack format](../../backup/FORMAT.md). Streaming changes
how bytes move through the implementation, not how existing packs are encoded.

## Goals and boundaries

Streaming pack I/O is designed to:

- bound heap use by configured buffers, decoder windows, and concurrency rather
  than object size;
- preserve format-v1 compatibility and the existing buffered APIs;
- verify stored and decoded bytes before treating output as authoritative;
- keep resource-policy limits explicit and independently configurable; and
- behave predictably while pack files are cached, retired, or deleted.

The design does not introduce a new pack format, support objects larger than
the format-v1 length fields allow, or make format-v1 encrypted entries
streamable. Those require separate format work.

## Read contracts

Buffered and streaming reads intentionally make different guarantees.

| Contract | Use | Verification guarantee |
| --- | --- | --- |
| Buffered read | Callers that need a complete byte slice or seekable result | Returns content only after the complete entry is verified |
| Streaming read | Callers that can consume incrementally | May deliver a prefix; successful terminal EOF or an explicit verification result proves the complete entry |

A streaming caller must not treat bytes as authoritative merely because they
were read successfully. It must consume through the terminal result. Closing a
stream before that point reports incomplete verification rather than implying
success.

Both paths verify the same properties:

- the stored length and stored-byte checksum;
- the decoded length;
- the object digest; and
- the absence of trailing or truncated encoded data.

Failures are sticky: after a stream reaches a verification or decoding error,
later reads return that failure rather than presenting an EOF-shaped success.

## Data flow

Consumer streaming follows a direct verification pipeline:

```text
catalog lookup
    -> leased pack descriptor
    -> bounded entry reader
    -> optional bounded decompressor
    -> length and digest verification
    -> terminal success
```

Operations that create authoritative output add a private staging boundary:

```text
verified source
    -> private staging file
    -> flush, sync, and close
    -> no-clobber publication
    -> directory sync
    -> catalog or manifest update
```

This separation lets ordinary consumers receive data incrementally while
ensuring restoration, unpacking, and similar operations never publish a
partial or unverified prefix.

## Preparing and appending entries

Entry preparation is independent from pack mutation. The preparation phase
reads the source once into private scratch storage while computing its raw
length and object identity. It may also create a compressed candidate. The
candidate is selected only when it clears the compression-savings threshold;
otherwise the raw representation is retained.

A prepared entry owns its scratch files until it is appended or closed. Scratch
names are unique to the preparation, and cleanup targets only those exact
paths. This makes concurrent preparation safe and prevents broad cleanup from
removing another operation's state.

Appending copies the chosen representation into the pack in format order, then
writes the corresponding metadata. Once entry bytes begin entering the pack, a
copy or verification failure poisons that writer: the partially written pack
cannot safely accept later entries. Failures before pack mutation leave the
writer reusable.

Preparation can temporarily require both raw and compressed scratch space.
That disk amplification is deliberate; it trades bounded heap use for an exact
choice between compatible format-v1 representations.

## Descriptor leases and retirement

A cached pack descriptor is not owned solely by the cache. Each active stream
holds a lease, and eviction or cache closure retires the descriptor rather than
closing it immediately. The underlying file closes after the final lease is
released.

Pack resolution follows catalog authority:

1. Resolve the object to a currently cataloged pack.
2. Acquire a descriptor lease.
3. If the file changed during acquisition, re-resolve once against the catalog.
4. Stream from the stable leased descriptor.

Maintenance removes catalog membership before retiring the corresponding
descriptor. On systems that permit unlinking open files, physical deletion can
proceed while existing leases drain. On systems that do not, deletion returns
a typed retryable result until the final lease closes. Unsupported or ambiguous
states fail closed; they are not converted into success.

The descriptor limit therefore bounds idle cached descriptors, while active
streams may temporarily hold additional descriptors. Callers must account for
both cache capacity and streaming concurrency when setting process limits.

## Authority and crash ordering

Any operation that makes data visible outside the stream follows one rule:
authority advances only after content is verified and the new representation
is durable.

For a new pack, this means entry data and metadata are completed, the pack is
synced and closed, the final name is published without overwriting an existing
file, and the containing directory is synced before catalog state points to it.

For an unpacked or restored object, bytes are copied into a private file and
verified there. The file is synced and closed before no-clobber publication,
then the destination directory is synced. A failed copy, verification, sync, or
publication leaves existing authoritative data untouched.

Cleanup is exact and idempotent. Recovery may remove a known private staging
path, but it must not infer ownership of neighboring files or partially advance
catalog state.

## Compatibility and limits

Format-v1 entry bytes, footer layout, and trailer semantics do not change.
Existing buffered entry points remain suitable for callers that require an
all-or-nothing in-memory result. Streaming entry points add bounded transport
without weakening the buffered contract.

Resource limits describe distinct risks and should remain distinct:

- object length limits bound decoded content;
- stored-entry and container limits bound encoded input;
- footer and entry-count limits bound metadata allocation;
- decoder-window limits bound decompression memory;
- scratch-space limits bound preparation and staging disk use; and
- concurrency limits bound aggregate buffers and open descriptors.

Compressed entries use a bounded decoder window. Legacy format-v1 entries may
declare a window related to their raw size; when that exceeds policy, the read
fails with a typed policy error instead of silently increasing memory use.

Format-v1 encrypted entries remain buffered because their whole-entry
authenticated-encryption construction cannot authenticate a streamed prefix.
True encrypted streaming requires a chunk-authenticated representation in a
future format.

The format-v1 length fields also impose their existing object-size ceiling.
Streaming removes heap proportionality within that ceiling; it does not extend
the wire format beyond it.
