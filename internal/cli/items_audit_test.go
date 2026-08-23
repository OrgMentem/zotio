// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zotio/internal/store"
)

func auditIsolateEnv(t *testing.T, baseURL string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZOTIO_DEMO", "0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_DATA_DIR", "")
	t.Setenv("ZOTERO_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if baseURL != "" {
		t.Setenv("ZOTERO_BASE_URL", baseURL)
	} else {
		t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/api/users/0")
	}
	savedGroup := activeGroupIDLocked()
	setActiveGroupID("")
	savedNoColor := noColor
	savedHuman := humanFriendly
	t.Cleanup(func() {
		setActiveGroupID(savedGroup)
		noColor = savedNoColor
		humanFriendly = savedHuman
	})
}

func auditSeedDB(t *testing.T, items []json.RawMessage) {
	t.Helper()
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		_ = db.Close()
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func runItemsAudit(t *testing.T, flags *rootFlags, args ...string) (string, string, error) {
	t.Helper()
	cmd := newItemsAuditCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errBuf.String(), err
}

func decodeAuditMap(t *testing.T, s string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("decode audit JSON %q: %v", s, err)
	}
	return m
}

func decodeAuditSlice(t *testing.T, raw json.RawMessage) []map[string]any {
	t.Helper()
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatalf("decode audit slice %q: %v", string(raw), err)
	}
	return arr
}

func decodeAuditFindings(t *testing.T, raw json.RawMessage) []Finding {
	t.Helper()
	var findings []Finding
	if err := json.Unmarshal(raw, &findings); err != nil {
		t.Fatalf("decode findings %q: %v", string(raw), err)
	}
	return findings
}

// TestItemsAuditCmd executes the real Cobra command against a seeded local store.
// It covers flag routing, default summary, JSON finding fields, severity, verify-files
// and human output without ever reaching localhost:23119.
func TestItemsAuditCmd(t *testing.T) {
	// Area 2 helper: verify selectedItemsAuditChecks default set.
	t.Run("no_flag_default_set", func(t *testing.T) {
		checks := selectedItemsAuditChecks(false, false, false, false, false)
		if len(checks) != 0 {
			t.Fatalf("selectedItemsAuditChecks() = %v, want empty (summary mode)", checks)
		}
		all := selectedItemsAuditChecks(true, true, true, true, true)
		if len(all) != 5 {
			t.Fatalf("all checks = %d, want 5", len(all))
		}
		wantNames := map[string]bool{
			"missing_pdf":      true,
			"missing_abstract": true,
			"missing_doi":      true,
			"missing_tags":     true,
			"missing_citation": true,
		}
		for _, c := range all {
			if !wantNames[c.name] {
				t.Fatalf("unexpected check %q", c.name)
			}
			delete(wantNames, c.name)
		}
		if len(wantNames) != 0 {
			t.Fatalf("missing checks %v", wantNames)
		}

		// No flag at all should return the audit summary (human and JSON).
		auditIsolateEnv(t, "")
		auditSeedDB(t, []json.RawMessage{
			json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"P1"}}`),
			json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"journalArticle","title":"P2","DOI":"10/x","abstractNote":"abs","tags":[{"tag":"t"}],"creators":[{"lastName":"Doe","creatorType":"author"}],"date":"2020","publicationTitle":"J"}}`),
			json.RawMessage(`{"key":"A2","version":1,"data":{"key":"A2","itemType":"attachment","parentItem":"P2","contentType":"application/pdf","linkMode":"imported_file","filename":"a.pdf"}}`),
		})
		// JSON summary path
		flags := &rootFlags{asJSON: true, timeout: 2 * time.Second}
		out, _, err := runItemsAudit(t, flags)
		if err != nil {
			t.Fatalf("audit default JSON: %v", err)
		}
		var summary itemsAuditSummary
		if err := json.Unmarshal([]byte(out), &summary); err != nil {
			t.Fatalf("decode summary %q: %v", out, err)
		}
		if summary.TopLevelItems != 2 {
			t.Fatalf("top_level_items = %d, want 2", summary.TopLevelItems)
		}
		if summary.Findings == nil {
			t.Fatalf("findings is nil, want empty slice")
		}
		// Human summary path (non-JSON) should print counts via printItemsAuditSummary.
		flags2 := &rootFlags{timeout: 2 * time.Second}
		humanOut, _, err := runItemsAudit(t, flags2)
		if err != nil {
			t.Fatalf("audit default human: %v", err)
		}
		if !strings.Contains(humanOut, "Scope:") || !strings.Contains(humanOut, "top-level items") {
			t.Fatalf("human summary %q missing Scope line", humanOut)
		}
		for _, check := range []string{"missing-pdf", "missing-abstract", "missing-doi", "missing-tags", "missing-citation"} {
			if !strings.Contains(humanOut, check) {
				t.Fatalf("human summary %q missing check %q", humanOut, check)
			}
		}
	})

	t.Run("check_flags_isolated", func(t *testing.T) {
		type tc struct {
			name      string
			flag      string
			checkName string
			dirty     json.RawMessage
			attach    json.RawMessage
		}
		cases := []tc{
			{
				name:      "missing-pdf",
				flag:      "--missing-pdf",
				checkName: "missing_pdf",
				dirty:     json.RawMessage(`{"key":"MPDF","version":1,"data":{"key":"MPDF","itemType":"journalArticle","title":"Needs PDF","creators":[{"lastName":"Doe","creatorType":"author"}],"date":"2020","publicationTitle":"J","DOI":"10/mpdf","abstractNote":"abs","tags":[{"tag":"t"}]}}`),
				attach:    nil,
			},
			{
				name:      "missing-abstract",
				flag:      "--missing-abstract",
				checkName: "missing_abstract",
				dirty:     json.RawMessage(`{"key":"MABS","version":1,"data":{"key":"MABS","itemType":"journalArticle","title":"Needs Abstract","creators":[{"lastName":"Doe","creatorType":"author"}],"date":"2020","publicationTitle":"J","DOI":"10/mabs","tags":[{"tag":"t"}]}}`),
				attach:    json.RawMessage(`{"key":"A_MABS","version":1,"data":{"key":"A_MABS","itemType":"attachment","parentItem":"MABS","contentType":"application/pdf","linkMode":"imported_file","filename":"a.pdf"}}`),
			},
			{
				name:      "missing-doi",
				flag:      "--missing-doi",
				checkName: "missing_doi",
				dirty:     json.RawMessage(`{"key":"MDOI","version":1,"data":{"key":"MDOI","itemType":"journalArticle","title":"Needs DOI","creators":[{"lastName":"Doe","creatorType":"author"}],"date":"2020","publicationTitle":"J","abstractNote":"abs","tags":[{"tag":"t"}]}}`),
				attach:    json.RawMessage(`{"key":"A_MDOI","version":1,"data":{"key":"A_MDOI","itemType":"attachment","parentItem":"MDOI","contentType":"application/pdf","linkMode":"imported_file","filename":"a.pdf"}}`),
			},
			{
				name:      "missing-tags",
				flag:      "--missing-tags",
				checkName: "missing_tags",
				dirty:     json.RawMessage(`{"key":"MTAG","version":1,"data":{"key":"MTAG","itemType":"journalArticle","title":"Needs Tags","creators":[{"lastName":"Doe","creatorType":"author"}],"date":"2020","publicationTitle":"J","DOI":"10/mtag","abstractNote":"abs"}}`),
				attach:    json.RawMessage(`{"key":"A_MTAG","version":1,"data":{"key":"A_MTAG","itemType":"attachment","parentItem":"MTAG","contentType":"application/pdf","linkMode":"imported_file","filename":"a.pdf"}}`),
			},
			{
				name:      "missing-citation",
				flag:      "--missing-citation",
				checkName: "missing_citation",
				dirty:     json.RawMessage(`{"key":"MCIT","version":1,"data":{"key":"MCIT","itemType":"journalArticle","title":"Needs Citation","date":"2020","publicationTitle":"J","DOI":"10/mcit","abstractNote":"abs","tags":[{"tag":"t"}]}}`),
				attach:    json.RawMessage(`{"key":"A_MCIT","version":1,"data":{"key":"A_MCIT","itemType":"attachment","parentItem":"MCIT","contentType":"application/pdf","linkMode":"imported_file","filename":"a.pdf"}}`),
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				auditIsolateEnv(t, "")
				clean := json.RawMessage(`{"key":"CLEAN","version":1,"data":{"key":"CLEAN","itemType":"journalArticle","title":"Clean","creators":[{"lastName":"Doe","creatorType":"author"}],"date":"2020","publicationTitle":"J","DOI":"10/clean","abstractNote":"abs","tags":[{"tag":"t"}]}}`)
				cleanAttach := json.RawMessage(`{"key":"A_CLEAN","version":1,"data":{"key":"A_CLEAN","itemType":"attachment","parentItem":"CLEAN","contentType":"application/pdf","linkMode":"imported_file","filename":"clean.pdf"}}`)
				items := []json.RawMessage{clean, cleanAttach, tc.dirty}
				if tc.attach != nil {
					items = append(items, tc.attach)
				}
				auditSeedDB(t, items)

				flags := &rootFlags{asJSON: true, timeout: 2 * time.Second}
				out, _, err := runItemsAudit(t, flags, tc.flag)
				if err != nil {
					t.Fatalf("audit %s: %v", tc.flag, err)
				}
				m := decodeAuditMap(t, out)
				raw, ok := m[tc.checkName]
				if !ok {
					t.Fatalf("check %q missing in output %v", tc.checkName, m)
				}
				rows := decodeAuditSlice(t, raw)
				if len(rows) != 1 {
					t.Fatalf("%s rows = %d, want 1: %v", tc.checkName, len(rows), rows)
				}
				wantKey := map[string]string{"missing_pdf": "MPDF", "missing_abstract": "MABS", "missing_doi": "MDOI", "missing_tags": "MTAG", "missing_citation": "MCIT"}[tc.checkName]
				if got := sqlStringValue(rows[0]["key"]); got != wantKey {
					t.Fatalf("%s key = %q, want %q", tc.checkName, got, wantKey)
				}
				fRaw, ok := m["findings"]
				if !ok {
					t.Fatalf("findings missing for %s", tc.checkName)
				}
				findings := decodeAuditFindings(t, fRaw)
				if len(findings) != 1 {
					t.Fatalf("findings = %d, want 1 for %s: %+v", len(findings), tc.checkName, findings)
				}
				if findings[0].Kind != tc.checkName {
					t.Fatalf("finding kind = %q, want %q", findings[0].Kind, tc.checkName)
				}

				otherFlags := map[string]string{
					"missing_pdf":      "--missing-pdf",
					"missing_abstract": "--missing-abstract",
					"missing_doi":      "--missing-doi",
					"missing_tags":     "--missing-tags",
					"missing_citation": "--missing-citation",
				}
				for otherName, otherFlag := range otherFlags {
					if otherName == tc.checkName {
						continue
					}
					flags2 := &rootFlags{asJSON: true, timeout: 2 * time.Second}
					out2, _, err := runItemsAudit(t, flags2, otherFlag)
					if err != nil {
						t.Fatalf("audit other %s: %v", otherFlag, err)
					}
					m2 := decodeAuditMap(t, out2)
					raw2 := m2[otherName]
					rows2 := decodeAuditSlice(t, raw2)
					if len(rows2) != 0 {
						t.Fatalf("other check %s on %s store returned %d rows, want 0: %v", otherName, tc.name, len(rows2), rows2)
					}
					f2 := decodeAuditFindings(t, m2["findings"])
					if len(f2) != 0 {
						t.Fatalf("other check %s findings = %d, want 0: %+v", otherName, len(f2), f2)
					}
				}
			})
		}
	})

	t.Run("json_finding_fields", func(t *testing.T) {
		auditIsolateEnv(t, "")
		// Seed an item missing DOI (high severity, autofixable, command action) plus a clean control.
		auditSeedDB(t, []json.RawMessage{
			json.RawMessage(`{"key":"MCLEAN","version":1,"data":{"key":"MCLEAN","itemType":"journalArticle","title":"Clean","creators":[{"lastName":"Doe","creatorType":"author"}],"date":"2020","publicationTitle":"J","DOI":"10/clean","abstractNote":"abs","tags":[{"tag":"t"}]}}`),
			json.RawMessage(`{"key":"A_MCLEAN","version":1,"data":{"key":"A_MCLEAN","itemType":"attachment","parentItem":"MCLEAN","contentType":"application/pdf","linkMode":"imported_file","filename":"clean.pdf"}}`),
			json.RawMessage(`{"key":"JDOI","version":1,"data":{"key":"JDOI","itemType":"journalArticle","title":"Needs DOI","creators":[{"lastName":"Doe","creatorType":"author"}],"date":"2020","publicationTitle":"J","abstractNote":"abs","tags":[{"tag":"t"}]}}`),
			json.RawMessage(`{"key":"A_JDOI","version":1,"data":{"key":"A_JDOI","itemType":"attachment","parentItem":"JDOI","contentType":"application/pdf","linkMode":"imported_file","filename":"a.pdf"}}`),
		})
		flags := &rootFlags{asJSON: true, timeout: 2 * time.Second}
		out, _, err := runItemsAudit(t, flags, "--missing-doi")
		if err != nil {
			t.Fatalf("audit --missing-doi --json: %v", err)
		}
		m := decodeAuditMap(t, out)
		findings := decodeAuditFindings(t, m["findings"])
		if len(findings) != 1 {
			t.Fatalf("findings = %d, want 1: %+v", len(findings), findings)
		}
		f := findings[0]
		if f.ItemKey != "JDOI" {
			t.Fatalf("finding item_key = %q, want JDOI", f.ItemKey)
		}
		if f.Kind != "missing_doi" {
			t.Fatalf("finding kind = %q, want missing_doi", f.Kind)
		}
		if f.Severity != sevHigh {
			t.Fatalf("finding severity = %q, want %q", f.Severity, sevHigh)
		}
		if !f.Autofixable {
			t.Fatalf("finding autofixable = false, want true")
		}
		if f.RecommendedAction == nil || f.RecommendedAction.Command != "zotio items enrich --missing-doi --keys-from -" {
			t.Fatalf("finding action = %+v, want command %q", f.RecommendedAction, "zotio items enrich --missing-doi --keys-from -")
		}
		if f.Source.Kind != "local" {
			t.Fatalf("finding source = %q, want local", f.Source.Kind)
		}
		// Also assert the row carries evidence fields via the Finding evidence.
		rows := decodeAuditSlice(t, m["missing_doi"])
		if len(rows) != 1 || sqlStringValue(rows[0]["key"]) != "JDOI" {
			t.Fatalf("row key = %v, want JDOI", rows)
		}

		// missing_citation now routes to a fixer, like every peer content check.
		auditIsolateEnv(t, "")
		auditSeedDB(t, []json.RawMessage{
			json.RawMessage(`{"key":"CCLEAN","version":1,"data":{"key":"CCLEAN","itemType":"journalArticle","title":"Clean","creators":[{"lastName":"Doe","creatorType":"author"}],"date":"2020","publicationTitle":"J","DOI":"10/clean","abstractNote":"abs","tags":[{"tag":"t"}]}}`),
			json.RawMessage(`{"key":"A_CCLEAN","version":1,"data":{"key":"A_CCLEAN","itemType":"attachment","parentItem":"CCLEAN","contentType":"application/pdf","linkMode":"imported_file","filename":"clean.pdf"}}`),
			json.RawMessage(`{"key":"CCIT","version":1,"data":{"key":"CCIT","itemType":"journalArticle","title":"Needs Citation","date":"2020","publicationTitle":"J","DOI":"10/ccit","abstractNote":"abs","tags":[{"tag":"t"}]}}`),
			json.RawMessage(`{"key":"A_CCIT","version":1,"data":{"key":"A_CCIT","itemType":"attachment","parentItem":"CCIT","contentType":"application/pdf","linkMode":"imported_file","filename":"a.pdf"}}`),
		})
		flags2 := &rootFlags{asJSON: true, timeout: 2 * time.Second}
		out2, _, err := runItemsAudit(t, flags2, "--missing-citation")
		if err != nil {
			t.Fatalf("audit --missing-citation --json: %v", err)
		}
		m2 := decodeAuditMap(t, out2)
		f2 := decodeAuditFindings(t, m2["findings"])
		if len(f2) != 1 {
			t.Fatalf("citation findings = %d, want 1", len(f2))
		}
		if f2[0].RecommendedAction == nil || f2[0].RecommendedAction.Command != "zotio items enrich --missing-citation --keys-from -" {
			t.Fatalf("citation finding action = %+v, want the enrich fixer command", f2[0].RecommendedAction)
		}
		if f2[0].RecommendedAction.Text != "" {
			t.Fatalf("citation finding should have Command not Text, got %+v", f2[0].RecommendedAction)
		}
		if !f2[0].Autofixable {
			t.Fatalf("citation finding autofixable = false, want true now that a fixer exists")
		}
	})

	t.Run("severity_assignment", func(t *testing.T) {
		auditIsolateEnv(t, "")
		// High: missing_doi ; Info: missing_abstract — same store, different flags.
		auditSeedDB(t, []json.RawMessage{
			json.RawMessage(`{"key":"SEVDOI","version":1,"data":{"key":"SEVDOI","itemType":"journalArticle","title":"High DOI","creators":[{"lastName":"Doe","creatorType":"author"}],"date":"2020","publicationTitle":"J","abstractNote":"abs","tags":[{"tag":"t"}]}}`),
			json.RawMessage(`{"key":"A_SEVDOI","version":1,"data":{"key":"A_SEVDOI","itemType":"attachment","parentItem":"SEVDOI","contentType":"application/pdf","linkMode":"imported_file","filename":"a.pdf"}}`),
			json.RawMessage(`{"key":"SEVABS","version":1,"data":{"key":"SEVABS","itemType":"journalArticle","title":"Info Abstract","creators":[{"lastName":"Doe","creatorType":"author"}],"date":"2020","publicationTitle":"J","DOI":"10/sevabs","tags":[{"tag":"t"}]}}`),
			json.RawMessage(`{"key":"A_SEVABS","version":1,"data":{"key":"A_SEVABS","itemType":"attachment","parentItem":"SEVABS","contentType":"application/pdf","linkMode":"imported_file","filename":"a.pdf"}}`),
		})
		// High check
		flagsHigh := &rootFlags{asJSON: true, timeout: 2 * time.Second}
		outHigh, _, err := runItemsAudit(t, flagsHigh, "--missing-doi")
		if err != nil {
			t.Fatalf("missing-doi: %v", err)
		}
		fHigh := decodeAuditFindings(t, decodeAuditMap(t, outHigh)["findings"])
		if len(fHigh) != 1 || fHigh[0].Severity != sevHigh {
			t.Fatalf("missing_doi severity = %q, want %q", fHigh[0].Severity, sevHigh)
		}
		if fHigh[0].Kind != "missing_doi" {
			t.Fatalf("missing_doi kind = %q", fHigh[0].Kind)
		}
		// Info check
		flagsInfo := &rootFlags{asJSON: true, timeout: 2 * time.Second}
		outInfo, _, err := runItemsAudit(t, flagsInfo, "--missing-abstract")
		if err != nil {
			t.Fatalf("missing-abstract: %v", err)
		}
		fInfo := decodeAuditFindings(t, decodeAuditMap(t, outInfo)["findings"])
		if len(fInfo) != 1 || fInfo[0].Severity != sevInfo {
			t.Fatalf("missing_abstract severity = %q, want %q", fInfo[0].Severity, sevInfo)
		}
		// Also verify via direct helper for the two kinds.
		if got := itemsAuditFindingSeverity("missing_doi"); got != sevHigh {
			t.Fatalf("itemsAuditFindingSeverity(missing_doi) = %q, want %q", got, sevHigh)
		}
		if got := itemsAuditFindingSeverity("missing_abstract"); got != sevInfo {
			t.Fatalf("itemsAuditFindingSeverity(missing_abstract) = %q, want %q", got, sevInfo)
		}
	})

	t.Run("verify_files", func(t *testing.T) {
		t.Run("broken_attachment_missing_file", func(t *testing.T) {
			brokenPath := filepath.Join(t.TempDir(), "does-not-exist.pdf")
			// Ensure it really does not exist.
			_ = os.Remove(brokenPath)
			fileMap := map[string]string{
				"BAD1": "file://" + brokenPath,
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				parts := strings.Split(r.URL.Path, "/")
				var key string
				for i, p := range parts {
					if p == "items" && i+1 < len(parts) {
						key = parts[i+1]
						break
					}
				}
				if u, ok := fileMap[key]; ok {
					fmt.Fprint(w, u)
					return
				}
				http.NotFound(w, r)
			}))
			t.Cleanup(srv.Close)
			auditIsolateEnv(t, srv.URL)
			auditSeedDB(t, []json.RawMessage{
				json.RawMessage(`{"key":"BAD1","version":1,"data":{"key":"BAD1","itemType":"attachment","parentItem":"P1","contentType":"application/pdf","linkMode":"imported_file","filename":"bad.pdf"}}`),
			})
			flags := &rootFlags{asJSON: true, timeout: 2 * time.Second}
			out, _, err := runItemsAudit(t, flags, "--verify-files")
			if err != nil {
				t.Fatalf("verify-files broken: %v", err)
			}
			var payload struct {
				Checked  int              `json:"checked"`
				Broken   []map[string]any `json:"broken"`
				Findings []Finding        `json:"findings"`
			}
			if err := json.Unmarshal([]byte(out), &payload); err != nil {
				t.Fatalf("decode verify-files %q: %v", out, err)
			}
			if payload.Checked != 1 {
				t.Fatalf("checked = %d, want 1", payload.Checked)
			}
			if len(payload.Broken) != 1 {
				t.Fatalf("broken = %d, want 1: %+v", len(payload.Broken), payload.Broken)
			}
			if len(payload.Findings) != 1 {
				t.Fatalf("findings = %d, want 1: %+v", len(payload.Findings), payload.Findings)
			}
			f := payload.Findings[0]
			if f.Kind != "broken_attachment_file" {
				t.Fatalf("finding kind = %q, want broken_attachment_file", f.Kind)
			}
			if f.Severity != sevCritical {
				t.Fatalf("finding severity = %q, want %q", f.Severity, sevCritical)
			}
			if f.ItemKey != "BAD1" {
				t.Fatalf("finding item_key = %q, want BAD1", f.ItemKey)
			}
			if got := fmt.Sprintf("%v", payload.Broken[0]["reason"]); got != "missing" {
				t.Fatalf("broken reason = %q, want missing", got)
			}
			// Also directly exercise brokenAttachmentFindings and attachmentFileStatus helpers.
			broken := []map[string]any{{"key": "BAD1", "parent": "P1", "name": "bad.pdf", "path": brokenPath, "reason": "missing"}}
			findings := brokenAttachmentFindings(broken)
			if len(findings) != 1 || findings[0].Severity != sevCritical {
				t.Fatalf("brokenAttachmentFindings = %+v, want one critical", findings)
			}
		})

		t.Run("verify_files_real_file_no_finding", func(t *testing.T) {
			dir := t.TempDir()
			realPath := filepath.Join(dir, "real.pdf")
			if err := os.WriteFile(realPath, []byte("pdf"), 0o600); err != nil {
				t.Fatalf("write real.pdf: %v", err)
			}
			fileMap := map[string]string{
				"GOOD1": "file://" + realPath,
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				parts := strings.Split(r.URL.Path, "/")
				var key string
				for i, p := range parts {
					if p == "items" && i+1 < len(parts) {
						key = parts[i+1]
						break
					}
				}
				if u, ok := fileMap[key]; ok {
					fmt.Fprint(w, u)
					return
				}
				http.NotFound(w, r)
			}))
			t.Cleanup(srv.Close)
			auditIsolateEnv(t, srv.URL)
			auditSeedDB(t, []json.RawMessage{
				json.RawMessage(`{"key":"GOOD1","version":1,"data":{"key":"GOOD1","itemType":"attachment","parentItem":"P1","contentType":"application/pdf","linkMode":"imported_file","filename":"real.pdf"}}`),
			})
			flags := &rootFlags{asJSON: true, timeout: 2 * time.Second}
			out, _, err := runItemsAudit(t, flags, "--verify-files")
			if err != nil {
				t.Fatalf("verify-files good: %v", err)
			}
			var payload struct {
				Checked  int              `json:"checked"`
				Broken   []map[string]any `json:"broken"`
				Findings []Finding        `json:"findings"`
			}
			if err := json.Unmarshal([]byte(out), &payload); err != nil {
				t.Fatalf("decode verify-files %q: %v", out, err)
			}
			if payload.Checked != 1 {
				t.Fatalf("checked = %d, want 1", payload.Checked)
			}
			if len(payload.Broken) != 0 {
				t.Fatalf("broken = %d, want 0: %+v", len(payload.Broken), payload.Broken)
			}
			if len(payload.Findings) != 0 {
				t.Fatalf("findings = %d, want 0: %+v", len(payload.Findings), payload.Findings)
			}
		})
	})

	t.Run("human_summary_and_exit_code", func(t *testing.T) {
		// Clean library should exit 0 and report Scope line.
		auditIsolateEnv(t, "")
		auditSeedDB(t, []json.RawMessage{
			json.RawMessage(`{"key":"HCLEAN","version":1,"data":{"key":"HCLEAN","itemType":"journalArticle","title":"Clean","creators":[{"lastName":"Doe","creatorType":"author"}],"date":"2020","publicationTitle":"J","DOI":"10/clean","abstractNote":"abs","tags":[{"tag":"t"}]}}`),
			json.RawMessage(`{"key":"A_HCLEAN","version":1,"data":{"key":"A_HCLEAN","itemType":"attachment","parentItem":"HCLEAN","contentType":"application/pdf","linkMode":"imported_file","filename":"clean.pdf"}}`),
		})
		flagsClean := &rootFlags{timeout: 2 * time.Second}
		outClean, stderrClean, errClean := runItemsAudit(t, flagsClean)
		cleanExitCode := 0
		if errClean != nil {
			cleanExitCode = ExitCode(errClean)
		}
		if cleanExitCode != 0 {
			t.Fatalf("clean exit code = %d, want 0: %v; stdout=%q stderr=%q", cleanExitCode, errClean, outClean, stderrClean)
		}
		if !strings.Contains(outClean, "Scope:") {
			t.Fatalf("clean human output %q missing Scope", outClean)
		}
		if !strings.Contains(outClean, "missing-pdf") {
			t.Fatalf("clean human output %q missing check names", outClean)
		}

		// Library with findings should still exit 0 but human output reflects counts >0.
		auditIsolateEnv(t, "")
		auditSeedDB(t, []json.RawMessage{
			json.RawMessage(`{"key":"HDIRTY","version":1,"data":{"key":"HDIRTY","itemType":"journalArticle","title":"Dirty","creators":[{"lastName":"Doe","creatorType":"author"}],"date":"2020","publicationTitle":"J","DOI":"10/dirty","abstractNote":"abs","tags":[{"tag":"t"}]}}`),
			// No attachment on purpose => missing_pdf count should be 1.
		})
		flagsDirty := &rootFlags{timeout: 2 * time.Second}
		outDirty, stderrDirty, errDirty := runItemsAudit(t, flagsDirty)
		dirtyExitCode := 0
		if errDirty != nil {
			dirtyExitCode = ExitCode(errDirty)
		}
		if dirtyExitCode != 0 {
			t.Fatalf("dirty exit code = %d, want 0: %v; stdout=%q stderr=%q", dirtyExitCode, errDirty, outDirty, stderrDirty)
		}
		if !strings.Contains(outDirty, "Scope:") {
			t.Fatalf("dirty human output %q missing Scope", outDirty)
		}
		// JSON summary for dirty should show missing_pdf 1.
		auditIsolateEnv(t, "")
		auditSeedDB(t, []json.RawMessage{
			json.RawMessage(`{"key":"HDIRTY2","version":1,"data":{"key":"HDIRTY2","itemType":"journalArticle","title":"Dirty2"}}`),
		})
		flagsJSON := &rootFlags{asJSON: true, timeout: 2 * time.Second}
		outJSON, _, err := runItemsAudit(t, flagsJSON)
		if err != nil {
			t.Fatalf("dirty JSON: %v", err)
		}
		var sum itemsAuditSummary
		if err := json.Unmarshal([]byte(outJSON), &sum); err != nil {
			t.Fatalf("decode dirty summary %q: %v", outJSON, err)
		}
		if sum.MissingPDF == 0 {
			t.Fatalf("dirty summary missing_pdf = %d, want >0", sum.MissingPDF)
		}
	})
}
