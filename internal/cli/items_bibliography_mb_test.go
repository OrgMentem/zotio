// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// mbBibServer is a fake Web API that echoes each bib request's itemKey list
// and records every request it receives.
type mbBibServer struct {
	mu       sync.Mutex
	requests [][]string
}

func mbStartBibServer(t *testing.T) *mbBibServer {
	t.Helper()
	s := &mbBibServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		chunk := splitNonEmpty(q.Get("itemKey"), ",")
		s.mu.Lock()
		s.requests = append(s.requests, chunk)
		n := len(s.requests)
		s.mu.Unlock()
		if q.Get("format") != "bib" {
			http.Error(w, "unexpected format", http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprintf(w, "chunk%d:%s", n, q.Get("itemKey")) //nolint:gosec // G705: httptest double echoing request params back to the test client; no browser sink.
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_API_KEY", "testkey")
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	return s
}

func (s *mbBibServer) snapshot() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.requests...)
}

func mbSeedCiteKeys(t *testing.T, citeKeys map[string]string) {
	t.Helper()
	items := make([]json.RawMessage, 0, len(citeKeys))
	for key, citeKey := range citeKeys {
		items = append(items, json.RawMessage(fmt.Sprintf(
			`{"key":%q,"version":1,"data":{"key":%q,"itemType":"journalArticle","title":"Title %s","extra":"Citation Key: %s"}}`,
			key, key, key, citeKey)))
	}
	cliTestSeedItemsStore(t, items)
}

func TestMbManuscriptExportsOnlyCitedItemsInFirstCitationOrder(t *testing.T) {
	isolateCSLTestEnv(t)
	mbSeedCiteKeys(t, map[string]string{"ITEMA": "alpha", "ITEMB": "beta", "ITEMC": "gamma"})
	api := mbStartBibServer(t)

	dir := t.TempDir()
	intro := filepath.Join(dir, "intro.md")
	methods := filepath.Join(dir, "methods.tex")
	writeTestFile(t, intro, "As shown [@gamma; @alpha], and again @gamma.\n")
	writeTestFile(t, methods, "\\cite{alpha}\n\\parencite{gamma, alpha}\n")

	out, err := runCSLItemsCommand(t, &rootFlags{noCache: true}, []string{"bibliography", "--manuscript", intro, "--manuscript", methods})
	if err != nil {
		t.Fatalf("items bibliography --manuscript: %v", err)
	}
	if got, want := api.snapshot(), [][]string{{"ITEMC", "ITEMA"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bib requests = %v, want %v (cited items once, first-citation order, uncited ITEMB absent)", got, want)
	}
	if got := out.String(); got != "chunk1:ITEMC,ITEMA" {
		t.Fatalf("output = %q, want rendered bibliography for ITEMC,ITEMA", got)
	}
}

func TestMbManuscriptRefusesUnresolvedCitations(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name:    "unknown",
			content: "Intro.\n\\cite{alpha, missing}\n",
			want:    []string{"paper.tex:2", `unknown citation key "missing"`},
		},
		{
			name:    "ambiguous",
			content: "Intro.\n\nSee \\cite{alpha}.\n\\textcite{dup}\n",
			want:    []string{"paper.tex:4", `ambiguous citation key "dup"`, "DUP1", "DUP2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateCSLTestEnv(t)
			mbSeedCiteKeys(t, map[string]string{"ITEMA": "alpha", "DUP1": "dup", "DUP2": "dup"})
			api := mbStartBibServer(t)
			manuscript := filepath.Join(t.TempDir(), "paper.tex")
			writeTestFile(t, manuscript, tc.content)

			for _, asJSON := range []bool{false, true} {
				out, err := runCSLItemsCommand(t, &rootFlags{noCache: true, asJSON: asJSON}, []string{"bibliography", "--manuscript", manuscript, "--format", "bibtex"})
				if code := ExitCode(err); code != 11 {
					t.Fatalf("json=%v ExitCode = %d (err=%v), want 11", asJSON, code, err)
				}
				for _, want := range tc.want {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("json=%v error %q missing %q", asJSON, err.Error(), want)
					}
				}
				if out.Len() != 0 {
					t.Fatalf("json=%v output = %q, want no partial bibliography", asJSON, out.String())
				}
			}
			if got := api.snapshot(); len(got) != 0 {
				t.Fatalf("API requests = %v, want none before citations resolve", got)
			}
		})
	}
}

func TestMbManuscriptChunksFiftyOneItemsAcrossTwoBatches(t *testing.T) {
	isolateCSLTestEnv(t)
	citeKeys := make(map[string]string, 51)
	wantKeys := make([]string, 0, 51)
	var tex strings.Builder
	// Cite in reverse key order so the batches follow citation order, not key order.
	for i := 51; i >= 1; i-- {
		key := fmt.Sprintf("K%03d", i)
		citeKeys[key] = fmt.Sprintf("ref%03d", i)
		wantKeys = append(wantKeys, key)
		fmt.Fprintf(&tex, "\\cite{ref%03d}\n", i)
	}
	mbSeedCiteKeys(t, citeKeys)
	api := mbStartBibServer(t)
	manuscript := filepath.Join(t.TempDir(), "paper.tex")
	writeTestFile(t, manuscript, tex.String())

	if _, err := runCSLItemsCommand(t, &rootFlags{noCache: true}, []string{"bibliography", "--manuscript", manuscript}); err != nil {
		t.Fatalf("items bibliography --manuscript: %v", err)
	}
	got := api.snapshot()
	if len(got) != 2 || len(got[0]) != 50 || len(got[1]) != 1 {
		t.Fatalf("batches = %v, want 50 keys then 1", got)
	}
	if merged := append(append([]string{}, got[0]...), got[1]...); !reflect.DeepEqual(merged, wantKeys) {
		t.Fatalf("batched keys = %v, want citation order %v", merged, wantKeys)
	}
}

func TestMbManuscriptAndScopeAreMutuallyExclusive(t *testing.T) {
	isolateCSLTestEnv(t)
	api := mbStartBibServer(t)
	manuscript := filepath.Join(t.TempDir(), "paper.md")
	writeTestFile(t, manuscript, "@alpha\n")

	_, err := runCSLItemsCommand(t, &rootFlags{noCache: true}, []string{"bibliography", "--scope", "library", "--manuscript", manuscript})
	if code := ExitCode(err); code != 2 {
		t.Fatalf("ExitCode = %d (err=%v), want usage exit 2", code, err)
	}
	if got := api.snapshot(); len(got) != 0 {
		t.Fatalf("API requests = %v, want none", got)
	}
}

func TestMbManuscriptFollowIncludesExportsChapterCitations(t *testing.T) {
	isolateCSLTestEnv(t)
	mbSeedCiteKeys(t, map[string]string{"ITEMA": "alpha", "ITEMB": "beta"})
	api := mbStartBibServer(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "thesis.tex")
	writeTestFile(t, root, "\\cite{alpha}\n\\input{chapters/one}\n")
	if err := os.MkdirAll(filepath.Join(dir, "chapters"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeTestFile(t, filepath.Join(dir, "chapters", "one.tex"), "\\cite{beta}\n")

	if _, err := runCSLItemsCommand(t, &rootFlags{noCache: true}, []string{"bibliography", "--manuscript", root}); err != nil {
		t.Fatalf("without --follow-includes: %v", err)
	}
	if _, err := runCSLItemsCommand(t, &rootFlags{noCache: true}, []string{"bibliography", "--manuscript", root, "--follow-includes"}); err != nil {
		t.Fatalf("with --follow-includes: %v", err)
	}
	if got, want := api.snapshot(), [][]string{{"ITEMA"}, {"ITEMA", "ITEMB"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bib requests = %v, want chapter item only with --follow-includes: %v", got, want)
	}
}

func TestMbFollowIncludesRequiresManuscript(t *testing.T) {
	isolateCSLTestEnv(t)
	api := mbStartBibServer(t)
	_, err := runCSLItemsCommand(t, &rootFlags{noCache: true}, []string{"bibliography", "--scope", "library", "--follow-includes"})
	if code := ExitCode(err); code != 2 {
		t.Fatalf("ExitCode = %d (err=%v), want usage exit 2", code, err)
	}
	if got := api.snapshot(); len(got) != 0 {
		t.Fatalf("API requests = %v, want none", got)
	}
}
