// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// importFileEnvelope is the mutation envelope subset these tests assert on.
type importFileEnvelope struct {
	OK            bool   `json:"ok"`
	Operation     string `json:"operation"`
	Mode          string `json:"mode"`
	PreviewReason string `json:"preview_reason"`
	Plan          struct {
		Summary struct {
			Selected int `json:"selected"`
			Planned  int `json:"planned"`
		} `json:"summary"`
		Operations []struct {
			ID      string `json:"id"`
			Key     string `json:"key"`
			Changes []struct {
				Field string `json:"field"`
				Add   any    `json:"add"`
			} `json:"changes"`
		} `json:"operations"`
	} `json:"plan"`
	Result *struct {
		Summary struct {
			Applied int `json:"applied"`
			Failed  int `json:"failed"`
		} `json:"summary"`
		Items []struct {
			Status string `json:"status"`
			Reason any    `json:"reason"`
		} `json:"items"`
	} `json:"result"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeImportFixture(t *testing.T, content string) string {
	t.Helper()
	filePath := filepath.Join(t.TempDir(), "items.bib")
	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		t.Fatalf("write import fixture: %v", err)
	}
	return filePath
}

func runImportFile(t *testing.T, flags *rootFlags, args ...string) (importFileEnvelope, string, error) {
	t.Helper()
	cmd := newImportFileCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err := cmd.Execute()

	var env importFileEnvelope
	if out.Len() > 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
			t.Fatalf("decode envelope %q: %v", out.String(), decodeErr)
		}
	}
	return env, out.String(), err
}

// The write gate is the point of the command: without --yes nothing is sent,
// including under --agent, which only changes formatting.
func TestImportFilePreviewsWithoutWriting(t *testing.T) {
	for _, tc := range []struct {
		name       string
		flags      rootFlags
		wantReason string
	}{
		{name: "bare", flags: rootFlags{asJSON: true, maxChanges: -1}, wantReason: "default"},
		{name: "agent", flags: rootFlags{asJSON: true, agent: true, maxChanges: -1}, wantReason: "default"},
		{name: "dry-run", flags: rootFlags{asJSON: true, dryRun: true, maxChanges: -1}, wantReason: "dry_run"},
		{name: "dry-run beats yes", flags: rootFlags{asJSON: true, dryRun: true, yes: true, maxChanges: -1}, wantReason: "dry_run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"successful":{"0":{"key":"NEWKEY11"}},"success":{},"unchanged":{},"failed":{}}`))
			}))
			defer srv.Close()
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

			filePath := writeImportFixture(t, "@article{example,\n  title = {Example}\n}\n")
			flags := tc.flags
			env, raw, err := runImportFile(t, &flags, filePath)
			if err != nil {
				t.Fatalf("import file preview: %v", err)
			}
			if requests != 0 {
				t.Fatalf("preview issued %d HTTP request(s); want none", requests)
			}
			if env.Mode != "preview" || env.PreviewReason != tc.wantReason {
				t.Fatalf("mode=%q reason=%q, want preview/%s (%s)", env.Mode, env.PreviewReason, tc.wantReason, raw)
			}
			if env.Result != nil {
				t.Fatalf("preview reported a result: %s", raw)
			}
			if env.Plan.Summary.Planned != 1 {
				t.Fatalf("planned = %d, want 1 (%s)", env.Plan.Summary.Planned, raw)
			}
			// Each parsed record is its own operation, carrying the parsed body.
			if len(env.Plan.Operations) != 1 || len(env.Plan.Operations[0].Changes) != 1 {
				t.Fatalf("operations = %+v, want one record", env.Plan.Operations)
			}
			if env.Plan.Operations[0].Key != "Example" {
				t.Fatalf("operation key = %q, want the parsed title", env.Plan.Operations[0].Key)
			}
			item, ok := env.Plan.Operations[0].Changes[0].Add.(map[string]any)
			if !ok || item["title"] != "Example" {
				t.Fatalf("previewed record = %v, want the parsed item body", env.Plan.Operations[0].Changes[0].Add)
			}
		})
	}
}

// --max-changes is charged per parsed record, so an oversized file is refused
// before any network call.
func TestImportFileRefusesRecordCountAboveMaxChanges(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	var content strings.Builder
	for i := range 3 {
		fmt.Fprintf(&content, "@article{example%d,\n  title = {Example %d}\n}\n", i, i)
	}
	filePath := writeImportFixture(t, content.String())

	env, raw, err := runImportFile(t, &rootFlags{asJSON: true, yes: true, maxChanges: 2}, filePath)
	if err == nil {
		t.Fatal("import error = nil, want a max-changes refusal")
	}
	if env.Error == nil || env.Error.Code != "max_changes_exceeded" {
		t.Fatalf("envelope = %s, want a max_changes_exceeded gate error", raw)
	}
	if !strings.Contains(env.Error.Message, "planned 3 change(s)") || !strings.Contains(env.Error.Message, "cap of 2") {
		t.Fatalf("gate message = %q, want the per-record count and cap", env.Error.Message)
	}
	if requests != 0 {
		t.Fatalf("refused import issued %d HTTP request(s); want none", requests)
	}
}

func TestImportFileReportsBatchWriteFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"successful":{},"success":{},"unchanged":{},"failed":{"0":{"code":400,"message":"title is required"}}}`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	filePath := writeImportFixture(t, "@article{example,\n  title = {Example}\n}\n")
	env, raw, err := runImportFile(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, filePath)
	if err == nil || ExitCode(err) != 13 {
		t.Fatalf("import error = %v, exit=%d; want degraded failure", err, ExitCode(err))
	}
	if env.Result == nil || env.Result.Summary.Failed != 1 || env.Result.Summary.Applied != 0 {
		t.Fatalf("result = %s, want the single record failed", raw)
	}
	reason, _ := env.Result.Items[0].Reason.(string)
	for _, want := range []string{"index 0", "code 400", "title is required"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("failure reason = %q, want %q", reason, want)
		}
	}
}

func TestImportFileOffsetsLaterBatchWriteFailureIndexes(t *testing.T) {
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		if requestCount == 2 {
			_, _ = w.Write([]byte(`{"successful":{},"success":{},"unchanged":{},"failed":{"0":{"code":400,"message":"title is required"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"successful":{"0":{"key":"NEWKEY11"}},"success":{},"unchanged":{},"failed":{}}`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	var content strings.Builder
	for i := range importFileBatchSize + 1 {
		fmt.Fprintf(&content, "@article{example%d,\n  title = {Example %d}\n}\n", i, i)
	}
	filePath := writeImportFixture(t, content.String())

	env, raw, err := runImportFile(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, filePath)
	if err == nil || ExitCode(err) != 13 {
		t.Fatalf("import error = %v, exit=%d; want degraded failure", err, ExitCode(err))
	}
	if requestCount != 2 {
		t.Fatalf("import requests = %d, want 2 batches", requestCount)
	}
	// The rejection lands on the record that produced it, and the 50 records
	// posted in the first batch are still reported as applied.
	if env.Result == nil || env.Result.Summary.Applied != importFileBatchSize || env.Result.Summary.Failed != 1 {
		t.Fatalf("result = %s, want %d applied and 1 failed", raw, importFileBatchSize)
	}
	if got := env.Result.Items[importFileBatchSize].Status; got != "failed" {
		t.Fatalf("record %d status = %q, want failed", importFileBatchSize, got)
	}
	reason, _ := env.Result.Items[importFileBatchSize].Reason.(string)
	if !strings.Contains(reason, "index 50") {
		t.Fatalf("failure reason = %q, want source-relative index 50", reason)
	}
}

func TestImportFilePreservesNonNumericBatchWriteFailureIndexes(t *testing.T) {
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		if requestCount == 2 {
			_, _ = w.Write([]byte(`{"successful":{},"success":{},"unchanged":{},"failed":{"unexpected":{"code":400,"message":"title is required"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"successful":{"0":{"key":"NEWKEY11"}},"success":{},"unchanged":{},"failed":{}}`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	var content strings.Builder
	for i := range importFileBatchSize + 1 {
		fmt.Fprintf(&content, "@article{example%d,\n  title = {Example %d}\n}\n", i, i)
	}
	filePath := writeImportFixture(t, content.String())

	env, raw, err := runImportFile(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, filePath)
	if err == nil || ExitCode(err) != 13 {
		t.Fatalf("import error = %v, exit=%d; want degraded failure", err, ExitCode(err))
	}
	if requestCount != 2 {
		t.Fatalf("import requests = %d, want 2 batches", requestCount)
	}
	if env.Result == nil || env.Result.Summary.Failed != 1 {
		t.Fatalf("result = %s, want the non-numeric rejection reported once", raw)
	}
	// A non-numeric element index cannot name a record, so it is charged to the
	// batch's first record rather than dropped.
	reason, _ := env.Result.Items[importFileBatchSize].Reason.(string)
	if !strings.Contains(reason, "index unexpected") {
		t.Fatalf("failure reason = %q, want the preserved non-numeric index", reason)
	}
}

func TestImportFileReportsSuccessfulBatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"successful":{"0":{"key":"NEWKEY11"}},"success":{},"unchanged":{},"failed":{}}`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	filePath := writeImportFixture(t, "@article{example,\n  title = {Example}\n}\n")
	env, raw, err := runImportFile(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, filePath)
	if err != nil {
		t.Fatalf("import successful batch: %v", err)
	}
	if !env.OK || env.Mode != "apply" {
		t.Fatalf("envelope = %s, want an applied run", raw)
	}
	if env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Summary.Failed != 0 {
		t.Fatalf("result = %s, want one applied batch", raw)
	}
}

func TestImportFileRejectsCSLJSONWithoutConnector(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "items.json")
	if err := os.WriteFile(filePath, []byte(`[{"type":"article-journal","title":"Example"}]`), 0o600); err != nil {
		t.Fatalf("write CSL JSON fixture: %v", err)
	}

	cmd := newImportFileCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"--format", "csljson", filePath})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--via connector") {
		t.Fatalf("CSL JSON error = %v, want translator guidance via --via connector", err)
	}
}
func TestImportFile_RIS(t *testing.T) {
	t.Run("dispatchThroughParseImportFileItems", func(t *testing.T) {
		ris := "TY  - JOUR\nTI  - Dispatch Title\nAU  - Doe, John\nPY  - 2020/05/10\nER  -\n"
		items, err := parseImportFileItems(ris, "ris", "")
		if err != nil {
			t.Fatalf("parseImportFileItems ris = %v, want nil", err)
		}
		if len(items) != 1 {
			t.Fatalf("items len = %d, want 1", len(items))
		}
		if got := items[0]["itemType"]; got != "journalArticle" {
			t.Fatalf("dispatch itemType = %v, want %v", got, "journalArticle")
		}
		if got := items[0]["title"]; got != "Dispatch Title" {
			t.Fatalf("dispatch title = %v, want %v", got, "Dispatch Title")
		}
		if got := items[0]["date"]; got != "2020" {
			t.Fatalf("dispatch date = %v, want %v", got, "2020")
		}
	})

	t.Run("itemTypeMapping", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			ty   string
			want string
		}{
			{name: "JOUR", ty: "JOUR", want: "journalArticle"},
			{name: "JFULL", ty: "JFULL", want: "journalArticle"},
			{name: "EJOUR", ty: "EJOUR", want: "journalArticle"},
			{name: "BOOK", ty: "BOOK", want: "book"},
			{name: "CHAP", ty: "CHAP", want: "bookSection"},
			{name: "CONF", ty: "CONF", want: "conferencePaper"},
			{name: "CPAPER", ty: "CPAPER", want: "conferencePaper"},
			{name: "THES", ty: "THES", want: "thesis"},
			{name: "RPRT", ty: "RPRT", want: "report"},
			{name: "ELEC", ty: "ELEC", want: "webpage"},
			{name: "WEB", ty: "WEB", want: "webpage"},
			{name: "unmapped fallback", ty: "VIDEO", want: "document"},
			{name: "unmapped fallback lower", ty: "video", want: "document"},
			{name: "empty maps to document", ty: "UNKNOWN_TYPE", want: "document"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ris := "TY  - " + tc.ty + "\nTI  - Title\nER  -\n"
				items, err := parseRISItems(ris, "")
				if err != nil {
					t.Fatalf("parseRISItems TY %q = %v, want nil", tc.ty, err)
				}
				if len(items) != 1 {
					t.Fatalf("items len = %d, want 1 for TY %q", len(items), tc.ty)
				}
				if got := items[0]["itemType"]; got != tc.want {
					t.Fatalf("TY %q itemType = %v, want %v", tc.ty, got, tc.want)
				}
			})
		}
	})

	t.Run("multiEntryFixture", func(t *testing.T) {
		ris := strings.Join([]string{
			"TY  - JOUR",
			"TI  - Journal Article Title",
			"AU  - Smith, John",
			"PY  - 2020/05/15",
			"JO  - Journal of Testing",
			"DO  - 10.1234/journal.doi",
			"AB  - Journal abstract",
			"UR  - https://example.com/jour",
			"ER  -",
			"",
			"TY  - BOOK",
			"T1  - Book Title",
			"A1  - Jane Doe",
			"Y1  - 2019",
			"JF  - Book Series",
			"N2  - Book abstract via N2",
			"UR  - https://example.com/book",
			"ER  -",
			"TY  - CONF",
			"TI  - Conference Paper Title",
			"AU  - Doe, Jane A.",
			"PY  - 2021/12/01",
			"T2  - Proceedings of Testing",
			"DO  - 10.1234/conf.doi",
			"ER  -",
			"TY  - CHAP",
			"TI  - Chapter Title",
			"AU  - First Middle Last",
			"T2  - Container Book",
			"ER  -",
			"TY  - THES",
			"TI  - Thesis Title",
			"AU  - Plato",
			"ER  -",
			"TY  - RPRT",
			"TI  - Report Title",
			"ER  -",
			"TY  - ELEC",
			"TI  - Webpage Title",
			"ER  -",
			"TY  - VIDEO",
			"TI  - Fallback Title",
			"ER  -",
		}, "\n") + "\n"

		items, err := parseRISItems(ris, "")
		if err != nil {
			t.Fatalf("parseRISItems multi = %v, want nil", err)
		}
		if len(items) != 8 {
			t.Fatalf("items len = %d, want 8", len(items))
		}
		wantTypes := []string{"journalArticle", "book", "conferencePaper", "bookSection", "thesis", "report", "webpage", "document"}
		for i, want := range wantTypes {
			if got := items[i]["itemType"]; got != want {
				t.Fatalf("item %d itemType = %v, want %v", i, got, want)
			}
		}
		// JO maps to publicationTitle (journal path).
		if got := items[0]["publicationTitle"]; got != "Journal of Testing" {
			t.Fatalf("JOUR publicationTitle = %v, want %v", got, "Journal of Testing")
		}
		if got := items[0]["DOI"]; got != "10.1234/journal.doi" {
			t.Fatalf("JOUR DOI = %v, want %v", got, "10.1234/journal.doi")
		}
		if got := items[0]["abstractNote"]; got != "Journal abstract" {
			t.Fatalf("JOUR abstractNote = %v, want %v", got, "Journal abstract")
		}
		if got := items[0]["url"]; got != "https://example.com/jour" {
			t.Fatalf("JOUR url = %v, want %v", got, "https://example.com/jour")
		}
		// JF maps to publicationTitle (book path).
		if got := items[1]["publicationTitle"]; got != "Book Series" {
			t.Fatalf("BOOK publicationTitle (JF) = %v, want %v", got, "Book Series")
		}
		if got := items[1]["abstractNote"]; got != "Book abstract via N2" {
			t.Fatalf("BOOK abstractNote (N2) = %v, want %v", got, "Book abstract via N2")
		}
		// T2 maps to publicationTitle (conference/chapter path).
		if got := items[2]["publicationTitle"]; got != "Proceedings of Testing" {
			t.Fatalf("CONF publicationTitle (T2) = %v, want %v", got, "Proceedings of Testing")
		}
		if got := items[3]["publicationTitle"]; got != "Container Book" {
			t.Fatalf("CHAP publicationTitle (T2) = %v, want %v", got, "Container Book")
		}
		// Title tags TI and T1 both map.
		if got := items[0]["title"]; got != "Journal Article Title" {
			t.Fatalf("TI title = %v, want %v", got, "Journal Article Title")
		}
		if got := items[1]["title"]; got != "Book Title" {
			t.Fatalf("T1 title = %v, want %v", got, "Book Title")
		}
		// Count helper for same fixture.
		if got := countImportFileRecords(ris, "ris"); got != 8 {
			t.Fatalf("countImportFileRecords ris = %d, want 8", got)
		}
		// Same through the extension dispatch.
		dispatched, err := parseImportFileItems(ris, "ris", "")
		if err != nil {
			t.Fatalf("parseImportFileItems multi = %v, want nil", err)
		}
		if len(dispatched) != 8 {
			t.Fatalf("dispatched len = %d, want 8", len(dispatched))
		}
		for i, want := range wantTypes {
			if got := dispatched[i]["itemType"]; got != want {
				t.Fatalf("dispatched item %d itemType = %v, want %v", i, got, want)
			}
		}
	})

	t.Run("titleTagsBoth", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			field string
		}{
			{name: "TI", field: "TI"},
			{name: "T1", field: "T1"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ris := "TY  - JOUR\n" + tc.field + "  - Title via " + tc.field + "\nER  -\n"
				items, err := parseRISItems(ris, "")
				if err != nil {
					t.Fatalf("parseRISItems %s = %v, want nil", tc.field, err)
				}
				if len(items) != 1 {
					t.Fatalf("items len = %d, want 1", len(items))
				}
				if got := items[0]["title"]; got != "Title via "+tc.field {
					t.Fatalf("title via %s = %v, want %v", tc.field, got, "Title via "+tc.field)
				}
			})
		}
	})

	t.Run("authorTagsBoth", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			tag  string
		}{
			{name: "AU", tag: "AU"},
			{name: "A1", tag: "A1"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ris := "TY  - JOUR\n" + tc.tag + "  - Smith, John\nER  -\n"
				items, err := parseRISItems(ris, "")
				if err != nil {
					t.Fatalf("parseRISItems %s = %v, want nil", tc.tag, err)
				}
				creators := risCreators(t, items[0])
				if len(creators) != 1 {
					t.Fatalf("creators len = %d, want 1 for %s", len(creators), tc.tag)
				}
				if got := creators[0]["lastName"]; got != "Smith" {
					t.Fatalf("lastName = %v, want %v", got, "Smith")
				}
				if got := creators[0]["firstName"]; got != "John" {
					t.Fatalf("firstName = %v, want %v", got, "John")
				}
				if got := creators[0]["creatorType"]; got != "author" {
					t.Fatalf("creatorType = %v, want %v", got, "author")
				}
			})
		}
	})

	t.Run("authorShapes", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			raw       string
			wantFirst string
			wantLast  string
		}{
			{name: "LastCommaFirst", raw: "Smith, John", wantFirst: "John", wantLast: "Smith"},
			{name: "LastCommaFirstWithMiddle", raw: "Doe, Jane A.", wantFirst: "Jane A.", wantLast: "Doe"},
			{name: "FirstLast", raw: "Jane Doe", wantFirst: "Jane", wantLast: "Doe"},
			{name: "FirstMiddleLast", raw: "First Middle Last", wantFirst: "First Middle", wantLast: "Last"},
			{name: "SingleName", raw: "Plato", wantFirst: "", wantLast: "Plato"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ris := "TY  - JOUR\nAU  - " + tc.raw + "\nER  -\n"
				items, err := parseRISItems(ris, "")
				if err != nil {
					t.Fatalf("parseRISItems author %q = %v, want nil", tc.raw, err)
				}
				creators := risCreators(t, items[0])
				if len(creators) != 1 {
					t.Fatalf("creators len = %d, want 1 for %q", len(creators), tc.raw)
				}
				gotLast := creators[0]["lastName"]
				if gotLast != tc.wantLast {
					t.Fatalf("lastName for %q = %v, want %v", tc.raw, gotLast, tc.wantLast)
				}
				gotFirst, _ := creators[0]["firstName"].(string)
				if gotFirst != tc.wantFirst {
					t.Fatalf("firstName for %q = %v, want %v", tc.raw, gotFirst, tc.wantFirst)
				}
				if got := creators[0]["creatorType"]; got != "author" {
					t.Fatalf("creatorType for %q = %v, want %v", tc.raw, got, "author")
				}
				// Field-by-field shape: only creatorType, lastName, and optionally firstName.
				if len(creators[0]) != 2 && tc.wantFirst == "" {
					// Single name: creatorType + lastName.
					if len(creators[0]) != 2 {
						t.Fatalf("creator fields for %q = %v, want 2 keys (creatorType, lastName)", tc.raw, creators[0])
					}
				}
				if tc.wantFirst != "" && len(creators[0]) != 3 {
					t.Fatalf("creator fields for %q = %v, want 3 keys (creatorType, firstName, lastName)", tc.raw, creators[0])
				}
			})
		}
	})

	t.Run("multipleAuthors", func(t *testing.T) {
		ris := "TY  - JOUR\nAU  - Smith, John\nAU  - Jane Doe\nAU  - Plato\nER  -\n"
		items, err := parseRISItems(ris, "")
		if err != nil {
			t.Fatalf("parseRISItems multi authors = %v, want nil", err)
		}
		creators := risCreators(t, items[0])
		if len(creators) != 3 {
			t.Fatalf("creators len = %d, want 3", len(creators))
		}
		if got := creators[0]["lastName"]; got != "Smith" {
			t.Fatalf("creators[0] lastName = %v, want %v", got, "Smith")
		}
		if got := creators[1]["firstName"]; got != "Jane" {
			t.Fatalf("creators[1] firstName = %v, want %v", got, "Jane")
		}
		if got := creators[1]["lastName"]; got != "Doe" {
			t.Fatalf("creators[1] lastName = %v, want %v", got, "Doe")
		}
		if got := creators[2]["lastName"]; got != "Plato" {
			t.Fatalf("creators[2] lastName = %v, want %v", got, "Plato")
		}
	})

	t.Run("dates", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			tag  string
			val  string
			want string
		}{
			{name: "PY plain year", tag: "PY", val: "2020", want: "2020"},
			{name: "Y1 plain year", tag: "Y1", val: "2019", want: "2019"},
			{name: "PY slash YYYY/MM/DD", tag: "PY", val: "2020/05/15", want: "2020"},
			{name: "Y1 slash YYYY/MM/DD", tag: "Y1", val: "2018/12/01", want: "2018"},
			{name: "PY slash YYYY/MM", tag: "PY", val: "2021/12", want: "2021"},
			{name: "PY slash with spaces", tag: "PY", val: " 2022 / 01 / 03 ", want: "2022"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ris := "TY  - JOUR\n" + tc.tag + "  - " + tc.val + "\nER  -\n"
				items, err := parseRISItems(ris, "")
				if err != nil {
					t.Fatalf("parseRISItems date %s %q = %v, want nil", tc.tag, tc.val, err)
				}
				if got := items[0]["date"]; got != tc.want {
					t.Fatalf("date for %s %q = %v, want %v", tc.tag, tc.val, got, tc.want)
				}
			})
		}
	})

	t.Run("containersDOIABN2UR", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			tag   string
			field string
			value string
		}{
			{name: "JO", tag: "JO", field: "publicationTitle", value: "Journal via JO"},
			{name: "JF", tag: "JF", field: "publicationTitle", value: "Journal via JF"},
			{name: "T2", tag: "T2", field: "publicationTitle", value: "Container via T2"},
			{name: "DO", tag: "DO", field: "DOI", value: "10.1000/test.doi"},
			{name: "AB", tag: "AB", field: "abstractNote", value: "Abstract via AB"},
			{name: "N2", tag: "N2", field: "abstractNote", value: "Abstract via N2"},
			{name: "UR", tag: "UR", field: "url", value: "https://example.com/ris"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ris := "TY  - JOUR\n" + tc.tag + "  - " + tc.value + "\nER  -\n"
				items, err := parseRISItems(ris, "")
				if err != nil {
					t.Fatalf("parseRISItems %s = %v, want nil", tc.tag, err)
				}
				if got := items[0][tc.field]; got != tc.value {
					t.Fatalf("%s -> %s = %v, want %v", tc.tag, tc.field, got, tc.value)
				}
			})
		}
	})

	t.Run("recordBoundariesER", func(t *testing.T) {
		// ER ends a record; missing final ER still flushes the last record.
		ris := "TY  - JOUR\nTI  - First\nER  -\nTY  - BOOK\nTI  - Second\nER  -\nTY  - CONF\nTI  - Third without ER\n"
		items, err := parseRISItems(ris, "")
		if err != nil {
			t.Fatalf("parseRISItems boundaries = %v, want nil", err)
		}
		if len(items) != 3 {
			t.Fatalf("items len = %d, want 3", len(items))
		}
		if got := items[0]["title"]; got != "First" {
			t.Fatalf("item 0 title = %v, want %v", got, "First")
		}
		if got := items[1]["title"]; got != "Second" {
			t.Fatalf("item 1 title = %v, want %v", got, "Second")
		}
		if got := items[2]["title"]; got != "Third without ER" {
			t.Fatalf("item 2 title = %v, want %v", got, "Third without ER")
		}
		if got := countImportFileRecords(ris, "ris"); got != 3 {
			t.Fatalf("countImportFileRecords = %d, want 3", got)
		}
		// Stray blank lines between records must not create phantom records.
		risBlanks := "TY  - JOUR\nTI  - A\nER  -\n\n\nTY  - BOOK\nTI  - B\nER  -\n"
		items, err = parseRISItems(risBlanks, "")
		if err != nil {
			t.Fatalf("parseRISItems blanks = %v, want nil", err)
		}
		if len(items) != 2 {
			t.Fatalf("blanks items len = %d, want 2", len(items))
		}
		if got := countImportFileRecords(risBlanks, "ris"); got != 2 {
			t.Fatalf("countImportFileRecords blanks = %d, want 2", got)
		}
	})

	t.Run("crlfAndBlankLines", func(t *testing.T) {
		risLF := strings.Join([]string{
			"TY  - JOUR",
			"TI  - First",
			"AU  - Smith, John",
			"ER  -",
			"",
			"TY  - BOOK",
			"T1  - Second",
			"A1  - Jane Doe",
			"ER  -",
			"TY  - CONF",
			"TI  - Third",
			"ER  -",
		}, "\n") + "\n"
		risCRLF := strings.ReplaceAll(risLF, "\n", "\r\n")
		// Insert an extra stray CRLF blank line between records to mimic reference-manager export.
		risCRLF = strings.ReplaceAll(risCRLF, "ER  -\r\nTY", "ER  -\r\n\r\nTY")

		itemsLF, err := parseRISItems(risLF, "")
		if err != nil {
			t.Fatalf("parseRISItems LF = %v, want nil", err)
		}
		itemsCRLF, err := parseRISItems(risCRLF, "")
		if err != nil {
			t.Fatalf("parseRISItems CRLF = %v, want nil", err)
		}
		if len(itemsCRLF) != 3 {
			t.Fatalf("CRLF items len = %d, want 3", len(itemsCRLF))
		}
		if len(itemsLF) != len(itemsCRLF) {
			t.Fatalf("LF len %d != CRLF len %d", len(itemsLF), len(itemsCRLF))
		}
		for i := range itemsLF {
			if got, want := itemsCRLF[i]["title"], itemsLF[i]["title"]; got != want {
				t.Fatalf("CRLF item %d title = %v, want %v", i, got, want)
			}
			if got, want := itemsCRLF[i]["itemType"], itemsLF[i]["itemType"]; got != want {
				t.Fatalf("CRLF item %d itemType = %v, want %v", i, got, want)
			}
		}
		if got := countImportFileRecords(risCRLF, "ris"); got != 3 {
			t.Fatalf("countImportFileRecords CRLF = %d, want 3", got)
		}
		// Also via dispatch.
		dispatched, err := parseImportFileItems(risCRLF, "ris", "")
		if err != nil {
			t.Fatalf("parseImportFileItems CRLF = %v, want nil", err)
		}
		if len(dispatched) != 3 {
			t.Fatalf("dispatched CRLF len = %d, want 3", len(dispatched))
		}
	})

	t.Run("collectionCarried", func(t *testing.T) {
		ris := "TY  - JOUR\nTI  - With Collection\nER  -\nTY  - BOOK\nTI  - Second\nER  -\n"
		const coll = "ABC12345"
		items, err := parseRISItems(ris, coll)
		if err != nil {
			t.Fatalf("parseRISItems collection = %v, want nil", err)
		}
		if len(items) != 2 {
			t.Fatalf("items len = %d, want 2", len(items))
		}
		for i, item := range items {
			cols := risCollections(t, item)
			if len(cols) != 1 || cols[0] != coll {
				t.Fatalf("item %d collections = %v, want [%v]", i, cols, coll)
			}
		}
		// Dispatch path also carries it.
		dispatched, err := parseImportFileItems(ris, "ris", coll)
		if err != nil {
			t.Fatalf("parseImportFileItems collection = %v, want nil", err)
		}
		for i, item := range dispatched {
			cols := risCollections(t, item)
			if len(cols) != 1 || cols[0] != coll {
				t.Fatalf("dispatched item %d collections = %v, want [%v]", i, cols, coll)
			}
		}
		// Empty collection adds nothing.
		empty, err := parseRISItems(ris, "")
		if err != nil {
			t.Fatalf("parseRISItems empty collection = %v, want nil", err)
		}
		for i, item := range empty {
			if _, ok := item["collections"]; ok {
				t.Fatalf("item %d has collections %v, want none for empty collection", i, item["collections"])
			}
		}
	})

	t.Run("countImportFileRecordsSameFixture", func(t *testing.T) {
		ris := "TY  - JOUR\nTI  - A\nER  -\nTY  - BOOK\nTI  - B\nER  -\nTY  - CONF\nTI  - C\nER  -\n"
		if got := countImportFileRecords(ris, "ris"); got != 3 {
			t.Fatalf("countImportFileRecords = %d, want 3", got)
		}
		if got := countImportFileRecords(ris, "RIS"); got != 3 {
			t.Fatalf("countImportFileRecords RIS upper = %d, want 3", got)
		}
		if got := countImportFileRecords("", "ris"); got != 0 {
			t.Fatalf("countImportFileRecords empty = %d, want 0", got)
		}
	})
}

func risCreators(t *testing.T, item map[string]any) []map[string]any {
	t.Helper()
	raw, ok := item["creators"]
	if !ok {
		t.Fatalf("item has no creators: %v", item)
	}
	switch v := raw.(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for i, el := range v {
			m, ok := el.(map[string]any)
			if !ok {
				t.Fatalf("creators[%d] = %T, want map[string]any", i, el)
			}
			out = append(out, m)
		}
		return out
	default:
		t.Fatalf("creators type = %T, want []map[string]any or []any", raw)
		return nil
	}
}

func risCollections(t *testing.T, item map[string]any) []string {
	t.Helper()
	raw, ok := item["collections"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for i, el := range v {
			s, ok := el.(string)
			if !ok {
				t.Fatalf("collections[%d] = %T, want string", i, el)
			}
			out = append(out, s)
		}
		return out
	default:
		t.Fatalf("collections type = %T, want []string or []any", raw)
		return nil
	}
}
