// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"zotio/internal/client"
	"zotio/internal/cliutil"
	"zotio/internal/store"
)

func cliTestSeedItemsStore(t *testing.T, items []json.RawMessage) {
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
		t.Fatalf("close seeded store: %v", err)
	}
}

func helpersTestAPIError(status int, body string) error {
	return &client.APIError{Method: "GET", Path: "/items", StatusCode: status, Body: body}
}

func helpersTestIsolateConfigEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{
		"ZOTERO_API_KEY",
		"ZOTERO_CONFIG",
		"ZOTERO_HOME",
		"ZOTERO_CONFIG_DIR",
		"ZOTERO_DATA_DIR",
		"XDG_CONFIG_HOME",
		"XDG_DATA_HOME",
		"ZOTIO_DEMO",
	} {
		t.Setenv(name, "")
	}
}

func helpersTestDefaultDBPath(t *testing.T, name string) string {
	t.Helper()
	path, err := defaultDBPath(name)
	if err != nil {
		t.Fatalf("default database path: %v", err)
	}
	return path
}

func helpersTestJournalDir(t *testing.T) string {
	t.Helper()
	dir, err := journalDir()
	if err != nil {
		t.Fatalf("journal directory: %v", err)
	}
	return dir
}

func helpersTestWriteCredentialsAPIKey(t *testing.T, key string) {
	t.Helper()
	path, err := cliutil.CredentialsFilePath()
	if err != nil {
		t.Fatalf("CredentialsFilePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir credentials dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("api_key = \""+key+"\"\n"), 0o600); err != nil {
		t.Fatalf("write credentials.toml: %v", err)
	}
}

func helpersTestAssertCLIError(t *testing.T, got error, wantCode int) {
	t.Helper()
	var ce *cliError
	if !errors.As(got, &ce) {
		t.Fatalf("error type = %T, want *cliError", got)
	}
	if ce.code != wantCode {
		t.Fatalf("cliError.code = %d, want %d (err: %v)", ce.code, wantCode, got)
	}
}

func helpersTestAssertJSONEqual(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var gotAny any
	if err := json.Unmarshal(got, &gotAny); err != nil {
		t.Fatalf("got invalid JSON %q: %v", string(got), err)
	}
	var wantAny any
	if err := json.Unmarshal([]byte(want), &wantAny); err != nil {
		t.Fatalf("want invalid JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(gotAny, wantAny) {
		t.Fatalf("JSON mismatch\ngot:  %s\nwant: %s", string(got), want)
	}
}

func TestHelpersClassifyAPIError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "unauthorized", err: helpersTestAPIError(401, "bad key"), want: 4},
		{name: "forbidden", err: helpersTestAPIError(403, "permission denied"), want: 4},
		{name: "not found", err: helpersTestAPIError(404, "missing"), want: 3},
		{name: "rate limited api error", err: helpersTestAPIError(429, "slow down"), want: 7},
		{name: "server error", err: helpersTestAPIError(500, "upstream exploded"), want: 5},
		{name: "generic error", err: errors.New("dial tcp refused"), want: 5},
		{name: "rate limit error", err: errors.New("rate limited: HTTP 429 for https://api.example.test/items"), want: 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			helpersTestAssertCLIError(t, classifyAPIError(tt.err, nil), tt.want)
		})
	}
}

func TestHelpersClassifyAPIErrorRedactsBadRequestAuthBody(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		wantCode    int
		credentials bool
	}{
		{name: "bad_request_auth_env", status: 400, wantCode: 4},
		{name: "unauthorized_env", status: 401, wantCode: 4},
		{name: "forbidden_env", status: 403, wantCode: 4},
		{name: "not_found_env", status: 404, wantCode: 3},
		{name: "rate_limited_env", status: 429, wantCode: 7},
		{name: "default_credentials_file", status: 418, wantCode: 5, credentials: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			helpersTestIsolateConfigEnv(t)
			secret := fmt.Sprintf("MARKER_SECRET_%d_9f8e7d6c", tt.status)
			if tt.credentials {
				helpersTestWriteCredentialsAPIKey(t, secret)
			} else {
				t.Setenv("ZOTERO_API_KEY", secret)
			}

			err := &client.APIError{
				Method:     "GET",
				Path:       "/items",
				StatusCode: tt.status,
				Body:       "upstream reflected credential " + secret,
			}

			got := classifyAPIError(err, nil)
			msg := got.Error()
			// Regression guard: previous %w-based implementations re-rendered
			// client.APIError.Error() and leaked reflected response bodies.
			if strings.Contains(msg, secret) {
				t.Fatalf("classifyAPIError() leaked API key in error text: %q", msg)
			}
			if code := ExitCode(got); code != tt.wantCode {
				t.Fatalf("ExitCode(classifyAPIError()) = %d, want %d", code, tt.wantCode)
			}
			var apiErr *client.APIError
			if !errors.As(got, &apiErr) {
				t.Fatalf("errors.As(classifyAPIError(), *client.APIError) = false, want true")
			}
			if tt.status == 400 && !strings.Contains(msg, "zotio doctor") {
				t.Fatalf("classifyAPIError() = %q, want zotio doctor hint", msg)
			}
		})
	}

	t.Run("local_write_rejection", func(t *testing.T) {
		helpersTestIsolateConfigEnv(t)
		const secret = "MARKER_SECRET_local_9f8e7d6c"
		t.Setenv("ZOTERO_API_KEY", secret)
		err := &client.APIError{Method: "POST", Path: "/items", StatusCode: 400, Body: "Endpoint does not support method " + secret}
		got := classifyAPIError(err, nil)
		if msg := got.Error(); strings.Contains(msg, secret) {
			t.Fatalf("classifyAPIError() leaked API key in local write error text: %q", msg)
		}
		if code := ExitCode(got); code != 5 {
			t.Fatalf("ExitCode(classifyAPIError()) = %d, want 5", code)
		}
		var apiErr *client.APIError
		if !errors.As(got, &apiErr) {
			t.Fatalf("errors.As(classifyAPIError(), *client.APIError) = false, want true")
		}
	})

	t.Run("precondition_failed", func(t *testing.T) {
		helpersTestIsolateConfigEnv(t)
		const secret = "MARKER_SECRET_412_9f8e7d6c"
		t.Setenv("ZOTERO_API_KEY", secret)
		err := &client.APIError{Method: "PATCH", Path: "/items/abc", StatusCode: 412, Body: "version conflict " + secret}
		got := classifyAPIError(err, nil)
		if msg := got.Error(); strings.Contains(msg, secret) {
			t.Fatalf("classifyAPIError() leaked API key in 412 error text: %q", msg)
		}
		if code := ExitCode(got); code != 5 {
			t.Fatalf("ExitCode(classifyAPIError()) = %d, want 5", code)
		}
		var apiErr *client.APIError
		if !errors.As(got, &apiErr) {
			t.Fatalf("errors.As(classifyAPIError(), *client.APIError) = false, want true")
		}
	})
}

// TestHelpersClassifyAPIErrorStatusCodeMapping was TestHelpersClassifyDeleteError
// before classifyDeleteError was folded away: with --ignore-missing now resolved
// as a structured mutation no_op inside items/collections delete's own Apply
// (see N4-2's fix and its follow-up), classifyDeleteError's only remaining
// behaviour was delegating straight to classifyAPIError, so this table now
// exercises that directly rather than through a dead wrapper.
func TestHelpersClassifyAPIErrorStatusCodeMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "unauthorized", err: helpersTestAPIError(401, "bad key"), want: 4},
		{name: "forbidden", err: helpersTestAPIError(403, "permission denied"), want: 4},
		{name: "not found", err: helpersTestAPIError(404, "missing"), want: 3},
		{name: "rate limited", err: helpersTestAPIError(429, "slow down"), want: 7},
		{name: "server error", err: helpersTestAPIError(500, "upstream exploded"), want: 5},
		{name: "generic error", err: errors.New("delete failed"), want: 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			helpersTestAssertCLIError(t, classifyAPIError(tt.err, nil), tt.want)
		})
	}
}

func TestHelpersAccessDenial(t *testing.T) {
	bodyTests := []struct {
		name string
		body string
		want bool
	}{
		{name: "forbidden", body: "Forbidden for this resource", want: true},
		{name: "not authorized", body: "caller is not_authorized for this workspace", want: true},
		{name: "insufficient permission", body: "insufficient_permission: admin required", want: true},
		{name: "normal body", body: "author biography mentions pagination_token and insufficient_funds", want: false},
		{name: "empty", body: "", want: false},
	}
	for _, tt := range bodyTests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeAccessDenial(tt.body); got != tt.want {
				t.Fatalf("looksLikeAccessDenial(%q) = %t, want %t", tt.body, got, tt.want)
			}
		})
	}

	warning, ok := isSyncAccessWarning(fmt.Errorf("sync page: %w", helpersTestAPIError(403, "Forbidden by ACL")))
	if !ok || warning == nil {
		t.Fatal("wrapped 403 API error was not classified as an access warning")
	}
	if warning.Status != 403 || warning.Reason != "forbidden" || warning.Message != "Forbidden by ACL" {
		t.Fatalf("warning = %#v, want 403 forbidden with original body", warning)
	}

	warning, ok = isSyncAccessWarning(fmt.Errorf("sync page: %w", helpersTestAPIError(400, "missing scope: library.read")))
	if !ok || warning == nil || warning.Status != 400 || warning.Reason != "insufficient_access" {
		t.Fatalf("400 access body warning = %#v, %t; want insufficient_access", warning, ok)
	}

	if warning, ok := isSyncAccessWarning(errors.New("network down")); ok || warning != nil {
		t.Fatalf("unrelated error warning = %#v, %t; want nil,false", warning, ok)
	}
}

func TestHelpersStringsAndSuggestions(t *testing.T) {
	truncateTests := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{name: "empty", s: "", max: 5, want: ""},
		{name: "under", s: "abcd", max: 5, want: "abcd"},
		{name: "at", s: "abcde", max: 5, want: "abcde"},
		{name: "over ellipsis", s: "abcdef", max: 5, want: "ab..."},
		{name: "over tiny max", s: "abcdef", max: 3, want: "abc"},
	}
	for _, tt := range truncateTests {
		t.Run("truncate "+tt.name, func(t *testing.T) {
			if got := truncate(tt.s, tt.max); got != tt.want {
				t.Fatalf("truncate(%q, %d) = %q, want %q", tt.s, tt.max, got, tt.want)
			}
		})
	}

	if got := replacePathParam("/users/{user}/items/{item}", "item", "a/b c?x=1"); got != "/users/{user}/items/a%2Fb%20c%3Fx=1" {
		t.Fatalf("replacePathParam escaped path metacharacters = %q", got)
	}

	caseTests := []struct {
		name string
		in   string
		want string
	}{
		{name: "camel", in: "orderDate", want: "order-date"},
		{name: "lower", in: "orderdate", want: "orderdate"},
		{name: "upper boundary", in: "statusCode", want: "status-code"},
	}
	for _, tt := range caseTests {
		t.Run("camelToKebab "+tt.name, func(t *testing.T) {
			if got := camelToKebab(tt.in); got != tt.want {
				t.Fatalf("camelToKebab(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	splitTests := []struct {
		in   string
		want []string
	}{
		{in: "OrderDate", want: []string{"order", "date"}},
		{in: "statusCode", want: []string{"status", "code"}},
		{in: "page_size", want: []string{"page", "size"}},
		{in: "page-size", want: []string{"page", "size"}},
	}
	for _, tt := range splitTests {
		t.Run("splitCamelCase "+tt.in, func(t *testing.T) {
			got := splitCamelCase(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("splitCamelCase(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("splitCamelCase(%q) = %#v, want %#v", tt.in, got, tt.want)
				}
			}
		})
	}

	distanceTests := []struct {
		a, b string
		want int
	}{
		{a: "same", b: "same", want: 0},
		{a: "kitten", b: "sitting", want: 3},
		{a: "abc", b: "xyz", want: 3},
		{a: "", b: "flag", want: 4},
	}
	for _, tt := range distanceTests {
		t.Run("levenshtein", func(t *testing.T) {
			if got := levenshteinDistance(tt.a, tt.b); got != tt.want {
				t.Fatalf("levenshteinDistance(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}

	cmd := &cobra.Command{Use: "helpers-test"}
	cmd.Flags().String("limit", "", "limit")
	cmd.Flags().Bool("include-deleted", false, "include deleted")
	cmd.PersistentFlags().String("api-key", "", "api key")
	if got := suggestFlag("--limt", cmd); got != "limit" {
		t.Fatalf("suggestFlag near miss = %q, want limit", got)
	}
	if got := suggestFlag("--zzzzzzzz", cmd); got != "" {
		t.Fatalf("suggestFlag far miss = %q, want empty", got)
	}
}

func TestHelpersPaginationExtraction(t *testing.T) {
	obj := map[string]json.RawMessage{
		"results": json.RawMessage(`[{"id":1},{"id":2}]`),
		"meta":    json.RawMessage(`{"next":{"cursor":"abc"},"has_more":true}`),
	}
	items, ok := extractPaginatedItems(obj)
	if !ok || len(items) != 2 || string(items[1]) != `{"id":2}` {
		t.Fatalf("extractPaginatedItems results = %v, %t; want two items", items, ok)
	}

	singleArrayObj := map[string]json.RawMessage{"payload": json.RawMessage(`[{"only":true}]`), "count": json.RawMessage(`1`)}
	items, ok = extractPaginatedItems(singleArrayObj)
	if !ok || len(items) != 1 || string(items[0]) != `{"only":true}` {
		t.Fatalf("extractPaginatedItems single array fallback = %v, %t", items, ok)
	}

	multiArrayObj := map[string]json.RawMessage{"a": json.RawMessage(`[1]`), "b": json.RawMessage(`[2]`)}
	if items, ok := extractPaginatedItems(multiArrayObj); ok || items != nil {
		t.Fatalf("extractPaginatedItems multiple anonymous arrays = %v, %t; want nil,false", items, ok)
	}
}

// TestWrapWithProvenanceResultsAlwaysArray pins the read-envelope invariant:
// .results is always a JSON array, whether the underlying read returned a
// single object (items get, collections get, ...), an already-array list
// read (items list, search, ...), or a non-JSON payload (a --format bib
// response). A jq pipeline written against results[0] or results[] must
// work uniformly across all three.
func TestWrapWithProvenanceResultsAlwaysArray(t *testing.T) {
	prov := DataProvenance{Source: "live"}

	t.Run("single object becomes a one-element array", func(t *testing.T) {
		wrapped, err := wrapWithProvenance(json.RawMessage(`{"key":"ABCD1234","version":7}`), prov)
		if err != nil {
			t.Fatalf("wrapWithProvenance: %v", err)
		}
		var env struct {
			Results []map[string]any `json:"results"`
			Meta    struct {
				Source string `json:"source"`
			} `json:"meta"`
		}
		if err := json.Unmarshal(wrapped, &env); err != nil {
			t.Fatalf("decode %s: %v", wrapped, err)
		}
		if len(env.Results) != 1 {
			t.Fatalf("results length = %d, want 1 (envelope: %s)", len(env.Results), wrapped)
		}
		// jq-style traversal: results[0].key must reach the wrapped object.
		if got := env.Results[0]["key"]; got != "ABCD1234" {
			t.Fatalf("results[0].key = %v, want ABCD1234", got)
		}
		if env.Meta.Source != "live" {
			t.Fatalf("meta.source = %q, want live", env.Meta.Source)
		}
	})

	t.Run("array passes through unchanged", func(t *testing.T) {
		wrapped, err := wrapWithProvenance(json.RawMessage(`[{"key":"A"},{"key":"B"}]`), prov)
		if err != nil {
			t.Fatalf("wrapWithProvenance: %v", err)
		}
		var env struct {
			Results []map[string]any `json:"results"`
		}
		if err := json.Unmarshal(wrapped, &env); err != nil {
			t.Fatalf("decode %s: %v", wrapped, err)
		}
		if len(env.Results) != 2 || env.Results[0]["key"] != "A" || env.Results[1]["key"] != "B" {
			t.Fatalf("results = %v, want [A B] unchanged", env.Results)
		}
	})

	t.Run("empty array stays empty, not a one-element array of an empty array", func(t *testing.T) {
		wrapped, err := wrapWithProvenance(json.RawMessage(`[]`), prov)
		if err != nil {
			t.Fatalf("wrapWithProvenance: %v", err)
		}
		var env struct {
			Results []map[string]any `json:"results"`
		}
		if err := json.Unmarshal(wrapped, &env); err != nil {
			t.Fatalf("decode %s: %v", wrapped, err)
		}
		if env.Results == nil || len(env.Results) != 0 {
			t.Fatalf("results = %v, want empty array", env.Results)
		}
	})

	t.Run("non-JSON payload becomes a one-element array", func(t *testing.T) {
		wrapped, err := wrapWithProvenance(json.RawMessage("Smith, J. (2020). A paper."), prov)
		if err != nil {
			t.Fatalf("wrapWithProvenance: %v", err)
		}
		var env struct {
			Results []string `json:"results"`
		}
		if err := json.Unmarshal(wrapped, &env); err != nil {
			t.Fatalf("decode %s: %v", wrapped, err)
		}
		if len(env.Results) != 1 || env.Results[0] != "Smith, J. (2020). A paper." {
			t.Fatalf("results = %v, want one-element array holding the raw text", env.Results)
		}
	})
}

func TestHelpersFilterFields(t *testing.T) {
	data := json.RawMessage(`[{"id":1,"name":"paper","owner":{"email":"a@example.test","name":"Ada"},"createdAt":"2026-06-01T12:00:00Z","ignored":true}]`)
	helpersTestAssertJSONEqual(t, filterFields(data, "id,owner.email,created-at"), `[{"id":1,"owner":{"email":"a@example.test"},"createdAt":"2026-06-01T12:00:00Z"}]`)

	nested := filterFieldsRec(json.RawMessage(`{"owner":{"email":"a@example.test","name":"Ada"},"name":"paper"}`), [][]string{{"owner", "name"}})
	helpersTestAssertJSONEqual(t, nested, `{"owner":{"name":"Ada"}}`)

	whole := filterFieldsRec(json.RawMessage(`{"owner":{"email":"a@example.test","name":"Ada"},"name":"paper"}`), [][]string{{"owner"}})
	helpersTestAssertJSONEqual(t, whole, `{"owner":{"email":"a@example.test","name":"Ada"}}`)

	keepWhole := map[string]bool{"order-date": true}
	subPaths := map[string][][]string{"owner": {{"email"}}}
	if got := matchSelectSegment("orderDate", keepWhole, subPaths); got != "order-date" {
		t.Fatalf("matchSelectSegment camel/kebab = %q, want order-date", got)
	}
	if got := matchSelectSegment("Owner", keepWhole, subPaths); got != "owner" {
		t.Fatalf("matchSelectSegment subpath = %q, want owner", got)
	}
	if got := matchSelectSegment("unrelated", keepWhole, subPaths); got != "" {
		t.Fatalf("matchSelectSegment miss = %q, want empty", got)
	}
}

func TestHelpersCompactExtractAndFormat(t *testing.T) {
	helpersTestAssertJSONEqual(t, compactFields(json.RawMessage(`[{"id":"1","name":"Paper","description":"long","body":"long","unknown":"drop"}]`)), `[{"id":"1","name":"Paper"}]`)
	helpersTestAssertJSONEqual(t, compactFields(json.RawMessage(`{"id":"1","description":"long","body":"long","comments":[1],"name":"Paper"}`)), `{"id":"1","name":"Paper"}`)

	formatTests := []struct {
		name string
		in   any
		want string
	}{
		{name: "string", in: "plain", want: "plain"},
		{name: "float integral", in: float64(3), want: "3"},
		{name: "float fractional", in: float64(3.14159), want: "3.14"},
		{name: "bool", in: true, want: "true"},
		{name: "nil", in: nil, want: ""},
		{name: "simple array", in: []any{"alpha", float64(2), true}, want: "alpha, 2, true"},
		{name: "iso date", in: "2026-06-15T12:34:56Z", want: "2026-06-15"},
	}
	for _, tt := range formatTests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatCellValue(tt.in); got != tt.want {
				t.Fatalf("formatCellValue(%#v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	obj := map[string]any{"Name": "Ada", "title": "Engineer", "id": "42"}
	if got := findField(obj, "title", "name"); got != "Engineer" {
		t.Fatalf("findField priority = %q, want Engineer", got)
	}
	if got := findField(obj, "missing"); got != "" {
		t.Fatalf("findField miss = %q, want empty", got)
	}
}

func TestFormatCellValueSanitizesTerminalControls(t *testing.T) {
	got := formatCellValue("safe \x1b]0;pwned\x07 title")
	if strings.Contains(got, "\x1b") || strings.Contains(got, "\x07") {
		t.Fatalf("formatCellValue left raw terminal controls in %q", got)
	}
}

func TestPrintAutoTableSanitizesTerminalControls(t *testing.T) {
	oldNoColor, oldHumanFriendly := noColor, humanFriendly
	noColor = true
	humanFriendly = false
	t.Cleanup(func() {
		noColor = oldNoColor
		humanFriendly = oldHumanFriendly
	})

	var out strings.Builder
	err := printAutoTable(&out, []map[string]any{
		{"key": "K1", "title": "safe \x1b]0;pwned\x07 title"},
	})
	if err != nil {
		t.Fatalf("printAutoTable returned error: %v", err)
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Fatalf("printAutoTable left raw ESC in %q", out.String())
	}
}

func TestPrintReadingListSanitizesTerminalControls(t *testing.T) {
	var out strings.Builder
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	err := printReadingList(cmd, readingListResult{
		Count:  1,
		Oldest: "2026-07-09",
		Items: []readingListItem{{
			Key:       "K1",
			Title:     "title \x1b]0;pwned\x07",
			Author:    "author \x1b[31m",
			Year:      "2026\x07",
			DateAdded: "2026-07-09\x1b",
			ItemType:  "journalArticle\x07",
		}},
	})
	if err != nil {
		t.Fatalf("printReadingList returned error: %v", err)
	}
	if strings.Contains(out.String(), "\x1b") || strings.Contains(out.String(), "\x07") {
		t.Fatalf("printReadingList left raw terminal controls in %q", out.String())
	}
}

// --agent/--compact must not collapse a Zotero
// resource envelope ({key, data:{...}}) down to {key}; it should keep the
// nested fields minus the verbose ones.
func TestCompactFieldsPreservesEnvelopeData(t *testing.T) {
	in := json.RawMessage(`[{"key":"K1","version":9,"data":{"itemType":"journalArticle","title":"T","DOI":"10.1","abstractNote":"long abstract","relations":{"a":"b"}}}]`)
	var items []map[string]any
	if err := json.Unmarshal(compactFields(in), &items); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}
	if items[0]["key"] != "K1" {
		t.Errorf("key not preserved: %v", items[0])
	}
	data, ok := items[0]["data"].(map[string]any)
	if !ok {
		t.Fatalf("envelope collapsed, data dropped: %v", items[0])
	}
	if data["title"] != "T" || data["itemType"] != "journalArticle" || data["DOI"] != "10.1" {
		t.Errorf("useful nested fields not preserved: %v", data)
	}
	if _, ok := data["abstractNote"]; ok {
		t.Errorf("verbose abstractNote not stripped: %v", data)
	}
	if _, ok := data["relations"]; ok {
		t.Errorf("verbose relations not stripped: %v", data)
	}
}

// TestCompactFieldsKeepsRowsTheAllowlistCannotMatch pins the other half of the
// same contract. The allowlist names item-ish fields, so a hand-written report
// row shares none of them and used to compact to {}: measured live on
// 2026-08-22, `zotio --agent journal list` answered ten empty objects and the
// whole audit trail was gone. Compaction must strip nothing rather than
// everything when it recognises no field in a row.
func TestCompactFieldsKeepsRowsTheAllowlistCannotMatch(t *testing.T) {
	in := json.RawMessage(`[{"run_id":"R1","operation":"import.apply","timestamp":"2026-08-22T09:29:51Z","ok":true,"summary":{"applied":1}}]`)
	var items []map[string]any
	if err := json.Unmarshal(compactFields(in), &items); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 row, got %d", len(items))
	}
	if len(items[0]) == 0 {
		t.Fatalf("compaction emptied a row the allowlist does not name: %v", items[0])
	}
	for _, field := range []string{"run_id", "operation", "timestamp", "ok", "summary"} {
		if _, ok := items[0][field]; !ok {
			t.Errorf("row lost %s: %v", field, items[0])
		}
	}

	// A row the allowlist DOES name still gets compacted, so the passthrough
	// cannot become a blanket exemption.
	mixed := json.RawMessage(`[{"key":"K1","title":"T","abstractNote":"long","relations":{"a":"b"}}]`)
	var got []map[string]any
	if err := json.Unmarshal(compactFields(mixed), &got); err != nil {
		t.Fatalf("unmarshal mixed: %v", err)
	}
	if _, ok := got[0]["abstractNote"]; ok {
		t.Errorf("passthrough leaked a verbose field on a matched row: %v", got[0])
	}
	if got[0]["key"] != "K1" || got[0]["title"] != "T" {
		t.Errorf("matched row lost its allowed fields: %v", got[0])
	}
}

func TestHelpersTruncateJSONArray(t *testing.T) {
	helpersTestAssertJSONEqual(t, truncateJSONArray(json.RawMessage(`[1,2,3,4]`), 2), `[1,2]`)

	short := json.RawMessage(`[1,2]`)
	if got := truncateJSONArray(short, 3); string(got) != string(short) {
		t.Fatalf("truncateJSONArray short = %s, want original %s", string(got), string(short))
	}

	nonArray := json.RawMessage(`{"id":1}`)
	if got := truncateJSONArray(nonArray, 1); string(got) != string(nonArray) {
		t.Fatalf("truncateJSONArray non-array = %s, want original %s", string(got), string(nonArray))
	}
}

func TestHelpersWriteNoopAndAPIErrorEnvelopeUseCommandWriters(t *testing.T) {
	var stdout, stderr bytes.Buffer
	flags := &rootFlags{}
	root := newRootCmd(flags)
	root.SilenceErrors = true
	root.SilenceUsage = true
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.AddCommand(&cobra.Command{
		Use: "capture-writers",
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := writeNoop(flags.out(), flags.errOut(), flags, "already_deleted", "already deleted (no-op)"); err != nil {
				return err
			}
			return classifyAPIError(helpersTestAPIError(409, "already exists"), flags)
		},
	})
	root.SetArgs([]string{"--json", "capture-writers"})

	processStdout, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatalf("create process stdout capture: %v", err)
	}
	processStderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("create process stderr capture: %v", err)
	}
	originalStdout, originalStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = processStdout, processStderr
	defer func() {
		os.Stdout, os.Stderr = originalStdout, originalStderr
		_ = processStdout.Close()
		_ = processStderr.Close()
	}()

	classified := root.Execute()
	if classified == nil {
		t.Fatal("capture-writers command succeeded, want API conflict")
	}

	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("command stdout lines = %q, want JSON no-op and API error envelope", stdout.String())
	}
	if lines[0] != `{"status":"noop","reason":"already_deleted"}` {
		t.Fatalf("JSON no-op = %q", lines[0])
	}
	var envelope struct {
		Error string `json:"error"`
		Code  int    `json:"code"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &envelope); err != nil {
		t.Fatalf("decode API error envelope %q: %v", lines[1], err)
	}
	if envelope.Error != classified.Error() || envelope.Code != ExitCode(classified) {
		t.Fatalf("API error envelope = %+v, want error=%q code=%d", envelope, classified, ExitCode(classified))
	}
	if stderr.Len() != 0 {
		t.Fatalf("command stderr = %q, want no JSON output", stderr.String())
	}
	flags.asJSON = false
	if err := writeNoop(root.OutOrStdout(), root.ErrOrStderr(), flags, "already_deleted", "already deleted (no-op)"); err != nil {
		t.Fatalf("write prose no-op: %v", err)
	}
	if stderr.String() != "already deleted (no-op)\n" {
		t.Fatalf("command stderr = %q, want prose no-op", stderr.String())
	}

	if err := processStdout.Sync(); err != nil {
		t.Fatalf("sync process stdout capture: %v", err)
	}
	if err := processStderr.Sync(); err != nil {
		t.Fatalf("sync process stderr capture: %v", err)
	}
	processOut, err := os.ReadFile(processStdout.Name())
	if err != nil {
		t.Fatalf("read process stdout capture: %v", err)
	}
	processErr, err := os.ReadFile(processStderr.Name())
	if err != nil {
		t.Fatalf("read process stderr capture: %v", err)
	}
	if len(processOut) != 0 || len(processErr) != 0 {
		t.Fatalf("process streams received output: stdout=%q stderr=%q", processOut, processErr)
	}
}

func TestHelpersWriterAccessorsFallBackForBareFlags(t *testing.T) {
	processStdout, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatalf("create process stdout capture: %v", err)
	}
	processStderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("create process stderr capture: %v", err)
	}
	originalStdout, originalStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = processStdout, processStderr
	defer func() {
		os.Stdout, os.Stderr = originalStdout, originalStderr
		_ = processStdout.Close()
		_ = processStderr.Close()
	}()

	flags := &rootFlags{asJSON: true}
	_ = classifyAPIError(helpersTestAPIError(409, "already exists"), flags)
	flags.asJSON = false
	if err := writeNoop(flags.out(), flags.errOut(), flags, "already_deleted", "already deleted (no-op)"); err != nil {
		t.Fatalf("write prose no-op: %v", err)
	}

	if err := processStdout.Sync(); err != nil {
		t.Fatalf("sync process stdout capture: %v", err)
	}
	if err := processStderr.Sync(); err != nil {
		t.Fatalf("sync process stderr capture: %v", err)
	}
	processOut, err := os.ReadFile(processStdout.Name())
	if err != nil {
		t.Fatalf("read process stdout capture: %v", err)
	}
	processErr, err := os.ReadFile(processStderr.Name())
	if err != nil {
		t.Fatalf("read process stderr capture: %v", err)
	}
	if len(processOut) == 0 || len(processErr) == 0 {
		t.Fatalf("bare flags did not use fallback writers: stdout=%q stderr=%q", processOut, processErr)
	}
}
