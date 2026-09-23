// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"zotio/internal/client"
)

// zoteroBatchWriteMax is Zotero's hard ceiling on objects per write request,
// documented for both multi-object create and multi-object update. Sending
// more is rejected outright, so the updater splits its work at this size.
const zoteroBatchWriteMax = 50

// batchItemUpdater turns N per-item PATCHes into ceil(N/50) array POSTs while
// keeping one mutation.Op per item, so every item still reports its own
// status, its own conflict, and its own server message.
//
// Two contract differences from the per-item path, both irreducible:
//
//   - The precondition travels as each object's body `version` rather than an
//     If-Unmodified-Since-Version header. The header carries one value, and a
//     batch spans many items at different versions. Per-object `version` is
//     the documented mechanism and is still a real precondition: a stale
//     object comes back in `failed` with code 412 and is reported as that
//     item's conflict. The write goes through Client.PostVersionedObjects so
//     the request keeps the 429 retry and limiter back-off that a header
//     precondition would have earned it.
//   - Fail-fast cannot hold. Every object in a chunk reaches Zotero in one
//     request, so an early rejection cannot un-send its batch-mates. Callers
//     must run with ContinueOnError, exactly as collections create does.
//
// The version each object carries is the one read while planning, not a fresh
// apply-time read. That is what removes the second request per item, and it
// widens the window in which a concurrent edit can land. A concurrent edit
// then loses the precondition and is reported as a conflict, never as a
// silent overwrite.
type batchItemUpdater struct {
	c         *client.Client
	path      string
	operation string

	// objects is index-aligned with the caller's ops.
	objects []map[string]any

	done      map[int]bool              // chunk start index -> executed
	failed    map[int]batchWriteFailure // absolute object index -> failure
	transport map[int]error             // chunk start index -> request failure
	// unattributable marks a chunk whose response cannot prove its objects'
	// outcomes: it named an index that cannot belong to the chunk, or it was
	// not the batch envelope at all. Every object in that chunk has an unknown
	// outcome.
	unattributable map[int]bool

	// err aggregates whatever the command should return as its own error.
	err error
}

func newBatchItemUpdater(c *client.Client, path, operation string, objects []map[string]any) *batchItemUpdater {
	return &batchItemUpdater{
		c:              c,
		path:           path,
		operation:      operation,
		objects:        objects,
		done:           make(map[int]bool),
		failed:         make(map[int]batchWriteFailure),
		transport:      make(map[int]error),
		unattributable: make(map[int]bool),
	}
}

// chunkStart returns the first index of the request that carries index.
func chunkStart(index int) int {
	return index - index%zoteroBatchWriteMax
}

// dispatched reports whether the request carrying index has already been sent.
// A cancelled run must not claim such an item was never attempted.
func (b *batchItemUpdater) dispatched(index int) bool {
	if index < 0 || index >= len(b.objects) {
		return false
	}
	start := chunkStart(index)
	if !b.done[start] {
		return false
	}
	// The client preserves ambiguity once a mutating request reaches its
	// transport. A plain context error means cancellation before dispatch
	// (or after a definitive rejection), so untouched siblings are retryable.
	err := b.transport[start]
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return client.IsAmbiguousWriteError(err)
	}
	return true
}

// outcome reports one object's result, sending its chunk on first use. The
// engine calls this once per op, in order, so the first op of each chunk pays
// for the request and the rest read the decoded response.
func (b *batchItemUpdater) outcome(index int) (string, any, error) {
	if index < 0 || index >= len(b.objects) {
		err := fmt.Errorf("batch index %d out of range", index)
		return "failed", err.Error(), err
	}
	start := chunkStart(index)
	if !b.done[start] {
		b.done[start] = true
		b.send(start)
	}
	if transportErr := b.transport[start]; transportErr != nil {
		return "failed", transportErr.Error(), transportErr
	}
	if failure, ok := b.failed[index]; ok {
		detail := map[string]any{"code": failure.Code, "message": failure.Message}
		// 412 is a lost precondition and 428 a missing one; both are the
		// conflict the per-item path reports, so they must not be flattened
		// into a generic failure.
		if failure.Code == 412 || failure.Code == 428 {
			return "conflict", detail, nil
		}
		return "failed", detail, nil
	}
	if b.unattributable[start] {
		// The response named an index this request cannot own, so the absence
		// of a failure for this object proves nothing. Reporting "applied"
		// here would turn an unparseable response into a success envelope.
		return "failed", "batch response could not be attributed; this item's outcome is unknown", nil
	}
	return "applied", nil, nil
}

func (b *batchItemUpdater) send(start int) {
	end := min(start+zoteroBatchWriteMax, len(b.objects))
	data, _, err := b.c.PostVersionedObjects(b.path, b.objects[start:end])
	if err != nil {
		b.transport[start] = err
		if b.err == nil {
			b.err = err
		}
		return
	}
	// A 2xx without the batch envelope proves nothing: it may be a proxy
	// error page, a singleton object, or truncated JSON. Decoding only Failed
	// would read all of those as zero failures and report the whole chunk
	// applied, so the envelope must be verified first.
	if envErr := checkBatchEnvelope(data, end-start); envErr != nil {
		b.unattributable[start] = true
		if b.err == nil {
			b.err = fmt.Errorf("%s: %w; the outcome of the %d item(s) in that request is unknown", b.operation, envErr, end-start)
		}
		return
	}
	resp := decodeBatchWriteResponse(data)
	// checkBatchEnvelope already proved the union of claimed indices covers the
	// chunk, so this loop only distributes failures. The range guard stays as
	// defence in depth: a response that contradicts the verified envelope
	// must fail the chunk, never poison the failure map.
	for index, failure := range resp.Failed {
		offset, convErr := strconv.Atoi(index)
		// Compare inside the chunk: start+offset would overflow for an
		// offset near MaxInt and wrap past this bound.
		if convErr != nil || offset < 0 || offset >= end-start {
			b.unattributable[start] = true
			if b.err == nil {
				b.err = fmt.Errorf("%s: batch response reported unattributable index %q; the outcome of the %d item(s) in that request is unknown", b.operation, index, end-start)
			}
			continue
		}
		b.failed[start+offset] = failure
	}
}

// checkBatchEnvelope verifies that a 2xx batch response proves the per-index
// contract for a chunk of want objects: a JSON object whose successful,
// success, unchanged and failed maps are themselves objects whose claimed
// indices cover exactly the chunk-relative indices 0..want-1 in union.
//
// A create response legitimately carries the same index in both success (the
// assigned key) and successful (the full object), so that pair is one claim,
// not a double claim. Any other overlap -- for example the same index in
// both successful and failed -- is a conflicting claim and is rejected.
//
// Anything else — malformed JSON, a singleton object, a proxy error page, or
// partial coverage — leaves at least one outcome unproven, so the caller must
// mark the chunk unattributable rather than report its objects applied.
func checkBatchEnvelope(data []byte, want int) error {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(data, &body); err != nil {
		return fmt.Errorf("batch response is not valid JSON: %w", err)
	}
	present := false
	for _, field := range []string{"successful", "success", "unchanged", "failed"} {
		if raw, ok := body[field]; ok && string(raw) != "null" {
			present = true
			break
		}
	}
	if !present {
		return fmt.Errorf("batch response is not a batch envelope: missing successful/success/unchanged/failed")
	}
	seen := make(map[int]string, want)
	for _, field := range []string{"successful", "success", "unchanged", "failed"} {
		raw, ok := body[field]
		if !ok || string(raw) == "null" {
			continue
		}
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			return fmt.Errorf("batch response field %q is not an object", field)
		}
		for key := range entries {
			offset, convErr := strconv.Atoi(key)
			if convErr != nil || offset < 0 || offset >= want {
				return fmt.Errorf("batch response reported unattributable index %q", key)
			}
			if prev, dup := seen[offset]; dup {
				if (prev == "success" && field == "successful") || (prev == "successful" && field == "success") {
					continue
				}
				return fmt.Errorf("batch response attributed index %q twice (%s and %s)", key, prev, field)
			}
			seen[offset] = field
		}
	}
	if len(seen) != want {
		return fmt.Errorf("batch response attributed %d of %d object(s)", len(seen), want)
	}
	return nil
}

// Err returns a request-level failure that the engine's generic "mutation
// incomplete" cannot express: a transport failure, or a response whose
// indices could not be attributed.
//
// Per-object rejections are deliberately NOT aggregated here. Each one is
// already reported as that item's own status and carries the server's own
// message in its reason, which names the item rather than a position in an
// array the operator never sees. Wrapping them again would also relabel a
// 412 as a write failure and downgrade a conflict from exit 1 to the generic
// degraded exit 13 — the contract measured live for the per-item path in
// dev/field-report-2026-08-30-conflict-contract-live.md. Aggregating was also
// unbounded: 50 server-controlled messages per chunk, every chunk, joined
// into one string.
func (b *batchItemUpdater) Err() error {
	return b.err
}
