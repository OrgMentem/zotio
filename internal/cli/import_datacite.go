// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//
// DataCite is the second DOI registration agency zotio resolves against.
// CrossRef and DataCite partition the DOI space by registrant, not by subject:
// a DOI registered with one is a hard 404 at the other, so a CrossRef-only
// lookup can never resolve a DataCite DOI no matter how well-formed it is.
//
// The prefix that forced this is arXiv's own 10.48550/arXiv.*, which is
// registered with DataCite. `items preprint-check` already knew that (it
// deliberately skips CrossRef for arXiv self-DOIs, see arxivSelfDOIPrefix),
// but the import path did not, so every arXiv-DOI PDF resolved to
// "unresolved: fetching CrossRef metadata: HTTP 404" and blocked the import.
// Reported by papio, dev/field-report-2026-08-22-papio-arxiv.md finding 1.
//
// Resolution is a fallback rather than prefix routing. Routing by prefix would
// fix arXiv alone and leave every other DataCite registrant (Zenodo, Dryad,
// figshare, OSF, institutional repositories) failing the same way, and would
// need a prefix table that goes stale. Falling back when CrossRef reports no
// record costs one extra request only on the miss path, and generalises.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

var enrichDataCiteBase = "https://api.datacite.org"

type dataCiteResponse struct {
	Data struct {
		Attributes dataCiteAttributes `json:"attributes"`
	} `json:"data"`
}

type dataCiteAttributes struct {
	DOI             string             `json:"doi"`
	Titles          []dataCiteTitle    `json:"titles"`
	Creators        []dataCiteCreator  `json:"creators"`
	Publisher       dataCiteName       `json:"publisher"`
	Published       string             `json:"published"`
	PublicationYear int                `json:"publicationYear"`
	Types           dataCiteTypes      `json:"types"`
	URL             string             `json:"url"`
	Descriptions    []dataCiteAbstract `json:"descriptions"`
	Container       dataCiteContainer  `json:"container"`
}

type dataCiteTitle struct {
	Title string `json:"title"`
}

type dataCiteCreator struct {
	Name       string `json:"name"`
	NameType   string `json:"nameType"`
	GivenName  string `json:"givenName"`
	FamilyName string `json:"familyName"`
}

type dataCiteTypes struct {
	ResourceTypeGeneral string `json:"resourceTypeGeneral"`
}

type dataCiteAbstract struct {
	Description     string `json:"description"`
	DescriptionType string `json:"descriptionType"`
}

type dataCiteContainer struct {
	Title string `json:"title"`
}

// dataCiteName tolerates both shapes DataCite uses for a name-bearing field.
// Schema 4.5 turned `publisher` from a bare string into {"name": "..."}, and
// the REST API serves both depending on when the DOI was registered. A plain
// string field would make json.Unmarshal fail on the whole document, so an
// object-form publisher would not lose one field - it would fail the entire
// resolution with a type error.
type dataCiteName struct {
	Name string
}

func (n *dataCiteName) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if trimmed[0] == '"' {
		return json.Unmarshal(data, &n.Name)
	}
	var obj struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	n.Name = obj.Name
	return nil
}

// fetchDataCiteAttributes resolves a DOI against DataCite. A record with no
// title is treated as a miss: DataCite returns 200 with a sparse document for
// some registered-but-unpublished DOIs, and an item whose title is its own DOI
// is worse than an honest failure the caller can report.
func fetchDataCiteAttributes(ctx context.Context, httpClient *http.Client, doi string, providerCache *providerJSONCache) (dataCiteAttributes, error) {
	if doi == "" {
		return dataCiteAttributes{}, fmt.Errorf("DOI is required")
	}
	var decoded dataCiteResponse
	rawURL := enrichDataCiteBase + "/dois/" + url.PathEscape(doi)
	if err := getCappedProviderJSON(ctx, httpClient, providerDataCite, rawURL, providerCache, &decoded); err != nil {
		return dataCiteAttributes{}, err
	}
	if dataCiteFirstTitle(decoded.Data.Attributes.Titles) == "" {
		return dataCiteAttributes{}, fmt.Errorf("no title in DataCite record")
	}
	return decoded.Data.Attributes, nil
}

func dataCiteFirstTitle(titles []dataCiteTitle) string {
	for _, t := range titles {
		if title := strings.TrimSpace(t.Title); title != "" {
			return title
		}
	}
	return ""
}

// dataCiteItemFromAttributes maps a DataCite record onto a Zotero item.
//
// requestedDOI wins over the DOI in the record: DataCite normalises the suffix
// to lower case ("10.48550/arxiv.2605.10930"), while arXiv, Zotero users, and
// this repo's own fixtures all write "10.48550/arXiv.2605.10930". Echoing the
// caller's spelling also keeps a manifest entry's Identifier and its resolved
// item's DOI field identical, which the apply step compares.
func dataCiteItemFromAttributes(attrs dataCiteAttributes, requestedDOI string) map[string]any {
	item := map[string]any{
		"itemType": dataCiteItemType(attrs.Types.ResourceTypeGeneral),
		"title":    dataCiteFirstTitle(attrs.Titles),
	}

	doi := strings.TrimSpace(requestedDOI)
	if doi == "" {
		doi = strings.TrimSpace(attrs.DOI)
	}
	if doi != "" {
		item["DOI"] = doi
	}
	if creators := dataCiteCreators(attrs.Creators); len(creators) > 0 {
		item["creators"] = creators
	}
	if date := dataCiteDate(attrs); date != "" {
		item["date"] = date
	}
	if abstract := dataCiteAbstractText(attrs.Descriptions); abstract != "" {
		item["abstractNote"] = abstract
	}
	if rawURL := strings.TrimSpace(attrs.URL); rawURL != "" {
		item["url"] = rawURL
	}

	// An arXiv self-DOI carries the identifiers Zotero's preprint type has
	// dedicated fields for. Match arxivItemFromEntry exactly so a PDF imported
	// by DOI and the same paper imported by arXiv ID produce the same item.
	if id := arxivIDFromSelfDOI(doi); id != "" {
		item["archiveID"] = "arXiv:" + id
		item["repository"] = "arXiv"
		item["extra"] = "arXiv: " + id
	} else if publisher := strings.TrimSpace(attrs.Publisher.Name); publisher != "" {
		if item["itemType"] == "preprint" {
			item["repository"] = publisher
		} else {
			item["publisher"] = publisher
		}
	}

	if container := strings.TrimSpace(attrs.Container.Title); container != "" {
		setCrossRefContainerTitle(item, container)
	}
	return item
}

// arxivIDFromSelfDOI returns the bare arXiv ID for an arXiv self-DOI, or "".
// DOIs are case-insensitive, and this prefix is written both ways in the wild
// ("arXiv" by arXiv itself, "arxiv" by DataCite's own normalisation), so the
// comparison is folded.
func arxivIDFromSelfDOI(doi string) string {
	doi = strings.TrimSpace(doi)
	if !strings.HasPrefix(strings.ToLower(doi), arxivSelfDOIPrefix) {
		return ""
	}
	return strings.TrimSpace(doi[len(arxivSelfDOIPrefix):])
}

// dataCiteItemType maps DataCite's resourceTypeGeneral vocabulary onto Zotero
// item types. Unmapped kinds become "document", matching crossRefItemType:
// a wrong-but-specific type is harder for a user to notice than a generic one.
func dataCiteItemType(resourceTypeGeneral string) string {
	switch strings.ToLower(strings.TrimSpace(resourceTypeGeneral)) {
	case "preprint":
		return "preprint"
	case "journalarticle":
		return "journalArticle"
	case "conferencepaper", "conferenceproceeding":
		return "conferencePaper"
	case "book":
		return "book"
	case "bookchapter":
		return "bookSection"
	case "dataset":
		return "dataset"
	case "software", "computationalnotebook":
		return "computerProgram"
	case "report":
		return "report"
	case "dissertation":
		return "thesis"
	case "standard":
		return "standard"
	default:
		return "document"
	}
}

func dataCiteCreators(creators []dataCiteCreator) []map[string]any {
	out := make([]map[string]any, 0, len(creators))
	for _, c := range creators {
		creator := map[string]any{"creatorType": "author"}
		given := strings.TrimSpace(c.GivenName)
		family := strings.TrimSpace(c.FamilyName)
		name := strings.TrimSpace(c.Name)

		// An organisation is one indivisible name, not a split personal name.
		// Zotero models that as a single-field creator.
		if strings.EqualFold(strings.TrimSpace(c.NameType), "organizational") {
			if name == "" {
				continue
			}
			creator["name"] = name
			out = append(out, creator)
			continue
		}
		if given == "" && family == "" {
			// DataCite guarantees `name` but not the split parts. "Family,
			// Given" is its documented personal-name serialisation.
			if name == "" {
				continue
			}
			if comma := strings.Index(name, ","); comma >= 0 {
				family = strings.TrimSpace(name[:comma])
				given = strings.TrimSpace(name[comma+1:])
			} else {
				family = name
			}
		}
		if given != "" {
			creator["firstName"] = given
		}
		if family != "" {
			creator["lastName"] = family
		}
		if len(creator) > 1 {
			out = append(out, creator)
		}
	}
	return out
}

// dataCiteDate prefers the full `published` string, which can carry a month or
// a day, and falls back to the bare publicationYear.
func dataCiteDate(attrs dataCiteAttributes) string {
	if published := strings.TrimSpace(attrs.Published); published != "" {
		return published
	}
	if attrs.PublicationYear > 0 {
		return strconv.Itoa(attrs.PublicationYear)
	}
	return ""
}

func dataCiteAbstractText(descriptions []dataCiteAbstract) string {
	for _, d := range descriptions {
		if !strings.EqualFold(strings.TrimSpace(d.DescriptionType), "abstract") {
			continue
		}
		if abstract := strings.TrimSpace(d.Description); abstract != "" {
			return abstract
		}
	}
	return ""
}
