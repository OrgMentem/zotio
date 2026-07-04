package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSyncResourceTerminalEmptyPageClearsCursorAndStoresCheckpoint(t *testing.T) {
	syncTestWithHumanFriendly(t, false)

	var starts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/items" {
			t.Errorf("server path = %q, want /items", r.URL.Path)
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Last-Modified-Version", "200")
		start := r.URL.Query().Get("start")
		starts = append(starts, start)
		switch start {
		case "0":
			fmt.Fprint(w, syncTestItemsJSON("first", 100))
		case "100":
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("unexpected start cursor %q", start)
			http.Error(w, "unexpected cursor", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	db := syncTestOpenStore(t)
	defer db.Close()

	result := syncResource(syncTestClient(server.URL), db, "items", 0, false, 0, false)
	if result.Err != nil || result.Warn != nil {
		t.Fatalf("syncResource result Err = %v Warn = %v", result.Err, result.Warn)
	}
	if result.Count != 100 {
		t.Fatalf("syncResource count = %d, want 100", result.Count)
	}
	if got, want := fmt.Sprint(starts), "[0 100]"; got != want {
		t.Fatalf("requested page starts = %s, want %s", got, want)
	}
	cursor, _, total, err := db.GetSyncState("items")
	if err != nil {
		t.Fatalf("GetSyncState(items): %v", err)
	}
	if cursor != "" || total != 100 {
		t.Fatalf("sync state cursor=%q total=%d, want cleared cursor and total=100", cursor, total)
	}
	if version, err := db.GetLibraryVersion("items"); err != nil || version != 200 {
		t.Fatalf("library version = %d (err %v), want 200 after terminal empty page", version, err)
	}
}
