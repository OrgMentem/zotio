# Dependency update contracts

Read this reference before work in this area. Paths in code spans and commands are repository-root-relative unless stated otherwise.

## Dependency Updates

Two invariants that a routine `go get -u` breaks silently. Both are gated, so `make ci` catches them before CI does.

- **`modernc.org/sqlite` and `modernc.org/libc` move together, in one commit.** sqlite ships a transpiled C runtime whose ABI is coupled to the one exact libc revision its own `go.mod` pins; a `require` line is a floor, not an equality, so MVS accepts a higher libc that tidies, verifies, builds, and then faults at run time (upstream reports SIGBUS in the WAL index under concurrent access — gitlab.com/cznic/sqlite#177). `make lockstep` asserts the equality, in the `tidy` job and before every release. The `sqlite-currency` job in `vuln.yml` reports a lagging embedded SQLite as a warning annotation and still passes: upstream patch releases are outside this repo's control, and a job that sits red for weeks is a job nobody reads. Check the SQLite release notes for security content when it warns.
- **Attribution is generated, not hand-written.** `THIRD_PARTY_LICENSES.txt` is produced by `make notices` from the modules the two released binaries link, unioned across all six release targets, and it is drift-gated in CI. Adding, removing, or bumping a dependency means regenerating it. Every channel ships it: release archives (hence also the Homebrew cask, Scoop, and WinGet), deb/rpm/apk under `/usr/share/doc/zotio/`, the MCPB bundles, and the container image.

No advisory database covers most of this module set, so `vuln.yml` runs both `govulncheck` (Go module paths) and OSV-Scanner (GHSA and other feeds). Neither can see a SQLite CVE; that is what the currency job is for.
