// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"zotio/internal/cache"
)

const providerCacheTTL = 7 * 24 * time.Hour

const (
	providerCOCI            = "coci"
	providerSemanticScholar = "semantic_scholar"
	providerCrossRef        = "crossref"
	providerOpenAlex        = "openalex"
)

type providerJSONCache struct {
	store  *cache.Store
	bypass bool
}

func newProviderJSONCache(noCache bool) *providerJSONCache {
	if noCache {
		return &providerJSONCache{bypass: true}
	}
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		return &providerJSONCache{bypass: true}
	}
	return &providerJSONCache{store: cache.New(filepath.Join(dir, "zotio", "providers"), providerCacheTTL)}
}

func (pc *providerJSONCache) get(provider, rawURL string) (json.RawMessage, bool) {
	if pc == nil || pc.bypass || pc.store == nil {
		return nil, false
	}
	return pc.store.Get(provider + "|" + rawURL)
}

func (pc *providerJSONCache) set(provider, rawURL string, body json.RawMessage) {
	if pc == nil || pc.bypass || pc.store == nil {
		return
	}
	pc.store.Set(provider+"|"+rawURL, body)
}

func getCappedProviderJSON(ctx context.Context, httpClient *http.Client, provider string, rawURL string, pc *providerJSONCache, out any) error {
	if cached, ok := pc.get(provider, rawURL); ok {
		if err := json.Unmarshal(cached, out); err == nil {
			return nil
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", crossrefContentType)
	req.Header.Set("User-Agent", crossrefUserAgent)
	resp, err := externalHTTPClient(httpClient, false).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := readCappedExternalBody(resp.Body, 4<<20)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	pc.set(provider, rawURL, json.RawMessage(body))
	return json.Unmarshal(body, out)
}
