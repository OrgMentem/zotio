// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
)

func TestRtwTailTagIdentityReachesNDJSONAndTrigger(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted=%t", deleted), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Last-Modified-Version", "7")
				if r.URL.Path == "/deleted" {
					if deleted {
						_, _ = fmt.Fprint(w, `{"tags":["research"]}`)
					} else {
						_, _ = fmt.Fprint(w, `{}`)
					}
					return
				}
				_, _ = fmt.Fprint(w, `[{"tag":"research","version":7},{"tag":"method","version":7}]`)
			}))
			defer srv.Close()
			c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
			c.NoCache = true
			db := tailTestStore(t)
			if err := db.SaveLibraryVersion("tail:tags", c.BaseURL, 6); err != nil {
				t.Fatalf("seed cursor: %v", err)
			}
			var out bytes.Buffer
			var batch tailChangeBatch
			n, err := emitChangesWithHook(context.Background(), c, db, "tags", "/tags", DeliverSink{Scheme: "stdout"}, &out, func(b tailChangeBatch) error {
				batch = b
				return nil
			})
			if err != nil {
				t.Fatalf("tail poll: %v", err)
			}
			want := []string{"research", "method"}
			if deleted {
				want = append(want, "research")
			}
			var keys []string
			decoder := json.NewDecoder(&out)
			for range n {
				var event struct {
					Key string `json:"key"`
				}
				if err := decoder.Decode(&event); err != nil {
					t.Fatalf("decode NDJSON: %v", err)
				}
				keys = append(keys, event.Key)
			}
			if !reflect.DeepEqual(keys, want) {
				t.Fatalf("NDJSON keys = %v, want %v", keys, want)
			}
			var batchKeys []string
			for _, event := range batch.Events {
				batchKeys = append(batchKeys, event.Key)
			}
			if !reflect.DeepEqual(batchKeys, want) {
				t.Fatalf("trigger event keys = %v, want %v", batchKeys, want)
			}
			trigger, reason := tailWorkflowTrigger("tags", batch)
			if reason != "" {
				t.Fatalf("trigger skipped: %s", reason)
			}
			wantUpserts := []string{"research", "method"}
			wantDeletes := []string{}
			if deleted {
				wantUpserts = []string{"method"}
				wantDeletes = []string{"research"}
			}
			if !reflect.DeepEqual(trigger.UpsertKeys, wantUpserts) {
				t.Fatalf("trigger upsert keys = %v, want %v", trigger.UpsertKeys, wantUpserts)
			}
			if !reflect.DeepEqual(trigger.DeleteKeys, wantDeletes) {
				t.Fatalf("trigger delete keys = %v, want %v", trigger.DeleteKeys, wantDeletes)
			}
		})
	}
}
