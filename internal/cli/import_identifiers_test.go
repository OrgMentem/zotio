// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// Verify PubMed eSummary records map to Zotero journalArticle fields.
func TestPubMedItemFromSummary(t *testing.T) {
	item := pubmedItemFromSummary(map[string]any{
		"title":           "PubMed Title",
		"authors":         []any{map[string]any{"name": "Lovelace A"}},
		"pubdate":         "1843",
		"fulljournalname": "Proceedings of Computation",
		"volume":          "1",
		"issue":           "2",
		"pages":           "3-9",
		"articleids":      []any{map[string]any{"idtype": "doi", "value": "10.1000/pmid"}},
	})

	if item["itemType"] != "journalArticle" || item["title"] != "PubMed Title" {
		t.Fatalf("pubmed item = %v", item)
	}
	if item["DOI"] != "10.1000/pmid" || item["date"] != "1843" {
		t.Errorf("pubmed DOI/date = %v/%v", item["DOI"], item["date"])
	}
	creators, ok := item["creators"].([]map[string]any)
	if !ok || len(creators) != 1 {
		t.Fatalf("creators = %v, want one creator", item["creators"])
	}
	if creators[0]["lastName"] != "Lovelace" || creators[0]["firstName"] != "A" {
		t.Errorf("creator[0] = %v", creators[0])
	}
}

// Verify arXiv Atom entries map to Zotero preprint fields.
func TestArxivItemFromEntry(t *testing.T) {
	item := arxivItemFromEntry(arxivEntry{
		Title:     "\n  ArXiv   Title  \n",
		Summary:   "\nAbstract text.\n",
		Published: "2024-01-02T03:04:05Z",
		Authors:   []arxivAuthor{{Name: "Ada Lovelace"}},
		DOI:       "10.48550/arXiv.2401.00001",
	}, "2401.00001")

	if item["itemType"] != "preprint" || item["title"] != "ArXiv Title" {
		t.Fatalf("arxiv item = %v", item)
	}
	if item["abstractNote"] != "Abstract text." || item["date"] != "2024-01-02" {
		t.Errorf("arxiv abstract/date = %v/%v", item["abstractNote"], item["date"])
	}
	if item["DOI"] != "10.48550/arXiv.2401.00001" || item["extra"] != "arXiv: 2401.00001" {
		t.Errorf("arxiv DOI/extra = %v/%v", item["DOI"], item["extra"])
	}
	creators, ok := item["creators"].([]map[string]any)
	if !ok || len(creators) != 1 {
		t.Fatalf("creators = %v, want one creator", item["creators"])
	}
}

// Verify Open Library records map to Zotero book fields.
func TestOpenLibraryItemFromData(t *testing.T) {
	item := openLibraryItemFromData(map[string]any{
		"title":           "Book Title",
		"authors":         []any{map[string]any{"name": "Grace Hopper"}},
		"publish_date":    "1952",
		"publishers":      []any{map[string]any{"name": "Compiler Press"}},
		"number_of_pages": float64(256),
	}, "9781234567890")

	if item["itemType"] != "book" || item["title"] != "Book Title" {
		t.Fatalf("isbn item = %v", item)
	}
	if item["ISBN"] != "9781234567890" || item["publisher"] != "Compiler Press" || item["numPages"] != float64(256) {
		t.Errorf("isbn mapped fields = %v", item)
	}
	creators, ok := item["creators"].([]map[string]any)
	if !ok || len(creators) != 1 {
		t.Fatalf("creators = %v, want one creator", item["creators"])
	}
}

// Smoke-test PubMed import --dry-run against a capped httptest response.
func TestImportPmidDryRun(t *testing.T) {
	allowPrivateMetadataProviderServers(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/esummary.fcgi" || r.URL.Query().Get("id") != "314159" {
			t.Errorf("PubMed request URL = %s", r.URL.String())
		}
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != "zotio/1.0.0" {
			t.Errorf("PubMed headers Accept=%q User-Agent=%q", r.Header.Get("Accept"), r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"uids":["314159"],"314159":{"title":"PubMed Dry Title","authors":[{"name":"Curie M"}],"pubdate":"1911","fulljournalname":"Journal of Radium","articleids":[{"idtype":"doi","value":"10.1000/radium"}]}}}`))
	}))
	defer srv.Close()
	withBase(t, &importPubMedBase, srv.URL)

	flags := &rootFlags{asJSON: true, timeout: 5 * time.Second, dryRun: true, maxChanges: -1}
	cmd := newImportPmidCmd(flags)
	cmd.SetArgs([]string{"314159"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import pmid --dry-run: %v", err)
	}

	env := decodeIdentifierPreview(t, out.Bytes())
	if env.Mode != "preview" || env.PreviewReason != "dry_run" {
		t.Fatalf("mode=%q reason=%q, want preview/dry_run", env.Mode, env.PreviewReason)
	}
	if env.Source != "PubMed (314159)" {
		t.Errorf("source = %q", env.Source)
	}
	if env.Item["itemType"] != "journalArticle" || env.Item["title"] != "PubMed Dry Title" {
		t.Fatalf("pubmed preview = %+v", env.Item)
	}
	if env.Item["DOI"] != "10.1000/radium" || env.Item["date"] != "1911" {
		t.Errorf("pubmed preview DOI/date = %v/%v", env.Item["DOI"], env.Item["date"])
	}
	assertIdentifierDryRunCreator(t, env.Item)
}

// Smoke-test arXiv import --dry-run against a capped httptest response.
func TestImportArxivDryRun(t *testing.T) {
	allowPrivateMetadataProviderServers(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/query" || r.URL.Query().Get("id_list") != "2401.00001" {
			t.Errorf("arXiv request URL = %s", r.URL.String())
		}
		if r.Header.Get("User-Agent") != "zotio/1.0.0" {
			t.Errorf("arXiv User-Agent = %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:arxiv="http://arxiv.org/schemas/atom">
  <entry>
    <id>http://arxiv.org/abs/2401.00001v1</id>
    <title>ArXiv Dry Title</title>
    <summary>
      Dry abstract.
    </summary>
    <published>2024-01-02T03:04:05Z</published>
    <author><name>Ada Lovelace</name></author>
    <arxiv:doi>10.48550/arXiv.2401.00001</arxiv:doi>
  </entry>
</feed>`))
	}))
	defer srv.Close()
	withBase(t, &importArxivBase, srv.URL)

	flags := &rootFlags{asJSON: true, timeout: 5 * time.Second, dryRun: true, maxChanges: -1}
	cmd := newImportArxivCmd(flags)
	cmd.SetArgs([]string{"2401.00001"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import arxiv --dry-run: %v", err)
	}

	env := decodeIdentifierPreview(t, out.Bytes())
	if env.Mode != "preview" || env.PreviewReason != "dry_run" {
		t.Fatalf("mode=%q reason=%q, want preview/dry_run", env.Mode, env.PreviewReason)
	}
	if env.Source != "arXiv (2401.00001)" {
		t.Errorf("source = %q", env.Source)
	}
	if env.Item["itemType"] != "preprint" || env.Item["title"] != "ArXiv Dry Title" {
		t.Fatalf("arxiv preview = %+v", env.Item)
	}
	if env.Item["DOI"] != "10.48550/arXiv.2401.00001" || env.Item["date"] != "2024-01-02" {
		t.Errorf("arxiv preview DOI/date = %v/%v", env.Item["DOI"], env.Item["date"])
	}
	assertIdentifierDryRunCreator(t, env.Item)
}

// Smoke-test ISBN import --dry-run against a capped httptest response.
func TestImportIsbnDryRun(t *testing.T) {
	allowPrivateMetadataProviderServers(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/books" || r.URL.Query().Get("bibkeys") != "ISBN:9781234567890" {
			t.Errorf("Open Library request URL = %s", r.URL.String())
		}
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != "zotio/1.0.0" {
			t.Errorf("Open Library headers Accept=%q User-Agent=%q", r.Header.Get("Accept"), r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ISBN:9781234567890":{"title":"ISBN Dry Title","authors":[{"name":"Octavia Butler"}],"publish_date":"1979","publishers":[{"name":"Doubleday"}],"number_of_pages":264}}`))
	}))
	defer srv.Close()
	withBase(t, &importOpenLibraryBase, srv.URL)

	flags := &rootFlags{asJSON: true, timeout: 5 * time.Second, dryRun: true, maxChanges: -1}
	cmd := newImportIsbnCmd(flags)
	cmd.SetArgs([]string{"9781234567890"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import isbn --dry-run: %v", err)
	}

	env := decodeIdentifierPreview(t, out.Bytes())
	if env.Mode != "preview" || env.PreviewReason != "dry_run" {
		t.Fatalf("mode=%q reason=%q, want preview/dry_run", env.Mode, env.PreviewReason)
	}
	if env.Source != "Open Library (9781234567890)" {
		t.Errorf("source = %q", env.Source)
	}
	if env.Item["itemType"] != "book" || env.Item["title"] != "ISBN Dry Title" {
		t.Fatalf("isbn preview = %+v", env.Item)
	}
	if env.Item["ISBN"] != "9781234567890" {
		t.Errorf("isbn preview ISBN = %v", env.Item["ISBN"])
	}
	assertIdentifierDryRunCreator(t, env.Item)
}

// The gate is the point of the fix: none of the one-item creators may write
// without --yes, and --agent only changes formatting, not consent.
func TestSingleItemCreatorsPreviewWithoutWriting(t *testing.T) {
	allowPrivateMetadataProviderServers(t)

	pubmed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"uids":["314159"],"314159":{"title":"Gated PubMed"}}}`))
	}))
	defer pubmed.Close()
	withBase(t, &importPubMedBase, pubmed.URL)

	arxiv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom"><entry><id>http://arxiv.org/abs/2401.00001v1</id><title>Gated ArXiv</title></entry></feed>`))
	}))
	defer arxiv.Close()
	withBase(t, &importArxivBase, arxiv.URL)

	openLibrary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ISBN:9781234567890":{"title":"Gated ISBN"}}`))
	}))
	defer openLibrary.Close()
	withBase(t, &importOpenLibraryBase, openLibrary.URL)

	commands := []struct {
		name string
		new  func(*rootFlags) *cobra.Command
		args []string
	}{
		{name: "import pmid", new: newImportPmidCmd, args: []string{"314159"}},
		{name: "import arxiv", new: newImportArxivCmd, args: []string{"2401.00001"}},
		{name: "import isbn", new: newImportIsbnCmd, args: []string{"9781234567890"}},
		{name: "items new", new: newItemsNewCmd, args: []string{"--item-type", "journalArticle"}},
	}
	modes := []struct {
		name       string
		flags      rootFlags
		wantReason string
	}{
		{name: "bare", flags: rootFlags{asJSON: true, maxChanges: -1}, wantReason: "default"},
		{name: "agent", flags: rootFlags{asJSON: true, agent: true, maxChanges: -1}, wantReason: "default"},
		{name: "dry-run beats yes", flags: rootFlags{asJSON: true, dryRun: true, yes: true, maxChanges: -1}, wantReason: "dry_run"},
	}

	for _, tc := range commands {
		for _, mode := range modes {
			t.Run(tc.name+"/"+mode.name, func(t *testing.T) {
				writes := 0
				zotero := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet {
						writes++
					}
					w.Header().Set("Content-Type", "application/json")
					// items new fetches its schema template with GET /items/new.
					_, _ = w.Write([]byte(`{"itemType":"journalArticle","title":"","creators":[],"collections":[]}`))
				}))
				defer zotero.Close()
				t.Setenv("ZOTERO_BASE_URL", zotero.URL+"/users/0")
				t.Setenv("ZOTERO_API_KEY", "gate-test")

				flags := mode.flags
				flags.timeout = 5 * time.Second
				cmd := tc.new(&flags)
				cmd.SilenceErrors, cmd.SilenceUsage = true, true
				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&bytes.Buffer{})
				cmd.SetArgs(tc.args)
				if err := cmd.Execute(); err != nil {
					t.Fatalf("%s preview: %v", tc.name, err)
				}
				if writes != 0 {
					t.Fatalf("%s issued %d write request(s) without --yes", tc.name, writes)
				}
				env := decodeIdentifierPreview(t, out.Bytes())
				if env.Mode != "preview" || env.PreviewReason != mode.wantReason {
					t.Fatalf("mode=%q reason=%q, want preview/%s", env.Mode, env.PreviewReason, mode.wantReason)
				}
			})
		}
	}
}
func TestIdentifierProvidersRedirectPolicy(t *testing.T) {
	allowPrivateMetadataProviderServers(t)

	type providerCase struct {
		name     string
		base     *string
		path     string
		response string
		fetch    func(*cobra.Command) error
	}
	providers := []providerCase{
		{
			name:     "PubMed",
			base:     &importPubMedBase,
			path:     "/esummary.fcgi",
			response: `{"result":{"123":{"title":"Redirected PubMed"}}}`,
			fetch: func(cmd *cobra.Command) error {
				_, err := fetchPubMedItem(cmd, time.Second, "123")
				return err
			},
		},
		{
			name:     "arXiv",
			base:     &importArxivBase,
			path:     "/query",
			response: `<feed xmlns="http://www.w3.org/2005/Atom"><entry><title>Redirected arXiv</title></entry></feed>`,
			fetch: func(cmd *cobra.Command) error {
				_, err := fetchArxivItem(cmd, time.Second, "2401.00001")
				return err
			},
		},
		{
			name:     "OpenLibrary",
			base:     &importOpenLibraryBase,
			path:     "/api/books",
			response: `{"ISBN:9781234567890":{"title":"Redirected Book"}}`,
			fetch: func(cmd *cobra.Command) error {
				_, err := fetchOpenLibraryItem(cmd, time.Second, "9781234567890")
				return err
			},
		},
	}

	for _, provider := range providers {
		provider := provider
		t.Run(provider.name+"/same-origin", func(t *testing.T) {
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == provider.path {
					http.Redirect(w, r, srv.URL+"/redirected", http.StatusFound)
					return
				}
				if r.URL.Path != "/redirected" {
					t.Errorf("redirected path = %q", r.URL.Path)
				}
				_, _ = w.Write([]byte(provider.response))
			}))
			defer srv.Close()
			withBase(t, provider.base, srv.URL)

			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			if err := provider.fetch(cmd); err != nil {
				t.Fatalf("same-origin redirect failed: %v", err)
			}
		})

		for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
			status := status
			t.Run(provider.name+"/cross-origin/"+http.StatusText(status), func(t *testing.T) {
				targetHits := 0
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					targetHits++
					_, _ = w.Write([]byte(provider.response))
				}))
				defer target.Close()
				source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, target.URL+"/metadata", status)
				}))
				defer source.Close()
				withBase(t, provider.base, source.URL)

				cmd := &cobra.Command{}
				cmd.SetContext(context.Background())
				if err := provider.fetch(cmd); err == nil {
					t.Fatal("cross-origin redirect succeeded")
				}
				if targetHits != 0 {
					t.Fatalf("cross-origin redirect hit target %d times", targetHits)
				}
			})
		}
	}
}

func allowPrivateMetadataProviderServers(t *testing.T) {
	t.Helper()
	old := allowPrivateOutboundForTests
	allowPrivateOutboundForTests = true
	t.Cleanup(func() { allowPrivateOutboundForTests = old })
}

type identifierPreview struct {
	Mode          string
	PreviewReason string
	Source        string
	Item          map[string]any
}

// decodeIdentifierPreview reads the shared mutation envelope the one-item
// importers emit and pulls out the proposed item and its metadata source.
func decodeIdentifierPreview(t *testing.T, data []byte) identifierPreview {
	t.Helper()
	var env struct {
		Mode          string `json:"mode"`
		PreviewReason string `json:"preview_reason"`
		Result        *any   `json:"result"`
		Plan          struct {
			Operations []struct {
				Changes []struct {
					Field string `json:"field"`
					Add   any    `json:"add"`
				} `json:"changes"`
			} `json:"operations"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("decode %q: %v", string(data), err)
	}
	if env.Result != nil {
		t.Fatalf("preview reported a result: %s", data)
	}
	if len(env.Plan.Operations) == 0 {
		t.Fatalf("no planned operation in %s", data)
	}
	out := identifierPreview{Mode: env.Mode, PreviewReason: env.PreviewReason}
	for _, change := range env.Plan.Operations[0].Changes {
		switch change.Field {
		case "source":
			out.Source, _ = change.Add.(string)
		case "item":
			out.Item, _ = change.Add.(map[string]any)
		}
	}
	if out.Item == nil {
		t.Fatalf("no previewed item in %s", data)
	}
	return out
}

// Assert dry-run JSON preserved at least one mapped Zotero creator.
func assertIdentifierDryRunCreator(t *testing.T, item map[string]any) {
	t.Helper()
	creators, ok := item["creators"].([]any)
	if !ok || len(creators) == 0 {
		t.Fatalf("creators = %v, want at least one creator", item["creators"])
	}
}

func TestPubmedInitialsShape(t *testing.T) {
	// Genuine initials (letters only, <=4, uppercase) should still be detected.
	for _, tok := range []string{"J", "J.", "JF", "J.F.", "J-F", "ABC", "ABCD"} {
		if !pubmedInitials(tok) {
			t.Errorf("pubmedInitials(%q) = false, want true", tok)
		}
	}
	// All-caps surnames or oversized/impure tokens must NOT be treated as initials.
	for _, tok := range []string{"SMITH", "JOHNSON", "ABCDE", "A1", "J1", "SMITH-JONES", "hello", ""} {
		if pubmedInitials(tok) {
			t.Errorf("pubmedInitials(%q) = true, want false", tok)
		}
	}
	// Lowercase and mixed case are not initials.
	for _, tok := range []string{"jf", "Jf", "Smith"} {
		if pubmedInitials(tok) {
			t.Errorf("pubmedInitials(%q) = true, want false (must be all uppercase)", tok)
		}
	}
}

func TestPubmedCreatorNamePubMedScope(t *testing.T) {
	// PubMed-style "Last FM" should normalize to "Last, FM" for parseCreatorName.
	cases := []struct {
		in        string
		wantLast  string
		wantFirst string
	}{
		{"Smith J", "Smith", "J"},
		{"Smith J.", "Smith", "J."},
		{"Smith JF", "Smith", "JF"},
		{"Smith J.F.", "Smith", "J.F."},
	}
	for _, tc := range cases {
		normalized := pubmedCreatorName(tc.in)
		c := parseCreatorName(normalized)
		if c == nil {
			t.Fatalf("parseCreatorName(pubmedCreatorName(%q)=%q) = nil", tc.in, normalized)
		}
		if c["lastName"] != tc.wantLast || c["firstName"] != tc.wantFirst {
			t.Errorf("pubmedCreatorName(%q)->%q parse = %v, want lastName=%q firstName=%q", tc.in, normalized, c, tc.wantLast, tc.wantFirst)
		}
	}
	// "John SMITH" must NOT be swapped — SMITH is a surname shape, not initials.
	noSwap := pubmedCreatorName("John SMITH")
	if noSwap != "John SMITH" {
		t.Errorf("pubmedCreatorName(%q) = %q, want unchanged (SMITH is not initials)", "John SMITH", noSwap)
	}
	c := parseCreatorName(noSwap)
	if c == nil {
		t.Fatalf("parseCreatorName(%q) = nil", noSwap)
	}
	// parseCreatorName on "John SMITH" (no comma) treats "John" as lastName via its own split;
	// the key assertion is that pubmedCreatorName did NOT rewrite to "John, SMITH".
	if c["lastName"] == "John" && c["firstName"] == "SMITH" {
		t.Errorf("unexpected swap for John SMITH: %v (pubmedCreatorName should not have inserted a comma)", c)
	}
}
