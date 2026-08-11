# Deployment: the outward-facing boundary

The engine's control-plane API has **no authentication and no TLS**, and that is a
design decision rather than a gap: §1.4 excludes the credential family from the engine
permanently, because every byte the engine can hand a guest is readable by uid 0
inside it — so the engine cannot be a secrecy boundary for anything. Authentication
therefore lives in front of it, in a component outside the engine.

This directory holds the configuration for that component. The decision behind it,
including the options rejected, is [`ADR-0007`](../docs/decisions/ADR-0007-outward-facing-boundary.md).

## The engine enforces its own half

The orchestrator **refuses a non-loopback `--addr` at startup**. That is what makes
whatever you put in front of it the only outward-facing surface *by construction*
rather than by an operator remembering: if the engine could also listen on a routable
address, a gateway would be a door standing beside an open wall.

```
$ orchestrator --addr 0.0.0.0:8080
orchestrator: the §6.1 listen address is not loopback: "0.0.0.0:8080" is a routable
address. The §6.1 API has NO authentication and NO TLS …
```

Every loopback spelling is accepted (`127.0.0.1`, anything in `127/8`, `::1`, the
v4-mapped `[::ffff:127.0.0.1]`). An empty host (`:8080`) is refused, because it binds
every interface. A **name** is accepted only when every address it resolves to is
loopback — one routable answer in `/etc/hosts` is enough to publish the API.

`--insecure-listen` permits a routable listener, for a deployment where the network
itself is the boundary. It exists so that publishing an unauthenticated control plane
is a decision on the record — in `ps` output and in the startup log — rather than an
omission.

## Two ways to provide the other half

### A reverse proxy — the recommended path

[`nginx-gateway.conf`](nginx-gateway.conf) is a complete, tested nginx server block:
TLS termination, client-certificate authentication, and `grpc_pass` to the engine's
loopback listener with the timeouts long-lived process streams need.

Recommended for the plain reason that most operators already run a reverse proxy:
configuring a component you already trust beats trusting a new binary of ours on the
security path. Caddy or Envoy work equally well — the requirements to carry across are
in the config's comments.

```sh
cp deploy/nginx-gateway.conf /etc/nginx/sites-available/warren.conf
ln -s /etc/nginx/sites-available/warren.conf /etc/nginx/sites-enabled/
# server.pem/server.key = this gateway's identity
# client-ca.pem         = the CA that issues CALLER certificates. Scope it to a CA you
#                         issue from: a public bundle here authenticates the internet.
nginx -t && systemctl reload nginx
```

### `cmd/gateway` — for deployments that would rather not add a proxy

The same boundary as a single binary from this repo. Requires the same three PEM files
and refuses to start without them, since a gateway that starts without the material to
authenticate would forward every caller to an unauthenticated control plane while
looking like a boundary.

```sh
go build ./cmd/gateway
./gateway --addr :8443 \
          --cert server.pem --key server.key \
          --client-ca client-ca.pem \
          --upstream http://127.0.0.1:8080
```

mTLS rather than bearer tokens: a token would mean this repo ships issuance, storage
and revocation, and the whole family is what §1.4 excludes. mTLS puts caller identity
in the TLS layer, where your existing PKI holds it. The trade is that you issue and
rotate client certificates — which is the reason the reverse proxy stays the
recommended path for anyone who already runs an identity provider.

## What neither option does

**Neither forwards caller identity to the engine.** nginx can pass the verified
certificate on and this config does not; `cmd/gateway` strips the `X-Forwarded-*`
headers. The engine has no authorization subject, and §1.1 bars the names such a
header would need — authentication is complete at the gateway, and that is the entire
contract between the two processes.

**Neither is on the engine's startup path.** Like the process behind `egress.via[]`,
the engine does not start it, health-check it, or know it exists. A gateway that is
down is an unreachable API, not a degraded engine.

## Verifying a deployment

The four checks worth running, and what a correct deployment answers:

```sh
# 1. no client certificate → refused, and the engine never sees the request
curl --cacert ca.pem https://gateway/v1/sandboxes
#    → 400 No required SSL certificate was sent

# 2. a certificate from another CA → refused
curl --cacert ca.pem --cert rogue.pem --key rogue.key https://gateway/v1/sandboxes
#    → 400 The SSL certificate error

# 3. a valid certificate → served
curl --cacert ca.pem --cert client.pem --key client.key https://gateway/v1/sandboxes
#    → 200

# 4. the engine is NOT reachable directly from off-host
curl http://engine-host:8080/v1/sandboxes
#    → connection refused (the listener is on loopback)
```

Check 4 is the one that is easy to skip and the one that decides whether the other
three mean anything.
