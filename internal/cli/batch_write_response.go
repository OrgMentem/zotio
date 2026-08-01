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

type batchWriteFailure struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// decodeBatchWriteResponse leaves ordinary single-object responses alone because
// Zotero does not use the batch response shape for every successful POST.
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
	if err := json.Unmarshal(body["failed"], &response.Failed); err != nil {
		response.Failed = nil
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
		failure := failures[index]
		details = append(details, fmt.Sprintf("index %s: code %d: %s", index, failure.Code, failure.Message))
	}
	return fmt.Errorf("%s: batch write failed: %s", operation, strings.Join(details, "; "))
}
