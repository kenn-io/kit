# spill Instructions

## Scope

`spill/` holds an append-only list of opaque strings in memory up to a byte
limit and in a private temporary file past it. Keep it a container: no
record formats, sorting, or caller policy.

## Invariants

- A list at or under its limit never touches the disk. The limit counts
  payload bytes (the sum of record lengths) only.
- Create the file only with `safefileio.CreatePrivateTemp` in the caller's
  directory, and remove it on `Close` and when the move to disk fails.
- A failed move to disk leaves the list unchanged. A write failure after the
  move is sticky: every later `Append` and `All` returns it, because the file
  no longer matches the records appended.
- Frame records as a uvarint length followed by the bytes so no byte value is
  reserved.
- Reading checks the file against the record count and payload size written.
  A short, truncated, or inconsistent file is an error, never a shorter list,
  and a length is checked against the payload left before allocating.
- Read with `ReadAt` through a section bounded to the bytes written, so an
  iteration sees the records present when it started and appends during or
  after a read keep writing at the end of the file.
- `Close` is idempotent and reports flush, close, and remove errors joined.
  A `List` is not safe for concurrent use.
