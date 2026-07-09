// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestItemsCiteNamedCSLStyleUsesWebAPI(t *testing.T) {
	isolateCSLTestEnv(t)
	t.Setenv("ZOTERO_API_KEY", "testkey")

	const rendered = "<div class=\"csl-entry\">Rendered APA citation.</div>"
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/users/0/items/ITEM123" {
			t.Errorf("path = %q, want /users/0/items/ITEM123", r.URL.Path)
		}
		q := r.URL.Query()
		if got := q.Get("format"); got != "bib" {
			t.Errorf("format query = %q, want bib", got)
		}
		if got := q.Get("style"); got != "apa" {
			t.Errorf("style query = %q, want apa", got)
		}
		_, _ = w.Write([]byte(rendered))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	out, err := runCSLItemsCommand(t, &rootFlags{noCache: true}, []string{"cite", "ITEM123", "--style", "apa"})
	if err != nil {
		t.Fatalf("items cite --style apa: %v", err)
	}
	if got := out.String(); got != rendered {
		t.Fatalf("output = %q, want rendered server payload %q", got, rendered)
	}
	if hits != 1 {
		t.Fatalf("server hits = %d, want 1", hits)
	}
}

func TestItemsCiteNamedCSLStyleWithoutAPIKeyReturnsPreconditionEnvelope(t *testing.T) {
	isolateCSLTestEnv(t)
	t.Setenv("ZOTERO_API_KEY", "")

	out, err := runCSLItemsCommand(t, &rootFlags{asJSON: true, noCache: true}, []string{"cite", "ITEM123", "--style", "apa"})
	if err == nil {
		t.Fatal("items cite --style apa without API key returned nil error, want precondition error")
	}
	var cliErr *cliError
	if !errors.As(err, &cliErr) || cliErr.code != 9 {
		t.Fatalf("error = %T %[1]v, want cli precondition error code 9", err)
	}
	if code := ExitCode(err); code != 9 {
		t.Fatalf("ExitCode(err) = %d, want 9", code)
	}

	var envelope map[string]any
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("decode precondition envelope %q: %v", out.String(), err)
	}
	want := map[string]any{
		"kind":         "precondition_unmet",
		"precondition": "web_api_key",
		"title":        "Zotero Web API key required",
	}
	for field, value := range want {
		if envelope[field] != value {
			t.Fatalf("envelope[%q] = %#v, want %#v; envelope=%v", field, envelope[field], value, envelope)
		}
	}
	if detail, ok := envelope["detail"].(string); !ok || detail == "" {
		t.Fatalf("envelope detail = %#v, want non-empty string", envelope["detail"])
	}
	remediation, ok := envelope["remediation"].([]any)
	if !ok || len(remediation) == 0 {
		t.Fatalf("envelope remediation = %#v, want non-empty array", envelope["remediation"])
	}
}

func TestItemsCiteBibtexStyleUsesLocalAPIFormat(t *testing.T) {
	isolateCSLTestEnv(t)
	t.Setenv("ZOTERO_API_KEY", "")

	const rendered = "@article{item123,title={Local BibTeX}}"
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/users/0/items/ITEM123" {
			t.Errorf("path = %q, want /api/users/0/items/ITEM123", r.URL.Path)
		}
		q := r.URL.Query()
		if got := q.Get("format"); got != "bibtex" {
			t.Errorf("format query = %q, want bibtex", got)
		}
		if got := q.Get("style"); got != "" {
			t.Errorf("style query = %q, want empty for local BibTeX", got)
		}
		_, _ = w.Write([]byte(rendered))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/api/users/0")

	out, err := runCSLItemsCommand(t, &rootFlags{noCache: true}, []string{"cite", "ITEM123", "--style", "bibtex"})
	if err != nil {
		t.Fatalf("items cite --style bibtex: %v", err)
	}
	if got := out.String(); got != rendered {
		t.Fatalf("output = %q, want local API payload %q", got, rendered)
	}
	if hits != 1 {
		t.Fatalf("server hits = %d, want 1", hits)
	}
}

func TestItemsBibliographyChunksCSLRequestsInScopeOrder(t *testing.T) {
	isolateCSLTestEnv(t)
	t.Setenv("ZOTERO_API_KEY", "testkey")

	keys := make([]string, 53)
	for i := range keys {
		keys[i] = fmt.Sprintf("K%02d", i+1)
	}
	var bibRequests [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/users/0/items" {
			t.Errorf("path = %q, want /users/0/items", r.URL.Path)
		}
		q := r.URL.Query()
		switch q.Get("format") {
		case "json":
			if got := q.Get("limit"); got != "100" {
				t.Errorf("scope limit = %q, want 100", got)
			}
			if got := q.Get("start"); got != "" {
				t.Errorf("scope start = %q, want empty for single page", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(bibliographyScopeJSON(keys)))
		case "bib":
			if got := q.Get("style"); got != "apa" {
				t.Errorf("style query = %q, want apa", got)
			}
			chunk := splitNonEmpty(q.Get("itemKey"), ",")
			bibRequests = append(bibRequests, chunk)
			_, _ = fmt.Fprintf(w, "chunk%d:%s", len(bibRequests), q.Get("itemKey")) //nolint:gosec // G705: httptest double echoing request params back to the test client; no browser sink.
		default:
			t.Errorf("format query = %q, want json or bib", q.Get("format"))
			http.Error(w, "unexpected format", http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	out, err := runCSLItemsCommand(t, &rootFlags{noCache: true}, []string{"bibliography", "--scope", "library", "--style", "apa"})
	if err != nil {
		t.Fatalf("items bibliography --scope library --style apa: %v", err)
	}
	if len(bibRequests) != 2 {
		t.Fatalf("bib request count = %d, want 2; requests=%v", len(bibRequests), bibRequests)
	}
	if got := []int{len(bibRequests[0]), len(bibRequests[1])}; !reflect.DeepEqual(got, []int{50, 3}) {
		t.Fatalf("chunk sizes = %v, want [50 3]", got)
	}
	if got := append(append([]string{}, bibRequests[0]...), bibRequests[1]...); !reflect.DeepEqual(got, keys) {
		t.Fatalf("chunked key order = %v, want %v", got, keys)
	}
	wantOut := "chunk1:" + joinStrings(keys[:50], ",") + "\nchunk2:" + joinStrings(keys[50:], ",")
	if got := out.String(); got != wantOut {
		t.Fatalf("bibliography output = %q, want concatenated chunks %q", got, wantOut)
	}
}

func isolateCSLTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_HOME", t.TempDir())
	t.Setenv("ZOTERO_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTIO_DEMO", "0")
	savedGroup := activeGroupID
	activeGroupID = ""
	t.Cleanup(func() { activeGroupID = savedGroup })
}

func runCSLItemsCommand(t *testing.T, flags *rootFlags, args []string) (*bytes.Buffer, error) {
	t.Helper()
	cmd := newItemsCmd(flags)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	return &out, cmd.Execute()
}

func bibliographyScopeJSON(keys []string) string {
	rows := make([]map[string]string, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, map[string]string{"key": key})
	}
	data, _ := json.Marshal(rows)
	return string(data)
}

func splitNonEmpty(s, sep string) []string {
	if s == "" {
		return nil
	}
	out := make([]string, 0)
	for _, part := range strings.Split(s, sep) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, part := range parts[1:] {
		out += sep + part
	}
	return out
}
