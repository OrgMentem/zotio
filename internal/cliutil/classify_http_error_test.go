// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// ClassifyHTTPError is the single source of HTTP
// status detection shared by the CLI and MCP layers; pin its mapping and the
// 409 > 400-auth > 401 > 403 > 404 > 429 precedence.

package cliutil

import "testing"

func TestClassifyHTTPError(t *testing.T) {
	cases := []struct {
		msg  string
		want HTTPErrorKind
	}{
		{"fetching items: HTTP 409: conflict", HTTPErrConflict},
		{"HTTP 400: missing api key", HTTPErrBadRequestAuth},
		{"HTTP 400: malformed cursor", HTTPErrOther}, // 400 without auth signal
		{"HTTP 401: unauthorized", HTTPErrUnauthorized},
		{"HTTP 403: forbidden by resource ACL", HTTPErrForbidden},
		{"HTTP 404: not found", HTTPErrNotFound},
		{"HTTP 429: slow down", HTTPErrRateLimited},
		{"connection refused", HTTPErrOther},
		{"", HTTPErrOther},
	}
	for _, c := range cases {
		if got := ClassifyHTTPError(c.msg); got != c.want {
			t.Errorf("ClassifyHTTPError(%q) = %d, want %d", c.msg, got, c.want)
		}
	}
}
