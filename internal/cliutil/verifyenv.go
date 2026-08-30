// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cliutil

import (
	"os"
	"strconv"
)

// VerifyEnvVar enables zotio's verify sandbox mode. Under ZOTIO_VERIFY=1,
// commands that perform visible side effects (open browser tabs, send
// notifications, dial out to OS handlers) MUST short-circuit so automated
// verification does not spam the user's environment.
//
// The transport layer in internal/client also gates mutating HTTP verbs
// (DELETE/POST/PUT/PATCH) on this var: under verify mode such requests
// short-circuit with a synthetic envelope and never dial. VerifyLiveHTTPEnvVar
// can opt those mutating requests back in to the real wire path when a test or
// mock server needs to exercise actual HTTP behavior.
const VerifyEnvVar = "ZOTIO_VERIFY"

// VerifyLiveHTTPEnvVar opts a verify-mode process back in to the real HTTP
// wire path for mutating verbs. It is intentionally asymmetric with
// VerifyEnvVar: setting ZOTIO_VERIFY_LIVE_HTTP=1 alone (with ZOTIO_VERIFY
// unset) has no behavioral effect, because the gate only consults this var
// when IsVerifyEnv() is also true. Operators leave ZOTIO_VERIFY_LIVE_HTTP
// unset so mutating requests no-op during sandboxed verify runs; focused
// integration tests can set BOTH vars when their mock server must receive
// mutating requests.
const VerifyLiveHTTPEnvVar = "ZOTIO_VERIFY_LIVE_HTTP"

// IsVerifyEnv reports whether the current process is running under zotio's
// verify sandbox. Commands with visible side effects pair this check with
// print-by-default + explicit opt-in (--launch, --send, --play) so a verify
// pass does not pop browser tabs or fire off real notifications.
//
// Defense-in-depth: even if a side-effecting command misses an explicit
// sandbox guard, this env-var short-circuit catches it.
//
//	if cliutil.IsVerifyEnv() {
//	    fmt.Fprintln(cmd.OutOrStdout(), "would launch:", url)
//	    return nil
//	}
func IsVerifyEnv() bool {
	return os.Getenv(VerifyEnvVar) == "1"
}

// IsVerifyLiveHTTPEnv reports whether the current process has opted back in to
// the real HTTP wire path while running under zotio's verify sandbox. Only
// meaningful when IsVerifyEnv() is also true; on its own this returns true DOES
// NOT enable any sandbox behavior — see VerifyLiveHTTPEnvVar's docstring for
// the asymmetric semantics.
//
// The client uses this gate as:
//
//	if isMutatingVerb(method) && cliutil.IsVerifyEnv() && !cliutil.IsVerifyLiveHTTPEnv() {
//	    // synthetic envelope, no network call
//	}
//
// Read-only operations on POST-backed search, GraphQL, or JSON-RPC
// endpoints must not be routed through that gate — they do not mutate
// remote state and must dial even under verify mode.
func IsVerifyLiveHTTPEnv() bool {
	return os.Getenv(VerifyLiveHTTPEnvVar) == "1"
}

// StaleVersionEnvVar forces every outgoing write's If-Unmodified-Since-Version
// precondition to a fixed value, so a release rehearsal can provoke a REAL
// Zotero version conflict against a live library.
//
// Why this exists. The write engine resolves each object's version at apply
// time, immediately before the write, precisely so a stale plan cannot clobber
// a concurrent edit. That is correct, and it also makes a genuine 412
// unreachable from any CLI invocation: no command exposes a stale-version flag.
// The mutation conflict contract — a conflict returns the conflict rather than
// a generic "results incomplete" exit code — therefore shipped in 0.22.0
// defended by unit tests alone, and release gate 3 exists because a green suite
// hid every P0 in the 0.17.0 cycle by mocking the read/write plane split.
//
// Set it to a version the server has already moved past — 1 is always safe on a
// synced library — and the next write returns Zotero's own 412. It is
// deliberately an env var rather than a flag: it belongs to the rehearsal
// harness, not the command surface, so it stays out of --help, the capability
// registry, and the generated reference docs.
//
// Unset (or non-numeric, or <= 0) it has no effect whatsoever.
const StaleVersionEnvVar = "ZOTIO_TEST_STALE_VERSION"

// StaleVersionOverride returns the forced If-Unmodified-Since-Version value and
// whether one is in effect. See StaleVersionEnvVar.
func StaleVersionOverride() (int, bool) {
	raw := os.Getenv(StaleVersionEnvVar)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}
