// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Add PMID, arXiv, and ISBN import adapters.

package cli

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var importPubMedBase = "https://eutils.ncbi.nlm.nih.gov/entrez/eutils"
var importArxivBase = "https://export.arxiv.org/api"
var importOpenLibraryBase = "https://openlibrary.org"

type arxivFeed struct {
	Entries []arxivEntry `xml:"entry"`
}

type arxivEntry struct {
	Title     string        `xml:"title"`
	Summary   string        `xml:"summary"`
	Published string        `xml:"published"`
	Authors   []arxivAuthor `xml:"author"`
	ID        string        `xml:"id"`
	DOI       string        `xml:"http://arxiv.org/schemas/atom doi"`
}

type arxivAuthor struct {
	Name string `xml:"name"`
}

// Register PubMed ID import as a reviewable Zotero item create.
func newImportPmidCmd(flags *rootFlags) *cobra.Command {
	var flagCollection string
	var flagDryRun bool
	var flagFetchPDF bool

	cmd := &cobra.Command{
		Use:         "pmid <pmid>",
		Short:       "Import a journal article from PubMed metadata",
		Annotations: map[string]string{"zotio:method": "POST", "zotio:path": "/items"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}

			pmid := args[0]
			item, err := fetchPubMedItem(cmd, flags.timeout, pmid)
			if err != nil {
				return err
			}
			addImportCollection(item, flagCollection)

			if flags.dryRun || flagDryRun {
				return printImportDryRun(cmd, item, "PubMed ("+pmid+")", flags)
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}
			// Route item creates through the desktop connector when available.
			res, err := routeCreateItem(cmd.Context(), flags, c, item, itemCreateSourceURI(item), cmd.Flags().Changed("collection"))
			if err != nil {
				return err
			}
			if flagFetchPDF {
				if res.Via != "connector" {
					return preconditionErr(fmt.Errorf("--fetch-pdf requires the desktop connector; use --via connector"))
				}
				attachResolverPDF(cmd.Context(), flags, &res)
			}
			if res.Via == "connector" {
				refreshItemsFromLocalAPI(cmd.Context(), flags)
			}
			return printCreateResult(cmd, flags, res, res.WebData)
		},
	}
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Collection key to add the item to")
	cmd.Flags().BoolVar(&flagDryRun, "dry-run", false, "Preview import without sending requests")
	cmd.Flags().BoolVar(&flagFetchPDF, "fetch-pdf", false, "Attach an open-access PDF via Zotero's desktop resolver (requires --via connector)")

	return cmd
}

// Fetch PubMed eSummary JSON and map it to a Zotero journalArticle.
func fetchPubMedItem(cmd *cobra.Command, timeout time.Duration, pmid string) (map[string]any, error) {
	if pmid == "" {
		return nil, fmt.Errorf("PubMed ID is required")
	}

	endpoint := importPubMedBase + "/esummary.fcgi?db=pubmed&retmode=json&id=" + url.QueryEscape(pmid)
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("creating PubMed request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "zotio/1.0.0")

	resp, err := sameOriginExternalFetchHTTPClient(&http.Client{Timeout: timeout}, false).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching PubMed metadata: %w", err)
	}
	defer resp.Body.Close()

	body, err := readCappedExternalBody(resp.Body, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("reading PubMed response: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("PubMed ID not found: %s", pmid)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("PubMed request failed: HTTP %d", resp.StatusCode)
	}

	var decoded struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("parsing PubMed response: %w", err)
	}
	rec, ok := decoded.Result[pmid].(map[string]any)
	if !ok || len(rec) == 0 {
		return nil, fmt.Errorf("PubMed ID not found: %s", pmid)
	}
	return pubmedItemFromSummary(rec), nil
}

// Convert a PubMed eSummary record into a Zotero journalArticle map.
func pubmedItemFromSummary(rec map[string]any) map[string]any {
	item := map[string]any{"itemType": "journalArticle"}
	if title := importIdentifierString(rec["title"]); title != "" {
		item["title"] = title
	}
	if creators := pubmedCreators(rec["authors"]); len(creators) > 0 {
		item["creators"] = creators
	}
	if pubdate := importIdentifierString(rec["pubdate"]); pubdate != "" {
		item["date"] = pubdate
	}
	if journal := importIdentifierString(rec["fulljournalname"]); journal != "" {
		item["publicationTitle"] = journal
	} else if source := importIdentifierString(rec["source"]); source != "" {
		item["publicationTitle"] = source
	}
	if volume := importIdentifierString(rec["volume"]); volume != "" {
		item["volume"] = volume
	}
	if issue := importIdentifierString(rec["issue"]); issue != "" {
		item["issue"] = issue
	}
	if pages := importIdentifierString(rec["pages"]); pages != "" {
		item["pages"] = pages
	}
	if doi := pubmedDOI(rec["articleids"]); doi != "" {
		item["DOI"] = doi
	}
	return item
}

// Register arXiv import as a reviewable Zotero item create.
func newImportArxivCmd(flags *rootFlags) *cobra.Command {
	var flagCollection string
	var flagDryRun bool
	var flagFetchPDF bool

	cmd := &cobra.Command{
		Use:         "arxiv <id>",
		Short:       "Import a preprint from arXiv metadata",
		Annotations: map[string]string{"zotio:method": "POST", "zotio:path": "/items"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}

			id := args[0]
			item, err := fetchArxivItem(cmd, flags.timeout, id)
			if err != nil {
				return err
			}
			addImportCollection(item, flagCollection)

			if flags.dryRun || flagDryRun {
				return printImportDryRun(cmd, item, "arXiv ("+id+")", flags)
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}
			// Route item creates through the desktop connector when available.
			res, err := routeCreateItem(cmd.Context(), flags, c, item, itemCreateSourceURI(item), cmd.Flags().Changed("collection"))
			if err != nil {
				return err
			}
			if flagFetchPDF {
				if res.Via != "connector" {
					return preconditionErr(fmt.Errorf("--fetch-pdf requires the desktop connector; use --via connector"))
				}
				attachResolverPDF(cmd.Context(), flags, &res)
			}
			if res.Via == "connector" {
				refreshItemsFromLocalAPI(cmd.Context(), flags)
			}
			return printCreateResult(cmd, flags, res, res.WebData)
		},
	}
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Collection key to add the item to")
	cmd.Flags().BoolVar(&flagDryRun, "dry-run", false, "Preview import without sending requests")
	cmd.Flags().BoolVar(&flagFetchPDF, "fetch-pdf", false, "Attach an open-access PDF via Zotero's desktop resolver (requires --via connector)")

	return cmd
}

// Fetch arXiv Atom XML and map the first matching entry to a Zotero preprint.
func fetchArxivItem(cmd *cobra.Command, timeout time.Duration, id string) (map[string]any, error) {
	if id == "" {
		return nil, fmt.Errorf("arXiv ID is required")
	}

	endpoint := importArxivBase + "/query?id_list=" + url.QueryEscape(id)
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("creating arXiv request: %w", err)
	}
	req.Header.Set("Accept", "application/atom+xml, application/xml;q=0.9, */*;q=0.8")
	req.Header.Set("User-Agent", "zotio/1.0.0")

	resp, err := sameOriginExternalFetchHTTPClient(&http.Client{Timeout: timeout}, false).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching arXiv metadata: %w", err)
	}
	defer resp.Body.Close()

	body, err := readCappedExternalBody(resp.Body, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("reading arXiv response: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("arXiv ID not found: %s", id)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("arXiv request failed: HTTP %d", resp.StatusCode)
	}

	var feed arxivFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("parsing arXiv response: %w", err)
	}
	if len(feed.Entries) == 0 {
		return nil, fmt.Errorf("arXiv ID not found: %s", id)
	}
	return arxivItemFromEntry(feed.Entries[0], id), nil
}

// Convert an arXiv Atom entry into a Zotero preprint map.
func arxivItemFromEntry(entry arxivEntry, id string) map[string]any {
	item := map[string]any{"itemType": "preprint"}
	if title := strings.Join(strings.Fields(entry.Title), " "); title != "" {
		item["title"] = title
	}
	if abstract := strings.TrimSpace(entry.Summary); abstract != "" {
		item["abstractNote"] = abstract
	}
	if creators := arxivCreators(entry.Authors); len(creators) > 0 {
		item["creators"] = creators
	}
	if date := arxivDate(entry.Published); date != "" {
		item["date"] = date
	}
	if id != "" {
		item["archiveID"] = "arXiv:" + id
		item["repository"] = "arXiv"
		item["extra"] = "arXiv: " + id
	}
	if doi := strings.TrimSpace(entry.DOI); doi != "" {
		item["DOI"] = doi
	}
	return item
}

// Register ISBN import as a reviewable Zotero item create.
func newImportIsbnCmd(flags *rootFlags) *cobra.Command {
	var flagCollection string
	var flagDryRun bool
	var flagFetchPDF bool

	cmd := &cobra.Command{
		Use:         "isbn <isbn>",
		Short:       "Import a book from Open Library ISBN metadata",
		Annotations: map[string]string{"zotio:method": "POST", "zotio:path": "/items"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}

			isbn := args[0]
			item, err := fetchOpenLibraryItem(cmd, flags.timeout, isbn)
			if err != nil {
				return err
			}
			addImportCollection(item, flagCollection)

			if flags.dryRun || flagDryRun {
				return printImportDryRun(cmd, item, "Open Library ("+isbn+")", flags)
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}
			// Route item creates through the desktop connector when available.
			res, err := routeCreateItem(cmd.Context(), flags, c, item, itemCreateSourceURI(item), cmd.Flags().Changed("collection"))
			if err != nil {
				return err
			}
			if flagFetchPDF {
				if res.Via != "connector" {
					return preconditionErr(fmt.Errorf("--fetch-pdf requires the desktop connector; use --via connector"))
				}
				attachResolverPDF(cmd.Context(), flags, &res)
			}
			if res.Via == "connector" {
				refreshItemsFromLocalAPI(cmd.Context(), flags)
			}
			return printCreateResult(cmd, flags, res, res.WebData)
		},
	}
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Collection key to add the item to")
	cmd.Flags().BoolVar(&flagDryRun, "dry-run", false, "Preview import without sending requests")
	cmd.Flags().BoolVar(&flagFetchPDF, "fetch-pdf", false, "Attach an open-access PDF via Zotero's desktop resolver (requires --via connector)")

	return cmd
}

// Fetch Open Library ISBN JSON and map it to a Zotero book.
func fetchOpenLibraryItem(cmd *cobra.Command, timeout time.Duration, isbn string) (map[string]any, error) {
	if isbn == "" {
		return nil, fmt.Errorf("ISBN is required")
	}

	key := "ISBN:" + isbn
	endpoint := importOpenLibraryBase + "/api/books?format=json&jscmd=data&bibkeys=ISBN:" + url.QueryEscape(isbn)
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("creating Open Library request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "zotio/1.0.0")

	resp, err := sameOriginExternalFetchHTTPClient(&http.Client{Timeout: timeout}, false).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching Open Library metadata: %w", err)
	}
	defer resp.Body.Close()

	body, err := readCappedExternalBody(resp.Body, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("reading Open Library response: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("ISBN not found: %s", isbn)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("Open Library request failed: HTTP %d", resp.StatusCode)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("parsing Open Library response: %w", err)
	}
	rec, ok := decoded[key].(map[string]any)
	if !ok || len(rec) == 0 {
		return nil, fmt.Errorf("ISBN not found: %s", isbn)
	}
	return openLibraryItemFromData(rec, isbn), nil
}

// Convert an Open Library book record into a Zotero book map.
func openLibraryItemFromData(rec map[string]any, isbn string) map[string]any {
	item := map[string]any{"itemType": "book", "ISBN": isbn}
	if title := importIdentifierString(rec["title"]); title != "" {
		item["title"] = title
	}
	if creators := importIdentifierCreators(rec["authors"]); len(creators) > 0 {
		item["creators"] = creators
	}
	if publishDate := importIdentifierString(rec["publish_date"]); publishDate != "" {
		item["date"] = publishDate
	}
	if publisher := importIdentifierFirstName(rec["publishers"]); publisher != "" {
		item["publisher"] = publisher
	}
	if pages, ok := rec["number_of_pages"]; ok {
		item["numPages"] = pages
	}
	return item
}

// Parse PubMed's "Last FM" author names through the existing creator parser.
func pubmedCreators(raw any) []map[string]any {
	entries, ok := raw.([]any)
	if !ok {
		return nil
	}
	creators := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		rec, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if creator := parseCreatorName(pubmedCreatorName(importIdentifierString(rec["name"]))); creator != nil {
			creators = append(creators, creator)
		}
	}
	return creators
}

// Normalize PubMed surname-initial author strings before parsing.
func pubmedCreatorName(name string) string {
	fields := strings.Fields(name)
	if len(fields) < 2 || !pubmedInitials(fields[len(fields)-1]) {
		return name
	}
	return strings.Join(fields[:len(fields)-1], " ") + ", " + fields[len(fields)-1]
}

// Detect PubMed compact initial tokens such as "FM" or "F.M.".
func pubmedInitials(value string) bool {
	clean := strings.ReplaceAll(strings.ReplaceAll(value, ".", ""), "-", "")
	if clean == "" {
		return false
	}
	return clean == strings.ToUpper(clean)
}

// Extract creator maps from identifier provider arrays without introducing a second name convention.
func importIdentifierCreators(raw any) []map[string]any {
	entries, ok := raw.([]any)
	if !ok {
		return nil
	}
	creators := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		rec, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if creator := parseCreatorName(importIdentifierString(rec["name"])); creator != nil {
			creators = append(creators, creator)
		}
	}
	return creators
}

// Extract DOI values from PubMed articleids arrays.
func pubmedDOI(raw any) string {
	entries, ok := raw.([]any)
	if !ok {
		return ""
	}
	for _, entry := range entries {
		rec, ok := entry.(map[string]any)
		if !ok || !strings.EqualFold(importIdentifierString(rec["idtype"]), "doi") {
			continue
		}
		if value := importIdentifierString(rec["value"]); value != "" {
			return value
		}
		if id := importIdentifierString(rec["id"]); id != "" {
			return id
		}
	}
	return ""
}

// Convert arXiv author elements through the existing Zotero creator parser.
func arxivCreators(authors []arxivAuthor) []map[string]any {
	creators := make([]map[string]any, 0, len(authors))
	for _, author := range authors {
		if creator := parseCreatorName(author.Name); creator != nil {
			creators = append(creators, creator)
		}
	}
	return creators
}

// Normalize arXiv timestamps to Zotero date strings.
func arxivDate(published string) string {
	published = strings.TrimSpace(published)
	if len(published) >= 10 {
		return published[:10]
	}
	return published
}

// Extract the first Open Library-style name value from provider arrays.
func importIdentifierFirstName(raw any) string {
	entries, ok := raw.([]any)
	if !ok {
		return ""
	}
	for _, entry := range entries {
		rec, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if name := importIdentifierString(rec["name"]); name != "" {
			return name
		}
	}
	return ""
}

// Read string fields from decoded provider JSON maps.
func importIdentifierString(raw any) string {
	value, ok := raw.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}
