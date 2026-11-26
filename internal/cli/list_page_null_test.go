// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
)

// A paged walker ends on a short page. json.Unmarshal reads a JSON null body
// into a nil slice with no error, so each walker must refuse null itself; these
// tests drive every walker that used to read null as the end of its list.

func nullTestClient(t *testing.T, handler http.HandlerFunc) (*client.Client, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Last-Modified-Version", "7")
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	c := client.New(&config.Config{BaseURL: srv.URL}, 5*time.Second, 0)
	c.NoCache = true
	return c, &requests
}

// pageOrNull serves row(i) for start offsets below nullAt in full pages of
// pageSize, and the JSON literal null from nullAt on.
func pageOrNull(nullAt, pageSize int, row func(i int) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start, _ := strconv.Atoi(r.URL.Query().Get("start"))
		if start >= nullAt {
			_, _ = w.Write([]byte("null"))
			return
		}
		rows := make([]string, 0, pageSize)
		for i := start; i < start+pageSize; i++ {
			rows = append(rows, row(i))
		}
		_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
	}
}

func requireNullRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("got nil error, want a refusal of the JSON null page")
	}
	if !strings.Contains(err.Error(), "JSON null") {
		t.Fatalf("error = %v, want it to name the JSON null page", err)
	}
}

func TestDecodeListPage(t *testing.T) {
	if _, err := decodeListPage[json.RawMessage]([]byte(" null\n"), "things"); err == nil || !strings.Contains(err.Error(), "JSON null") {
		t.Fatalf("null page error = %v, want a JSON null refusal", err)
	}
	if _, err := decodeListPage[json.RawMessage]([]byte(`{"a":1}`), "things"); err == nil {
		t.Fatal("an object page decoded, want an error")
	}
	empty, err := decodeListPage[json.RawMessage]([]byte(`[]`), "things")
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty array = %v, %v, want an empty page and no error", empty, err)
	}
	rows, err := decodeListPage[map[string]any]([]byte(`[{"a":1},{"a":2}]`), "things")
	if err != nil || len(rows) != 2 {
		t.Fatalf("array = %v, %v, want 2 rows", rows, err)
	}
}

type nullChildrenReader struct {
	bodies []string
}

func (r *nullChildrenReader) GetFromWriteBaseWithVersionContext(_ context.Context, _ string, params map[string]string) (json.RawMessage, int, error) {
	start, _ := strconv.Atoi(params["start"])
	return json.RawMessage(r.bodies[start/connectorChildrenPageSize]), 1, nil
}

func linkedChildrenPage(n int) string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = fmt.Sprintf(`{"key":"CH%06d","data":{"itemType":"attachment","linkMode":"imported_file"}}`, i)
	}
	return "[" + strings.Join(rows, ",") + "]"
}

func TestFindLinkedURLAttachmentRefusesNullPage(t *testing.T) {
	for name, bodies := range map[string][]string{
		"first page":     {"null"},
		"mid pagination": {linkedChildrenPage(connectorChildrenPageSize), "null"},
	} {
		t.Run(name, func(t *testing.T) {
			reader := &nullChildrenReader{bodies: bodies}
			key, err := findLinkedURLAttachment(context.Background(), reader, "PARENT01", "https://example.org/oa.pdf")
			requireNullRefusal(t, err)
			if key != "" {
				t.Fatalf("key = %q, want none", key)
			}
		})
	}
}

func TestChildRowsRefusesNullPage(t *testing.T) {
	c, _ := nullTestClient(t, pageOrNull(connectorChildrenPageSize, connectorChildrenPageSize, func(i int) string {
		return fmt.Sprintf(`{"key":"CH%06d","data":{"itemType":"attachment"}}`, i)
	}))
	rows, err := attachmentChildRows(c, "PARENT01")
	requireNullRefusal(t, err)
	if rows != nil {
		t.Fatalf("rows = %d, want no partial list", len(rows))
	}
}

type nullCollectionReader struct{ handler http.HandlerFunc }

func (r nullCollectionReader) Get(path string, params map[string]string) (json.RawMessage, error) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path+"?start="+params["start"], nil)
	r.handler(rec, req)
	return json.RawMessage(rec.Body.Bytes()), nil
}

func TestCollectionExportRefusesNullPages(t *testing.T) {
	t.Run("items", func(t *testing.T) {
		reader := nullCollectionReader{handler: pageOrNull(zoteroPageMax, zoteroPageMax, func(i int) string {
			return fmt.Sprintf(`{"key":"IT%06d"}`, i)
		})}
		var items []json.RawMessage
		err := collectCollectionItems(reader, "COLL0001", "", true, zoteroPageMax, map[string]bool{}, &items)
		requireNullRefusal(t, err)
	})
	t.Run("subcollections", func(t *testing.T) {
		reader := nullCollectionReader{handler: pageOrNull(zoteroPageMax, zoteroPageMax, func(i int) string {
			return fmt.Sprintf(`{"key":"SC%06d"}`, i)
		})}
		keys, err := fetchSubcollectionKeys(reader, "COLL0001")
		requireNullRefusal(t, err)
		if keys != nil {
			t.Fatalf("keys = %d, want no partial subtree list", len(keys))
		}
	})
}

func TestSnapshotVerifyRefusesNullPage(t *testing.T) {
	c, _ := nullTestClient(t, pageOrNull(100, 100, func(i int) string {
		return fmt.Sprintf(`{"key":"IT%06d","version":1,"data":{"key":"IT%06d","version":1}}`, i, i)
	}))
	items, err := fetchSnapshotItemsForVerify(context.Background(), c, "/items", nil, &rootFlags{})
	requireNullRefusal(t, err)
	if items != nil {
		t.Fatalf("items = %d, want no partial snapshot", len(items))
	}
}

type nullBatchClient struct{ syncHTTPClient }

func (nullBatchClient) GetWithVersion(string, map[string]string) (json.RawMessage, int, error) {
	return json.RawMessage("null"), 1, nil
}

func (nullBatchClient) GetWithVersionContext(context.Context, string, map[string]string) (json.RawMessage, int, error) {
	return json.RawMessage("null"), 1, nil
}

func TestReapedCollectionRefetchRefusesNullBatch(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	ctx := context.Background()
	db := recoverySeed(t, filepath.Join(t.TempDir(), "mirror.db"))
	defer db.Close()

	var out incrementalReapOutcome
	if refreshReapedCollectionMembers(ctx, nullBatchClient{}, db, []string{recoveryCollKey}, nil, &out) {
		t.Fatal("refresh reported success for a null itemKey batch; the collection row would be reaped with its members unrepaired")
	}
	if !out.incomplete {
		t.Error("a null itemKey batch was not reported as an incomplete reconciliation")
	}
	if out.membersSkipped != 0 {
		t.Errorf("membersSkipped = %d, want 0: a null batch must not mark every member gone", out.membersSkipped)
	}
}

func TestTagSelectorsRefuseNullPage(t *testing.T) {
	handler := pageOrNull(100, 100, func(i int) string {
		return fmt.Sprintf(`{"key":"IT%06d","version":1,"data":{"key":"IT%06d","version":1,"tags":[{"tag":"old"}]}}`, i, i)
	})
	t.Run("delete", func(t *testing.T) {
		c, _ := nullTestClient(t, handler)
		keys, matched, err := tagsDeleteCarrierKeys(c, []string{"old"}, 100)
		requireNullRefusal(t, err)
		if keys != nil || matched != 0 {
			t.Fatalf("keys=%d matched=%d, want no partial selection", len(keys), matched)
		}
	})
	t.Run("rename", func(t *testing.T) {
		c, _ := nullTestClient(t, handler)
		updates, matched, err := listTagRenameUpdates(c, "old", "new", 100)
		requireNullRefusal(t, err)
		if updates != nil || matched != 0 {
			t.Fatalf("updates=%d matched=%d, want no partial rename set", len(updates), matched)
		}
	})
	t.Run("rename first page", func(t *testing.T) {
		c, _ := nullTestClient(t, pageOrNull(0, 100, nil))
		_, _, err := listTagRenameUpdates(c, "old", "new", 100)
		requireNullRefusal(t, err)
	})
}

func TestTagRenameEscapesLeadingDash(t *testing.T) {
	var queries []string
	c, _ := nullTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query().Get("tag"))
		_, _ = w.Write([]byte(`[{"key":"IT000001","version":1,"data":{"key":"IT000001","version":1,"tags":[{"tag":"-draft"}]}}]`))
	})
	updates, matched, err := listTagRenameUpdates(c, "-draft", "draft", 100)
	if err != nil {
		t.Fatalf("listTagRenameUpdates: %v", err)
	}
	if len(queries) != 1 || queries[0] != `\-draft` {
		t.Fatalf("tag queries = %q, want the dash escaped so Zotero does not negate the filter", queries)
	}
	if len(updates) != 1 || matched != 1 {
		t.Fatalf("updates=%d matched=%d, want 1 and 1", len(updates), matched)
	}
	if got := zoteroLiteralTagQuery("plain"); got != "plain" {
		t.Errorf("zoteroLiteralTagQuery(plain) = %q", got)
	}
}

func TestCreateChildNoteRefusesNullDiscoveryPage(t *testing.T) {
	const parentKey = "PARENTK9"
	var posts atomic.Int64
	c, _ := nullTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			_, _ = w.Write([]byte(`{"success":{"0":"NOTEKEY1"}}`))
			return
		}
		_, _ = w.Write([]byte("null"))
	})
	_, err := createChildNote(c, parentKey, "<p>body</p>", "")
	if err == nil {
		t.Fatal("createChildNote succeeded although discovery could not prove no managed child exists")
	}
	requireNullRefusal(t, err)
	if posts.Load() != 0 {
		t.Fatalf("posts = %d, want no create after an unprovable discovery", posts.Load())
	}
}

func TestLiveTrashRefusesNullPage(t *testing.T) {
	t.Run("first page", func(t *testing.T) {
		c, _ := nullTestClient(t, pageOrNull(0, 100, nil))
		_, _, err := fetchAllLiveTrash(context.Background(), c, &rootFlags{}, "/items/trash")
		requireNullRefusal(t, err)
	})
	t.Run("mid pagination", func(t *testing.T) {
		c, _ := nullTestClient(t, pageOrNull(zoteroPageMax, zoteroPageMax, func(i int) string {
			return fmt.Sprintf(`{"key":"TR%06d"}`, i)
		}))
		_, _, err := fetchAllLiveTrash(context.Background(), c, &rootFlags{dryRun: true}, "/items/trash")
		requireNullRefusal(t, err)
	})
}

func TestConnectorTargetRefusesNullCollectionsPage(t *testing.T) {
	c, _ := nullTestClient(t, pageOrNull(100, 100, func(i int) string {
		return fmt.Sprintf(`{"key":"CO%06d","data":{"key":"CO%06d","name":"c%d"}}`, i, i, i)
	}))
	flags := &rootFlags{configPath: testConfigFile(t, c.BaseURL)}
	_, err := apiCollectionPath(flags, "CO999999")
	requireNullRefusal(t, err)
}

func TestItemsFilePagesChildrenToFindPDF(t *testing.T) {
	var starts []string
	c, _ := nullTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		start := r.URL.Query().Get("start")
		starts = append(starts, start)
		var rows []string
		switch start {
		case "0":
			for i := range connectorChildrenPageSize {
				rows = append(rows, fmt.Sprintf(`{"key":"AN%06d","data":{"key":"AN%06d","itemType":"annotation","parentItem":"PARENT01"}}`, i, i))
			}
		default:
			rows = append(rows, `{"key":"PDFKEY01","data":{"key":"PDFKEY01","itemType":"attachment","contentType":"application/pdf","parentItem":"PARENT01"}}`)
		}
		_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
	})
	key, err := findParentPDFAttachmentKey(c, "PARENT01")
	if err != nil {
		t.Fatalf("findParentPDFAttachmentKey: %v", err)
	}
	if key != "PDFKEY01" {
		t.Fatalf("key = %q, want the PDF past the first page", key)
	}
	if strings.Join(starts, ",") != "0,100" {
		t.Fatalf("starts = %v, want 0 then 100", starts)
	}

	c, _ = nullTestClient(t, pageOrNull(connectorChildrenPageSize, connectorChildrenPageSize, func(i int) string {
		return fmt.Sprintf(`{"key":"AN%06d","data":{"itemType":"annotation","parentItem":"PARENT01"}}`, i)
	}))
	_, err = findParentPDFAttachmentKey(c, "PARENT01")
	requireNullRefusal(t, err)
}

// listServer serves total rows at /items, honoring limit (capped at 100) and
// start, with the API's default page of 25 when no limit is sent.
func listServer(t *testing.T, total int, sendTotal bool) (*client.Client, *[]string) {
	t.Helper()
	var requests []string
	c, _ := nullTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		requests = append(requests, q.Get("start")+"/"+q.Get("limit"))
		limit, err := strconv.Atoi(q.Get("limit"))
		if err != nil || limit <= 0 {
			limit = 25
		}
		if limit > 100 {
			limit = 100
		}
		start, _ := strconv.Atoi(q.Get("start"))
		end := start + limit
		if end > total {
			end = total
		}
		rows := []string{}
		for i := start; i < end; i++ {
			rows = append(rows, fmt.Sprintf(`{"key":"K%06d"}`, i))
		}
		if sendTotal {
			w.Header().Set("Total-Results", strconv.Itoa(total))
		}
		_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
	})
	return c, &requests
}

func listCount(t *testing.T, data json.RawMessage) int {
	t.Helper()
	var rows []json.RawMessage
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	return len(rows)
}

func TestLiveListReadWalksPagesForLimitZero(t *testing.T) {
	for _, tc := range []struct {
		name         string
		params       map[string]string
		wantRows     int
		wantRequests []string
	}{
		{"limit 0 reads everything", map[string]string{}, 230, []string{"0/100", "100/100", "200/100"}},
		{"limit above one page stops at the limit", map[string]string{"limit": "120"}, 120, []string{"0/100", "100/20"}},
		{"explicit start walks from there", map[string]string{"start": "100"}, 130, []string{"100/100", "200/100"}},
		{"a window within one page stays one request", map[string]string{"limit": "50"}, 50, []string{"/50"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, requests := listServer(t, 230, true)
			data, prov, err := resolveListRead(context.Background(), c, &rootFlags{dataSource: "live"}, "items", false, "/items", tc.params)
			if err != nil {
				t.Fatalf("resolveListRead: %v", err)
			}
			if prov.Source != "live" {
				t.Errorf("source = %q, want live", prov.Source)
			}
			if got := listCount(t, data); got != tc.wantRows {
				t.Errorf("rows = %d, want %d", got, tc.wantRows)
			}
			if strings.Join(*requests, " ") != strings.Join(tc.wantRequests, " ") {
				t.Errorf("requests = %v, want %v", *requests, tc.wantRequests)
			}
		})
	}
}

func TestLiveListReadAcceptsEmptyPageBeyondTotal(t *testing.T) {
	// The desktop API clamps an offset past the end and returns [].
	c, _ := listServer(t, 30, true)
	data, _, err := resolveListRead(context.Background(), c, &rootFlags{dataSource: "live"}, "items", false, "/items", map[string]string{"start": "100"})
	if err != nil {
		t.Fatalf("resolveListRead: %v", err)
	}
	if got := listCount(t, data); got != 0 {
		t.Fatalf("rows = %d, want an empty list", got)
	}

	// A non-empty page beyond the reported total is still refused.
	c, _ = nullTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Total-Results", "5")
		_, _ = w.Write([]byte(`[{"key":"K000001"}]`))
	})
	_, _, err = resolveListRead(context.Background(), c, &rootFlags{dataSource: "live"}, "items", false, "/items", map[string]string{"start": "100"})
	if err == nil || !strings.Contains(err.Error(), "Total-Results") {
		t.Fatalf("error = %v, want a Total-Results refusal for a non-empty page past the total", err)
	}
}

func TestLiveListReadShortAnswerIsOneRequest(t *testing.T) {
	c, requests := listServer(t, 7, false)
	data, _, err := resolveListRead(context.Background(), c, &rootFlags{dataSource: "live"}, "searches", false, "/searches", map[string]string{})
	if err != nil {
		t.Fatalf("resolveListRead: %v", err)
	}
	if got := listCount(t, data); got != 7 {
		t.Fatalf("rows = %d, want 7", got)
	}
	if len(*requests) != 1 {
		t.Fatalf("requests = %v, want a single read", *requests)
	}
}

func TestLiveListReadRefusesPartialOrMalformedPages(t *testing.T) {
	fullPage := func() string {
		rows := make([]string, zoteroPageMax)
		for i := range rows {
			rows[i] = fmt.Sprintf(`{"key":"K%06d"}`, i)
		}
		return "[" + strings.Join(rows, ",") + "]"
	}
	t.Run("null page", func(t *testing.T) {
		c, _ := nullTestClient(t, pageOrNull(zoteroPageMax, zoteroPageMax, func(i int) string {
			return fmt.Sprintf(`{"key":"K%06d"}`, i)
		}))
		_, _, err := resolveListRead(context.Background(), c, &rootFlags{dataSource: "live"}, "items", false, "/items", map[string]string{})
		requireNullRefusal(t, err)
	})
	t.Run("Total-Results disagrees with the rows received", func(t *testing.T) {
		c, _ := nullTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Total-Results", "130")
			if r.URL.Query().Get("start") == "0" {
				_, _ = w.Write([]byte(fullPage()))
				return
			}
			_, _ = w.Write([]byte(`[]`))
		})
		_, _, err := resolveListRead(context.Background(), c, &rootFlags{dataSource: "live"}, "items", false, "/items", map[string]string{})
		if err == nil || !strings.Contains(err.Error(), "Total-Results") {
			t.Fatalf("error = %v, want a Total-Results refusal", err)
		}
	})
	t.Run("server ignores start", func(t *testing.T) {
		c, _ := nullTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(fullPage()))
		})
		_, _, err := resolveListRead(context.Background(), c, &rootFlags{dataSource: "live"}, "items", false, "/items", map[string]string{})
		if err == nil || !strings.Contains(err.Error(), "ignored start") {
			t.Fatalf("error = %v, want an ignored-start refusal", err)
		}
	})
	t.Run("non-JSON format is one request", func(t *testing.T) {
		c, requests := nullTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("K1\nK2\n"))
		})
		data, _, err := resolveListRead(context.Background(), c, &rootFlags{dataSource: "live"}, "items", false, "/items", map[string]string{"format": "keys"})
		if err != nil || string(data) != "K1\nK2\n" || requests.Load() != 1 {
			t.Fatalf("data=%q err=%v requests=%d, want the raw keys in one request", data, err, requests.Load())
		}
	})
}

func TestParseStateCommentRefusesTruncatedComment(t *testing.T) {
	body := "---\nzotero_key: ABCD1234\n---\n" + vaultNotesBegin + "\nmine\n" + vaultNotesEnd + "\n" + vaultStatePrefix + `{"schema":1,"note_key":"NOTEKEY1","note_ver`
	if _, err := parseStateComment(body); err == nil {
		t.Fatal("a state comment with no closing marker parsed as no state; push would create a duplicate child note")
	}
	if _, err := parseStateComment("no state here\n"); err != nil {
		t.Fatalf("a note with no state comment must parse as unmanaged: %v", err)
	}

	repaired := replaceOrInsertStateComment(body, pushState{Schema: 1, NoteKey: "NOTEKEY1", NoteVersion: 4})
	if strings.Count(repaired, vaultStatePrefix) != 1 {
		t.Fatalf("repaired body keeps %d state comments, want exactly one:\n%s", strings.Count(repaired, vaultStatePrefix), repaired)
	}
	st, err := parseStateComment(repaired)
	if err != nil || st.NoteKey != "NOTEKEY1" || st.NoteVersion != 4 {
		t.Fatalf("repaired state = %+v, %v", st, err)
	}
	if !strings.Contains(repaired, "mine") {
		t.Fatalf("repair lost the notes region:\n%s", repaired)
	}
}

func TestResumableTemporaryParentWaitsForUnregisteredMD5(t *testing.T) {
	req := reparentRequest(t, "TARGET01")
	young := time.Now().UTC().Format(time.RFC3339)
	old := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)

	// serve builds a plane with one marked parent whose child reports md5s[n]
	// on its n-th read (the last value repeats); "same" is the upload's hash.
	serve := func(t *testing.T, added string, md5s ...string) *rootFlags {
		var reads atomic.Int64
		mux := http.NewServeMux()
		mux.HandleFunc("/users/0/items/top", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("start") != "0" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"key": "ORPHAN01", "version": 1,
				"data": map[string]any{
					"key": "ORPHAN01", "itemType": "document", "dateAdded": added,
					"abstractNote": connectorTempParentMarker("deadbeefdeadbeef", "TARGET01"),
				},
			}})
		})
		mux.HandleFunc("/users/0/items/ORPHAN01/children", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("start") != "0" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			n := int(reads.Add(1)) - 1
			if n >= len(md5s) {
				n = len(md5s) - 1
			}
			md5 := md5s[n]
			if md5 == "same" {
				md5 = req.MD5
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"key": "CHILD001", "version": 1,
				"data": map[string]any{
					"key": "CHILD001", "itemType": "attachment", "linkMode": "imported_url",
					"md5": md5, "parentItem": "ORPHAN01",
				},
			}})
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		oldInterval, oldWait := connectorReparentPollInterval, connectorMD5RegistrationWait
		connectorReparentPollInterval = time.Millisecond
		connectorMD5RegistrationWait = 100 * time.Millisecond
		t.Cleanup(func() {
			connectorReparentPollInterval = oldInterval
			connectorMD5RegistrationWait = oldWait
		})
		return &rootFlags{configPath: testConfigFile(t, srv.URL+"/users/0"), timeout: 5 * time.Second}
	}

	t.Run("hash registers during the wait and the orphan is adopted", func(t *testing.T) {
		flags := serve(t, young, "", "", "same")
		tempKey, attachKey, err := findResumableTemporaryParent(context.Background(), flags, "TARGET01", req.MD5)
		if err != nil || tempKey != "ORPHAN01" || attachKey != "CHILD001" {
			t.Fatalf("got %q %q %v, want the orphan adopted once its hash registered", tempKey, attachKey, err)
		}
	})
	t.Run("hash never registers and no second parent is allowed", func(t *testing.T) {
		flags := serve(t, young, "")
		tempKey, _, err := findResumableTemporaryParent(context.Background(), flags, "TARGET01", req.MD5)
		if err == nil || tempKey != "" {
			t.Fatalf("got %q, %v, want a refusal while an unconfirmed marked parent may own the upload", tempKey, err)
		}
		if !strings.Contains(err.Error(), "not registered") {
			t.Fatalf("error = %v, want it to explain the unregistered hash", err)
		}
	})
	t.Run("registered hash of different content is not a match and does not block", func(t *testing.T) {
		flags := serve(t, young, "", "0123456789abcdef0123456789abcdef")
		tempKey, _, err := findResumableTemporaryParent(context.Background(), flags, "TARGET01", req.MD5)
		if err != nil || tempKey != "" {
			t.Fatalf("got %q, %v, want nothing to resume", tempKey, err)
		}
	})
	t.Run("an abandoned parent with no hash does not block", func(t *testing.T) {
		flags := serve(t, old, "")
		tempKey, _, err := findResumableTemporaryParent(context.Background(), flags, "TARGET01", req.MD5)
		if err != nil || tempKey != "" {
			t.Fatalf("got %q, %v, want an abandoned parent ignored", tempKey, err)
		}
	})
}
