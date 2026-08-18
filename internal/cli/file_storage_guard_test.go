// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/connector"
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

// stubLoadedZoteroFileStorage installs an already-loaded reading, for tests
// that need a multi-profile fold the single-profile stub cannot express.
func stubLoadedZoteroFileStorage(t *testing.T, fs zoteroprefs.FileStorage) {
	t.Helper()
	old := loadZoteroFileStorage
	loadZoteroFileStorage = func() (zoteroprefs.FileStorage, error) { return fs, nil }
	resetZoteroFileStorageCache()
	t.Cleanup(func() {
		loadZoteroFileStorage = old
		resetZoteroFileStorageCache()
	})
}

func writeTestPrefs(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("create profile dir: %v", err)
	}
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
			got := storedUploadRefusal(t.Context(), tc.flags, storedUploadToExistingItem)
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

	detail := storedUploadRefusal(t.Context(), &rootFlags{}, storedUploadToExistingItem)
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

// Every other command-level guard test stubs the prefs loader, so none of them
// would notice if loadAcross stopped unioning hazards or the parser stopped
// decoding the WebDAV protocol. This one drives the real reader over a real
// two-profile fixture on disk, through the real Cobra preflight, and asserts
// the refusal both fires and attributes correctly.
//
// This drives the real prefs reader through the real Cobra preflight. Note it
// does NOT discriminate attribution: the WebDAV sibling outranks the cloud
// profile, so it becomes the representative and the pre-fix code named it
// correctly by accident. TestRefusalNamesTheNonRepresentativeContributor
// covers that axis; this one covers parsing and wiring.
func TestAttachmentsAddRefusalReadsRealProfilesEndToEnd(t *testing.T) {
	root := t.TempDir()
	cloud := filepath.Join(root, "cloud.default")
	writeTestPrefs(t, cloud, `
user_pref("extensions.zotero.sync.storage.protocol", "zotero");
user_pref("extensions.zotero.sync.storage.enabled", true);
`)
	// The sibling that carries the hazard. It is not the preferred profile,
	// which is the case a single-profile fixture cannot produce.
	nas := filepath.Join(root, "nas.other")
	writeTestPrefs(t, nas, `
user_pref("extensions.zotero.sync.storage.protocol", "webdav");
user_pref("extensions.zotero.sync.storage.url", "nas.example.com/zotero");
`)

	old := loadZoteroFileStorage
	loadZoteroFileStorage = func() (zoteroprefs.FileStorage, error) {
		return zoteroprefs.LoadAcrossForTest([]string{cloud, nas}, cloud)
	}
	resetZoteroFileStorageCache()
	t.Cleanup(func() { loadZoteroFileStorage = old; resetZoteroFileStorageCache() })

	cmdRoot, _, out, _ := newPreflightTestRoot(t)
	add := mustFindPreflightCommand(t, cmdRoot, "attachments", "add")
	runExecuted := false
	add.RunE = func(cmd *cobra.Command, args []string) error {
		runExecuted = true
		return nil
	}

	cmdRoot.SetArgs([]string{"--json", "attachments", "add", "PARENT1", "/tmp/x.pdf", "--mode", "stored"})
	err := cmdRoot.Execute()
	if err == nil {
		t.Fatal("a WebDAV sibling profile did not refuse the stored upload")
	}
	if runExecuted {
		t.Fatal("attachments add RunE executed after the storage precondition failed")
	}
	assertPreconditionExitCode(t, err, 9)

	var env preconditionUnmetEnvelope
	if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
		t.Fatalf("decode precondition envelope: %v; output=%q", decodeErr, out.String())
	}
	// The host is only reachable by actually parsing the fixture.
	if !strings.Contains(env.Detail, "nas.example.com") {
		t.Fatalf("detail = %q, want the WebDAV host read from the fixture", env.Detail)
	}
	joined := strings.Join(env.Remediation, " ")
	if !strings.Contains(joined, nas) {
		t.Fatalf("remediation = %q, want the WebDAV profile named", joined)
	}
	if strings.Contains(joined, cloud) {
		t.Fatalf("remediation = %q, must not name the cloud profile that evidenced nothing", joined)
	}
}

// The attribution regression is only reachable when the profile that evidenced
// the hazard LOSES the risk ranking, because the old code reported the
// risk-ranked representative. Two profiles in the same storage mode tie, so
// the preferred one stays representative while the sibling carries the
// hazard - the exact shape that made a refusal name a profile whose settings
// contradicted it. Runs the real reader through the real preflight.
func TestRefusalNamesTheNonRepresentativeContributor(t *testing.T) {
	root := t.TempDir()
	// Preferred, innocent: same mode as the sibling, file syncing ON.
	innocent := filepath.Join(root, "innocent.default")
	writeTestPrefs(t, innocent, `
user_pref("extensions.zotero.sync.storage.protocol", "zotero");
user_pref("extensions.zotero.sync.storage.enabled", true);
`)
	// Guilty sibling: identical mode, so it can never win the ranking.
	guilty := filepath.Join(root, "guilty.other")
	writeTestPrefs(t, guilty, `
user_pref("extensions.zotero.sync.storage.protocol", "zotero");
user_pref("extensions.zotero.sync.storage.enabled", false);
`)

	old := loadZoteroFileStorage
	loadZoteroFileStorage = func() (zoteroprefs.FileStorage, error) {
		return zoteroprefs.LoadAcrossForTest([]string{innocent, guilty}, innocent)
	}
	// The premise that makes this test discriminating: the representative is
	// the INNOCENT profile. If a future ranking change made the guilty one
	// representative, this test would silently degrade into the vacuous shape
	// it was written to replace, so assert it rather than assume it.
	if fs, loadErr := zoteroprefs.LoadAcrossForTest([]string{innocent, guilty}, innocent); loadErr != nil {
		t.Fatalf("LoadAcrossForTest: %v", loadErr)
	} else if fs.ProfilePath != innocent {
		t.Fatalf("representative = %q, want the innocent profile %q; the fixture no longer reproduces the regression", fs.ProfilePath, innocent)
	}
	resetZoteroFileStorageCache()
	t.Cleanup(func() { loadZoteroFileStorage = old; resetZoteroFileStorageCache() })

	cmdRoot, _, out, _ := newPreflightTestRoot(t)
	add := mustFindPreflightCommand(t, cmdRoot, "attachments", "add")
	runExecuted := false
	add.RunE = func(cmd *cobra.Command, args []string) error {
		runExecuted = true
		return nil
	}

	cmdRoot.SetArgs([]string{"--json", "attachments", "add", "PARENT1", "/tmp/x.pdf", "--mode", "stored"})
	err := cmdRoot.Execute()
	if err == nil {
		t.Fatal("a sync-disabled sibling profile did not refuse the stored upload")
	}
	if runExecuted {
		t.Fatal("attachments add RunE executed after the storage precondition failed")
	}
	assertPreconditionExitCode(t, err, 9)

	var env preconditionUnmetEnvelope
	if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
		t.Fatalf("decode precondition envelope: %v; output=%q", decodeErr, out.String())
	}
	joined := strings.Join(env.Remediation, " ")
	if !strings.Contains(joined, guilty) {
		t.Fatalf("remediation = %q, want the profile whose setting caused the refusal", joined)
	}
	// The regression itself: the representative evidenced nothing, and naming
	// it sends the operator to verify the one file that contradicts the message.
	if strings.Contains(joined, innocent) {
		t.Fatalf("remediation = %q, must not name the representative profile that has syncing ON", joined)
	}
}

// An unreadable profile evidenced only the INABILITY to evaluate it. Saying a
// reading "came from" it is false, and sends the operator looking for a
// setting in a file zotio never parsed.
func TestUnreadableProfileRefusalDoesNotClaimAReading(t *testing.T) {
	old := loadZoteroFileStorage
	loadZoteroFileStorage = func() (zoteroprefs.FileStorage, error) {
		return zoteroprefs.FileStorage{}, errors.New("/tmp/broken.default/prefs.js: permission denied")
	}
	resetZoteroFileStorageCache()
	t.Cleanup(func() { loadZoteroFileStorage = old; resetZoteroFileStorageCache() })

	verdict := storedUploadDecision(t.Context(), &rootFlags{}, storedUploadToExistingItem)
	if verdict.reason != storedUploadReasonUnreadable {
		t.Fatalf("reason = %v, want the unreadable classification", verdict.reason)
	}
	steps := strings.Join(storedUploadRefusalSteps(verdict), " ")
	if strings.Contains(steps, "This reading came from") {
		t.Fatalf("remediation = %q, must not claim a reading came from a file it could not read", steps)
	}
}

// When EVERY discovered profile fails to read, naming the paths matters most:
// the operator has no other way to learn which file to fix. The loader used to
// drop them on this branch, so the error travelled without its evidence.
func TestAllProfilesUnreadableStillNamesThePaths(t *testing.T) {
	root := t.TempDir()
	var dirs []string
	for _, name := range []string{"broken-a.default", "broken-b.other"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(dir, "prefs.js"), 0o750); err != nil {
			t.Fatalf("create unreadable prefs: %v", err)
		}
		dirs = append(dirs, dir)
	}

	fs, err := zoteroprefs.LoadAcrossForTest(dirs, dirs[0])
	if err == nil {
		t.Fatal("two unreadable profiles produced no error")
	}
	if fs.Found() {
		t.Fatal("a total read failure reported itself as a successful reading")
	}
	if got := fs.UnreadableProfiles(); len(got) != 2 {
		t.Fatalf("unreadable profiles = %v, want both paths carried alongside the error", got)
	}

	old := loadZoteroFileStorage
	loadZoteroFileStorage = func() (zoteroprefs.FileStorage, error) { return fs, err }
	resetZoteroFileStorageCache()
	t.Cleanup(func() { loadZoteroFileStorage = old; resetZoteroFileStorageCache() })

	steps := strings.Join(storedUploadRefusalSteps(storedUploadDecision(t.Context(), &rootFlags{}, storedUploadToExistingItem)), " ")
	for _, dir := range dirs {
		if !strings.Contains(steps, dir) {
			t.Fatalf("remediation = %q, want the unreadable path %q named", steps, dir)
		}
	}
}

// Group hazards are attributed like every other one, so a group refusal must
// name its contributors too. Omitting them left doctor showing the
// risk-ranked representative, which for a group refusal routinely evidenced
// nothing.
func TestGroupSyncRefusalNamesItsContributingProfiles(t *testing.T) {
	verdict := storedUploadVerdict{
		reason:   storedUploadReasonGroupSyncOff,
		detail:   "group syncing off",
		profiles: []string{"/tmp/group-guilty.default"},
	}
	steps := strings.Join(storedUploadRefusalSteps(verdict), " ")
	if !strings.Contains(steps, "/tmp/group-guilty.default") {
		t.Fatalf("remediation = %q, want the contributing profile named", steps)
	}
}

// Plural agreement runs through the predicate noun and the trailing pronoun,
// not just the verb; a refusal is the wrong place to be visibly sloppy.
func TestRefusalPinAgreesInNumber(t *testing.T) {
	one := strings.Join(storedUploadRefusalSteps(storedUploadVerdict{
		reason: storedUploadReasonWebDAV, detail: "d", profiles: []string{"/a"},
	}), " ")
	if !strings.Contains(one, "came from profile /a, which is not necessarily the profile") {
		t.Fatalf("singular pin = %q", one)
	}
	many := strings.Join(storedUploadRefusalSteps(storedUploadVerdict{
		reason: storedUploadReasonWebDAV, detail: "d", profiles: []string{"/a", "/b"},
	}), " ")
	if !strings.Contains(many, "came from profiles /a, /b, which are not necessarily profiles") ||
		!strings.Contains(many, "if they are not") {
		t.Fatalf("plural pin = %q", many)
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

// A profile that CANNOT BE READ is not the same as no profile at all. Zotero
// is installed here and its configuration exists; the reader simply could not
// evaluate it, so a WebDAV setting cannot be ruled out. Treating that as
// consent re-opened the silent misroute the guard exists to prevent, which is
// what the earlier version of this test asserted.
func TestStoredUploadRefusedWhenProfileCannotBeEvaluated(t *testing.T) {
	old := loadZoteroFileStorage
	loadZoteroFileStorage = func() (zoteroprefs.FileStorage, error) {
		return zoteroprefs.FileStorage{}, errors.New("prefs.js exceeds 8388608 bytes")
	}
	resetZoteroFileStorageCache()
	t.Cleanup(func() { loadZoteroFileStorage = old; resetZoteroFileStorageCache() })

	verdict := storedUploadDecision(t.Context(), &rootFlags{}, storedUploadToExistingItem)
	if !verdict.refused() {
		t.Fatal("an unevaluable profile allowed the upload; the misroute is reachable again")
	}
	if verdict.reason != storedUploadReasonUnreadable {
		t.Fatalf("reason = %v, want the unreadable classification, which is distinct from a decoded-but-unrecognised setting", verdict.reason)
	}
	if !strings.Contains(verdict.detail, "could not be read") {
		t.Fatalf("detail = %q, want the read failure named", verdict.detail)
	}

	// The override still works, so a cloud user is never stuck.
	if got := storedUploadRefusal(t.Context(), &rootFlags{allowZoteroCloud: true}, storedUploadToExistingItem); got != "" {
		t.Fatalf("override refused anyway: %q", got)
	}

	// And the remediation names the pin, not a WebDAV workaround that has
	// nothing to do with an unreadable file.
	steps := storedUploadRefusalRemediation(storedUploadReasonUnreadable)
	if !strings.Contains(strings.Join(steps, " "), zoteroprefs.ProfileDirEnv) {
		t.Fatalf("remediation = %v, want the profile pin named", steps)
	}
}

// Absence of Zotero altogether stays permissive: zotio runs against the Web
// API on machines with no desktop, and refusing there would block correct work
// on no evidence.
func TestStoredUploadAllowedWhenNoZoteroDesktopExists(t *testing.T) {
	old := loadZoteroFileStorage
	loadZoteroFileStorage = func() (zoteroprefs.FileStorage, error) {
		return zoteroprefs.FileStorage{}, nil
	}
	resetZoteroFileStorageCache()
	t.Cleanup(func() { loadZoteroFileStorage = old; resetZoteroFileStorageCache() })

	if got := storedUploadRefusal(t.Context(), &rootFlags{}, storedUploadToExistingItem); got != "" {
		t.Fatalf("storedUploadRefusal = %q, want no desktop to allow the upload", got)
	}
}

// doctor must show where Zotero keeps files, so the operator can see the
// mismatch without first tripping over a refusal.
func TestDoctorReportsWebDAVFileStorageAndTheRefusal(t *testing.T) {
	stubZoteroFileStorage(t, "webdav", true)

	report := map[string]any{}
	addDoctorFileStorageReport(t.Context(), report, &rootFlags{})

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
	addDoctorFileStorageReport(t.Context(), report, &rootFlags{})

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
		// The exact silent-misroute trigger: newClient installs hybrid routing
		// for any local base capturing only flags.group, so writes go to
		// /users/<id> even when the local base names a group.
		{name: "local group base writes personal", baseURL: "http://localhost:23119/api/groups/12345", wantGroup: false},
		// The library prefix is the TAIL of the URL; a query string, a host,
		// or a deployment prefix must not be mistaken for it. The non-tail
		// cases are the ones a scan-for-the-last-match implementation got
		// wrong, and a false group verdict skips the WebDAV refusal entirely.
		{name: "groups only in query string", baseURL: "https://api.zotero.org/users/1?next=/groups/123", wantGroup: false},
		{name: "groups only in host", baseURL: "https://groups.example/api/users/1", wantGroup: false},
		{name: "groups then users in path", baseURL: "https://proxy.example/groups/tenant/api/users/1", wantGroup: false},
		{name: "group prefix with deployment suffix", baseURL: "https://proxy.example/proxy/groups/tenant/v1", wantGroup: false},
		{name: "group prefix with resource suffix", baseURL: "https://api.zotero.org/groups/123/items", wantGroup: false},
		{name: "group base with trailing slash", baseURL: "https://api.zotero.org/groups/12345/", wantGroup: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			flags := &rootFlags{group: tc.group, configPath: testConfigFile(t, tc.baseURL)}
			if got := storedUploadTargetsGroup(flags); got != tc.wantGroup {
				t.Fatalf("storedUploadTargetsGroup = %v, want %v", got, tc.wantGroup)
			}
			// The classifier is only useful if it agrees with where writes
			// actually go. When --group is set, production routing rewrites
			// the base through rewriteLibraryPrefix, so check the classifier
			// against that rather than trusting the two to stay in step.
			if tc.group != "" {
				routed := rewriteLibraryPrefix(tc.baseURL, tc.group)
				if !strings.Contains(routed, "/groups/"+tc.group) {
					t.Fatalf("write base %q does not target the group the classifier claimed", routed)
				}
			}
		})
	}
}

// A WebDAV personal library must not block an upload that genuinely targets a
// group, since Zotero always uses its own storage for groups.
func TestWebDAVPersonalLibraryDoesNotRefuseConfiguredGroupUploads(t *testing.T) {
	stubZoteroFileStorage(t, "webdav", true)

	flags := &rootFlags{configPath: testConfigFile(t, "https://api.zotero.org/groups/12345")}
	if got := storedUploadRefusal(t.Context(), flags, storedUploadToExistingItem); got != "" {
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
	if got := storedUploadRefusal(t.Context(), flags, storedUploadToExistingItem); got != "" {
		t.Fatalf("cloud-configured desktop refused: %q", got)
	}
	// Repeat reads within one invocation must not re-parse prefs.js.
	_ = storedUploadRefusal(t.Context(), flags, storedUploadToExistingItem)
	if calls != 1 {
		t.Fatalf("prefs read %d times in one invocation, want 1", calls)
	}

	// The operator switches Zotero to WebDAV; the next invocation must see it.
	protocol = "webdav"
	resetZoteroFileStorageCache()
	if got := storedUploadRefusal(t.Context(), flags, storedUploadToExistingItem); got == "" {
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

// Zotero's -P switch means Default=1 does not prove which profile is running,
// so zoteroprefs folds a WebDAV sibling in and marks the result ambiguous.
//
// The ambiguity is produced by loading TWO REAL profiles, not by setting the
// Ambiguous and ProfileCount fields by hand: the hand-built version pinned only
// the message format, and would have stayed green with discovery, risk ranking,
// and the cross-profile hazard union all deleted.
func TestAmbiguousProfilesStillRefuseAndNameTheFix(t *testing.T) {
	root := t.TempDir()
	cloud := filepath.Join(root, "aaaa.default")
	webdav := filepath.Join(root, "bbbb.other")
	for _, dir := range []string{cloud, webdav} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	writeTestPrefs(t, cloud, `
user_pref("extensions.zotero.sync.storage.protocol", "zotero");
`)
	writeTestPrefs(t, webdav, `
user_pref("extensions.zotero.sync.storage.protocol", "webdav");
user_pref("extensions.zotero.sync.storage.url", "nas.example.com/zotero");
`)

	// The cloud profile is the preferred one, so a reading that trusted the
	// default would report cloud and never refuse.
	ambiguous, err := zoteroprefs.LoadAcrossForTest([]string{cloud, webdav}, cloud)
	if err != nil {
		t.Fatalf("loadAcross: %v", err)
	}
	if !ambiguous.Ambiguous {
		t.Fatal("two profiles did not produce an ambiguous reading")
	}
	if ambiguous.ProfileCount != 2 {
		t.Fatalf("ProfileCount = %d, want 2", ambiguous.ProfileCount)
	}

	old := loadZoteroFileStorage
	loadZoteroFileStorage = func() (zoteroprefs.FileStorage, error) { return ambiguous, nil }
	resetZoteroFileStorageCache()
	t.Cleanup(func() { loadZoteroFileStorage = old; resetZoteroFileStorageCache() })

	detail := storedUploadRefusal(t.Context(), &rootFlags{}, storedUploadToExistingItem)
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
	addDoctorFileStorageReport(t.Context(), report, &rootFlags{})
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
			if u.Path != tc.path {
				t.Fatalf("decoded path = %q, want the original %q", u.Path, tc.path)
			}
		})
	}
}

// A relative path must denote the file that was actually read. Prefixing "/"
// would claim a location at the filesystem root that was never opened, and
// that value syncs as attachment metadata. The earlier version of this case
// asserted the buggy output and called it a round trip.
func TestLocalFileURLResolvesRelativePathsAgainstTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	got := localFileURL(filepath.Join("papers", "scan.pdf"))

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	want := filepath.Join(dir, "papers", "scan.pdf")
	// macOS resolves TempDir through /private; compare on the real path.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		want = filepath.Join(resolved, "papers", "scan.pdf")
	}
	if u.Path != want && u.Path != filepath.Join(dir, "papers", "scan.pdf") {
		t.Fatalf("localFileURL(relative) = %q (path %q), want it resolved under %q", got, u.Path, dir)
	}
	if strings.HasPrefix(u.Path, "/papers/") {
		t.Fatalf("localFileURL(relative) = %q, which claims the filesystem root", got)
	}
}

// The refusal must distinguish "no local route exists" from "the local route
// exists but is not available right now". Telling an operator the connector
// cannot address an existing item, when the real problem is that Zotero is
// closed, sends them looking for a limitation instead of opening an app.
func TestRefusalNamesWhyTheLocalRouteIsUnavailable(t *testing.T) {
	stubZoteroFileStorage(t, "webdav", true)

	existing := storedUploadRefusal(t.Context(), &rootFlags{}, storedUploadToExistingItem)
	if !strings.Contains(existing, "already exists in the library") {
		t.Fatalf("existing-item refusal = %q, want the unaddressable-parent reason", existing)
	}

	// Assert the forced-web explanation POSITIVELY. Checking only that the
	// existing-item sentence is absent let the whole clause be deleted.
	forced := storedUploadRefusal(t.Context(), &rootFlags{via: "web"},
		storedUploadCreateFellBack(createRoute{via: "web", cause: webRouteCauseExplicitWeb}))
	if !strings.Contains(forced, "--via web forces") {
		t.Fatalf("create refusal = %q, want the forced-web route named", forced)
	}
	if strings.Contains(forced, "already exists in the library") {
		t.Fatalf("create refusal = %q, must not claim the item already exists", forced)
	}

	// The apply-time backstop serves callers on both sides, so it must claim
	// neither.
	unknown := storedUploadRefusal(t.Context(), &rootFlags{}, storedUploadRouteUnknown)
	if strings.Contains(unknown, "already exists in the library") {
		t.Fatalf("backstop refusal = %q, want no claim about alternatives", unknown)
	}
	if !strings.Contains(unknown, "webdav") && !strings.Contains(unknown, "WebDAV") {
		t.Fatalf("backstop refusal = %q, want the store still named", unknown)
	}
}

// The recorded cause must survive into the message unchanged. Re-probing the
// connector to explain an earlier decision reported what was true at the time
// of the probe, which is how an automatic fallback came to be described as an
// operator forcing --via web.
func TestRouteClauseRendersTheRecordedCauseNotCurrentState(t *testing.T) {
	cases := []struct {
		name  string
		route storedUploadRoute
		want  string
		deny  string
	}{
		{
			name:  "explicit web",
			route: storedUploadCreateFellBack(createRoute{cause: webRouteCauseExplicitWeb}),
			want:  "--via web forces",
			deny:  "not running",
		},
		{
			name:  "connector unavailable",
			route: storedUploadCreateFellBack(createRoute{cause: webRouteCauseConnectorUnavailable, detail: "Zotero desktop is not running (nothing answered on 127.0.0.1:23119) — start Zotero and run this again"}),
			want:  "not running",
			deny:  "--via web forces",
		},
		{
			name:  "group target",
			route: storedUploadCreateFellBack(createRoute{cause: webRouteCauseGroup}),
			want:  "no group parameter",
			deny:  "--via web forces",
		},
		{
			name:  "non-local base",
			route: storedUploadCreateFellBack(createRoute{cause: webRouteCauseNonLocalBase}),
			want:  "not a local Zotero API",
			deny:  "not running",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := storedUploadRouteClause(tc.route)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("clause = %q, want it to name %q", got, tc.want)
			}
			if strings.Contains(got, tc.deny) {
				t.Fatalf("clause = %q, must not claim %q", got, tc.deny)
			}
		})
	}
}

// A non-local base means the desktop connector is not reachable at all, which
// is a different sentence from "Zotero is not running".
func TestConnectorUnavailableReasonNamesTheActualObstacle(t *testing.T) {
	remote := &rootFlags{configPath: testConfigFile(t, "https://api.zotero.org/users/1")}
	if got := connectorUnavailableReason(t.Context(), remote); !strings.Contains(got, "not a local Zotero API") {
		t.Fatalf("remote base reason = %q, want the non-local base named", got)
	}

	// A group target never reaches the personal-WebDAV route clause, so assert
	// the behaviour that actually governs it: the refusal for a group library
	// must not offer the connector, which has no group parameter.
	grouped := &rootFlags{group: "12345", configPath: testConfigFile(t, "http://localhost:23119/api/users/0")}
	if got := connectorUnavailableReason(t.Context(), grouped); !strings.Contains(got, "group") {
		t.Fatalf("group reason = %q, want the group limitation named", got)
	}
	for _, step := range storedUploadRefusalRemediation(storedUploadReasonGroupSyncOff) {
		if strings.Contains(step, "--via connector") {
			t.Fatalf("group remediation offers the connector, which has no group parameter: %q", step)
		}
	}
}

// A ping failure has several causes and they need different fixes. Collapsing
// them into "Zotero is not running" made two of the three false.
func TestConnectorPingFailureIsClassifiedNotCollapsed(t *testing.T) {
	refused := &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	if got := describeConnectorPingFailure(context.Background(), refused); !strings.Contains(got, "not running") {
		t.Fatalf("connection refused = %q, want the desktop reported as not running", got)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	got := describeConnectorPingFailure(cancelled, context.Canceled)
	if strings.Contains(got, "not running") {
		t.Fatalf("cancellation = %q, must not claim Zotero is stopped", got)
	}
	if !strings.Contains(got, "cancelled") {
		t.Fatalf("cancellation = %q, want the cancellation named", got)
	}

	deadline := describeConnectorPingFailure(context.Background(), context.DeadlineExceeded)
	if strings.Contains(deadline, "not running") {
		t.Fatalf("timeout = %q, must not claim Zotero is stopped", deadline)
	}
}

// The branch an operator hits with Zotero closed, proven against a real closed
// port rather than a cancelled context. A cancelled context fails the ping no
// matter what the desktop is doing, so it could never distinguish a stopped
// Zotero from a running one.
func TestUnreachableConnectorProducesTheStartZoteroRefusal(t *testing.T) {
	stubZoteroFileStorage(t, "webdav", true)
	allowPrivateOutboundForTests = true
	t.Cleanup(func() { allowPrivateOutboundForTests = false })

	// Bind and immediately release a port so the address is genuinely closed
	// but routable, giving a deterministic connection-refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closedAddr := ln.Addr().String()
	_ = ln.Close()

	conn := connector.New("http://"+closedAddr+"/connector", 2*time.Second)
	detail := describeConnectorPingFailure(t.Context(), connectorPing(t.Context(), conn))
	if !strings.Contains(detail, "not running") {
		t.Fatalf("detail = %q, want a closed port reported as Zotero not running", detail)
	}
	if !strings.Contains(detail, "start Zotero") && !strings.Contains(detail, "Start Zotero") {
		t.Fatalf("detail = %q, want an actionable instruction", detail)
	}

	// And the refusal built from that recorded cause says the same thing,
	// without blaming an existing item or a flag the caller never passed.
	refusal := storedUploadRefusal(t.Context(), &rootFlags{},
		storedUploadCreateFellBack(createRoute{cause: webRouteCauseConnectorUnavailable, detail: detail}))
	if !strings.Contains(refusal, "not running") {
		t.Fatalf("refusal = %q, want the unreachable desktop named", refusal)
	}
	if strings.Contains(refusal, "already exists in the library") {
		t.Fatalf("refusal = %q, must not blame an existing item for a create", refusal)
	}
	if strings.Contains(refusal, "--via web forces") {
		t.Fatalf("refusal = %q, must not blame --via web when the desktop is unreachable", refusal)
	}
}

// The remediation used to be reachable only under --json, so the terminal told
// operators they were blocked and never how to proceed.
func TestHumanPreconditionMessageCarriesTheRemediation(t *testing.T) {
	steps := []string{"Do the first thing.", "Do the second thing."}
	msg := humanPreconditionMessage("attachments add", "zotero_file_storage", "the store is wrong", steps)

	for _, want := range append([]string{"attachments add", "zotero_file_storage", "the store is wrong"}, steps...) {
		if !strings.Contains(msg, want) {
			t.Fatalf("message = %q, missing %q", msg, want)
		}
	}
	if !strings.Contains(msg, "What to do instead:") {
		t.Fatalf("message = %q, want the remediation introduced", msg)
	}
	// One heading, and the detail must not be repeated under it.
	if strings.Count(msg, "the store is wrong") != 1 {
		t.Fatalf("message = %q, want the detail stated once", msg)
	}
}

// Human output carries the remediation inline; JSON keeps a single-line error
// because the envelope already carries the list in a parseable field.
func TestPreconditionRemediationReachesBothSurfaces(t *testing.T) {
	human := &bytes.Buffer{}
	humanErr := emitPreconditionUnmet(human, &rootFlags{}, "attachments add", preconditionZoteroFileStorage, "the store is wrong")
	if humanErr == nil {
		t.Fatal("emitPreconditionUnmet returned nil for a failure")
	}
	if human.Len() != 0 {
		t.Fatalf("non-JSON run wrote %q to stdout; the error is the only channel", human.String())
	}
	if !strings.Contains(humanErr.Error(), "--allow-zotero-cloud") {
		t.Fatalf("human error = %q, want the remediation visible without --json", humanErr.Error())
	}
	if ExitCode(humanErr) != 9 {
		t.Fatalf("exit code = %d, want 9", ExitCode(humanErr))
	}

	jsonOut := &bytes.Buffer{}
	jsonErr := emitPreconditionUnmet(jsonOut, &rootFlags{asJSON: true}, "attachments add", preconditionZoteroFileStorage, "the store is wrong")
	var env struct {
		Kind        string   `json:"kind"`
		Remediation []string `json:"remediation"`
	}
	if err := json.Unmarshal(jsonOut.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Kind != "precondition_unmet" || len(env.Remediation) == 0 {
		t.Fatalf("envelope = %+v, want the remediation in a parseable field", env)
	}
	if strings.Contains(jsonErr.Error(), "\n") {
		t.Fatalf("json error = %q, want a single line for logs", jsonErr.Error())
	}
}

// Profile evidence is machine-wide and cannot be bound to the Zotero account a
// command targets (dev/adr/0006-unbound-profile-evidence.md), so a profile
// belonging to a DIFFERENT account can refuse a correct upload. The refusal is
// only recoverable if it says which profile the evidence came from.
//
// The representative profile is NOT that answer. riskRank orders storage MODES
// only, so a sync-off hazard is routinely carried by a profile that lost the
// ranking: reporting the representative names a profile whose own settings
// contradict the message, sending the operator to check the wrong file.
func TestRefusalNamesTheProfileItsEvidenceCameFromNotTheRepresentative(t *testing.T) {
	root := t.TempDir()

	// Innocent: Zotero's cloud, syncing on. Preferred, and tied on risk rank,
	// so this is the representative.
	innocent := filepath.Join(root, "innocent.default")
	writeTestPrefs(t, innocent, `
user_pref("extensions.zotero.sync.storage.protocol", "zotero");
user_pref("extensions.zotero.sync.storage.enabled", true);
`)
	// Guilty: same mode, so it never becomes the representative, but it is the
	// only profile positively saying file syncing is off.
	guilty := filepath.Join(root, "guilty.other")
	writeTestPrefs(t, guilty, `
user_pref("extensions.zotero.sync.storage.protocol", "zotero");
user_pref("extensions.zotero.sync.storage.enabled", false);
`)

	fs, err := zoteroprefs.LoadAcrossForTest([]string{innocent, guilty}, innocent)
	if err != nil {
		t.Fatalf("LoadAcrossForTest: %v", err)
	}
	if fs.ProfilePath != innocent {
		t.Fatalf("representative = %q, want the innocent profile %q; the fixture no longer reproduces the mis-attribution", fs.ProfilePath, innocent)
	}
	stubLoadedZoteroFileStorage(t, fs)

	verdict := storedUploadDecision(t.Context(), &rootFlags{}, storedUploadToExistingItem)
	if !verdict.refused() {
		t.Fatal("a sync-off profile did not refuse")
	}
	if !slices.Equal(verdict.profiles, []string{guilty}) {
		t.Fatalf("verdict.profiles = %v, want exactly the contributing profile %q", verdict.profiles, guilty)
	}

	steps := strings.Join(storedUploadRefusalSteps(verdict), " ")
	if !strings.Contains(steps, guilty) {
		t.Fatalf("remediation = %q, want the profile whose setting caused the refusal", steps)
	}
	if strings.Contains(steps, innocent) {
		t.Fatalf("remediation = %q, must not name a profile that has file syncing switched ON", steps)
	}
	if !strings.Contains(steps, zoteroprefs.ProfileDirEnv) {
		t.Fatalf("remediation = %q, want the pinning override named", steps)
	}
}

// Every contributor is named, not just the first: an operator who checks one
// path, finds it innocent of the setting described, and is told nothing about
// the second has no way to reach the truth.
func TestRefusalNamesEveryContributingProfile(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "a.default")
	second := filepath.Join(root, "b.other")
	for _, dir := range []string{first, second} {
		writeTestPrefs(t, dir, `
user_pref("extensions.zotero.sync.storage.protocol", "webdav");
user_pref("extensions.zotero.sync.storage.url", "nas.example.com/zotero");
`)
	}

	fs, err := zoteroprefs.LoadAcrossForTest([]string{first, second}, first)
	if err != nil {
		t.Fatalf("LoadAcrossForTest: %v", err)
	}
	stubLoadedZoteroFileStorage(t, fs)

	verdict := storedUploadDecision(t.Context(), &rootFlags{}, storedUploadToExistingItem)
	if !slices.Equal(verdict.profiles, []string{first, second}) {
		t.Fatalf("verdict.profiles = %v, want both WebDAV profiles", verdict.profiles)
	}
	steps := strings.Join(storedUploadRefusalSteps(verdict), " ")
	for _, want := range []string{first, second} {
		if !strings.Contains(steps, want) {
			t.Fatalf("remediation = %q, missing contributing profile %q", steps, want)
		}
	}
}
