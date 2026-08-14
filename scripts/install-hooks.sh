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

# git feeds the push plan on stdin and it can be read ONCE. Capture it here and hand it
# to the one gate that needs it; the others must not inherit a consumed stdin, hence the
# </dev/null below.
plan=$(cat)

if [[ -x "$root/scripts/check-no-direct-main.sh" ]]; then
  printf '%s\n' "$plan" | "$root/scripts/check-no-direct-main.sh" || fail=1
fi

for s in check-asks check-spec-code-drift; do
  [[ -x "$root/scripts/$s.sh" ]] || continue
  "$root/scripts/$s.sh" </dev/null || fail=1
done
if [[ $fail -ne 0 ]]; then
  echo
  echo "pre-push: a gate failed. Fix it, or --no-verify if the gate is wrong."
  exit 1
fi
HOOK
chmod +x .git/hooks/pre-push
echo "installed .git/hooks/pre-push"
