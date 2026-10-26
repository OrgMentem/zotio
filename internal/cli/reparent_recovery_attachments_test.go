// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"zotio/internal/mutation"
)

func TestConnectorEmptyParentJournalAndRetryReapAttachments(t *testing.T) {
	for _, retryFails := range []bool{false, true} {
		name := "success"
		if retryFails {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			previous := mutationJournalRecorder
			mutationJournalRecorder = recordMutationJournal
			t.Cleanup(func() { mutationJournalRecorder = previous })
			fake := &reparentFake{tempParentKey: "TEMPABCD", saveAttachmentStatus: http.StatusInternalServerError}
			srv := fake.server(t)
			flags := reparentFlags(t, srv)
			flags.yes, flags.maxChanges = true, -1
			c, err := flags.newWriteClient()
			if err != nil {
				t.Fatal(err)
			}
			req := reparentRequest(t, "TARGET01")
			op := mutation.Op{ID: "attachments.add:001:stored", Key: req.ParentKey, Kind: "attachment_upload", Changes: []mutation.Change{{Field: "attachment", Add: req.Path}}, Apply: func() (string, any, error) {
				return applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, req)
			}}
			env, err := runMutation(context.Background(), flags, "attachments.add", []mutation.Op{op})
			if err == nil || env.Result == nil || env.Result.Items[0].Status != "conflict" {
				t.Fatalf("first run=%+v error=%v", env, err)
			}
			detail, _ := env.Result.Items[0].Reason.(map[string]any)
			if detail["committed"] != true || detail["temp_parent_key"] != "TEMPABCD" || detail["temp_parent_marker"] == "" {
				t.Fatalf("missing parent evidence: %#v", detail)
			}
			entries, err := mutation.ListEntries(helpersTestJournalDir(t))
			if err != nil || len(entries) != 1 || len(entries[0].Ops) != 1 {
				t.Fatalf("journal=%+v error=%v", entries, err)
			}
			fake.mu.Lock()
			fake.strandedParentKey = fake.tempParentKey
			fake.strandedChildDetached = true
			fake.tempParentKey = "TEMPBCDE"
			if !retryFails {
				fake.attachChildren = []string{"ATTACHAB"}
				fake.saveAttachmentStatus = 0
			}
			fake.mu.Unlock()
			fake.setMarkedAddedAt(time.Now().Add(-10 * time.Minute))
			status, _, err := op.Apply()
			if retryFails {
				if err == nil || status != "conflict" {
					t.Fatalf("retry status=%s error=%v", status, err)
				}
			} else if err != nil || status != "applied" {
				t.Fatalf("retry status=%s error=%v", status, err)
			}
			if !slices.Contains(fake.sequence(), "web.trash:TEMPABCD") {
				t.Fatalf("retry left earlier empty parent: %v", fake.sequence())
			}
		})
	}
}
