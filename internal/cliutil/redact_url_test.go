// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cliutil

import (
	"strings"
	"testing"
)

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "userinfo and query token",
			raw:  "https://u:sekret@example.com/api?token=abc&x=1",
			want: "https://***@example.com/api?token=***&x=1",
		},
		{
			name: "plain local URL",
			raw:  "http://localhost:23119/api/users/0",
			want: "http://localhost:23119/api/users/0",
		},
		{
			name: "bare userinfo",
			raw:  "https://sekret@api.zotero.org/users/123",
			want: "https://***@api.zotero.org/users/123",
		},
		{
			// `key` is the parameter Zotero's own Web API accepts for the API key.
			name: "zotero key parameter",
			raw:  "https://api.zotero.org/users/1?key=SECRET&limit=5",
			want: "https://api.zotero.org/users/1?key=***&limit=5",
		},
		{
			name: "mixed-case key parameter",
			raw:  "https://api.zotero.org/users/1?Key=SECRET",
			want: "https://api.zotero.org/users/1?Key=***",
		},
		{
			name: "mixed-case api_key parameter",
			raw:  "https://api.zotero.org/users/1?limit=5&API_KEY=SECRET",
			want: "https://api.zotero.org/users/1?limit=5&API_KEY=***",
		},
		{
			name: "percent-encoded key parameter",
			raw:  "https://api.zotero.org/users/1?%6Bey=SECRET",
			want: "https://api.zotero.org/users/1?%6Bey=***",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactURL(tt.raw); got != tt.want {
				t.Fatalf("RedactURL(%q): want %q, got %q", tt.raw, tt.want, got)
			}
		})
	}
}

func TestRedactURLFailsClosedOnUnparseableInput(t *testing.T) {
	const raw = "http://[::1/api/users/0?token=abc"
	got := RedactURL(raw)
	if got != unparseableBaseURLPlaceholder {
		t.Fatalf("RedactURL(%q): want fixed placeholder %q, got %q", raw, unparseableBaseURLPlaceholder, got)
	}
	if strings.Contains(got, "token=abc") || strings.Contains(got, raw) {
		t.Fatalf("RedactURL(%q) leaked the unparseable input: %q", raw, got)
	}
}
