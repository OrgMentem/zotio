// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// watchHealthMonitor tracks health findings across successful watch sync cycles.
type watchHealthMonitor struct {
	enabled  bool
	preset   string
	kinds    []string
	webhook  string
	flags    *rootFlags
	previous map[string]Finding
	baseline bool
}

// watchHealthWebhookPayload is the drift notification contract for --health-webhook.
type watchHealthWebhookPayload struct {
	CycleAt       time.Time     `json:"cycle_at"`
	Preset        string        `json:"preset"`
	New           []Finding     `json:"new"`
	ResolvedCount int           `json:"resolved_count"`
	Totals        healthSummary `json:"totals"`
}

// newWatchHealthMonitor normalizes watch health flags against the library-health preset registry.
func newWatchHealthMonitor(flags *rootFlags, enabled bool, presetRaw string, webhook string) (*watchHealthMonitor, error) {
	preset := strings.ToLower(strings.TrimSpace(presetRaw))
	if preset == "" {
		preset = "quick"
	}
	kinds, ok := healthPresets[preset]
	if !ok {
		return nil, usageErr(fmt.Errorf("invalid --health-for %q: must be quick, citation, systematic-review, or all", presetRaw))
	}
	webhook = strings.TrimSpace(webhook)
	if enabled && webhook != "" {
		if err := validateExternalHTTPURL(webhook, false); err != nil {
			return nil, usageErr(fmt.Errorf("invalid --health-webhook: %w", err))
		}
	}
	return &watchHealthMonitor{
		enabled:  enabled,
		preset:   preset,
		kinds:    kinds,
		webhook:  webhook,
		flags:    flags,
		previous: map[string]Finding{},
	}, nil
}

// run performs best-effort health reporting without affecting watch sync liveness.
func (m *watchHealthMonitor) run(ctx context.Context, cmd *cobra.Command, cycleAt time.Time) {
	if m == nil || !m.enabled || ctx.Err() != nil {
		return
	}
	report, err := m.report(ctx)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "[health] %s check error: %v\n", cycleAt.Format(time.RFC3339), err)
		return
	}

	current := watchHealthFindingSet(report.Findings)
	newFindings := make([]Finding, 0)
	resolvedCount := 0
	if !m.baseline {
		m.baseline = true
		fmt.Fprintf(cmd.OutOrStdout(), "[health] baseline %s total=%d critical=%d high=%d info=%d skipped=%d\n", m.preset, report.Summary.Total, report.Summary.Critical, report.Summary.High, report.Summary.Info, len(report.Skipped))
	} else {
		for _, f := range report.Findings {
			if _, ok := m.previous[watchHealthFindingKey(f)]; !ok {
				newFindings = append(newFindings, f)
				fmt.Fprintf(cmd.OutOrStdout(), "[health] new %s %s %s %q\n", f.Severity, f.Kind, watchHealthFindingDisplayKey(f), watchHealthFindingTitle(f))
			}
		}
		for key := range m.previous {
			if _, ok := current[key]; !ok {
				resolvedCount++
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "[health] resolved_count %d\n", resolvedCount)
	}
	m.previous = current

	if m.webhook != "" {
		m.deliverWebhook(ctx, cmd, cycleAt, newFindings, resolvedCount, report.Summary)
	}
}

// report reuses library-health internals over the local synced store.
func (m *watchHealthMonitor) report(ctx context.Context) (healthReport, error) {
	rawDB, err := openStoreForRead(ctx, "zotio")
	if err != nil {
		return healthReport{}, fmt.Errorf("opening database: %w", err)
	}
	if rawDB == nil {
		return healthReport{}, fmt.Errorf("library health: local store is not synced; run 'zotio sync' first")
	}
	defer rawDB.Close()
	db := localQueryStore{Store: rawDB}

	var syncedAt *time.Time
	if _, lastSynced, _, err := db.GetSyncState("items"); err == nil && !lastSynced.IsZero() {
		ls := lastSynced
		syncedAt = &ls
	}
	healthCtx := &healthContext{
		src:    FindingSource{Kind: "local", SyncedAt: syncedAt},
		preset: m.preset,
		flags:  m.flags,
	}
	return assembleHealthReport(db, healthCtx, m.preset, m.kinds, "", scopeResult{All: true, Expr: "library"})
}

// deliverWebhook posts the compact drift payload using the shared delivery webhook conventions.
func (m *watchHealthMonitor) deliverWebhook(ctx context.Context, cmd *cobra.Command, cycleAt time.Time, newFindings []Finding, resolvedCount int, totals healthSummary) {
	payload := watchHealthWebhookPayload{
		CycleAt:       cycleAt,
		Preset:        m.preset,
		New:           newFindings,
		ResolvedCount: resolvedCount,
		Totals:        totals,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "[health] %s webhook payload error: %v\n", cycleAt.Format(time.RFC3339), err)
		return
	}
	if err := deliverWebhook(ctx, m.webhook, body, false); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "[health] %s webhook delivery error: %v\n", cycleAt.Format(time.RFC3339), err)
	}
}

// watchHealthFindingSet indexes findings by the stable health taxonomy identity.
func watchHealthFindingSet(findings []Finding) map[string]Finding {
	out := make(map[string]Finding, len(findings))
	for _, f := range findings {
		out[watchHealthFindingKey(f)] = f
	}
	return out
}

// watchHealthFindingKey preserves (kind,item_key), with grouped evidence keys for aggregate findings.
func watchHealthFindingKey(f Finding) string {
	if f.ItemKey != "" {
		return f.Kind + "\x00item\x00" + f.ItemKey
	}
	group := sqlStringValue(f.Evidence["group"])
	value := sqlStringValue(f.Evidence["value"])
	if group != "" || value != "" {
		return f.Kind + "\x00group\x00" + group + "\x00" + value
	}
	canonical := sqlStringValue(f.Evidence["canonical"])
	if canonical != "" {
		return f.Kind + "\x00canonical\x00" + canonical
	}
	data, err := json.Marshal(f.Evidence)
	if err == nil && len(data) > 0 {
		return f.Kind + "\x00evidence\x00" + string(data)
	}
	return f.Kind
}

// watchHealthFindingDisplayKey turns the diff identity into a concise human token.
func watchHealthFindingDisplayKey(f Finding) string {
	if f.ItemKey != "" {
		return f.ItemKey
	}
	group := sqlStringValue(f.Evidence["group"])
	value := sqlStringValue(f.Evidence["value"])
	if group != "" || value != "" {
		if value == "" {
			return group
		}
		if group == "" {
			return value
		}
		return group + ":" + value
	}
	if canonical := sqlStringValue(f.Evidence["canonical"]); canonical != "" {
		return canonical
	}
	return "-"
}

// watchHealthFindingTitle supplies a quoted label for item and grouped health lines.
func watchHealthFindingTitle(f Finding) string {
	if f.Title != "" {
		return f.Title
	}
	switch f.Kind {
	case "duplicate_candidates":
		group := sqlStringValue(f.Evidence["group"])
		value := sqlStringValue(f.Evidence["value"])
		if group != "" || value != "" {
			return strings.Trim(group+"="+value, "=")
		}
	case "tag_drift":
		if canonical := sqlStringValue(f.Evidence["canonical"]); canonical != "" {
			return "canonical tag " + canonical
		}
	}
	return ""
}
