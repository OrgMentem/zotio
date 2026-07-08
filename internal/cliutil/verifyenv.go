// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cliutil

import "os"

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
//	if !readOnlyIntent && isMutatingVerb(method) && cliutil.IsVerifyEnv() && !cliutil.IsVerifyLiveHTTPEnv() {
//	    // synthetic envelope, no network call
//	}
//
// readOnlyIntent is set by Client.doRead() callers (the PostQuery* family used
// for read-only operations on mutating verbs — GraphQL queries, JSON-RPC reads,
// POST-based search).
func IsVerifyLiveHTTPEnv() bool {
	return os.Getenv(VerifyLiveHTTPEnvVar) == "1"
}
