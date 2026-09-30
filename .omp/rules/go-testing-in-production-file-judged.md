---
name: go-testing-in-production-file-judged
description: "Judged companion to go-test-only-code-in-production-file: any .go file without the _test.go suffix that uses testing ships it in the binary"
condition:
  - '\*testing\.(T|B|TB)\b'
  - '(?m)^\s*"testing"\s*$'
globs:
  - "*.go"
scope:
  - "tool:edit"
  - "tool:write"
question: "Is this edited file a Go file whose path does not end in _test.go, and does it import testing or take a *testing.T, *testing.B, or testing.TB?"
---

**Only a `_test.go` suffix keeps a file out of the build.** This file compiles
into the shipped binary, so `testing` and its hooks ship with it.

Rename the file to the `_test.go` suffix, or move the test-only code into a
`_test.go` file. Then check the result:

```bash
grep -ln '"testing"' --include=*.go -r internal cmd | grep -v _test.go
```

That command must print nothing. Background: `rule://go-test-only-code-in-production-file`.
