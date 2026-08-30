// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
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
	if adoptKey, adoptAttach, adoptErr := findResumableTemporaryParent(ctx, flags, req.ParentKey, req.MD5); adoptErr == nil && adoptKey != "" {
		out.TempParentKey = adoptKey
		out.AttachmentKey = adoptAttach
		out.Resumed = true
		return finishConnectorReparent(ctx, cmd, webClient, req, out, "")
	}

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
	if err := saveConnectorAttachment(ctx, flags, res, req); err != nil {
		return out, err
	}

	// The desktop's own database is authoritative the moment the connector
	// returns, and the temporary parent has exactly one attachment child by
	// construction — so this is deterministic, not a recency guess.
	attachKey, err := soleAttachmentChild(ctx, flags, out.TempParentKey, out.TempTitle)
	if err != nil {
		return out, err
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
	// duplicate, and take our redundant copy away with its temporary parent.
	if winner, checkErr := findAttachmentByMD5(webClient, req.ParentKey, req.MD5); checkErr == nil && winner != "" {
		out.AttachmentKey = winner
		out.RaceLost = true
		if trashErr := trashTemporaryParent(ctx, webClient, out.TempParentKey, trashNonce, req.ParentKey); trashErr == nil {
			out.TempTrashed = true
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"warning: another run attached this file first; the temporary parent %s could not be trashed: %v\n"+
					"         delete it by hand: zotio items delete %s --yes\n",
				out.TempParentKey, trashErr, out.TempParentKey)
		}
		return out, nil
	}

	if err := reparentAttachment(webClient, out.AttachmentKey, req.ParentKey, out.TempParentKey, version); err != nil {
		return out, fmt.Errorf("attachment %s is stored under temporary parent %s but could not be moved to %s: %w",
			out.AttachmentKey, out.TempParentKey, req.ParentKey, err)
	}

	// Only now is the temporary parent safe to trash. Reversed, this risks the
	// attachment following its parent into the trash.
	if err := trashTemporaryParent(ctx, webClient, out.TempParentKey, trashNonce, req.ParentKey); err != nil {
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

// saveConnectorAttachment posts the bytes into the connector session that
// created the temporary parent.
func saveConnectorAttachment(ctx context.Context, flags *rootFlags, res itemCreateResult, req storedUploadRequest) error {
	data, err := os.ReadFile(req.Path)
	if err != nil {
		return fmt.Errorf("temporary parent %s was created but its file could not be read: %w", res.WebKey, err)
	}
	conn, err := connectorForCreate(flags)
	if err != nil {
		return fmt.Errorf("temporary parent %s was created but the connector is unavailable: %w", res.WebKey, err)
	}
	// Zotero's importFromNetworkStream hard-rejects an empty url with an opaque
	// HTTP 500 AFTER the parent exists, so a local file always sends its own URI
	// — the same provenance import pdf records for standalone attachments.
	if err := conn.SaveAttachment(ctx, res.Session, res.ConnKey, req.Title, localFileURL(req.Path), req.ContentType, data); err != nil {
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

type attachmentChildRow struct {
	Key  string `json:"key"`
	Data struct {
		ItemType string `json:"itemType"`
		MD5      string `json:"md5"`
		URL      string `json:"url"`
		Deleted  int    `json:"deleted"`
	} `json:"data"`
}

// attachmentChildRows reads every attachment child, following pages. Zotero's
// default page is 25, so an unpaged read truncates silently.
func attachmentChildRows(c *client.Client, parentKey string) ([]attachmentChildRow, error) {
	var all []attachmentChildRow
	for start := 0; ; start += connectorChildrenPageSize {
		data, _, err := c.GetWithVersion("/items/"+parentKey+"/children", map[string]string{
			"itemType": "attachment",
			"limit":    strconv.Itoa(connectorChildrenPageSize),
			"start":    strconv.Itoa(start),
		})
		if err != nil {
			var respErr *client.APIError
			if errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound {
				return nil, notFoundErr(fmt.Errorf("item %s not found", parentKey))
			}
			return nil, fmt.Errorf("listing children of %s: %w", parentKey, err)
		}
		var page []attachmentChildRow
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

// attachmentParentVersion reads an attachment's current parent and version in
// one request. The 412 retry path needs to tell three states apart: already
// moved (the PATCH landed and its response was lost), still under the temporary
// parent (safe to retry), or sitting somewhere else entirely (abandon).
func attachmentParentVersion(c *client.Client, key string) (string, int, error) {
	body, version, err := c.GetWithVersion("/items/"+key, nil)
	if err != nil {
		return "", 0, err
	}
	if version <= 0 {
		return "", 0, fmt.Errorf("attachment %s was read without a version", key)
	}
	var envelope struct {
		Data struct {
			ParentItem string `json:"parentItem"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", 0, fmt.Errorf("decoding attachment %s: %w", key, err)
	}
	return envelope.Data.ParentItem, version, nil
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
// Retrying is safe here precisely because it is checked, not blind: the
// attachment must still be a child of the temporary parent this run created, so
// the only edit being absorbed is one to an object the route owns. An
// attachment that has moved elsewhere abandons with the original conflict
// rather than being wrenched back.
func reparentAttachment(c *client.Client, attachKey, newParentKey, tempParentKey string, version int) error {
	if version <= 0 {
		return fmt.Errorf("refusing to move attachment %s without a version to guard the write", attachKey)
	}
	if !zoteroItemKeyRE.MatchString(attachKey) || !zoteroItemKeyRE.MatchString(newParentKey) {
		return fmt.Errorf("refusing to move %q onto %q: not both valid item keys", attachKey, newParentKey)
	}
	for attempt := 0; ; attempt++ {
		headers := map[string]string{"If-Unmodified-Since-Version": strconv.Itoa(version)}
		_, _, err := c.PatchWithHeaders("/items/"+attachKey, map[string]any{"parentItem": newParentKey}, headers)
		if err == nil {
			return nil
		}
		if attempt >= connectorReparentConflictRetries || !isPreconditionFailed(err) {
			return err
		}
		// Every branch below returns the ORIGINAL 412 rather than a read error,
		// so the operator sees the conflict that actually stopped the route.
		currentParent, fresh, readErr := attachmentParentVersion(c, attachKey)
		switch {
		case readErr != nil:
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
func trashTemporaryParent(ctx context.Context, c *client.Client, key, nonce, targetKey string) error {
	if key == "" || key == targetKey {
		return fmt.Errorf("refusing to trash %q: it is the target item, not a temporary parent", key)
	}
	if !zoteroItemKeyRE.MatchString(key) {
		return fmt.Errorf("refusing to trash %q: not a valid item key", key)
	}
	deadline := pollDeadline(ctx)
	for {
		body, version, err := c.GetWithVersion("/items/"+key, nil)
		switch {
		case err == nil && version > 0:
			if itemAlreadyTrashed(body) {
				return nil
			}
			if !bodyIsTemporaryParentFor(body, targetKey, nonce) {
				return fmt.Errorf("refusing to trash %s: it does not carry this run's marker, "+
					"so it is not the temporary parent this route created", key)
			}
			headers := map[string]string{"If-Unmodified-Since-Version": strconv.Itoa(version)}
			_, _, writeErr := c.PatchWithHeaders("/items/"+key, map[string]any{"deleted": 1}, headers)
			return writeErr
		case err == nil:
			return fmt.Errorf("item %s was read from the write plane without a version, so the trash cannot be guarded", key)
		case !isNotFoundOrLag(err):
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("temporary parent %s never became visible on the write plane", key)
		}
		if err := sleepWithContext(ctx, connectorReparentPollInterval); err != nil {
			return err
		}
	}
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
			continue
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
	if matched != 1 {
		return "", "", nil
	}
	return tempKey, attachKey, nil
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
