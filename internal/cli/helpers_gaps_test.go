// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean test-gaps bnt5): Cover pure CLI helper classification, filtering, formatting, pagination, and string behavior.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/spf13/cobra"

	"zotio/internal/client"
)

func helpersTestAPIError(status int, body string) error {
	return &client.APIError{Method: "GET", Path: "/items", StatusCode: status, Body: body}
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

func TestHelpersClassifyDeleteError(t *testing.T) {
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
			helpersTestAssertCLIError(t, classifyDeleteError(tt.err, nil), tt.want)
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

	if got := replacePathParam("/users/{user}/items/{item}", "item", "a/b c?x=1"); got != "/users/{user}/items/a/b c?x=1" {
		t.Fatalf("replacePathParam preserved raw replacement = %q", got)
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
	if raw, ok := rawAtPath(obj, "meta.next.cursor"); !ok || string(raw) != `"abc"` {
		t.Fatalf("rawAtPath nested cursor = %s, %t; want abc", string(raw), ok)
	}
	if raw, ok := rawAtPath(obj, "meta.missing.cursor"); ok || raw != nil {
		t.Fatalf("rawAtPath missing = %s, %t; want nil,false", string(raw), ok)
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

	helpersTestAssertJSONEqual(t, extractResponseData(json.RawMessage(`{"status":"success","data":[{"id":1}]}`)), `[{"id":1}]`)
	helpersTestAssertJSONEqual(t, extractResponseData(json.RawMessage(`{"data":[{"id":1}],"has_more":true}`)), `{"data":[{"id":1}],"has_more":true}`)

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

// PATCH(glean compact-envelope): --agent/--compact must not collapse a Zotero
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
