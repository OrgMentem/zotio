// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package update discovers newer zotio releases without collecting user data.
package update

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultReleasesURL = "https://api.github.com/repos/OrgMentem/zotio/releases/latest"
	// ReleasesPageURL is the stable browser URL for zotio releases.
	ReleasesPageURL = "https://github.com/OrgMentem/zotio/releases/latest"
	cacheName       = "update-cache.json"
	checkInterval   = 24 * time.Hour
)

// Info describes the latest zotio release known to the checker.
type Info struct {
	LatestVersion string
	URL           string
	CheckedAt     time.Time
}

// Options configures a Checker. ReleasesURL, Client, and Now make checks
// deterministic in tests; production callers normally use New.
type Options struct {
	DataDir     string
	ReleasesURL string
	Client      *http.Client
	Now         func() time.Time
}

// Checker caches public release metadata in zotio's cache directory.
type Checker struct {
	dataDir     string
	releasesURL string
	client      *http.Client
	now         func() time.Time
	mu          sync.Mutex
}

type cacheEntry struct {
	ETag          string    `json:"etag,omitempty"`
	LatestVersion string    `json:"latest_version,omitempty"`
	URL           string    `json:"url,omitempty"`
	CheckedAt     time.Time `json:"checked_at,omitempty"`
}

type release struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

// New creates a checker for the public Zotio GitHub releases endpoint.
func New(dataDir string) *Checker {
	return NewWithOptions(Options{DataDir: dataDir})
}

// NewWithOptions creates a checker with explicit transport and clock seams.
func NewWithOptions(options Options) *Checker {
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	releasesURL := options.ReleasesURL
	if releasesURL == "" {
		releasesURL = defaultReleasesURL
	}
	return &Checker{
		dataDir:     options.DataDir,
		releasesURL: releasesURL,
		client:      client,
		now:         now,
	}
}

// Check returns cached release metadata or refreshes it with one anonymous GET.
// Network, cache, status, and decoding failures are deliberately soft: callers
// receive any prior cached result (or nil), never an error to surface to users.
func (c *Checker) Check(ctx context.Context) (*Info, error) {
	if c == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	cached := c.readCache()
	now := c.now().UTC()
	if checkedRecently(cached.CheckedAt, now) {
		return cached.info(), nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.releasesURL, nil)
	if err != nil {
		return cached.info(), nil
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if cached.ETag != "" {
		req.Header.Set("If-None-Match", cached.ETag)
	}
	response, err := c.client.Do(req)
	if err != nil {
		return cached.info(), nil
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusNotModified {
		if cached.info() == nil {
			return nil, nil
		}
		cached.CheckedAt = now
		_ = c.writeCache(cached)
		return cached.info(), nil
	}
	if response.StatusCode != http.StatusOK {
		return cached.info(), nil
	}

	var latest release
	if err := json.NewDecoder(response.Body).Decode(&latest); err != nil {
		return cached.info(), nil
	}
	latest.TagName = strings.TrimPrefix(latest.TagName, "v")
	if latest.TagName == "" || latest.HTMLURL == "" {
		return cached.info(), nil
	}

	cached.ETag = response.Header.Get("ETag")
	cached.LatestVersion = latest.TagName
	cached.URL = latest.HTMLURL
	cached.CheckedAt = now
	_ = c.writeCache(cached)
	return cached.info(), nil
}

// IsNewer reports whether latest is newer than current. Development builds do
// not have a release version and must never be described as behind.
func IsNewer(latest, current string) bool {
	if IsDevelopmentVersion(current) {
		return false
	}
	return compareVersion(latest, current) > 0
}

// IsDevelopmentVersion reports whether version is zotio's unstamped build
// default. It is exported so callers can avoid an unnecessary request entirely.
func IsDevelopmentVersion(version string) bool {
	switch strings.ToLower(strings.TrimSpace(version)) {
	case "", "dev", "devel", "development":
		return true
	default:
		return false
	}
}

// UpgradeHint returns the channel-appropriate update instruction.
func UpgradeHint(executable, releaseURL string) string {
	if isHomebrewExecutable(executable) {
		return "brew upgrade zotio"
	}
	return releaseURL
}

func compareVersion(left, right string) int {
	parse := func(value string) [3]int {
		var parts [3]int
		value = strings.TrimPrefix(value, "v")
		value = strings.SplitN(value, "-", 2)[0]
		value = strings.SplitN(value, "+", 2)[0]
		for i, raw := range strings.SplitN(value, ".", 3) {
			parts[i], _ = strconv.Atoi(raw)
		}
		return parts
	}
	a, b := parse(left), parse(right)
	for i := range a {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

func isHomebrewExecutable(executable string) bool {
	clean := filepath.Clean(executable)
	prefixes := []string{
		"/opt/homebrew",
		"/usr/local",
		"/home/linuxbrew/.linuxbrew",
	}
	for _, prefix := range prefixes {
		if isWithin(clean, prefix) {
			return true
		}
	}
	return false
}

func isWithin(path, prefix string) bool {
	rel, err := filepath.Rel(prefix, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (c *Checker) cachePath() string {
	return filepath.Join(c.dataDir, cacheName)
}

func (c *Checker) readCache() cacheEntry {
	data, err := os.ReadFile(c.cachePath())
	if err != nil {
		return cacheEntry{}
	}
	var cached cacheEntry
	if json.Unmarshal(data, &cached) != nil {
		return cacheEntry{}
	}
	return cached
}

func (c *Checker) writeCache(cached cacheEntry) error {
	data, err := json.Marshal(cached)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.dataDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(c.dataDir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.dataDir, ".update-cache-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, c.cachePath())
}

func (cached cacheEntry) info() *Info {
	if cached.LatestVersion == "" || cached.URL == "" {
		return nil
	}
	return &Info{
		LatestVersion: cached.LatestVersion,
		URL:           cached.URL,
		CheckedAt:     cached.CheckedAt,
	}
}

func checkedRecently(then, now time.Time) bool {
	return !then.IsZero() && now.Sub(then) < checkInterval
}
