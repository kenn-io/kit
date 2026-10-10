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
unknown daily event, an unlisted key property, missing state path or blank
`Options.DistinctID`, including when opted out. `Report` and enabled `Capture`
return `ErrUnsupportedEvent` for blank events. Daily keys must be caller-owned
properties; reporter defaults cannot identify a daily claim.

The state file reserves each installation, event and screen before SDK enqueue.
The file lock covers reservation, enqueue and rollback. A rejected enqueue
releases its claim. A failed reservation save releases any published claim
under the same lock. Storage failures return errors for a later retry; if
release also fails, the error includes both failures.
Claims retain accepted UTC dates, so correcting a future clock permits an
unreported date and returning to a counted date skips it. Dates remain in the
file because pruning could repeat a count after clock correction. The file
grows by one date per accepted installation, event, key and day. Reservation,
event timestamp and installation age use one time sampled under the claim lock.

Use `Report` with a caller context. `Capture` bounds daily lock waiting with
`ShutdownTimeout`. A malformed state file starts with empty claims; the next
save replaces it. A newer format version returns an error and leaves the file intact.
Request lock waits follow caller cancellation and deadlines; HTTP owners should bound request contexts as needed.
A report racing `Close` can hold the daily file lock while waiting for SDK shutdown, up to `ShutdownTimeout`, so other writers can time out and retry.
Recovering damaged state forgets accepted dates and can count them again.

Accepted reports mean SDK queue acceptance. A crash between reservation and
acceptance can lose a count. If release fails before publication, the same
`DailyClaims` object retries cleanup on its next call; process exit before that
cleanup can leave a reservation. The SDK can also fail network delivery after acceptance.

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
