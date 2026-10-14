// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func runImportValidationCmd(t *testing.T, newCmd func(*rootFlags) *cobra.Command, args ...string) (string, error) {
	t.Helper()
	cmd := newCmd(&rootFlags{asJSON: true, timeout: 5 * time.Second, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// A malformed identifier is a usage error that never reaches the network: the
// upstream bases point at a server that fails the test when it is called.
func TestImportIdentifierSyntaxRejectedBeforeNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected upstream request %s", r.URL)
	}))
	defer srv.Close()
	withBase(t, &enrichCrossRefBase, srv.URL)
	withBase(t, &importPubMedBase, srv.URL)
	withBase(t, &importArxivBase, srv.URL)
	withBase(t, &importOpenLibraryBase, srv.URL)

	for _, tc := range []struct {
		name   string
		newCmd func(*rootFlags) *cobra.Command
		arg    string
	}{
		{"doi", newImportDoiCmd, "notadoi"},
		{"isbn", newImportIsbnCmd, "123"},
		{"isbn non-bookland prefix", newImportIsbnCmd, "9771234567898"},
		{"pmid", newImportPmidCmd, "abc"},
		{"arxiv", newImportArxivCmd, "notanid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runImportValidationCmd(t, tc.newCmd, tc.arg)
			if err == nil || ExitCode(err) != 2 {
				t.Fatalf("err = %v (exit %d), want usage error (exit 2)", err, ExitCode(err))
			}
		})
	}
}

// A well-formed identifier the upstream does not know exits 3 and keeps the
// upstream wording in the message.
func TestImportIdentifierNotFoundUpstreamExitsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	withBase(t, &importOpenLibraryBase, srv.URL)

	_, err := runImportValidationCmd(t, newImportIsbnCmd, "9781234567897")
	if err == nil || ExitCode(err) != 3 || !strings.Contains(err.Error(), "ISBN not found: 9781234567897") {
		t.Fatalf("err = %v (exit %d), want exit 3 with the upstream text", err, ExitCode(err))
	}
}

// An unparsable URL is reported as invalid in the preview, with exit 2, the
// same as apply would.
func TestImportURLInvalidPreviewExitsUsage(t *testing.T) {
	out, err := runImportValidationCmd(t, newImportUrlCmd, "notaurl")
	if err == nil || ExitCode(err) != 2 {
		t.Fatalf("err = %v (exit %d), want usage error (exit 2)", err, ExitCode(err))
	}
	if !strings.Contains(out, `"ok": false`) || !strings.Contains(out, "invalid URL") {
		t.Fatalf("preview output = %s, want an ok:false envelope naming the invalid URL", out)
	}
}

// A truncating --limit marks the manifest so a consumer cannot mistake it for
// the whole directory.
func TestListPDFsLimitedReportsTruncation(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.pdf", "b.pdf", "c.pdf"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("%PDF-1.4"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pdfs, truncated, err := listPDFsLimited(dir, 2)
	if err != nil || len(pdfs) != 2 || !truncated {
		t.Fatalf("limit 2: pdfs=%v truncated=%t err=%v, want 2 PDFs and truncated", pdfs, truncated, err)
	}
	pdfs, truncated, err = listPDFsLimited(dir, 3)
	if err != nil || len(pdfs) != 3 || truncated {
		t.Fatalf("limit 3: pdfs=%v truncated=%t err=%v, want 3 PDFs and not truncated", pdfs, truncated, err)
	}
	pdfs, truncated, err = listPDFsLimited(dir, 0)
	if err != nil || len(pdfs) != 3 || truncated {
		t.Fatalf("limit 0: pdfs=%v truncated=%t err=%v, want all 3 PDFs and not truncated", pdfs, truncated, err)
	}
}

// A missing PDF is reported invalid (exit 2) even when no desktop connector is
// available, because local input is validated before the connector is probed.
func TestImportPDFMissingFileInvalidWithoutConnector(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.pdf")
	cmd := newImportPDFCmd(&rootFlags{asJSON: true, via: "web", timeout: time.Second, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{missing})
	err := cmd.Execute()
	if err == nil || ExitCode(err) != 2 {
		t.Fatalf("err = %v (exit %d), want usage error (exit 2)", err, ExitCode(err))
	}
	if !strings.Contains(out.String(), `"ok": false`) || !strings.Contains(out.String(), "cannot read PDF") {
		t.Fatalf("output = %s, want an ok:false envelope naming the unreadable PDF", out.String())
	}
}
