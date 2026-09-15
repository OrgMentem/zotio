// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
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
//     item's conflict.
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

	// err aggregates whatever the command should return as its own error.
	err error
}

func newBatchItemUpdater(c *client.Client, path, operation string, objects []map[string]any) *batchItemUpdater {
	return &batchItemUpdater{
		c:         c,
		path:      path,
		operation: operation,
		objects:   objects,
		done:      make(map[int]bool),
		failed:    make(map[int]batchWriteFailure),
		transport: make(map[int]error),
	}
}

// chunkStart returns the first index of the request that carries index.
func chunkStart(index int) int {
	return index - index%zoteroBatchWriteMax
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
	failure, ok := b.failed[index]
	if !ok {
		return "applied", nil, nil
	}
	detail := map[string]any{"code": failure.Code, "message": failure.Message}
	// 412 is a lost precondition and 428 a missing one; both are the
	// conflict the per-item path reports, so they must not be flattened
	// into a generic failure.
	if failure.Code == 412 || failure.Code == 428 {
		return "conflict", detail, nil
	}
	return "failed", detail, nil
}

func (b *batchItemUpdater) send(start int) {
	end := min(start+zoteroBatchWriteMax, len(b.objects))
	data, _, err := b.c.Post(b.path, b.objects[start:end])
	if err != nil {
		b.transport[start] = err
		if b.err == nil {
			b.err = err
		}
		return
	}
	for index, failure := range decodeBatchWriteResponse(data).Failed {
		offset, convErr := strconv.Atoi(index)
		if convErr != nil || offset < 0 || start+offset >= end {
			// An index the response invented cannot be attributed to an
			// item. Surface it as the run's error rather than dropping it.
			if b.err == nil {
				b.err = fmt.Errorf("%s: batch response reported unattributable index %q", b.operation, index)
			}
			continue
		}
		b.failed[start+offset] = failure
	}
}

// Err returns the aggregate error for the command, or nil when every object
// was accepted. Per-object rejections are already reported per item, so this
// marks the run degraded rather than replacing that detail.
func (b *batchItemUpdater) Err() error {
	if b.err != nil {
		return b.err
	}
	if len(b.failed) == 0 {
		return nil
	}
	return degradedErr(batchWriteFailuresError(b.operation, b.indexedFailures()))
}

// indexedFailures re-keys absolute object indexes for the shared message
// builder, which reports the position the operator can count to in their own
// input list.
func (b *batchItemUpdater) indexedFailures() map[string]batchWriteFailure {
	out := make(map[string]batchWriteFailure, len(b.failed))
	for index, failure := range b.failed {
		out[strconv.Itoa(index)] = failure
	}
	return out
}
