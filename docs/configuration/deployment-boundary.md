# Deployment boundary

The orchestrator has no TLS or authentication. Keep it on loopback and put an
authenticating boundary in front for remote callers.

## Listener enforcement

The default listener is `127.0.0.1:8080`. A hostname is accepted only when every
resolved address is loopback. Empty-host and routable addresses are refused unless
`--insecure-listen` is explicitly set. The enforcement and its cases are in
[internal/orchestrator/listen.go](../../internal/orchestrator/listen.go) and
[internal/orchestrator/listen_test.go](../../internal/orchestrator/listen_test.go).

The override records an operator decision; it does not add protection. Use it only
when an external network boundary already makes the listener unreachable to
untrusted clients.

## nginx with mTLS

[deploy/nginx-gateway.conf](../../deploy/nginx-gateway.conf) terminates TLS 1.3,
requires a client certificate, carries streaming REST/Connect traffic, and routes
native gRPC requests to the same loopback listener. Supply a narrowly scoped client
CA, server certificate, and private key. Do not use a public trust bundle as the
client CA.

The configuration clears forwarded identity headers. Authentication is complete at
the gateway; the engine has no caller identity or authorization subject.

## Bundled gateway

Deployments without a reverse proxy can run the repository gateway:

```sh
./bin/gateway \
  --addr :8443 \
  --cert /etc/khora/server.pem \
  --key /etc/khora/server-key.pem \
  --client-ca /etc/khora/client-ca.pem \
  --upstream http://127.0.0.1:8080
```

It refuses to start without the material required for mTLS. Proxy behavior and
header stripping are tested in [cmd/gateway](../../cmd/gateway).

## systemd

The supplied [systemd unit](../../deploy/khora-orchestrator.service) uses absolute
state paths, leaves runsc Placements outside routine orchestrator shutdown, and sets
its stop timeout above the orchestrator's bounded drain. Adjust the unit and
`--shutdown-timeout` together. Validate edits with `systemd-analyze verify` and the
real host profile; syntactic validation cannot prove that namespace or quota
operations remain permitted.
