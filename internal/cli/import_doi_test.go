// Copyright 2026 OrgMentem. Licensed under MIT.

package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCrossRefItemFromWorkPlacesContainerInValidField(t *testing.T) {
	tests := []struct {
		name           string
		crossRefType   string
		itemType       string
		containerField string
		template       map[string]any
	}{
		{
			name:           "journal article",
			crossRefType:   "journal-article",
			itemType:       "journalArticle",
			containerField: "publicationTitle",
			template: map[string]any{
				"itemType": "", "title": "", "DOI": "", "publicationTitle": "",
			},
		},
		{
			name:           "conference paper",
			crossRefType:   "proceedings-article",
			itemType:       "conferencePaper",
			containerField: "proceedingsTitle",
			template: map[string]any{
				"itemType": "", "title": "", "DOI": "", "proceedingsTitle": "",
			},
		},
		{
			name:           "book section",
			crossRefType:   "book-chapter",
			itemType:       "bookSection",
			containerField: "bookTitle",
			template: map[string]any{
				"itemType": "", "title": "", "DOI": "", "bookTitle": "",
			},
		},
		{
			name:           "report",
			crossRefType:   "report",
			itemType:       "report",
			containerField: "extra",
			template: map[string]any{
				"itemType": "", "title": "", "DOI": "", "extra": "",
			},
		},
		{
			name:           "preprint",
			crossRefType:   "posted-content",
			itemType:       "preprint",
			containerField: "extra",
			template: map[string]any{
				"itemType": "", "title": "", "DOI": "", "extra": "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := crossRefItemFromWork(crossRefWork{
				Type:           tt.crossRefType,
				Title:          []string{"Container test"},
				DOI:            "10.1234/container",
				ContainerTitle: []string{"Container Name"},
			}, "10.1234/container")

			if got := item["itemType"]; got != tt.itemType {
				t.Fatalf("itemType = %q, want %q", got, tt.itemType)
			}
			wantContainer := "Container Name"
			if tt.containerField == "extra" {
				wantContainer = "Container: Container Name"
			}
			if got := item[tt.containerField]; got != wantContainer {
				t.Errorf("%s = %q, want %q", tt.containerField, got, wantContainer)
			}
			// Use the same template-backed validator used before an import POST.
			// The per-type template deliberately includes only the valid destination
			// field, so an accidental publicationTitle assignment fails here.
			if err := validateItemFields(tt.template, item); err != nil {
				t.Fatalf("validateItemFields(%v) = %v", item, err)
			}
		})
	}
}

func TestSetCrossRefContainerTitleUsesPublicationTitleForPeriodicals(t *testing.T) {
	for _, itemType := range []string{"journalArticle", "magazineArticle", "newspaperArticle"} {
		t.Run(itemType, func(t *testing.T) {
			item := map[string]any{"itemType": itemType}
			setCrossRefContainerTitle(item, "Periodical Name")

			if got := item["publicationTitle"]; got != "Periodical Name" {
				t.Errorf("publicationTitle = %q, want %q", got, "Periodical Name")
			}
			if err := validateItemFields(map[string]any{
				"itemType": "", "publicationTitle": "",
			}, item); err != nil {
				t.Fatalf("validateItemFields(%v) = %v", item, err)
			}
		})
	}
}

func TestCrossRefContainerTitleOmitsUnverifiedThesisUniversity(t *testing.T) {
	item := crossRefItemFromWork(crossRefWork{
		Type:           "dissertation",
		Title:          []string{"Thesis title"},
		DOI:            "10.1234/thesis",
		ContainerTitle: []string{"Container Name"},
	}, "10.1234/thesis")

	if _, ok := item["university"]; ok {
		t.Errorf("university = %q, want omitted because a CrossRef container is not verified as a university", item["university"])
	}
	if _, ok := item["extra"]; ok {
		t.Errorf("extra = %q, want omitted for a thesis container", item["extra"])
	}
	if err := validateItemFields(map[string]any{
		"itemType": "", "title": "", "DOI": "",
	}, item); err != nil {
		t.Fatalf("validateItemFields(%v) = %v", item, err)
	}
}

func TestImportDoiMaxChangesPreflightSkipsMetadataAndWrites(t *testing.T) {
	crossRefRequests := 0
	crossRef := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		crossRefRequests++
		http.Error(w, "CrossRef must not be called", http.StatusInternalServerError)
	}))
	t.Cleanup(crossRef.Close)
	withBase(t, &enrichCrossRefBase, crossRef.URL)

	zoteroPosts := 0
	zotero := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			zoteroPosts++
		}
		http.Error(w, "Zotero must not be called", http.StatusInternalServerError)
	}))
	t.Cleanup(zotero.Close)
	t.Setenv("ZOTERO_BASE_URL", zotero.URL+"/users/0")

	cmd := newImportDoiCmd(&rootFlags{asJSON: true, yes: true, via: "web", timeout: time.Second, maxChanges: 0})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"10.1234/safety"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("import doi --yes --max-changes 0 succeeded, want refusal")
	}
	if crossRefRequests != 0 {
		t.Fatalf("CrossRef requests after zero-cap refusal = %d, want 0", crossRefRequests)
	}
	if zoteroPosts != 0 {
		t.Fatalf("Zotero POSTs after zero-cap refusal = %d, want 0", zoteroPosts)
	}
}

func TestImportDoiRequiresYesToCreate(t *testing.T) {
	crossRef := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":{"type":"journal-article","title":["Safety Test"],"DOI":"10.1234/safety"}}`))
	}))
	t.Cleanup(crossRef.Close)
	withBase(t, &enrichCrossRefBase, crossRef.URL)

	createRequests := 0
	zotero := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/users/0/items" {
			http.NotFound(w, r)
			return
		}
		createRequests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":{"0":"NEWKEY11"},"successful":{},"unchanged":{},"failed":{}}`))
	}))
	t.Cleanup(zotero.Close)
	t.Setenv("ZOTERO_BASE_URL", zotero.URL+"/users/0")

	run := func(flags rootFlags) []byte {
		t.Helper()
		cmd := newImportDoiCmd(&flags)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(io.Discard)
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		cmd.SetArgs([]string{"10.1234/safety"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("import doi (flags=%+v): %v", flags, err)
		}
		return out.Bytes()
	}

	defaultFlags := rootFlags{asJSON: true, via: "web", timeout: time.Second}
	preview := run(defaultFlags)
	if createRequests != 0 {
		t.Fatalf("create requests without --yes = %d, want 0", createRequests)
	}
	var env struct {
		Mode string `json:"mode"`
		Plan struct {
			Operations []struct {
				Changes []struct {
					Field string          `json:"field"`
					Add   json.RawMessage `json:"add"`
				} `json:"changes"`
			} `json:"operations"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(preview, &env); err != nil {
		t.Fatalf("decode preview: %v; %s", err, preview)
	}
	if env.Mode != "preview" || len(env.Plan.Operations) != 1 {
		t.Fatalf("preview = %+v, want one planned create", env)
	}
	var gotDOI string
	var gotItem map[string]any
	for _, change := range env.Plan.Operations[0].Changes {
		switch change.Field {
		case "doi":
			if err := json.Unmarshal(change.Add, &gotDOI); err != nil {
				t.Fatalf("decode DOI change: %v", err)
			}
		case "item":
			if err := json.Unmarshal(change.Add, &gotItem); err != nil {
				t.Fatalf("decode item change: %v", err)
			}
		}
	}
	if gotDOI != "10.1234/safety" || gotItem["DOI"] != "10.1234/safety" {
		t.Fatalf("planned create DOI=%q item=%v, want DOI and item", gotDOI, gotItem)
	}

	var fetchPDFEnv struct {
		Plan struct {
			Summary struct {
				Planned int `json:"planned"`
			} `json:"summary"`
			Operations []struct {
				Kind    string `json:"kind"`
				Changes []struct {
					Field string          `json:"field"`
					Add   json.RawMessage `json:"add"`
				} `json:"changes"`
			} `json:"operations"`
		} `json:"plan"`
	}
	cmd := newImportDoiCmd(&rootFlags{asJSON: true, via: "web", timeout: time.Second, maxChanges: -1})
	var fetchPDFOut bytes.Buffer
	cmd.SetOut(&fetchPDFOut)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"10.1234/safety", "--fetch-pdf"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import doi --fetch-pdf preview: %v", err)
	}
	if err := json.Unmarshal(fetchPDFOut.Bytes(), &fetchPDFEnv); err != nil {
		t.Fatalf("decode --fetch-pdf preview: %v; %s", err, fetchPDFOut.Bytes())
	}
	if fetchPDFEnv.Plan.Summary.Planned != 2 || len(fetchPDFEnv.Plan.Operations) != 2 {
		t.Fatalf("--fetch-pdf plan = %+v, want create and conditional attachment writes", fetchPDFEnv.Plan)
	}
	if fetchPDFEnv.Plan.Operations[1].Kind != "attachment_create" {
		t.Fatalf("--fetch-pdf second operation kind = %q, want attachment_create", fetchPDFEnv.Plan.Operations[1].Kind)
	}
	var attachment map[string]string
	for _, change := range fetchPDFEnv.Plan.Operations[1].Changes {
		if change.Field == "attachment" {
			if err := json.Unmarshal(change.Add, &attachment); err != nil {
				t.Fatalf("decode attachment change: %v", err)
			}
		}
	}
	if attachment["source"] != "resolver" || attachment["condition"] == "" {
		t.Fatalf("attachment preview = %v, want conditional resolver attachment", attachment)
	}

	for _, previewFlags := range []rootFlags{
		{asJSON: true, yes: true, dryRun: true, via: "web", timeout: time.Second},
		{asJSON: true, agent: true, via: "web", timeout: time.Second},
	} {
		var previewEnv struct {
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal(run(previewFlags), &previewEnv); err != nil {
			t.Fatalf("decode preview (flags=%+v): %v", previewFlags, err)
		}
		if previewEnv.Mode != "preview" {
			t.Fatalf("preview mode (flags=%+v) = %q, want preview", previewFlags, previewEnv.Mode)
		}
	}
	if createRequests != 0 {
		t.Fatalf("create requests before --yes = %d, want 0", createRequests)
	}

	refusedCmd := newImportDoiCmd(&rootFlags{asJSON: true, yes: true, via: "web", timeout: time.Second, maxChanges: 0})
	refusedCmd.SetOut(io.Discard)
	refusedCmd.SetErr(io.Discard)
	refusedCmd.SilenceErrors = true
	refusedCmd.SilenceUsage = true
	refusedCmd.SetArgs([]string{"10.1234/safety"})
	if err := refusedCmd.Execute(); err == nil {
		t.Fatal("import doi --yes --max-changes 0 succeeded, want refusal")
	}
	if createRequests != 0 {
		t.Fatalf("create requests after zero-cap refusal = %d, want 0", createRequests)
	}

	apply := run(rootFlags{asJSON: true, yes: true, via: "web", timeout: time.Second, maxChanges: -1})
	if createRequests != 1 {
		t.Fatalf("create requests with --yes = %d, want 1", createRequests)
	}
	var applyEnv struct {
		OK        bool   `json:"ok"`
		Mode      string `json:"mode"`
		Operation string `json:"operation"`
		Result    *struct {
			Summary struct {
				Applied int `json:"applied"`
				Failed  int `json:"failed"`
			} `json:"summary"`
		} `json:"result"`
	}
	if err := json.Unmarshal(apply, &applyEnv); err != nil {
		t.Fatalf("decode apply output: %v; %s", err, apply)
	}
	if !applyEnv.OK || applyEnv.Mode != "apply" || applyEnv.Operation != "import.doi" || applyEnv.Result == nil || applyEnv.Result.Summary.Applied != 1 || applyEnv.Result.Summary.Failed != 0 {
		t.Fatalf("apply envelope = %+v, want successful import.doi apply", applyEnv)
	}
}

func TestImportDoiFetchPDFUsesResolvedConnectorRoute(t *testing.T) {
	crossRef := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":{"type":"journal-article","title":["Route Test"],"DOI":"10.1234/route"}}`))
	}))
	t.Cleanup(crossRef.Close)
	withBase(t, &enrichCrossRefBase, crossRef.URL)

	var pingRequests, saveItemRequests, resolverChecks, attachmentRequests, webPosts int
	listener, err := net.Listen("tcp", "127.0.0.1:23119")
	if err != nil {
		t.Skipf("port 23119 is unavailable: %v", err)
	}
	connectorServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connector/ping":
			pingRequests++
			if pingRequests > 1 {
				http.Error(w, "second route resolution must not happen", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		case "/connector/saveItems":
			saveItemRequests++
			w.WriteHeader(http.StatusCreated)
		case "/connector/hasAttachmentResolvers":
			resolverChecks++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("true"))
		case "/connector/saveAttachmentFromResolver":
			attachmentRequests++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`"Route Test PDF"`))
		case "/api/users/0/items":
			if r.Method == http.MethodPost {
				webPosts++
			}
			w.Header().Set("Last-Modified-Version", "0")
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	connectorServer.Listener = listener
	connectorServer.Start()
	t.Cleanup(connectorServer.Close)

	flags := &rootFlags{
		asJSON:     true,
		yes:        true,
		via:        "auto",
		timeout:    time.Second,
		maxChanges: -1,
		configPath: testConfigFile(t, "http://127.0.0.1:23119/api/users/0"),
	}
	cmd := newImportDoiCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"10.1234/route", "--fetch-pdf"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import doi --fetch-pdf through connector: %v; %s", err, out.String())
	}

	var env struct {
		OK     bool   `json:"ok"`
		Mode   string `json:"mode"`
		Result *struct {
			Summary struct {
				Applied int `json:"applied"`
			} `json:"summary"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode import output: %v; %s", err, out.Bytes())
	}
	if !env.OK || env.Mode != "apply" || env.Result == nil || env.Result.Summary.Applied != 2 {
		t.Fatalf("import envelope = %+v, want successful create and attachment", env)
	}
	if pingRequests != 1 {
		t.Fatalf("connector pings = %d, want one route resolution", pingRequests)
	}
	if saveItemRequests != 1 || resolverChecks != 1 || attachmentRequests != 1 {
		t.Fatalf("connector requests = saveItems:%d hasAttachmentResolvers:%d saveAttachmentFromResolver:%d, want one each", saveItemRequests, resolverChecks, attachmentRequests)
	}
	if webPosts != 0 {
		t.Fatalf("Web API item POSTs = %d, want connector-only import", webPosts)
	}
}
