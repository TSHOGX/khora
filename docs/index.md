# khora documentation

Start with [Getting started](getting-started.md) for the shortest path from a
built checkout to a running process.

## Understand the engine

- [Boundaries](concepts/boundaries.md): what belongs in khora and what belongs in
  an adapter.
- [Components](concepts/components.md): orchestrator, sandboxd, gateway, CLI, and
  subsystem relationships.
- [Objects](concepts/objects.md): Sandbox, Template, Overlay, Volume, Mount,
  Binding, Placement, and Event.
- [Lifecycle](concepts/lifecycle.md): state transitions, pause/resume, health,
  and reclamation.
- [Capacity](concepts/capacity.md): CPU, memory, disk, and admission.
- [Networking](concepts/networking.md): the persisted reachability declaration.
- [Security](concepts/security.md): trust boundaries and current limitations.

## Accomplish a task

- [Run and reconnect to processes](guides/processes.md)
- [Move files and use volumes](guides/storage.md)
- [Restrict egress and publish a port](guides/networking.md)

## Configure and operate

- [Host requirements](configuration/host.md)
- [Orchestrator flags](configuration/orchestrator.md)
- [Build Templates](configuration/templates.md)
- [Deploy the outward-facing boundary](configuration/deployment-boundary.md)
- [Start and stop](operations/startup-shutdown.md)
- [Read health and metrics](operations/health-and-metrics.md)
- [Recover after failures](operations/recovery.md)
- [Troubleshoot the substrate](operations/troubleshooting.md)

## Integrate and contribute

- [API overview](api/overview.md), [HTTP API](api/http.md),
  [data-plane API](api/data-plane.md), [errors and events](api/errors-and-events.md),
  and the generated [CLI reference](api/cli.md)
- [Internals](internals/README.md): implementation constraints, measurements,
  counterexamples, and their code/test anchors
- [Development workflow](development/workflow.md),
  [adding a provider](development/providers.md),
  [testing](development/testing.md), and
  [generated files](development/generated-files.md)

The machine-readable control-plane contract is the generated
[OpenAPI document](api/openapi.json).
