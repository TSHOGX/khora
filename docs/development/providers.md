# Adding a provider implementation

Provider work currently happens inside this repository. The four mechanism seams
and their shipped implementations are listed in [Engine
boundaries](../concepts/boundaries.md). Their packages and the standard assembly are
under `internal/`; khora does not yet expose a public provider API or a configuration
selector. That future direction is tracked in
[Issue #25](https://github.com/TSHOGX/khora/issues/25).

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

The standard constructors are currently selected directly in
`internal/orchestrator.New`. An in-tree implementation is not shipped merely because
it satisfies an interface: wire it only when its configuration, startup failures,
capabilities, reconciliation behavior, and substrate requirements are defined.

Until Issue #25 establishes configurable selection, adding a deployment choice
requires an explicit assembly change. Keep provider-specific configuration at that
assembly boundary; do not put it in Sandbox, Template, Volume, or public API objects.

Update the implementation table, host requirements, configuration, operations, and
test instructions in the same change. Describe unsupported behavior as a limitation,
not as a future implementation that already exists.
