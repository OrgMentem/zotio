package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"

	"zotio/internal/client"
)

// Zotero refuses a key-based write that carries no precondition ("Either
// If-Unmodified-Since-Version or object version property must be provided for
// key-based writes"). Two distinct hazards motivate the helpers below:
//
//  1. Dispatching a PATCH with an absent version (0) relays that opaque 428 to
//     the user instead of naming the real cause. Failing closed before the
//     request is the established pattern (see creators_audit_fix.go and
//     tags_rename.go).
//  2. Under hybrid routing, reads come from the Zotero desktop local API while
//     writes are routed to api.zotero.org. The two planes version objects
//     independently, so a local version is not a valid Web precondition: it
//     either spuriously conflicts or, when the numbers happen to coincide,
//     silently overwrites a concurrent Web edit. The precondition must be read
//     from the plane the write lands on.
//
// Both helpers send the precondition as an If-Unmodified-Since-Version header
// rather than a body `version` property. The header is canonical for key-based
// writes and avoids a body/header dual-precondition whose halves can disagree.

// writePlaneReader is the subset of *client.Client needed to resolve a
// precondition from the plane a write is routed to.
type writePlaneReader interface {
	GetFromWriteBaseWithVersionContext(ctx context.Context, path string, params map[string]string) (json.RawMessage, int, error)
}

// patchWithVersionGuard PATCHes path with an If-Unmodified-Since-Version
// precondition, refusing to dispatch when version is absent. Callers that
// already hold a write-plane version use this directly; callers whose version
// came from a read plane must use patchWithWritePlaneVersion instead.
func patchWithVersionGuard(c *client.Client, path string, body map[string]any, version int) (string, any, error) {
	if c == nil {
		err := fmt.Errorf("no API client available for %s", path)
		return "failed", err.Error(), err
	}
	if version <= 0 {
		err := fmt.Errorf("no write-plane version for %s; refusing to write without an If-Unmodified-Since-Version precondition", path)
		return "failed", err.Error(), err
	}
	headers := map[string]string{"If-Unmodified-Since-Version": strconv.Itoa(version)}
	if _, _, err := c.PatchWithHeaders(path, body, headers); err != nil {
		if client.IsAmbiguousWriteError(err) {
			current, observedVersion, readErr := c.GetFromWriteBaseWithVersion(path, nil)
			return classifyReconciledPatch(path, body, current, observedVersion, err, readErr)
		}
		return classifyWriteError(err)
	}
	return "applied", nil, nil
}

// patchWithWritePlaneVersion resolves path's version from the plane the write
// is routed to, then PATCHes under that precondition. Use this whenever the
// version a caller has in hand was read from a different plane (the local
// desktop API) than the one the write reaches.
func patchWithWritePlaneVersion(ctx context.Context, m apiMutator, path string, body map[string]any) (string, any, error) {
	if m == nil {
		err := fmt.Errorf("no API client available for %s", path)
		return "failed", err.Error(), err
	}
	reader, ok := m.(writePlaneReader)
	if !ok {
		err := fmt.Errorf("client for %s cannot read the write plane; refusing to write without an If-Unmodified-Since-Version precondition", path)
		return "failed", err.Error(), err
	}
	_, version, err := reader.GetFromWriteBaseWithVersionContext(ctx, path, nil)
	if err != nil {
		return classifyWriteError(err)
	}
	if version <= 0 {
		err := fmt.Errorf("no write-plane version for %s; refusing to write without an If-Unmodified-Since-Version precondition", path)
		return "failed", err.Error(), err
	}
	headers := map[string]string{"If-Unmodified-Since-Version": strconv.Itoa(version)}
	if _, _, err := m.PatchWithHeaders(path, body, headers); err != nil {
		if client.IsAmbiguousWriteError(err) {
			current, observedVersion, readErr := reader.GetFromWriteBaseWithVersionContext(ctx, path, nil)
			return classifyReconciledPatch(path, body, current, observedVersion, err, readErr)
		}
		return classifyWriteError(err)
	}
	return "applied", nil, nil
}

func putWithVersionGuard(c *client.Client, path string, body map[string]any, version int) (json.RawMessage, int, string, any, error) {
	if c == nil {
		err := fmt.Errorf("no API client available for %s", path)
		return nil, 0, "failed", err.Error(), err
	}
	if version <= 0 {
		err := fmt.Errorf("no write-plane version for %s; refusing to write without an If-Unmodified-Since-Version precondition", path)
		return nil, 0, "failed", err.Error(), err
	}
	headers := map[string]string{"If-Unmodified-Since-Version": strconv.Itoa(version)}
	data, statusCode, err := c.PutWithHeaders(path, body, headers)
	if err == nil {
		return data, statusCode, "applied", nil, nil
	}
	if !client.IsAmbiguousWriteError(err) {
		status, detail, classified := classifyWriteError(err)
		return nil, statusCode, status, detail, classified
	}
	current, observedVersion, readErr := c.GetFromWriteBaseWithVersion(path, nil)
	status, detail, reconciledErr := classifyReconciledPatch(path, body, current, observedVersion, err, readErr)
	if reconciledErr == nil {
		return current, http.StatusOK, status, detail, nil
	}
	return nil, statusCode, status, detail, reconciledErr
}

func deleteWithVersionGuard(c *client.Client, path string, version int) (json.RawMessage, int, string, any, error) {
	if c == nil {
		err := fmt.Errorf("no API client available for %s", path)
		return nil, 0, "failed", err.Error(), err
	}
	if version <= 0 {
		err := fmt.Errorf("no write-plane version for %s; refusing to write without an If-Unmodified-Since-Version precondition", path)
		return nil, 0, "failed", err.Error(), err
	}
	headers := map[string]string{"If-Unmodified-Since-Version": strconv.Itoa(version)}
	data, statusCode, err := c.DeleteWithHeaders(path, headers)
	if err == nil {
		return data, statusCode, "applied", nil, nil
	}
	if !client.IsAmbiguousWriteError(err) {
		status, detail, classified := classifyWriteError(err)
		return nil, statusCode, status, detail, classified
	}
	_, _, readErr := c.GetFromWriteBaseWithVersion(path, nil)
	var apiErr *client.APIError
	if errors.As(readErr, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil, http.StatusNoContent, "applied", map[string]any{"reconciled": true, "deleted": true}, nil
	}
	if readErr != nil {
		combined := fmt.Errorf("delete outcome for %s is ambiguous and reconciliation failed: %w", path, errors.Join(err, readErr))
		return nil, statusCode, "failed", combined.Error(), combined
	}
	status, detail, classified := classifyWriteError(err)
	return nil, statusCode, status, detail, classified
}

func classifyReconciledPatch(path string, body map[string]any, current json.RawMessage, observedVersion int, writeErr, readErr error) (string, any, error) {
	if readErr != nil {
		err := fmt.Errorf("write outcome for %s is ambiguous and reconciliation failed: %w", path, errors.Join(writeErr, readErr))
		return "failed", err.Error(), err
	}
	matched, err := requestedPatchFieldsMatch(current, body)
	if err != nil {
		err = fmt.Errorf("write outcome for %s is ambiguous and its current value cannot be compared: %w", path, errors.Join(writeErr, err))
		return "failed", err.Error(), err
	}
	if matched {
		return "applied", map[string]any{
			"reconciled":       true,
			"observed_version": observedVersion,
		}, nil
	}
	conflictErr := fmt.Errorf("write outcome for %s is ambiguous and the current value differs from the requested fields: %w", path, writeErr)
	return "conflict", map[string]any{
		"reconciled":       false,
		"observed_version": observedVersion,
	}, conflictErr
}

func requestedPatchFieldsMatch(current json.RawMessage, body map[string]any) (bool, error) {
	var currentObject map[string]any
	if err := json.Unmarshal(current, &currentObject); err != nil {
		return false, err
	}
	if data, ok := currentObject["data"].(map[string]any); ok {
		currentObject = data
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	var normalizedBody map[string]any
	if err := json.Unmarshal(bodyJSON, &normalizedBody); err != nil {
		return false, err
	}
	for field, wanted := range normalizedBody {
		observed, ok := currentObject[field]
		if !ok {
			return false, nil
		}
		switch field {
		case "tags":
			wantedTags, ok := normalizedTagSet(wanted)
			if !ok {
				return false, fmt.Errorf("invalid requested tags value")
			}
			observedTags, ok := normalizedTagSet(observed)
			if !ok || !reflect.DeepEqual(wantedTags, observedTags) {
				return false, nil
			}
		case "collections":
			wantedCollections, ok := normalizedStringSet(wanted)
			if !ok {
				return false, fmt.Errorf("invalid requested collections value")
			}
			observedCollections, ok := normalizedStringSet(observed)
			if !ok || !reflect.DeepEqual(wantedCollections, observedCollections) {
				return false, nil
			}
		default:
			if !reflect.DeepEqual(wanted, observed) {
				return false, nil
			}
		}
	}
	return true, nil
}

func normalizedStringSet(value any) ([]string, bool) {
	raw, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		s, ok := entry.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out, true
}

func normalizedTagSet(value any) ([]string, bool) {
	raw, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		tag, ok := entry.(map[string]any)
		if !ok {
			return nil, false
		}
		name, ok := tag["tag"].(string)
		if !ok {
			return nil, false
		}
		tagType := float64(0)
		if rawType, present := tag["type"]; present {
			tagType, ok = rawType.(float64)
			if !ok {
				return nil, false
			}
		}
		out = append(out, fmt.Sprintf("%s\x00%g", name, tagType))
	}
	sort.Strings(out)
	return out, true
}

// classifyWriteError maps a Zotero precondition rejection to a conflict status
// so callers can distinguish a losing optimistic write from a hard failure.
func classifyWriteError(err error) (string, any, error) {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusPreconditionFailed || apiErr.StatusCode == http.StatusPreconditionRequired) {
		return "conflict", apiErr.Body, err
	}
	return "failed", err.Error(), err
}
