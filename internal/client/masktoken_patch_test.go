// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(glean security-audit): regression test for maskToken. Short tokens must
// be fully masked rather than revealing their last 4 characters (which for a
// short token is most of the secret).

package client

import "testing"

func TestMaskToken(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"abc", "****"},
		{"short", "****"},            // 5 chars: < 12, fully masked
		{"elevenchars", "****"},      // 11 chars: < 12, fully masked
		{"abcdefghijkl", "****ijkl"}, // 12 chars: reveal last 4
		{"zotero-api-key-1234567890", "****7890"},
	}
	for _, c := range cases {
		if got := maskToken(c.in); got != c.want {
			t.Errorf("maskToken(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
