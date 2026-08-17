// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/zoteroprefs"
)

// stubZoteroFileStorage makes the desktop's file-storage configuration
// deterministic for a test and clears the process-wide memo around it.
func stubZoteroFileStorage(t *testing.T, protocol string, enabled bool) {
	t.Helper()
	old := loadZoteroFileStorage
	loadZoteroFileStorage = func() (zoteroprefs.FileStorage, error) {
		return zoteroprefs.FileStorage{}, nil
	}
	if protocol != "" || !enabled {
		dir := t.TempDir()
		prefs := "user_pref(\"extensions.zotero.sync.storage.protocol\", \"" + protocol + "\");\n"
		if !enabled {
			prefs += "user_pref(\"extensions.zotero.sync.storage.enabled\", false);\n"
		}
		writeTestPrefs(t, dir, prefs)
		loadZoteroFileStorage = func() (zoteroprefs.FileStorage, error) {
			return zoteroprefs.LoadProfile(dir)
		}
	}
	resetZoteroFileStorageCache()
	t.Cleanup(func() {
		loadZoteroFileStorage = old
		resetZoteroFileStorageCache()
	})
}

func writeTestPrefs(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "prefs.js"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write prefs.js: %v", err)
	}
}

// A WebDAV-configured desktop must refuse; every other state must proceed, so
// zotio keeps working on machines with no Zotero and for group libraries,
// which always use Zotero's own storage.
func TestStoredUploadRefusalOnlyFiresOnPositiveMisrouteEvidence(t *testing.T) {
	tests := []struct {
		name       string
		protocol   string
		enabled    bool
		flags      *rootFlags
		wantRefuse bool
	}{
		{name: "webdav personal library", protocol: "webdav", enabled: true, flags: &rootFlags{}, wantRefuse: true},
		{name: "file syncing disabled", protocol: "zotero", enabled: false, flags: &rootFlags{}, wantRefuse: true},
		{name: "zotero cloud", protocol: "zotero", enabled: true, flags: &rootFlags{}, wantRefuse: false},
		{name: "no zotero desktop", protocol: "", enabled: true, flags: &rootFlags{}, wantRefuse: false},
		{name: "webdav overridden", protocol: "webdav", enabled: true, flags: &rootFlags{allowZoteroCloud: true}, wantRefuse: false},
		{name: "webdav but group library", protocol: "webdav", enabled: true, flags: &rootFlags{group: "12345"}, wantRefuse: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stubZoteroFileStorage(t, tc.protocol, tc.enabled)
			got := storedUploadRefusal(tc.flags)
			if (got != "") != tc.wantRefuse {
				t.Fatalf("storedUploadRefusal = %q, wantRefuse=%v", got, tc.wantRefuse)
			}
		})
	}
}

// The refusal must name the destination Zotero is configured for; a bare
// "not allowed" leaves the operator with nothing to act on.
func TestStoredUploadRefusalNamesTheConfiguredStore(t *testing.T) {
	dir := t.TempDir()
	writeTestPrefs(t, dir, `
user_pref("extensions.zotero.sync.storage.protocol", "webdav");
user_pref("extensions.zotero.sync.storage.url", "webdav.example.com/home/Sources");
`)
	old := loadZoteroFileStorage
	loadZoteroFileStorage = func() (zoteroprefs.FileStorage, error) { return zoteroprefs.LoadProfile(dir) }
	resetZoteroFileStorageCache()
	t.Cleanup(func() { loadZoteroFileStorage = old; resetZoteroFileStorageCache() })

	detail := storedUploadRefusal(&rootFlags{})
	if !strings.Contains(detail, "webdav.example.com") {
		t.Fatalf("detail = %q, want the configured WebDAV host named", detail)
	}
	if !strings.Contains(detail, "cloud storage") {
		t.Fatalf("detail = %q, want the wrong destination named", detail)
	}
}

// attachments add always targets an item that already exists, which the
// desktop connector cannot address, so a stored upload is refused before any
// plan is rendered.
func TestAttachmentsAddStoredRefusedUnderWebDAV(t *testing.T) {
	stubZoteroFileStorage(t, "webdav", true)
	root, _, out, _ := newPreflightTestRoot(t)

	add := mustFindPreflightCommand(t, root, "attachments", "add")
	runExecuted := false
	add.RunE = func(cmd *cobra.Command, args []string) error {
		runExecuted = true
		return nil
	}

	root.SetArgs([]string{"--json", "attachments", "add", "PARENT1", "/tmp/x.pdf", "--mode", "stored"})
	err := root.Execute()
	if err == nil {
		t.Fatal("stored attach under WebDAV succeeded, want a refusal")
	}
	if runExecuted {
		t.Fatal("attachments add RunE executed after the storage precondition failed")
	}
	assertPreconditionExitCode(t, err, 9)

	var env preconditionUnmetEnvelope
	if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
		t.Fatalf("decode precondition envelope: %v; output=%q", decodeErr, out.String())
	}
	if env.Precondition != preconditionZoteroFileStorage {
		t.Fatalf("precondition = %q, want %s", env.Precondition, preconditionZoteroFileStorage)
	}
	if env.Capability != "attachments add" {
		t.Fatalf("capability = %q, want attachments add", env.Capability)
	}
	if len(env.Remediation) == 0 {
		t.Fatal("remediation is empty; the refusal must be actionable")
	}
	joined := strings.Join(env.Remediation, " ")
	if !strings.Contains(joined, "--via connector") {
		t.Fatalf("remediation = %q, want the working local route named", joined)
	}
	if !strings.Contains(joined, "--allow-zotero-cloud") {
		t.Fatalf("remediation = %q, want the override named", joined)
	}
}

// linked-file never uploads bytes, so it must keep working unchanged on a
// WebDAV-configured desktop.
func TestAttachmentsAddLinkedFileUnaffectedByWebDAV(t *testing.T) {
	stubZoteroFileStorage(t, "webdav", true)
	root, _, _, _ := newPreflightTestRoot(t)

	add := mustFindPreflightCommand(t, root, "attachments", "add")
	runExecuted := false
	add.RunE = func(cmd *cobra.Command, args []string) error {
		runExecuted = true
		return nil
	}

	root.SetArgs([]string{"--json", "attachments", "add", "PARENT1", "/tmp/x.pdf", "--mode", "linked-file"})
	if err := root.Execute(); err != nil {
		t.Fatalf("linked-file attach under WebDAV failed: %v", err)
	}
	if !runExecuted {
		t.Fatal("linked-file attach was blocked; it never uploads bytes")
	}
}

func TestAttachmentsAddStoredAllowedWithOverride(t *testing.T) {
	stubZoteroFileStorage(t, "webdav", true)
	root, _, _, _ := newPreflightTestRoot(t)

	add := mustFindPreflightCommand(t, root, "attachments", "add")
	runExecuted := false
	add.RunE = func(cmd *cobra.Command, args []string) error {
		runExecuted = true
		return nil
	}

	root.SetArgs([]string{"--json", "attachments", "add", "PARENT1", "/tmp/x.pdf", "--mode", "stored", "--allow-zotero-cloud"})
	if err := root.Execute(); err != nil {
		t.Fatalf("stored attach with --allow-zotero-cloud failed: %v", err)
	}
	if !runExecuted {
		t.Fatal("--allow-zotero-cloud did not restore the upload path")
	}
}

// applyStoredUpload is the single funnel for every Web API stored upload, so
// the guard there covers routes preflight cannot decide in advance (import
// apply's per-entry Web fallback, import pdf's retro-attach onto a duplicate).
func TestApplyStoredUploadBackstopRefusesWithoutTouchingTheNetwork(t *testing.T) {
	stubZoteroFileStorage(t, "webdav", true)

	// A nil client would itself fail; the guard must fire first and name the
	// storage mismatch rather than reporting a missing write client.
	status, _, err := applyStoredUpload(t.Context(), nil, storedUploadRequest{ParentKey: "PARENT1"}, &rootFlags{})
	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}
	if err == nil || !strings.Contains(err.Error(), "refusing stored upload") {
		t.Fatalf("err = %v, want the stored-upload refusal", err)
	}
	if !strings.Contains(err.Error(), "WebDAV") {
		t.Fatalf("err = %v, want the configured store named", err)
	}
}

// The counterpart to the refusal: with no evidence of a misroute the guard
// must stay out of the way and let a real upload complete. Using a live fake
// Zotero is what makes this test able to fail — a nil client would error for
// unrelated reasons and pass even if the guard were deleted.
func TestApplyStoredUploadProceedsWithoutZoteroDesktop(t *testing.T) {
	stubZoteroFileStorage(t, "", true)

	fake := newFakeZoteroUpload(t, "PARENT1")
	setUploadTestEnv(t, fake)
	flags := &rootFlags{}
	c, err := flags.newClient()
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	path := writeUploadFixture(t, "paper.pdf", []byte(uploadFixturePDF))
	req, err := newStoredUploadRequest("PARENT1", path, "")
	if err != nil {
		t.Fatalf("newStoredUploadRequest: %v", err)
	}

	status, _, err := applyStoredUpload(t.Context(), c, req, flags)
	if err != nil {
		t.Fatalf("applyStoredUpload: %v", err)
	}
	if status == "failed" {
		t.Fatalf("status = %q, want the upload to proceed", status)
	}
	if _, uploads, _ := fake.snapshot(); uploads == 0 {
		t.Fatal("no bytes uploaded; the guard blocked an upload with no evidence of a misroute")
	}
}

// The override has to work at APPLY time, not just in preview: a regression
// where guardStoredUpload ignores allowZoteroCloud would still let the preview
// render and only refuse once bytes were about to move.
func TestApplyStoredUploadHonoursOverrideUnderWebDAV(t *testing.T) {
	stubZoteroFileStorage(t, "webdav", true)

	fake := newFakeZoteroUpload(t, "PARENT1")
	setUploadTestEnv(t, fake)
	flags := &rootFlags{allowZoteroCloud: true}
	c, err := flags.newClient()
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	path := writeUploadFixture(t, "paper.pdf", []byte(uploadFixturePDF))
	req, err := newStoredUploadRequest("PARENT1", path, "")
	if err != nil {
		t.Fatalf("newStoredUploadRequest: %v", err)
	}

	if _, _, err := applyStoredUpload(t.Context(), c, req, flags); err != nil {
		t.Fatalf("applyStoredUpload with --allow-zotero-cloud: %v", err)
	}
	if _, uploads, _ := fake.snapshot(); uploads == 0 {
		t.Fatal("--allow-zotero-cloud did not reach the upload at apply time")
	}
}

// A profile that cannot be read is not evidence of a misroute, so the guard
// must allow rather than invent a refusal from a read failure.
func TestStoredUploadAllowedWhenProfileReadFails(t *testing.T) {
	old := loadZoteroFileStorage
	loadZoteroFileStorage = func() (zoteroprefs.FileStorage, error) {
		return zoteroprefs.FileStorage{}, errors.New("prefs.js exceeds 8388608 bytes")
	}
	resetZoteroFileStorageCache()
	t.Cleanup(func() { loadZoteroFileStorage = old; resetZoteroFileStorageCache() })

	if got := storedUploadRefusal(&rootFlags{}); got != "" {
		t.Fatalf("storedUploadRefusal = %q, want an unreadable profile to allow the upload", got)
	}
}

// doctor must show where Zotero keeps files, so the operator can see the
// mismatch without first tripping over a refusal.
func TestDoctorReportsWebDAVFileStorageAndTheRefusal(t *testing.T) {
	stubZoteroFileStorage(t, "webdav", true)

	report := map[string]any{}
	addDoctorFileStorageReport(report, &rootFlags{})

	got, _ := report["file_storage"].(string)
	if !strings.Contains(got, "WebDAV") {
		t.Fatalf("file_storage = %q, want WebDAV named", got)
	}
	if !strings.Contains(got, "refused") {
		t.Fatalf("file_storage = %q, want the refusal surfaced", got)
	}
	if _, ok := report["file_storage_hint"]; !ok {
		t.Fatal("file_storage_hint missing; the report must be actionable")
	}
}

func TestDoctorReportsUnknownFileStorageWithoutZoteroDesktop(t *testing.T) {
	stubZoteroFileStorage(t, "", true)

	report := map[string]any{}
	addDoctorFileStorageReport(report, &rootFlags{})

	got, _ := report["file_storage"].(string)
	if !strings.HasPrefix(got, "unknown") {
		t.Fatalf("file_storage = %q, want unknown", got)
	}
	if strings.Contains(got, "refused") {
		t.Fatalf("file_storage = %q, want no refusal without evidence", got)
	}
}

// import apply's stored route reaches an existing item through the Web API, so
// it is refused for the same reason and with the same machine-readable
// envelope.
func TestImportApplyStoredAttachRefusedUnderWebDAV(t *testing.T) {
	stubZoteroFileStorage(t, "webdav", true)
	pdfPath := writeUploadFixture(t, "attach.pdf", []byte(uploadFixturePDF))

	m := importApplyTestManifest()
	m.Entries = []importManifestEntry{{
		Path:       pdfPath,
		Action:     "attach",
		Status:     "resolved",
		MatchedKey: "MATCH1",
		Title:      "Attach Paper",
	}}
	manifestPath := writeImportApplyTestManifest(t, m)

	_, _, err := runImportApplyTestCmdWithFlags(t, applyFlags(), []string{"--attach-mode", "stored", manifestPath})
	if err == nil {
		t.Fatal("import apply stored attach under WebDAV succeeded, want a refusal")
	}
	assertPreconditionExitCode(t, err, 9)
}

// The connector route creates the parent and its file in one desktop session,
// which is the one path that reaches a WebDAV store, so it must NOT be refused.
func TestImportApplyStoredCreateViaConnectorNotRefusedUnderWebDAV(t *testing.T) {
	stubZoteroFileStorage(t, "webdav", true)
	pdfPath := writeUploadFixture(t, "create.pdf", []byte(uploadFixturePDF))

	m := importApplyTestManifest()
	m.Entries = []importManifestEntry{{
		Path:   pdfPath,
		Action: "create",
		Status: "resolved",
		Title:  "Created Paper",
		Item:   map[string]any{"itemType": "journalArticle", "title": "Created Paper"},
	}}
	manifestPath := writeImportApplyTestManifest(t, m)

	flags := &rootFlags{asJSON: true, via: "connector", configPath: testConfigFile(t, "http://localhost:23119/api/users/0")}
	_, stderr, err := runImportApplyTestCmdWithFlags(t, flags, []string{"--attach-mode", "stored", manifestPath})
	if err != nil {
		t.Fatalf("connector-route stored preview refused: %v; stderr=%s", err, stderr)
	}
}

// import apply with --via web resolves to the Web uploader without any route
// probe, so the mismatch is knowable at preview time.
func TestImportApplyStoredCreateViaWebRefusedAtPreview(t *testing.T) {
	stubZoteroFileStorage(t, "webdav", true)
	pdfPath := writeUploadFixture(t, "create.pdf", []byte(uploadFixturePDF))

	m := importApplyTestManifest()
	m.Entries = []importManifestEntry{{
		Path:   pdfPath,
		Action: "create",
		Status: "resolved",
		Title:  "Created Paper",
		Item:   map[string]any{"itemType": "journalArticle", "title": "Created Paper"},
	}}
	manifestPath := writeImportApplyTestManifest(t, m)

	_, _, err := runImportApplyTestCmdWithFlags(t, &rootFlags{asJSON: true, via: "web"}, []string{"--attach-mode", "stored", manifestPath})
	if err == nil {
		t.Fatal("import apply --via web stored create under WebDAV succeeded, want a refusal")
	}
	assertPreconditionExitCode(t, err, 9)
}

// newClient only installs hybrid write routing for a LOCAL base URL, so a
// configured base_url that already points at /groups/<id> writes to that group
// with --group unset. Reading the flag alone would call that the personal
// library and refuse a correct group upload.
func TestGroupScopeComesFromTheResolvedWriteTargetNotJustTheFlag(t *testing.T) {
	tests := []struct {
		name      string
		baseURL   string
		group     string
		wantGroup bool
	}{
		{name: "explicit flag", baseURL: "http://localhost:23119/api/users/0", group: "12345", wantGroup: true},
		{name: "configured group base", baseURL: "https://api.zotero.org/groups/12345", wantGroup: true},
		{name: "configured user base", baseURL: "https://api.zotero.org/users/999", wantGroup: false},
		{name: "local user base", baseURL: "http://localhost:23119/api/users/0", wantGroup: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			flags := &rootFlags{group: tc.group, configPath: testConfigFile(t, tc.baseURL)}
			if got := storedUploadTargetsGroup(flags); got != tc.wantGroup {
				t.Fatalf("storedUploadTargetsGroup = %v, want %v", got, tc.wantGroup)
			}
		})
	}
}

// A WebDAV personal library must not block an upload that genuinely targets a
// group, since Zotero always uses its own storage for groups.
func TestWebDAVPersonalLibraryDoesNotRefuseConfiguredGroupUploads(t *testing.T) {
	stubZoteroFileStorage(t, "webdav", true)

	flags := &rootFlags{configPath: testConfigFile(t, "https://api.zotero.org/groups/12345")}
	if got := storedUploadRefusal(flags); got != "" {
		t.Fatalf("storedUploadRefusal = %q, want a group upload to proceed", got)
	}
}

// The MCP server runs this Cobra tree in-process against one long-lived
// RootCmd, so a process-lifetime memo would keep judging a reconfigured
// desktop by a stale reading.
func TestStorageReadingIsMemoizedPerInvocationNotPerProcess(t *testing.T) {
	protocol := "zotero"
	calls := 0
	old := loadZoteroFileStorage
	loadZoteroFileStorage = func() (zoteroprefs.FileStorage, error) {
		calls++
		dir := t.TempDir()
		writeTestPrefs(t, dir, `user_pref("extensions.zotero.sync.storage.protocol", "`+protocol+`");`)
		return zoteroprefs.LoadProfile(dir)
	}
	resetZoteroFileStorageCache()
	t.Cleanup(func() { loadZoteroFileStorage = old; resetZoteroFileStorageCache() })

	flags := &rootFlags{}
	if got := storedUploadRefusal(flags); got != "" {
		t.Fatalf("cloud-configured desktop refused: %q", got)
	}
	// Repeat reads within one invocation must not re-parse prefs.js.
	_ = storedUploadRefusal(flags)
	if calls != 1 {
		t.Fatalf("prefs read %d times in one invocation, want 1", calls)
	}

	// The operator switches Zotero to WebDAV; the next invocation must see it.
	protocol = "webdav"
	resetZoteroFileStorageCache()
	if got := storedUploadRefusal(flags); got == "" {
		t.Fatal("reconfigured desktop still permitted the upload; the memo outlived the invocation")
	}
}

// Every command passes through the root PersistentPreRunE, which is where the
// per-invocation reset has to happen for the MCP case to be covered.
func TestRootPersistentPreRunResetsTheStorageMemo(t *testing.T) {
	stubZoteroFileStorage(t, "zotero", true)
	if _, err := zoteroFileStorage(); err != nil {
		t.Fatalf("seed read: %v", err)
	}
	if !zoteroFileStorageLoaded {
		t.Fatal("memo not populated by the seed read")
	}

	root, _, _, _ := newPreflightTestRoot(t)
	status := mustFindPreflightCommand(t, root, "auth", "status")
	status.RunE = func(cmd *cobra.Command, args []string) error {
		if zoteroFileStorageLoaded {
			t.Error("storage memo survived into the next command invocation")
		}
		return nil
	}
	root.SetArgs([]string{"auth", "status"})
	if err := root.Execute(); err != nil {
		t.Fatalf("auth status: %v", err)
	}
}

// Zotero's -P switch means Default=1 does not prove which profile is running.
// zoteroprefs folds a WebDAV sibling in and marks the result ambiguous (proven
// in that package against loadAcross); what matters here is that the refusal
// still fires AND tells the operator how to resolve the ambiguity. Built from
// a real LoadProfile result so no platform-specific discovery layout is
// needed — the earlier version of this test skipped on every OS and asserted
// nothing.
func TestAmbiguousProfilesStillRefuseAndNameTheFix(t *testing.T) {
	dir := t.TempDir()
	writeTestPrefs(t, dir, `
user_pref("extensions.zotero.sync.storage.protocol", "webdav");
user_pref("extensions.zotero.sync.storage.url", "nas.example.com/zotero");
`)
	ambiguous, err := zoteroprefs.LoadProfile(dir)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	ambiguous.Ambiguous = true
	ambiguous.ProfileCount = 2

	old := loadZoteroFileStorage
	loadZoteroFileStorage = func() (zoteroprefs.FileStorage, error) { return ambiguous, nil }
	resetZoteroFileStorageCache()
	t.Cleanup(func() { loadZoteroFileStorage = old; resetZoteroFileStorageCache() })

	detail := storedUploadRefusal(&rootFlags{})
	if detail == "" {
		t.Fatal("a WebDAV profile did not refuse; the risky direction was masked")
	}
	if !strings.Contains(detail, zoteroprefs.ProfileDirEnv) {
		t.Fatalf("detail = %q, want the pinning override named", detail)
	}
	if !strings.Contains(detail, "nas.example.com") {
		t.Fatalf("detail = %q, want the folded-in WebDAV host named", detail)
	}

	// doctor must keep BOTH the refusal workaround and the pinning instruction.
	report := map[string]any{}
	addDoctorFileStorageReport(report, &rootFlags{})
	hint, _ := report["file_storage_hint"].(string)
	if !strings.Contains(hint, zoteroprefs.ProfileDirEnv) {
		t.Fatalf("hint = %q, want the profile-pinning instruction retained", hint)
	}
	if !strings.Contains(hint, "--allow-zotero-cloud") {
		t.Fatalf("hint = %q, want the refusal workaround retained", hint)
	}
}

// The one route that reaches a WebDAV store must survive a source-less local
// PDF. Zotero rejects an empty url with an opaque 500 AFTER creating the
// parent, so this drives the real apply path and asserts the file:// fallback
// is what lands in the connector's X-Metadata. Testing localFileURL alone
// would not catch the fallback being dropped at the call site.
func TestImportApplyConnectorSendsFileURLForSourcelessPDF(t *testing.T) {
	stubZoteroFileStorage(t, "webdav", true)

	listener, err := net.Listen("tcp", "127.0.0.1:23119")
	if err != nil {
		t.Skipf("port 23119 is unavailable: %v", err)
	}
	var gotMetadata string
	var saveItems, saveAttachments int
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connector/ping":
			w.WriteHeader(http.StatusOK)
		case "/connector/saveItems":
			saveItems++
			w.WriteHeader(http.StatusCreated)
		case "/connector/saveAttachment":
			saveAttachments++
			gotMetadata = r.Header.Get("X-Metadata")
			w.WriteHeader(http.StatusCreated)
		default:
			w.Header().Set("Last-Modified-Version", "0")
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)

	// A path with a space proves the value is a properly escaped URI rather
	// than a concatenation.
	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "scanned paper.pdf")
	if err := os.WriteFile(pdfPath, []byte(uploadFixturePDF), 0o600); err != nil {
		t.Fatal(err)
	}

	m := importApplyTestManifest()
	m.Entries = []importManifestEntry{{
		Path:   pdfPath,
		Action: "create",
		Status: "resolved",
		Title:  "Sourceless Scan",
		Item:   map[string]any{"itemType": "journalArticle", "title": "Sourceless Scan"},
	}}
	manifestPath := writeImportApplyTestManifest(t, m)

	flags := &rootFlags{
		asJSON:     true,
		yes:        true,
		via:        "connector",
		timeout:    5 * time.Second,
		maxChanges: -1,
		configPath: testConfigFile(t, "http://127.0.0.1:23119/api/users/0"),
	}
	if _, stderr, err := runImportApplyTestCmdWithFlags(t, flags, []string{"--attach-mode", "stored", manifestPath}); err != nil {
		t.Fatalf("connector apply: %v; stderr=%s", err, stderr)
	}

	if saveItems != 1 || saveAttachments != 1 {
		t.Fatalf("connector calls = saveItems:%d saveAttachment:%d, want one each", saveItems, saveAttachments)
	}
	var meta struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(gotMetadata), &meta); err != nil {
		t.Fatalf("decode X-Metadata %q: %v", gotMetadata, err)
	}
	if meta.URL == "" {
		t.Fatal("connector received an empty url; Zotero answers 500 after creating the parent")
	}
	if want := localFileURL(pdfPath); meta.URL != want {
		t.Fatalf("connector url = %q, want the escaped file URI %q", meta.URL, want)
	}
	if !strings.Contains(meta.URL, "%20") {
		t.Fatalf("connector url = %q, want the space percent-escaped", meta.URL)
	}
}

// Zotero writes this value into the attachment item's url field, so it has to
// be a real URI. String-joining "file://" with a path leaves separators and
// reserved characters unescaped and mis-parses a Windows drive letter as an
// authority.
func TestLocalFileURLProducesAValidFileURI(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "plain unix path", path: "/tmp/paper.pdf", want: "file:///tmp/paper.pdf"},
		{name: "spaces escaped", path: "/tmp/my papers/a b.pdf", want: "file:///tmp/my%20papers/a%20b.pdf"},
		{name: "hash escaped", path: "/tmp/draft#2.pdf", want: "file:///tmp/draft%232.pdf"},
		{name: "question mark escaped", path: "/tmp/what?.pdf", want: "file:///tmp/what%3F.pdf"},
		{name: "percent escaped", path: "/tmp/100%.pdf", want: "file:///tmp/100%25.pdf"},
		{name: "relative path gets empty authority", path: "rel/a.pdf", want: "file:///rel/a.pdf"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := localFileURL(tc.path)
			if got != tc.want {
				t.Fatalf("localFileURL(%q) = %q, want %q", tc.path, got, tc.want)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("result does not parse as a URL: %v", err)
			}
			if u.Scheme != "file" {
				t.Fatalf("scheme = %q, want file", u.Scheme)
			}
			if u.Host != "" {
				t.Fatalf("host = %q, want an empty authority", u.Host)
			}
			// The decoded path must round-trip back to the original.
			want := tc.path
			if !strings.HasPrefix(want, "/") {
				want = "/" + want
			}
			if u.Path != want {
				t.Fatalf("decoded path = %q, want %q", u.Path, want)
			}
		})
	}
}
