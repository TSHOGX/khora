# Startup and shutdown

## Startup sequence

Before serving traffic, the orchestrator seeds built-in Templates, reconciles
durable records with surviving Placements, installs the engine-level second-hop
network floor, and sweeps orphaned storage. `/readyz` stays unavailable until this
boot sequence completes. Assembly and loop ordering are in
[cmd/orchestrator/main.go](../../cmd/orchestrator/main.go) and
[internal/orchestrator/loops.go](../../internal/orchestrator/loops.go).

Start with explicit production paths:

```sh
sudo ./bin/orchestrator \
  --addr 127.0.0.1:8080 \
  --state /srv/khora/state \
  --store /var/lib/khora/store.db \
  --templates /var/lib/khora/templates
```

Use the generated [flag reference](../configuration/orchestrator.md) for defaults.

## Graceful shutdown

On the first SIGTERM, the orchestrator marks itself unready, stops accepting new
work, and drains in-flight handlers for at most `--shutdown-timeout`. Running
Sandboxes and their runsc units remain running so the next process can adopt them.
Routine shutdown does not pause Sandboxes because pause would discard their process
state.

A second SIGTERM forces immediate exit. The supplied systemd unit uses
`KillMode=process` so systemd escalation does not kill runsc units intended to
survive the control-plane restart. The shutdown behavior is tested in
[cmd/orchestrator](../../cmd/orchestrator) and
[test/pause_drain_substrate_test.go](../../test/pause_drain_substrate_test.go).

## Operational checks

After start, wait for `GET /readyz`, not only an open TCP socket. Before stopping,
remove the instance from upstream routing or wait for readiness polling to observe
the draining state. See [Health and metrics](health-and-metrics.md).
