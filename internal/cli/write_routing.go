// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Hybrid write routing. The Zotero local API is read-only, so when the CLI is
// pointed at it, mutating commands route to the Web API (api.zotero.org) while reads
// stay local. resolveWebWriteBase builds the Web API base for the configured key;
// fetchZoteroUserID resolves the numeric user ID once (cached to config).

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zotio/internal/config"
	"zotio/internal/connector"
)

// zoteroWebAPIBase is the Zotero Web API root. A package var so tests can point the
// write-routing resolver at an httptest server.
var zoteroWebAPIBase = "https://api.zotero.org"

var connectorPing = func(ctx context.Context, c *connector.Client) error {
	return c.Ping(ctx)
}

// connectorBaseFromAPIBase maps the configured local data API base
// (http://localhost:23119/api/users/0) to the desktop Connector API root.
func connectorBaseFromAPIBase(baseURL string) (string, bool) {
	if !isLocalZoteroAPI(baseURL) {
		return "", false
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", false
	}
	return strings.ToLower(parsed.Scheme) + "://" + parsed.Host + "/connector", true
}

// newConnector returns a desktop Connector API client for a local Zotero base URL.
func (f *rootFlags) newConnector() (*connector.Client, error) {
	cfg, err := config.Load(f.configPath)
	if err != nil {
		return nil, configErr(err)
	}
	if f.group != "" {
		cfg.BaseURL = rewriteLibraryPrefix(cfg.BaseURL, f.group)
	}
	base, ok := connectorBaseFromAPIBase(cfg.BaseURL)
	if !ok {
		return nil, fmt.Errorf("the desktop connector is only available with a local Zotero base URL")
	}
	return connector.New(base, f.timeout), nil
}

// resolveCreateVia chooses the item-creation write route. --via affects only
// create operations; updates/deletes/tags keep using the Web API write path.
func (f *rootFlags) resolveCreateVia(ctx context.Context, collectionRequested bool) (string, error) {
	switch f.via {
	case "", "auto":
		cfg, err := config.Load(f.configPath)
		if err != nil {
			return "", configErr(err)
		}
		if f.group != "" {
			// PATCH: desktop connector has no group parameter; keep group writes on Web API.
			return "web", nil
		}
		if !isLocalZoteroAPI(cfg.BaseURL) {
			return "web", nil
		}
		conn, err := f.newConnector()
		if err != nil {
			return "web", nil
		}
		if err := connectorPing(ctx, conn); err != nil {
			return "web", nil
		}
		return "connector", nil
	case "web":
		return "web", nil
	case "connector":
		if f.group != "" {
			return "", fmt.Errorf("--via connector cannot honor --group; use --via web for group writes")
		}
		conn, err := f.newConnector()
		if err != nil {
			return "", fmt.Errorf("--via connector requires a local Zotero base URL")
		}
		if err := connectorPing(ctx, conn); err != nil {
			return "", fmt.Errorf("--via connector set but desktop Zotero is not reachable on :23119: %w", err)
		}
		return "connector", nil
	default:
		return "", fmt.Errorf("invalid --via value %q: must be auto, connector, or web", f.via)
	}
}

// resolveWebWriteBase returns the Web API base URL writes should target, or "" when
// no key is configured (writes then hit the read-only local API and its guard). For
// a group the path needs only the group ID; for a personal library it needs the
// numeric user ID, taken from config/ZOTERO_USER_ID or a one-time keys/current
// lookup that is persisted to config.
func resolveWebWriteBase(cfg *config.Config, group string, timeout time.Duration) (string, error) {
	if cfg == nil || cfg.AuthHeader() == "" {
		return "", nil
	}
	if group != "" {
		return zoteroWebAPIBase + "/groups/" + group, nil
	}
	id := cfg.UserID
	if id == "" {
		resolved, err := fetchZoteroUserID(cfg, timeout)
		if err != nil {
			return "", err
		}
		id = resolved
		_ = cfg.SaveUserID(id) // best-effort cache; re-resolved next run if it fails
	}
	return zoteroWebAPIBase + "/users/" + id, nil
}

// fetchZoteroUserID resolves the numeric Zotero user ID for the configured key via
// the Web API keys/current endpoint.
func fetchZoteroUserID(cfg *config.Config, timeout time.Duration) (string, error) {
	req, err := http.NewRequest(http.MethodGet, zoteroWebAPIBase+"/keys/current", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Zotero-API-Key", cfg.AuthHeader())
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return "", fmt.Errorf("resolving Zotero user ID: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolving Zotero user ID: keys/current returned HTTP %d", resp.StatusCode)
	}
	var meta struct {
		UserID json.Number `json:"userID"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return "", fmt.Errorf("parsing keys/current response: %w", err)
	}
	if meta.UserID.String() == "" {
		return "", fmt.Errorf("keys/current returned no userID")
	}
	return meta.UserID.String(), nil
}

// keyGroupWriteAccess reports whether the configured API key can WRITE to the
// given group, read from /keys/current access. This is the accurate writability
// signal for `groups inspect`: the group's editing *policy* (data.libraryEditing)
// is near-always non-empty and does not reflect the key's per-group permission.
// known=false when there is no key or the lookup fails, so callers report
// "unknown" rather than over-claiming write access.
// PATCH(glean review P1): key-permission write verdict for groups inspect.
func keyGroupWriteAccess(cfg *config.Config, timeout time.Duration, groupID string) (canWrite bool, known bool) {
	if cfg == nil || cfg.AuthHeader() == "" {
		return false, false
	}
	req, err := http.NewRequest(http.MethodGet, zoteroWebAPIBase+"/keys/current", nil)
	if err != nil {
		return false, false
	}
	req.Header.Set("Zotero-API-Key", cfg.AuthHeader())
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, false
	}
	var payload struct {
		Access struct {
			Groups map[string]struct {
				Write bool `json:"write"`
			} `json:"groups"`
		} `json:"access"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return false, false
	}
	if g, ok := payload.Access.Groups[groupID]; ok {
		return g.Write, true
	}
	if all, ok := payload.Access.Groups["all"]; ok {
		return all.Write, true
	}
	return false, true
}
