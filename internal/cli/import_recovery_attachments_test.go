// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zotio/internal/mutation"
)

func TestImportApplyUncertainWebUploadResumesExistingParentAttachments(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	previous := mutationJournalRecorder
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = previous })
	fake := newFakeZoteroUpload(t, "")
	fake.dropRegisterResponse = "lose"
	path := writeUploadFixture(t, "paper.pdf", []byte(uploadFixturePDF))
	manifest := writeImportApplyTestManifest(t, importManifest{SchemaVersion: importManifestSchemaVersion, Entries: []importManifestEntry{{
		Path: path, Action: "create", Status: "resolved", Title: "Paper", Item: map[string]any{"itemType": "journalArticle", "title": "Paper"},
	}}})
	flags := &rootFlags{asJSON: true, yes: true, via: "web", maxChanges: -1, configPath: testConfigFile(t, fake.srv.URL+"/users/0")}
	env, _, err := runImportApplyTestCmdWithFlags(t, flags, []string{"--attach-mode", "stored", manifest})
	if err == nil || env.Result == nil || len(env.Result.Items) != 1 {
		t.Fatalf("env=%+v error=%v", env, err)
	}
	result := env.Result.Items[0]
	reason, _ := result.Reason.(map[string]any)
	if result.Status != "conflict" || reason["attachment_key"] != "ATT1" || reason["registered"] != "unknown" || reason["committed"] != true {
		t.Fatalf("lost uncertain upload evidence: %+v", result)
	}
	if !strings.Contains(reason["message"].(string), "zotio attachments add PARENTAB") {
		t.Fatalf("recovery=%v", reason)
	}
	entries, err := mutation.ListEntries(helpersTestJournalDir(t))
	if err != nil || len(entries) != 1 || len(entries[0].Ops) != 1 {
		t.Fatalf("journal=%+v error=%v", entries, err)
	}
	env, _, err = runAttachmentsAdd(t, flags, []string{"add", "PARENTAB", path})
	if err != nil || env.Result == nil || env.Result.Summary.Applied != 1 {
		t.Fatalf("resume=%+v error=%v", env, err)
	}
	if creates, _, _ := fake.snapshot(); creates != 1 || fake.parentSnapshot() != 1 {
		t.Fatalf("resume duplicated parent or child: children=%d parents=%d", creates, fake.parentSnapshot())
	}
}

// The first session deliberately bypasses runMutation: its durable intent must
// survive even when no operation has reached the engine's journal checkpoint.
func TestImportApplyConnectorIntentPrecedesUploadAndBlocksReplayAttachments(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := writeUploadFixture(t, "paper.pdf", []byte(uploadFixturePDF))
	hash, err := fileMD5(path)
	if err != nil {
		t.Fatal(err)
	}
	fake := startImportApplySessionFake(t, "", map[string]string{"Paper A": hash})
	flags := &rootFlags{asJSON: true, yes: true, via: "connector", timeout: time.Second, maxChanges: -1, configPath: testConfigFile(t, "http://127.0.0.1:23119/api/users/0")}
	manifest := importManifest{SchemaVersion: importManifestSchemaVersion, Entries: []importManifestEntry{{Path: path, Action: "create", Status: "resolved", Title: "Paper A", Item: map[string]any{"itemType": "journalArticle", "title": "Paper A"}}}}
	journal, err := journalDir()
	if err != nil {
		t.Fatal(err)
	}
	sawIntent := false
	fake.onAttach = func(string) {
		paths, err := filepath.Glob(filepath.Join(journal+".import-pending", "*.json"))
		if err != nil || len(paths) != 1 {
			t.Errorf("intent before upload: paths=%v error=%v", paths, err)
			return
		}
		raw, err := os.ReadFile(paths[0])
		if err != nil {
			t.Error(err)
			return
		}
		var rec importApplyPending
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Error(err)
			return
		}
		if rec.Session == "" || rec.FileMD5 != hash || rec.EntryNumber != 1 || len(rec.ConnectorKeys) != 1 || fake.titleByConn[rec.ConnectorKeys[0]] != "Paper A" {
			t.Errorf("intent=%+v", rec)
			return
		}
		sawIntent = true
	}
	cmd := newImportApplyCmd(flags)
	cmd.SetContext(context.Background())
	ops := importApplyOps(cmd, flags, nil, nil, manifest, "stored", false, "")
	status, detail, err := ops[0].Apply()
	if err != nil || status != "applied" || !sawIntent {
		t.Fatalf("first session: status=%s detail=%v error=%v intent=%v", status, detail, err, sawIntent)
	}
	// New command/session, same manifest and bytes, no in-memory session state.
	retry := newImportApplyCmd(flags)
	retry.SetContext(context.Background())
	retryOps := importApplyOps(retry, flags, nil, nil, manifest, "stored", false, "")
	status, _, err = retryOps[0].Apply()
	if err == nil || status != "failed" || !strings.Contains(err.Error(), "refusing automatic replay") {
		t.Fatalf("retry status=%s error=%v", status, err)
	}
	fake.mu.Lock()
	saves := fake.saveItems
	fake.mu.Unlock()
	if saves != 1 {
		t.Fatalf("replayed SaveItems %d times", saves)
	}
	if err := retry.Flags().Set("reimport", "true"); err != nil {
		t.Fatal(err)
	}
	override := importApplyOps(retry, flags, nil, nil, manifest, "stored", false, "")
	if status, detail, err := override[0].Apply(); err != nil || status != "applied" {
		t.Fatalf("explicit replay: %s %v %v", status, detail, err)
	}
}

func TestImportApplyConnectorIntentRetiresOnlyAfterJournalAttachments(t *testing.T) {
	for _, failure := range []string{"", "Paper B"} {
		name := "success"
		if failure != "" {
			name = "partial"
		}
		t.Run(name, func(t *testing.T) {
			_, env, runErr := runImportApplySessionManifest(t, failure)
			if failure == "" && (runErr != nil || env.Journal == nil) {
				t.Fatalf("successful import was not journaled: env=%+v error=%v", env, runErr)
			}
			if failure != "" && runErr == nil {
				t.Fatal("partial session unexpectedly succeeded")
			}
			journal, err := journalDir()
			if err != nil {
				t.Fatal(err)
			}
			paths, err := filepath.Glob(filepath.Join(journal+".import-pending", "*.json"))
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if failure != "" {
				want = 3
			}
			if len(paths) != want {
				t.Fatalf("intent records=%v, want %d", paths, want)
			}
		})
	}
}

func TestImportApplyMissingFileCannotShrinkReplayGuardAttachments(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	manifest := importManifest{SchemaVersion: importManifestSchemaVersion}
	hashes := map[string]string{}
	for _, title := range []string{"Paper A", "Paper B"} {
		path := writeUploadFixture(t, title+".pdf", []byte(uploadFixturePDF))
		hash, err := fileMD5(path)
		if err != nil {
			t.Fatal(err)
		}
		hashes[title] = hash
		manifest.Entries = append(manifest.Entries, importManifestEntry{
			Path: path, Action: "create", Status: "resolved", Title: title,
			Item: map[string]any{"itemType": "journalArticle", "title": title},
		})
	}
	fake := startImportApplySessionFake(t, "", hashes)
	flags := &rootFlags{asJSON: true, yes: true, via: "connector", timeout: time.Second, maxChanges: -1, configPath: testConfigFile(t, "http://127.0.0.1:23119/api/users/0")}
	cmd := newImportApplyCmd(flags)
	cmd.SetContext(context.Background())
	ops := importApplyOps(cmd, flags, nil, nil, manifest, "stored", false, "")
	if status, detail, err := ops[0].Apply(); err != nil || status != "applied" {
		t.Fatalf("first session: status=%s detail=%v error=%v", status, detail, err)
	}
	// The batch committed, but no journal checkpoint or cleanup ran.
	if err := os.Remove(manifest.Entries[0].Path); err != nil {
		t.Fatal(err)
	}
	retry := newImportApplyCmd(flags)
	retry.SetContext(context.Background())
	retryOps := importApplyOps(retry, flags, nil, nil, manifest, "stored", false, "")
	for _, op := range retryOps {
		status, _, err := op.Apply()
		if status != "failed" || err == nil || !strings.Contains(err.Error(), "refusing automatic replay") {
			t.Fatalf("retry after source removal: status=%s error=%v", status, err)
		}
	}
	fake.mu.Lock()
	saves := fake.saveItems
	fake.mu.Unlock()
	if saves != 1 {
		t.Fatalf("source removal replayed SaveItems: %d calls", saves)
	}
}
