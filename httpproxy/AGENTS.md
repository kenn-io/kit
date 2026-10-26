# httpproxy invariants

- The proxy listens only on loopback and reaches only `Config.Allow`
  addresses. Check credentials before the allowlist, and refuse before any
  dial, so an unauthenticated caller learns nothing about allowed upstreams.
- Every request carries a Client's Basic proxy credentials. Compare passwords
  in constant time. Never forward `Proxy-Authorization` upstream.
- TLS goes through `CONNECT` tunnels; the proxy never terminates TLS or sees
  upstream credentials. Plain HTTP uses absolute-form requests.
- A Client starts requests and tunnels only while bound. Track every inbound
  and upstream connection under the binding that admitted it, and refuse to
  track once that binding has ended, so a cutoff cannot miss a connection
  opened during the cutoff.
- When a binding's context ends, close the Client's connections last used by
  that binding or an earlier one. This is the package's purpose: it stops
  transfers in code the caller cannot cancel. Scope the cutoff by binding
  generation: a cutoff callback can run after `stop` and a new `Bind`, and
  must not close the newer binding's connections. `stop` ends a binding
  without closing idle connections.
- Each Client owns its plain-HTTP upstream transport. Close its idle
  connections on cutoff, `Client.Close`, and `Proxy.Close`; in-flight
  upstream requests end through their request context.
- Forwarded responses flush every upstream read immediately. Buffering would
  hold a slow response's headers until the cutoff.
- `Proxy.Close` waits for every tunnel goroutine; start tunnels under the
  proxy lock after checking `closed`.
