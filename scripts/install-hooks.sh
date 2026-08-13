#!/usr/bin/env bash
# Installs the pre-push hook. Run once per clone: ./scripts/install-hooks.sh
#
# Why a hook rather than CI alone: branch protection is unavailable on a private repo
# on this plan (the API returns 403), so a CI check CANNOT block a merge. CI is the
# second opinion; the hook is the only thing that actually gates (ADR-0009).
#
# Escape hatch, for when the gate is wrong rather than you: git push --no-verify
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
mkdir -p .git/hooks
cat > .git/hooks/pre-push <<'HOOK'
#!/usr/bin/env bash
set -uo pipefail
root=$(git rev-parse --show-toplevel)
fail=0
for s in check-asks; do
  [[ -x "$root/scripts/$s.sh" ]] || continue
  "$root/scripts/$s.sh" || fail=1
done
if [[ $fail -ne 0 ]]; then
  echo
  echo "pre-push: a gate failed. Fix it, or --no-verify if the gate is wrong."
  exit 1
fi
HOOK
chmod +x .git/hooks/pre-push
echo "installed .git/hooks/pre-push"
