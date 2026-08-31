// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// support resumable header-free paginated snapshot exports.

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"zotio/internal/client"
)

type exportCheckpointSource struct {
	ReadBase string `json:"read_base"`
	Library  string `json:"library"`
	Profile  string `json:"profile,omitempty"`
}

type exportCheckpoint struct {
	Path      string                 `json:"path"`
	NextStart int                    `json:"next_start"`
	Fetched   int                    `json:"fetched"`
	Done      bool                   `json:"done"`
	Scope     string                 `json:"scope"`
	Source    exportCheckpointSource `json:"source"`
}

func exportReadSource(c *client.Client, profile string) exportCheckpointSource {
	readBase := strings.TrimRight(c.Plane(), "/")
	if parsed, err := url.Parse(readBase); err == nil {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.Fragment = ""
		readBase = strings.TrimRight(parsed.String(), "/")
	}
	library := "unscoped"
	if kind, id, ok := baseURLLibraryPrefix(readBase); ok {
		library = kind + ":" + id
	}
	return exportCheckpointSource{
		ReadBase: readBase,
		Library:  library,
		Profile:  strings.TrimSpace(profile),
	}
}

func normalizedExportPageSize(pageSize int) int {
	if pageSize <= 0 || pageSize > 100 {
		return 100
	}
	return pageSize
}

func exportCheckpointScope(source exportCheckpointSource, path string, params map[string]string, pageSize, limit int) string {
	canonicalParams := make(map[string]string, len(params))
	for key, value := range params {
		canonicalParams[key] = value
	}
	payload, _ := json.Marshal(struct {
		Source   exportCheckpointSource `json:"source"`
		Path     string                 `json:"path"`
		Params   map[string]string      `json:"params"`
		PageSize int                    `json:"page_size"`
		Limit    int                    `json:"limit"`
	}{
		Source:   source,
		Path:     path,
		Params:   canonicalParams,
		PageSize: pageSize,
		Limit:    limit,
	})
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%x", sum)
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

func resumablePaginatedFetch(ctx context.Context, c *client.Client, path string, params map[string]string, pageSize, limit int, checkpointFile, profile string, onPage func(page []json.RawMessage) error) (fetched int, err error) {
	pageSize = normalizedExportPageSize(pageSize)
	source := exportReadSource(c, profile)
	scope := exportCheckpointScope(source, path, params, pageSize, limit)
	start := 0
	if checkpointFile != "" {
		if cp, ok := readExportCheckpoint(checkpointFile); ok && !cp.Done {
			if cp.Path != path || cp.Source != source || cp.Scope != scope {
				return 0, fmt.Errorf("checkpoint scope does not match this export; remove the checkpoint or rerun without --resume")
			}
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
			cp := exportCheckpoint{Path: path, Scope: scope, Source: source, NextStart: start, Fetched: fetched, Done: done}
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
