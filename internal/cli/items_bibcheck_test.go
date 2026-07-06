// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"zotio/internal/store"
)

func TestBibcheckParseLatexCiteKeys(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name: "commands-starred-options-and-comma-lists",
			content: `
Intro \cite{alpha, beta}.
Starred text cite \citet*{gamma} and parenthetical \citep[see][ch. 2]{delta,epsilon}.
Modern commands \parencite[12]{zeta} \textcite{eta} \autocite{theta}.
Wildcard-only nocite is ignored: \nocite{*}; real nocite stays: \nocite{iota}.
`,
			want: []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta", "iota"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseLatexCiteKeys(tc.content); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseLatexCiteKeys() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestBibcheckParsePandocMarkdownCiteKeys(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name: "citations-outside-inline-and-fenced-code",
			content: "" +
				"Narrative @alpha and bracketed [see @beta; -@gamma].\n" +
				"Inline code `@inline` must not count, but @delta. does.\n" +
				"```\n@fenced\n```\n" +
				"~~~{.r}\n@tilde\n~~~\n" +
				"Escaped \\@escaped must not count.\n",
			want: []string{"alpha", "beta", "gamma", "delta"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parsePandocMarkdownCiteKeys(tc.content); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parsePandocMarkdownCiteKeys() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestBibcheckUnsupportedExtensionIsUsageError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paper.rst")
	writeTestFile(t, path, "@alpha")
	_, _, err := parseManuscriptCiteKeys(path)
	if err == nil {
		t.Fatal("parseManuscriptCiteKeys(.rst) returned nil error, want usage error")
	}
	var cliErr *cliError
	if !errors.As(err, &cliErr) || cliErr.code != 2 {
		t.Fatalf("parseManuscriptCiteKeys(.rst) error = %T %[1]v, want usage error code 2", err)
	}
}

func TestBibcheckCommandResolvesStatusesAndFailGate(t *testing.T) {
	home := bibcheckIsolatedHome(t)
	db, err := store.OpenWithContext(context.Background(), defaultDBPath("zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	items := []json.RawMessage{
		json.RawMessage(`{"key":"OK1","version":1,"data":{"key":"OK1","itemType":"journalArticle","title":"Known Alpha","extra":"Citation Key: alpha"}}`),
		json.RawMessage(`{"key":"DUP1","version":1,"data":{"key":"DUP1","itemType":"journalArticle","title":"Duplicate One","extra":"Citation Key: dup"}}`),
		json.RawMessage(`{"key":"DUP2","version":1,"data":{"key":"DUP2","itemType":"book","title":"Duplicate Two","extra":"Citation Key: dup"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		_ = db.Close()
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	manuscript := filepath.Join(home, "paper.tex")
	writeTestFile(t, manuscript, `\cite{alpha, missing, alpha}\n\parencite{dup}`)

	flags := &rootFlags{asJSON: true}
	cmd := newItemsCmd(flags)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"bibcheck", manuscript, "--fail-on-unknown"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})

	err = cmd.Execute()
	if err == nil {
		t.Fatal("items bibcheck --fail-on-unknown returned nil error, want gate error")
	}
	var cliErr *cliError
	if !errors.As(err, &cliErr) || cliErr.code != 11 {
		t.Fatalf("items bibcheck error = %T %[1]v, want cli gate error code 11", err)
	}
	if code := ExitCode(err); code != 11 {
		t.Fatalf("ExitCode(err) = %d, want 11", code)
	}

	var report bibcheckReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode bibcheck JSON %q: %v", out.String(), err)
	}
	if report.Format != "tex" {
		t.Fatalf("format = %q, want tex", report.Format)
	}
	if report.Summary != (bibcheckSummary{Total: 3, OK: 1, Unknown: 1, Ambiguous: 1}) {
		t.Fatalf("summary = %+v, want total=3 ok=1 unknown=1 ambiguous=1", report.Summary)
	}
	byKey := map[string]bibcheckKeyResult{}
	for _, result := range report.Keys {
		byKey[result.CiteKey] = result
	}
	if got := byKey["alpha"]; got.Status != "ok" || got.Occurrences != 2 || got.ItemKey != "OK1" || got.Title != "Known Alpha" {
		t.Errorf("alpha result = %+v, want ok with two occurrences and OK1 metadata", got)
	}
	if got := byKey["missing"]; got.Status != "unknown" || got.Occurrences != 1 || got.ItemKey != "" || len(got.Matches) != 0 {
		t.Errorf("missing result = %+v, want unknown without item metadata", got)
	}
	if got := byKey["dup"]; got.Status != "ambiguous" || got.Occurrences != 1 || len(got.Matches) != 2 || got.Matches[0].ItemKey != "DUP1" || got.Matches[1].ItemKey != "DUP2" {
		t.Errorf("dup result = %+v, want ambiguous matches DUP1,DUP2", got)
	}
}

func bibcheckIsolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	savedGroup := activeGroupID
	activeGroupID = ""
	t.Cleanup(func() { activeGroupID = savedGroup })
	return home
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
