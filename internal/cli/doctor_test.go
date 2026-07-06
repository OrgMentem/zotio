// Copyright 2026 OrgMentem. Licensed under Apache-2.0. See LICENSE.
// PATCH: Cover local Zotero API detection.

package cli

import "testing"

func TestIsLocalZoteroAPI(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    bool
	}{
		{name: "localhost", baseURL: "http://localhost:23119/api/users/0", want: true},
		{name: "ipv4 loopback", baseURL: "http://127.0.0.1:23119/api/users/0", want: true},
		{name: "ipv6 loopback", baseURL: "http://[::1]:23119/api/users/0", want: true},
		{name: "web api", baseURL: "https://api.zotero.org/users/0", want: false},
		{name: "localhost suffix", baseURL: "http://localhost.example:23119/api/users/0", want: false},
		{name: "ipv4 suffix", baseURL: "http://127.0.0.1.example:23119/api/users/0", want: false},
		{name: "wrong port", baseURL: "http://localhost:1234/api/users/0", want: false},
		{name: "missing port", baseURL: "http://localhost/api/users/0", want: false},
		{name: "malformed", baseURL: "http://[::1/api/users/0", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLocalZoteroAPI(tt.baseURL); got != tt.want {
				t.Fatalf("isLocalZoteroAPI(%q): want %t, got %t", tt.baseURL, tt.want, got)
			}
		})
	}
}
