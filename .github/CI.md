# CI

GitHub CI runs the checks that are reproducible on hosted runners:

- build all packages;
- vet all packages;
- cross-compile for amd64;
- run unit and contract tests;
- check the committed OpenAPI output and regenerate protocol outputs, failing on a diff;
- verify reproducible builds.

Hosted CI does not run the substrate suite. The suite needs a conforming Linux host with root,
gVisor/runsc, overlayfs, netns, cgroup v2, btrfs in `squota` mode, `nft` and `ip`. Developers
run it locally before marking a PR ready.

## PR verification

The PR template records the substrate result after the last material change:

```text
Substrate: passed | failed | not required
Command: <Make target or exact command>
Host: <host profile>
Result: <pass, failure summary, or reason not required>
```

Run `go run ./test/preflight/substrategate --base origin/main` first when reviewing a PR. It
classifies the complete committed PR diff plus any local changes. A material diff requires the
substrate suite. Documentation, repository-process files, unit-test-only changes and
token-preserving production Go changes may record that it is not required. Re-run the decision
after any later material change.

The local run is evidence attached to the PR, not a generated document committed to this
repository. CI does not claim to have run the substrate suite or infer a local result from a
checkbox.
