// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Cover hand-written arXiv publication detection.

package cli

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func arxivAtomFeedXML(content string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:arxiv="http://arxiv.org/schemas/atom">
  <entry>` + content + `</entry>
</feed>`
}

func crossrefWorkJSON(doi, venue string, year int) string {
	return `{"message":{"DOI":"` + doi + `","container-title":["` + venue + `"],"published":{"date-parts":[[` + strconv.Itoa(year) + `]]}}}`
}

func TestLookupCrossrefArxiv_ArxivDOIResolvesCrossrefWork(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Hostname() {
		case "export.arxiv.org":
			if got := req.URL.Query().Get("id_list"); got != "2301.00001" {
				t.Fatalf("arXiv id_list: want 2301.00001, got %q", got)
			}
			return testHTTPResponse(http.StatusOK, arxivAtomFeedXML(`<arxiv:doi>10.1145/1234567.890123</arxiv:doi>`)), nil
		case "api.crossref.org":
			if got := req.URL.EscapedPath(); got != "/works/10.1145%2F1234567.890123" {
				t.Fatalf("CrossRef path: want escaped DOI path, got %q", got)
			}
			return testHTTPResponse(http.StatusOK, crossrefWorkJSON("10.1145/1234567.890123", "Journal of Tests", 2024)), nil
		default:
			t.Fatalf("unexpected host %q", req.URL.Hostname())
			return nil, nil
		}
	})}

	match, found, err := lookupCrossrefArxiv(context.Background(), client, "2301.00001v2")
	if err != nil {
		t.Fatalf("lookupCrossrefArxiv returned error: %v", err)
	}
	if !found {
		t.Fatalf("expected published CrossRef match")
	}
	if match.DOI != "10.1145/1234567.890123" {
		t.Errorf("DOI: want 10.1145/1234567.890123, got %q", match.DOI)
	}
	if match.Venue != "Journal of Tests" {
		t.Errorf("Venue: want Journal of Tests, got %q", match.Venue)
	}
	if match.Year != 2024 {
		t.Errorf("Year: want 2024, got %d", match.Year)
	}
}

func TestLookupCrossrefArxiv_DOILinkResolvesCrossrefWork(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Hostname() {
		case "export.arxiv.org":
			return testHTTPResponse(http.StatusOK, arxivAtomFeedXML(`<link title="doi" href="https://doi.org/10.5555/test.doi"/>`)), nil
		case "api.crossref.org":
			return testHTTPResponse(http.StatusOK, crossrefWorkJSON("10.5555/test.doi", "DOI Link Journal", 2025)), nil
		default:
			t.Fatalf("unexpected host %q", req.URL.Hostname())
			return nil, nil
		}
	})}

	match, found, err := lookupCrossrefArxiv(context.Background(), client, "2301.00002")
	if err != nil {
		t.Fatalf("lookupCrossrefArxiv returned error: %v", err)
	}
	if !found {
		t.Fatalf("expected DOI link to resolve")
	}
	if match.DOI != "10.5555/test.doi" {
		t.Errorf("DOI: want 10.5555/test.doi, got %q", match.DOI)
	}
}

func TestLookupCrossrefArxiv_CrossrefNotFoundReturnsFalse(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Hostname() {
		case "export.arxiv.org":
			return testHTTPResponse(http.StatusOK, arxivAtomFeedXML(`<arxiv:doi>10.5555/missing.paper</arxiv:doi>`)), nil
		case "api.crossref.org":
			return testHTTPResponse(http.StatusNotFound, `{"status":"failed"}`), nil
		default:
			t.Fatalf("unexpected host %q", req.URL.Hostname())
			return nil, nil
		}
	})}

	_, found, err := lookupCrossrefArxiv(context.Background(), client, "2301.00003")
	if err != nil {
		t.Fatalf("lookupCrossrefArxiv returned error: %v", err)
	}
	if found {
		t.Fatalf("CrossRef 404 should not mark the preprint as published")
	}
}

func TestLookupCrossrefArxiv_MissingArxivDOIDoesNotPublish(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() == "api.crossref.org" {
			t.Fatalf("CrossRef should not be queried without an external DOI")
		}
		return testHTTPResponse(http.StatusOK, arxivAtomFeedXML(`<title>No external DOI yet</title>`)), nil
	})}

	_, found, err := lookupCrossrefArxiv(context.Background(), client, "2301.00004")
	if err != nil {
		t.Fatalf("lookupCrossrefArxiv returned error: %v", err)
	}
	if found {
		t.Fatalf("missing arXiv DOI should not mark the preprint as published")
	}
}

func TestLookupCrossrefArxiv_IgnoresArxivSelfDOI(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() == "api.crossref.org" {
			t.Fatalf("CrossRef should not be queried for arXiv DataCite self DOI")
		}
		return testHTTPResponse(http.StatusOK, arxivAtomFeedXML(`<arxiv:doi>10.48550/arXiv.2301.00005</arxiv:doi>`)), nil
	})}

	_, found, err := lookupCrossrefArxiv(context.Background(), client, "2301.00005")
	if err != nil {
		t.Fatalf("lookupCrossrefArxiv returned error: %v", err)
	}
	if found {
		t.Fatalf("arXiv DataCite DOI should not mark the preprint as externally published")
	}
}
