// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"zotio/internal/client"
	"zotio/internal/store"
)

// Bounded incremental reconciliation (finding zotio-e647c89d4ad919eb).
//
// A default `zotio sync` never sweeps: SweepMissing runs only after a pass
// that observed every object, and an incremental pass carries a `since`
// filter, so absence means "unchanged", not "gone". Two staleness gaps
// follow from that, both confirmed live against the Zotero desktop API:
//
//   - a collection erased upstream is absent from the incremental
//     collections pass, so its mirror row survives;
//   - erasing a collection does not bump member item versions (it only
//     deletes the collectionItems rows), so the incremental items pass never
//     refetches the members and their data.collections keeps naming it;
//   - a trash row purged upstream is absent from the incremental trash pass,
//     so the mirror keeps it.
//
// The desktop plane has no /deleted feed (ADR-0007), so the only complete
// evidence of absence is a complete listing. This file fetches exactly two:
// the collection key set and the trash key set, both with `format=keys`.
// Each costs one small request: both planes return the full key set when
// `limit` is omitted (local API docs; dataserver applies LIMIT only when the
// param is present), so one response is one consistent snapshot and there is
// no paging whose offsets a concurrent change could shift. A Total-Results
// mismatch refuses the listing the same way a parse failure does.
//
// The wire format is newline-separated text (text/plain, no trailing
// newline, empty body for an empty set), per the Web API v3 docs and the
// desktop's makeDataObjectResponse (`keys.join('\n')`). The parser accepts
// exactly that and refuses everything else.
//
// Trash is not erase, and the listing cannot tell them apart: the desktop
// omits trashed collections from /collections (getByLibrary without
// includeTrashed) but keeps their member links (trash() only inserts into
// deletedCollections, for restore), while erase deletes the collectionItems
// rows. So before reaping an absent collection that still has mirrored live
// members, one member is probed on the plane: if it still names the
// collection, the collection is merely trashed and everything is kept (the
// mirror already matches the plane, and a later restore needs no repair);
// otherwise it is erased and the row is reaped with member repair.
//
// Member repair refetches by key only the mirrored items that listed a
// confirmed-erased collection — zero requests when nothing was reaped, which
// is the common case. Trash rows are never edited: the mirror reflects the
// plane's last observation, and invented payload edits would diverge from it
// on any plane whose trash payloads keep the key. A full pass already sweeps
// everything, so this runs on incremental passes only and never changes exit
// codes: every failure warns and keeps the mirror, exactly like today's
// behavior without it.
type incrementalReapOutcome struct {
	collectionsReaped int
	trashReaped       int
	membersRefreshed  int
	membersSkipped    int
	requests          int
}

// reconcileIncrementalSync reaps upstream deletions an incremental pass
// cannot observe. resources is the normalized sync set; clean marks the
// resources whose pass completed without error or access warning. A
// resource that did not sync, or did not complete, is left alone: the
// reconciliation only acts on evidence its own complete listing provides,
// and only for resources this run already observed successfully.
//
// It never fails the sync. Every problem warns (stderr prose when
// human-friendly, a sync_anomaly event otherwise) and keeps the mirror.
func reconcileIncrementalSync(ctx context.Context, c syncHTTPClient, db *store.Store, resources []string, clean map[string]bool) incrementalReapOutcome {
	var out incrementalReapOutcome
	if ctx.Err() != nil {
		return out
	}
	inSet := make(map[string]bool, len(resources))
	for _, r := range resources {
		inSet[r] = true
	}
	var confirmedCollections []string
	var probes *reapProbes
	if inSet["collections"] && clean["collections"] {
		confirmedCollections, probes = sweepIncrementalCollections(ctx, c, db, &out)
	}
	if ctx.Err() != nil {
		return out
	}
	if inSet["items-trash"] && clean["items-trash"] {
		sweepIncrementalResource(ctx, c, db, "items-trash", &out)
	}
	if len(confirmedCollections) == 0 {
		return out
	}
	if ctx.Err() != nil {
		return out
	}
	if inSet["items"] && clean["items"] {
		refreshReapedCollectionMembers(ctx, c, db, confirmedCollections, probes, &out)
	} else {
		incrementalReapWarn(ctx, "items",
			"an erased collection was reaped but items did not sync, so member links still name it; run `zotio sync` (which includes items) to refresh them")
	}
	return out
}

// sweepIncrementalResource reaps mirrored rows of one resource absent from a
// complete key listing. It reuses SweepMissing, so deletion markers retire
// exactly as on a full pass (ADR-0007): a marker whose key the complete
// listing stopped carrying is confirmed, one the listing still carries keeps
// suppressing.
func sweepIncrementalResource(ctx context.Context, c syncHTTPClient, db *store.Store, resource string, out *incrementalReapOutcome) {
	storeResource := canonicalStoreResource(resource)
	path, err := syncResourcePath(resource)
	if err != nil {
		incrementalReapWarn(ctx, resource, fmt.Sprintf("could not reconcile deleted %s rows: %v", resource, err))
		return
	}
	seen, complete := fetchPlaneKeySet(ctx, syncClientForResource(c, resource), path, resource, out)
	if !complete {
		// fetchPlaneKeySet already warned with the reason.
		return
	}
	reaped, err := db.SweepMissingContext(ctx, storeResource, seen)
	if err != nil {
		incrementalReapWarn(ctx, resource, fmt.Sprintf("could not reap deleted %s rows: %v; kept them", storeResource, err))
		return
	}
	switch resource {
	case "collections":
		out.collectionsReaped += reaped
	case "items-trash":
		out.trashReaped += reaped
	}
	if reaped > 0 && humanFriendly {
		fmt.Fprintf(os.Stderr, "  reaped %d %s row(s) for objects that no longer exist\n", reaped, storeResource)
	}
}

// sweepIncrementalCollections reaps mirrored collections absent from the
// complete key listing, except ones the plane still references (see
// classifyAbsentCollections). It returns the confirmed-erased ids — the only
// ones whose member links may be repaired — with the probe cache that lets
// the repair reuse their fetches.
func sweepIncrementalCollections(ctx context.Context, c syncHTTPClient, db *store.Store, out *incrementalReapOutcome) ([]string, *reapProbes) {
	path, err := syncResourcePath("collections")
	if err != nil {
		incrementalReapWarn(ctx, "collections", fmt.Sprintf("could not reconcile deleted collections rows: %v", err))
		return nil, nil
	}
	before, err := db.ResourceIDs("collections")
	if err != nil {
		incrementalReapWarn(ctx, "collections", fmt.Sprintf("could not read mirrored collections rows: %v; kept them", err))
		return nil, nil
	}
	seen, complete := fetchPlaneKeySet(ctx, syncClientForResource(c, "collections"), path, "collections", out)
	if !complete {
		// fetchPlaneKeySet already warned with the reason.
		return nil, nil
	}
	var absent []string
	for id := range before {
		if !seen[id] {
			absent = append(absent, id)
		}
	}
	if len(absent) == 0 {
		return nil, nil
	}
	confirmed, spared, probes := classifyAbsentCollections(ctx, syncClientForResource(c, "collections"), db, absent, out)
	for _, id := range spared {
		seen[id] = true
	}
	reaped, err := db.SweepMissingContext(ctx, "collections", seen)
	if err != nil {
		incrementalReapWarn(ctx, "collections", fmt.Sprintf("could not reap deleted collections rows: %v; kept them", err))
		return nil, nil
	}
	out.collectionsReaped += reaped
	if reaped > 0 && humanFriendly {
		fmt.Fprintf(os.Stderr, "  reaped %d collections row(s) for objects that no longer exist\n", reaped)
	}
	return confirmed, probes
}

// fetchPlaneKeySet fetches one complete `format=keys` listing in a single
// unpaginated request. complete is false on any request, decode, or
// completeness failure, and the caller must not sweep then: a partial key
// set cannot tell "deleted" from "not fetched". The listing carries no
// narrowing filter (no since), so a completed fetch observed every object
// and its absences mean "gone from the listing" (trashed or erased for
// collections — see classifyAbsentCollections).
func fetchPlaneKeySet(ctx context.Context, c syncHTTPClient, path, resource string, out *incrementalReapOutcome) (seen map[string]bool, complete bool) {
	params := map[string]string{"format": "keys"}
	// Both planes mandate Total-Results on multi-object reads, and the
	// local plane sends it on every response including keys listings, so a
	// missing or non-numeric header is missing evidence rather than an
	// empty set: a 200 with a swallowed body must never read as zero keys
	// and reap the whole mirror. A client without header reads cannot
	// establish completeness at all. Every client in the sync path is a
	// *client.Client (in both planes — the plane is only a base URL), so
	// this skips only test doubles.
	headerClient, ok := c.(syncHeaderHTTPClient)
	if !ok {
		incrementalReapWarn(ctx, resource, fmt.Sprintf("could not list %s keys: header reads unsupported; kept mirror rows (run `zotio sync --full` to reap)", resource))
		return nil, false
	}
	data, totalHeader, err := headerClient.GetWithHeaderContext(ctx, path, params, "Total-Results")
	out.requests++
	if err != nil {
		incrementalReapWarn(ctx, resource, fmt.Sprintf("could not list %s keys: %v; kept mirror rows (run `zotio sync --full` to reap)", resource, err))
		return nil, false
	}
	keys, ok := parsePlaneKeySet(data)
	if !ok {
		incrementalReapWarn(ctx, resource, fmt.Sprintf("could not parse %s key listing; kept mirror rows (run `zotio sync --full` to reap)", resource))
		return nil, false
	}
	n, perr := strconv.Atoi(strings.TrimSpace(totalHeader))
	if perr != nil || n != len(keys) {
		incrementalReapWarn(ctx, resource, fmt.Sprintf("could not list %s keys: Total-Results %q does not match %d keys sent; kept mirror rows", resource, strings.TrimSpace(totalHeader), len(keys)))
		return nil, false
	}
	seen = make(map[string]bool, len(keys))
	for _, k := range keys {
		seen[k] = true
	}
	return seen, true
}
func parsePlaneKeySet(data json.RawMessage) ([]string, bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, true
	}
	if trimmed[0] == '[' || trimmed[0] == '{' {
		return nil, false
	}
	var keys []string
	for _, line := range bytes.Split(trimmed, []byte{'\n'}) {
		key := string(bytes.TrimSpace(line))
		if key == "" {
			continue
		}
		if err := validateZoteroKey(key); err != nil {
			return nil, false
		}
		keys = append(keys, key)
	}
	return keys, true
}

// reapProbe caches one member's plane state so an item shared by several
// absent collections is fetched at most once per sync.
type reapProbe struct {
	fetched   bool
	ok        bool // fetch succeeded with a parseable object
	notFound  bool // the plane 404s the member
	obj       json.RawMessage
	fetchErr  error
	readable  bool // no deletion marker; a marked key is gone locally
	canUpsert bool // no local trash row; the lifecycle tie would kill the live row
	upserted  bool
}

// reapProbes carries classifyAbsentCollections' per-member fetch cache and
// trash-row guard to refreshReapedCollectionMembers within one
// reconciliation. Both run sequentially inside reconcileIncrementalSync.
type reapProbes struct {
	byKey   map[string]*reapProbe
	inTrash map[string]bool
}

// classifyAbsentCollections splits mirrored collections absent from the key
// listing into confirmed-erased and spared. An absent collection with
// mirrored live members is probed: the first usable member is fetched, and
// if the plane object still names the collection it is merely trashed —
// spared with its links intact (a later restore then needs no repair). Any
// fetch error spares too, so a transient failure retries next sync instead
// of reaping on doubt. With no usable member (none, all locally deleted, or
// all gone on the plane) the row is still reaped — an empty collection has
// no links to break and restore resurrects the row — but no member links are
// touched without confirmation.
func classifyAbsentCollections(ctx context.Context, c syncHTTPClient, db *store.Store, absent []string, out *incrementalReapOutcome) (confirmed, spared []string, probes *reapProbes) {
	sort.Strings(absent)
	probes = &reapProbes{byKey: map[string]*reapProbe{}, inTrash: map[string]bool{}}
	guarded, err := db.KeysWithCollections(ctx, "items-trash", absent)
	if err != nil {
		// The trash-row guard is lost; lifecycle arbitration in the normal
		// upsert path is the backstop, same as any sync pass.
		incrementalReapWarn(ctx, "items", fmt.Sprintf("could not read trashed items listing a deleted collection: %v", err))
	}
	for _, k := range guarded {
		probes.inTrash[k] = true
	}
	probeFor := func(key string) *reapProbe {
		if p, ok := probes.byKey[key]; ok {
			return p
		}
		p := &reapProbe{}
		probes.byKey[key] = p
		if deleted, derr := db.PendingDeletion("items", key); derr != nil || deleted {
			// A confirmed local delete (or an unreadable marker table)
			// makes the member unusable as a witness.
			return p
		}
		p.readable = true
		p.canUpsert = !probes.inTrash[key]
		return p
	}
	fetchProbe := func(key string, p *reapProbe) {
		if p.fetched || !p.readable {
			return
		}
		p.fetched = true
		data, _, gerr := c.GetWithVersionContext(ctx, "/items/"+url.PathEscape(key), nil)
		out.requests++
		if gerr != nil {
			var apiErr *client.APIError
			if errors.As(gerr, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
				p.notFound = true
			} else {
				p.fetchErr = gerr
			}
			return
		}
		p.obj = data
		p.ok = true
	}
	var fetchFailed int
	var firstFetchErr error
	for _, id := range absent {
		if err := ctx.Err(); err != nil {
			return confirmed, spared, probes
		}
		members, merr := db.KeysWithCollections(ctx, "items", []string{id})
		if merr != nil {
			incrementalReapWarn(ctx, "collections", fmt.Sprintf("could not find items listing deleted collection %s: %v; kept it", id, merr))
			spared = append(spared, id)
			continue
		}
		sort.Strings(members)
		for _, m := range members {
			p := probeFor(m)
			if !p.readable {
				continue
			}
			fetchProbe(m, p)
			if p.fetchErr != nil {
				fetchFailed++
				if firstFetchErr == nil {
					firstFetchErr = p.fetchErr
				}
				spared = append(spared, id)
				break
			}
			if p.notFound {
				continue
			}
			lists, ok := planeCollections(p.obj)
			if !ok {
				fetchFailed++
				if firstFetchErr == nil {
					firstFetchErr = fmt.Errorf("unparseable item %s", m)
				}
				spared = append(spared, id)
				break
			}
			if lists[id] {
				// The plane still names the collection: trashed, not
				// erased. Keep the row and every link; restore needs no
				// repair because nothing was stripped.
				spared = append(spared, id)
			} else {
				confirmed = append(confirmed, id)
			}
			break
		}
		// No usable witness (no live members, all locally deleted, or all
		// gone on the plane): the row is still reaped below, but no member
		// links are touched without confirmation.
	}
	if fetchFailed > 0 {
		incrementalReapWarn(ctx, "collections", fmt.Sprintf("could not probe %d collection(s) absent from the listing (first error: %v); kept them for the next sync", fetchFailed, firstFetchErr))
	}
	return confirmed, spared, probes
}

// planeCollections returns the collection set a plane item object names.
func planeCollections(raw json.RawMessage) (map[string]bool, bool) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	data, ok := obj["data"].(map[string]any)
	if !ok {
		return nil, false
	}
	colls, ok := data["collections"].([]any)
	if !ok {
		return nil, false
	}
	out := make(map[string]bool, len(colls))
	for _, entry := range colls {
		s, ok := entry.(string)
		if !ok {
			return nil, false
		}
		out[s] = true
	}
	return out, true
}

// reapMemberBatchSize caps one /items repair fetch: Zotero accepts up to 50
// itemKey values per request.
const reapMemberBatchSize = 50

// refreshReapedCollectionMembers refetches by key every mirrored live item
// that still lists a confirmed-erased collection, so its data.collections
// matches the plane. Probe-fetched objects are reused, and the rest are
// fetched via /items?itemKey= in batches of up to 50, so N members cost at
// most ceil(N/50) requests plus the earlier probes. Batches are upserted
// through the normal batch path, which replays pending field writes and
// drops rows carrying a deletion marker — a member deleted locally is
// skipped before any request. Equal versions still land (the store guard is
// >=), which is exactly the unchanged-version case that motivated this
// repair.
//
// Rows the mirror cannot arbitrate are left for the normal sync: a member
// with local trash state (the tie would kill the just-confirmed live row),
// and a member the plane no longer returns (gone; the next full pass reaps
// the row).
func refreshReapedCollectionMembers(ctx context.Context, c syncHTTPClient, db *store.Store, reaped []string, probes *reapProbes, out *incrementalReapOutcome) {
	members, err := db.KeysWithCollections(ctx, "items", reaped)
	if err != nil {
		incrementalReapWarn(ctx, "items", fmt.Sprintf("could not find items listing an erased collection: %v; member links still name it until `zotio sync --full`", err))
		return
	}
	if probes == nil {
		probes = &reapProbes{byKey: map[string]*reapProbe{}, inTrash: map[string]bool{}}
	}
	guarded, err := db.KeysWithCollections(ctx, "items-trash", reaped)
	if err != nil {
		incrementalReapWarn(ctx, "items", fmt.Sprintf("could not read trashed items listing an erased collection: %v", err))
	} else {
		for _, k := range guarded {
			probes.inTrash[k] = true
		}
	}
	sort.Strings(members)
	// Partition members into probe-cached objects (no new request), keys to
	// fetch in itemKey batches, and skips. Each member lands in exactly one.
	type pendingRefetch struct {
		key   string
		obj   json.RawMessage
		probe *reapProbe
	}
	var cached []pendingRefetch
	var fetchQueue []string
	var failed int
	var firstErr error
	for _, key := range members {
		if err := ctx.Err(); err != nil {
			return
		}
		p, ok := probes.byKey[key]
		if !ok {
			// A member the partition never probed (its collections were
			// confirmed through another member): build a fresh probe.
			p = &reapProbe{readable: true, canUpsert: !probes.inTrash[key]}
			if deleted, derr := db.PendingDeletion("items", key); derr != nil || deleted {
				out.membersSkipped++
				continue
			}
			probes.byKey[key] = p
		}
		if !p.readable || !p.canUpsert {
			out.membersSkipped++
			continue
		}
		if p.upserted {
			continue
		}
		if p.fetched {
			if p.ok {
				cached = append(cached, pendingRefetch{key: key, obj: p.obj, probe: p})
			} else {
				out.membersSkipped++
			}
			continue
		}
		fetchQueue = append(fetchQueue, key)
	}
	upsertRefetches := func(batch []pendingRefetch) {
		objs := make([]json.RawMessage, 0, len(batch))
		for _, r := range batch {
			objs = append(objs, r.obj)
		}
		if _, _, uerr := upsertResourceBatch(ctx, db, "items", objs); uerr != nil {
			failed += len(batch)
			if firstErr == nil {
				firstErr = uerr
			}
			return
		}
		for _, r := range batch {
			r.probe.upserted = true
		}
		out.membersRefreshed += len(batch)
	}
	for start := 0; start < len(cached); start += reapMemberBatchSize {
		if err := ctx.Err(); err != nil {
			return
		}
		end := start + reapMemberBatchSize
		if end > len(cached) {
			end = len(cached)
		}
		upsertRefetches(cached[start:end])
	}
	for start := 0; start < len(fetchQueue); start += reapMemberBatchSize {
		if err := ctx.Err(); err != nil {
			return
		}
		end := start + reapMemberBatchSize
		if end > len(fetchQueue) {
			end = len(fetchQueue)
		}
		chunk := fetchQueue[start:end]
		data, _, gerr := c.GetWithVersionContext(ctx, "/items", map[string]string{"itemKey": strings.Join(chunk, ",")})
		out.requests++
		if gerr != nil {
			failed += len(chunk)
			if firstErr == nil {
				firstErr = gerr
			}
			continue
		}
		var raws []json.RawMessage
		if jerr := json.Unmarshal(data, &raws); jerr != nil {
			failed += len(chunk)
			if firstErr == nil {
				firstErr = jerr
			}
			continue
		}
		want := make(map[string]bool, len(chunk))
		for _, k := range chunk {
			want[k] = true
		}
		var batch []pendingRefetch
		for _, raw := range raws {
			id := pendingWriteID("items", raw)
			if id == "" || !want[id] {
				// Unattributable payload: the plane sent something
				// unkeyable or unasked-for; it verifies nothing.
				failed++
				if firstErr == nil {
					firstErr = fmt.Errorf("unattributable item in batch refetch")
				}
				continue
			}
			delete(want, id)
			p := probes.byKey[id]
			p.fetched = true
			p.obj = raw
			p.ok = true
			batch = append(batch, pendingRefetch{key: id, obj: raw, probe: p})
		}
		for range want {
			// Requested but unreturned: gone on the plane; the next
			// full pass reaps the row.
			out.membersSkipped++
		}
		upsertRefetches(batch)
	}
	if failed > 0 {
		incrementalReapWarn(ctx, "items", fmt.Sprintf("could not refresh %d item(s) that listed an erased collection (first error: %v); run `zotio sync --full` to reconcile", failed, firstErr))
	} else if out.membersRefreshed > 0 && humanFriendly {
		fmt.Fprintf(os.Stderr, "  refreshed %d item(s) that listed an erased collection\n", out.membersRefreshed)
	}
}

// incrementalReapWarn reports a reconciliation problem without failing the
// sync: the incremental passes already succeeded, and this repair is
// best-effort on top. Same two channels every other sync warning uses.
func incrementalReapWarn(ctx context.Context, resource, message string) {
	if humanFriendly {
		fmt.Fprintf(os.Stderr, "warning: %s\n", message)
		return
	}
	emitSyncEvent(ctx, struct {
		Event    string `json:"event"`
		Resource string `json:"resource"`
		Reason   string `json:"reason"`
		Message  string `json:"message"`
	}{
		Event:    "sync_anomaly",
		Resource: resource,
		Reason:   "incremental_reap_skipped",
		Message:  message,
	})
}
