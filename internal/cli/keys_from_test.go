// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
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
			args: []string{"KEYAAAA2", "KEYAAAA3"},
			want: []string{"KEYAAAA2", "KEYAAAA3"},
		},
		{
			name:        "line file",
			fileContent: ptr("KEYAAAA2\nKEYAAAA3\n"),
			want:        []string{"KEYAAAA2", "KEYAAAA3"},
		},
		{
			name:     "stdin lines",
			keysFrom: "-",
			stdin:    strings.NewReader("KEYAAAA2\nKEYAAAA3\n"),
			want:     []string{"KEYAAAA2", "KEYAAAA3"},
		},
		{
			name:        "json string array",
			fileContent: ptr(`["KEYAAAA2","KEYAAAA3"]`),
			want:        []string{"KEYAAAA2", "KEYAAAA3"},
		},
		{
			name:        "json object array",
			fileContent: ptr(`[{"key":"KEYAAAA2"},{"key":"KEYAAAA3"}]`),
			want:        []string{"KEYAAAA2", "KEYAAAA3"},
		},
		{
			name: "dedupe order preserved",
			args: []string{"KEYAAAA2", "KEYAAAA3", "KEYAAAA2", "KEYAAAA4", "KEYAAAA3"},
			want: []string{"KEYAAAA2", "KEYAAAA3", "KEYAAAA4"},
		},
		{
			name:        "blank lines skipped",
			fileContent: ptr("\n  KEYAAAA2  \n\nKEYAAAA3\n  \n"),
			want:        []string{"KEYAAAA2", "KEYAAAA3"},
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
			args:     []string{"KEYAAAA2"},
			keysFrom: "-",
			stdin:    strings.NewReader("KEYAAAA3\n"),
			wantErr:  "--keys-from cannot be combined with positional keys",
		},
		{
			name:        "malformed json error",
			fileContent: ptr(`["KEYAAAA2",`),
			wantErr:     "malformed keys JSON:",
		},
		{
			name:    "bad positional key is a usage error",
			args:    []string{"KEYAAAA2", "notakey"},
			wantErr: `invalid Zotero item key "notakey"`,
		},
		{
			name:        "bad line names its line",
			fileContent: ptr("KEYAAAA2\nKEY0O1AB\n"),
			wantErr:     `--keys-from line 2: invalid Zotero item key "KEY0O1AB"`,
		},
		{
			name:        "bad json key is a usage error",
			fileContent: ptr(`[{"key":"KEYAAAA2"},{"key":"short"}]`),
			wantErr:     `invalid Zotero item key "short"`,
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
				if ExitCode(err) != 2 {
					t.Fatalf("ExitCode(resolveKeys error) = %d, want 2 (usage); err = %v", ExitCode(err), err)
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

func TestResolveKeysFromJSONContracts(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    []string
		wantErr string
	}{
		{
			name:  "findings envelope preserves first seen keys",
			input: `{"findings":[{"item_key":"KEYAAAA2"},{"item_key":"KEYAAAA3"},{"item_key":"KEYAAAA2"}]}`,
			want:  []string{"KEYAAAA2", "KEYAAAA3"},
		},
		{
			name:  "findings envelope skips grouped findings without keys",
			input: `{"findings":[{"kind":"library_warning"},{"item_key":""},{"item_key":"KEYAAAA2"},{"key":"KEYAAAA3"},{"key":"  "}]}`,
			want:  []string{"KEYAAAA2", "KEYAAAA3"},
		},
		{
			name:  "items envelope reads item keys",
			input: `{"items":[{"key":"KEYAAAA2"},{"key":"KEYAAAA3"}]}`,
			want:  []string{"KEYAAAA2", "KEYAAAA3"},
		},
		{
			name:  "bare item key object array",
			input: `[{"item_key":"KEYAAAA2"},{"item_key":"KEYAAAA3"}]`,
			want:  []string{"KEYAAAA2", "KEYAAAA3"},
		},
		{
			name:  "legacy line list",
			input: "KEYAAAA2\nKEYAAAA3\n",
			want:  []string{"KEYAAAA2", "KEYAAAA3"},
		},
		{
			name:  "legacy string array",
			input: `["KEYAAAA2","KEYAAAA3"]`,
			want:  []string{"KEYAAAA2", "KEYAAAA3"},
		},
		{
			name:  "legacy key object array",
			input: `[{"key":"KEYAAAA2"},{"key":"KEYAAAA3"}]`,
			want:  []string{"KEYAAAA2", "KEYAAAA3"},
		},
		{
			name:    "malformed top level object fails loudly",
			input:   `{"notes":[{"key":"KEYAAAA2"}]}`,
			wantErr: "malformed keys JSON: object must contain a findings or items array",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveKeys(nil, "-", strings.NewReader(tt.input))
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

func TestResolveKeysFromFindingsReportPipe(t *testing.T) {
	report := FindingsReport{
		Findings: []Finding{
			{
				Kind:        "missing_citation",
				Severity:    "high",
				ItemKey:     "KEYAAAA2",
				Source:      FindingSource{Kind: "local"},
				Autofixable: true,
			},
			{
				Kind:     "grouped_library_warning",
				Severity: "medium",
				Evidence: map[string]any{"count": 2},
				Source:   FindingSource{Kind: "local"},
			},
			{
				Kind:     "preprint_published",
				Severity: "medium",
				ItemKey:  "KEYAAAA3",
				Source:   FindingSource{Kind: "web_api"},
			},
		},
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal FindingsReport: %v", err)
	}
	var decoded FindingsReport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal FindingsReport: %v", err)
	}

	got, err := resolveKeys(nil, "-", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("resolveKeys error = %v", err)
	}
	want := []string{"KEYAAAA2", "KEYAAAA3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveKeys = %#v, want %#v", got, want)
	}
}
