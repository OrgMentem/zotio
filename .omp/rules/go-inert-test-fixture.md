---
name: go-inert-test-fixture
description: "A fixture built then discarded, or a test that ends in t.Skip, reports success while proving nothing"
condition:
  - '(?m)^\s*\b(\w+)\s*:?=\s*httptest\.NewServer[\s\S]{0,3000}?^\s*_ = \1\s*$'
  - '(?s)httptest\.NewServer[\s\S]{0,3000}?\bt\.Skip\('
  - '(?s)\bt\.Skip\([^)]*\b(informational|documents|records that|cannot be exercised|for reference)'
globs:
  - "*_test.go"
scope:
  - "tool:edit"
  - "tool:write"
interruptMode: always
---

**A test that cannot fail is worse than no test: it reads as coverage.** Two
shapes trip this rule, and both shipped in this repo.

**A fixture that is built and then discarded.**
`TestBrokenAttachmentFile_LocalProbeErrorYieldsSkip_NoHeldPort` built an
httptest server whose handler was the whole assertion, registered its cleanup,
then wrote `_ = srv`. No production function was ever called with `srv.URL`, so
the handler was unreachable and the assertion inside it could never fire. The
comment above it claimed to prove the ephemeral base takes the other skip path.
Nothing proved it.

**A test that ends in `t.Skip`.** The toolchain reports SKIP even on a clean
run, so a dashboard counting skips as not-run sees nothing, and a reader
scanning results never learns that most of the body is inert. `t.Skip` is for a
precondition the environment cannot meet, decided before the work — not for
prose about an assertion that was never wired up.

If the claim can be tested, test it. If it cannot, delete the code and leave a
comment; a comment does not pretend to be a passing test.

**Before writing either shape, prove the test bites.** Break the production
line it names, run it, and require a FAIL that names the right thing. Restore
and require a PASS. A test that passes both ways is not defending anything.

`t.Skip` guarding a genuinely absent precondition is fine — an occupied port, a
missing binary — as long as the condition is checked, not assumed.
