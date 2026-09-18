# Adding a provider implementation

Provider work currently happens inside this repository. The four mechanism seams
and their shipped implementations are listed in [Engine
boundaries](../concepts/boundaries.md). Their packages and the standard assembly are
under `internal/`; Khora does not expose a public provider API. Volume selection is
an internal, typed assembly choice; third-party providers and dynamic plugins remain
outside the engine.

## Choose the owning seam

Add behavior to the subsystem that owns the mechanism:

| Need | Seam |
| --- | --- |
| Durable records, references, and atomic Event commits | `store.Store` |
| Live isolation, resource enforcement, and installed network rules | `runtime.Provider` |
| Template, Overlay, and merged rootfs materialization | `rootfs.Provider` |
| Independent Volume storage, capacity, and derivation | `volume.Backend` |

Network declaration validation and rule derivation remain in `network`; transport
to the guest remains in `transport`. Do not introduce a second provider name for
either concern or let one subsystem call another. A lifecycle operation that spans
mechanisms belongs in `internal/lifecycle`.

## Implement the contract

Read the complete interface contract before choosing a mechanism. Policy crosses a
seam as IDs, numeric intent, enums, declarations, and portable handles. Cgroup
paths, namespace descriptors, quota identifiers, and unrestricted host paths stay
inside the implementation.

Provider differences must not change lifecycle correctness. A Store delivers the
required atomicity rather than advertising whether it has transactions. A Volume
backend reports the capabilities the contract explicitly models and returns the
typed unsupported error before allocating an ID or moving bytes. Do not add a
capability branch merely to excuse an implementation from an existing obligation.

## Prove the implementation

Keep implementation unit and substrate tests beside the owning package. A fake used
by lifecycle tests proves the caller's behavior under controlled results; it is not
evidence that a provider implementation satisfies its contract.

- Every Store runs `internal/store/storetest.RunContract`.
- Every Volume backend runs `internal/volume/volumetest.RunContract`.
- Runtime and rootfs currently have one implementation each. Before adding a second,
  extract reusable behavioral assertions where both implementations can run them;
  retain mechanism-specific substrate tests for facts a shared suite cannot prove.
- A seam-shape change must keep the structural guards green. A new structural rule
  needs a committed negative fixture that demonstrates its failure.
- Cross-seam lifecycle and externally observable API behavior belongs in lifecycle,
  orchestrator, or `test/` rather than being duplicated in each provider suite.

The test layers and focused commands are documented in [Testing](testing.md).

## Wire the assembly

The standard constructors are selected in `internal/orchestrator.New` through the
typed `volume.ProviderConfig` registry. The shipped choices are `local_btrfs` and
the uncapped `plain_dir` arm; the latter is intended for development and contract
tests, while a positive cap is refused by its capability contract. Existing Volume
rows always route by their persisted `backend` name, not by the current default.
Unknown or unavailable names fail startup (and operation-level lookups fail closed)
instead of falling back.

The assembly fields are:

| Selection | Provider-owned settings |
| --- | --- |
| `VolumeProvider: "local_btrfs"` (default) | `VolumeLocalBtrfs.VolumesDir`, or `VolumesDir`/`StateDir/volumes` when omitted |
| `VolumeProvider: "plain_dir"` | `VolumePlainDir.Dir` (uncapped; positive caps are rejected) |

The command-line equivalent is `--volume-provider` plus `--volumes-dir`. A provider
constructor error, a failed readiness check, or a durable row naming a provider not
present in the registry prevents startup; no row is silently redirected.

Keep provider-specific configuration at that assembly boundary; do not put it in
Sandbox, Template, Volume, or public API objects. To add an internal choice,
implement `volume.Backend`, add its typed settings and constructor to
`volume.ProviderConfig`/`volume.Assemble`, run the shared contract suite, and add
startup and mixed-provider routing tests before exposing the name in the CLI.

Update the implementation table, host requirements, configuration, operations, and
test instructions in the same change. Describe unsupported behavior as a limitation,
not as a future implementation that already exists.

## Quota and measurement capabilities

`Capabilities.Usage` states whether per-unit and aggregate allocation measurements
are available. `plain_dir` measures allocated file blocks. `local_btrfs` uses
usable `squota` for quota enforcement and measurement. With quotas confirmed off,
it still supports native subvolumes, snapshots and clones, but reports neither
Quota nor Usage. A configured node disk budget requires measurement from every
registered Volume backend. Unknown quota state is an error, not an uncapped mode.

## Deferred storage cleanup

`volume.Backend.Reclaim(ctx)` processes the backend's own recorded residue and
returns a diagnostic count plus any error, including partial failure. Empty work
is a no-op. Pending work and substrate identifiers remain inside the provider;
lifecycle supplies each pass with a deadline and continues other maintenance after
failure. Implementations must honor cancellation and retain unconfirmed work.
