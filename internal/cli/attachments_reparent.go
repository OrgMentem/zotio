// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/client"
)

// The connector re-parent route: the only way to attach a file to an item that
// ALREADY exists without the bytes landing in Zotero's own cloud storage.
//
// Zotero's connector resolves a parent only inside its own save session
// (SaveSession.getItemByConnectorKey), so an existing item key is unaddressable
// and `POST /connector/saveAttachment` cannot target it. A Web API stored upload
// can target it, but always uploads into Zotero's cloud storage, which is wrong
// — and refused — when the desktop keeps files on personal WebDAV.
//
// So the file is created where the connector CAN put it, then moved:
//
//  1. create a temporary parent AND the file in one connector session, so the
//     bytes go through the running desktop into whatever file store it uses;
//  2. re-parent the attachment onto the real item with a Web API PATCH;
//  3. trash the now-empty temporary parent.
//
// Every step of this was measured against a real WebDAV-backed library before it
// was built; see dev/field-report-2026-08-22-papio-round2.md. The two facts it
// rests on: Zotero accepts a parentItem change on an already-stored attachment
// (HTTP 204, md5 and mtime unchanged), and every storage name — local directory,
// upload zip, and both WebDAV remote names — derives from the ATTACHMENT's key
// rather than its parent, so the move relocates no bytes.
//
// Ordering is not a preference. The attachment must leave the temporary parent
// BEFORE that parent is trashed.
//
// # Identity, and why it is not the title
//
// Zotero renames a saved file after its PARENT item's title, at save time and
// never retroactively. The temporary parent therefore has to borrow the target's
// title, or the operator's PDF is stored under a zotio marker name.
//
// That makes the title useless as identity: the target is then always a title
// match. The create route's own key recovery is title-based and best-effort, and
// with a two-minute clock-skew floor it can return the TARGET as the sole match
// — after which this route would re-parent inside the target and then trash it.
// Reviewers found that path before it shipped.
//
// So identity is a per-run nonce written into the temporary parent's
// abstractNote. The target cannot carry this run's nonce, recovery matches on it
// rather than on the title, and the one destructive write refuses to act on any
// item that does not carry it.

// connectorTempParentPrefix marks the throwaway item this route creates, in its
// abstractNote. Zotero's search covers that field, so an orphan left by a killed
// process is still findable by this string.
const connectorTempParentPrefix = "[zotio] temporary attachment parent"

// connectorReparentVisibilityTimeout bounds the wait for a connector-created
// item to appear on a plane that can see it.
//
// The desktop commits immediately, but api.zotero.org has been observed lagging
// 15-20s behind it. items_delete.go carries the scar: key SDLDFA9W was reported
// deleted inside that window, then materialised on the write plane untrashed.
// So this polls for real visibility instead of assuming it, and fails honestly
// when the window closes without the item appearing.
const connectorReparentVisibilityTimeout = 90 * time.Second

// connectorReparentRouteTimeout caps the WHOLE route, connector I/O included.
// Four sequential polling loops would otherwise be able to add up to four
// visibility windows, and a hung request under `--timeout 0` would never return
// at all. Every loop derives its deadline from this budget via pollDeadline, so
// the route cannot outlive it however the waits fall.
const connectorReparentRouteTimeout = 5 * time.Minute

// pollDeadline bounds one waiting loop: at most a visibility window, and never
// past the route's own budget.
func pollDeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(connectorReparentVisibilityTimeout)
	if routeDeadline, ok := ctx.Deadline(); ok && routeDeadline.Before(deadline) {
		return routeDeadline
	}
	return deadline
}

// connectorReparentPollInterval paces those waits. A var, not a const, so tests
// can exercise the polling loops without spending real seconds on them, matching
// the existing connectorForCreate seam.
var connectorReparentPollInterval = 3 * time.Second

// connectorChildrenPageSize pages the children reads. Zotero's default page is
// 25, so an unpaged read silently truncates: an item with many attachments would
// hide the sibling that makes a retry a no-op.
const connectorChildrenPageSize = 100

// connectorReparentResult reports what the route did, so a partial failure names
// every object it created. Nothing this route makes may become unfindable.
type connectorReparentResult struct {
	AttachmentKey string
	TempParentKey string
	TempTitle     string
	Nonce         string
	TempTrashed   bool
	// RaceLost records that another run attached this exact content first, so
	// this run abandoned rather than adding a duplicate.
	RaceLost bool
	// Resumed records that this run adopted a previous run's stranded temporary
	// parent instead of creating one, so no second copy of the bytes was made.
	Resumed bool
	// SaveReplyLost records that the connector's save reply never arrived but the
	// desktop had already committed the child, so this run adopted that child
	// instead of reporting a failure it would have posted the bytes again to fix.
	SaveReplyLost bool
}

// newConnectorReparentNonce returns a short random tag identifying one run.
func newConnectorReparentNonce() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generating a run identifier: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// connectorTempParentMarker is the abstractNote written to the temporary parent.
// It carries the nonce (identity) and the target key (so a human reading an
// orphan knows what it was for).
func connectorTempParentMarker(nonce, targetKey string) string {
	return fmt.Sprintf("%s %s for item %s. Safe to delete: zotio creates this to route attachment "+
		"bytes through Zotero desktop, then moves the file to %s and trashes this item.",
		connectorTempParentPrefix, nonce, targetKey, targetKey)
}

// applyConnectorReparentUpload runs the whole route. It returns a mutation
// status, a detail map, and an error, matching applyStoredUpload so both routes
// report through the same envelope.
func applyConnectorReparentUpload(ctx context.Context, cmd *cobra.Command, flags *rootFlags, webClient *client.Client, req storedUploadRequest) (string, any, error) {
	if webClient == nil {
		return "failed", nil, fmt.Errorf("missing write client")
	}

	// Retry-safety first, and against the TARGET rather than the temporary
	// parent: a re-run must no-op instead of leaving a second throwaway item and
	// a duplicate file behind.
	//
	// This route cannot reuse findStoredSibling, which matches on filename and
	// linkMode "imported_file". Neither holds here. Zotero renames a saved file
	// after its parent item's title, so the stored filename is the target's
	// title rather than the file's own name, and the connector creates
	// "imported_url". Matching on either made every retry create another
	// attachment — measured against a real library before this was fixed.
	//
	// The content hash is the only identity that survives both. It depends on
	// Zotero having registered the hash on the write plane, so a retry issued
	// inside that window can still duplicate; the help text says so.
	existing, err := findAttachmentByMD5(webClient, req.ParentKey, req.MD5)
	if err != nil {
		return "failed", nil, err
	}
	if existing != "" {
		return "no_op", map[string]any{
			// attachment_key names the child that holds the file, explicitly.
			// item_key is kept for symmetry with the Web route, but a consumer
			// should prefer attachment_key: it can never be mistaken for the
			// target item, which would record an item as its own attachment.
			"attachment_key": existing,
			"item_key":       existing,
			"parent_key":     req.ParentKey,
			"via":            "connector",
			"note":           "an attachment with identical content is already on this item",
		}, nil
	}

	// One deadline for the whole route. Without it, four visibility loops plus a
	// client configured with `--timeout 0` could hang indefinitely.
	routeCtx, cancel := context.WithTimeout(ctx, connectorReparentRouteTimeout)
	defer cancel()
	// A client carries its own base context, so the budget above bounds nothing
	// until the client is told about it. Without this the deadline caps only the
	// sleeps between polls, not the HTTP calls they wrap.
	//
	// The client belongs to the caller, so the borrow is given back: without the
	// restore this op returns a client permanently bound to a cancelled context,
	// and any later request on it fails with context.Canceled for no visible
	// reason.
	callerCtx := webClient.Context()
	webClient.SetContext(routeCtx)
	defer webClient.SetContext(callerCtx)

	out, err := runConnectorReparent(routeCtx, cmd, flags, webClient, req)
	detail := map[string]any{"via": "connector"}
	if out.TempParentKey != "" {
		detail["temp_parent_key"] = out.TempParentKey
		detail["temp_parent_trashed"] = out.TempTrashed
	}
	if out.TempTitle != "" {
		detail["temp_parent_title"] = sanitizeForTerminal(out.TempTitle)
	}
	if out.AttachmentKey != "" {
		// Emitted explicitly and named unambiguously, so a consumer never has to
		// fall back to a key whose meaning depends on which branch produced it.
		detail["attachment_key"] = out.AttachmentKey
		detail["item_key"] = out.AttachmentKey
	}
	if out.Resumed {
		detail["resumed"] = true
		detail["note"] = "adopted a temporary parent left by an interrupted run instead of creating another"
	}
	if out.SaveReplyLost {
		detail["save_reply_lost"] = true
		detail["note"] = "the connector's save reply never arrived but the file was already committed; " +
			"this run adopted it instead of attaching a second copy"
	}
	if err != nil {
		// The detail map carries everything needed to finish by hand, including
		// the nonce: if this process died instead of returning, the operator can
		// still find the temporary parent by searching Zotero for the marker.
		if out.Nonce != "" {
			detail["temp_parent_marker"] = connectorTempParentPrefix + " " + out.Nonce
		}
		detail["message"] = err.Error()
		return "failed", detail, err
	}
	detail["parent_key"] = req.ParentKey
	if out.RaceLost {
		detail["note"] = "another run attached identical content first; this run added nothing"
		return "no_op", detail, nil
	}
	return "applied", detail, nil
}

func runConnectorReparent(ctx context.Context, cmd *cobra.Command, flags *rootFlags, webClient *client.Client, req storedUploadRequest) (connectorReparentResult, error) {
	var out connectorReparentResult

	// Confirm the destination before creating anything. Zotero rejects an
	// attachment whose parent is itself a child item, and discovering that after
	// the connector has already written the file would leave an orphan.
	targetTitle, err := verifyReparentTarget(ctx, webClient, req.ParentKey)
	if err != nil {
		return out, err
	}

	nonce, err := newConnectorReparentNonce()
	if err != nil {
		return out, err
	}
	out.Nonce = nonce

	// The title is borrowed from the target so Zotero names the stored file the
	// way a direct attach would. Identity lives in the abstractNote nonce, never
	// in this title — see the note at the top of this file.
	out.TempTitle = strings.TrimSpace(targetTitle)
	if out.TempTitle == "" {
		out.TempTitle = req.Title
	}

	// Resume before creating. A run killed between the connector save and the
	// move leaves a temporary parent holding the file. Without this, the retry
	// passes the target reconciliation (the file never reached the target) and
	// creates a SECOND temporary parent and a second copy of the bytes.
	//
	// Any run's marker is accepted here, not just this one's, because the point
	// is to adopt a previous run's orphan. It is still identified by the marker
	// and by the target key it names, never by title.
	adoptKey, adoptAttach, adoptErr := findResumableTemporaryParent(ctx, flags, req.ParentKey, req.MD5)
	if adoptErr != nil {
		return out, fmt.Errorf("checking for a resumable connector attachment: %w", adoptErr)
	}
	if adoptKey != "" {
		out.TempParentKey = adoptKey
		out.AttachmentKey = adoptAttach
		out.Resumed = true
		return finishConnectorReparent(ctx, cmd, webClient, req, out, "")
	}
	source, err := openVerifiedAttachmentSource(req)
	if err != nil {
		return out, fmt.Errorf("opening connector attachment before creating its temporary parent: %w", err)
	}
	defer func() { _ = source.Close() }()

	// A "document" is the most neutral regular item type: it carries no
	// bibliographic claim that could be mistaken for a real record if a run dies
	// before the trash step.
	tempItem := map[string]any{
		"itemType":     "document",
		"title":        out.TempTitle,
		"abstractNote": connectorTempParentMarker(nonce, req.ParentKey),
	}

	res, createErr := routeCreateItemVia(ctx, flags, "connector", webClient, tempItem, localFileURL(req.Path), false)
	if createErr != nil {
		// routeCreateItemVia can report a post-commit failure with an EMPTY
		// result, so the item may exist with nothing naming it. Look for the
		// nonce before giving up, or the operator is told a create failed while
		// a temporary parent sits in their library.
		if key, _, findErr := findTemporaryParentByMarker(ctx, flags, nonce, req.ParentKey); findErr == nil && key != "" {
			out.TempParentKey = key
		} else if res.WebKey != "" {
			out.TempParentKey = res.WebKey
		}
		if out.TempParentKey != "" || res.Session != "" {
			return out, fmt.Errorf("temporary parent %q was created but the route could not continue: %w",
				sanitizeForTerminal(out.TempTitle), createErr)
		}
		return out, fmt.Errorf("creating temporary parent via connector: %w", createErr)
	}
	if res.Via != "connector" {
		return out, fmt.Errorf("route resolved to %q, not the connector; --via connector is required for this route", res.Via)
	}

	// The create route's own recovery is title-based, so its key is a CANDIDATE
	// here, never an answer: with a borrowed title it can name the target. Trust
	// it only once it proves it carries this run's nonce.
	out.TempParentKey, err = resolveTemporaryParent(ctx, flags, res.WebKey, nonce, req.ParentKey)
	if err != nil {
		return out, err
	}

	// Put the file in the same session, so it becomes a child of the temporary
	// parent. This is the step the Web API cannot perform against an item that
	// already exists.
	var attachKey string
	if saveErr := saveConnectorAttachment(ctx, flags, res, req, source); saveErr != nil {
		// The connector protocol has no endpoint that closes a save session, so a
		// reply lost AFTER the desktop committed the child is indistinguishable
		// from a save that never landed. Reconcile before reporting: returning
		// here would strand a live attachment under a live temporary parent, and
		// the obvious retry would post the same bytes a second time.
		adopted, adoptErr := adoptLostConnectorSave(ctx, flags, out.TempParentKey)
		if adoptErr != nil {
			return out, errors.Join(saveErr, adoptErr)
		}
		if adopted == "" {
			// Nothing was committed, so the save error is the whole truth.
			return out, saveErr
		}
		out.SaveReplyLost = true
		attachKey = adopted
	} else {
		// The desktop's own database is authoritative the moment the connector
		// returns, and the temporary parent has exactly one attachment child by
		// construction — so this is deterministic, not a recency guess.
		var childErr error
		attachKey, childErr = soleAttachmentChild(ctx, flags, out.TempParentKey, out.TempTitle)
		if childErr != nil {
			return out, childErr
		}
	}
	out.AttachmentKey = attachKey

	return finishConnectorReparent(ctx, cmd, webClient, req, out, nonce)
}

// finishConnectorReparent runs the tail both entry paths share: confirm the
// attachment on the write plane, reconcile once more, move it, then trash the
// temporary parent.
//
// trashNonce pins the trash to one run's marker. It is empty when resuming a
// previous run's orphan, whose nonce differs but whose marker and target key
// have already been verified.
func finishConnectorReparent(ctx context.Context, cmd *cobra.Command, webClient *client.Client, req storedUploadRequest, out connectorReparentResult, trashNonce string) (connectorReparentResult, error) {
	// The PATCH needs the write plane's own version for the attachment, so wait
	// for the write plane to actually see it rather than assuming it does. The
	// same read confirms the attachment is still where we left it, so the
	// guarded write cannot move somebody else's file.
	version, err := awaitWritePlaneAttachment(ctx, webClient, out.AttachmentKey, out.TempParentKey)
	if err != nil {
		return out, err
	}

	// Last-moment reconciliation. Two runs, or a run racing the operator, can
	// both pass the reconciliation at the top of this route and then both move
	// an identical file onto the same item. The attachment version guard cannot
	// see that: it proves nobody changed THIS attachment, not that the target
	// gained a copy meanwhile.
	//
	// Re-checking here shrinks the window from the whole route to the moment
	// between this read and the PATCH. If another run won, abandon rather than
	// duplicate, and clean up both objects this run created.
	winner, checkErr := findAttachmentByMD5(webClient, req.ParentKey, req.MD5)
	if checkErr != nil {
		return out, fmt.Errorf("checking target %s for a competing attachment before moving %s: %w",
			req.ParentKey, out.AttachmentKey, checkErr)
	}
	if winner != "" {
		return abandonToWinner(ctx, cmd, webClient, req, out, trashNonce, winner)
	}

	if err := reparentAttachment(webClient, out.AttachmentKey, req.ParentKey, out.TempParentKey, req.MD5, version); err != nil {
		var raceLost *reparentRaceLostError
		if errors.As(err, &raceLost) {
			// A competing run finished inside the retry window, and its key came
			// back with the error rather than from a second lookup. The operator's
			// item has the content; ours is redundant, so clean up and report a
			// no-op rather than a conflict.
			return abandonToWinner(ctx, cmd, webClient, req, out, trashNonce, raceLost.winner)
		}
		return out, fmt.Errorf("attachment %s is stored under temporary parent %s but could not be moved to %s: %w",
			out.AttachmentKey, out.TempParentKey, req.ParentKey, err)
	}

	// Only now is the temporary parent safe to trash, and the reason is not a
	// cascade: Zotero does not propagate a trash to children. It is that a
	// failed re-parent must leave the operator a coherent pair to recover, an
	// untrashed parent holding the attachment, rather than a trashed parent with
	// a live child and the file apparently nowhere.
	orphaned, err := trashTemporaryParent(ctx, webClient, out.TempParentKey, trashNonce, req.ParentKey)
	// Report the observed children whatever the trash returned. On error the
	// helper names them without assuming that the guarded PATCH committed or
	// suggesting deletion; on confirmed trash it gives both recovery routes.
	warnOrphanedChildren(cmd, out.TempParentKey, orphaned, err)
	if err != nil {
		// The real work succeeded. Report the litter without failing the run,
		// since re-running would create a second temporary parent rather than
		// clean this one up.
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: attachment moved to %s, but the temporary parent %s could not be trashed: %v\n"+
				"         delete it by hand: zotio items delete %s --yes\n",
			req.ParentKey, out.TempParentKey, err, out.TempParentKey)
		return out, nil
	}
	out.TempTrashed = true
	return out, nil
}

// abandonToWinner cleans up after losing the race to another run and reports a
// no-op. It is reached from two places: the reconciliation just before the PATCH,
// and a retry that discovers the target gained the content mid-window.
//
// Order is not incidental. Zotero's server does not cascade a trash to children,
// so this run's own attachment must be trashed FIRST; trashing only the
// temporary parent leaves the copy live beneath it, unreachable from the trash
// and still holding its stored bytes.
//
// The returned error distinguishes two outcomes that must never be conflated.
// A CLEANUP failure returns nil: the operator's item already holds the content
// they asked for, so the run achieved what was asked and only litter remains,
// which each branch names along with the command that removes it. An ABORTED
// abandon returns an error: the target does not hold the content, this run did
// not put it there, and nothing was trashed - reporting that as applied would
// tell a scripted caller the file reached the item when it did not.
func abandonToWinner(ctx context.Context, cmd *cobra.Command, webClient *client.Client, req storedUploadRequest, out connectorReparentResult, trashNonce, winner string) (connectorReparentResult, error) {
	// Capture our own key before reporting the winner's: the objects to clean up
	// are ours, and out.AttachmentKey is about to name someone else's.
	ours := out.AttachmentKey
	if winner == "" {
		// Never reached with a confirmed winner absent, and refusing rather than
		// trusting the caller is the difference between litter and data loss:
		// trashing our copy when the target holds none leaves the operator's item
		// with nothing at all. Keep both objects and fail.
		return out, fmt.Errorf("abandoning to another run was requested without naming the winning attachment, "+
			"so nothing was trashed and %s was not attached to %s; "+
			"attachment %s remains under temporary parent %s",
			req.Path, req.ParentKey, ours, out.TempParentKey)
	}
	out.AttachmentKey = winner
	out.RaceLost = true
	// Confirm the winner is STILL live, still on the target, and still carries
	// this content, as late as possible before destroying our own copy. Zotero
	// offers no multi-object transaction, so a residual window remains: the
	// winner could be trashed or moved between this read and the PATCH below.
	// Narrowing it to a single round trip is the best available, and keeping both
	// objects when the check fails turns the worst case into litter the operator
	// can see rather than content that quietly disappeared.
	if live, liveErr := attachmentIsLiveWithHash(webClient, winner, req.ParentKey, req.MD5); liveErr != nil || !live {
		// The abandon is off. Restore this run's own key so the envelope names
		// the object that actually exists, and fail: the target does not hold
		// the content, so this is not a no-op and certainly not applied.
		out.RaceLost = false
		out.AttachmentKey = ours
		detail := "it is no longer a live child of that item carrying this content"
		if liveErr != nil {
			detail = liveErr.Error()
		}
		return out, fmt.Errorf("another run appeared to attach this file to %s first, but attachment %s "+
			"could not be confirmed on it (%s), so nothing was trashed; "+
			"this run's copy %s remains under temporary parent %s - "+
			"attach it by hand or re-run once the item settles",
			req.ParentKey, winner, detail, ours, out.TempParentKey)
	}
	if trashErr := trashRedundantAttachment(ctx, webClient, ours, out.TempParentKey, req.MD5); trashErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: another run attached this file first; our redundant copy %s could not be trashed: %v\n"+
				"         delete it by hand: zotio items delete %s --yes\n",
			ours, trashErr, ours)
		// The temporary parent deliberately stays: trashTemporaryParent refuses
		// while a live child remains, and reporting that refusal on top of the
		// warning above would only repeat it.
		return out, nil
	}
	orphaned, trashErr := trashTemporaryParent(ctx, webClient, out.TempParentKey, trashNonce, req.ParentKey)
	// Report the observed children whatever the trash returned, with wording
	// that preserves the unknown parent state on an error.
	warnOrphanedChildren(cmd, out.TempParentKey, orphaned, trashErr)
	if trashErr == nil {
		out.TempTrashed = true
	} else {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: another run attached this file first; the temporary parent %s could not be trashed: %v\n"+
				"         delete it by hand: zotio items delete %s --yes\n",
			out.TempParentKey, trashErr, out.TempParentKey)
	}
	return out, nil
}

// warnOrphanedChildren reports live non-attachment children observed before a
// temporary-parent trash. A nil trashErr confirms the parent is trashed. A
// non-nil trashErr does not: the write might have failed before dispatch or
// committed before its response was lost, so the warning must not claim either
// state or suggest deleting a child that may still be normally reachable.
func warnOrphanedChildren(cmd *cobra.Command, tempParentKey string, orphaned []string, trashErr error) {
	if len(orphaned) == 0 {
		return
	}
	if trashErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: cleanup observed %d live non-attachment child item(s) beneath temporary parent %s (%s), "+
				"but its trash request returned an error and did not confirm the parent's state\n"+
				"         inspect the parent and each observed child before deleting anything\n",
			len(orphaned), tempParentKey, strings.Join(orphaned, ", "))
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"warning: temporary parent %s was trashed after cleanup observed %d live non-attachment child item(s) (%s)\n"+
			"         Zotero does not cascade a trash to children; restore the parent with: zotio items restore %s --yes\n"+
			"         inspect each child first; if it still belongs to this temporary parent and should be removed:\n",
		tempParentKey, len(orphaned), strings.Join(orphaned, ", "), tempParentKey)
	// One command per key. `items delete` takes a single <itemKey> and reads only
	// args[0], so a joined list would delete the first and silently ignore the
	// rest while appearing to name them all.
	for _, key := range orphaned {
		fmt.Fprintf(cmd.ErrOrStderr(), "           zotio items delete %s --yes\n", key)
	}
}

// attachmentIsLiveWithHash reports whether key names a live attachment whose
// registered content hash matches. It is the last check before this route
// destroys its own copy, so an unreadable answer is reported as an error rather
// than as either outcome.
func attachmentIsLiveWithHash(c *client.Client, key, wantParent, md5hex string) (bool, error) {
	if key == "" {
		return false, fmt.Errorf("no attachment key to confirm")
	}
	body, version, err := c.GetWithVersion("/items/"+key, nil)
	if err != nil {
		return false, err
	}
	if version <= 0 {
		return false, fmt.Errorf("attachment %s was read without a version", key)
	}
	if itemAlreadyTrashed(body) {
		return false, nil
	}
	var row struct {
		Data struct {
			ParentItem string `json:"parentItem"`
			MD5        string `json:"md5"`
		} `json:"data"`
	}
	if uerr := json.Unmarshal(body, &row); uerr != nil {
		return false, fmt.Errorf("parsing attachment %s: %w", key, uerr)
	}
	// Parentage must be re-asserted, not assumed. findAttachmentByMD5 located
	// this key by reading the TARGET's children, so it was on the target then.
	// Between that read and this one another actor can re-parent it elsewhere
	// while leaving it live with the same hash. Checking only deleted and md5
	// would pass such a winner, and the caller would then destroy this run's
	// copy on the strength of content that had already left the target.
	if wantParent != "" && !strings.EqualFold(row.Data.ParentItem, wantParent) {
		return false, nil
	}
	// An unregistered hash is not a mismatch: the desktop writes md5 a moment
	// after the file is stored, exactly as it does for this run's own copy.
	if md5hex != "" && row.Data.MD5 != "" && !strings.EqualFold(row.Data.MD5, md5hex) {
		return false, nil
	}
	return true, nil
}

// resolveTemporaryParent settles which key is this run's temporary parent.
//
// A candidate from the create route is accepted only after it proves it carries
// this run's nonce. Anything else is discarded and the marker lookup decides,
// because accepting an unverified key here is what let an earlier version
// re-parent inside the target and then trash it.
func resolveTemporaryParent(ctx context.Context, flags *rootFlags, candidate, nonce, targetKey string) (string, error) {
	if candidate != "" && candidate != targetKey {
		if ok, err := itemCarriesNonce(ctx, flags, candidate, targetKey, nonce); err == nil && ok {
			return candidate, nil
		}
	}
	return awaitTempParentKey(ctx, flags, nonce, targetKey)
}

// itemCarriesNonce reports whether an item is this run's temporary parent.
func itemCarriesNonce(ctx context.Context, flags *rootFlags, key, targetKey, nonce string) (bool, error) {
	c, err := localClientForRoute(ctx, flags)
	if err != nil {
		return false, err
	}
	body, _, err := c.GetWithVersion("/items/"+key, nil)
	if err != nil {
		return false, err
	}
	return bodyIsTemporaryParentFor(body, targetKey, nonce), nil
}

// bodyIsTemporaryParentFor reports whether an item is a temporary parent this
// route created for targetKey. A nonce pins it to one run; an empty nonce
// accepts any run's, which is what resuming a crashed run needs.
//
// Identity is never the title. It is the marker string zotio authors, the
// neutral "document" type, and the target key the marker names — none of which a
// real bibliographic record carries.
func bodyIsTemporaryParentFor(body json.RawMessage, targetKey, nonce string) bool {
	var envelope struct {
		Data struct {
			ItemType     string `json:"itemType"`
			AbstractNote string `json:"abstractNote"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return false
	}
	return markerIdentifies(envelope.Data.ItemType, envelope.Data.AbstractNote, targetKey, nonce)
}

func markerIdentifies(itemType, abstract, targetKey, nonce string) bool {
	if itemType != "document" || !strings.Contains(abstract, connectorTempParentPrefix) {
		return false
	}
	if !strings.Contains(abstract, "for item "+targetKey) {
		return false
	}
	return nonce == "" || strings.Contains(abstract, nonce)
}

// awaitTempParentKey polls for the item carrying this run's nonce.
func awaitTempParentKey(ctx context.Context, flags *rootFlags, nonce, targetKey string) (string, error) {
	deadline := pollDeadline(ctx)
	for {
		key, matched, err := findTemporaryParentByMarker(ctx, flags, nonce, targetKey)
		switch {
		case err != nil:
			return "", err
		case key != "":
			return key, nil
		case matched > 1:
			return "", fmt.Errorf("%d items carry this run's marker; refusing to guess which one to use", matched)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no item carrying the marker %q appeared within %s; "+
				"search Zotero for %q and delete any item that has no file",
				nonce, connectorReparentVisibilityTimeout, connectorTempParentPrefix)
		}
		if err := sleepWithContext(ctx, connectorReparentPollInterval); err != nil {
			return "", err
		}
	}
}

// findTemporaryParentByMarker looks for a recently added item whose abstractNote
// carries this run's nonce.
//
// The target key is excluded explicitly. That is belt and braces — the target
// cannot carry a nonce generated moments ago — but this is the input to the only
// destructive write in the route, so it is checked rather than reasoned about.
func findTemporaryParentByMarker(ctx context.Context, flags *rootFlags, nonce, targetKey string) (string, int, error) {
	c, err := localClientForRoute(ctx, flags)
	if err != nil {
		return "", 0, err
	}
	// The item was created seconds ago; a cached list would not contain it.
	c.NoCache = true

	var found string
	matched := 0
	for start := 0; start < 500; start += connectorChildrenPageSize {
		data, err := c.Get("/items/top", map[string]string{
			"sort":      "dateAdded",
			"direction": "desc",
			"limit":     strconv.Itoa(connectorChildrenPageSize),
			"start":     strconv.Itoa(start),
		})
		if err != nil {
			return "", 0, fmt.Errorf("looking for the temporary parent: %w", err)
		}
		var rows []struct {
			Key  string `json:"key"`
			Data struct {
				ItemType     string `json:"itemType"`
				AbstractNote string `json:"abstractNote"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &rows); err != nil {
			return "", 0, fmt.Errorf("decoding recent items: %w", err)
		}
		for _, row := range rows {
			if row.Key == targetKey {
				continue
			}
			if !markerIdentifies(row.Data.ItemType, row.Data.AbstractNote, targetKey, nonce) {
				continue
			}
			if !zoteroItemKeyRE.MatchString(row.Key) {
				continue
			}
			matched++
			found = row.Key
		}
		if len(rows) < connectorChildrenPageSize {
			break
		}
	}
	// Reporting a guessed key is worse than reporting none.
	if matched != 1 {
		return "", matched, nil
	}
	return found, matched, nil
}

// localClientForRoute returns a read client bound to the route's deadline, so a
// hung desktop cannot keep a polling loop alive past the route's budget. A
// client carries its own base context, so binding it is what makes the deadline
// bound HTTP rather than only the sleeps between polls.
func localClientForRoute(ctx context.Context, flags *rootFlags) (*client.Client, error) {
	c, err := flags.newClient()
	if err != nil {
		return nil, err
	}
	c.SetContext(ctx)
	return c, nil
}

// verifyReparentTarget checks the destination can legally hold an attachment and
// returns its title. Zotero requires a regular item: an attachment cannot parent
// another, and only embedded-image attachments may have child parents.
//
// A 404 here is polled, not fatal. The obvious caller creates an item and
// attaches its PDF straight afterwards, and an item created through the desktop
// connector takes 15-20s to reach the write plane — so the first read of a
// brand-new target legitimately misses. Failing immediately made
// create-then-attach unusable, which is exactly the shape a downstream consumer
// uses; found by smoke-testing the release binary against a real library.
func verifyReparentTarget(ctx context.Context, c *client.Client, parentKey string) (string, error) {
	var body json.RawMessage
	deadline := pollDeadline(ctx)
	for {
		var err error
		body, _, err = c.GetWithVersion("/items/"+parentKey, nil)
		if err == nil {
			break
		}
		if !isNotFoundOrLag(err) {
			return "", fmt.Errorf("reading target item %s: %w", parentKey, err)
		}
		if time.Now().After(deadline) {
			return "", notFoundErr(fmt.Errorf("target item %s did not appear on the write plane within %s; "+
				"if it was just created, it is still propagating", parentKey, connectorReparentVisibilityTimeout))
		}
		if err := sleepWithContext(ctx, connectorReparentPollInterval); err != nil {
			return "", err
		}
	}
	var envelope struct {
		Data struct {
			ItemType   string `json:"itemType"`
			ParentItem string `json:"parentItem"`
			Title      string `json:"title"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", fmt.Errorf("decoding target item %s: %w", parentKey, err)
	}
	switch {
	case envelope.Data.ItemType == "attachment":
		return "", usageErr(fmt.Errorf("target %s is an attachment; attachments cannot hold child attachments", parentKey))
	case envelope.Data.ItemType == "note":
		return "", usageErr(fmt.Errorf("target %s is a note; notes cannot hold child attachments", parentKey))
	case envelope.Data.ParentItem != "":
		return "", usageErr(fmt.Errorf("target %s is itself a child of %s; attach to the top-level item instead",
			parentKey, envelope.Data.ParentItem))
	}
	return envelope.Data.Title, nil
}

// saveConnectorAttachment streams bytes into the connector session that
// created the temporary parent. The connector takes ownership of source.
func saveConnectorAttachment(ctx context.Context, flags *rootFlags, res itemCreateResult, req storedUploadRequest, source io.ReadCloser) error {
	conn, err := connectorForCreate(flags)
	if err != nil {
		return fmt.Errorf("temporary parent %s was created but the connector is unavailable: %w", res.WebKey, err)
	}
	// Zotero's importFromNetworkStream hard-rejects an empty url with an opaque
	// HTTP 500 AFTER the parent exists, so a local file always sends its own URI
	// — the same provenance import pdf records for standalone attachments.
	if err := conn.SaveAttachment(ctx, res.Session, res.ConnKey, req.Title, localFileURL(req.Path), req.ContentType, source, req.Size); err != nil {
		return fmt.Errorf("temporary parent %s was created but its file did not attach: %w", res.WebKey, err)
	}
	return nil
}

// soleAttachmentChild returns the temporary parent's one attachment child,
// reading through the configured base. That is Zotero's own database for this
// route, because connectorForCreate refuses a non-local base
// (write_routing.go), so the connector's write is visible immediately.
//
// It refuses when the count is not exactly one. More than one means something
// else wrote into this item, and picking a candidate could attach the wrong file
// to the operator's paper — the same reason confirmConnectorCreate refuses to
// guess between multiple matches.
func soleAttachmentChild(ctx context.Context, flags *rootFlags, tempParentKey, tempTitle string) (string, error) {
	if strings.TrimSpace(tempParentKey) == "" {
		return "", fmt.Errorf("the connector did not report a key for temporary parent %q; "+
			"search Zotero for %q and delete any item that has no file",
			sanitizeForTerminal(tempTitle), connectorTempParentPrefix)
	}
	local, err := localClientForRoute(ctx, flags)
	if err != nil {
		return "", fmt.Errorf("reading children of temporary parent %s: %w", tempParentKey, err)
	}
	deadline := pollDeadline(ctx)
	for {
		keys, err := attachmentChildKeys(local, tempParentKey)
		if err == nil {
			switch len(keys) {
			case 1:
				return keys[0], nil
			case 0:
				// Keep waiting: the connector's own file import is asynchronous.
			default:
				return "", fmt.Errorf("temporary parent %s has %d attachment children, expected exactly one; "+
					"refusing to guess which file to move — inspect it in Zotero",
					tempParentKey, len(keys))
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return "", fmt.Errorf("reading children of temporary parent %s: %w", tempParentKey, err)
			}
			return "", fmt.Errorf("the file did not appear under temporary parent %s within %s; "+
				"inspect it in Zotero and delete it if it holds no file",
				tempParentKey, connectorReparentVisibilityTimeout)
		}
		if err := sleepWithContext(ctx, connectorReparentPollInterval); err != nil {
			return "", err
		}
	}
}

// connectorSaveReconcileAttempts bounds the reconcile after a lost save reply.
// The desktop commits the child before it answers, so if the save did land the
// child is already there or arrives within a poll or two. A full visibility
// window here would instead make every genuinely failed save wait it out before
// reporting the real cause.
const connectorSaveReconcileAttempts = 3

// adoptLostConnectorSave looks for the child that a lost save reply may have
// left behind. It returns that attachment's key when the desktop committed
// exactly one live child, "" when nothing was committed and the caller should
// report its original error, and an error only when the children cannot be
// listed or when more than one of them makes adoption unsafe.
//
// It matches on parentage alone — this run's own nonce-marked temporary parent,
// which no other run can address — deliberately NOT on md5. The desktop
// registers the hash a moment AFTER creating the attachment (measured live on
// 2026-08-30, see dev/field-report-2026-08-30-connector-reparent-live.md), so a
// hash lookup inside this window reports absence for a file that does exist,
// which is the false negative that would duplicate the bytes on retry.
//
// A trashed child is not adopted: liveAttachmentChildren excludes it, so an
// earlier run's cleaned-up litter cannot be mistaken for this run's file.
func adoptLostConnectorSave(ctx context.Context, flags *rootFlags, tempParentKey string) (string, error) {
	if strings.TrimSpace(tempParentKey) == "" {
		// Without a parent key there is nothing addressable to reconcile against.
		return "", nil
	}
	local, err := localClientForRoute(ctx, flags)
	if err != nil {
		return "", fmt.Errorf("reading children of temporary parent %s: %w", tempParentKey, err)
	}
	for attempt := range connectorSaveReconcileAttempts {
		if attempt > 0 {
			if sleepErr := sleepWithContext(ctx, connectorReparentPollInterval); sleepErr != nil {
				return "", sleepErr
			}
		}
		live, listErr := liveAttachmentChildren(local, tempParentKey)
		if listErr != nil {
			return "", fmt.Errorf("reading children of temporary parent %s: %w", tempParentKey, listErr)
		}
		switch len(live) {
		case 0:
			// Keep waiting: the commit may still be propagating.
		case 1:
			return live[0], nil
		default:
			return "", fmt.Errorf("temporary parent %s has %d live attachment children after a lost save reply, "+
				"expected at most one; refusing to guess which file to move — inspect it in Zotero",
				tempParentKey, len(live))
		}
	}
	return "", nil
}

// attachmentChildKeys lists a parent's attachment children, paged.
func attachmentChildKeys(c *client.Client, parentKey string) ([]string, error) {
	rows, err := attachmentChildRows(c, parentKey)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Data.ItemType == "attachment" && zoteroItemKeyRE.MatchString(row.Key) {
			keys = append(keys, row.Key)
		}
	}
	return keys, nil
}

type childRow struct {
	Key  string `json:"key"`
	Data struct {
		ItemType string `json:"itemType"`
		LinkMode string `json:"linkMode"`
		Filename string `json:"filename"`
		Path     string `json:"path"`
		MD5      string `json:"md5"`
		URL      string `json:"url"`
		Note     string `json:"note"`
		Deleted  int    `json:"deleted"`
	} `json:"data"`
}

// attachmentChildRows reads every attachment child, following pages. Zotero's
// default page is 25, so an unpaged read truncates silently.
func attachmentChildRows(c *client.Client, parentKey string) ([]childRow, error) {
	return childRows(c, parentKey, "attachment")
}

// allChildRows reads every child of parentKey regardless of item type. Zotero
// permits notes beneath a regular document, and its trash does not cascade, so
// the cleanup path needs to see children the attachment-filtered read hides.
func allChildRows(c *client.Client, parentKey string) ([]childRow, error) {
	return childRows(c, parentKey, "")
}

// childRows pages one children endpoint. An empty itemType omits the filter.
func childRows(c *client.Client, parentKey, itemType string) ([]childRow, error) {
	var all []childRow
	for start := 0; ; start += connectorChildrenPageSize {
		params := map[string]string{
			"limit": strconv.Itoa(connectorChildrenPageSize),
			"start": strconv.Itoa(start),
		}
		if itemType != "" {
			params["itemType"] = itemType
		}
		data, _, err := c.GetWithVersion("/items/"+parentKey+"/children", params)
		if err != nil {
			var respErr *client.APIError
			if errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound {
				return nil, notFoundErr(fmt.Errorf("parent item %s not found", parentKey))
			}
			return nil, fmt.Errorf("listing children of %s: %w", parentKey, err)
		}
		var page []childRow
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, fmt.Errorf("parsing children of %s: %w", parentKey, err)
		}
		all = append(all, page...)
		if len(page) < connectorChildrenPageSize {
			return all, nil
		}
	}
}

// awaitWritePlaneAttachment waits until the write plane can see the attachment,
// confirms it is still a child of the temporary parent, and returns its version
// there. A 404 in this window is propagation lag, not a missing item.
//
// The parent check matters: the version guard proves nobody else changed THIS
// item, but it cannot prove the key names the item this route created.
func awaitWritePlaneAttachment(ctx context.Context, c *client.Client, key, wantParent string) (int, error) {
	deadline := pollDeadline(ctx)
	for {
		body, version, err := c.GetWithVersion("/items/"+key, nil)
		switch {
		case err == nil && version > 0:
			var envelope struct {
				Data struct {
					ItemType   string `json:"itemType"`
					ParentItem string `json:"parentItem"`
				} `json:"data"`
			}
			if err := json.Unmarshal(body, &envelope); err != nil {
				return 0, fmt.Errorf("decoding attachment %s: %w", key, err)
			}
			if envelope.Data.ItemType != "attachment" {
				return 0, fmt.Errorf("item %s is a %q, not an attachment; refusing to move it",
					key, envelope.Data.ItemType)
			}
			if envelope.Data.ParentItem != wantParent {
				return 0, fmt.Errorf("attachment %s is a child of %s, not of the temporary parent %s; "+
					"refusing to move a file this run did not create",
					key, envelope.Data.ParentItem, wantParent)
			}
			return version, nil
		case err == nil:
			// Read succeeded with no version header. That is not propagation
			// lag, and calling it lag would misdirect the operator.
			return 0, fmt.Errorf("attachment %s was read from the write plane without a version, "+
				"so the move cannot be guarded", key)
		case !isNotFoundOrLag(err):
			return 0, fmt.Errorf("reading attachment %s on the write plane: %w", key, err)
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("attachment %s did not reach the write plane within %s, so it cannot be moved yet; "+
				"the file is stored and a retry will resume", key, connectorReparentVisibilityTimeout)
		}
		if err := sleepWithContext(ctx, connectorReparentPollInterval); err != nil {
			return 0, err
		}
	}
}

func isNotFoundOrLag(err error) bool {
	var respErr *client.APIError
	if errors.As(err, &respErr) {
		return respErr.StatusCode == http.StatusNotFound
	}
	return false
}

// isPreconditionFailed reports whether Zotero rejected a guarded write because
// the object moved on under us (HTTP 412).
func isPreconditionFailed(err error) bool {
	var respErr *client.APIError
	if errors.As(err, &respErr) {
		return respErr.StatusCode == http.StatusPreconditionFailed
	}
	return false
}

// reparentRaceLostError reports that the target gained this content while the
// route was retrying its guarded PATCH, and carries the winning attachment's
// key. It is not a failure: the operator's item ends up with exactly the
// attachment they asked for, just not this run's copy, so the caller cleans up
// its own objects and reports a no-op.
//
// The key travels with the error deliberately. Having the caller look the winner
// up again would reopen the very window this detects, and a winner that vanished
// in between would leave the caller trashing this run's copy with nothing on the
// target to replace it.
type reparentRaceLostError struct{ winner string }

func (e *reparentRaceLostError) Error() string {
	return fmt.Sprintf("another run attached identical content to the target first, as attachment %s", e.winner)
}

// attachmentParentVersion reads an attachment's current parent, trashed state,
// and version in one request. The 412 retry path needs to tell four states
// apart: already moved (the PATCH landed and its response was lost), still under
// the temporary parent (safe to retry), sitting somewhere else entirely
// (abandon), or trashed (abandon — another actor's cleanup deliberately removed
// it, and replaying the move would resurrect it onto the target).
func attachmentParentVersion(c *client.Client, key string) (parent string, version int, trashed bool, err error) {
	body, version, err := c.GetWithVersion("/items/"+key, nil)
	if err != nil {
		return "", 0, false, err
	}
	if version <= 0 {
		return "", 0, false, fmt.Errorf("attachment %s was read without a version", key)
	}
	var envelope struct {
		Data struct {
			ParentItem string `json:"parentItem"`
			Deleted    int    `json:"deleted"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", 0, false, fmt.Errorf("decoding attachment %s: %w", key, err)
	}
	return envelope.Data.ParentItem, version, envelope.Data.Deleted != 0, nil
}

// connectorReparentConflictRetries bounds the re-reads after a 412 on the
// re-parent PATCH. The desktop's file registration is one bump, not a stream,
// so a couple of retries covers it; more would mean something else is writing
// and the route should stop rather than fight it.
const connectorReparentConflictRetries = 2

// reparentAttachment moves an existing attachment onto a different parent.
//
// This is the whole point of the route, and it is one guarded PATCH. Zotero
// accepts it for an already-stored, already-synced attachment, and it relocates
// no bytes: md5 and mtime are unchanged and the storage directory keeps the
// attachment's own key.
//
// The 412 retry is not defensive padding; it is the failure this route hits in
// practice. The connector creates the attachment, and moments later the DESKTOP
// registers the stored file by writing md5 and mtime, which bumps the version
// the route resolved just before its PATCH. Measured live on 2026-08-30:
// "expected 15136, found 15137", where 15137 was the desktop's own registration
// of the very file this run had created, and a PATCH replayed against the fresh
// version returned 204. See
// dev/field-report-2026-08-30-connector-reparent-live.md.
//
// Retrying is safe here precisely because it is checked, not blind. Before any
// replay the route re-establishes all three facts the first attempt relied on:
// the attachment is still a child of the temporary parent this run created, it
// has not been trashed by another actor's cleanup, and the target has not gained
// a copy of this content meanwhile. The last one matters because the pre-PATCH
// reconciliation happens once, before the first attempt, and a competing run can
// finish inside the retry window - replaying without re-checking would put a
// second identical attachment on the operator's item.
//
// It returns errReparentRaceLost when the target gained the content, so the
// caller runs its race-loss cleanup rather than reporting a conflict.
func reparentAttachment(c *client.Client, attachKey, newParentKey, tempParentKey, md5hex string, version int) error {
	if version <= 0 {
		return fmt.Errorf("refusing to move attachment %s without a version to guard the write", attachKey)
	}
	if !zoteroItemKeyRE.MatchString(attachKey) || !zoteroItemKeyRE.MatchString(newParentKey) {
		return fmt.Errorf("refusing to move %q onto %q: not both valid item keys", attachKey, newParentKey)
	}
	for attempt := 0; ; attempt++ {
		_, _, err := patchWithVersionGuard(c, "/items/"+attachKey, map[string]any{"parentItem": newParentKey}, version)
		if err == nil {
			return nil
		}
		if attempt >= connectorReparentConflictRetries || !isPreconditionFailed(err) {
			return err
		}
		// Every branch below returns the ORIGINAL 412 rather than a read error,
		// so the operator sees the conflict that actually stopped the route.
		currentParent, fresh, trashed, readErr := attachmentParentVersion(c, attachKey)
		switch {
		case readErr != nil:
			return err
		case trashed:
			// Checked BEFORE the on-target case, deliberately. An attachment that
			// reached the target and was then trashed by another actor must not be
			// reported as a success: the operator's item does not have a live copy,
			// and only the temporary parent gets cleaned up afterwards.
			return err
		case currentParent == newParentKey:
			// The PATCH did land; only its response was lost.
			return nil
		case tempParentKey == "" || currentParent != tempParentKey:
			// Someone else owns it now. Do not fight for it.
			return err
		case fresh <= version:
			// A 412 with no version movement is not the race this handles, and
			// retrying the same precondition would spin.
			return err
		}
		// The target is re-checked on every replay, not once per route. A
		// competing run can win inside this window, and a blind replay would
		// duplicate its work onto the operator's item.
		//
		// A re-check that FAILS is not permission to replay. Without a completed
		// read there is no proof the target is still winner-free, so this returns
		// the original conflict rather than risking a duplicate.
		winner, checkErr := findAttachmentByMD5(c, newParentKey, md5hex)
		if checkErr != nil {
			return err
		}
		if winner != "" && winner != attachKey {
			// The winner's key travels WITH the error. Re-reading it in the
			// caller would reopen the window: a winner that vanished in between
			// would leave the caller trashing our copy with nothing on the target.
			return &reparentRaceLostError{winner: winner}
		}
		version = fresh
	}
}

// trashTemporaryParent trashes the throwaway item, reversibly, and ONLY after
// confirming the key names an item this run created.
//
// The identity check is the point. The version precondition proves nobody else
// changed the item; it cannot prove the item is the right one. Without this
// check, a mis-recovered key meant a PATCH of deleted:1 onto the operator's real
// paper, reported as "applied".
//
// It is never permanently deleted: a permanent delete destroys child attachments
// outright, and if the re-parent silently failed that would take the operator's
// file with it.
//
// It also refuses while any live child remains. Zotero's server does not cascade
// a trash to children, so trashing a parent that still holds one leaves that
// child live and unreachable from the trash, still holding its stored bytes.
// Measured on 2026-08-30: three probe runs left three such orphans against a
// library that was otherwise byte-identical, and only a revert detector noticed.
// Every caller must therefore move or trash the child FIRST, and this refusal is
// what keeps a future caller from forgetting.
func trashTemporaryParent(ctx context.Context, c *client.Client, key, nonce, targetKey string) ([]string, error) {
	if key == "" || key == targetKey {
		return nil, fmt.Errorf("refusing to trash %q: it is the target item, not a temporary parent", key)
	}
	if !zoteroItemKeyRE.MatchString(key) {
		return nil, fmt.Errorf("refusing to trash %q: not a valid item key", key)
	}
	deadline := pollDeadline(ctx)
	for {
		body, version, err := c.GetWithVersion("/items/"+key, nil)
		switch {
		case err == nil && version > 0:
			if !bodyIsTemporaryParentFor(body, targetKey, nonce) {
				return nil, fmt.Errorf("refusing to trash %s: it does not carry this run's marker, "+
					"so it is not the temporary parent this route created", key)
			}
			// Checked BEFORE the already-trashed short-circuit, deliberately. A
			// parent another actor trashed while a child still hung beneath it is
			// exactly the orphan this guard exists to prevent, and returning "done"
			// there would report success over the broken state.
			live, liveErr := liveAttachmentChildren(c, key)
			if liveErr != nil {
				return nil, fmt.Errorf("cannot confirm temporary parent %s holds no live child, so refusing to trash it: %w", key, liveErr)
			}
			if len(live) > 0 {
				return nil, fmt.Errorf("refusing to trash temporary parent %s: it still holds %d live attachment(s) (%s), "+
					"which Zotero would leave live and orphaned because it does not cascade a trash to children; "+
					"move or trash them first",
					key, len(live), strings.Join(live, ", "))
			}
			// A live child of any OTHER type is reported but does not refuse.
			// The refusal above exists to protect STORED BYTES: an attachment
			// left beneath a trashed parent strands a file the operator may be
			// paying WebDAV to host. A note has no bytes, so blocking on one
			// would trade a theoretical orphan for guaranteed litter on every
			// run - this route must always be able to clean up after itself.
			// Naming it is what keeps the orphan from being silent.
			orphaned, orphanErr := liveNonAttachmentChildren(c, key)
			if orphanErr != nil {
				return nil, fmt.Errorf("cannot confirm what temporary parent %s still holds, so refusing to trash it: %w", key, orphanErr)
			}
			if itemAlreadyTrashed(body) {
				return orphaned, nil
			}
			_, _, writeErr := patchWithVersionGuard(c, "/items/"+key, map[string]any{"deleted": 1}, version)
			return orphaned, writeErr
		case err == nil:
			return nil, fmt.Errorf("item %s was read from the write plane without a version, so the trash cannot be guarded", key)
		case !isNotFoundOrLag(err):
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("temporary parent %s never became visible on the write plane", key)
		}
		if err := sleepWithContext(ctx, connectorReparentPollInterval); err != nil {
			return nil, err
		}
	}
}

// liveAttachmentChildren returns the keys of attachment children of parentKey
// that are not trashed, in the order Zotero reports them.
//
// A trashed child is excluded deliberately: it is already reversible and
// reachable from the trash, so it does not block trashing its parent.
//
// It FAILS CLOSED on a read it cannot complete, including a 404. That is the
// opposite of findAttachmentByMD5, which maps a 404 to "no match" because a
// target that has not propagated cannot hold this run's hash, and guessing wrong
// there merely proceeds to create. Here a wrong guess orphans the operator's
// file: the caller would read "no live children" as permission to trash the
// parent, and a child still propagating would surface afterwards, live and
// beneath a trashed parent. Absence of evidence is not evidence of absence when
// the next step is destructive.
func liveAttachmentChildren(c *client.Client, parentKey string) ([]string, error) {
	rows, err := attachmentChildRows(c, parentKey)
	if err != nil {
		return nil, err
	}
	var live []string
	for _, row := range rows {
		if row.Data.ItemType == "attachment" && row.Data.Deleted == 0 {
			live = append(live, row.Key)
		}
	}
	return live, nil
}

// liveNonAttachmentChildren returns the keys of live children of parentKey that
// are NOT attachments - in practice notes, which Zotero permits beneath a
// regular document.
//
// These do not block trashing the parent. The refusal in trashTemporaryParent
// guards stored bytes: an attachment stranded under a trashed parent leaves a
// file the operator may be paying WebDAV to host. A note carries no bytes, and
// refusing on one would mean any note a plugin or the operator adds to this
// route's temporary parent permanently blocks the route's own cleanup, trading a
// rare orphan for certain litter on every run. So they are named, not obeyed:
// the caller reports them, and nothing is orphaned silently.
func liveNonAttachmentChildren(c *client.Client, parentKey string) ([]string, error) {
	rows, err := allChildRows(c, parentKey)
	if err != nil {
		return nil, err
	}
	var live []string
	for _, row := range rows {
		if row.Data.ItemType != "attachment" && row.Data.Deleted == 0 {
			live = append(live, row.Key)
		}
	}
	return live, nil
}

// trashRedundantAttachment trashes the copy this run uploaded after another run
// won the race, reversibly, and ONLY after proving the key names that copy.
//
// It exists because Zotero's server does not cascade a trash to children.
// Trashing the temporary parent alone leaves this attachment LIVE beneath a
// trashed parent, still holding its stored bytes: measured on 2026-08-30, where
// three probe runs left three such orphans against a library that was otherwise
// byte-identical. Absence of a cascade is what makes the re-parent safe in the
// first place (see reparentAttachment), so it is the same fact read from the
// other side, and it obliges this route to clean up both objects by hand.
//
// The identity check is two-part and both halves matter. The attachment must
// still be a child of THIS run's temporary parent, which carries a per-run
// nonce, and it must carry the content hash this run uploaded. A key that
// escaped to any other parent belongs to someone else's route now.
//
// A 404 is treated as propagation lag, NOT as "already gone", and is polled out
// to the same visibility deadline the route uses elsewhere. The connector
// creates the attachment on the desktop and the write plane sees it seconds
// later, so a cleanup that read 404 as absence would report nothing to do,
// release the temporary parent for trashing, and let the attachment surface
// afterwards as a live orphan beneath a trashed parent.
func trashRedundantAttachment(ctx context.Context, c *client.Client, attachKey, tempParentKey, md5hex string) error {
	if attachKey == "" || tempParentKey == "" {
		return fmt.Errorf("refusing to trash attachment %q: no temporary parent to prove it belongs to this run", attachKey)
	}
	if !zoteroItemKeyRE.MatchString(attachKey) {
		return fmt.Errorf("refusing to trash %q: not a valid item key", attachKey)
	}
	deadline := pollDeadline(ctx)
	var body json.RawMessage
	var version int
	for {
		var err error
		body, version, err = c.GetWithVersion("/items/"+attachKey, nil)
		if err == nil {
			break
		}
		if !isNotFoundOrLag(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("attachment %s never became visible on the write plane, so it cannot be trashed; "+
				"it may still surface beneath temporary parent %s", attachKey, tempParentKey)
		}
		if sleepErr := sleepWithContext(ctx, connectorReparentPollInterval); sleepErr != nil {
			// Wrapped rather than propagated bare: a cancelled or expired route
			// leaves an object the operator needs named, and "context deadline
			// exceeded" on its own says nothing about what to look for.
			return fmt.Errorf("stopped waiting for attachment %s, which may still surface beneath temporary parent %s: %w",
				attachKey, tempParentKey, sleepErr)
		}
	}
	if version <= 0 {
		return fmt.Errorf("attachment %s was read from the write plane without a version, so the trash cannot be guarded", attachKey)
	}
	if itemAlreadyTrashed(body) {
		return nil
	}
	var row struct {
		Data struct {
			ParentItem string `json:"parentItem"`
			MD5        string `json:"md5"`
		} `json:"data"`
	}
	if uerr := json.Unmarshal(body, &row); uerr != nil {
		return fmt.Errorf("parsing attachment %s: %w", attachKey, uerr)
	}
	if row.Data.ParentItem != tempParentKey {
		return fmt.Errorf("refusing to trash attachment %s: its parent is %q, not this run's temporary parent %s",
			attachKey, row.Data.ParentItem, tempParentKey)
	}
	// An EMPTY registered hash is not a mismatch, and must not block cleanup.
	// The desktop writes md5 and mtime a moment AFTER the connector creates the
	// attachment - that lag is the very race the 412 retry above exists for - so
	// at this point the field is legitimately often unset. Only a hash that is
	// present and DIFFERENT proves this is somebody else's file.
	if md5hex != "" && row.Data.MD5 != "" && !strings.EqualFold(row.Data.MD5, md5hex) {
		return fmt.Errorf("refusing to trash attachment %s: its content hash %q is not the one this run uploaded",
			attachKey, row.Data.MD5)
	}
	_, _, writeErr := patchWithVersionGuard(c, "/items/"+attachKey, map[string]any{"deleted": 1}, version)
	return writeErr
}

// findAttachmentByMD5 returns the key of an attachment child of parentKey whose
// registered content hash matches, or "" when none does.
//
// Deliberately hash-only: it ignores filename and linkMode, because this route
// controls neither. Zotero derives the stored filename from the parent item's
// title, and the connector produces "imported_url" rather than "imported_file".
func findAttachmentByMD5(c *client.Client, parentKey, md5hex string) (string, error) {
	if strings.TrimSpace(md5hex) == "" {
		return "", nil
	}
	rows, err := attachmentChildRows(c, parentKey)
	if err != nil {
		// A 404 means the target is not visible on this plane YET, which is the
		// normal state of an item created through the connector seconds ago. It
		// cannot have an attachment carrying our hash, so report "none" and let
		// verifyReparentTarget — which polls — decide whether the item really is
		// missing. Treating it as fatal here broke create-then-attach, the exact
		// shape a consumer uses, and no unit test caught it because both fakes
		// serve the target immediately.
		var cliErr *cliError
		if errors.As(err, &cliErr) {
			return "", nil
		}
		return "", err
	}
	for _, row := range rows {
		// A trashed attachment is not "already attached": the operator removed
		// it, so re-attaching is the requested outcome rather than a duplicate.
		if row.Data.ItemType != "attachment" || row.Data.Deleted != 0 {
			continue
		}
		if strings.EqualFold(row.Data.MD5, md5hex) {
			return row.Key, nil
		}
	}
	return "", nil
}

// findResumableTemporaryParent looks for a temporary parent left behind by an
// earlier run for this same target, which already holds the requested content.
//
// This is what makes the route crash-safe rather than merely retry-safe. A run
// killed between the connector save and the move leaves the file under a
// temporary parent; the target reconciliation cannot see it, because the file
// never reached the target. Without adoption, every retry would create another
// temporary parent and another copy of the bytes.
//
// Any run's marker qualifies — the point is to adopt an orphan — but identity is
// still the marker plus the target key it names, never the title. It returns ""
// when there is nothing to resume, and refuses when several orphans qualify.
type resumableTemporaryParentAmbiguityError struct {
	targetKey string
	matches   int
}

func (e *resumableTemporaryParentAmbiguityError) Error() string {
	return fmt.Sprintf("%d resumable temporary attachments match target %s; refusing to choose one",
		e.matches, e.targetKey)
}

func findResumableTemporaryParent(ctx context.Context, flags *rootFlags, targetKey, md5hex string) (string, string, error) {
	if strings.TrimSpace(md5hex) == "" {
		return "", "", nil
	}
	c, err := localClientForRoute(ctx, flags)
	if err != nil {
		return "", "", err
	}
	c.NoCache = true

	candidates, err := markedTemporaryParents(c, targetKey)
	if err != nil {
		return "", "", err
	}
	var tempKey, attachKey string
	matched := 0
	for _, candidate := range candidates {
		rows, err := attachmentChildRows(c, candidate)
		if err != nil {
			return "", "", fmt.Errorf("reading children of resumable temporary parent %s: %w", candidate, err)
		}
		for _, row := range rows {
			if row.Data.ItemType != "attachment" || row.Data.Deleted != 0 {
				continue
			}
			if strings.EqualFold(row.Data.MD5, md5hex) && zoteroItemKeyRE.MatchString(row.Key) {
				matched++
				tempKey, attachKey = candidate, row.Key
			}
		}
	}
	// Adopting the wrong orphan would move a file the operator did not name.
	switch matched {
	case 0:
		return "", "", nil
	case 1:
		return tempKey, attachKey, nil
	default:
		return "", "", &resumableTemporaryParentAmbiguityError{targetKey: targetKey, matches: matched}
	}
}

// markedTemporaryParents lists recent items carrying this route's marker for one
// target, whatever run wrote them.
func markedTemporaryParents(c *client.Client, targetKey string) ([]string, error) {
	var keys []string
	for start := 0; start < 500; start += connectorChildrenPageSize {
		data, err := c.Get("/items/top", map[string]string{
			"sort":      "dateAdded",
			"direction": "desc",
			"limit":     strconv.Itoa(connectorChildrenPageSize),
			"start":     strconv.Itoa(start),
		})
		if err != nil {
			return nil, fmt.Errorf("looking for a resumable temporary parent: %w", err)
		}
		var rows []struct {
			Key  string `json:"key"`
			Data struct {
				ItemType     string `json:"itemType"`
				AbstractNote string `json:"abstractNote"`
				Deleted      int    `json:"deleted"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &rows); err != nil {
			return nil, fmt.Errorf("decoding recent items: %w", err)
		}
		for _, row := range rows {
			if row.Key == targetKey || row.Data.Deleted != 0 {
				continue
			}
			if !markerIdentifies(row.Data.ItemType, row.Data.AbstractNote, targetKey, "") {
				continue
			}
			if zoteroItemKeyRE.MatchString(row.Key) {
				keys = append(keys, row.Key)
			}
		}
		if len(rows) < connectorChildrenPageSize {
			break
		}
	}
	return keys, nil
}
