// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestRmsManuscriptIncludesUseSourceOrder(t *testing.T) {
	for _, content := range []string{
		"\\input{chapter}\n\\cite{alpha}\n",
		"% comment before include\n\\input{chapter} \\cite{alpha}\n",
		"\\begin{verbatim}example\\end{verbatim} \\input{chapter} \\cite{alpha}\n",
	} {
		t.Run(content, func(t *testing.T) {
			isolateCSLTestEnv(t)
			mbSeedCiteKeys(t, map[string]string{"ITEMA": "alpha", "ITEMB": "beta"})
			api := mbStartBibServer(t)
			dir := t.TempDir()
			root := filepath.Join(dir, "root.tex")
			writeTestFile(t, root, content)
			writeTestFile(t, filepath.Join(dir, "chapter.tex"), "\\cite{beta}\n")
			out, err := runCSLItemsCommand(t, &rootFlags{noCache: true}, []string{
				"bibliography", "--manuscript", root, "--follow-includes",
			})
			if err != nil {
				t.Fatalf("bibliography: %v", err)
			}
			if got := out.String(); got != "chunk1:ITEMB,ITEMA" {
				t.Fatalf("bibliography = %q, want beta before alpha", got)
			}
			if got := api.snapshot(); !reflect.DeepEqual(got, [][]string{{"ITEMB", "ITEMA"}}) {
				t.Fatalf("selection = %v, want beta before alpha", got)
			}
		})
	}
}

func TestRmsManuscriptIncludeOrderRetainsCitationLocations(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root.tex")
	chapter := filepath.Join(dir, "chapter.tex")
	writeTestFile(t, root, "\\cite{before}\n\\input{chapter}\n\n\\cite{after}\n")
	writeTestFile(t, chapter, "\n\\cite{middle}\n")
	got, findings, err := parseManuscriptFiles([]string{root}, true)
	if err != nil {
		t.Fatalf("parse manuscript: %v", err)
	}
	want := []bibcheckOccurrence{
		{CiteKey: "before", File: root, Line: 1, Format: "tex"},
		{CiteKey: "middle", File: chapter, Line: 2, Format: "tex"},
		{CiteKey: "after", File: root, Line: 4, Format: "tex"},
	}
	if !reflect.DeepEqual(got, want) || len(findings) != 0 {
		t.Fatalf("occurrences = %+v, findings = %+v; want %+v and no findings", got, findings, want)
	}
}

func TestRmsBibcheckKeepsRootFirstReportOrder(t *testing.T) {
	home := bibcheckIncludeFixture(t, map[string]string{
		"root.tex":    "\\input{chapter}\n\\cite{alpha}\n",
		"chapter.tex": "\\cite{beta}\n",
	})
	report, _, err := runBibcheckJSON(t, filepath.Join(home, "root.tex"), "--follow-includes")
	if err != nil {
		t.Fatalf("bibcheck: %v", err)
	}
	var got []string
	for _, key := range report.Keys {
		got = append(got, key.CiteKey)
	}
	if !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Fatalf("bibcheck key order = %v, want root first", got)
	}
}

func TestRmsManuscriptExportsReorderAttributedRecordsAcrossChunks(t *testing.T) {
	for _, format := range []string{"bibtex", "biblatex", "ris"} {
		t.Run(format, func(t *testing.T) {
			isolateCSLTestEnv(t)
			citeKeys := make(map[string]string, 51)
			var manuscript, want strings.Builder
			for i := 51; i >= 1; i-- {
				key := fmt.Sprintf("K%03d", i)
				citeKeys[key] = fmt.Sprintf("ref%03d", i)
				fmt.Fprintf(&manuscript, "@ref%03d\n", i)
				want.WriteString(rmsExportRecord(format, key))
			}
			mbSeedCiteKeys(t, citeKeys)
			var chunks []int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				keys := splitNonEmpty(q.Get("itemKey"), ",")
				chunks = append(chunks, len(keys))
				// This deliberately returns key order, not manuscript order.
				slices.Sort(keys)
				if q.Get("format") != "json" || q.Get("include") != format {
					var raw strings.Builder
					for _, key := range keys {
						raw.WriteString(rmsExportRecord(format, key))
					}
					_, _ = w.Write([]byte(raw.String()))
					return
				}
				if q.Get("limit") != fmt.Sprint(len(keys)) {
					t.Errorf("limit = %q, want %d", q.Get("limit"), len(keys))
				}
				rows := make([]map[string]string, 0, len(keys))
				for _, key := range keys {
					rows = append(rows, map[string]string{"key": key, format: rmsExportRecord(format, key)})
				}
				_ = json.NewEncoder(w).Encode(rows)
			}))
			defer srv.Close()
			t.Setenv("ZOTERO_API_KEY", "testkey")
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
			path := filepath.Join(t.TempDir(), "paper.md")
			writeTestFile(t, path, manuscript.String())
			out, err := runCSLItemsCommand(t, &rootFlags{noCache: true}, []string{
				"bibliography", "--manuscript", path, "--format", format,
			})
			if err != nil {
				t.Fatalf("bibliography: %v", err)
			}
			if got := out.String(); got != want.String() {
				t.Fatalf("export = %q, want manuscript order %q", got, want.String())
			}
			if !slices.Equal(chunks, []int{50, 1}) {
				t.Fatalf("chunks = %v, want 50 then 1", chunks)
			}
		})
	}
}

func rmsExportRecord(format, key string) string {
	if format == "ris" {
		return "TY  - JOUR\nID  - " + key + "\nER  - \n"
	}
	return "@article{" + key + ",\n  title = {" + key + "}\n}\n"
}

func TestRmsManuscriptCapabilityRequiresMirrorOnlyForManuscript(t *testing.T) {
	var entry capabilityEntry
	for _, candidate := range buildCapabilityRegistry(RootCmd()) {
		if candidate.Path == "items bibliography" {
			entry = candidate
			break
		}
	}
	if len(entry.Requires) != 0 {
		t.Fatalf("unconditional requirements = %v, want unchanged mirror-free scope", entry.Requires)
	}
	if len(entry.Routes) != 2 || entry.Routes[0].Via != "default" || len(entry.Routes[0].Requires) != 0 ||
		entry.Routes[1].Via != "manuscript" || !slices.Equal(entry.Routes[1].Requires, []string{preconditionSyncedStore}) {
		t.Fatalf("routes = %+v, want mirror-free default and manuscript synced_store", entry.Routes)
	}
	isolateCSLTestEnv(t)
	mbStartBibServer(t)
	path := filepath.Join(t.TempDir(), "paper.md")
	writeTestFile(t, path, "@alpha\n")
	out, err := runCSLItemsCommand(t, &rootFlags{asJSON: true, noCache: true}, []string{"bibliography", "--manuscript", path})
	if ExitCode(err) != 9 {
		t.Fatalf("missing mirror exit = %d (%v), want 9", ExitCode(err), err)
	}
	var envelope preconditionUnmetEnvelope
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("decode precondition: %v", err)
	}
	if envelope.Kind != "precondition_unmet" || envelope.Precondition != preconditionSyncedStore {
		t.Fatalf("precondition = %+v, want synced_store", envelope)
	}
}

func TestRmsManuscriptExportRefusesUnattributableRecords(t *testing.T) {
	cases := []struct {
		name, response, want string
	}{
		{"omitted", `[{"key":"ITEMA","bibtex":"alpha"}]`, `omitted scoped item "ITEMB"`},
		{"duplicate", `[{"key":"ITEMA","bibtex":"alpha"},{"key":"ITEMA","bibtex":"alpha"}]`, `item "ITEMA" more than once`},
		{"unexpected", `[{"key":"OTHER","bibtex":"other"}]`, `unexpected item "OTHER"`},
		{"missing export", `[{"key":"ITEMA"}]`, `decoding bibtex export for item "ITEMA"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateCSLTestEnv(t)
			mbSeedCiteKeys(t, map[string]string{"ITEMA": "alpha", "ITEMB": "beta"})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.response))
			}))
			defer srv.Close()
			t.Setenv("ZOTERO_API_KEY", "testkey")
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
			path := filepath.Join(t.TempDir(), "paper.md")
			writeTestFile(t, path, "@beta @alpha\n")
			out, err := runCSLItemsCommand(t, &rootFlags{noCache: true}, []string{
				"bibliography", "--manuscript", path, "--format", "bibtex",
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Fatalf("partial bibliography = %q, want no output", out.String())
			}
		})
	}
}
