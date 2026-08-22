---
name: go-test-only-code-in-production-file
description: "A Go file with test in its name but no _test.go suffix compiles into the shipped binary — testing and its hooks must not ship"
condition:
  - '\*testing\.T\b'
  - '\*testing\.B\b'
  - '(?m)^\s*"testing"\s*$'
globs:
  - "*test_*.go"
  - "*_test_*.go"
  - "*testing_*.go"
  - "*testhelper*.go"
  - "*_tests.go"
scope:
  - "tool:edit"
  - "tool:write"
interruptMode: always
---

**Only a `_test.go` suffix keeps a file out of the build.** A name that merely
contains `test` does not. `fast_retry_test_helper.go` reads like a test file and
is not one: Go compiles it into the CLI, links `testing` into the shipped
binary, and exports the hook to production callers.

This happened in this repo on 2026-08-22. A helper holding
`fastRetryBackoff(t *testing.T)` landed as `internal/cli/fast_retry_test_helper.go`.
Every gate passed — it is valid Go — and the file was caught by eye, not by CI.
It is the same defect class the dead-code audit had just finished removing:
test-only symbols kept alive in production source.

Rename to the `_test.go` suffix:

```
internal/cli/fast_retry_test_helper.go   ->  internal/cli/fast_retry_helper_test.go
```

Check the result rather than the intention:

```bash
grep -ln '"testing"' --include=*.go -r internal cmd | grep -v _test.go
```

That command must print nothing.

**Scope of this guard.** It gates on the filename, because the content of a
misnamed helper is identical to a correct one. A production file whose name
carries no hint of testing — `helpers.go` holding a `*testing.T` parameter —
does not match, so the `grep` above stays the real check before a commit.
