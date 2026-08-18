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
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"

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
	// zoteroFileStorageMutationRead records that this invocation has already
	// taken its one fresh reading at the planning/mutation boundary.
	zoteroFileStorageMutationRead bool
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
	zoteroFileStorageMutationRead = false
}

// refreshZoteroFileStorageForMutation forces ONE re-read at the boundary where
// an invocation stops planning and starts moving bytes.
//
// Preflight populates the memo while deciding whether a command may run. Apply
// then reused that same snapshot, so the apply-time backstop was a second look
// at the first reading rather than an independent check, and a desktop
// reconfigured in between was judged by a stale answer. Re-reading once here
// closes that window without paying for a read per attachment in a bulk import.
func refreshZoteroFileStorageForMutation() {
	if zoteroFileStorageMutationRead {
		return
	}
	zoteroFileStorageMutationRead = true
	zoteroFileStorageVal = zoteroprefs.FileStorage{}
	zoteroFileStorageErr = nil
	zoteroFileStorageLoaded = false
}

// (The library prefix is parsed by baseURLLibraryPrefix, which anchors to the
// path tail rather than scanning for a pattern anywhere in the URL.)

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
// a group.
//
// The library prefix is the TAIL of a Zotero base URL, and this enforces that
// rather than merely preferring it. Scanning for the last "/groups/<id>"
// anywhere in the path classified a reverse-proxy or deployment base such as
// /proxy/groups/tenant/v1 as a group, and a false group verdict skips the
// personal-WebDAV refusal entirely — the silent misroute this guard exists to
// prevent. Only the final two non-empty path segments are consulted, so a
// "/groups/" that is not the library prefix cannot be mistaken for one.
func baseURLTargetsGroup(baseURL string) bool {
	kind, _, ok := baseURLLibraryPrefix(baseURL)
	return ok && kind == "groups"
}

// baseURLLibraryPrefix returns the trailing /users/<id> or /groups/<id> library
// segment of a Zotero API base URL. A base whose path does not END in one is
// not a library base and reports ok=false, leaving the caller to decide rather
// than guessing from arbitrary path text.
func baseURLLibraryPrefix(baseURL string) (kind, id string, ok bool) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", "", false
	}
	segments := strings.Split(u.Path, "/")
	// Trailing slashes are common in configured bases and are not significant.
	for len(segments) > 0 && segments[len(segments)-1] == "" {
		segments = segments[:len(segments)-1]
	}
	if len(segments) < 2 {
		return "", "", false
	}
	kind = segments[len(segments)-2]
	id = segments[len(segments)-1]
	if kind != "users" && kind != "groups" {
		return "", "", false
	}
	if id == "" {
		return "", "", false
	}
	return kind, id, true
}

// storedUploadRouteKind describes what the Web API uploader was asked to do.
// It decides whether the refusal can name an actionable local alternative:
// "no local route exists" and "the local route exists but is not available
// right now" are different problems with different fixes.
type storedUploadRouteKind int

const (
	// storedUploadRouteKindUnknown makes no claim about alternatives. Used by
	// the apply-time backstop, which is shared by callers in both situations.
	storedUploadRouteKindUnknown storedUploadRouteKind = iota
	// storedUploadKindExistingItem: the file must attach to an item already in
	// the library, which the connector cannot address at all.
	storedUploadKindExistingItem
	// storedUploadKindCreateFellBack: the item would have been created
	// alongside its file, so the connector could have carried this and
	// something prevented it. That something is recorded, not re-derived.
	storedUploadKindCreateFellBack
)

// storedUploadRoute is the immutable explanation of one refusal's routing.
// The cause is supplied by whoever made the decision; the guard never probes
// to reconstruct it, because a probe answers for now rather than for then.
type storedUploadRoute struct {
	kind  storedUploadRouteKind
	cause webRouteCause
	// detail carries the classified connector failure for
	// webRouteCauseConnectorUnavailable.
	detail string
}

// storedUploadRouteUnknown is the no-claim route.
var storedUploadRouteUnknown = storedUploadRoute{kind: storedUploadRouteKindUnknown}

// storedUploadToExistingItem is the route for attaching to an item that is
// already in the library.
var storedUploadToExistingItem = storedUploadRoute{kind: storedUploadKindExistingItem}

// storedUploadCreateFellBack builds the route for a create that the Web API
// uploader picked up, carrying the recorded reason it did.
func storedUploadCreateFellBack(route createRoute) storedUploadRoute {
	return storedUploadRoute{
		kind:   storedUploadKindCreateFellBack,
		cause:  route.cause,
		detail: route.detail,
	}
}

// storedUploadReason identifies WHICH problem a refusal is about. The three
// are genuinely different — a wrong destination, a destination that is right
// but will never be fetched, and a destination that could not be established —
// and each needs its own remediation. One WebDAV-shaped list of alternatives
// attached to all of them told group callers to use a connector that has no
// group parameter.
type storedUploadReason int

const (
	storedUploadReasonNone storedUploadReason = iota
	// storedUploadReasonWebDAV: the desktop keeps personal files on WebDAV, so
	// a Web API upload lands in the wrong store.
	storedUploadReasonWebDAV
	// storedUploadReasonPersonalSyncOff: personal file syncing is switched off.
	storedUploadReasonPersonalSyncOff
	// storedUploadReasonGroupSyncOff: group file syncing is switched off.
	storedUploadReasonGroupSyncOff
	// storedUploadReasonUnevaluable: Zotero is installed here but its
	// configuration could not be read, so the destination is unproven.
	storedUploadReasonUnevaluable
)

// storedUploadVerdict is a refusal and the reason behind it, so that the
// remediation can be chosen from the reason instead of assumed.
type storedUploadVerdict struct {
	reason storedUploadReason
	detail string
}

// refused reports whether the upload must not proceed.
func (v storedUploadVerdict) refused() bool { return v.detail != "" }

// storedUploadRefusal reports why a Zotero Web API stored-file upload must not
// run for the targeted library, or "" when it may proceed.
func storedUploadRefusal(ctx context.Context, flags *rootFlags, route storedUploadRoute) string {
	return storedUploadDecision(ctx, flags, route).detail
}

// storedUploadDecision is storedUploadRefusal plus the reason, for callers that
// must tailor remediation or reporting to it.
//
// The decision reads the UNION of hazards across every readable profile, not
// the single representative reading: a profile positively saying "file syncing
// is off" is independent evidence and must not be discarded because a
// different profile happened to have a riskier storage mode.
//
// Absence of evidence still permits the upload — zotio is routinely run against
// the Web API on machines with no desktop at all, and refusing there would
// block correct work for no reason. But INABILITY TO EVALUATE evidence that
// demonstrably exists is not absence, and no longer permits it: an unreadable
// profile, an explicit pin that resolves to nothing, or a storage protocol this
// package cannot decode are all positive signs that the reader is outside its
// contract, and treating them as consent re-opens the misroute.
func storedUploadDecision(ctx context.Context, flags *rootFlags, route storedUploadRoute) storedUploadVerdict {
	if flags != nil && flags.allowZoteroCloud {
		return storedUploadVerdict{}
	}
	fs, err := zoteroFileStorage()
	if err != nil {
		return storedUploadVerdict{
			reason: storedUploadReasonUnevaluable,
			detail: fmt.Sprintf(
				"Zotero desktop's file-storage configuration could not be read, so this upload's destination cannot be established (%v)", err),
		}
	}
	if !fs.Found() {
		// No Zotero on this host says nothing about a misroute.
		return storedUploadVerdict{}
	}
	if fs.AnyUnreadableProfile() {
		return storedUploadVerdict{
			reason: storedUploadReasonUnevaluable,
			detail: fmt.Sprintf(
				"Zotero desktop has %d profile(s) here whose configuration could not be read, so a WebDAV setting among them cannot be ruled out and this upload's destination is unproven",
				fs.UnreadableProfileCount()),
		}
	}
	// Zotero always uses its own storage for group libraries — WebDAV is a
	// personal-library setting — so a Web API upload is the correct route
	// there. Only file syncing being switched off is worth naming.
	if storedUploadTargetsGroup(flags) {
		if fs.AnyGroupSyncDisabled() {
			return storedUploadVerdict{
				reason: storedUploadReasonGroupSyncOff,
				detail: "Zotero desktop has group-library file syncing turned off (sync.storage.groups.enabled is false), so a stored attachment uploaded through the Zotero Web API would be billed against the group owner's storage plan and never downloaded by Zotero",
			}
		}
		return storedUploadVerdict{}
	}

	switch {
	case fs.AnyPersonalWebDAV():
		detail := fmt.Sprintf(
			"Zotero desktop keeps personal-library attachment files on %s, but a stored attachment uploaded through the Zotero Web API always lands in Zotero's own cloud storage and is billed against that storage plan",
			fs.Describe(zoteroprefs.StorageWebDAV))
		if clause := storedUploadRouteClause(route); clause != "" {
			detail += ". " + clause
		}
		if fs.Ambiguous {
			detail += fmt.Sprintf(
				". Zotero has %d profiles here and which one is running cannot be determined, so this reads the WebDAV one; pin it with %s if that is not the profile you use",
				fs.ProfileCount, zoteroprefs.ProfileDirEnv)
		}
		return storedUploadVerdict{reason: storedUploadReasonWebDAV, detail: detail}
	case fs.AnyPersonalSyncDisabled():
		// A separate problem from the WebDAV mismatch: the destination is
		// right, but Zotero will never download what is uploaded.
		return storedUploadVerdict{
			reason: storedUploadReasonPersonalSyncOff,
			detail: "Zotero desktop has personal-library file syncing turned off (sync.storage.enabled is false), so a stored attachment uploaded through the Zotero Web API would consume the account's storage plan and never be downloaded by Zotero",
		}
	case fs.AnyPersonalModeUnknown():
		// A protocol this package does not model. The destination is not
		// Zotero's cloud by default; it is simply unknown.
		return storedUploadVerdict{
			reason: storedUploadReasonUnevaluable,
			detail: "Zotero desktop is configured with a file-storage protocol this version of zotio does not recognise, so this upload's destination cannot be established",
		}
	default:
		return storedUploadVerdict{}
	}
}

// storedUploadRouteClause explains why the local route is not carrying this
// upload. Saying "the connector cannot attach to an existing item" when the
// real problem is that Zotero is closed sends the operator looking for a
// limitation instead of opening an application.
//
// Every branch renders a cause that was RECORDED when the route was chosen.
func storedUploadRouteClause(route storedUploadRoute) string {
	switch route.kind {
	case storedUploadKindExistingItem:
		return "Zotero's connector cannot attach a file to an item that already exists in the library, so this upload has no local route"
	case storedUploadKindCreateFellBack:
		switch route.cause {
		case webRouteCauseExplicitWeb:
			return "--via web forces the Zotero Web API uploader; '--via auto' (the default) or '--via connector' creates the item and its file together in Zotero desktop, which reaches the configured store"
		case webRouteCauseGroup:
			return "a group target keeps this on the Zotero Web API, because the desktop connector has no group parameter"
		case webRouteCauseNonLocalBase:
			return "the configured base_url is not a local Zotero API, so the desktop connector could not carry this"
		case webRouteCauseConnectorUnavailable:
			clause := "creating the item and its file together in one Zotero desktop session would reach the configured store"
			if route.detail != "" {
				return clause + ", but " + route.detail
			}
			return clause + ", but the desktop connector did not answer"
		default:
			return ""
		}
	default:
		return ""
	}
}

// describeConnectorPingFailure classifies why a connector ping failed, in the
// operator's terms.
//
// Collapsing every failure into "Zotero is not running" made three different
// problems indistinguishable and two of them false. A cancelled or timed-out
// check says nothing about whether Zotero is running, and reporting it as a
// stopped application sends the operator to the wrong fix.
func describeConnectorPingFailure(ctx context.Context, err error) string {
	if err == nil {
		return ""
	}
	// The caller's context dying is a fact about this invocation, not about
	// the desktop. Check it first: a cancelled request also surfaces as a
	// transport error, and the transport reading would be the misleading one.
	switch {
	case errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled:
		return "the reachability check was cancelled before Zotero answered, so whether the desktop is running is unknown"
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded:
		return "Zotero did not answer on 127.0.0.1:23119 within the timeout — it may be starting, busy, or stopped"
	}
	var connErr *net.OpError
	if errors.As(err, &connErr) || errors.Is(err, syscall.ECONNREFUSED) {
		return "Zotero desktop is not running (nothing answered on 127.0.0.1:23119) — start Zotero and run this again"
	}
	// A reachable port that answered wrongly is a different problem again, and
	// the operator needs the actual error to act on it.
	return fmt.Sprintf("the desktop connector on 127.0.0.1:23119 did not answer usably (%v)", err)
}

// connectorUnavailableReason names, in the operator's terms, why the desktop
// connector cannot be used for this invocation, or "" when it is available.
func connectorUnavailableReason(ctx context.Context, flags *rootFlags) string {
	if flags == nil {
		return ""
	}
	if strings.TrimSpace(flags.group) != "" {
		return "the desktop connector has no group parameter, so group writes cannot use it"
	}
	conn, err := flags.newConnector()
	if err != nil {
		return "the configured base_url is not a local Zotero API, so the desktop connector is not reachable from here"
	}
	if err := connectorPing(ctx, conn); err != nil {
		return describeConnectorPingFailure(ctx, err)
	}
	return ""
}

// storedUploadRefusalRemediation lists the routes that do reach a file store
// other than Zotero's cloud, chosen for the reason actually being refused.
//
// A single WebDAV-shaped list was wrong for the other reasons: it told a group
// caller to create the item with '--via connector', which has no group
// parameter, and promised that attaching in Zotero desktop would sync the
// bytes, which is exactly what a syncing-disabled profile will not do.
func storedUploadRefusalRemediation(reason storedUploadReason) []string {
	const linkedFile = "Use '--mode linked-file' (or '--attach-mode linked-file') to record the local path without uploading; the file stays on this machine and is not synced."
	const override = "Pass --allow-zotero-cloud to upload into Zotero's cloud storage anyway."

	switch reason {
	case storedUploadReasonPersonalSyncOff:
		return []string{
			"Turn personal file syncing back on in Zotero: Settings -> Sync -> File Syncing -> 'Sync attachment files in My Library'.",
			linkedFile,
			override,
		}
	case storedUploadReasonGroupSyncOff:
		return []string{
			"Turn group file syncing back on in Zotero: Settings -> Sync -> File Syncing -> 'Sync attachment files in group libraries'.",
			linkedFile,
			override,
		}
	case storedUploadReasonUnevaluable:
		return []string{
			fmt.Sprintf("Point %s at the Zotero profile this library belongs to, so the destination can be established.", zoteroprefs.ProfileDirEnv),
			linkedFile,
			override,
		}
	default:
		return []string{
			"Attach the file in Zotero desktop (right-click the item -> Add Attachment -> Attach Stored Copy of File); Zotero then syncs the bytes to your own file store.",
			"For a NEW item, create it and its file in one desktop session: 'zotio import apply --attach-mode stored --via connector', which hands the bytes to Zotero instead of uploading them.",
			linkedFile,
			override,
		}
	}
}

// guardStoredUpload is the apply-time backstop. Every stored Web API upload
// funnels through applyStoredUpload, so placing the check there catches the
// routes that preflight cannot decide in advance: import apply's per-entry
// fallback to the Web route, and import pdf's retro-attach onto a duplicate.
//
// Those callers sit on both sides of the existing-item split, so this makes no
// claim about alternatives; the preflight refusals, which know, do.
func guardStoredUpload(ctx context.Context, flags *rootFlags) error {
	refreshZoteroFileStorageForMutation()
	verdict := storedUploadDecision(ctx, flags, storedUploadRouteUnknown)
	if !verdict.refused() {
		return nil
	}
	return fmt.Errorf("refusing stored upload: %s; %s", verdict.detail,
		strings.Join(storedUploadRefusalRemediation(verdict.reason), " "))
}

// refuseStoredWebUpload emits the standard precondition_unmet envelope when a
// command is about to use the Zotero Web API stored-file uploader against a
// library whose files Zotero keeps elsewhere. Returns nil when the upload may
// proceed, so callers can guard inline.
func refuseStoredWebUpload(cmd *cobra.Command, flags *rootFlags, capability string, route storedUploadRoute) error {
	verdict := storedUploadDecision(cmd.Context(), flags, route)
	if !verdict.refused() {
		return nil
	}
	return emitPreconditionUnmetWithRemediation(cmd.OutOrStdout(), flags, capability,
		preconditionZoteroFileStorage, verdict.detail,
		storedUploadRefusalRemediation(verdict.reason))
}

// addDoctorFileStorageReport records where Zotero desktop keeps attachment
// files, and whether stored uploads are consequently refused. Reported for the
// library this invocation targets, since WebDAV is personal-library only.
func addDoctorFileStorageReport(ctx context.Context, report map[string]any, flags *rootFlags) {
	fs, err := zoteroFileStorage()
	if err != nil {
		report["file_storage"] = sanitizeForTerminal(fmt.Sprintf("unreadable (%v); stored uploads are refused because the destination cannot be established", err))
		report["file_storage_hint"] = fmt.Sprintf("point %s at the Zotero profile this library belongs to, or pass --allow-zotero-cloud", zoteroprefs.ProfileDirEnv)
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
	// Report file syncing from the same hazard UNION the guard decides on. The
	// representative profile's own flag disagreed with the verdict whenever a
	// sibling profile was the one with syncing switched off, so doctor showed
	// an enabled library and refused it in the same breath.
	syncOff := !lib.Enabled
	if scope == "personal library" {
		syncOff = syncOff || fs.AnyPersonalSyncDisabled()
	} else {
		syncOff = syncOff || fs.AnyGroupSyncDisabled()
	}
	if syncOff {
		desc += ", file syncing off"
	}

	// Hints accumulate: an ambiguous refusal needs BOTH how to work around the
	// refusal and how to resolve the ambiguity that produced it.
	var hints []string
	if fs.Ambiguous {
		desc += fmt.Sprintf(", ambiguous across %d profiles", fs.ProfileCount)
		hints = append(hints, fmt.Sprintf("several Zotero profiles exist and the running one cannot be determined; pin it with %s", zoteroprefs.ProfileDirEnv))
	}
	if fs.AnyUnreadableProfile() {
		desc += fmt.Sprintf(", %d unreadable", fs.UnreadableProfileCount())
	}

	verdict := storedUploadDecision(ctx, flags, storedUploadRouteUnknown)
	switch {
	case verdict.refused():
		desc += " — stored uploads via the Web API are refused: " + refusalSummary(verdict.reason)
		// The doctor hint mirrors the refusal's own remediation, so the two
		// surfaces cannot drift into giving different advice.
		hints = append(hints, strings.Join(storedUploadRefusalRemediation(verdict.reason), " "))
	case flags != nil && flags.allowZoteroCloud:
		desc += " — stored uploads forced into Zotero's cloud storage by --allow-zotero-cloud"
	default:
		desc += " — stored uploads via the Web API reach the configured store"
	}
	report["file_storage"] = sanitizeForTerminal(desc)
	if len(hints) > 0 {
		report["file_storage_hint"] = sanitizeForTerminal(strings.Join(hints, "; also: "))
	}
}

// refusalSummary names, in a few words, which problem doctor is reporting.
func refusalSummary(reason storedUploadReason) string {
	switch reason {
	case storedUploadReasonNone:
		// Not a refusal; nothing to summarise.
		return ""
	case storedUploadReasonWebDAV:
		return "they would go to Zotero's cloud storage instead of the configured WebDAV store"
	case storedUploadReasonPersonalSyncOff:
		return "personal-library file syncing is off, so Zotero would never download them"
	case storedUploadReasonGroupSyncOff:
		return "group-library file syncing is off, so Zotero would never download them"
	case storedUploadReasonUnevaluable:
		return "the destination could not be established"
	default:
		return "the destination is not the configured store"
	}
}
