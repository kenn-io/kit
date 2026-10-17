# Daily screen reports

Configure a reporter with the application's screen names and a private state
path whose parent directory already exists:

```go
posthog.WithAllowedEvent("screen_viewed",
    posthog.RequireProperty("screen", posthog.AllowStringValues("queue", "review"))),
posthog.WithDailyEvent("screen_viewed", "screen", posthog.NewDailyClaims(statePath)),
```

`Options.DistinctID` scopes claims to an installation. HTTP and `Report` reject
invalid properties before claiming. The handler requires `application/json`,
one JSON value and a body of at most 64 KiB. Applications own routes and
authentication.

Keep daily event and key allowlists finite. Reporter construction rejects an
unknown daily event, an unlisted key property or missing state path, including
when opted out. `Report` and enabled `Capture` return `ErrUnsupportedEvent`
for blank events.
Daily keys must be caller-owned properties; reporter defaults cannot identify a daily claim.

The state file reserves each installation, event and screen before SDK enqueue.
The in-process semaphore and file lock cover reservation, enqueue and settlement.
A rejected enqueue releases only its pending record. A failed reservation save
releases any published reservation under the same locks. If that release fails
before publication, the error includes both failures. If release publishes
despite an error, only the reservation error returns.
After queue acceptance, the reporter marks the reservation accepted and saves
again. If that save fails, `Report` returns `queued` and logs a warning through
the configured logger. The same `DailyClaims` instance retries
settlement as accepted on its next call. The persisted pending record keeps
other instances from enqueueing the same date during recovery.
Each key retains its 35 most recently accepted UTC dates in acceptance order.
Returning to a retained date skips it, even after a wrong-clock report. Sparse
activity can retain dates older than 35 calendar days. A previously accepted
date outside this set may count again after a clock correction.
Unresolved reservations keep their date in the normal list and add a separate
four-part JSON key containing the installation, event, screen, and an instance
owner token. Its value is a one-date list. Settlement removes that owner's
marker, so an old owner cannot release a replacement claim for a retired date.
Accepted settlement keeps
the reservation's original order, including when another writer accepts later
dates before cleanup retries.
Reports compact all keys and remove empty keys under the claim lock. A skipped
report saves only when compaction changed the state. If that housekeeping save
fails, the reporter logs a warning, returns `skipped`, and retries compaction
on a later report. Other storage failures return errors.
Reservation, event timestamp and installation age use one time sampled under
the claim lock.

Use `Report` with a caller context. Reports on the same `DailyClaims` instance
wait on an in-process semaphore before the file lock. Both waits honor the
caller's context; `Capture` bounds them with `ShutdownTimeout`. The file keeps
version 1 and date-string lists. Released v1 readers preserve unresolved
metadata while reading and rewriting the file. Upgrade all writers sharing a
path together: a released writer cannot mark its own reservation unresolved
or distinguish it from a replacement during stale cleanup.
Numeric versions other than the exact literal `1` that may be at least 1 are
refused unchanged. Other unreadable content resets to empty claims. Read
errors other than a missing file are returned.
Request lock waits follow caller cancellation and deadlines; HTTP owners should bound request contexts as needed.
A report racing `Close` can hold the in-process semaphore and daily file lock
while waiting for SDK shutdown, up to `ShutdownTimeout`. Other reports on the
same instance wait on the semaphore with the same context bound, so they and
other writers can time out and retry.
Recovering damaged state forgets accepted dates and can count them again.

Accepted reports mean SDK queue acceptance. A crash between reservation and
acceptance can lose a count. Process exit before settlement can leave a pending
record until its owner retries or 35 later known acceptances retire it. New
dates can report while old reservations remain unresolved. Repeated failures
can grow this uncertain history until those later acceptances drain it; the
35-date bound applies to known accepted dates. Replacing the file forgets all
deduplication history and can count dates again.
The SDK can also fail network delivery after acceptance.

Report screens on later focus, navigation or terminal interaction,
independently of app-open reporting. Use delivery results to detect failures
and retry on later interaction. Report the initial terminal queue and use its
acceptance result. A split pane showing queue
and review reports both screen keys. Daily deduplication belongs to the server.
`queued`, `skipped` and `disabled` complete the current attempt; retry errors on later interaction.

When adopting this API, remove copied daily gates, use `Report` or `PostEvent`
results for retries, and transfer existing same-day claims if upgrade continuity
is required. Product adoption belongs in each application's own change.

Require a session duration through the existing caller-owned filter:

```go
posthog.WithAllowedEvent("session_ended",
    posthog.RequireProperty("duration_bucket", posthog.AllowStringValues(
        "under_1m", "1_to_5m", "5_to_30m", "over_30m", "30m_to_2h", "over_2h"))),
```

Older tabs can still send `over_30m`, meaning more than thirty minutes with no
further detail. Preserve that meaning alongside current buckets. Missing or
unsupported required durations return an error before enqueue.
