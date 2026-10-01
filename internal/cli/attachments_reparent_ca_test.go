// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"slices"
	"testing"
	"time"
)

// A run killed after it moved the attachment but before it trashed the
// temporary parent leaves the file on the target and an empty marked parent in
// the library. The retry no-ops on the target's content, so it is the only
// later run that ever looks: it must trash that parent once no live run can own
// it, and must leave it alone while one still could.
func TestConnectorReparentRetryReapsParentStrandedByCrashAfterMove(t *testing.T) {
	req := reparentRequest(t, "TARGET01")
	crashCtx, crash := context.WithCancel(context.Background())
	defer crash()
	fake := &reparentFake{
		tempParentKey:  "TEMP0001",
		attachChildren: []string{"ATTACH01"},
		movedChildMD5:  req.MD5,
		crashAfterMove: crash,
	}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	run := func(ctx context.Context) (string, any, error) {
		t.Helper()
		c, err := flags.newWriteClient()
		if err != nil {
			t.Fatalf("newWriteClient: %v", err)
		}
		return applyConnectorReparentUpload(ctx, reparentCmd(t), flags, c, req)
	}

	_, _, _ = run(crashCtx)
	first := fake.sequence()
	if !slices.Contains(first, "web.reparent") || !slices.Contains(first, "crash") || slices.Contains(first, "web.trash:TEMP0001") {
		t.Fatalf("first run = %v, want the move done and the trash cut off", first)
	}

	// Too soon: a parent this young may still belong to a live run.
	status, detail, err := run(context.Background())
	if err != nil || status != "no_op" {
		t.Fatalf("young retry status=%q detail=%v err=%v, want no_op", status, detail, err)
	}
	if slices.Contains(fake.sequence(), "web.trash:TEMP0001") {
		t.Fatalf("retry trashed a parent a live run could still own: %v", fake.sequence())
	}

	fake.setMarkedAddedAt(time.Now().Add(-connectorTempParentAbandonedAfter - time.Minute))
	before := len(fake.sequence())
	status, detail, err = run(context.Background())
	if err != nil || status != "no_op" {
		t.Fatalf("aged retry status=%q detail=%v err=%v, want no_op", status, detail, err)
	}
	retry := fake.sequence()[before:]
	if !slices.Equal(retry, []string{"web.trash:TEMP0001"}) {
		t.Fatalf("aged retry = %v, want only the stranded parent trashed", retry)
	}
	trashed, _ := detail.(map[string]any)["temp_parents_trashed"].([]string)
	if !slices.Equal(trashed, []string{"TEMP0001"}) {
		t.Fatalf("detail = %v, want the trashed parent named", detail)
	}
}
