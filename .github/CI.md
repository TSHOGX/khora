# CI

GitHub CI runs the checks that are reproducible on hosted runners:

- build all packages;
- vet all packages;
- cross-compile for amd64;
- run unit and contract tests;
- check the committed OpenAPI output and regenerate protocol outputs, failing on a diff;
- verify reproducible builds.

Hosted CI does not run the substrate suite. It runs locally on a conforming host;
see [Testing and verification](../docs/development/testing.md) for the host
requirements, change classification, and commands. A successful hosted CI run
provides no evidence about real-substrate behavior.
