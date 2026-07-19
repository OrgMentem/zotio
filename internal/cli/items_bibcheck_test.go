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
	"strings"
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
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
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
	if report.Summary != (bibcheckSummary{Total: 3, OK: 1, Unknown: 1, Ambiguous: 1, Incomplete: 3}) {
		t.Fatalf("summary = %+v, want total=3 ok=1 unknown=1 ambiguous=1 incomplete=3", report.Summary)
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

func TestBibcheckCommandMultiFileJSONAggregatesFilesAndFindings(t *testing.T) {
	home := bibcheckIsolatedHome(t)
	seedBibcheckItems(t, []json.RawMessage{
		json.RawMessage(`{"key":"OK1","version":1,"data":{"key":"OK1","itemType":"journalArticle","title":"Known Alpha","creators":[{"lastName":"Doe"}],"date":"2024","publicationTitle":"Journal A","extra":"Citation Key: alpha"}}`),
		json.RawMessage(`{"key":"INC1","version":1,"data":{"key":"INC1","itemType":"journalArticle","title":"Incomplete Work","extra":"Citation Key: incomplete"}}`),
	})

	tex := filepath.Join(home, "a.tex")
	md := filepath.Join(home, "b.md")
	writeTestFile(t, tex, `\cite{alpha}`)
	writeTestFile(t, md, "Known @incomplete.\nMissing @absent.\n")

	report, _, err := runBibcheckJSON(t, tex, md)
	if err != nil {
		t.Fatalf("items bibcheck multi-file returned error: %v", err)
	}
	if report.Summary != (bibcheckSummary{Total: 3, OK: 2, Unknown: 1, Incomplete: 1}) {
		t.Fatalf("aggregate summary = %+v, want total=3 ok=2 unknown=1 incomplete=1", report.Summary)
	}
	if got := bibcheckCiteKeyOrder(report.Keys); !reflect.DeepEqual(got, []string{"alpha", "incomplete", "absent"}) {
		t.Fatalf("aggregate keys = %#v, want alpha/incomplete/absent", got)
	}
	if len(report.Files) != 2 {
		t.Fatalf("files len = %d, want 2: %+v", len(report.Files), report.Files)
	}
	files := map[string]bibcheckFileReport{}
	for _, file := range report.Files {
		files[file.File] = file
	}
	if got := files[tex]; got.Format != "tex" || got.Summary != (bibcheckSummary{Total: 1, OK: 1}) || !reflect.DeepEqual(bibcheckCiteKeyOrder(got.Keys), []string{"alpha"}) {
		t.Errorf("tex file report = %+v, want one ok alpha key", got)
	}
	if got := files[md]; got.Format != "pandoc-markdown" || got.Summary != (bibcheckSummary{Total: 2, OK: 1, Unknown: 1, Incomplete: 1}) || !reflect.DeepEqual(bibcheckCiteKeyOrder(got.Keys), []string{"incomplete", "absent"}) {
		t.Errorf("markdown file report = %+v, want incomplete and absent with incomplete finding counted", got)
	}

	undefined := bibcheckFindingByKind(t, report, "undefined_citekey")
	if undefined.Severity != sevHigh {
		t.Fatalf("undefined severity = %q, want %q", undefined.Severity, sevHigh)
	}
	if got := evidenceString(t, undefined, "citekey"); got != "absent" {
		t.Fatalf("undefined evidence citekey = %q, want absent", got)
	}
	if got := evidenceString(t, undefined, "file"); got != md {
		t.Fatalf("undefined evidence file = %q, want %q", got, md)
	}
	if got := evidenceNumber(t, undefined, "line"); got != 2 {
		t.Fatalf("undefined evidence line = %v, want 2", got)
	}
	if got := evidenceString(t, undefined, "file_line"); got != md+":2" {
		t.Fatalf("undefined evidence file_line = %q, want %q", got, md+":2")
	}
	if got := evidenceStrings(t, undefined, "file_lines"); !reflect.DeepEqual(got, []string{md + ":2"}) {
		t.Fatalf("undefined evidence file_lines = %#v, want b.md line 2", got)
	}

	incomplete := bibcheckFindingByKind(t, report, "incomplete_citation")
	if incomplete.Severity != sevHigh || incomplete.ItemKey != "INC1" {
		t.Fatalf("incomplete finding = %+v, want high severity for INC1", incomplete)
	}
	if got := evidenceString(t, incomplete, "citekey"); got != "incomplete" {
		t.Fatalf("incomplete evidence citekey = %q, want incomplete", got)
	}
	if got := evidenceStrings(t, incomplete, "missing_fields"); !reflect.DeepEqual(got, []string{"creators", "date", "publicationTitle"}) {
		t.Fatalf("incomplete missing_fields = %#v, want creators/date/publicationTitle", got)
	}
	if got := evidenceString(t, incomplete, "missing"); !strings.Contains(got, "creators") || !strings.Contains(got, "date") || !strings.Contains(got, "publicationTitle") {
		t.Fatalf("incomplete missing evidence = %q, want named citation-core fields", got)
	}
}

func TestBibcheckFailOnSeverityGate(t *testing.T) {
	home := bibcheckIsolatedHome(t)
	seedBibcheckItems(t, []json.RawMessage{
		json.RawMessage(`{"key":"OK1","version":1,"data":{"key":"OK1","itemType":"journalArticle","title":"Known Alpha","creators":[{"lastName":"Doe"}],"date":"2024","publicationTitle":"Journal A","extra":"Citation Key: alpha"}}`),
		json.RawMessage(`{"key":"INC1","version":1,"data":{"key":"INC1","itemType":"journalArticle","title":"Incomplete Work","extra":"Citation Key: incomplete"}}`),
	})
	tex := filepath.Join(home, "a.tex")
	md := filepath.Join(home, "b.md")
	writeTestFile(t, tex, `\cite{alpha}`)
	writeTestFile(t, md, "Known @incomplete.\nMissing @absent.\n")

	cases := []struct {
		name     string
		args     []string
		wantCode int
	}{
		{name: "omitted", args: []string{tex, md}, wantCode: 0},
		{name: "none", args: []string{tex, md, "--fail-on", "none"}, wantCode: 0},
		{name: "high", args: []string{tex, md, "--fail-on", "high"}, wantCode: 11},
		{name: "high-with-fail-on-unknown", args: []string{tex, md, "--fail-on", "high", "--fail-on-unknown"}, wantCode: 11},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report, _, err := runBibcheckJSON(t, tc.args...)
			if len(report.Findings) == 0 {
				t.Fatalf("fixture produced no findings")
			}
			if tc.wantCode == 0 {
				if err != nil {
					t.Fatalf("items bibcheck %v returned error %v, want nil", tc.args, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("items bibcheck %v returned nil error, want exit %d", tc.args, tc.wantCode)
			}
			if code := ExitCode(err); code != tc.wantCode {
				t.Fatalf("ExitCode(err) = %d, want %d (err=%v)", code, tc.wantCode, err)
			}
		})
	}
}

func TestBibcheckSingleFileJSONFieldsSurviveFindingsEnvelope(t *testing.T) {
	home := bibcheckIsolatedHome(t)
	seedBibcheckItems(t, []json.RawMessage{
		json.RawMessage(`{"key":"OK1","version":1,"data":{"key":"OK1","itemType":"journalArticle","title":"Known Alpha","creators":[{"lastName":"Doe"}],"date":"2024","publicationTitle":"Journal A","extra":"Citation Key: alpha"}}`),
	})
	manuscript := filepath.Join(home, "paper.tex")
	writeTestFile(t, manuscript, `\cite{alpha}`)

	report, raw, err := runBibcheckJSON(t, manuscript)
	if err != nil {
		t.Fatalf("items bibcheck single-file returned error: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode raw JSON envelope %q: %v", string(raw), err)
	}
	for _, field := range []string{"manuscript", "format", "summary", "keys", "findings"} {
		if _, ok := envelope[field]; !ok {
			t.Fatalf("single-file JSON missing %q in %s", field, string(raw))
		}
	}
	if report.Manuscript != manuscript {
		t.Fatalf("manuscript = %q, want %q", report.Manuscript, manuscript)
	}
	if report.Format != "tex" {
		t.Fatalf("format = %q, want tex", report.Format)
	}
	if report.Summary != (bibcheckSummary{Total: 1, OK: 1}) {
		t.Fatalf("summary = %+v, want one ok key", report.Summary)
	}
	if got := bibcheckCiteKeyOrder(report.Keys); !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Fatalf("keys = %#v, want alpha", got)
	}
	if report.Findings == nil {
		t.Fatalf("findings = nil, want additive empty array")
	}
}

func seedBibcheckItems(t *testing.T, items []json.RawMessage) {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		_ = db.Close()
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
}

func runBibcheckJSON(t *testing.T, args ...string) (bibcheckReport, []byte, error) {
	t.Helper()
	flags := &rootFlags{asJSON: true}
	cmd := newItemsCmd(flags)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs(append([]string{"bibcheck"}, args...))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})

	err := cmd.Execute()
	raw := append([]byte(nil), out.Bytes()...)
	var report bibcheckReport
	if decodeErr := json.Unmarshal(raw, &report); decodeErr != nil {
		t.Fatalf("decode bibcheck JSON %q: %v (command err=%v)", string(raw), decodeErr, err)
	}
	return report, raw, err
}

func bibcheckCiteKeyOrder(keys []bibcheckKeyResult) []string {
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key.CiteKey)
	}
	return out
}

func bibcheckFindingByKind(t *testing.T, report bibcheckReport, kind string) Finding {
	t.Helper()
	for _, finding := range report.Findings {
		if finding.Kind == kind {
			return finding
		}
	}
	t.Fatalf("finding kind %q not present in %+v", kind, report.Findings)
	return Finding{}
}

func evidenceString(t *testing.T, finding Finding, key string) string {
	t.Helper()
	value, ok := finding.Evidence[key].(string)
	if !ok {
		t.Fatalf("%s evidence %q = %#v, want string", finding.Kind, key, finding.Evidence[key])
	}
	return value
}

func evidenceNumber(t *testing.T, finding Finding, key string) float64 {
	t.Helper()
	value, ok := finding.Evidence[key].(float64)
	if !ok {
		t.Fatalf("%s evidence %q = %#v, want JSON number", finding.Kind, key, finding.Evidence[key])
	}
	return value
}

func evidenceStrings(t *testing.T, finding Finding, key string) []string {
	t.Helper()
	values, ok := finding.Evidence[key].([]any)
	if !ok {
		t.Fatalf("%s evidence %q = %#v, want JSON string array", finding.Kind, key, finding.Evidence[key])
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		str, ok := value.(string)
		if !ok {
			t.Fatalf("%s evidence %q contains %#v, want string", finding.Kind, key, value)
		}
		out = append(out, str)
	}
	return out
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
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
