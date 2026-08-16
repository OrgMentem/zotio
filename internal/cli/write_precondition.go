package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
		return classifyWriteError(err)
	}
	return "applied", nil, nil
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
