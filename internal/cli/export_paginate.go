// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean roadmap-phase6 d27f99d4): support resumable header-free paginated snapshot exports.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"zotio/internal/client"
)

type exportCheckpoint struct {
	Path      string `json:"path"`
	NextStart int    `json:"next_start"`
	Fetched   int    `json:"fetched"`
	Done      bool   `json:"done"`
}

func readExportCheckpoint(file string) (exportCheckpoint, bool) {
	data, err := os.ReadFile(file)
	if err != nil {
		return exportCheckpoint{}, false
	}
	var cp exportCheckpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return exportCheckpoint{}, false
	}
	return cp, true
}

func writeExportCheckpoint(file string, cp exportCheckpoint) error {
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(file, data, 0o600)
}

func resumablePaginatedFetch(ctx context.Context, c *client.Client, path string, params map[string]string, pageSize, limit int, checkpointFile string, onPage func(page []json.RawMessage) error) (fetched int, err error) {
	if pageSize <= 0 {
		pageSize = 100
	} else if pageSize > 100 {
		pageSize = 100
	}

	start := 0
	if checkpointFile != "" {
		if cp, ok := readExportCheckpoint(checkpointFile); ok && cp.Path == path && !cp.Done {
			start = cp.NextStart
			fetched = cp.Fetched
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return fetched, err
		}

		thisLimit := pageSize
		if limit > 0 {
			remaining := limit - fetched
			if remaining <= 0 {
				return fetched, nil
			}
			if remaining < thisLimit {
				thisLimit = remaining
			}
		}

		params2 := make(map[string]string, len(params)+2)
		for key, value := range params {
			params2[key] = value
		}
		params2["start"] = strconv.Itoa(start)
		params2["limit"] = strconv.Itoa(thisLimit)

		data, err := c.Get(path, params2)
		if err != nil {
			return fetched, fmt.Errorf("fetching %s page at start %d: %w", path, start, classifyAPIError(err, &rootFlags{}))
		}

		var page []json.RawMessage
		if err := json.Unmarshal(data, &page); err != nil {
			return fetched, fmt.Errorf("decoding %s page at start %d: %w", path, start, err)
		}
		if len(page) > 0 {
			if err := onPage(page); err != nil {
				return fetched, err
			}
		}

		fetched += len(page)
		start += len(page)
		done := len(page) < thisLimit || (limit > 0 && fetched >= limit)
		if checkpointFile != "" {
			cp := exportCheckpoint{Path: path, NextStart: start, Fetched: fetched, Done: done}
			if err := writeExportCheckpoint(checkpointFile, cp); err != nil {
				return fetched, err
			}
		}
		if done {
			break
		}
	}

	return fetched, nil
}
