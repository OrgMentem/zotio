// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"zotio/internal/store"
)

func TestKeysDemoPrismaInvalidKeyExplainsAlphabet(t *testing.T) {
	for _, value := range []string{"ABCD1234", "ABCDEFG", "ABCDEFG22", "ABCDEFG0", "ABCDEFGO", "smith2020"} {
		err := validateKeyArg("item", value)
		if err == nil || ExitCode(err) != 2 {
			t.Fatalf("validate %q = %v, want usage error", value, err)
		}
		for _, want := range []string{"exactly 8 characters", "2-9 and A-Z", "no 0, 1 or O"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("validate %q = %v, missing %q", value, err, want)
			}
		}
		if value == "smith2020" && !strings.Contains(err.Error(), "zotio items find --citekey smith2020") {
			t.Errorf("citation-key hint missing: %v", err)
		}
	}
}

func TestKeysDemoPrismaFixtureKeysAndReferences(t *testing.T) {
	var fixture map[string][]struct {
		Key  string `json:"key"`
		Data struct {
			Key         string   `json:"key"`
			ParentItem  string   `json:"parentItem"`
			Collections []string `json:"collections"`
		} `json:"data"`
	}
	if err := json.Unmarshal(demoLibraryFixture, &fixture); err != nil {
		t.Fatal(err)
	}
	keys := make(map[string]bool)
	for kind, rows := range fixture {
		for _, row := range rows {
			if err := validateKeyArg(kind, row.Key); err != nil {
				t.Error(err)
			}
			if row.Key != row.Data.Key || keys[row.Key] {
				t.Errorf("inconsistent or duplicate key %q", row.Key)
			}
			keys[row.Key] = true
		}
	}
	for _, rows := range fixture {
		for _, row := range rows {
			refs := append([]string{}, row.Data.Collections...)
			if row.Data.ParentItem != "" {
				refs = append(refs, row.Data.ParentItem)
			}
			for _, key := range refs {
				if !keys[key] {
					t.Errorf("%s references missing key %q", row.Key, key)
				}
			}
		}
	}
	isolateDemoEnv(t, "1")
	runDemoCmd(t)
	key := fixture["items"][0].Key
	out, stderr, err := executeRootForDeliverTest(t, "--data-source", "local", "--json", "items", "get", key)
	if err != nil || !strings.Contains(out, key) {
		t.Fatalf("items get %s: %v; stdout=%s; stderr=%s", key, err, out, stderr)
	}
}

func TestKeysDemoPrismaRequiresCompletedItemsMirror(t *testing.T) {
	for _, state := range []string{"never", "unfinished", "complete-empty"} {
		t.Run(state, func(t *testing.T) {
			isolateDemoEnv(t, "1")
			path, err := defaultDBPath("zotio")
			if err != nil {
				t.Fatal(err)
			}
			db, err := store.OpenWithContext(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := db.UpsertBatch("collections", []json.RawMessage{json.RawMessage(`{"key":"ABCD2345","version":1,"data":{"key":"ABCD2345","name":"Only collections"}}`)}); err != nil {
				t.Fatal(err)
			}
			if err := db.SaveSyncState("collections", "", 1); err != nil {
				t.Fatal(err)
			}
			switch state {
			case "unfinished":
				err = db.SaveSyncResumeStateContext(context.Background(), "items", "100", "items?limit=100", 1)
			case "complete-empty":
				err = db.SaveSyncState("items", "", 0)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			cmd := newLibraryPrismaCmd(&rootFlags{asJSON: true, dataSource: "local", maxChanges: -1})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(nil)
			err = cmd.Execute()
			if state == "complete-empty" {
				if err != nil {
					t.Fatalf("completed empty mirror: %v; output=%s", err, out.String())
				}
				var report libraryPrismaReport
				if err := json.Unmarshal(out.Bytes(), &report); err != nil {
					t.Fatalf("decode completed empty mirror report: %v; output=%s", err, out.String())
				}
				if report.SchemaVersion != 1 || report.Identified.Total != 0 {
					t.Fatalf("completed empty mirror report = %+v", report)
				}
				return
			}
			if err == nil || ExitCode(err) != 9 {
				t.Fatalf("%s mirror: %v; output=%s", state, err, out.String())
			}
			if !strings.Contains(out.String(), "sync --resources items") || strings.Contains(out.String(), `"identified"`) {
				t.Fatalf("expected sync remediation without counts: %s", out.String())
			}
		})
	}
}
