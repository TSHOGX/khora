#!/usr/bin/env bash
# build.sh — build the bare Template's rootfs from this directory's definition.
#
# THE REPOSITORY HOLDS THIS DEFINITION, NOT THE ARTIFACT IT PRODUCES. The built tree
# measures ~716 MB (measured, after cleanup, on arm64), and git would store that
# permanently — with a full second copy on every re-pin of BASE, since compressed
# binaries have no useful delta. So the small, diffable, reviewable inputs live here and
# the artifact is built on demand or published as a release asset. See the documentation.
#
# Usage: build.sh <dest-template-dir> <path-to-sandboxd-binary>
#
# Reproducibility comes from the INPUTS being pinned, not from a bit-identical output:
# BASE names an image by DIGEST, so the starting tree is fixed. Package versions still
# float with the Debian mirror; pinning those too means a snapshot mirror, which is a
# separate decision (see the documentation).
set -euo pipefail

DEST="${1:?usage: build.sh <dest-template-dir> <sandboxd-binary>}"
SANDBOXD="${2:?usage: build.sh <dest-template-dir> <sandboxd-binary>}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

[ -f "$SANDBOXD" ] || { echo "build.sh: no sandboxd binary at $SANDBOXD" >&2; exit 1; }
for tool in skopeo umoci; do
	command -v "$tool" >/dev/null || { echo "build.sh: $tool is required" >&2; exit 1; }
done
[ "$(id -u)" = 0 ] || { echo "build.sh: must run as root (unpack sets ownership, chroot installs)" >&2; exit 1; }

BASE="$(grep -v '^[[:space:]]*#' "$HERE/BASE" | head -1)"
[ -n "$BASE" ] || { echo "build.sh: BASE is empty" >&2; exit 1; }
case "$BASE" in
	*@sha256:*) ;;
	*) echo "build.sh: BASE must be digest-pinned (…@sha256:…), got: $BASE" >&2; exit 1 ;;
esac

# REFUSE TO WRITE INSIDE A GO MODULE TREE, unless the caller says so out loud. This is a
# measured footgun rather than fastidiousness: a Debian rootfs contains /etc/ssl/private at
# mode 0700 root:root, and `go build ./...` walks the whole module tree — so a Template
# built under the module makes every subsequent `go build ./...` fail with
#
#     pattern ./...: open …/etc/ssl/private: permission denied
#
# for the ordinary (non-root) user, in a way that looks like a broken checkout rather than
# a misplaced build product. Note --state does NOT hit this only because overlay dirs are
# 0755; it is the Template's 0700 that bites. Refusing here is the same
# refuse-rather-than-degrade discipline the engine applies to a cap it cannot enforce.
if [ "${ALLOW_IN_MODULE:-0}" != 1 ]; then
	# realpath -m, because DEST does not exist yet: resolving with `cd` would fail on the
	# missing directory and silently skip this whole check, which is how the guard would
	# come to exist while never firing.
	probe="$(realpath -m "$DEST" 2>/dev/null || echo "")"
	while [ -n "$probe" ] && [ "$probe" != "/" ]; do
		if [ -f "$probe/go.mod" ]; then
			echo "build.sh: refusing to build into $DEST — it is inside the Go module at $probe," >&2
			echo "          and a Template contains mode-0700 root-owned directories that make" >&2
			echo "          \`go build ./...\` fail for a non-root user. Build outside the module" >&2
			echo "          (e.g. /srv/khora/templates/bare), or set ALLOW_IN_MODULE=1 to override." >&2
			exit 1
		fi
		probe="$(dirname "$probe")"
	done
fi

WORK="$(mktemp -d)"
# The tmpfs /dev below must be unmounted before the workdir can go, or rm walks into it.
cleanup() { umount "$WORK/u/rootfs/dev" 2>/dev/null || true; rm -rf "$WORK"; }
trap cleanup EXIT

echo "build.sh: pulling $BASE"
skopeo copy "docker://$BASE" "oci:$WORK/base:latest"
umoci unpack --image "$WORK/base:latest" "$WORK/u"
ROOT="$WORK/u/rootfs"

# A TMPFS /dev WITH OUR OWN /dev/null, RATHER THAN A BIND OF THE HOST'S — and this is a
# measured trap, not caution. apt writes to /dev/null constantly, and in a sandbox or CI
# the host's /dev/null is frequently READ-ONLY. When it is, apt does not report a
# permission problem: it fails with
#
#     E: gpgv, gpgv2 or gpgv1 required for verification, but neither seems installed
#
# which sends you off installing gpgv — and gpgv is ALREADY PRESENT in this base. Chasing
# that message is how a builder ends up reaching for --allow-unauthenticated, which would
# put a supply-chain hole in the artifact to work around a read-only file. Nothing here
# passes that flag, and signature verification is active: corrupting
# /etc/apt/trusted.gpg.d makes this build FAIL, which is what makes the pinned digest
# worth having.
mount -t tmpfs tmpfs "$ROOT/dev"
mknod -m 666 "$ROOT/dev/null" c 1 3
mknod -m 666 "$ROOT/dev/zero" c 1 5
mknod -m 644 "$ROOT/dev/urandom" c 1 9

# A resolver for the BUILD only, so apt can reach the mirror. It is removed below: the
# artifact deliberately ships none (see the documentation).
cp /etc/resolv.conf "$ROOT/etc/resolv.conf"

mapfile -t PKGS < <(sed 's/#.*//' "$HERE/packages" | tr -s '[:space:]' '\n' | grep -v '^$')
echo "build.sh: installing ${#PKGS[@]} package(s)"
chroot "$ROOT" /usr/bin/env DEBIAN_FRONTEND=noninteractive sh -c \
	"apt-get update -q && apt-get install -y -q --no-install-recommends $(printf '%s ' "${PKGS[@]}")"

# Config the base does not ship, or ships too thin to be useful.
install -m 0644 "$HERE/files/nsswitch.conf" "$ROOT/etc/nsswitch.conf"

# Mount points preinstalled, which is a MEASURED by-product rather than tidiness: a bind
# destination already present in the lower does NOT trigger copy-up, while a missing one
# dirties the upper — so this keeps Promoted layers clean and demotes the Rootfs subsystem's exclusion
# entries to a backstop (rootfs mount constraints). It cannot cover a caller's own choice of
# dest, which is why that mechanism still exists.
for d in proc sys dev tmp run home root etc/ssl workspace data; do
	mkdir -p "$ROOT/$d"
done
chmod 1777 "$ROOT/tmp"

install -m 0755 "$SANDBOXD" "$ROOT/usr/sbin/sandboxd"

echo "build.sh: pruning build residue"
chroot "$ROOT" sh -c 'apt-get clean' || true
rm -rf "$ROOT"/var/lib/apt/lists/* "$ROOT"/var/cache/apt/* \
	"$ROOT"/usr/share/doc "$ROOT"/usr/share/man "$ROOT"/usr/share/locale

# NO /etc/resolv.conf IN THE ARTIFACT, and the engine injects none either (the Rootfs subsystem, settled).
# Baking one in pins a BUILD-MACHINE resolver into a supposedly portable artifact, and it
# is actively harmful rather than merely wrong elsewhere: an address the deployment's
# egress.allow[] does not name produces a 10–20 s hang then `Could not resolve host`
# (network substrate measurements) — the worst diagnostic shape in the system — BY DEFAULT, on every
# deployment that does not happen to match this machine.
rm -f "$ROOT/etc/resolv.conf"

umount "$ROOT/dev"
rm -rf "${DEST:?}"
mkdir -p "$(dirname "$DEST")"
mv "$ROOT" "$DEST"

echo "build.sh: built $DEST ($(du -sh "$DEST" | cut -f1))"
echo "build.sh: NOTE — this sandbox resolves no names until the deployment supplies both"
echo "          /etc/resolv.conf and a UDP/53 egress.allow[] entry (the documentation)."
