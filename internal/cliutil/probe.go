// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cliutil

import "time"

// defaultProbeTimeout caps the request when the caller passes a nil
// client and didn't set a context deadline. Without this cap, a probe
// against a non-responsive host could hang indefinitely (the global
// http.DefaultClient has no Timeout). Callers who pass their own
// *http.Client are expected to set Timeout there; this value only
// applies to the nil-client fallback.
const defaultProbeTimeout = 10 * time.Second

// ReachabilityStatus is one of the strings returned by ProbeReachable.
// Callers (typically a doctor command listing per-source health) match
// on these constants when deciding whether to render OK/WARN/FAIL.
const (
	// ReachabilityReachable means the host responded with a 2xx, a 206
	// (partial — Range honored), or a 416 (Range not honored, but the
	// host did respond with headers). All three are evidence that the
	// host is alive and responding to GET.
	ReachabilityReachable = "reachable"
	// ReachabilityBlocked means the host responded with a 4xx (other
	// than 416) or 5xx. The host is up but is refusing this request —
	// usually a CDN bot screen, a paywall, or a server error.
	ReachabilityBlocked = "blocked"
	// ReachabilityUnreachable means the request errored at the network
	// layer — DNS failure, connection refused, TLS shutdown, timeout.
	ReachabilityUnreachable = "unreachable"
)
