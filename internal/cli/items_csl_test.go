// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"zotio/internal/config"
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

func TestItemsBibliographyCSLJSONMergesChunksAndUsesCiteKeys(t *testing.T) {
	isolateCSLTestEnv(t)
	t.Setenv("ZOTERO_API_KEY", "testkey")

	keys := make([]string, 53)
	citeKeys := make(map[string]string, len(keys))
	for i := range keys {
		keys[i] = fmt.Sprintf("K%02d", i+1)
		citeKeys[keys[i]] = fmt.Sprintf("cite%02d", i+1)
	}
	var exportRequests [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("format") {
		case "json":
			_, _ = w.Write([]byte(bibliographyScopeJSONWithCiteKeys(keys, citeKeys)))
		case "csljson":
			chunk := splitNonEmpty(r.URL.Query().Get("itemKey"), ",")
			exportRequests = append(exportRequests, chunk)
			if got := r.URL.Query().Get("limit"); got != fmt.Sprint(len(chunk)) {
				t.Errorf("csljson limit = %q, want %d", got, len(chunk))
			}
			entries := make([]map[string]any, 0, len(chunk))
			// The server may return its own sort order. The command must restore
			// the scope order before it merges chunks.
			for i := len(chunk) - 1; i >= 0; i-- {
				key := chunk[i]
				id := key
				if i%2 == 0 {
					id = "123/" + key
				}
				entries = append(entries, map[string]any{"id": id, "title": "Title " + key})
			}
			_ = json.NewEncoder(w).Encode(entries)
		default:
			http.Error(w, "unexpected format", http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	out, err := runCSLItemsCommand(t, &rootFlags{asJSON: true, noCache: true}, []string{"bibliography", "--format", "csljson"})
	if err != nil {
		t.Fatalf("items bibliography --format csljson: %v", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(out.Bytes(), &entries); err != nil {
		t.Fatalf("decode CSL-JSON %q: %v", out.String(), err)
	}
	if len(entries) != len(keys) {
		t.Fatalf("CSL-JSON entries = %d, want %d", len(entries), len(keys))
	}
	for i, entry := range entries {
		if got := entry["id"]; got != citeKeys[keys[i]] {
			t.Fatalf("entry %d id = %v, want %q", i, got, citeKeys[keys[i]])
		}
		if got := entry["title"]; got != "Title "+keys[i] {
			t.Fatalf("entry %d title = %v, want preserved title", i, got)
		}
	}
	if got := []int{len(exportRequests[0]), len(exportRequests[1])}; !reflect.DeepEqual(got, []int{50, 3}) {
		t.Fatalf("CSL-JSON chunk sizes = %v, want [50 3]", got)
	}
}

func TestItemsBibliographyItemScopeFetchesCiteKeyMetadata(t *testing.T) {
	isolateCSLTestEnv(t)
	t.Setenv("ZOTERO_API_KEY", "testkey")
	var formats []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		format := r.URL.Query().Get("format")
		formats = append(formats, format)
		if got := r.URL.Query().Get("itemKey"); got != "K1" {
			t.Errorf("itemKey = %q, want K1", got)
		}
		switch format {
		case "json":
			_, _ = w.Write([]byte(`[{"key":"K1","data":{"key":"K1","extra":"Citation Key: directKey"}}]`))
		case "csljson":
			_, _ = w.Write([]byte(`{"items":[{"id":"123/K1","title":"Direct"}]}`))
		default:
			http.Error(w, "unexpected format", http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	out, err := runCSLItemsCommand(t, &rootFlags{asJSON: true, noCache: true}, []string{"bibliography", "--scope", "item:K1", "--format", "csljson"})
	if err != nil {
		t.Fatalf("item-scope CSL-JSON: %v", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(out.Bytes(), &entries); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(entries) != 1 || entries[0]["id"] != "directKey" {
		t.Fatalf("entries = %#v, want id directKey", entries)
	}
	if !reflect.DeepEqual(formats, []string{"json", "csljson"}) {
		t.Fatalf("request formats = %v, want metadata then export", formats)
	}
}

func TestItemsBibliographyCSLJSONRejectsMissingAndDuplicateCiteKeys(t *testing.T) {
	tests := []struct {
		name string
		rows string
		want string
	}{
		{
			name: "missing",
			rows: `[{"key":"K1","data":{"key":"K1","title":"No Key"}}]`,
			want: "have no Better BibTeX citation key",
		},
		{
			name: "duplicate",
			rows: `[
				{"key":"K1","data":{"key":"K1","extra":"Citation Key: same"}},
				{"key":"K2","data":{"key":"K2","citationKey":"same"}}
			]`,
			want: "are duplicated",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateCSLTestEnv(t)
			t.Setenv("ZOTERO_API_KEY", "testkey")
			var exportCalled bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("format") == "csljson" {
					exportCalled = true
				}
				_, _ = w.Write([]byte(tc.rows))
			}))
			defer srv.Close()
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

			out, err := runCSLItemsCommand(t, &rootFlags{asJSON: true, noCache: true}, []string{"bibliography", "--format", "csljson"})
			if err == nil {
				t.Fatal("CSL-JSON with invalid citekeys returned nil error")
			}
			if exportCalled {
				t.Fatal("CSL-JSON export ran before citekey validation")
			}
			var env preconditionUnmetEnvelope
			if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
				t.Fatalf("decode precondition output %q: %v", out.String(), decodeErr)
			}
			if env.Precondition != preconditionBetterBibTeX || !strings.Contains(env.Detail, tc.want) {
				t.Fatalf("precondition = %+v, want better_bibtex detail containing %q", env, tc.want)
			}
		})
	}
}

func TestItemsBibliographyBibTeXChunksWithoutChangingText(t *testing.T) {
	isolateCSLTestEnv(t)
	t.Setenv("ZOTERO_API_KEY", "testkey")
	keys := make([]string, 53)
	for i := range keys {
		keys[i] = fmt.Sprintf("K%02d", i+1)
	}
	var exportRequests [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("format") {
		case "json":
			_, _ = w.Write([]byte(bibliographyScopeJSON(keys)))
		case "bibtex":
			chunk := splitNonEmpty(r.URL.Query().Get("itemKey"), ",")
			exportRequests = append(exportRequests, chunk)
			if got := r.URL.Query().Get("limit"); got != fmt.Sprint(len(chunk)) {
				t.Errorf("bibtex limit = %q, want %d", got, len(chunk))
			}
			if got := r.URL.Query().Get("style"); got != "" {
				t.Errorf("bibtex style = %q, want none", got)
			}
			_, _ = fmt.Fprintf(w, "chunk%d:%s", len(exportRequests), strings.Join(chunk, ","))
		default:
			http.Error(w, "unexpected format", http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	out, err := runCSLItemsCommand(t, &rootFlags{noCache: true}, []string{"bibliography", "--format", "bibtex"})
	if err != nil {
		t.Fatalf("items bibliography --format bibtex: %v", err)
	}
	want := "chunk1:" + strings.Join(keys[:50], ",") + "\nchunk2:" + strings.Join(keys[50:], ",")
	if got := out.String(); got != want {
		t.Fatalf("BibTeX output = %q, want exact chunks %q", got, want)
	}
}

func TestItemsBibliographyFormatValidation(t *testing.T) {
	for _, format := range []string{"bib", "csljson", "bibtex", "biblatex", "ris"} {
		if got, err := normalizeBibliographyFormat(format); err != nil || got != format {
			t.Errorf("normalize %q = (%q, %v)", format, got, err)
		}
	}
	if _, err := normalizeBibliographyFormat("yaml"); err == nil {
		t.Fatal("invalid bibliography format returned nil error")
	}
}

func TestItemsBibliographyCSLJSONSkipsNonCiteableChildItems(t *testing.T) {
	isolateCSLTestEnv(t)
	t.Setenv("ZOTERO_API_KEY", "testkey")
	var exportedKeys string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("format") {
		case "json":
			_, _ = w.Write([]byte(`[
				{"key":"BOOK","data":{"key":"BOOK","itemType":"book","citationKey":"bookKey"}},
				{"key":"PDF","data":{"key":"PDF","itemType":"attachment","parentItem":"BOOK"}},
				{"key":"NOTE","data":{"key":"NOTE","itemType":"note","parentItem":"BOOK"}}
			]`))
		case "csljson":
			exportedKeys = r.URL.Query().Get("itemKey")
			_, _ = w.Write([]byte(`{"items":[{"id":"123/BOOK","title":"Book"}]}`))
		default:
			http.Error(w, "unexpected format", http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	out, err := runCSLItemsCommand(t, &rootFlags{asJSON: true, noCache: true}, []string{"bibliography", "--format", "csljson"})
	if err != nil {
		t.Fatalf("CSL-JSON with child items: %v", err)
	}
	if exportedKeys != "BOOK" {
		t.Fatalf("exported itemKey = %q, want only BOOK", exportedKeys)
	}
	var entries []map[string]any
	if err := json.Unmarshal(out.Bytes(), &entries); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(entries) != 1 || entries[0]["id"] != "bookKey" {
		t.Fatalf("entries = %#v, want one bookKey item", entries)
	}
}

func isolateCSLTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_HOME", t.TempDir())
	t.Setenv("ZOTERO_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTIO_DEMO", "0")
	savedGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(savedGroup) })
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

func bibliographyScopeJSONWithCiteKeys(keys []string, citeKeys map[string]string) string {
	rows := make([]map[string]any, 0, len(keys))
	for i, key := range keys {
		data := map[string]any{"key": key}
		if i%2 == 0 {
			data["citationKey"] = citeKeys[key]
		} else {
			data["extra"] = "Citation Key: " + citeKeys[key]
		}
		rows = append(rows, map[string]any{"key": key, "data": data})
	}
	encoded, _ := json.Marshal(rows)
	return string(encoded)
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

// TestItemsBibliographyReadOnlyDoesNotPersistUserID asserts the read-only
// invariant for items bibliography: with an absent configured UserID, a
// personal-library Web read resolves the Web base via keys/current but
// leaves the config file byte-identical (and mtime unchanged), matching the
// harness in readonly_contract_test.go ("writes nothing to the config file").
// The resolved ID is still usable in-memory for the current invocation, so
// the command succeeds and renders the bibliography.
func TestItemsBibliographyReadOnlyDoesNotPersistUserID(t *testing.T) {
	oldAllowPrivateOutbound := allowPrivateOutboundForTests
	allowPrivateOutboundForTests = true
	t.Cleanup(func() { allowPrivateOutboundForTests = oldAllowPrivateOutbound })

	// Web API mock: userID resolution plus a BIB render.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/keys/current":
			_, _ = w.Write([]byte(`{"userID":99,"username":"x","access":{}}`))
		case "/users/99/items":
			q := r.URL.Query()
			if q.Get("format") == "bib" {
				_, _ = w.Write([]byte("Dune, 1965."))
				return
			}
			// Scope probe before render: empty library yields empty array.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	oldBase := zoteroWebAPIBase
	zoteroWebAPIBase = api.URL
	defer func() { zoteroWebAPIBase = oldBase }()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	// Local data API base triggers hybrid routing in newWebReadClient.
	// No user_id on purpose — resolution must happen via keys/current.
	if err := os.WriteFile(configPath, []byte("base_url = \"http://localhost:23119/api/users/0\"\napi_key = \"k\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config before: %v", err)
	}
	beforeInfo, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat config before: %v", err)
	}

	// Drive items bibliography through the CLI, not just the resolver, so
	// the test covers the exact user-facing promise: `zotio items bibliography`
	// with a missing UserID renders while writing nothing to config.
	t.Setenv("ZOTERO_CONFIG", configPath)
	t.Setenv("ZOTERO_API_KEY", "k")
	t.Setenv("ZOTERO_BASE_URL", "http://localhost:23119/api/users/0")
	// Isolate HOME so other probes cannot create ~/.zotio side effects.
	t.Setenv("HOME", dir)
	t.Setenv("ZOTIO_DEMO", "0")
	// Some helpers touch the temp dir via ZOTERO_DATA_DIR; point it inside dir.
	t.Setenv("ZOTERO_DATA_DIR", filepath.Join(dir, "data"))
	activeGroupSave := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(activeGroupSave) })

	flags := &rootFlags{configPath: configPath, noCache: true}
	// Scope "item:K1" avoids the paginated scope-key loop hitting the Web API
	// with unmocked params; the single Get for BIB is what we want to prove
	// still succeeds after the in-memory userID is filled.
	out, err := runCSLItemsCommand(t, flags, []string{"bibliography", "--scope", "item:K1"})
	if err != nil {
		t.Fatalf("items bibliography: %v", err)
	}
	if got := out.String(); got != "Dune, 1965." {
		t.Fatalf("bibliography output = %q, want %q", got, "Dune, 1965.")
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config after: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("config mutated by read-only bibliography:\nbefore %q\nafter  %q", string(before), string(after))
	}
	if fi, err := os.Stat(configPath); err != nil {
		t.Fatalf("stat after: %v", err)
	} else if !fi.ModTime().Equal(beforeInfo.ModTime()) {
		t.Fatalf("config mtime changed: before %v, after %v", beforeInfo.ModTime(), fi.ModTime())
	}

	// Counter-proof: the mutating path still persists.
	mutDir := t.TempDir()
	mutPath := filepath.Join(mutDir, "config.toml")
	if err := os.WriteFile(mutPath, []byte("base_url = \"http://localhost:23119/api/users/0\"\napi_key = \"k\"\n"), 0o600); err != nil {
		t.Fatalf("write mut config: %v", err)
	}
	mutCfg, err := config.Load(mutPath)
	if err != nil {
		t.Fatalf("load mut cfg: %v", err)
	}
	// Resolve via the write path — it must now persist.
	if _, err := resolveWebWriteBase(context.Background(), mutCfg, "", 0); err != nil {
		t.Fatalf("resolve write path: %v", err)
	}
	mutAfter, err := os.ReadFile(mutPath)
	if err != nil {
		t.Fatalf("read mut config after: %v", err)
	}
	if !strings.Contains(string(mutAfter), "user_id") {
		t.Fatalf("write path did not persist user_id; file = %q", string(mutAfter))
	}
	if mutCfg.UserID != "99" {
		t.Fatalf("write path UserID = %q, want 99", mutCfg.UserID)
	}

	// And the read-only path leaves its own file alone even when exercised
	// through the lower-level resolver (covers both entry points).
	roDir := t.TempDir()
	roPath := filepath.Join(roDir, "config.toml")
	if err := os.WriteFile(roPath, []byte("base_url = \"http://localhost:23119/api/users/0\"\napi_key = \"k\"\n"), 0o600); err != nil {
		t.Fatalf("write ro config: %v", err)
	}
	roCfg, err := config.Load(roPath)
	if err != nil {
		t.Fatalf("load ro cfg: %v", err)
	}
	roBefore, _ := os.ReadFile(roPath)
	if _, err := resolveWebWriteBaseWithoutPersist(context.Background(), roCfg, "", 0); err != nil {
		t.Fatalf("resolve read path: %v", err)
	}
	if roCfg.UserID != "99" {
		t.Fatalf("read path in-memory UserID = %q, want 99", roCfg.UserID)
	}
	roAfter, _ := os.ReadFile(roPath)
	if string(roAfter) != string(roBefore) {
		t.Fatalf("read path mutated config:\nbefore %q\nafter %q", string(roBefore), string(roAfter))
	}
}
