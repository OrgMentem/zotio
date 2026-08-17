// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Guard for stored attachment uploads that would reach the wrong file store.
//
// A "stored" attachment uploaded through the Zotero Web API file-upload
// protocol always lands in Zotero's own cloud storage and is billed against
// the account's storage plan. That is wrong for an operator whose Zotero
// desktop keeps attachment files on a personal WebDAV server: the bytes end up
// in a plan they do not use, and never reach the server they configured.
//
// There is no local alternative for a file that must attach to an item which
// already exists in the library. Zotero's desktop connector resolves
// /connector/saveAttachment's parentItemID exclusively through the save
// session that created it (SaveSession.getItemByConnectorKey, a lookup in an
// in-memory per-session map), so a real library item key is not addressable:
// it answers 500 for a live session and 400 SESSION_NOT_FOUND otherwise.
// Verified against Zotero 7 on 2026-08-17.
//
// So the honest behaviour is a refusal that names the mismatch, not a silent
// upload into the wrong store. `import apply --attach-mode stored --via
// connector` remains the working local route, because there zotio creates the
// parent and the file inside one connector session.
//
// # Strength of the guarantee
//
// This is a best-effort guard, not a proof. It reads Zotero desktop's prefs.js,
// which Zotero flushes lazily, so the file can lag the running application in
// EITHER direction: a desktop just switched to WebDAV may still read as cloud
// (the upload proceeds and is misrouted), and one just switched to cloud may
// still read as WebDAV (a correct upload is refused). Neither is eliminable
// from disk state alone. The design keeps the recoverable direction cheap —
// a wrong refusal is undone with --allow-zotero-cloud — and refuses whenever
// the evidence points at a misroute, including when several Zotero profiles
// make the running configuration ambiguous.
//
// Absence of evidence is not treated as evidence: no Zotero installation, an
// unreadable profile, or a preference that cannot be decoded all allow the
// upload, because zotio is routinely run against the Web API on machines with
// no desktop at all.

package cli

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/config"
	"zotio/internal/zoteroprefs"
)

// loadZoteroFileStorage reads Zotero desktop's file-storage configuration. A
// package var so tests can supply a desktop configuration without a profile on
// disk.
var loadZoteroFileStorage = zoteroprefs.Load

// Reading and parsing prefs.js per mutation op would repeat the same work once
// per attachment across a bulk import, so the desktop configuration is
// resolved once per command invocation.
//
// The scope is the INVOCATION, not the process: zotio's MCP server executes
// this same Cobra tree in-process against one long-lived cli.RootCmd, so a
// process-lifetime cache would let a desktop reconfigured mid-session keep
// being judged by a stale reading — re-enabling the exact misroute this guard
// exists to prevent. resetZoteroFileStorageCache runs from the root
// PersistentPreRunE, which every command passes through.
var (
	zoteroFileStorageVal    zoteroprefs.FileStorage
	zoteroFileStorageErr    error
	zoteroFileStorageLoaded bool
)

func zoteroFileStorage() (zoteroprefs.FileStorage, error) {
	if !zoteroFileStorageLoaded {
		zoteroFileStorageVal, zoteroFileStorageErr = loadZoteroFileStorage()
		zoteroFileStorageLoaded = true
	}
	return zoteroFileStorageVal, zoteroFileStorageErr
}

// resetZoteroFileStorageCache drops the memoized desktop configuration so the
// next read reflects the desktop as it is now.
func resetZoteroFileStorageCache() {
	zoteroFileStorageVal = zoteroprefs.FileStorage{}
	zoteroFileStorageErr = nil
	zoteroFileStorageLoaded = false
}

// libraryPrefixPattern matches the /users/<id> or /groups/<id> library segment
// of a Zotero API base URL.
var libraryPrefixPattern = regexp.MustCompile(`/(users|groups)/[^/]+`)

// storedUploadTargetsGroup reports whether this invocation's writes land in a
// group library.
//
// This has to mirror where writes ACTUALLY go, which is not simply what the
// base URL says:
//
//   - --group (or ZOTERO_GROUP, or a named profile) always wins; newClient
//     rewrites the base and resolveWebWriteBase routes to /groups/<id>.
//   - Otherwise a LOCAL base URL is always personal. newClient installs hybrid
//     write routing for any local base and captures only flags.group, so with
//     the flag empty resolveWebWriteBase returns /users/<id> no matter which
//     library the local read base names. Trusting the local path here would
//     read a group and skip the refusal while the bytes go to the personal
//     library — the exact silent misroute this guard exists to prevent.
//   - Only a remote base is classified by its own library prefix, because
//     those writes go to that URL unchanged.
func storedUploadTargetsGroup(flags *rootFlags) bool {
	if flags == nil {
		return false
	}
	if strings.TrimSpace(flags.group) != "" {
		return true
	}
	cfg, err := config.Load(flags.configPath)
	if err != nil || cfg == nil {
		return false
	}
	if isLocalZoteroAPI(cfg.BaseURL) {
		return false
	}
	return baseURLTargetsGroup(cfg.BaseURL)
}

// baseURLTargetsGroup reports whether a remote base URL's library prefix names
// a group. Only the path is examined, and only the LAST library segment counts:
// the library prefix is the tail of a Zotero base URL, so a query string or a
// deployment path prefix that happens to contain "/groups/" must not be
// mistaken for it.
func baseURLTargetsGroup(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	matches := libraryPrefixPattern.FindAllString(u.Path, -1)
	if len(matches) == 0 {
		return false
	}
	return strings.HasPrefix(matches[len(matches)-1], "/groups/")
}

// storedUploadRefusal reports why a Zotero Web API stored-file upload must not
// run for the targeted library, or "" when it may proceed.
//
// The decision reads the UNION of hazards across every readable profile, not
// the single representative reading: a profile positively saying "file syncing
// is off" is independent evidence and must not be discarded because a
// different profile happened to have a riskier storage mode.
func storedUploadRefusal(flags *rootFlags) string {
	if flags != nil && flags.allowZoteroCloud {
		return ""
	}
	fs, err := zoteroFileStorage()
	if err != nil || !fs.Found() {
		// An absent or unreadable profile is not evidence of a misroute.
		return ""
	}
	// Zotero always uses its own storage for group libraries — WebDAV is a
	// personal-library setting — so a Web API upload is the correct route
	// there. Only file syncing being switched off is worth naming.
	if storedUploadTargetsGroup(flags) {
		if fs.AnyGroupSyncDisabled() {
			return "Zotero desktop has group-library file syncing turned off (sync.storage.groups.enabled is false), so a stored attachment uploaded through the Zotero Web API would consume the account's storage plan and never be downloaded by Zotero"
		}
		return ""
	}

	switch {
	case fs.AnyPersonalWebDAV():
		detail := fmt.Sprintf(
			"Zotero desktop keeps personal-library attachment files on %s, but a stored attachment uploaded through the Zotero Web API always lands in Zotero's own cloud storage and is billed against that storage plan. Zotero's connector cannot attach a file to an item that already exists in the library, so this upload has no local route",
			fs.Describe(zoteroprefs.StorageWebDAV))
		if fs.Ambiguous {
			detail += fmt.Sprintf(
				". Zotero has %d profiles here and which one is running cannot be determined, so this reads the WebDAV one; pin it with %s if that is not the profile you use",
				fs.ProfileCount, zoteroprefs.ProfileDirEnv)
		}
		return detail
	case fs.AnyPersonalSyncDisabled():
		// A separate problem from the WebDAV mismatch: the destination is
		// right, but Zotero will never download what is uploaded.
		return "Zotero desktop has personal-library file syncing turned off (sync.storage.enabled is false), so a stored attachment uploaded through the Zotero Web API would consume the account's storage plan and never be downloaded by Zotero"
	default:
		return ""
	}
}

// storedUploadRefusalRemediation lists the routes that do reach a
// non-Zotero-cloud file store, plus the explicit override.
func storedUploadRefusalRemediation() []string {
	return []string{
		"Attach the file in Zotero desktop (right-click the item -> Add Attachment -> Attach Stored Copy of File); Zotero then syncs the bytes to your own file store.",
		"For a NEW item, create it and its file in one desktop session: 'zotio import apply --attach-mode stored --via connector', which hands the bytes to Zotero instead of uploading them.",
		"Use '--mode linked-file' (or '--attach-mode linked-file') to record the local path without uploading; the file stays on this machine and is not synced.",
		"Pass --allow-zotero-cloud to upload into Zotero's cloud storage anyway.",
	}
}

// guardStoredUpload is the apply-time backstop. Every stored Web API upload
// funnels through applyStoredUpload, so placing the check there catches the
// routes that preflight cannot decide in advance: import apply's per-entry
// fallback to the Web route, and import pdf's retro-attach onto a duplicate.
func guardStoredUpload(flags *rootFlags) error {
	detail := storedUploadRefusal(flags)
	if detail == "" {
		return nil
	}
	return fmt.Errorf("refusing stored upload: %s; %s", detail,
		strings.Join(storedUploadRefusalRemediation(), " "))
}

// refuseStoredWebUpload emits the standard precondition_unmet envelope when a
// command is about to use the Zotero Web API stored-file uploader against a
// library whose files Zotero keeps elsewhere. Returns nil when the upload may
// proceed, so callers can guard inline.
func refuseStoredWebUpload(cmd *cobra.Command, flags *rootFlags, capability string) error {
	detail := storedUploadRefusal(flags)
	if detail == "" {
		return nil
	}
	return emitPreconditionUnmet(cmd.OutOrStdout(), flags, capability, preconditionZoteroFileStorage, detail)
}

// addDoctorFileStorageReport records where Zotero desktop keeps attachment
// files, and whether stored uploads are consequently refused. Reported for the
// library this invocation targets, since WebDAV is personal-library only.
func addDoctorFileStorageReport(report map[string]any, flags *rootFlags) {
	fs, err := zoteroFileStorage()
	if err != nil {
		report["file_storage"] = sanitizeForTerminal(fmt.Sprintf("unknown (%v); stored uploads are allowed", err))
		return
	}
	if !fs.Found() {
		report["file_storage"] = "unknown (no Zotero desktop profile found; stored uploads are allowed)"
		return
	}
	// Values derived from another application's config file are not trusted
	// display text; doctor prints report values verbatim.
	report["file_storage_profile"] = sanitizeForTerminal(fs.ProfilePath)

	lib, scope := fs.Personal(), "personal library"
	if storedUploadTargetsGroup(flags) {
		lib, scope = fs.Group(), "group libraries"
	}
	desc := fmt.Sprintf("%s (%s)", fs.Describe(lib.Mode), scope)
	if !lib.Enabled {
		desc += ", file syncing off"
	}

	// Hints accumulate: an ambiguous refusal needs BOTH how to work around the
	// refusal and how to resolve the ambiguity that produced it.
	var hints []string
	if fs.Ambiguous {
		desc += fmt.Sprintf(", ambiguous across %d profiles", fs.ProfileCount)
		hints = append(hints, fmt.Sprintf("several Zotero profiles exist and the running one cannot be determined; pin it with %s", zoteroprefs.ProfileDirEnv))
	}
	switch {
	case storedUploadRefusal(flags) != "":
		desc += " — stored uploads via the Web API are refused; they would go to Zotero's cloud storage instead"
		hints = append(hints, "attach in Zotero desktop, or use 'zotio import apply --attach-mode stored --via connector' for new items; --allow-zotero-cloud overrides")
	case flags != nil && flags.allowZoteroCloud:
		desc += " — stored uploads forced into Zotero's cloud storage by --allow-zotero-cloud"
	case lib.Mode == zoteroprefs.StorageUnknown:
		// Saying uploads "reach the configured store" here would assert exactly
		// what an unrecognised protocol means we could not establish.
		desc += " — the destination could not be determined; stored uploads are allowed because this guard only refuses on positive evidence"
	default:
		desc += " — stored uploads via the Web API reach the configured store"
	}
	report["file_storage"] = sanitizeForTerminal(desc)
	if len(hints) > 0 {
		report["file_storage_hint"] = strings.Join(hints, "; also: ")
	}
}
