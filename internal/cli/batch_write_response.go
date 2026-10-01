// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// batchWriteResponse describes the per-element outcome returned by Zotero batch writes.
type batchWriteResponse struct {
	Successful int
	Success    int
	Unchanged  int
	Failed     map[string]batchWriteFailure
}

// batchWriteFailure is one entry of a batch response's `failed` map. Malformed
// marks an entry that is not a {code, message} object with a non-zero code:
// Zotero rejected the object in some way, but the response does not say how,
// so its outcome is unknown and it must never be reported applied.
type batchWriteFailure struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Malformed bool   `json:"-"`
}

// detail renders the failure for the object at index, as reported to the
// operator.
func (f batchWriteFailure) detail(index string) string {
	if f.Malformed {
		return fmt.Sprintf("index %s: batch response carried a malformed failure entry; the outcome is unknown", index)
	}
	return fmt.Sprintf("index %s: code %d: %s", index, f.Code, f.Message)
}

// decodeBatchWriteResponse leaves ordinary single-object responses alone because
// Zotero does not use the batch response shape for every successful POST.
//
// Each failed entry is decoded on its own. Decoding the map in one call drops
// every failure when any entry is malformed, which would report the rejected
// objects applied; a malformed entry is kept with Malformed set instead.
func decodeBatchWriteResponse(data []byte) batchWriteResponse {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(data, &body); err != nil {
		return batchWriteResponse{}
	}

	response := batchWriteResponse{
		Successful: batchWriteEntryCount(body["successful"]),
		Success:    batchWriteEntryCount(body["success"]),
		Unchanged:  batchWriteEntryCount(body["unchanged"]),
	}
	var failed map[string]json.RawMessage
	if err := json.Unmarshal(body["failed"], &failed); err != nil {
		return response
	}
	if len(failed) > 0 {
		response.Failed = make(map[string]batchWriteFailure, len(failed))
	}
	for index, raw := range failed {
		var failure batchWriteFailure
		if err := json.Unmarshal(raw, &failure); err != nil || failure.Code == 0 {
			failure = batchWriteFailure{Malformed: true}
		}
		response.Failed[index] = failure
	}
	return response
}

func batchWriteEntryCount(data json.RawMessage) int {
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return 0
	}
	return len(entries)
}

func batchWriteFailuresError(operation string, failures map[string]batchWriteFailure) error {
	if len(failures) == 0 {
		return nil
	}

	indexes := make([]string, 0, len(failures))
	for index := range failures {
		indexes = append(indexes, index)
	}
	sort.Slice(indexes, func(i, j int) bool {
		return indexes[i] < indexes[j]
	})

	details := make([]string, 0, len(indexes))
	for _, index := range indexes {
		details = append(details, failures[index].detail(index))
	}
	return fmt.Errorf("%s: batch write failed: %s", operation, strings.Join(details, "; "))
}
