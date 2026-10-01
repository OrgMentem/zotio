// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cliutil

import (
	"net/url"
	"strings"
)

const unparseableBaseURLPlaceholder = "<unparseable base URL redacted>"

// RedactURL masks the userinfo and sensitive query values of a URL so it can
// be printed. Input that does not parse is replaced by a fixed placeholder,
// because nothing in it can be safely located and masked.
func RedactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return unparseableBaseURLPlaceholder
	}
	redacted := false
	redactedUserinfo := false
	if parsed.User != nil && parsed.User.String() != "" {
		parsed.User = url.User("***")
		redacted = true
		redactedUserinfo = true
	}
	if parsed.RawQuery != "" {
		if rawQuery, ok := redactSensitiveURLQuery(parsed.RawQuery); ok {
			parsed.RawQuery = rawQuery
			redacted = true
		}
	}
	if !redacted {
		return raw
	}
	out := parsed.String()
	if redactedUserinfo {
		out = strings.Replace(out, "%2A%2A%2A@", "***@", 1)
	}
	return out
}

func redactSensitiveURLQuery(rawQuery string) (string, bool) {
	parts := strings.Split(rawQuery, "&")
	redacted := false
	for i, part := range parts {
		key := part
		if eq := strings.IndexByte(part, '='); eq >= 0 {
			key = part[:eq]
		}
		decodedKey, err := url.QueryUnescape(key)
		if err != nil {
			decodedKey = key
		}
		if sensitiveURLQueryKey(decodedKey) {
			parts[i] = key + "=***"
			redacted = true
		}
	}
	if !redacted {
		return rawQuery, false
	}
	return strings.Join(parts, "&"), true
}

func sensitiveURLQueryKey(key string) bool {
	switch strings.ToLower(key) {
	case "token", "key", "api_key", "apikey", "secret", "password", "auth":
		return true
	default:
		return false
	}
}
