---
name: go-no-t-fatal-off-test-goroutine
description: "t.Fatal inside an httptest handler runs on the server goroutine, where it cannot stop the test and may be swallowed"
condition:
  - '(?s)func\s*\(\s*\w+\s+http\.ResponseWriter\s*,\s*\w+\s+\*http\.Request\s*\)[^\n]*\{(?:(?!\}\))(?!\n\t\}).)*?\bt\.Fatalf?\('
globs:
  - "*_test.go"
scope:
  - "tool:edit"
  - "tool:write"
interruptMode: always
---

**`t.Fatal` must run on the goroutine that runs the test.** `net/http/httptest`
serves each request on its own goroutine, so a `t.Fatalf` in the handler calls
`runtime.Goexit` there. The documented consequence is that the test does not
stop: it keeps running past the point the author believed was fatal, and the
failure surfaces late, out of order, or not at all.

This is live in this repo: 69 call sites across 14 test files as of
2026-08-22. One was repaired the same day in
`internal/cli/library_health_test.go`, where a handler asserting "this server
must never be probed" could not have reported the violation it was written to
catch.

Report from the handler, assert on the test goroutine:

```go
// wrong: runs on the server goroutine
srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    t.Fatalf("must not be called: %s", r.URL.Path)
}))

// right: t.Errorf marks failure without Goexit, or record and assert after
var calls atomic.Int64
srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    calls.Add(1)
    t.Errorf("must not be called: %s", r.URL.Path)
}))
// ... exercise ...
if got := calls.Load(); got != 0 {
    t.Fatalf("server called %d times, want 0", got)
}
```

`t.Errorf` and `t.Logf` are safe from any goroutine. `t.Fatal`, `t.Fatalf`,
`t.FailNow` and `t.Skip` are not.
