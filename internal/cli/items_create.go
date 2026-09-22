// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	// Direct batch creates can route through the desktop connector.
	"zotio/internal/connector"
	"zotio/internal/mutation"
)

func newItemsCreateCmd(flags *rootFlags) *cobra.Command {
	var bodyItems string
	var stdinBody bool

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create one or more items",
		Long: `Create one or more items from JSON supplied with --items or --stdin.

This command shares one connector session across the invocation when routed
through --via connector; the Zotero desktop surfaces its own progress UI and
zotio cannot dismiss it — no connector endpoint closes or completes a save
session. Observed 2026-08-22: roughly 78 consecutive one-per-item
invocations left Zotero unresponsive with progress windows accumulating; no
proven mechanism has been established. Prefer one invocation with many items:
import file --via connector and this command share one session, while import
apply currently opens one per manifest entry. --rate-limit governs only Web
API requests and does not pace connector calls.`,
		Example:     "  zotio items create",
		Annotations: map[string]string{"zotio:endpoint": "items.create", "zotio:method": "POST", "zotio:path": "/items"},
		RunE: func(cmd *cobra.Command, args []string) error {
			const path = "/items"
			body, err := itemsCreateBody(cmd, bodyItems, stdinBody)
			if err != nil {
				return err
			}
			// One op per item, always. CheckGates counts operations, not the
			// Changes inside them, so a single batched Op (one Op, N Changes)
			// would charge an N-item array as 1 against --max-changes. These
			// same ops carry the real write's per-item Apply closures below, on
			// either route, so the previewed count, the gate's count and the
			// journaled count are always the same N.
			ops := itemsCreatePreflightOps(body)
			if !resolveMutationMode(flags).Apply {
				env, runErr := runMutation(cmd.Context(), flags, "items.create", ops)
				if renderErr := renderMutation(cmd, flags, env, nil); renderErr != nil {
					return renderErr
				}
				return runErr
			}
			// Gate before any network call. Both routes send the whole batch in
			// ONE request from the first op's Apply, so by the time the engine
			// is applying, every item is already committed and a refusal part
			// way through the ops could not un-write anything.
			if gateFailure := mutation.CheckGates(mutationOptions(flags), ops); gateFailure != nil {
				return fmt.Errorf("%s", gateFailure.Message)
			}
			via, err := flags.resolveCreateVia(cmd.Context(), false)
			if err != nil {
				return err
			}
			connectorItems, err := itemsCreateConnectorBatch(flags, via, body)
			if err != nil {
				return err
			}
			var batch itemsCreateBatch
			if connectorItems != nil {
				batch.attachConnector(cmd.Context(), flags, ops, connectorItems)
			} else {
				// Only the Web route needs an API client, and only it needs a
				// key: the connector writes to the desktop over localhost.
				c, err := flags.newClient()
				if err != nil {
					return err
				}
				batch.attachWeb(flags, c, path, body, ops)
			}
			// A rejected element travelled to Zotero alongside its batch-mates
			// and cannot un-submit them, so stopping at the first rejection
			// would report the rest as not_attempted -- a lie that invites a
			// duplicating re-create. Every item reports its own outcome.
			env, runErr := runMutation(cmd.Context(), flags, "items.create", ops, func(o *mutation.Options) {
				o.ContinueOnError = true
			})
			if renderErr := renderMutation(cmd, flags, env, nil); renderErr != nil {
				return renderErr
			}
			if connectorItems != nil && createNeedsMirrorRefresh(env) {
				refreshItemsFromLocalAPI(cmd.Context(), flags)
			}
			// batch.err carries the command's own classification
			// (classifyAPIError / degradedErr / the connector's retry-filing
			// guidance); the engine's generic "mutation incomplete" names
			// neither the cause nor what to do next.
			if batch.err != nil {
				return batch.err
			}
			return runErr
		},
	}
	cmd.Flags().StringVar(&bodyItems, "items", "", "Array of item objects to create, or a single item object (sent as a one-element array; use item-types and item-type-fields for schema)")
	cmd.Flags().BoolVar(&stdinBody, "stdin", false, "Read request body as JSON from stdin (array or single object, as --items)")

	return cmd
}

// itemsCreateBody reads the request body from --items or --stdin.
//
// Zotero's POST /items requires a bare JSON array of item objects. The generated
// shape wrapped it as {"items": [...]}, which the API rejects ("Uploaded data
// must be a JSON array"), so the parsed value is passed through unwrapped. Either
// an array or a single object is accepted: a single object is normalised to a
// one-element array here, before planning, so every route below posts the array
// shape Zotero requires and charges exactly one operation for one item.
func itemsCreateBody(cmd *cobra.Command, bodyItems string, stdinBody bool) (any, error) {
	switch {
	case stdinBody:
		// cmd.InOrStdin(), not os.Stdin: under a stdio MCP server os.Stdin IS
		// the JSON-RPC transport, so a model-issued --stdin would consume the
		// protocol stream and hang the session.
		stdinData, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, fmt.Errorf("reading stdin: %w", err)
		}
		var jsonBody any
		if err := json.Unmarshal(stdinData, &jsonBody); err != nil {
			return nil, fmt.Errorf("parsing stdin JSON: %w", err)
		}
		return normalizeItemsCreateBody(jsonBody), nil
	case bodyItems != "":
		var parsedItems any
		if err := json.Unmarshal([]byte(bodyItems), &parsedItems); err != nil {
			return nil, fmt.Errorf("parsing --items JSON: %w", err)
		}
		return normalizeItemsCreateBody(parsedItems), nil
	default:
		return nil, nil
	}
}

// normalizeItemsCreateBody wraps a single item object as a one-element array.
// Anything else -- an array, a scalar, null -- passes through unchanged; the
// connector route refuses non-array input explicitly, and the Web route lets
// Zotero reject what it cannot accept.
func normalizeItemsCreateBody(body any) any {
	if obj, ok := body.(map[string]any); ok {
		return []any{obj}
	}
	return body
}

// itemsCreateConnectorBatch reports the items a desktop-connector batch would
// save, or nil when this create has to go through the Web API. A connector save
// session has exactly one target, so per-item collection arrays cannot be
// honoured on that route; an explicit --via connector says so rather than
// silently dropping them, and an automatic route falls back to the Web API.
// Non-array input is not representable as a save session either: an explicit
// --via connector is a usage error there, never a silent fall back to the Web
// API, because the operator asked for the desktop's own file store. An
// automatic route still falls back to the Web API, which reports what Zotero
// itself rejects.
func itemsCreateConnectorBatch(flags *rootFlags, via string, body any) ([]map[string]any, error) {
	if via != "connector" {
		return nil, nil
	}
	items, ok := itemsCreateObjects(body)
	if !ok {
		if flags.via == "connector" {
			return nil, fmt.Errorf("--via connector requires an array of item objects in items create; the supplied body is not a non-empty item array")
		}
		return nil, nil
	}
	if itemsCreateHasCollections(items) && strings.TrimSpace(flags.connectorTarget) == "" {
		if flags.via == "connector" {
			return nil, fmt.Errorf("--via connector cannot honor per-item collections in items create; use --via web or --connector-target C<n>")
		}
		return nil, nil
	}
	return items, nil
}

// itemsCreateBatch wires one Apply closure per planned item onto a single
// batched write, so both routes report per-item outcomes through the same
// mutation envelope while issuing exactly the requests they always did: one
// POST /items, or one connector saveItems.
//
// err records the command's own classification of a batch-wide outcome the
// engine cannot express -- a transport failure, Zotero's per-element rejections,
// or a connector save that committed while its target filing failed.
type itemsCreateBatch struct {
	err error
}

// attachWeb routes the batch through POST /items. The first op's Apply issues
// the one batched request and caches the decoded response; every other op reads
// its own element's outcome from that cache, so the request count does not
// depend on how the engine iterates.
func (b *itemsCreateBatch) attachWeb(flags *rootFlags, c itemPoster, path string, body any, ops []mutation.Op) {
	var (
		executed  bool
		transport error // set only when the POST itself failed, not on a per-element rejection
		envelope  error // set when the 2xx body is not the batch envelope: no outcome is proven
		failed    map[string]batchWriteFailure
		keys      map[string]string
	)
	post := func() {
		if executed {
			return
		}
		executed = true
		data, _, postErr := c.Post(path, body)
		if postErr != nil {
			transport = classifyAPIError(postErr, flags)
			b.err = transport
			return
		}
		// A 2xx without the batch envelope proves nothing: it may be a proxy
		// error page, a singleton object, or truncated JSON. Decoding only Failed
		// would read all of those as zero failures and report the whole batch
		// applied, so the envelope must be verified first.
		if envErr := checkBatchEnvelope(data, len(ops)); envErr != nil {
			envelope = fmt.Errorf("items create: %w; the outcome of the %d item(s) in that request is unknown", envErr, len(ops))
			b.err = degradedErr(envelope)
			return
		}
		failed = decodeBatchWriteResponse(data).Failed
		keys = itemsCreateKeysByIndex(data)
		if bwErr := batchWriteFailuresError("items create", failed); bwErr != nil {
			b.err = degradedErr(bwErr)
		}
	}
	for i := range ops {
		index := i
		ops[i].Apply = func() (string, any, error) {
			post()
			if transport != nil {
				return "failed", nil, transport
			}
			if envelope != nil {
				return "failed", envelope.Error(), envelope
			}
			if failure, ok := failed[strconv.Itoa(index)]; ok {
				return "failed", fmt.Sprintf("index %d: code %d: %s", index, failure.Code, failure.Message), nil
			}
			return "applied", itemCreateAppliedReason("web", keys[strconv.Itoa(index)]), nil
		}
	}
}

// attachConnector routes the batch through one desktop save session. Zotero
// surfaces its own progress UI per session and no connector endpoint closes one,
// so the whole batch shares a single session (see this command's help text).
//
// ops and items are index-aligned: itemsCreateConnectorBatch returns items only
// for a body that itemsCreatePreflightOps splits into one op per object. The
// bounds check below keeps a future divergence from panicking after SaveItems
// has already committed, which would lose the envelope and the journal entry
// for a write that landed.
func (b *itemsCreateBatch) attachConnector(ctx context.Context, flags *rootFlags, ops []mutation.Op, items []map[string]any) {
	target := strings.TrimSpace(flags.connectorTarget)
	var (
		executed  bool
		saveErr   error
		saveCause error // SaveItems reported an error of unknown outcome; per-item reconciliation below decides
		filingErr error
		sessionID string
		keys      []string
	)
	save := func() {
		if executed {
			return
		}
		executed = true
		fail := func(err error) {
			saveErr = err
			b.err = err
		}
		conn, err := flags.newConnector()
		if err != nil {
			fail(err)
			return
		}
		if sessionID, err = connector.NewID(); err != nil {
			fail(err)
			return
		}
		// connector.SaveItems requires a unique connector-local "id" on every
		// item. It is a save-session correlation id, not a Zotero field, so it
		// travels on a copy: the caller's map is the op's recorded change, which
		// the journal stores and write-through mirrors, and neither may carry it.
		payload := make([]map[string]any, len(items))
		for i, item := range items {
			connectorKey, err := connector.NewID()
			if err != nil {
				fail(err)
				return
			}
			copied := make(map[string]any, len(item)+1)
			for field, value := range item {
				copied[field] = value
			}
			copied["id"] = connectorKey
			payload[i] = copied
		}
		createdAfter := time.Now().UTC().Add(-recentItemClockSkew)
		if err := conn.SaveItems(ctx, sessionID, "", payload); err != nil {
			// Zotero's connector can return an error AFTER having already
			// created the items (observed: HTTP 500 with the item present at
			// that instant; see routeCreateItemViaWithOptions). Marking the
			// whole batch failed would lose the committed identities and
			// invite a duplicating re-create, so reconcile each item against
			// the library first: recovered keys stay applied, the rest report
			// an unknown outcome the operator must inspect before retrying.
			keys = make([]string, len(items))
			for i, item := range items {
				keys[i], _, _ = confirmConnectorCreate(ctx, flags, item, createdAfter)
			}
			saveCause = err
			if !itemsCreateAllRecovered(keys) {
				b.err = connectorSaveAmbiguityError(sessionID, keys, err)
			}
			return
		}
		// Zotero's connector can succeed at SaveItems and then fail at the
		// follow-up UpdateSession target filing; there is no transaction across
		// the two calls. Treating that as a total failure would invite a
		// duplicating retry, so the items stay applied and the filing error is
		// reported separately.
		if target != "" {
			filingErr = conn.UpdateSession(ctx, sessionID, target, nil, "")
		}
		// Resolve the real Zotero key per item. The connector's own id means
		// nothing to the API, so without this the journal records nothing
		// actionable, `journal undo` has no item to trash, and write-through has
		// no key to mirror the row under.
		keys = make([]string, len(items))
		for i, item := range items {
			keys[i], _, _ = confirmConnectorCreate(ctx, flags, item, createdAfter)
		}
		if filingErr != nil {
			b.err = connectorFilingError(len(items), sessionID, target, keys, filingErr)
		}
	}
	for i := range ops {
		index := i
		ops[i].Apply = func() (string, any, error) {
			save()
			if saveErr != nil {
				return "failed", nil, saveErr
			}
			key := ""
			if index < len(keys) {
				key = keys[index]
			}
			// SaveItems errored but the write may already have committed: the
			// item was found in the library, so it exists and stays applied
			// with its recovered key. Anything else is an unknown outcome,
			// not a proven failure -- report it as a conflict carrying the
			// session evidence, so the journal records it and the operator
			// inspects Zotero instead of blindly re-creating.
			if saveCause != nil {
				if key != "" {
					reason := itemCreateAppliedReason("connector", key)
					reason["session"] = sessionID
					reason["recovered_after_save_error"] = true
					reason["save_error"] = saveCause.Error()
					reason["message"] = fmt.Sprintf("created via connector (session %s) although SaveItems reported an error: %v; the item was found in the library under key %s, do not re-create it", sessionID, saveCause, key)
					return "applied", reason, nil
				}
				detail := map[string]any{
					"via": "connector", "committed": true,
					"session":         sessionID,
					"connector_error": saveCause.Error(),
					"message":         fmt.Sprintf("connector SaveItems reported an error in session %s and the item could not be found in the library: %v; inspect Zotero before retrying, do not blindly re-create", sessionID, saveCause),
				}
				return "conflict", detail, nil
			}
			if filingErr != nil {
				reason := itemCreateAppliedReason("connector", key)
				reason["session"] = sessionID
				reason["target"] = target
				reason["filing_failed"] = true
				reason["filing_error"] = filingErr.Error()
				reason["message"] = fmt.Sprintf("created via connector (session %s) but target %q filing failed: %v; retry filing only, do not re-create the item", sessionID, target, filingErr)
				return "applied", reason, nil
			}
			return "applied", itemCreateAppliedReason("connector", key), nil
		}
	}
}

// itemsCreateAllRecovered reports whether every reconciled SaveItems outcome
// resolved to a library key.
func itemsCreateAllRecovered(keys []string) bool {
	for _, key := range keys {
		if key == "" {
			return false
		}
	}
	return true
}

// connectorSaveAmbiguityError reports a SaveItems error whose outcome stayed
// unknown after reconciliation. It names the session and every key that could
// be resolved so a retry files or inspects the existing items instead of
// creating them a second time.
func connectorSaveAmbiguityError(sessionID string, keys []string, cause error) error {
	recovered := make([]string, 0, len(keys))
	for _, key := range keys {
		if key != "" {
			recovered = append(recovered, key)
		}
	}
	if len(recovered) > 0 {
		return fmt.Errorf("created %d item(s) via connector (session %s, keys %v) but SaveItems reported an error: %w; some items could not be confirmed -- inspect Zotero before retrying, do not re-create the batch", len(keys), sessionID, recovered, cause)
	}
	return fmt.Errorf("connector SaveItems reported an error (session %s) and no created item could be confirmed: %w; inspect Zotero before retrying, do not blindly re-create the batch", sessionID, cause)
}

// connectorFilingError reports a save that committed under a filing that did
// not. It names the session and every key that could be resolved so a retry can
// file the existing items instead of creating them a second time.
func connectorFilingError(count int, sessionID, target string, keys []string, cause error) error {
	recovered := make([]string, 0, len(keys))
	for _, key := range keys {
		if key != "" {
			recovered = append(recovered, key)
		}
	}
	if len(recovered) > 0 {
		return fmt.Errorf("created %d item(s) via connector (session %s, keys %v) but target %q filing failed: %w; retry filing only, do not re-create the item", count, sessionID, recovered, target, cause)
	}
	return fmt.Errorf("created %d item(s) via connector (session %s) but target %q filing failed: %w (items remain; retry filing, not creation)", count, sessionID, target, cause)
}

// itemsCreateKeysByIndex maps each accepted element's request index to the key
// Zotero assigned it. mutation.Run adopts a create's key from its Apply reason,
// and both the journal and write-through target that key -- so without the
// per-index mapping a batched Web create journals N ops that `journal undo`
// cannot reverse and mirrors nothing. Zotero reports the keys under "success"
// and repeats them on the full objects under "successful"; both are read
// because a response carrying only one of them is still a create.
func itemsCreateKeysByIndex(data json.RawMessage) map[string]string {
	var body struct {
		Success    map[string]string `json:"success"`
		Successful map[string]struct {
			Key string `json:"key"`
		} `json:"successful"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil
	}
	keys := make(map[string]string, len(body.Success)+len(body.Successful))
	for index, row := range body.Successful {
		if key := strings.TrimSpace(row.Key); key != "" {
			keys[index] = key
		}
	}
	for index, key := range body.Success {
		if key = strings.TrimSpace(key); key != "" {
			keys[index] = key
		}
	}
	return keys
}

// itemsCreateObjects reports the object array a batch body holds. Single item
// objects never reach this as maps: itemsCreateBody normalises them to a
// one-element array first, so a non-array here is genuinely not representable
// as a batch (nil, scalar, empty, or a non-object element).
func itemsCreateObjects(body any) ([]map[string]any, bool) {
	rawItems, ok := body.([]any)
	if !ok || len(rawItems) == 0 {
		return nil, false
	}
	items := make([]map[string]any, 0, len(rawItems))
	for _, raw := range rawItems {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, false
		}
		items = append(items, item)
	}
	return items, true
}

func itemsCreateHasCollections(items []map[string]any) bool {
	for _, item := range items {
		if connectorCollectionKeyFromItem(item) != "" {
			return true
		}
	}
	return false
}

// itemsCreatePreflightOps builds one mutation.Op per item in a JSON object
// array body, mirroring collectionCreateOps (collections_create.go) and the
// per-record preflight pattern used before other batched writes. Beyond
// running mutation.CheckGates before any network call -- CheckGates counts
// operations, not the Changes within them, so without this an N-item array
// would charge as a single planned op no matter how large N is -- these same
// ops carry the real write's per-item Apply closures below, so the pre-write
// gate and the journaled result always count the same N. Single item objects
// reach this already normalised to a one-element array by itemsCreateBody; a
// non-array body here (nil, scalar, empty, or a non-object element) still
// charges, and applies, as one op.
func itemsCreatePreflightOps(body any) []mutation.Op {
	items, ok := itemsCreateObjects(body)
	if !ok {
		return []mutation.Op{{
			ID:      "items.create:1",
			Kind:    "item_create",
			Changes: []mutation.Change{{Field: "item", Add: body}},
		}}
	}
	ops := make([]mutation.Op, 0, len(items))
	for i, item := range items {
		key := fmt.Sprintf("%d", i+1)
		if title, ok := item["title"].(string); ok && title != "" {
			key = title
		}
		ops = append(ops, mutation.Op{
			ID:      fmt.Sprintf("items.create:%d", i+1),
			Key:     key,
			Kind:    "item_create",
			Changes: []mutation.Change{{Field: "item", Add: item}},
		})
	}
	return ops
}
