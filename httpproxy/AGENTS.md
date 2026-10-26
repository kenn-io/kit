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
- When a binding's context ends, close all of that Client's connections. This
  is the package's purpose: it stops transfers in code the caller cannot
  cancel. `stop` ends a binding without closing idle connections.
- Forwarded responses flush every upstream read immediately. Buffering would
  hold a slow response's headers until the cutoff.
- `Proxy.Close` waits for every tunnel goroutine; start tunnels under the
  proxy lock after checking `closed`.
