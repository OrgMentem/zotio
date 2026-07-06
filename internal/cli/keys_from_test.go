// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(glean write-safety): cover shared --keys-from item-key selection.

package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolveKeys(t *testing.T) {
	ptr := func(s string) *string { return &s }

	cases := []struct {
		name        string
		args        []string
		keysFrom    string
		stdin       *strings.Reader
		fileContent *string
		want        []string
		wantErr     string
	}{
		{
			name: "positional args",
			args: []string{"K1", "K2"},
			want: []string{"K1", "K2"},
		},
		{
			name:        "line file",
			fileContent: ptr("K1\nK2\n"),
			want:        []string{"K1", "K2"},
		},
		{
			name:     "stdin lines",
			keysFrom: "-",
			stdin:    strings.NewReader("K1\nK2\n"),
			want:     []string{"K1", "K2"},
		},
		{
			name:        "json string array",
			fileContent: ptr(`["K1","K2"]`),
			want:        []string{"K1", "K2"},
		},
		{
			name:        "json object array",
			fileContent: ptr(`[{"key":"K1"},{"key":"K2"}]`),
			want:        []string{"K1", "K2"},
		},
		{
			name: "dedupe order preserved",
			args: []string{"K1", "K2", "K1", "K3", "K2"},
			want: []string{"K1", "K2", "K3"},
		},
		{
			name:        "blank lines skipped",
			fileContent: ptr("\n  K1  \n\nK2\n  \n"),
			want:        []string{"K1", "K2"},
		},
		{
			name:    "empty positional args error",
			wantErr: "no item keys provided",
		},
		{
			name:        "empty input errors",
			fileContent: ptr("\n\n  \n"),
			wantErr:     "no item keys provided",
		},
		{
			name:     "both sources error",
			args:     []string{"K1"},
			keysFrom: "-",
			stdin:    strings.NewReader("K2\n"),
			wantErr:  "--keys-from cannot be combined with positional keys",
		},
		{
			name:        "malformed json error",
			fileContent: ptr(`["K1",`),
			wantErr:     "malformed keys JSON:",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			keysFrom := tt.keysFrom
			if tt.fileContent != nil {
				path := filepath.Join(t.TempDir(), "keys.txt")
				if err := os.WriteFile(path, []byte(*tt.fileContent), 0o600); err != nil {
					t.Fatalf("write keys file: %v", err)
				}
				keysFrom = path
			}

			var stdin strings.Reader
			stdinPtr := &stdin
			if tt.stdin != nil {
				stdinPtr = tt.stdin
			}

			got, err := resolveKeys(tt.args, keysFrom, stdinPtr)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("resolveKeys error = nil, want %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolveKeys error = %q, want containing %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveKeys error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("resolveKeys = %#v, want %#v", got, tt.want)
			}
		})
	}
}
