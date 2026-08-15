# The bare Template

The general-purpose base a sandbox is created from: one bare userland with a toolchain,
and **nothing agent-shaped**. This directory is its **definition**; `build.sh` turns that
into the rootfs the engine serves.

## Why the definition is here and the artifact is not

The built tree measures **~716 MB** (measured on arm64, after pruning docs and the apt
cache; the digest-pinned base alone is 101 MB, and `/usr/lib` is 411 MB of gcc, Node and
the Python runtime). Committing that would put 716 MB of binaries in git history
permanently — and every re-pin of `BASE` would store **another full copy**, because
compressed binaries have no useful delta. Extracting it again later is the expensive
direction.

So the repository holds what is small, diffable and worth reviewing:

| File | What it fixes |
| --- | --- |
| `BASE` | the base image, **by digest** — so the starting tree is a fixed input |
| `packages` | the content list, split **correctness** vs **convenience** (the rootfs subsystem) |
| `files/` | config the base does not ship (`nsswitch.conf`) |
| `build.sh` | definition → rootfs |

Build it with:

```sh
go build -o /tmp/sandboxd ./cmd/sandboxd
sudo ./templates/bare/build.sh /var/lib/khora/templates/bare /tmp/sandboxd
```

`/var/lib/khora/templates` is where `--templates` points by default. Each subdirectory
there is a builtin Template whose directory name IS its TemplateID, so the boot seed
registers `bare` on startup.

**The destination must be outside a Go module tree, and `build.sh` refuses otherwise.** A
Template contains mode-0700 root-owned directories (Debian's `/etc/ssl/private`), and
`go build ./...` walks the whole module tree — so one built under the repo makes every
later `go build ./...` fail with `permission denied` for the ordinary user, looking like a
broken checkout rather than a misplaced build product. `--state` escapes this only because
overlay directories are 0755. `ALLOW_IN_MODULE=1` overrides the refusal if you want it
anyway.

## What it ships, and why each part

**Correctness** — a sandbox is broken without these, and the failure is confusing rather
than obviously missing:

- **CA certificates.** Without them every HTTPS request fails at verification, so no
  `pip install`, no `npm install`, no `git clone` over HTTPS — most of what a sandbox is
  for. Verified through Python's SSL context rather than by listing a directory.
- **`/etc/nsswitch.conf`.** glibc's name-service switch consults it, so resolution can
  fail even with a valid resolver configured.
- **coreutils.** Package post-install hooks and build scripts shell out to
  `ls`/`install`/`mkdir`, so their absence breaks package installation.
- **A non-trivial `/etc/passwd`** (from the base). A root-only single line breaks
  `getpwuid` for tools that look themselves up. The guest is still root and the engine
  models no guest users (the guest identity boundary) — this is hygiene *inside* the guest,
  not an identity model.

**Convenience**, freely re-decidable: `python3` + `pip` + `venv`, `git`,
`build-essential`, `nodejs` + `npm`, `curl`, and `sudo`. Each is one exec away, so
shipping them trades cold-start size for convenience and commits to nothing.

**Preinstalled mount points**, which is a measured by-product rather than taste: a bind
destination already present in the lower does **not** trigger copy-up, while a missing one
dirties the upper — so this keeps Promoted layers clean and demotes the rootfs subsystem's exclusion
entries to a correctness backstop.

## No `/etc/resolv.conf`, deliberately

The Template ships none and **the engine injects none** (the rootfs subsystem, settled). Both
alternatives are worse: baking one in pins a build-machine resolver into a portable
artifact, and engine injection would put a deployment-specific network fact inside the
engine (the architecture boundary) *and* land in the overlay upper for every Promote to capture.

**So out of the box a sandbox resolves no names, and under `default: deny` that is by
design.** Resolution needs UDP/53 to reach a resolver; the resolver does not fall back to
TCP on its own (measured: with only TCP/53 allowed, the allow rule takes 0 packets while
the UDP drop counter climbs), and the engine deliberately does not add that entry for the
caller — it would have to know what a "resolver" is and hold an address.

A deployment supplies **both** halves: the file (through the transport subsystem's `fs` service after create,
or baked into its own Promoted Template) and the matching `egress.allow[]` entry. Which
resolver is even declarable is a property of that deployment — a loopback resolver is
structurally unreachable for a gVisor guest, a link-local one sits in the unliftable
floor, and the one stably-declarable host-local address is the sandbox's own `/30` gateway
(the runtime-provided address on the guest's point-to-point link).

## Agent stacks layer on this, outside the engine

No engine concept is needed: create a sandbox from `bare`, install with a one-shot exec
(the guest can start it directly without an init service), and **Promote**. The result is an ordinary
Template whose lower stack is `[the new layer, …bare's stack]`.

## Two traps in the builder

**`/dev` must be a tmpfs the builder creates, never a bind of the host's.** apt writes to
`/dev/null` constantly, and in a sandbox or CI the host's is frequently **read-only**. When
it is, apt does not report a permission problem — it reports
`gpgv … required for verification, but neither seems installed`, and **gpgv is already
present in this base**. Chasing that message is how a builder ends up reaching for
`--allow-unauthenticated`, putting a supply-chain hole in the artifact to work around a
read-only file. Nothing here passes that flag; signature verification is active, and
corrupting `/etc/apt/trusted.gpg.d` makes the build fail, which is what makes the pinned
digest worth having.

**Reproducibility here is about pinned INPUTS, not a bit-identical output.** `BASE` is a
digest, so the starting tree is fixed; package *versions* still float with the Debian
mirror. Pinning those too means building against a snapshot mirror — a separate decision,
not made here.
