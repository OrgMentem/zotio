// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateKeyArg(t *testing.T) {
	cases := []struct {
		name      string
		kind      string
		value     string
		wantErr   bool
		wantHint  bool
		wantInMsg string
	}{
		{name: "valid item key", kind: "item", value: "ABCD2345"},
		{name: "valid collection key", kind: "collection", value: "ZYXW9876"},
		{name: "too short", kind: "item", value: "ABCD234", wantErr: true},
		{name: "too long", kind: "item", value: "ABCD23456", wantErr: true, wantHint: true},
		{name: "forbidden letter O", kind: "item", value: "ABCDOOOO", wantErr: true},
		{name: "forbidden digit 1", kind: "search", value: "ABCD1234", wantErr: true, wantInMsg: `invalid Zotero search key "ABCD1234"`},
		{name: "citekey", kind: "item", value: "smith2020deep", wantErr: true, wantHint: true},
		{name: "lowercase 8 chars", kind: "item", value: "abcd2345", wantErr: true, wantHint: true},
		{name: "empty", kind: "collection", value: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateKeyArg(tc.kind, tc.value)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("validateKeyArg(%q) = %v, want nil", tc.value, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateKeyArg(%q) = nil, want a usage error", tc.value)
			}
			if got := ExitCode(err); got != 2 {
				t.Fatalf("ExitCode = %d, want 2", got)
			}
			want := "invalid Zotero " + tc.kind + " key"
			if tc.wantInMsg != "" {
				want = tc.wantInMsg
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %q, want %q", err, want)
			}
			hint := "zotio items find --citekey " + tc.value
			if got := strings.Contains(err.Error(), hint); got != tc.wantHint {
				t.Fatalf("citekey hint present = %v, want %v in %q", got, tc.wantHint, err)
			}
		})
	}
}

func TestPositionalArgsAreValidatedFromUse(t *testing.T) {
	// Not parallel: RootCmd() binds package-level globals.
	root := RootCmd()
	cases := []struct {
		name     string
		args     []string
		wantCode int // 0 = Args accepts
	}{
		{"item key ok", []string{"items", "get", "ABCD2345"}, 0},
		{"item key bad", []string{"items", "get", "smith2020"}, 2},
		{"collection key bad", []string{"collections", "get", "nope"}, 2},
		{"collection key ok", []string{"collections", "get", "ABCD2345"}, 0},
		{"search key bad", []string{"searches", "get", "nope"}, 2},
		{"variadic item keys bad second", []string{"reading-list", "add", "ABCD2345", "bad"}, 2},
		{"tag name is not a key", []string{"tags", "get", "my tag"}, 0},
		{"stray arg on a leaf", []string{"version", "stray"}, 2},
		{"stray word on a runnable parent", []string{"tags", "audit", "fx"}, 2},
		{"vault resolve takes a citekey", []string{"vault", "resolve", "smith2020", "--keep-vault"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, rest, err := root.Find(tc.args)
			if err != nil {
				t.Fatalf("Find(%q): %v", tc.args, err)
			}
			if err := cmd.ParseFlags(rest); err != nil {
				t.Fatalf("ParseFlags: %v", err)
			}
			args := cmd.Flags().Args()
			if cmd.Args == nil {
				t.Fatalf("%s has no Args validator", cmd.CommandPath())
			}
			err = cmd.Args(cmd, args)
			if got := 0; err != nil {
				got = ExitCode(err)
				if got != tc.wantCode {
					t.Fatalf("exit %d (%v), want %d", got, err, tc.wantCode)
				}
				return
			}
			if tc.wantCode != 0 {
				t.Fatalf("Args accepted %q, want exit %d", tc.args, tc.wantCode)
			}
		})
	}
}

func TestExplicitMissingConfigFlagExits10WithoutHTTP(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Error(w, "no request expected", http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL)
	typo := filepath.Join(t.TempDir(), "typo.toml")
	_, _, err := executeRootForDeliverTest(t, "--config", typo, "items", "list")
	if code := ExitCode(err); code != 10 {
		t.Fatalf("exit %d (%v), want 10 for a missing explicit --config", code, err)
	}
	if hits != 0 {
		t.Fatalf("made %d HTTP request(s), want none", hits)
	}
}
