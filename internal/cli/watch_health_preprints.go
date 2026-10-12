// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"zotio/internal/cliutil"
	"zotio/internal/store"
)

const (
	watchPreprintKind = "preprint_published"
	// watchPreprintDefaultRecheck is how long a "still a preprint" answer is
	// trusted. Journal-ref and DOI metadata reaches arXiv and CrossRef over
	// days to months, not between 5-minute sync cycles; a week keeps a
	// library of hundreds of preprints to a handful of provider requests a
	// day while still surfacing a publication within a week of its record
	// appearing.
	watchPreprintDefaultRecheck = 7 * 24 * time.Hour
	// watchPreprintMinRecheck floors the horizon: arXiv asks API clients to
	// stay near one request every few seconds, and a shorter horizon turns a
	// large library into a standing crawl.
	watchPreprintMinRecheck = time.Hour
	// watchPreprintMaxLookups bounds provider lookups per cycle so a first
	// scan or a horizon expiry spreads over cycles instead of one long burst.
	// Due preprints beyond it are reported in the skip and picked up next
	// cycle, oldest check first.
	watchPreprintMaxLookups = 50
	// watchPreprintMaxConsecutiveFailures ends a cycle's lookups when the
	// providers look down, instead of sending the rest of the budget into a
	// dead service. One bad identifier does not trip it.
	watchPreprintMaxConsecutiveFailures = 3
	// watchPreprintLookupPause matches the pacing of items preprint-check.
	watchPreprintLookupPause = 200 * time.Millisecond
	watchPreprintCacheName   = "watch-preprints.json"
	watchPreprintCacheSchema = 1
)

// parseWatchPreprintRecheck reads --health-preprint-recheck: empty for the
// default horizon, a Go duration, or a whole number of days ("7d") or weeks
// ("2w"), which time.ParseDuration does not accept.
func parseWatchPreprintRecheck(raw string) (time.Duration, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return watchPreprintDefaultRecheck, nil
	}
	invalid := func(why string) error {
		return usageErr(fmt.Errorf("invalid --health-preprint-recheck %q: %s", raw, why))
	}
	var d time.Duration
	switch {
	case strings.HasSuffix(text, "d"), strings.HasSuffix(text, "w"):
		unit := 24 * time.Hour
		if strings.HasSuffix(text, "w") {
			unit = 7 * 24 * time.Hour
		}
		n, err := strconv.Atoi(text[:len(text)-1])
		if err != nil || n <= 0 || time.Duration(n) > math.MaxInt64/unit {
			return 0, invalid("use a duration such as 168h, 7d, or 2w")
		}
		d = time.Duration(n) * unit
	default:
		parsed, err := time.ParseDuration(text)
		if err != nil {
			return 0, invalid("use a duration such as 168h, 7d, or 2w")
		}
		d = parsed
	}
	if d < watchPreprintMinRecheck {
		return 0, invalid("must be at least 1h")
	}
	return d, nil
}

// watchPreprintEntry is one provider answer for one arXiv ID. A published
// answer is final and never expires; a preprint answer is trusted until the
// recheck horizon passes.
type watchPreprintEntry struct {
	Published bool      `json:"published"`
	DOI       string    `json:"doi,omitempty"`
	Venue     string    `json:"venue,omitempty"`
	Year      int       `json:"year,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

func (e watchPreprintEntry) fresh(now time.Time, horizon time.Duration) bool {
	if e.Published {
		return true
	}
	// A check stamped in the future means the clock moved back; recheck.
	age := now.Sub(e.CheckedAt)
	return age >= 0 && age < horizon
}

type watchPreprintCacheFile struct {
	SchemaVersion int                           `json:"schema_version"`
	Entries       map[string]watchPreprintEntry `json:"entries"`
}

// watchPreprintCache persists successful provider answers by lowercased
// normalized arXiv ID under the zotio state directory. It is a regenerable
// memo: losing it costs provider requests, never a finding, so it takes the
// non-durable atomic write and no writer lock. Two watchers sharing one state
// directory can overwrite each other's newer answers, which only repeats a
// lookup. Failed lookups are never stored.
type watchPreprintCache struct {
	path    string
	entries map[string]watchPreprintEntry
	dirty   bool
}

// loadWatchPreprintCache reads the cache file. A missing file is an empty
// cache; an unreadable or incompatible one also yields an empty cache plus the
// reason, so the caller can report it instead of silently starting over.
func loadWatchPreprintCache(path string) (*watchPreprintCache, error) {
	cache := &watchPreprintCache{path: path, entries: map[string]watchPreprintEntry{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cache, nil
	}
	if err != nil {
		return cache, fmt.Errorf("reading %s: %w", path, err)
	}
	var file watchPreprintCacheFile
	if err := json.Unmarshal(data, &file); err != nil {
		cache.dirty = true
		return cache, fmt.Errorf("parsing %s: %w", path, err)
	}
	if file.SchemaVersion != watchPreprintCacheSchema {
		cache.dirty = true
		return cache, fmt.Errorf("%s has unsupported schema_version %d", path, file.SchemaVersion)
	}
	for id, entry := range file.Entries {
		cache.entries[id] = entry
	}
	return cache, nil
}

func (c *watchPreprintCache) set(id string, entry watchPreprintEntry) {
	c.entries[strings.ToLower(id)] = entry
	c.dirty = true
}

func (c *watchPreprintCache) get(id string) (watchPreprintEntry, bool) {
	entry, ok := c.entries[strings.ToLower(id)]
	return entry, ok
}

// save publishes the cache atomically when it changed. A failed save keeps the
// cache dirty so the next cycle tries again.
func (c *watchPreprintCache) save() error {
	if !c.dirty {
		return nil
	}
	data, err := json.Marshal(watchPreprintCacheFile{SchemaVersion: watchPreprintCacheSchema, Entries: c.entries})
	if err != nil {
		return err
	}
	if err := cliutil.AtomicWriteFile(c.path, append(data, '\n'), 0o600, 0o700); err != nil {
		return fmt.Errorf("writing %s: %w", c.path, err)
	}
	c.dirty = false
	return nil
}

// watchPreprintMonitor rechecks arXiv preprints for a published record across
// watch cycles. It lives for the watch process; the cache outlives it.
type watchPreprintMonitor struct {
	recheck time.Duration
	cache   *watchPreprintCache
	// cacheNote is a one-shot report of an unreadable cache file at startup.
	cacheNote string
	now       func() time.Time
	// pause separates provider lookups within a cycle.
	pause time.Duration
	// attempted records this process's last lookup per ID, success or not, so
	// a persistently failing ID rotates behind the others instead of
	// starving them.
	attempted map[string]time.Time
}

type watchPreprintOutcome struct {
	findings []Finding
	skip     *healthSkip
}

// enablePreprintCheck turns on the per-cycle preprint-published check, backed
// by the recheck cache under the state directory.
func (m *watchHealthMonitor) enablePreprintCheck(on bool, recheck time.Duration) error {
	if m == nil || !on {
		return nil
	}
	dir, err := cliutil.KindDir(cliutil.PathKindState)
	if err != nil {
		return preconditionErr(fmt.Errorf("--health-check-preprints needs the state directory for its recheck cache: %w", err))
	}
	cache, loadErr := loadWatchPreprintCache(filepath.Join(dir, watchPreprintCacheName))
	monitor := &watchPreprintMonitor{
		recheck:   recheck,
		cache:     cache,
		now:       time.Now,
		pause:     watchPreprintLookupPause,
		attempted: map[string]time.Time{},
	}
	if loadErr != nil {
		monitor.cacheNote = "ignored the unusable recheck cache and started empty: " + loadErr.Error()
	}
	m.preprints = monitor
	return nil
}

// withPreprintCheck appends the preprint_published check to a report. A store
// failure aborts the cycle like a failed retraction check, which leaves the
// baseline untouched; provider failures are skips and never an all-clear.
func (m *watchHealthMonitor) withPreprintCheck(ctx context.Context, db localQueryStore, scopeSet map[string]bool, report healthReport) (healthReport, error) {
	if m.preprints == nil {
		return report, nil
	}
	outcome, err := m.preprints.check(ctx, db, m.flags, scopeSet)
	if err != nil {
		return report, fmt.Errorf("health check %s: %w", watchPreprintKind, err)
	}
	report.Checks = append(report.Checks, healthCheckRun{Kind: watchPreprintKind, Ran: true, Count: len(outcome.findings)})
	report.Summary.add(sevInfo, len(outcome.findings))
	report.Summary.Total = report.Summary.Critical + report.Summary.High + report.Summary.Info
	report.allFindings = append(report.allFindings, outcome.findings...)
	report.Findings = append(report.Findings, outcome.findings...)
	sortHealthFindings(report.allFindings)
	sortHealthFindings(report.Findings)
	report.RemediationPlan = buildHealthRemediationPlan(report.allFindings)
	if outcome.skip != nil {
		report.Skipped = append(report.Skipped, *outcome.skip)
	}
	return report, nil
}

// watchPreprintLocalItems serves the candidate selection's /items queries from
// the synced mirror, so the monitor sees the same cohort as the rest of the
// health report and costs no Zotero API traffic per cycle.
type watchPreprintLocalItems struct {
	ctx context.Context
	db  *store.Store
}

func (g watchPreprintLocalItems) Get(path string, params map[string]string) (json.RawMessage, error) {
	data, handled, err := resolveLocalItemList(g.ctx, g.db, path, params)
	if err != nil {
		return nil, err
	}
	if !handled {
		return nil, fmt.Errorf("local store cannot serve %s", path)
	}
	return data, nil
}

// watchPreprintParentItem keeps bibliographic items. The candidate selection
// also returns child attachments of arXiv items (their titles and URLs name
// arXiv), and a published-DOI alert for an attachment key is not actionable.
func watchPreprintParentItem(item map[string]any) bool {
	switch zoteroString(item, "itemType") {
	case "attachment", "note", "annotation":
		return false
	}
	return zoteroString(item, "parentItem") == ""
}

// watchPreprintRemediated reports whether the item already records the
// published DOI, either in its DOI field or in the provenance line that
// items preprint-check fix appends to Extra, so a fixed item stops alerting.
func watchPreprintRemediated(item map[string]any, entry watchPreprintEntry) bool {
	doi := strings.ToLower(normalizeDOI(entry.DOI))
	if doi == "" {
		return false
	}
	if strings.ToLower(normalizeDOI(zoteroString(item, "DOI"))) == doi {
		return true
	}
	const marker = "published as doi:"
	for _, line := range strings.Split(strings.ToLower(zoteroString(item, "extra")), "\n") {
		for rest := line; ; {
			i := strings.Index(rest, marker)
			if i < 0 {
				break
			}
			rest = rest[i+len(marker):]
			token := rest
			if end := strings.IndexAny(token, " \t\r,;)"); end >= 0 {
				token = token[:end]
			}
			if strings.ToLower(normalizeDOI(strings.TrimRight(token, "."))) == doi {
				return true
			}
		}
	}
	return false
}

// lastTouch orders due IDs: never checked first, then the longest-unchecked.
func (p *watchPreprintMonitor) lastTouch(id string) time.Time {
	if at, ok := p.attempted[strings.ToLower(id)]; ok {
		return at
	}
	entry, _ := p.cache.get(id)
	return entry.CheckedAt
}

// dueIDs lists the distinct arXiv IDs of the cohort with no answer inside the
// horizon. Items sharing an ID share one lookup.
func (p *watchPreprintMonitor) dueIDs(cohort []map[string]any, now time.Time) []string {
	seen := map[string]bool{}
	var due []string
	for _, item := range cohort {
		id := extractArxivID(item)
		lower := strings.ToLower(id)
		if seen[lower] {
			continue
		}
		seen[lower] = true
		if entry, ok := p.cache.get(id); ok && entry.fresh(now, p.recheck) {
			continue
		}
		due = append(due, id)
	}
	sort.SliceStable(due, func(i, j int) bool {
		ti, tj := p.lastTouch(due[i]), p.lastTouch(due[j])
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return due[i] < due[j]
	})
	return due
}

// check rechecks the due preprints of the cohort and returns the findings
// the cache currently supports. The findings come from the cache, never from
// this cycle's lookups alone, so a failed lookup cannot clear a published
// preprint: it keeps its last known answer and the skip names the failure.
func (p *watchPreprintMonitor) check(ctx context.Context, db localQueryStore, flags *rootFlags, scopeSet map[string]bool) (watchPreprintOutcome, error) {
	candidates, err := fetchArxivPreprintCandidates(watchPreprintLocalItems{ctx: ctx, db: db.Store}, 0)
	if err != nil {
		return watchPreprintOutcome{}, err
	}
	cohort := make([]map[string]any, 0, len(candidates))
	for _, item := range candidates {
		if !watchPreprintParentItem(item) {
			continue
		}
		if scopeSet != nil && !scopeSet[zoteroString(item, "key")] {
			continue
		}
		cohort = append(cohort, item)
	}

	due := p.dueIDs(cohort, p.now())
	deferred := 0
	if len(due) > watchPreprintMaxLookups {
		deferred = len(due) - watchPreprintMaxLookups
		due = due[:watchPreprintMaxLookups]
	}

	var failures []string
	notTried := 0
	httpClient := &http.Client{Timeout: enrichTimeout(flags.timeout)}
	consecutive := 0
	for i, id := range due {
		if ctx.Err() != nil {
			break
		}
		if i > 0 && p.pause > 0 {
			if err := sleepWithContext(ctx, p.pause); err != nil {
				break
			}
		}
		match, found, err := lookupCrossrefArxiv(ctx, httpClient, id)
		at := p.now()
		p.attempted[strings.ToLower(id)] = at
		if err != nil {
			failures = append(failures, err.Error())
			consecutive++
			if consecutive >= watchPreprintMaxConsecutiveFailures {
				notTried = len(due) - i - 1
				break
			}
			continue
		}
		consecutive = 0
		entry := watchPreprintEntry{CheckedAt: at}
		if found {
			entry.Published, entry.DOI, entry.Venue, entry.Year = true, match.DOI, match.Venue, match.Year
		}
		p.cache.set(id, entry)
	}
	saveErr := p.cache.save()
	if err := ctx.Err(); err != nil {
		return watchPreprintOutcome{}, err
	}

	results := make([]preprintCheckResult, 0)
	for _, item := range cohort {
		id := extractArxivID(item)
		entry, ok := p.cache.get(id)
		if !ok || !entry.Published || watchPreprintRemediated(item, entry) {
			continue
		}
		results = append(results, preprintCheckResult{
			Key:     zoteroString(item, "key"),
			Title:   zoteroString(item, "title"),
			ArxivID: id,
			Status:  "published",
			DOI:     entry.DOI,
			Venue:   entry.Venue,
			Year:    entry.Year,
		})
	}

	var notes []string
	if p.cacheNote != "" {
		notes = append(notes, p.cacheNote)
		p.cacheNote = ""
	}
	if saveErr != nil {
		notes = append(notes, "the recheck cache was not saved, so a restart repeats these lookups: "+saveErr.Error())
	}
	return watchPreprintOutcome{
		findings: preprintCheckFindings(results),
		skip:     watchPreprintSkip(failures, notTried, deferred, notes),
	}, nil
}

// watchPreprintSkip describes everything the cycle could not verify, or nil
// when every due preprint was answered and the cache is sound. The first
// matching cause names the precondition.
func watchPreprintSkip(failures []string, notTried, deferred int, cacheNotes []string) *healthSkip {
	var details []string
	precondition := ""
	if len(failures) > 0 {
		precondition = "external_arxiv_crossref"
		details = append(details, fmt.Sprintf("arXiv/CrossRef preprint checking encountered %d lookup error(s); affected preprints keep their last known result and are retried next cycle: %s", len(failures), strings.Join(failures, "; ")))
	}
	if notTried > 0 {
		details = append(details, fmt.Sprintf("lookups stopped after %d consecutive failures; %d due preprint(s) were not tried", watchPreprintMaxConsecutiveFailures, notTried))
	}
	if deferred > 0 {
		if precondition == "" {
			precondition = "preprint_check_budget"
		}
		details = append(details, fmt.Sprintf("%d due preprint(s) were deferred to later cycles by the per-cycle limit of %d lookups", deferred, watchPreprintMaxLookups))
	}
	if len(cacheNotes) > 0 {
		if precondition == "" {
			precondition = "preprint_recheck_cache"
		}
		details = append(details, strings.Join(cacheNotes, "; "))
	}
	if len(details) == 0 {
		return nil
	}
	return &healthSkip{
		Kind:         watchPreprintKind,
		Precondition: precondition,
		Detail:       strings.Join(details, ". "),
		Remediation: []healthRemediation{
			{Action: "run_preprint_check", Command: "zotio items preprint-check"},
		},
	}
}
