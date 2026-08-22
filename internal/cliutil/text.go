// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cliutil

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// LooksLikeAuthError checks if an error message body contains auth-related keywords.
func LooksLikeAuthError(msg string) bool {
	lower := strings.ToLower(msg)
	patterns := []string{
		`\bkey\b`,
		`\btoken\b`,
		`\bunauthorized\b`,
		`\bapi_key\b`,
		`missing.{0,20}key`,
		`required.{0,20}key`,
		`\bforbidden\b`,
		`\bauthenticat`,
		`\bcredential`,
	}
	for _, p := range patterns {
		if matched, _ := regexp.MatchString(p, lower); matched {
			return true
		}
	}
	return false
}

// HTTPErrorKind classifies an API error by the HTTP status embedded in its
// message, so the CLI and MCP layers detect the same statuses in one place
// while each keeps its own user-facing hint text and result type.
// de-duplicates the brittle strings.Contains(msg,
// "HTTP NNN") ladder previously copy-pasted across internal/cli and internal/mcp.
type HTTPErrorKind int

const (
	HTTPErrOther HTTPErrorKind = iota
	HTTPErrConflict
	HTTPErrBadRequestAuth
	HTTPErrUnauthorized
	HTTPErrForbidden
	HTTPErrNotFound
	HTTPErrRateLimited
)

// ClassifyHTTPError maps an error message to an HTTPErrorKind by the HTTP
// status string it carries. Check order matches the historical CLI/MCP
// switches: 409, then 400-with-auth, 401, 403, 404, 429.
func ClassifyHTTPError(msg string) HTTPErrorKind {
	switch {
	case strings.Contains(msg, "HTTP 409"):
		return HTTPErrConflict
	case strings.Contains(msg, "HTTP 400") && LooksLikeAuthError(msg):
		return HTTPErrBadRequestAuth
	case strings.Contains(msg, "HTTP 401"):
		return HTTPErrUnauthorized
	case strings.Contains(msg, "HTTP 403"):
		return HTTPErrForbidden
	case strings.Contains(msg, "HTTP 404"):
		return HTTPErrNotFound
	case strings.Contains(msg, "HTTP 429"):
		return HTTPErrRateLimited
	default:
		return HTTPErrOther
	}
}

// credPatterns matches credential-shaped substrings in error bodies. Beyond
// the generic sk-/Bearer/key= shapes carried over from the generator template,
// it now recognizes this app's own Zotero-API-Key header scheme so a reflected
// or echoed key is redacted even though it carries no sk-/Bearer prefix.
// Match common API key forms and keep the regexp at package scope so it
// compiles once.
var credPatterns = regexp.MustCompile(`(?i)(sk-[a-zA-Z0-9]{8,}|sk_live_[a-zA-Z0-9]+|Bearer\s+[a-zA-Z0-9._\-]+|(?:zotero-api-)?key\s*[:=]\s*[a-zA-Z0-9._\-]+|zotero-api-key\s+[a-zA-Z0-9]{16,})`)

// SanitizeErrorBodyWithSecrets redacts credential-shaped substrings and any
// literal secret supplied (e.g. the configured Zotero API key), then truncates.
// Both redaction passes run before truncation: a credential straddling the
// length cap would otherwise be cut down to a prefix that no longer matches
// credPatterns, and that unmatched prefix is a real key fragment printed to
// the user's terminal and their logs.
// Secret-aware redaction lets the caller pass the live credential so reflected
// secrets are masked even when they do not match a generic token pattern.
func SanitizeErrorBodyWithSecrets(msg string, secrets ...string) string {
	for _, s := range secrets {
		if len(s) >= 8 {
			msg = strings.ReplaceAll(msg, s, "[REDACTED]")
		}
	}
	msg = credPatterns.ReplaceAllString(msg, "[REDACTED]")
	return truncateRunes(msg, 200)
}

// truncateRunes cuts msg to at most max bytes without splitting a rune, so a
// multi-byte character on the boundary cannot leave invalid UTF-8 in output
// that is about to be JSON-encoded. A non-positive max yields the marker
// alone: callers pass a constant today, but slicing on a negative bound would
// panic inside an error-formatting helper, which is the worst place for it.
func truncateRunes(msg string, max int) string {
	if max <= 0 {
		return "..."
	}
	if len(msg) <= max {
		return msg
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut] + "..."
}
