#!/usr/bin/env bash
# build-rootfs.sh — preflight: build a minimal guest rootfs Template directory
# from this machine's own userland.
#
# It replicates usrmerge (/bin→usr/bin, /lib→usr/lib, /lib64, /sbin) so ELF
# interpreter resolution works inside the guest; without it, dynamically-linked
# binaries fail to load (the root-required deployment). /bin/sh here is dash + libc.so.6 +
# ld-linux-aarch64.so.1 — no coreutils. The e2e assertions use host-side
# fs.write/read + `sh -c 'echo'`, which do not need guest coreutils.
#
# The static sandboxd binary (CGO off) is copied to /sbin/sandboxd as the
# container entrypoint (the guest startup topology init mode = PID 1).
#
# Usage: build-rootfs.sh <dest-template-dir> <path-to-sandboxd-binary>
# Idempotent: wipes and rebuilds dest. Runs unprivileged (plain file copy).
set -euo pipefail

DEST="${1:?usage: build-rootfs.sh <dest-template-dir> <sandboxd-binary>}"
SANDBOXD="${2:?usage: build-rootfs.sh <dest-template-dir> <sandboxd-binary>}"

if [[ ! -x "$SANDBOXD" ]]; then
  echo "build-rootfs: sandboxd binary not found/executable: $SANDBOXD" >&2
  exit 1
fi

rm -rf "$DEST"
mkdir -p "$DEST"

# usrmerge layout: real dirs under /usr, top-level symlinks pointing into them.
mkdir -p "$DEST/usr/bin" "$DEST/usr/lib" "$DEST/usr/sbin"
ln -s usr/bin "$DEST/bin"
ln -s usr/lib "$DEST/lib"
ln -s usr/sbin "$DEST/sbin"
# arm64 dynamic loader lives at /lib/ld-linux-aarch64.so.1 via the symlink above;
# also provide /lib64 for loaders that record that path.
ln -s usr/lib "$DEST/lib64"

# Runtime/kernel mountpoints + writable scratch the guest expects.
mkdir -p "$DEST/proc" "$DEST/sys" "$DEST/dev" "$DEST/tmp" "$DEST/run" \
         "$DEST/home" "$DEST/root" "$DEST/etc"
chmod 1777 "$DEST/tmp"

# --- /bin/sh = dash + its shared-object closure ------------------------------
copy_with_deps() {
  local bin="$1" dst_name="${2:-}"
  local real; real="$(readlink -f "$bin")"
  local target="$DEST/usr/bin/${dst_name:-$(basename "$bin")}"
  cp -a "$real" "$target"
  # copy every shared object ldd reports, preserving the aarch64-linux-gnu subdir
  ldd "$real" 2>/dev/null | grep -oE '/[^ ]+\.so[^ ]*' | sort -u | while read -r so; do
    [[ -e "$so" ]] || continue
    local rel="${so#/}"           # strip leading /
    rel="${rel#usr/}"             # normalize /usr/lib/... and /lib/... alike
    local out="$DEST/usr/${rel#lib/}"
    # Place under usr/lib/<...> preserving subpath after the first lib/
    local sub="${so#*/lib/}"
    mkdir -p "$DEST/usr/lib/$(dirname "$sub")"
    cp -aL "$so" "$DEST/usr/lib/$sub" 2>/dev/null || true
  done
}

# dash → /usr/bin/dash, and /bin/sh symlink
copy_with_deps /usr/bin/dash dash
ln -sf dash "$DEST/usr/bin/sh"

# The dynamic loader itself (ld-linux) — copy real file to the exact path the
# ELF interpreter field names: /lib/ld-linux-aarch64.so.1 (→ usr/lib via symlink).
LD_REAL="$(readlink -f /lib/ld-linux-aarch64.so.1)"
LD_SUB="${LD_REAL#*/lib/}"                 # e.g. aarch64-linux-gnu/ld-linux-aarch64.so.1
mkdir -p "$DEST/usr/lib/$(dirname "$LD_SUB")"
cp -aL "$LD_REAL" "$DEST/usr/lib/$LD_SUB"
# and the top-level name the interpreter is recorded as
cp -aL "$LD_REAL" "$DEST/usr/lib/ld-linux-aarch64.so.1"

# --- sandboxd entrypoint (static) --------------------------------------------
cp -a "$SANDBOXD" "$DEST/usr/sbin/sandboxd"
chmod 0755 "$DEST/usr/sbin/sandboxd"

# minimal /etc so getpwuid/hostname etc. don't explode
printf 'root:x:0:0:root:/root:/bin/sh\n' > "$DEST/etc/passwd"
printf 'root:x:0:\n' > "$DEST/etc/group"
printf 'guest\n' > "$DEST/etc/hostname"

echo "build-rootfs: built Template at $DEST"
