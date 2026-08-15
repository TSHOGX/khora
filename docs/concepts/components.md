# Components

## Deployment topology

```text
caller
  | HTTPS / mTLS
gateway
  | loopback HTTP + Connect-RPC
orchestrator
  | runtime, rootfs, volume, network, store providers
runsc sandbox
  | TCP inside the sandbox network namespace
sandboxd (PID 1 supervisor + transport service)
```

The `khora` CLI and Go client call the orchestrator. Control-plane resources use
HTTP; process and filesystem interaction uses Connect-RPC. A caller never reaches
`sandboxd` directly. The proxy boundary is implemented in
[cmd/gateway](../../cmd/gateway) and [internal/orchestrator/dataplane.go](../../internal/orchestrator/dataplane.go).

## Orchestrator

The orchestrator assembles providers, owns lifecycle verbs, serves the HTTP and
Connect surfaces, reconciles durable records with substrate state, and publishes
the durable outbox. It is the only component that coordinates subsystems.

## sandboxd

The guest binary runs in two roles. Its init role is PID 1, forwards signals,
reaps children, and supervises the transport role. The transport role exposes the
process and filesystem services. Both roles are implemented in
[cmd/sandboxd](../../cmd/sandboxd).

## Data ownership

The store owns durable control-plane facts. Runtime, rootfs, network, and volume
providers own substrate artifacts. A Placement is live provider state rather than
part of Sandbox identity. Startup and periodic reconciliation compare both sides
through the lifecycle mediator; see [Recovery](../operations/recovery.md).
