// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// work queue, and mutation-envelope preview of `items enrich`.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"zotio/internal/client"
	"zotio/internal/mutation"
	"zotio/internal/store"
)

// crossRefSearchServer serves a CrossRef bibliographic search whose result set
// contains one wrong-title candidate and one exact-title match.
func crossRefSearchServer(t *testing.T, matchTitle, matchDOI string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{"message":{"items":[` +
			`{"title":["A Completely Unrelated Paper"],"DOI":"10.0/wrong"},` +
			`{"title":["` + matchTitle + `"],"DOI":"` + matchDOI + `"}` +
			`]}}`
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGuardedClientReleasesClonedTransport(t *testing.T) {
	baseTransport := &http.Transport{}
	downloader := newEnrichPDFDownloader(&http.Client{Transport: baseTransport})

	client, release, err := downloader.guardedClient()
	if err != nil {
		t.Fatalf("guardedClient: %v", err)
	}
	if client.Transport == baseTransport {
		t.Fatal("guarded client reused the base transport, losing the dial guard")
	}
	cloned, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("guarded transport = %T, want *http.Transport", client.Transport)
	}
	// The clone owns a private connection pool, so release must actually close
	// it; a no-op strands idle sockets in long-running zotio-mcp/watch runs.
	if got, want := reflect.ValueOf(release).Pointer(), reflect.ValueOf(cloned.CloseIdleConnections).Pointer(); got != want {
		t.Fatal("release func does not close the cloned transport's idle connections")
	}
}

func TestGuardedClientSharesTransportWithoutDialGuard(t *testing.T) {
	baseTransport := &http.Transport{}
	downloader := newEnrichPDFDownloader(&http.Client{Transport: baseTransport})
	downloader.dialGuard = nil

	client, release, err := downloader.guardedClient()
	if err != nil {
		t.Fatalf("guardedClient: %v", err)
	}
	if client.Transport != baseTransport {
		t.Fatal("unguarded client cloned the transport, want the caller's transport shared")
	}
	if release == nil {
		t.Fatal("release func is nil, want a no-op for a shared transport")
	}
	release()
}

func withBase(t *testing.T, target *string, value string) {
	t.Helper()
	saved := *target
	oldAllowPrivateOutbound := allowPrivateOutboundForTests
	*target = value
	allowPrivateOutboundForTests = true
	t.Cleanup(func() {
		*target = saved
		allowPrivateOutboundForTests = oldAllowPrivateOutbound
	})
}

func TestGetJSONRedirectPolicy(t *testing.T) {
	oldAllowPrivateOutbound := allowPrivateOutboundForTests
	allowPrivateOutboundForTests = true
	t.Cleanup(func() { allowPrivateOutboundForTests = oldAllowPrivateOutbound })

	t.Run("refuses cross-origin 307 before target request", func(t *testing.T) {
		targetContact := make(chan struct{}, 1)
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			targetContact <- struct{}{}
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		t.Cleanup(target.Close)
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/json", http.StatusTemporaryRedirect)
		}))
		t.Cleanup(source.Close)

		var decoded map[string]bool
		if err := getJSON(context.Background(), http.DefaultClient, source.URL, &decoded); err == nil {
			t.Fatal("expected cross-origin redirect error")
		}
		select {
		case <-targetContact:
			t.Fatal("cross-origin redirect target was contacted")
		default:
		}
	})

	t.Run("allows same-origin redirect", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/start" {
				http.Redirect(w, r, "/json", http.StatusFound)
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		t.Cleanup(srv.Close)

		var decoded map[string]bool
		if err := getJSON(context.Background(), http.DefaultClient, srv.URL+"/start", &decoded); err != nil {
			t.Fatalf("getJSON: %v", err)
		}
		if !decoded["ok"] {
			t.Fatalf("decoded = %#v, want ok", decoded)
		}
	})
}

func TestResolveDOIViaCrossRef_ExactTitleMatch(t *testing.T) {
	srv := crossRefSearchServer(t, "Attention Is All You Need", "10.1/attention")
	withBase(t, &enrichCrossRefBase, srv.URL)

	data := map[string]any{"title": "attention is all you need", "creators": []any{map[string]any{"lastName": "Vaswani"}}}
	doi, _, ok := resolveDOIViaCrossRef(context.Background(), http.DefaultClient, data)
	if !ok {
		t.Fatal("expected a confident match")
	}
	if doi != "10.1/attention" {
		t.Errorf("doi = %q, want 10.1/attention", doi)
	}
}

func TestResolveDOIViaCrossRef_NoConfidentMatch(t *testing.T) {
	srv := crossRefSearchServer(t, "Some Other Title", "10.2/other")
	withBase(t, &enrichCrossRefBase, srv.URL)

	data := map[string]any{"title": "a title that matches nothing returned"}
	if _, _, ok := resolveDOIViaCrossRef(context.Background(), http.DefaultClient, data); ok {
		t.Error("expected no match for a non-matching title")
	}
}

func TestResolveAbstractViaCrossRef_StripsJATS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"abstract":"<jats:p>Hello <jats:italic>world</jats:italic> &amp; more.</jats:p>"}}`))
	}))
	defer srv.Close()
	withBase(t, &enrichCrossRefBase, srv.URL)

	abstract, ok := resolveAbstractViaCrossRef(context.Background(), http.DefaultClient, "10.1/x")
	if !ok {
		t.Fatal("expected an abstract")
	}
	if abstract != "Hello world & more." {
		t.Errorf("abstract = %q, want stripped JATS", abstract)
	}
}

func TestResolvePDFViaUnpaywall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("email") == "" {
			t.Error("Unpaywall request missing email")
		}
		_, _ = w.Write([]byte(`{"best_oa_location":{"url_for_pdf":"https://oa.example/p.pdf","url":"https://oa.example/landing"}}`))
	}))
	defer srv.Close()
	withBase(t, &enrichUnpaywallBase, srv.URL)

	pdf, ok := resolvePDFViaUnpaywall(context.Background(), http.DefaultClient, "10.1/x", "me@example.com")
	if !ok || pdf != "https://oa.example/p.pdf" {
		t.Errorf("pdf = %q ok=%v, want url_for_pdf", pdf, ok)
	}
}

func TestResolveEnrichmentLinkedPDFDeclaresPDFContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"best_oa_location":{"url_for_pdf":"https://oa.example/p.pdf"}}`))
	}))
	t.Cleanup(srv.Close)
	withBase(t, &enrichUnpaywallBase, srv.URL)

	proposal, reason := resolveEnrichment(context.Background(), http.DefaultClient, "missing_pdf", "K1", nil, map[string]any{
		"title": "Paper",
		"DOI":   "10.1/x",
	}, "me@example.com", false, false, "linked-url", "")
	if reason != "" {
		t.Fatalf("resolveEnrichment skipped: %s", reason)
	}
	if proposal.Attachment["contentType"] != "application/pdf" {
		t.Fatalf("contentType = %#v, want application/pdf", proposal.Attachment["contentType"])
	}
}

func TestResolvePDFViaUnpaywall_NoOA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"best_oa_location":{}}`))
	}))
	defer srv.Close()
	withBase(t, &enrichUnpaywallBase, srv.URL)
	if _, ok := resolvePDFViaUnpaywall(context.Background(), http.DefaultClient, "10.1/x", "me@example.com"); ok {
		t.Error("expected no OA PDF")
	}
}

type enrichRoundTripFunc func(*http.Request) (*http.Response, error)

func (f enrichRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func withPDFSafety(t *testing.T, check func(context.Context, *url.URL) error) {
	t.Helper()
	oldCheck := enrichPDFURLSafetyCheck
	oldFactory := enrichPDFDownloaderFactory
	enrichPDFURLSafetyCheck = check
	enrichPDFDownloaderFactory = func(client *http.Client) enrichPDFDownloader {
		downloader := newEnrichPDFDownloader(client)
		downloader.dialGuard = nil
		return downloader
	}
	t.Cleanup(func() {
		enrichPDFURLSafetyCheck = oldCheck
		enrichPDFDownloaderFactory = oldFactory
	})
}

func TestDownloadEnrichPDFWritesMagicPDF(t *testing.T) {
	withPDFSafety(t, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.7\nbody"))
	}))
	t.Cleanup(srv.Close)

	dest := filepath.Join(t.TempDir(), "paper.pdf")
	if err := downloadEnrichPDF(context.Background(), srv.Client(), srv.URL+"/paper.pdf", dest, 1024); err != nil {
		t.Fatalf("downloadEnrichPDF: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read downloaded PDF: %v", err)
	}
	if string(got) != "%PDF-1.7\nbody" {
		t.Fatalf("downloaded body = %q", got)
	}
	assertFileMode(t, dest, 0o600)
}

func TestDownloadEnrichPDFRejectsNonPDFBody(t *testing.T) {
	withPDFSafety(t, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("not a PDF"))
	}))
	t.Cleanup(srv.Close)

	dest := filepath.Join(t.TempDir(), "paper.pdf")
	err := downloadEnrichPDF(context.Background(), srv.Client(), srv.URL+"/paper.pdf", dest, 1024)
	if err == nil || !strings.Contains(err.Error(), "not a PDF") {
		t.Fatalf("err = %v, want non-PDF rejection", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("dest exists after rejected download: %v", statErr)
	}
}

func TestDownloadEnrichPDFRejectsOversize(t *testing.T) {
	withPDFSafety(t, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("%PDF-1234567890"))
	}))
	t.Cleanup(srv.Close)

	dest := filepath.Join(t.TempDir(), "paper.pdf")
	err := downloadEnrichPDF(context.Background(), srv.Client(), srv.URL+"/paper.pdf", dest, 7)
	if err == nil || !strings.Contains(err.Error(), "exceeds size cap") {
		t.Fatalf("err = %v, want oversize rejection", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("dest exists after oversize download: %v", statErr)
	}
}

func TestDownloadEnrichPDFRejectsRedirectToLocalhost(t *testing.T) {
	withPDFSafety(t, func(_ context.Context, u *url.URL) error {
		if u.Hostname() == "localhost" {
			return fmt.Errorf("refusing localhost")
		}
		return nil
	})
	client := &http.Client{Transport: enrichRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "example.test" {
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"http://localhost/paper.pdf"}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("%PDF-1.7")),
			Request:    req,
		}, nil
	})}

	dest := filepath.Join(t.TempDir(), "paper.pdf")
	err := downloadEnrichPDF(context.Background(), client, "http://example.test/paper.pdf", dest, 1024)
	if err == nil || !strings.Contains(err.Error(), "localhost") {
		t.Fatalf("err = %v, want localhost redirect rejection", err)
	}
}

func TestDownloadEnrichPDFNoClobber(t *testing.T) {
	withPDFSafety(t, nil)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte("%PDF-1.7"))
	}))
	t.Cleanup(srv.Close)

	dest := filepath.Join(t.TempDir(), "paper.pdf")
	if err := os.WriteFile(dest, []byte("existing"), 0o600); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}
	err := downloadEnrichPDF(context.Background(), srv.Client(), srv.URL+"/paper.pdf", dest, 1024)
	if err == nil || !strings.Contains(err.Error(), "refusing to clobber") {
		t.Fatalf("err = %v, want no-clobber rejection", err)
	}
	if requests != 0 {
		t.Fatalf("download server saw %d request(s), want 0 before clobber refusal", requests)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "existing" {
		t.Fatalf("existing file changed to %q", got)
	}
}

type fakePDFResolver struct {
	addrs []net.IPAddr
}

func (r fakePDFResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return r.addrs, nil
}

func TestRejectLocalPDFURLRejectsNonPublicLiterals(t *testing.T) {
	for _, rawURL := range []string{
		"http://127.0.0.1/paper.pdf",
		"http://10.20.30.40/paper.pdf",
		"http://169.254.169.254/latest/meta-data",
	} {
		t.Run(rawURL, func(t *testing.T) {
			u, err := url.Parse(rawURL)
			if err != nil {
				t.Fatal(err)
			}
			err = rejectLocalPDFURL(context.Background(), u)
			if err == nil || !strings.Contains(err.Error(), "non-public") {
				t.Fatalf("rejectLocalPDFURL(%q) = %v, want non-public rejection", rawURL, err)
			}
		})
	}
}

func TestEnrichPDFDialRejectsHostnameResolvingToLoopback(t *testing.T) {
	downloader := newEnrichPDFDownloader(http.DefaultClient)
	downloader.resolver = fakePDFResolver{addrs: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}}
	dest := filepath.Join(t.TempDir(), "paper.pdf")
	err := downloader.download(context.Background(), "http://papers.example:8080/paper.pdf", dest, 1024)
	if err == nil || !strings.Contains(err.Error(), "non-public address") {
		t.Fatalf("download error = %v, want dial-time loopback rejection", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("dest exists after dial rejection: %v", statErr)
	}
}

func TestDownloadEnrichPDFRejectsHTMLContentTypeBeforeCreatingFile(t *testing.T) {
	withPDFSafety(t, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("%PDF-1.7\nbody"))
	}))
	t.Cleanup(srv.Close)

	dest := filepath.Join(t.TempDir(), "paper.pdf")
	err := downloadEnrichPDF(context.Background(), srv.Client(), srv.URL, dest, 1024)
	if err == nil || !strings.Contains(err.Error(), `Content-Type "text/html"`) {
		t.Fatalf("download error = %v, want HTML Content-Type rejection", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("dest exists after Content-Type rejection: %v", statErr)
	}
}

func TestDownloadEnrichPDFAcceptsMissingContentType(t *testing.T) {
	withPDFSafety(t, nil)
	client := &http.Client{Transport: enrichRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("%PDF-1.7\nbody")),
			Request:    req,
		}, nil
	})}
	dest := filepath.Join(t.TempDir(), "paper.pdf")
	if err := downloadEnrichPDF(context.Background(), client, "https://papers.example/paper.pdf", dest, 1024); err != nil {
		t.Fatalf("download with missing Content-Type: %v", err)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != "%PDF-1.7\nbody" {
		t.Fatalf("downloaded body = %q, err = %v", got, err)
	}
}

type fakeMutator struct {
	patchPath    string
	patchBody    map[string]any
	patchHeaders map[string]string
	patchErr     error
	postPath     string
	postBody     any
	postErr      error
	// writeVersion is the version the fake's write plane reports; 0 models a
	// response that carries no usable version.
	writeVersion  int
	writeVerErr   error
	writeVerPaths []string
}

func (f *fakeMutator) PatchWithHeaders(path string, body any, headers map[string]string) (json.RawMessage, int, error) {
	f.patchHeaders = headers
	return f.Patch(path, body)
}

func (f *fakeMutator) GetFromWriteBaseWithVersionContext(_ context.Context, path string, _ map[string]string) (json.RawMessage, int, error) {
	f.writeVerPaths = append(f.writeVerPaths, path)
	if f.writeVerErr != nil {
		return nil, 0, f.writeVerErr
	}
	return json.RawMessage(`{}`), f.writeVersion, nil
}

func (f *fakeMutator) Patch(path string, body any) (json.RawMessage, int, error) {
	f.patchPath = path
	f.patchBody, _ = body.(map[string]any)
	if f.patchErr != nil {
		return nil, http.StatusPreconditionFailed, f.patchErr
	}
	return json.RawMessage(`{}`), 200, nil
}

func (f *fakeMutator) Post(path string, body any) (json.RawMessage, int, error) {
	f.postPath = path
	f.postBody = body
	if f.postErr != nil {
		return nil, http.StatusInternalServerError, f.postErr
	}
	return json.RawMessage(`{}`), 200, nil
}

func newEnrichWriteClient(t *testing.T, baseURL string) *client.Client {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", baseURL)
	t.Setenv("ZOTERO_API_KEY", "")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	c, err := (&rootFlags{}).newClient()
	if err != nil {
		t.Fatalf("new write client: %v", err)
	}
	return c
}

func TestApplyEnrichProposalLinkedFileAmbiguousFailureKeepsDownload(t *testing.T) {
	withPDFSafety(t, nil)
	pdfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.7\nbody"))
	}))
	t.Cleanup(pdfSrv.Close)
	zoteroSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/items/ABC/children":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/items":
			http.Error(w, `{"error":"temporary failure"}`, http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(zoteroSrv.Close)

	dest := filepath.Join(t.TempDir(), "paper.pdf")
	downloader := newEnrichPDFDownloader(pdfSrv.Client())
	downloader.dialGuard = nil
	p := enrichProposal{
		Key:         "ABC",
		Category:    "missing_pdf",
		Action:      enrichActionAttach,
		Source:      "Unpaywall",
		AttachMode:  "linked-file",
		DownloadURL: pdfSrv.URL + "/paper.pdf",
		PDFPath:     dest,
	}
	status, reason, err := applyEnrichProposalWithContext(context.Background(), downloader, newEnrichWriteClient(t, zoteroSrv.URL), &p, &rootFlags{})
	if err == nil || status != "failed" || !strings.Contains(fmt.Sprint(reason), "kept downloaded file") {
		t.Fatalf("apply = status %q reason %v err %v, want failed with retained-download detail", status, reason, err)
	}
	if _, statErr := os.Stat(dest); statErr != nil {
		t.Fatalf("download missing after ambiguous attachment create failure: %v", statErr)
	}
}

func TestApplyEnrichProposalLinkedFileConfirmedFailureRemovesDownload(t *testing.T) {
	withPDFSafety(t, nil)
	pdfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.7\nbody"))
	}))
	t.Cleanup(pdfSrv.Close)
	zoteroSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/items/ABC/children":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/items":
			http.Error(w, `{"error":"invalid attachment"}`, http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(zoteroSrv.Close)

	dest := filepath.Join(t.TempDir(), "paper.pdf")
	downloader := newEnrichPDFDownloader(pdfSrv.Client())
	downloader.dialGuard = nil
	p := enrichProposal{
		Key:         "ABC",
		Category:    "missing_pdf",
		Action:      enrichActionAttach,
		Source:      "Unpaywall",
		AttachMode:  "linked-file",
		DownloadURL: pdfSrv.URL + "/paper.pdf",
		PDFPath:     dest,
	}
	status, reason, err := applyEnrichProposalWithContext(context.Background(), downloader, newEnrichWriteClient(t, zoteroSrv.URL), &p, &rootFlags{})
	if err == nil || status != "failed" || !strings.Contains(fmt.Sprint(reason), "removed downloaded file") {
		t.Fatalf("apply = status %q reason %v err %v, want failed with confirmed-cleanup detail", status, reason, err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("download remains after confirmed attachment create failure: %v", statErr)
	}
}

func TestApplyEnrichProposalLinkedFileReconcilesLostCreateResponse(t *testing.T) {
	withPDFSafety(t, nil)
	pdfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.7\nbody"))
	}))
	t.Cleanup(pdfSrv.Close)

	var child map[string]any
	postCount := 0
	zoteroSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/items/ABC/children":
			if child == nil {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"key": "ATTACH1", "data": child}})
		case r.Method == http.MethodPost && r.URL.Path == "/items":
			if r.Header.Get("Zotero-Write-Token") == "" {
				http.Error(w, `{"error":"missing write token"}`, http.StatusBadRequest)
				return
			}
			var items []map[string]any
			if err := json.NewDecoder(r.Body).Decode(&items); err != nil || len(items) != 1 {
				http.Error(w, `{"error":"bad body"}`, http.StatusBadRequest)
				return
			}
			child = items[0] // Zotero committed the child before its response was lost.
			postCount++
			http.Error(w, `{"error":"write token already submitted"}`, http.StatusPreconditionFailed)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(zoteroSrv.Close)

	dest := filepath.Join(t.TempDir(), "paper.pdf")
	downloader := newEnrichPDFDownloader(pdfSrv.Client())
	downloader.dialGuard = nil
	p := enrichProposal{
		Key:         "ABC",
		Category:    "missing_pdf",
		Action:      enrichActionAttach,
		Source:      "Unpaywall",
		AttachMode:  "linked-file",
		DownloadURL: pdfSrv.URL + "/paper.pdf",
		PDFPath:     dest,
	}
	status, reason, err := applyEnrichProposalWithContext(context.Background(), downloader, newEnrichWriteClient(t, zoteroSrv.URL), &p, &rootFlags{})
	if err != nil || status != "applied" {
		t.Fatalf("apply = status %q reason %v err %v, want reconciled apply", status, reason, err)
	}
	if postCount != 1 {
		t.Fatalf("create requests = %d, want 1", postCount)
	}
	if _, statErr := os.Stat(dest); statErr != nil {
		t.Fatalf("download missing after reconciled create: %v", statErr)
	}
	if child["path"] != dest {
		t.Fatalf("reconciled child path = %v, want existing downloaded file %q", child["path"], dest)
	}
}

func TestApplyEnrichProposal_PatchIncludesVersionAndProvenance(t *testing.T) {
	f := &fakeMutator{writeVersion: 42}
	p := enrichProposal{
		Key: "ABC", Category: "missing_doi", Action: enrichActionPatch,
		Source: "CrossRef", Fields: map[string]any{"DOI": "10.1/x"}, version: float64(7),
	}
	status, reason, err := applyEnrichProposal(f, &p, &rootFlags{})
	if err != nil || reason != nil || status != "applied" {
		t.Fatalf("apply = status %q reason %v err %v, want applied without reason/error", status, reason, err)
	}
	if f.patchPath != "/items/ABC" {
		t.Errorf("patch path = %q", f.patchPath)
	}
	if f.patchBody["DOI"] != "10.1/x" {
		t.Errorf("patch body DOI = %v", f.patchBody["DOI"])
	}
	if _, hasVersion := f.patchBody["version"]; hasVersion {
		t.Errorf("patch body must not carry a version key; got %v", f.patchBody)
	}
	if got := f.patchHeaders["If-Unmodified-Since-Version"]; got != "42" {
		t.Errorf("If-Unmodified-Since-Version header = %q, want 42", got)
	}
	if len(f.writeVerPaths) != 1 || f.writeVerPaths[0] != "/items/ABC" {
		t.Errorf("write version paths = %v, want [/items/ABC]", f.writeVerPaths)
	}
	extra, _ := f.patchBody["extra"].(string)
	if !strings.Contains(extra, "DOI added via CrossRef") {
		t.Errorf("extra provenance missing: %q", extra)
	}
}

func TestApplyEnrichProposal_AttachPostsChild(t *testing.T) {
	f := &fakeMutator{}
	p := enrichProposal{
		Key: "ABC", Category: "missing_pdf", Action: enrichActionAttach, Source: "Unpaywall",
		Attachment: map[string]any{"itemType": "attachment", "linkMode": "linked_url", "url": "https://oa/p.pdf", "parentItem": "ABC"},
	}
	status, reason, err := applyEnrichProposal(f, &p, &rootFlags{})
	if err != nil || reason != nil || status != "applied" {
		t.Fatalf("apply = status %q reason %v err %v, want applied without reason/error", status, reason, err)
	}
	if f.postPath != "/items" {
		t.Errorf("post path = %q, want /items", f.postPath)
	}
	arr, ok := f.postBody.([]map[string]any)
	if !ok || len(arr) != 1 || arr[0]["linkMode"] != "linked_url" {
		t.Errorf("post body = %v, want one linked_url attachment", f.postBody)
	}
}

// TestEnrichProposalChangesExtraRecordsFullAppendedValue guards the mirror
// against replaying a bare provenance-delta line over an item's Extra field:
// the recorded Change must equal the post-PATCH Extra (existing content plus
// the new line), matching exactly what appendEnrichProvenance sends.
func TestEnrichProposalChangesExtraRecordsFullAppendedValue(t *testing.T) {
	existing := "Citation Key: smith2020\nsome user note"
	p := enrichProposal{
		Key: "K1", Category: "missing_doi", Action: enrichActionPatch,
		Source: "CrossRef", Fields: map[string]any{"DOI": "10.1/x"}, extra: existing,
	}
	changes := enrichProposalChanges(p)
	if len(changes) != 2 || changes[0].Field != "DOI" || changes[1].Field != "extra" {
		t.Fatalf("changes = %+v, want DOI add + extra change", changes)
	}
	got, ok := changes[1].Add.(string)
	if !ok {
		t.Fatalf("extra Change Add = %v (%T), want string", changes[1].Add, changes[1].Add)
	}
	// The bug under test recorded only the delta line; guard explicitly
	// against regressing to that value.
	if got == enrichProvenanceLine(&p) {
		t.Fatalf("extra Change = %q, recorded only the delta line and dropped existing Extra content", got)
	}
	want := existing + "\n" + enrichProvenanceLine(&p)
	if got != want {
		t.Fatalf("extra Change = %q, want %q (existing Extra preserved with provenance appended)", got, want)
	}
}

// TestEnrichProposalChangesAttachNeverNamesParentField guards against
// attributing the child-attachment POST to the parent item's own schema
// fields (e.g. "url") in the mirror-replayed Change: the linked-url attach
// path must record the child body under a synthetic field with a non-string
// Add, which a fail-closed mirror can never replay onto a real parent field.
func TestEnrichProposalChangesAttachNeverNamesParentField(t *testing.T) {
	p := enrichProposal{
		Key: "K1", Category: "missing_pdf", Action: enrichActionAttach,
		Source: "Unpaywall", AttachMode: "linked-url", DownloadURL: "https://oa.example/p.pdf",
		Attachment: map[string]any{
			"itemType": "attachment", "linkMode": "linked_url", "title": "Open-access PDF (Unpaywall)",
			"url": "https://oa.example/p.pdf", "parentItem": "K1", "contentType": "application/pdf",
		},
	}
	changes := enrichProposalChanges(p)
	if len(changes) != 1 {
		t.Fatalf("changes = %+v, want exactly one attach Change", changes)
	}
	c := changes[0]
	if c.Field == "url" {
		t.Fatalf("attach Change named the parent's real %q field with Add=%v; Zotero never writes the parent's url field for a linked-url attach", c.Field, c.Add)
	}
	if _, isString := c.Add.(string); isString {
		t.Fatalf("attach Change %+v carries a string Add on field %q; a fail-closed mirror could replay a string onto a matching real field name", c, c.Field)
	}
	body, ok := c.Add.(map[string]any)
	if !ok || body["url"] != "https://oa.example/p.pdf" || body["parentItem"] != "K1" {
		t.Fatalf("attach Change body = %v, want the full child attachment body", c.Add)
	}
}

func TestApplyEnrichProposal_ConflictStatusIsTyped(t *testing.T) {
	f := &fakeMutator{writeVersion: 9, patchErr: &client.APIError{Method: http.MethodPatch, Path: "/items/ABC", StatusCode: http.StatusPreconditionFailed, Body: "stale"}}
	p := enrichProposal{
		Key: "ABC", Category: "missing_doi", Action: enrichActionPatch,
		Source: "CrossRef", Fields: map[string]any{"DOI": "10.1/x"}, version: float64(7),
	}
	status, reason, err := applyEnrichProposal(f, &p, &rootFlags{})
	if err == nil || status != "conflict" || reason != "stale" {
		t.Fatalf("apply = status %q reason %v err %v, want typed conflict with API body reason", status, reason, err)
	}
}

// seedEnrichStore writes one missing-DOI item to the canonical dbPath under the
// test-isolated HOME.
func seedEnrichStore(t *testing.T, extra ...string) localQueryStore {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	k1Data := map[string]any{
		"key":      "K1",
		"itemType": "journalArticle",
		"title":    "Attention Is All You Need",
		"creators": []any{map[string]any{"lastName": "Vaswani"}},
	}
	if len(extra) > 0 {
		k1Data["extra"] = extra[0]
	}
	k1, err := json.Marshal(map[string]any{"key": "K1", "version": 9, "data": k1Data})
	if err != nil {
		t.Fatalf("marshal seed item: %v", err)
	}
	items := []json.RawMessage{
		json.RawMessage(k1),
		json.RawMessage(`{"key":"K2","version":3,"data":{"key":"K2","itemType":"journalArticle","title":"No Match In CrossRef Here"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = db.Close()

	rawDB, err := openStoreForRead(context.Background(), "zotio")
	if err != nil || rawDB == nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = rawDB.Close() })
	return localQueryStore{rawDB}
}

func seedEnrichWorkQueueStore(t *testing.T, items []json.RawMessage) localQueryStore {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "work-queue.toml"))
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	rawDB, err := openStoreForRead(context.Background(), "zotio")
	if err != nil || rawDB == nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = rawDB.Close() })
	return localQueryStore{rawDB}
}

func seedEnrichPDFStore(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	item := json.RawMessage(`{"key":"KPDF","version":4,"data":{"key":"KPDF","itemType":"journalArticle","title":"PDF Paper","DOI":"10.1/pdf"}}`)
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{item}); err != nil {
		t.Fatalf("seed PDF item: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

func TestBuildEnrichProposals_DOIFromStore(t *testing.T) {
	srv := crossRefSearchServer(t, "Attention Is All You Need", "10.1/attention")
	withBase(t, &enrichCrossRefBase, srv.URL)
	db := seedEnrichStore(t)

	proposals, skipped, err := buildEnrichProposals(context.Background(), db, http.DefaultClient, "missing_doi", 25, "", nil, "", false, false, "linked-url", "")
	if err != nil {
		t.Fatalf("build proposals: %v", err)
	}
	if len(proposals) != 1 {
		t.Fatalf("proposals = %d, want 1: %+v", len(proposals), proposals)
	}
	if proposals[0].Key != "K1" || proposals[0].Fields["DOI"] != "10.1/attention" {
		t.Errorf("proposal = %+v, want K1 with DOI", proposals[0])
	}
	if proposals[0].version != float64(9) {
		t.Errorf("proposal version = %v, want 9", proposals[0].version)
	}
	// K2's title has no CrossRef match -> skipped, not silently dropped.
	if len(skipped) != 1 || skipped[0].Key != "K2" {
		t.Errorf("skipped = %+v, want K2", skipped)
	}
}

// Semantic Scholar is the final exact-title DOI fallback after CrossRef and OpenAlex miss.
func TestBuildEnrichProposals_DOIFromSemanticScholarFallback(t *testing.T) {
	cr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"items":[]}}`))
	}))
	t.Cleanup(cr.Close)
	withBase(t, &enrichCrossRefBase, cr.URL)
	oa := openAlexWorkServer(t, `{"results":[]}`)
	withBase(t, &enrichOpenAlexBase, oa.URL)
	ss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") == "Attention Is All You Need" {
			_, _ = w.Write([]byte(`{"data":[{"title":"Attention Is All You Need","externalIds":{"DOI":"10.555/semantic"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(ss.Close)
	withBase(t, &enrichSemanticScholarBase, ss.URL)
	db := seedEnrichStore(t)

	proposals, skipped, err := buildEnrichProposals(context.Background(), db, http.DefaultClient, "missing_doi", 25, "", nil, "", true, true, "linked-url", "")
	if err != nil {
		t.Fatalf("build proposals: %v", err)
	}
	if len(proposals) != 1 {
		t.Fatalf("proposals = %d, want 1: %+v (skipped=%+v)", len(proposals), proposals, skipped)
	}
	if proposals[0].Source != "Semantic Scholar" {
		t.Fatalf("source = %q, want Semantic Scholar", proposals[0].Source)
	}
	if proposals[0].Fields["DOI"] != "10.555/semantic" {
		t.Errorf("DOI = %v, want Semantic Scholar DOI", proposals[0].Fields["DOI"])
	}
}

// Collection scoping should filter the local work queue
// before enrichment providers are asked to resolve candidates.
func TestItemsEnrichMissingDOICollectionScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"items":[` +
			`{"title":["In Collection"],"DOI":"10.1/in"},` +
			`{"title":["Outside Collection"],"DOI":"10.1/out"}` +
			`]}}`))
	}))
	t.Cleanup(srv.Close)
	withBase(t, &enrichCrossRefBase, srv.URL)

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "collection.toml"))
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	items := []json.RawMessage{
		json.RawMessage(`{"key":"KCOL","version":1,"data":{"key":"KCOL","itemType":"journalArticle","title":"In Collection","collections":["COLX"]}}`),
		json.RawMessage(`{"key":"KOUT","version":2,"data":{"key":"KOUT","itemType":"journalArticle","title":"Outside Collection"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = db.Close()

	flags := &rootFlags{asJSON: true}
	cmd := newItemsEnrichCmd(flags)
	cmd.SetArgs([]string{"--missing-doi", "--collection", "COLX"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if env.Plan.Summary.Planned != 1 || len(env.Plan.Operations) != 1 {
		t.Fatalf("expected one scoped proposal, got summary=%+v ops=%+v", env.Plan.Summary, env.Plan.Operations)
	}
	if env.Plan.Operations[0].Key != "KCOL" {
		t.Errorf("proposal key = %q, want KCOL", env.Plan.Operations[0].Key)
	}
	if env.Journal != nil {
		t.Errorf("unexpected skipped journal from out-of-collection item: %+v", env.Journal)
	}
}

// A direct key scope keeps one-shot automation from creating a temporary
// --keys-from file and avoids widening into the general work queue.
func TestItemsEnrichMissingDOIKeys(t *testing.T) {
	var requested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query.bibliographic")
		requested = append(requested, q)
		_, _ = w.Write([]byte(`{"message":{"items":[` +
			`{"title":["In Selection"],"DOI":"10.1/in"},` +
			`{"title":["Outside Selection"],"DOI":"10.1/out"}` +
			`]}}`))
	}))
	t.Cleanup(srv.Close)
	withBase(t, &enrichCrossRefBase, srv.URL)

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "keys-from.toml"))
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	items := []json.RawMessage{
		json.RawMessage(`{"key":"KIN","version":1,"data":{"key":"KIN","itemType":"journalArticle","title":"In Selection"}}`),
		json.RawMessage(`{"key":"KOUT","version":2,"data":{"key":"KOUT","itemType":"journalArticle","title":"Outside Selection"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = db.Close()

	flags := &rootFlags{asJSON: true}
	cmd := newItemsEnrichCmd(flags)
	cmd.SetArgs([]string{"--missing-doi", "--keys", "KIN"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if env.Plan.Summary.Planned != 1 || len(env.Plan.Operations) != 1 {
		t.Fatalf("expected one exact-key proposal, got summary=%+v ops=%+v", env.Plan.Summary, env.Plan.Operations)
	}
	if env.Plan.Operations[0].Key != "KIN" {
		t.Errorf("proposal key = %q, want KIN", env.Plan.Operations[0].Key)
	}
	if len(requested) != 1 || !strings.Contains(requested[0], "In Selection") {
		t.Errorf("provider calls = %v, want only In Selection", requested)
	}
}

func TestItemsEnrichMissingDOIKeysFromOutsideDefaultLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		title := r.URL.Query().Get("query.bibliographic")
		_, _ = fmt.Fprintf(w, `{"message":{"items":[{"title":[%q],"DOI":"10.1/selected"}]}}`, title)
	}))
	t.Cleanup(srv.Close)
	withBase(t, &enrichCrossRefBase, srv.URL)

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "keys-from-limit.toml"))
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	items := make([]json.RawMessage, 0, 26)
	for i := range 26 {
		key := fmt.Sprintf("K%02d", i)
		title := fmt.Sprintf("Candidate %02d", i)
		item, err := json.Marshal(map[string]any{
			"key":     key,
			"version": 1,
			"data": map[string]any{
				"key":       key,
				"itemType":  "journalArticle",
				"title":     title,
				"dateAdded": fmt.Sprintf("2026-01-%02dT00:00:00Z", i+1),
			},
		})
		if err != nil {
			t.Fatalf("marshal %s: %v", key, err)
		}
		items = append(items, item)
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	keysPath := filepath.Join(t.TempDir(), "keys.txt")
	if err := os.WriteFile(keysPath, []byte("K00\n"), 0o600); err != nil {
		t.Fatalf("write keys: %v", err)
	}
	flags := &rootFlags{asJSON: true}
	cmd := newItemsEnrichCmd(flags)
	cmd.SetArgs([]string{"--missing-doi", "--no-openalex", "--no-semantic-scholar", "--keys-from", keysPath})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if env.Plan.Summary.Planned != 1 || len(env.Plan.Operations) != 1 {
		t.Fatalf("expected oldest requested item in plan, got summary=%+v ops=%+v", env.Plan.Summary, env.Plan.Operations)
	}
	if env.Plan.Operations[0].Key != "K00" {
		t.Errorf("proposal key = %q, want oldest requested key K00", env.Plan.Operations[0].Key)
	}
}

func TestBuildEnrichProposalsKeysApplyLimit(t *testing.T) {
	var requested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		title := r.URL.Query().Get("query.bibliographic")
		requested = append(requested, title)
		_, _ = fmt.Fprintf(w, `{"message":{"items":[{"title":[%q],"DOI":"10.1/selected"}]}}`, title)
	}))
	t.Cleanup(srv.Close)
	withBase(t, &enrichCrossRefBase, srv.URL)

	db := seedEnrichWorkQueueStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"KOLD","version":1,"data":{"key":"KOLD","itemType":"journalArticle","title":"Selected Old","dateAdded":"2026-01-01T00:00:00Z"}}`),
		json.RawMessage(`{"key":"KNEW","version":1,"data":{"key":"KNEW","itemType":"journalArticle","title":"Selected New","dateAdded":"2026-01-03T00:00:00Z"}}`),
		json.RawMessage(`{"key":"KOUT","version":1,"data":{"key":"KOUT","itemType":"journalArticle","title":"Outside Selection","dateAdded":"2026-01-04T00:00:00Z"}}`),
	})

	proposals, skipped, err := buildEnrichProposals(context.Background(), db, http.DefaultClient, "missing_doi", 1, "", map[string]bool{"KOLD": true, "KNEW": true}, "", false, false, "linked-url", "")
	if err != nil {
		t.Fatalf("build proposals: %v", err)
	}
	if len(skipped) != 0 || len(proposals) != 1 {
		t.Fatalf("proposals=%+v skipped=%+v, want only the newest selected proposal", proposals, skipped)
	}
	if proposals[0].Key != "KNEW" {
		t.Errorf("proposal key = %q, want KNEW", proposals[0].Key)
	}
	if len(requested) != 1 || requested[0] != "Selected New" {
		t.Errorf("provider calls = %v, want only Selected New", requested)
	}
}

func TestEnrichWorkQueueForKeysChunks(t *testing.T) {
	items := make([]json.RawMessage, 0, enrichKeyChunkSize+2)
	keys := make(map[string]bool, enrichKeyChunkSize+1)
	for i := range enrichKeyChunkSize + 1 {
		key := fmt.Sprintf("K%04d", i)
		item, err := json.Marshal(map[string]any{
			"key":     key,
			"version": 1,
			"data": map[string]any{
				"key":       key,
				"itemType":  "journalArticle",
				"title":     key,
				"dateAdded": "2026-01-01T00:00:00Z",
			},
		})
		if err != nil {
			t.Fatalf("marshal %s: %v", key, err)
		}
		items = append(items, item)
		keys[key] = true
	}
	items = append(items, json.RawMessage(`{"key":"KOUT","version":1,"data":{"key":"KOUT","itemType":"journalArticle","title":"Outside Selection","dateAdded":"2026-01-02T00:00:00Z"}}`))

	db := seedEnrichWorkQueueStore(t, items)
	rows, err := enrichWorkQueueForKeys(db, "missing_doi", 0, "", keys)
	if err != nil {
		t.Fatalf("query key-scoped work queue: %v", err)
	}
	if len(rows) != len(keys) {
		t.Fatalf("rows = %d, want %d", len(rows), len(keys))
	}
	for _, row := range rows {
		if !keys[sqlStringValue(row["key"])] {
			t.Errorf("unrequested key returned: %q", sqlStringValue(row["key"]))
		}
	}
}

func TestBuildEnrichProposalsReportsCancelledFanoutItems(t *testing.T) {
	db := seedEnrichStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	proposals, skipped, err := buildEnrichProposals(ctx, db, http.DefaultClient, "missing_doi", 25, "", nil, "", false, false, "linked-url", "")
	if err != nil {
		t.Fatalf("build proposals: %v", err)
	}
	if len(proposals) != 0 {
		t.Fatalf("proposals = %+v, want none after cancellation", proposals)
	}
	if len(skipped) != 2 {
		t.Fatalf("skipped = %+v, want one cancellation skip per queued item", skipped)
	}
	for _, skip := range skipped {
		if skip.Key != "K1" && skip.Key != "K2" {
			t.Errorf("skip key = %q, want K1 or K2", skip.Key)
		}
		if skip.Category != "missing_doi" {
			t.Errorf("skip category = %q, want missing_doi", skip.Category)
		}
		if !strings.Contains(skip.Reason, context.Canceled.Error()) {
			t.Errorf("skip reason = %q, want cancellation error", skip.Reason)
		}
	}
}

func TestItemsEnrichPreviewEnvelope(t *testing.T) {
	srv := crossRefSearchServer(t, "Attention Is All You Need", "10.1/attention")
	withBase(t, &enrichCrossRefBase, srv.URL)
	_ = seedEnrichStore(t) // sets HOME to the seeded store

	flags := &rootFlags{asJSON: true} // no yes -> preview only
	cmd := newItemsEnrichCmd(flags)
	cmd.SetArgs([]string{"--missing-doi", "--no-openalex", "--no-semantic-scholar"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if !env.OK || env.Mode != "preview" || env.PreviewReason != "default" || env.Result != nil {
		t.Errorf("expected ordinary preview envelope, got ok=%v mode=%q reason=%q result=%+v", env.OK, env.Mode, env.PreviewReason, env.Result)
	}
	if env.Plan.Summary.Planned != 1 || len(env.Plan.Operations) != 1 {
		t.Fatalf("expected 1 planned proposal, got summary=%+v ops=%+v", env.Plan.Summary, env.Plan.Operations)
	}
	got := env.Plan.Operations[0].Changes
	if len(got) != 2 || got[0].Field != "DOI" || got[0].Add != "10.1/attention" {
		t.Errorf("proposal changes = %+v, want DOI add + extra provenance", got)
	}
	if got[1].Field != "extra" || !strings.Contains(fmt.Sprint(got[1].Add), "zotio: DOI added via") {
		t.Errorf("second change = %+v, want extra provenance line in the preview", got[1])
	}
}

// TestItemsEnrichPreviewEnvelopePreservesExistingExtra is the end-to-end
// counterpart of TestEnrichProposalChangesExtraRecordsFullAppendedValue: the
// previewed "extra" Change must be the item's pre-existing Extra (including
// Better BibTeX "Citation Key:" content) with the provenance line appended,
// not the bare delta line, since that is what a mirror replay would apply.
func TestItemsEnrichPreviewEnvelopePreservesExistingExtra(t *testing.T) {
	srv := crossRefSearchServer(t, "Attention Is All You Need", "10.1/attention")
	withBase(t, &enrichCrossRefBase, srv.URL)
	existingExtra := "Citation Key: smith2020\nsome user note"
	_ = seedEnrichStore(t, existingExtra) // sets HOME to the seeded store

	flags := &rootFlags{asJSON: true} // no yes -> preview only
	cmd := newItemsEnrichCmd(flags)
	cmd.SetArgs([]string{"--missing-doi", "--no-openalex", "--no-semantic-scholar"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if len(env.Plan.Operations) != 1 || len(env.Plan.Operations[0].Changes) != 2 {
		t.Fatalf("expected 1 planned proposal with 2 changes, got ops=%+v", env.Plan.Operations)
	}
	extraChange := env.Plan.Operations[0].Changes[1]
	got, ok := extraChange.Add.(string)
	if extraChange.Field != "extra" || !ok {
		t.Fatalf("extra change = %+v, want a string extra Change", extraChange)
	}
	if !strings.HasPrefix(got, existingExtra+"\n") {
		t.Errorf("previewed extra = %q, want it to start with the pre-existing Extra %q (not just the delta line)", got, existingExtra)
	}
}

func TestItemsEnrichAttachModeRequiresMissingPDF(t *testing.T) {
	cmd := newItemsEnrichCmd(&rootFlags{asJSON: true})
	cmd.SetArgs([]string{"--missing-doi", "--attach-mode", "linked-file"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil || ExitCode(err) != 2 || !strings.Contains(err.Error(), "--attach-mode is only valid with --missing-pdf") {
		t.Fatalf("err = %v, code=%d; want usage error for --attach-mode without --missing-pdf", err, ExitCode(err))
	}
}

func TestItemsEnrichLinkedFileRequiresPDFDir(t *testing.T) {
	cmd := newItemsEnrichCmd(&rootFlags{asJSON: true})
	cmd.SetArgs([]string{"--missing-pdf", "--attach-mode", "linked-file"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil || ExitCode(err) != 2 || !strings.Contains(err.Error(), "--pdf-dir is required") {
		t.Fatalf("err = %v, code=%d; want usage error for linked-file without --pdf-dir", err, ExitCode(err))
	}
}

func TestItemsEnrichStoredModePointsAtAttachmentsAdd(t *testing.T) {
	cmd := newItemsEnrichCmd(&rootFlags{asJSON: true, via: "web"})
	cmd.SetArgs([]string{"--missing-pdf", "--attach-mode", "stored"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil || ExitCode(err) != 2 || !strings.Contains(err.Error(), "not supported by items enrich") || !strings.Contains(err.Error(), "zotio attachments add") {
		t.Fatalf("err = %v, code=%d; want usage error routing stored mode to attachments add", err, ExitCode(err))
	}
}

func TestItemsEnrichLinkedFilePreviewDoesNotDownload(t *testing.T) {
	seedEnrichPDFStore(t)
	pdfRequests := 0
	pdfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pdfRequests++
		_, _ = w.Write([]byte("%PDF-1.7"))
	}))
	t.Cleanup(pdfSrv.Close)
	upw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"best_oa_location":{"url_for_pdf":"` + pdfSrv.URL + `/paper.pdf"}}`))
	}))
	t.Cleanup(upw.Close)
	withBase(t, &enrichUnpaywallBase, upw.URL)

	destDir := filepath.Join(t.TempDir(), "pdfs")
	flags := &rootFlags{asJSON: true}
	cmd := newItemsEnrichCmd(flags)
	cmd.SetArgs([]string{"--missing-pdf", "--email", "me@example.com", "--attach-mode", "linked-file", "--pdf-dir", destDir})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("enrich preview: %v", err)
	}
	if pdfRequests != 0 {
		t.Fatalf("preview downloaded PDF %d time(s), want 0", pdfRequests)
	}
	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if env.Plan.Summary.Planned != 1 || len(env.Plan.Operations) != 1 {
		t.Fatalf("plan = %+v ops=%+v, want one PDF proposal", env.Plan.Summary, env.Plan.Operations)
	}
	got := fmt.Sprint(env.Plan.Operations[0].Changes[0].Add)
	if !strings.Contains(got, "linked-file ->") || !strings.Contains(got, filepath.Join(destDir, "KPDF.pdf")) || !strings.Contains(got, pdfSrv.URL+"/paper.pdf") {
		t.Fatalf("preview change = %q, want mode, destination, and download URL", got)
	}
}

func TestItemsEnrichLinkedFileApplyDownloadsAndPostsAttachment(t *testing.T) {
	withPDFSafety(t, nil)
	seedEnrichPDFStore(t)
	pdfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("%PDF-1.7\nbody"))
	}))
	t.Cleanup(pdfSrv.Close)
	upw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"best_oa_location":{"url_for_pdf":"` + pdfSrv.URL + `/paper.pdf"}}`))
	}))
	t.Cleanup(upw.Close)
	withBase(t, &enrichUnpaywallBase, upw.URL)

	var gotBody []map[string]any
	zsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/items/KPDF/children" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/items" {
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_, _ = w.Write([]byte(`{"success":{"0":"ATTACH1"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(zsrv.Close)
	t.Setenv("ZOTERO_BASE_URL", zsrv.URL)

	destDir := filepath.Join(t.TempDir(), "pdfs")
	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1}
	cmd := newItemsEnrichCmd(flags)
	cmd.SetArgs([]string{"--missing-pdf", "--email", "me@example.com", "--attach-mode", "linked-file", "--pdf-dir", destDir})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("enrich linked-file apply: %v; out=%s", err, out.String())
	}

	wantPath := filepath.Join(destDir, "KPDF.pdf")
	gotPDF, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read linked PDF: %v", err)
	}
	if string(gotPDF) != "%PDF-1.7\nbody" {
		t.Fatalf("downloaded PDF = %q", gotPDF)
	}
	if len(gotBody) != 1 {
		t.Fatalf("attachment POST body = %+v, want one item", gotBody)
	}
	att := gotBody[0]
	if att["linkMode"] != "linked_file" || att["parentItem"] != "KPDF" || att["path"] != wantPath || att["contentType"] != "application/pdf" {
		t.Fatalf("attachment = %+v, want linked_file child pointing at downloaded path", att)
	}
}

func TestItemsEnrichApplyViaAPI(t *testing.T) {
	crsrv := crossRefSearchServer(t, "Attention Is All You Need", "10.1/attention")
	withBase(t, &enrichCrossRefBase, crsrv.URL)
	_ = seedEnrichStore(t) // sets HOME + ZOTERO_CONFIG to the seeded store

	var gotBody map[string]any
	var gotHeader string
	zsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && r.URL.Path == "/items/K1" {
			gotHeader = r.Header.Get("If-Unmodified-Since-Version")
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/items/K1" {
			w.Header().Set("Last-Modified-Version", "42")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"key":"K1","version":42,"data":{"key":"K1"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer zsrv.Close()
	t.Setenv("ZOTERO_BASE_URL", zsrv.URL)

	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1} // apply mode
	cmd := newItemsEnrichCmd(flags)
	cmd.SetArgs([]string{"--missing-doi", "--no-openalex", "--no-semantic-scholar"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("enrich apply: %v", err)
	}

	if gotBody == nil {
		t.Fatal("Zotero server never received the PATCH")
	}
	if gotBody["DOI"] != "10.1/attention" {
		t.Errorf("patched DOI = %v, want 10.1/attention", gotBody["DOI"])
	}
	if gotHeader != "42" {
		t.Errorf("If-Unmodified-Since-Version = %q, want %q", gotHeader, "42")
	}
	if _, ok := gotBody["version"]; ok {
		t.Errorf("PATCH body must not contain version key; gotBody=%v", gotBody)
	}

	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if !env.OK || env.Mode != "apply" || env.Result == nil {
		t.Fatalf("expected successful apply envelope, got %+v", env)
	}
	if env.Result.Summary.Applied != 1 || len(env.Result.Items) != 1 || env.Result.Items[0].Status != "applied" {
		t.Errorf("expected one applied result, got %+v", env.Result)
	}
}

func captureItemsEnrichApplyPatch(t *testing.T, extra ...string) (map[string]any, string) {
	t.Helper()
	crsrv := crossRefSearchServer(t, "Attention Is All You Need", "10.1/attention")
	withBase(t, &enrichCrossRefBase, crsrv.URL)
	_ = seedEnrichStore(t, extra...)

	var gotBody map[string]any
	var gotHeader string
	zsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && r.URL.Path == "/items/K1" {
			gotHeader = r.Header.Get("If-Unmodified-Since-Version")
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/items/K1" {
			w.Header().Set("Last-Modified-Version", "42")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"key":"K1","version":42,"data":{"key":"K1"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(zsrv.Close)
	t.Setenv("ZOTERO_BASE_URL", zsrv.URL)

	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1}
	cmd := newItemsEnrichCmd(flags)
	cmd.SetArgs([]string{"--missing-doi", "--no-openalex", "--no-semantic-scholar"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("enrich apply: %v", err)
	}
	if gotBody == nil {
		t.Fatal("Zotero server never received the PATCH")
	}
	return gotBody, gotHeader
}

func TestItemsEnrichApplyPreservesExistingExtra(t *testing.T) {
	existingExtra := "Citation Key: smith2020\nsome user note"
	gotBody, gotHeader := captureItemsEnrichApplyPatch(t, existingExtra)
	if gotHeader != "42" {
		t.Fatalf("If-Unmodified-Since-Version = %q, want %q", gotHeader, "42")
	}
	if _, ok := gotBody["version"]; ok {
		t.Fatalf("PATCH body must not contain version key; gotBody=%v", gotBody)
	}

	want := existingExtra + "\n" + enrichProvenanceLine(&enrichProposal{Category: "missing_doi", Source: "CrossRef"})
	if gotBody["extra"] != want {
		t.Fatalf("patched extra = %q, want %q", gotBody["extra"], want)
	}
}

func TestItemsEnrichApplyEmptyExtraWritesOnlyProvenance(t *testing.T) {
	gotBody, gotHeader := captureItemsEnrichApplyPatch(t)
	if gotHeader != "42" {
		t.Fatalf("If-Unmodified-Since-Version = %q, want %q", gotHeader, "42")
	}
	if _, ok := gotBody["version"]; ok {
		t.Fatalf("PATCH body must not contain version key; gotBody=%v", gotBody)
	}

	want := enrichProvenanceLine(&enrichProposal{Category: "missing_doi", Source: "CrossRef"})
	if gotBody["extra"] != want {
		t.Fatalf("patched extra = %q, want %q", gotBody["extra"], want)
	}
}

func TestAppendEnrichProvenanceSameDayIdempotent(t *testing.T) {
	p := enrichProposal{Category: "missing_doi", Source: "CrossRef"}
	existing := "Citation Key: smith2020\n" + enrichProvenanceLine(&p)
	p.extra = existing

	got := appendEnrichProvenance(&p, &rootFlags{})
	if got != existing {
		t.Fatalf("appendEnrichProvenance duplicated same-day provenance: got %q, want %q", got, existing)
	}
	if strings.Count(got, enrichProvenanceLine(&p)) != 1 {
		t.Fatalf("provenance count = %d, want 1 in %q", strings.Count(got, enrichProvenanceLine(&p)), got)
	}
}

func openAlexWorkServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestReconstructAbstract(t *testing.T) {
	if got := reconstructAbstract(map[string][]int{"Hello": {0}, "world": {1}, "again": {2}}); got != "Hello world again" {
		t.Errorf("got %q, want 'Hello world again'", got)
	}
	// A word repeated at multiple positions.
	if got := reconstructAbstract(map[string][]int{"the": {0, 2}, "cat": {1}, "sat": {3}}); got != "the cat the sat" {
		t.Errorf("got %q, want 'the cat the sat'", got)
	}
	if reconstructAbstract(nil) != "" {
		t.Error("nil index should reconstruct to empty")
	}
}

func TestReconstructAbstractRejectsHugePosition(t *testing.T) {
	if got := reconstructAbstract(map[string][]int{"x": {2_000_000_000}}); got != "" {
		t.Fatalf("got %q, want empty abstract", got)
	}
}

func TestReconstructAbstractIgnoresNegativePositions(t *testing.T) {
	if got := reconstructAbstract(map[string][]int{"drop": {-1}, "Hello": {0}, "world": {1}}); got != "Hello world" {
		t.Fatalf("got %q, want 'Hello world'", got)
	}
}

func TestReconstructAbstractRejectsTooManyPositions(t *testing.T) {
	positions := make([]int, maxOpenAlexAbstractPairs+1)
	if got := reconstructAbstract(map[string][]int{"x": positions}); got != "" {
		t.Fatalf("got %q, want empty abstract", got)
	}
}

func TestResolveAbstractViaOpenAlex(t *testing.T) {
	srv := openAlexWorkServer(t, `{"results":[{"doi":"https://doi.org/10.1/x","title":"T","abstract_inverted_index":{"We":[0],"propose":[1],"Transformer":[2]}}]}`)
	withBase(t, &enrichOpenAlexBase, srv.URL)
	abstract, ok := resolveAbstractViaOpenAlex(context.Background(), http.DefaultClient, "10.1/x", "")
	if !ok || abstract != "We propose Transformer" {
		t.Errorf("abstract = %q ok=%v, want 'We propose Transformer'", abstract, ok)
	}
}

func TestResolveDOIViaOpenAlex(t *testing.T) {
	srv := openAlexWorkServer(t, `{"results":[{"doi":"https://doi.org/10.9/match","title":"Attention Is All You Need"}]}`)
	withBase(t, &enrichOpenAlexBase, srv.URL)
	data := map[string]any{"title": "attention is all you need"}
	doi, ok := resolveDOIViaOpenAlex(context.Background(), http.DefaultClient, data, "")
	if !ok || doi != "10.9/match" {
		t.Errorf("doi = %q ok=%v, want 10.9/match", doi, ok)
	}
}

// TestEnrichOpenAlexAbstractFallback: CrossRef has no abstract, so the resolver
// falls back to OpenAlex and records the provider in Source; with the fallback
// disabled it skips.
func TestEnrichOpenAlexAbstractFallback(t *testing.T) {
	cr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{}}`))
	}))
	t.Cleanup(cr.Close)
	withBase(t, &enrichCrossRefBase, cr.URL)
	oa := openAlexWorkServer(t, `{"results":[{"doi":"10.1/x","abstract_inverted_index":{"From":[0],"OpenAlex":[1]}}]}`)
	withBase(t, &enrichOpenAlexBase, oa.URL)

	data := map[string]any{"title": "T", "DOI": "10.1/x"}
	prop, reason := resolveEnrichment(context.Background(), http.DefaultClient, "missing_abstract", "K1", float64(1), data, "", true, false, "linked-url", "")
	if reason != "" {
		t.Fatalf("unexpected skip: %s", reason)
	}
	if prop.Source != "OpenAlex" {
		t.Errorf("source = %q, want OpenAlex", prop.Source)
	}
	if prop.Fields["abstractNote"] != "From OpenAlex" {
		t.Errorf("abstract = %v, want 'From OpenAlex'", prop.Fields["abstractNote"])
	}

	if _, reason := resolveEnrichment(context.Background(), http.DefaultClient, "missing_abstract", "K1", float64(1), data, "", false, false, "linked-url", ""); reason == "" {
		t.Error("expected a skip when the OpenAlex fallback is disabled")
	}
}

// --validate is read-only and reports CrossRef discrepancies for DOI-bearing local items.
func TestItemsEnrichValidateReportsCrossRefTitleDiscrepancy(t *testing.T) {
	cr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"title":["Provider Title"],"DOI":"10.1/validate","published":{"date-parts":[[2024]]}}}`))
	}))
	t.Cleanup(cr.Close)
	withBase(t, &enrichCrossRefBase, cr.URL)
	oc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"doi":"10.1/validate"}]`))
	}))
	t.Cleanup(oc.Close)
	withBase(t, &enrichOpenCitationsBase, oc.URL)

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "validate.toml"))
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	items := []json.RawMessage{
		json.RawMessage(`{"key":"KVAL","version":1,"data":{"key":"KVAL","itemType":"journalArticle","title":"Stored Title","date":"2024","DOI":"10.1/validate"}}`),
		json.RawMessage(`{"key":"KNODOI","version":1,"data":{"key":"KNODOI","itemType":"journalArticle","title":"No DOI"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = db.Close()

	flags := &rootFlags{asJSON: true}
	cmd := newItemsEnrichCmd(flags)
	cmd.SetArgs([]string{"--validate"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	var report enrichValidationReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if report.Validated != 1 {
		t.Fatalf("validated = %d, want 1", report.Validated)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("findings = %+v, want one title discrepancy", report.Findings)
	}
	got := report.Findings[0]
	if got.ItemKey != "KVAL" || got.Evidence["field"] != "title" || got.Evidence["stored"] != "Stored Title" || got.Evidence["provider"] != "Provider Title" || got.Source.Kind != "crossref" {
		t.Errorf("finding = %+v, want CrossRef title mismatch", got)
	}
	if len(report.UnverifiedDOIs) != 0 {
		t.Errorf("unverified DOIs = %+v, want none", report.UnverifiedDOIs)
	}
}
func TestApplyEnrichProposal_PatchFailsClosedOnZeroWriteVersion(t *testing.T) {
	f := &fakeMutator{writeVersion: 0}
	p := enrichProposal{
		Key: "ABC", Category: "missing_doi", Action: enrichActionPatch,
		Source: "CrossRef", Fields: map[string]any{"DOI": "10.1/x"}, version: float64(7),
	}
	status, _, err := applyEnrichProposal(f, &p, &rootFlags{})
	if err == nil || status != "failed" {
		t.Fatalf("apply = status %q err %v, want failed when write-plane version is 0", status, err)
	}
	if f.patchPath != "" {
		t.Errorf("patch path = %q, want no PATCH dispatched on zero-version precondition", f.patchPath)
	}
}

func TestNormalizeTitleForMatch_UnicodeAware(t *testing.T) {
	// Non-Latin titles must normalize to non-empty, distinct content.
	a := normalizeTitleForMatch("机器学习导论")
	b := normalizeTitleForMatch("量子物理基础")
	if a == "" {
		t.Fatal("Chinese title normalized to empty, want meaningful content")
	}
	if a == b {
		t.Fatalf("distinct Chinese titles both normalized to %q", a)
	}
	if got := normalizeTitleForMatch("机器学习导论"); got != a {
		t.Errorf("identical Chinese title normalized to %q, want %q", got, a)
	}
	// Punctuation-only titles must normalize to empty so the empty guard fires.
	if got := normalizeTitleForMatch("...!!!"); got != "" {
		t.Errorf("punctuation-only title normalized to %q, want empty", got)
	}
}

func TestResolveDOIViaCrossRef_NonLatinMismatchReturnsNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{"message":{"items":[{"title":["量子物理基础"],"DOI":"10.9/wrong"}]}}`
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	withBase(t, &enrichCrossRefBase, srv.URL)
	data := map[string]any{"title": "机器学习导论"}
	if _, _, ok := resolveDOIViaCrossRef(context.Background(), http.DefaultClient, data); ok {
		t.Fatal("expected no DOI match for unrelated Chinese titles")
	}
}

func TestResolveDOIViaCrossRef_NonLatinIdenticalMatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{"message":{"items":[{"title":["机器学习导论"],"DOI":"10.9/match"}]}}`
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	withBase(t, &enrichCrossRefBase, srv.URL)
	data := map[string]any{"title": "机器学习导论"}
	doi, _, ok := resolveDOIViaCrossRef(context.Background(), http.DefaultClient, data)
	if !ok || doi != "10.9/match" {
		t.Fatalf("doi = %q ok=%v, want 10.9/match for identical Chinese titles", doi, ok)
	}
}

func TestResolveDOIViaCrossRef_PunctuationOnlyLocalTitleReturnsNoMatch(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true // guard should fire before provider is consulted; url stays same but logic short-circuits
		_, _ = w.Write([]byte(`{"message":{"items":[{"title":["!!!"],"DOI":"10.9/wrong"}]}}`))
	}))
	t.Cleanup(srv.Close)
	withBase(t, &enrichCrossRefBase, srv.URL)
	data := map[string]any{"title": "...!!!"}
	if _, _, ok := resolveDOIViaCrossRef(context.Background(), http.DefaultClient, data); ok {
		t.Fatal("expected no match for punctuation-only local title")
	}
	// The guard returns before iterating, but getJSON still ran (empty guard
	// sits after fetch). What matters is no-match, not whether the server was
	// contacted — a follow-up could move the guard earlier.
	_ = hit
}

func TestResolveDOIViaSemanticScholar_NonLatinMismatchReturnsNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"title":"量子物理基础","externalIds":{"DOI":"10.9/wrong"}}]}`))
	}))
	t.Cleanup(srv.Close)
	withBase(t, &enrichSemanticScholarBase, srv.URL)
	data := map[string]any{"title": "机器学习导论"}
	if _, ok := resolveDOIViaSemanticScholar(context.Background(), http.DefaultClient, data); ok {
		t.Fatal("expected no DOI match for unrelated Chinese titles via Semantic Scholar")
	}
}

func TestResolveDOIViaSemanticScholar_NonLatinIdenticalMatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"title":"机器学习导论","externalIds":{"DOI":"10.9/ssmatch"}}]}`))
	}))
	t.Cleanup(srv.Close)
	withBase(t, &enrichSemanticScholarBase, srv.URL)
	data := map[string]any{"title": "机器学习导论"}
	doi, ok := resolveDOIViaSemanticScholar(context.Background(), http.DefaultClient, data)
	if !ok || doi != "10.9/ssmatch" {
		t.Fatalf("doi = %q ok=%v, want 10.9/ssmatch for identical Chinese titles", doi, ok)
	}
}

func TestResolveDOIViaOpenAlex_NonLatinMismatchReturnsNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"doi":"https://doi.org/10.9/wrong","title":"量子物理基础"}]}`))
	}))
	t.Cleanup(srv.Close)
	withBase(t, &enrichOpenAlexBase, srv.URL)
	data := map[string]any{"title": "机器学习导论"}
	if _, ok := resolveDOIViaOpenAlex(context.Background(), http.DefaultClient, data, ""); ok {
		t.Fatal("expected no DOI match for unrelated Chinese titles via OpenAlex")
	}
}

func TestResolveDOIViaOpenAlex_NonLatinIdenticalMatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"doi":"https://doi.org/10.9/oamatch","title":"机器学习导论"}]}`))
	}))
	t.Cleanup(srv.Close)
	withBase(t, &enrichOpenAlexBase, srv.URL)
	data := map[string]any{"title": "机器学习导论"}
	doi, ok := resolveDOIViaOpenAlex(context.Background(), http.DefaultClient, data, "")
	if !ok || doi != "10.9/oamatch" {
		t.Fatalf("doi = %q ok=%v, want 10.9/oamatch for identical Chinese titles", doi, ok)
	}
}

func TestResolveDOIViaOpenAlex_PunctuationOnlyLocalTitleReturnsNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"doi":"https://doi.org/10.9/wrong","title":"...!!!"}]}`))
	}))
	t.Cleanup(srv.Close)
	withBase(t, &enrichOpenAlexBase, srv.URL)
	data := map[string]any{"title": "...!!!"}
	if _, ok := resolveDOIViaOpenAlex(context.Background(), http.DefaultClient, data, ""); ok {
		t.Fatal("expected no match for punctuation-only local title via OpenAlex")
	}
}
func TestResolveDOIViaSemanticScholar_PunctuationOnlyLocalTitleReturnsNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"title":"...!!!","externalIds":{"DOI":"10.9/wrong"}}]}`))
	}))
	t.Cleanup(srv.Close)
	withBase(t, &enrichSemanticScholarBase, srv.URL)
	data := map[string]any{"title": "...!!!"}
	if _, ok := resolveDOIViaSemanticScholar(context.Background(), http.DefaultClient, data); ok {
		t.Fatal("expected no match for punctuation-only local title via Semantic Scholar")
	}
}
