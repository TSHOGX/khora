# Implementation internals

These pages are for maintainers changing mechanisms. They record constraints that
are easy to violate, failure modes that determine control flow, measured substrate
behavior, and direct code or test evidence. Public usage belongs in concepts,
guides, configuration, operations, or API reference instead.

- [Lifecycle](lifecycle.md): transition locks, rollback, barriers, admission, and
  reconciliation
- [Store](store.md): authoritative records, atomic events, and publisher failure
- [Runtime](runtime.md): provider contract, runsc Placements, and nftables ownership
- [Guest runtime](guest-runtime.md): PID 1, process supervision, and quiescence
- [Rootfs](rootfs.md): overlay carrier, promotion, usage, and sweeps
- [Volume](volume.md): backend seam, access regime, lineage, and direct I/O
- [btrfs](btrfs.md): shared `squota` primitive and quota-row hazards
- [Network](network.md): rule ordering, fail-closed enforcement, and counters
- [Transport](transport.md): guest protocol, proxy barrier, and fan-out behavior
- [Contracts](contracts.md): durable object, epoch, operation, cursor, error,
  measurement, and decision-gate contracts

Open work is not kept as a parallel register here. Link the ruling Issue instead:
[remote provider input shape](https://github.com/TSHOGX/khora/issues/7).
