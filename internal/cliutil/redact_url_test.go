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
