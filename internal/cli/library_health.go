// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(glean roadmap-phase1): flagship composite library-health report — the
// read-only diagnostic from notes/roadmap.md. Composes the existing audit
// primitives (citekey/duplicate/missing-*/tag-drift/broken-attachment) into one
// ranked, finding-typed report with --for presets, a --fail-on CI gate (exit 11),
// and the precondition contract (live-only checks refuse loudly, never silently).

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/client"
)

const (
	sevCritical = "critical"
	sevHigh     = "high"
	sevInfo     = "info"
)

func severityRank(s string) int {
	switch s {
	case sevCritical:
		return 3
	case sevHigh:
		return 2
	case sevInfo:
		return 1
	default:
		return 0
	}
}

// failOnRank maps a --fail-on threshold to the minimum severity rank that trips
// the gate. "info" and legacy "any" trip on info (rank 1) and above.
func failOnRank(failOn string) int {
	switch failOn {
	case sevCritical:
		return 3
	case sevHigh:
		return 2
	case sevInfo, "any":
		return 1
	default:
		return 0
	}
}

// healthSource records where a finding's data came from and how fresh it is, so
// an agent can decide whether to trust it (local reads may be stale).
type healthSource struct {
	Kind     string     `json:"kind"`
	SyncedAt *time.Time `json:"synced_at,omitempty"`
}

type healthRecommendedAction struct {
	Command string `json:"command,omitempty"`
	Text    string `json:"text,omitempty"`
}

// healthFinding is the stable finding taxonomy (notes/roadmap.md). Identity is
// (kind, item_key) for per-item findings; grouped findings carry detail in
// Evidence and leave ItemKey empty.
type healthFinding struct {
	Kind              string                   `json:"kind"`
	Severity          string                   `json:"severity"`
	ItemKey           string                   `json:"item_key,omitempty"`
	Title             string                   `json:"title,omitempty"`
	Evidence          map[string]any           `json:"evidence,omitempty"`
	Source            healthSource             `json:"source"`
	Autofixable       bool                     `json:"autofixable"`
	RecommendedAction *healthRecommendedAction `json:"recommended_action,omitempty"`
}

type healthRemediation struct {
	Action  string `json:"action"`
	Command string `json:"command,omitempty"`
	Text    string `json:"text,omitempty"`
}

// healthSkip is a loud, machine-actionable notice that a check did not run
// because a precondition was unmet — never a silent omission.
type healthSkip struct {
	Kind         string              `json:"kind"`
	Precondition string              `json:"precondition"`
	Detail       string              `json:"detail"`
	Remediation  []healthRemediation `json:"remediation"`
}

type healthCheckRun struct {
	Kind    string `json:"kind"`
	Ran     bool   `json:"ran"`
	Count   int    `json:"count"`
	Skipped bool   `json:"skipped,omitempty"`
}

type healthScope struct {
	Expr     string     `json:"expr"`
	Source   string     `json:"source"`
	SyncedAt *time.Time `json:"synced_at,omitempty"`
	Items    int        `json:"items"`
}

type healthSummary struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Info     int `json:"info"`
	Total    int `json:"total"`
}

func (s *healthSummary) add(severity string, n int) {
	switch severity {
	case sevCritical:
		s.Critical += n
	case sevHigh:
		s.High += n
	case sevInfo:
		s.Info += n
	}
}

type healthGate struct {
	FailOn string `json:"fail_on"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// healthFreshness records the --require-fresh verdict: how old the local data is
// versus the allowed maximum, and whether that makes the report uncertifiable.
// PATCH(glean roadmap-phase2): freshness gate.
type healthFreshness struct {
	RequiredMaxAgeSeconds int    `json:"required_max_age_seconds"`
	AgeSeconds            int    `json:"age_seconds,omitempty"`
	Stale                 bool   `json:"stale"`
	Reason                string `json:"reason,omitempty"`
}

// healthRemediationPlanStep is the actionable plan derived from findings. It is
// intentionally preview-first: commands omit --yes and delegate to existing
// fixers. Scoped steps carry exact keys for `--keys-from -`; broad steps say why
// they cannot yet be scoped.
// PATCH(glean roadmap-phase3): safe remediation plan, no new write path.
type healthRemediationPlanStep struct {
	Kind    string   `json:"kind"`
	Command string   `json:"command"`
	Keys    []string `json:"keys,omitempty"`
	Count   int      `json:"count"`
	Scoped  bool     `json:"scoped"`
	Preview bool     `json:"preview"`
	Notes   string   `json:"notes,omitempty"`
}

type healthReport struct {
	SchemaVersion   int                         `json:"schema_version"`
	Scope           healthScope                 `json:"scope"`
	Preset          string                      `json:"preset"`
	Checks          []healthCheckRun            `json:"checks"`
	Findings        []healthFinding             `json:"findings"`
	Skipped         []healthSkip                `json:"skipped,omitempty"`
	Summary         healthSummary               `json:"summary"`
	RemediationPlan []healthRemediationPlanStep `json:"remediation_plan,omitempty"`
	Gate            *healthGate                 `json:"gate,omitempty"`
	Freshness       *healthFreshness            `json:"freshness,omitempty"`
	// PATCH(action-arc): baseline diff metadata is emitted only when --baseline is supplied.
	Baseline *healthBaselineReport `json:"baseline,omitempty"`

	// PATCH(action-arc): keep untruncated findings and the new-finding gate threshold out of JSON.
	allFindings  []healthFinding
	baselineGate string
}

// PATCH(marketing-heroes): shields.io endpoint badge payload and verdict mapping.
type healthBadge struct {
	SchemaVersion int    `json:"schemaVersion"`
	Label         string `json:"label"`
	Message       string `json:"message"`
	Color         string `json:"color"`
}

func healthBadgeForReport(report healthReport, label string) healthBadge {
	badge := healthBadge{SchemaVersion: 1, Label: label, Message: "healthy", Color: "brightgreen"}
	if report.Freshness != nil && report.Freshness.Stale {
		badge.Message = "stale — sync needed"
		badge.Color = "lightgrey"
		return badge
	}
	if report.Gate != nil && report.Gate.Status == "indeterminate" {
		badge.Message = "setup required"
		badge.Color = "orange"
		return badge
	}
	// PATCH(action-arc): baseline badges tell the delta story once an existing baseline was read.
	if report.Baseline != nil && report.Baseline.Established {
		newSummary := healthSummaryForFindings(report.Baseline.New)
		switch {
		case newSummary.Total == 0:
			badge.Message = "no new findings"
			badge.Color = "brightgreen"
		case report.baselineGate != "" && gateCrossed(newSummary, report.baselineGate):
			badge.Message = healthBadgeSeverityMessage(newSummary)
			badge.Color = "red"
		default:
			badge.Message = fmt.Sprintf("%d new findings", newSummary.Total)
			badge.Color = "yellow"
		}
		return badge
	}
	if report.Gate != nil {
		switch report.Gate.Status {
		case "failed":
			badge.Message = healthBadgeSeverityMessage(report.Summary)
			badge.Color = "red"
			return badge
		case "indeterminate":
			badge.Message = "setup required"
			badge.Color = "orange"
			return badge
		}
	}
	if report.Summary.Total > 0 {
		badge.Message = fmt.Sprintf("%d findings", report.Summary.Total)
		badge.Color = "yellow"
	}
	return badge
}

func healthBadgeSeverityMessage(summary healthSummary) string {
	parts := make([]string, 0, 3)
	for _, severity := range []struct {
		name  string
		count int
	}{
		{name: sevCritical, count: summary.Critical},
		{name: sevHigh, count: summary.High},
		{name: sevInfo, count: summary.Info},
	} {
		if severity.count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", severity.count, severity.name))
		}
	}
	if len(parts) == 0 {
		return "gate failed"
	}
	return strings.Join(parts, ", ")
}

func printHealthBadge(cmd *cobra.Command, badge healthBadge) error {
	data, err := json.Marshal(badge)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
	return err
}

// healthContext threads per-run state (provenance, limit, live-check opt-in) and
// memoizes the shared citekey scan so it runs once even when both citekey checks
// are in the preset.
type healthContext struct {
	src         healthSource
	preset      string
	limit       int
	verifyFiles bool
	// PATCH(marketing-heroes-2): opt-in CrossRef retraction check switch.
	checkRetractions bool
	flags            *rootFlags
	requireFresh     time.Duration
	citekeyRows      []citekeyConflictRow
	citekeyLoaded    bool
}

type healthCheckRunner func(db localQueryStore, ctx *healthContext) ([]healthFinding, *healthSkip, error)

type healthCheck struct {
	kind     string
	severity string
	run      healthCheckRunner
}

// healthCheckRegistry is the single source of truth for the available checks,
// their severities, and how to run them. Presets pick a subset by kind.
func healthCheckRegistry() []healthCheck {
	return []healthCheck{
		{kind: "citekey_conflict", severity: sevCritical, run: runCitekeyConflict},
		{kind: "citekey_missing", severity: sevHigh, run: runCitekeyMissing},
		{kind: "duplicate_candidates", severity: sevHigh, run: runDuplicateCandidates},
		{kind: "missing_citation", severity: sevHigh, run: itemCheckRunner(
			func(db localQueryStore) ([]map[string]any, error) { return queryCitationIncompleteItems(db, 0) },
			"missing_citation", sevHigh, false,
			&healthRecommendedAction{Text: "Add the missing core citation fields (creators, title, date, venue) in Zotero"})},
		{kind: "missing_doi", severity: sevHigh, run: itemCheckRunner(
			func(db localQueryStore) ([]map[string]any, error) { return queryMissingDOIItems(db, 0, "") },
			"missing_doi", sevHigh, true,
			&healthRecommendedAction{Command: "zotio items enrich --missing-doi --keys-from -"})},
		{kind: "missing_pdf", severity: sevHigh, run: itemCheckRunner(
			func(db localQueryStore) ([]map[string]any, error) { return queryMissingPDFItems(db, "", 0, "") },
			"missing_pdf", sevHigh, true,
			&healthRecommendedAction{Command: "zotio items enrich --missing-pdf --keys-from -"})},
		{kind: "missing_abstract", severity: sevInfo, run: itemCheckRunner(
			func(db localQueryStore) ([]map[string]any, error) { return queryMissingAbstractItems(db, 0, "") },
			"missing_abstract", sevInfo, true,
			&healthRecommendedAction{Command: "zotio items enrich --missing-abstract --keys-from -"})},
		{kind: "missing_tags", severity: sevInfo, run: itemCheckRunner(
			func(db localQueryStore) ([]map[string]any, error) { return queryMissingTagsItems(db, 0) },
			"missing_tags", sevInfo, false, nil)},
		{kind: "tag_drift", severity: sevHigh, run: runTagDrift},
		{kind: "broken_attachment_file", severity: sevCritical, run: runBrokenAttachmentFile},
		// PATCH(marketing-heroes-2): gate DOI-bearing items against CrossRef retraction notices.
		{kind: "retracted_item", severity: sevCritical, run: runRetractedItem},
	}
}

// healthPresets defines the "--for" intent selectors. They are tool-curated
// check-sets, distinct from the user-defined --profile flag bundles.
var healthPresets = map[string][]string{
	"quick":             {"citekey_conflict", "duplicate_candidates", "broken_attachment_file"},
	"citation":          {"citekey_missing", "citekey_conflict", "missing_citation", "duplicate_candidates"},
	"systematic-review": {"duplicate_candidates", "missing_abstract", "missing_pdf", "broken_attachment_file"},
	"all": {
		"citekey_conflict", "citekey_missing", "duplicate_candidates", "missing_citation",
		"missing_doi", "missing_pdf", "missing_abstract", "missing_tags", "tag_drift",
		"broken_attachment_file", "retracted_item",
	},
}

// healthPresetFailOn is the default gate threshold per preset; an explicit
// --fail-on overrides it.
var healthPresetFailOn = map[string]string{
	"quick":             "",
	"citation":          sevHigh,
	"systematic-review": sevHigh,
	"all":               "",
}

func newLibraryHealthCmd(flags *rootFlags) *cobra.Command {
	var flagFor string
	var flagFailOn string
	var flagLimit int
	var flagVerifyFiles bool
	// PATCH(marketing-heroes-2): opt in to the live CrossRef retraction health check.
	var flagCheckRetractions bool
	var flagScope string
	var flagRequireFresh time.Duration
	// PATCH(marketing-heroes): shields.io badge output controls.
	var flagBadge bool
	var flagBadgeLabel string
	// PATCH(action-arc): baseline-diff gating and report sidecar output for CI actions.
	var flagBaseline string
	var flagWriteBaseline string
	var flagFailOnNew string
	var flagReport string

	cmd := &cobra.Command{
		Use:   "health",
		Short: "Composite read-only library health report with a CI gate",
		Long: `Run a ranked, finding-typed health report over the locally synced library.

Composes the existing audit primitives (citekey conflicts, duplicates, missing
metadata, tag drift, broken attachments) into one report. Pick what "ready" means
with --for:

  --for citation           manuscript/bibliography readiness (citekeys, core fields, duplicates)
  --for systematic-review  PRISMA screening corpus (duplicates, screenable metadata, full text)
  --for quick   (default)  anything obviously broken (citekey conflicts, duplicates, attachments)
  --for all                every check

Gate CI with --fail-on critical|high|info (or legacy any) (exit 11 when the bar is not met).

The broken-attachment and retraction checks are live checks that need Zotero
desktop or CrossRef network access respectively; pass --verify-files and/or
--check-retractions to run them. When a gate-relevant check can't run because
its precondition is unmet, the command refuses loudly (exit 9) rather than passing.`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			// PATCH(marketing-heroes): the badge is already JSON, so refuse ambiguous output modes.
			if flagBadge && flags.asJSON {
				return usageErr(fmt.Errorf("--badge cannot be combined with --json"))
			}

			preset := strings.ToLower(strings.TrimSpace(flagFor))
			if preset == "" {
				preset = "quick"
			}
			kinds, ok := healthPresets[preset]
			if !ok {
				return usageErr(fmt.Errorf("invalid --for %q: must be quick, citation, systematic-review, or all", flagFor))
			}
			// PATCH(marketing-heroes-2): retracted_item is opt-in (network). It lives
			// only in the "all" preset (loud skip without the flag, like
			// broken_attachment_file); --check-retractions injects it into any other
			// preset so the default citation gate stays deterministic offline.
			if flagCheckRetractions && !slices.Contains(kinds, "retracted_item") {
				kinds = append(slices.Clone(kinds), "retracted_item")
			}

			failOn := strings.ToLower(strings.TrimSpace(flagFailOn))
			if flagFailOn == "" {
				failOn = healthPresetFailOn[preset]
			}
			// PATCH(action-arc): "none" disables the absolute gate (overrides the
			// preset default) so baseline mode can gate on new findings only.
			if failOn == "none" {
				failOn = ""
			}
			switch failOn {
			case "", sevCritical, sevHigh, sevInfo, "any":
			default:
				return usageErr(fmt.Errorf("invalid --fail-on %q: must be critical, high, info, any, or none", flagFailOn))
			}
			// PATCH(action-arc): --fail-on-new shares the health gate threshold vocabulary.
			failOnNew := strings.ToLower(strings.TrimSpace(flagFailOnNew))
			switch failOnNew {
			case "", sevCritical, sevHigh, sevInfo, "any":
			default:
				return usageErr(fmt.Errorf("invalid --fail-on-new %q: must be critical, high, info, or any", flagFailOnNew))
			}
			baselinePath := strings.TrimSpace(flagBaseline)
			if failOnNew != "" && baselinePath == "" {
				return usageErr(fmt.Errorf("--fail-on-new requires --baseline"))
			}
			reportPath := strings.TrimSpace(flagReport)
			if cmd.Flags().Changed("report") && reportPath == "" {
				return usageErr(fmt.Errorf("--report requires a non-empty path"))
			}
			writeBaselinePath := strings.TrimSpace(flagWriteBaseline)
			if cmd.Flags().Changed("write-baseline") && writeBaselinePath == "" {
				return usageErr(fmt.Errorf("--write-baseline requires a non-empty path"))
			}
			if flagLimit < 0 {
				return usageErr(fmt.Errorf("--limit must be zero or greater"))
			}

			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			if rawDB == nil {
				// PATCH(marketing-heroes): badge CI must fail loudly before a local sync exists.
				if flagBadge {
					if err := printHealthBadge(cmd, healthBadge{SchemaVersion: 1, Label: flagBadgeLabel, Message: "not synced", Color: "lightgrey"}); err != nil {
						return err
					}
					return preconditionErr(fmt.Errorf("library health: local store is not synced; run 'zotio sync' first"))
				}
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first.")
				return nil
			}
			defer rawDB.Close()
			db := localQueryStore{rawDB}

			var syncedAt *time.Time
			if _, lastSynced, _, e := db.GetSyncState("items"); e == nil && !lastSynced.IsZero() {
				ls := lastSynced
				syncedAt = &ls
			}
			ctx := &healthContext{
				src:         healthSource{Kind: "local", SyncedAt: syncedAt},
				preset:      preset,
				limit:       flagLimit,
				verifyFiles: flagVerifyFiles,
				// PATCH(marketing-heroes-2): thread CrossRef retraction opt-in into health runners.
				checkRetractions: flagCheckRetractions,
				flags:            flags,
				requireFresh:     flagRequireFresh,
			}

			scope := scopeResult{All: true, Expr: "library"}
			if strings.TrimSpace(flagScope) != "" {
				spec, perr := parseScopeSpec(flagScope)
				if perr != nil {
					return usageErr(perr)
				}
				scope, err = resolveScope(db, spec)
				if err != nil {
					return err
				}
				if scope.Precondition != "" {
					return preconditionErr(fmt.Errorf("scope %q needs the %s precondition (Zotero desktop / local API); open Zotero and enable Settings -> Advanced -> 'Allow other applications', then re-run", scope.Expr, scope.Precondition))
				}
			}

			report, err := assembleHealthReport(db, ctx, preset, kinds, failOn, scope)
			if err != nil {
				return err
			}
			// PATCH(action-arc): apply baseline diff before any output mode or report sidecar is rendered.
			report.baselineGate = failOnNew
			if baselinePath != "" {
				if err := applyHealthBaseline(&report, baselinePath); err != nil {
					return err
				}
			}
			if writeBaselinePath != "" {
				if err := writeHealthBaseline(writeBaselinePath, preset, healthCurrentFindings(report)); err != nil {
					return err
				}
			}
			if reportPath != "" {
				if err := writeHealthReportFile(reportPath, report); err != nil {
					return err
				}
			}

			// PATCH(marketing-heroes): badge mode renders only the shields endpoint payload; exit mapping below is unchanged.
			if flagBadge {
				if err := printHealthBadge(cmd, healthBadgeForReport(report, flagBadgeLabel)); err != nil {
					return err
				}
			} else if flags.asJSON {
				data, err := json.Marshal(report)
				if err != nil {
					return err
				}
				if err := printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags); err != nil {
					return err
				}
			} else {
				printHealthReport(cmd, report)
			}
			if ferr := healthFreshnessExitError(report); ferr != nil {
				return ferr
			}
			if gerr := healthGateExitError(report); gerr != nil {
				return gerr
			}
			return healthNewGateExitError(report)
		},
	}
	cmd.Flags().StringVar(&flagFor, "for", "quick", "Check preset: quick, citation, systematic-review, all")
	cmd.Flags().StringVar(&flagFailOn, "fail-on", "", "Exit 11 if findings reach this severity: critical, high, info/any; none disables the gate (default: the preset's)")
	cmd.Flags().IntVar(&flagLimit, "limit", 0, "Max findings listed per kind (0 = all); also caps the live attachment scan")
	cmd.Flags().BoolVar(&flagVerifyFiles, "verify-files", false, "Run the live broken-attachment check (needs Zotero desktop running)")
	// PATCH(marketing-heroes-2): expose the opt-in CrossRef retraction health check.
	cmd.Flags().BoolVar(&flagCheckRetractions, "check-retractions", false, "Run the live CrossRef retraction check (network; DOI-bearing items)")
	cmd.Flags().StringVar(&flagScope, "scope", "", "Limit to a cohort: collection:KEY | tag:NAME | item:KEY | query:TEXT | saved-search:KEY (default: whole library)")
	cmd.Flags().DurationVar(&flagRequireFresh, "require-fresh", 0, "Refuse (exit 12) when the local store is staler than this (e.g. 24h); 0 = disabled")
	// PATCH(marketing-heroes): register shields.io badge flags on the health command.
	cmd.Flags().BoolVar(&flagBadge, "badge", false, "Emit a shields.io endpoint JSON badge instead of the report")
	cmd.Flags().StringVar(&flagBadgeLabel, "badge-label", "bibliography", "Label for the shields.io endpoint badge")
	// PATCH(action-arc): register baseline diff, baseline persistence, and report output flags.
	cmd.Flags().StringVar(&flagBaseline, "baseline", "", "Read health finding identities from this baseline JSON; missing file establishes a baseline")
	cmd.Flags().StringVar(&flagWriteBaseline, "write-baseline", "", "Write current health finding identities to this baseline JSON after checks")
	cmd.Flags().StringVar(&flagFailOnNew, "fail-on-new", "", "Exit 11 if new baseline-diff findings reach this severity: critical, high, info/any (requires --baseline)")
	cmd.Flags().StringVar(&flagReport, "report", "", "Write the full JSON health report to this file in addition to stdout/badge output")
	return cmd
}

// assembleHealthReport runs the selected checks against db, ranks the findings,
// and records the gate verdict in report.Gate. The returned error is only a hard
// check failure that should abort before output; the gate outcome is mapped to
// an exit code separately by healthGateExitError after the report is rendered.
func assembleHealthReport(db localQueryStore, ctx *healthContext, preset string, kinds []string, failOn string, scope scopeResult) (healthReport, error) {
	report := healthReport{
		SchemaVersion: 1,
		Preset:        preset,
		Scope: healthScope{
			Expr:     scope.Expr,
			Source:   "local",
			SyncedAt: ctx.src.SyncedAt,
			Items:    scopeItemCount(db, scope),
		},
	}

	var scopeSet map[string]bool
	if !scope.All {
		scopeSet = make(map[string]bool, len(scope.Keys))
		for _, k := range scope.Keys {
			scopeSet[k] = true
		}
	}

	gateBlockedBySkip := false
	for _, chk := range selectHealthChecks(kinds) {
		findings, skip, err := chk.run(db, ctx)
		if err != nil {
			return report, fmt.Errorf("health check %s: %w", chk.kind, err)
		}
		if skip != nil {
			report.Skipped = append(report.Skipped, *skip)
			report.Checks = append(report.Checks, healthCheckRun{Kind: chk.kind, Skipped: true})
			if failOn != "" && severityRank(chk.severity) >= failOnRank(failOn) {
				gateBlockedBySkip = true
			}
			continue
		}
		findings = filterFindingsByScope(findings, scopeSet)
		report.Checks = append(report.Checks, healthCheckRun{Kind: chk.kind, Ran: true, Count: len(findings)})
		// PATCH(action-arc): baseline writes use the complete finding identity set, not the display limit.
		report.Summary.add(chk.severity, len(findings))
		report.allFindings = append(report.allFindings, findings...)
		report.Findings = append(report.Findings, truncateFindings(findings, ctx.limit)...)
	}
	report.Summary.Total = report.Summary.Critical + report.Summary.High + report.Summary.Info
	sortHealthFindings(report.allFindings)
	sortHealthFindings(report.Findings)
	report.RemediationPlan = buildHealthRemediationPlan(report.Findings)

	if failOn != "" {
		gate := &healthGate{FailOn: failOn}
		switch {
		case gateBlockedBySkip:
			gate.Status = "indeterminate"
			gate.Reason = "a gate-relevant check was skipped (precondition unmet); cannot certify health"
		case gateCrossed(report.Summary, failOn):
			gate.Status = "failed"
			gate.Reason = fmt.Sprintf("found findings at or above %q severity", failOn)
		default:
			gate.Status = "passed"
		}
		report.Gate = gate
	}

	if ctx.requireFresh > 0 {
		fr := &healthFreshness{RequiredMaxAgeSeconds: int(ctx.requireFresh.Seconds())}
		switch ctx.src.SyncedAt {
		case nil:
			fr.Stale = true
			fr.Reason = "local store has never been synced"
		default:
			age := time.Since(*ctx.src.SyncedAt)
			fr.AgeSeconds = int(age.Seconds())
			if age > ctx.requireFresh {
				fr.Stale = true
				fr.Reason = fmt.Sprintf("local data is %s old, older than the required %s", durationAgo(age), ctx.requireFresh)
			}
		}
		report.Freshness = fr
	}
	return report, nil
}

// scopeItemCount reports how many items the scope covers: the whole library
// when unscoped, else the number of resolved keys.
func scopeItemCount(db localQueryStore, scope scopeResult) int {
	if scope.All {
		return countCiteableItems(db)
	}
	return len(scope.Keys)
}

// filterFindingsByScope keeps only findings that reference an item in scope.
// Per-item findings match on item_key; grouped findings (e.g. duplicate
// candidates) match if any member key is in scope. Findings with no item keys
// (e.g. library-wide tag drift) are dropped from a scoped run. A nil set means
// "whole library" and returns the findings unchanged.
func filterFindingsByScope(findings []healthFinding, scopeSet map[string]bool) []healthFinding {
	if scopeSet == nil {
		return findings
	}
	out := make([]healthFinding, 0, len(findings))
	for _, f := range findings {
		if f.ItemKey != "" {
			if scopeSet[f.ItemKey] {
				out = append(out, f)
			}
			continue
		}
		if keys, ok := f.Evidence["keys"].([]string); ok {
			for _, k := range keys {
				if scopeSet[k] {
					out = append(out, f)
					break
				}
			}
		}
	}
	return out
}

// healthGateExitError maps a finished report's gate verdict to the process exit
// error: 11 (gateErr) when the bar was not met, 9 (preconditionErr) when a
// gate-relevant check could not run, nil otherwise. Called after the report is
// rendered so the report still prints on a non-zero exit.
func healthGateExitError(report healthReport) error {
	if report.Gate == nil {
		return nil
	}
	switch report.Gate.Status {
	case "indeterminate":
		return preconditionErr(fmt.Errorf("library health: %s", report.Gate.Reason))
	case "failed":
		return gateErr(fmt.Errorf("library health gate failed: %s", report.Gate.Reason))
	default:
		return nil
	}
}

// PATCH(action-arc): map --fail-on-new to the same quality-gate exit constructor as --fail-on.
func healthNewGateExitError(report healthReport) error {
	if report.Baseline == nil || report.baselineGate == "" {
		return nil
	}
	newSummary := healthSummaryForFindings(report.Baseline.New)
	if !gateCrossed(newSummary, report.baselineGate) {
		return nil
	}
	return gateErr(fmt.Errorf("library health gate failed: found new findings at or above %q severity", report.baselineGate))
}

// healthFreshnessExitError maps a stale --require-fresh verdict to exit 12. The
// remedy is `sync` + retry, distinct from a quality gate (11) or a precondition
// (9). Returns nil when freshness was not required or the data is fresh enough.
func healthFreshnessExitError(report healthReport) error {
	if report.Freshness == nil || !report.Freshness.Stale {
		return nil
	}
	return freshnessErr(fmt.Errorf("library health: %s; run 'zotio sync' and retry", report.Freshness.Reason))
}

func selectHealthChecks(kinds []string) []healthCheck {
	want := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		want[k] = true
	}
	out := make([]healthCheck, 0, len(kinds))
	for _, chk := range healthCheckRegistry() {
		if want[chk.kind] {
			out = append(out, chk)
		}
	}
	return out
}

// itemCheckRunner adapts a per-item audit query into a finding producer. It runs
// the query unlimited so counts are accurate; output truncation happens later.
func itemCheckRunner(query func(localQueryStore) ([]map[string]any, error), kind, severity string, autofix bool, action *healthRecommendedAction) healthCheckRunner {
	return func(db localQueryStore, ctx *healthContext) ([]healthFinding, *healthSkip, error) {
		rows, err := query(db)
		if err != nil {
			return nil, nil, err
		}
		findings := make([]healthFinding, 0, len(rows))
		for _, r := range rows {
			f := healthFinding{
				Kind:              kind,
				Severity:          severity,
				ItemKey:           sqlStringValue(r["key"]),
				Title:             sqlStringValue(r["title"]),
				Source:            ctx.src,
				Autofixable:       autofix,
				RecommendedAction: action,
			}
			ev := map[string]any{}
			if it := sqlStringValue(r["item_type"]); it != "" {
				ev["item_type"] = it
			}
			if m := sqlStringValue(r["missing"]); m != "" {
				ev["missing"] = m
			}
			if len(ev) > 0 {
				f.Evidence = ev
			}
			findings = append(findings, f)
		}
		return findings, nil, nil
	}
}

func (ctx *healthContext) loadCitekeyRows(db localQueryStore) ([]citekeyConflictRow, error) {
	if ctx.citekeyLoaded {
		return ctx.citekeyRows, nil
	}
	rows, err := db.QueryRaw(citekeyAuditQuery)
	if err != nil {
		return nil, err
	}
	ctx.citekeyRows = buildCitekeyConflictRows(rows, false, false)
	ctx.citekeyLoaded = true
	return ctx.citekeyRows, nil
}

func runCitekeyConflict(db localQueryStore, ctx *healthContext) ([]healthFinding, *healthSkip, error) {
	rows, err := ctx.loadCitekeyRows(db)
	if err != nil {
		return nil, nil, err
	}
	findings := make([]healthFinding, 0)
	for _, r := range rows {
		if r.Type != "conflict" {
			continue
		}
		findings = append(findings, healthFinding{
			Kind:              "citekey_conflict",
			Severity:          sevCritical,
			ItemKey:           r.Key,
			Title:             r.Title,
			Evidence:          map[string]any{"cite_key": r.CiteKey},
			Source:            ctx.src,
			RecommendedAction: &healthRecommendedAction{Text: "Resolve the duplicate Better BibTeX citation key in Zotero"},
		})
	}
	return findings, nil, nil
}

func runCitekeyMissing(db localQueryStore, ctx *healthContext) ([]healthFinding, *healthSkip, error) {
	rows, err := ctx.loadCitekeyRows(db)
	if err != nil {
		return nil, nil, err
	}
	findings := make([]healthFinding, 0)
	for _, r := range rows {
		if r.Type != "missing" {
			continue
		}
		findings = append(findings, healthFinding{
			Kind:              "citekey_missing",
			Severity:          sevHigh,
			ItemKey:           r.Key,
			Title:             r.Title,
			Source:            ctx.src,
			RecommendedAction: &healthRecommendedAction{Text: "Install Better BibTeX and assign a citation key"},
		})
	}
	return findings, nil, nil
}

func runDuplicateCandidates(db localQueryStore, ctx *healthContext) ([]healthFinding, *healthSkip, error) {
	doiRows, err := queryDuplicateDOIs(db)
	if err != nil {
		return nil, nil, err
	}
	titleRows, err := queryDuplicateTitles(db)
	if err != nil {
		return nil, nil, err
	}
	rows := normalizeDuplicateRows(append(doiRows, titleRows...))
	findings := make([]healthFinding, 0, len(rows))
	for _, r := range rows {
		group := sqlStringValue(r["group"])
		ev := map[string]any{
			"group": group,
			"value": sqlStringValue(r["value"]),
			"count": sqlIntValue(r["count"]),
		}
		if keys, ok := r["keys"].([]string); ok {
			ev["keys"] = keys
		}
		findings = append(findings, healthFinding{
			Kind:              "duplicate_candidates",
			Severity:          sevHigh,
			Evidence:          ev,
			Source:            ctx.src,
			Autofixable:       true,
			RecommendedAction: &healthRecommendedAction{Command: "zotio items duplicates resolve"},
		})
	}
	return findings, nil, nil
}

func runTagDrift(db localQueryStore, ctx *healthContext) ([]healthFinding, *healthSkip, error) {
	tagRows, err := db.QueryRaw(tagAuditDistinctQuery)
	if err != nil {
		return nil, nil, err
	}
	countRows, err := db.QueryRaw(tagAuditCountQuery)
	if err != nil {
		return nil, nil, err
	}
	plans := buildTagAuditPlans(tagRows, countRows)
	findings := make([]healthFinding, 0, len(plans))
	for _, p := range plans {
		if len(p.Aliases) == 0 {
			continue
		}
		findings = append(findings, healthFinding{
			Kind:     "tag_drift",
			Severity: sevHigh,
			Evidence: map[string]any{
				"canonical":   p.Canonical,
				"aliases":     p.Aliases,
				"total_items": p.TotalItems,
			},
			Source:            ctx.src,
			Autofixable:       true,
			RecommendedAction: &healthRecommendedAction{Command: "zotio tags audit fix"},
		})
	}
	return findings, nil, nil
}

// runBrokenAttachmentFile is the one live_local_api check. It runs only when
// --verify-files is set AND Zotero desktop is reachable; otherwise it returns a
// loud skip with remediation rather than silently omitting itself.
func runBrokenAttachmentFile(db localQueryStore, ctx *healthContext) ([]healthFinding, *healthSkip, error) {
	if !ctx.verifyFiles {
		return nil, &healthSkip{
			Kind:         "broken_attachment_file",
			Precondition: "live_local_api",
			Detail:       "Broken-attachment verification is a live check (needs Zotero desktop running) and is off by default.",
			Remediation: []healthRemediation{
				{Action: "run_verify_files", Command: "zotio library health --for " + ctx.preset + " --verify-files"},
				{Action: "open_zotero", Text: "Open Zotero desktop and enable Settings -> Advanced -> 'Allow other applications to communicate with Zotero'"},
			},
		}, nil
	}

	c, err := ctx.flags.newClient()
	if err != nil {
		return nil, nil, err
	}
	if _, probeErr := c.Get("/", nil); probeErr != nil {
		var apiErr *client.APIError
		if !errors.As(probeErr, &apiErr) {
			// Network-level failure: Zotero desktop / local API is not reachable.
			return nil, &healthSkip{
				Kind:         "broken_attachment_file",
				Precondition: "live_local_api",
				Detail:       fmt.Sprintf("Zotero desktop is not reachable on the local API (%s).", probeErr),
				Remediation: []healthRemediation{
					{Action: "open_zotero", Text: "Open Zotero desktop and enable Settings -> Advanced -> 'Allow other applications to communicate with Zotero', then re-run"},
				},
			}, nil
		}
	}

	attachments, err := queryPDFAttachments(db, ctx.limit)
	if err != nil {
		return nil, nil, fmt.Errorf("querying PDF attachments: %w", err)
	}
	findings := make([]healthFinding, 0)
	for _, a := range attachments {
		key := sqlStringValue(a["key"])
		path, reason := attachmentFileStatus(c, key)
		if reason == "" {
			continue
		}
		findings = append(findings, healthFinding{
			Kind:     "broken_attachment_file",
			Severity: sevCritical,
			ItemKey:  key,
			Title:    sqlStringValue(a["name"]),
			Evidence: map[string]any{
				"parent": sqlStringValue(a["parent"]),
				"path":   path,
				"reason": reason,
			},
			Source:            ctx.src,
			RecommendedAction: &healthRecommendedAction{Text: "Re-link the file in Zotero or re-download the attachment"},
		})
	}
	return findings, nil, nil
}

// PATCH(marketing-heroes-2): live CrossRef retraction health check, gated like verify-files.
func runRetractedItem(db localQueryStore, ctx *healthContext) ([]healthFinding, *healthSkip, error) {
	if !ctx.checkRetractions {
		return nil, &healthSkip{
			Kind:         "retracted_item",
			Precondition: "external_crossref",
			Detail:       "CrossRef retraction checking is a live network check and is off by default; re-run with --check-retractions.",
			Remediation: []healthRemediation{
				{Action: "run_check_retractions", Command: "zotio library health --for " + ctx.preset + " --check-retractions"},
			},
		}, nil
	}

	httpClient := &http.Client{Timeout: enrichTimeout(ctx.flags.timeout)}
	if err := probeCrossrefRetractionAPI(context.Background(), httpClient); err != nil {
		return nil, &healthSkip{
			Kind:         "retracted_item",
			Precondition: "external_crossref",
			Detail:       fmt.Sprintf("CrossRef retraction checking is not reachable (%s); re-run with --check-retractions after network access is restored.", err),
			Remediation: []healthRemediation{
				{Action: "retry_check_retractions", Command: "zotio library health --for " + ctx.preset + " --check-retractions"},
			},
		}, nil
	}

	report, err := runRetractionCheck(context.Background(), db, httpClient, ctx.limit, "")
	if err != nil {
		return nil, nil, err
	}
	if report.Summary.Errors > 0 {
		return nil, &healthSkip{
			Kind:         "retracted_item",
			Precondition: "external_crossref",
			Detail:       fmt.Sprintf("CrossRef retraction checking encountered %d lookup error(s); cannot certify retractions.", report.Summary.Errors),
			Remediation: []healthRemediation{
				{Action: "retry_check_retractions", Command: "zotio library health --for " + ctx.preset + " --check-retractions"},
			},
		}, nil
	}

	findings := make([]healthFinding, 0, len(report.Findings))
	for _, f := range report.Findings {
		if f.Status == "correction" {
			continue
		}
		findings = append(findings, healthFinding{
			Kind:     "retracted_item",
			Severity: sevCritical,
			ItemKey:  f.ItemKey,
			Title:    f.Title,
			Evidence: map[string]any{
				"doi":         f.DOI,
				"status":      f.Status,
				"update_type": f.UpdateType,
				"notice_doi":  f.NoticeDOI,
				"date":        f.UpdateDate,
				"source":      f.Source,
				"label":       f.Label,
			},
			Source:            ctx.src,
			RecommendedAction: &healthRecommendedAction{Command: "zotio items retract-check --json"},
		})
	}
	return findings, nil, nil
}

func countCiteableItems(db localQueryStore) int {
	rows, err := db.QueryRaw(`
SELECT COUNT(*) AS count
FROM resources
WHERE resource_type = 'items'
	AND COALESCE(item_type, '') NOT IN ('attachment', 'annotation', 'note')`)
	if err != nil {
		return 0
	}
	return firstCount(rows)
}

func truncateFindings(findings []healthFinding, limit int) []healthFinding {
	if limit > 0 && len(findings) > limit {
		return findings[:limit]
	}
	return findings
}

func sortHealthFindings(findings []healthFinding) {
	sort.SliceStable(findings, func(i, j int) bool {
		ri, rj := severityRank(findings[i].Severity), severityRank(findings[j].Severity)
		if ri != rj {
			return ri > rj
		}
		if findings[i].Kind != findings[j].Kind {
			return findings[i].Kind < findings[j].Kind
		}
		return findings[i].ItemKey < findings[j].ItemKey
	})
}

func gateCrossed(summary healthSummary, failOn string) bool {
	switch failOn {
	case sevCritical:
		return summary.Critical > 0
	case sevHigh:
		return summary.Critical+summary.High > 0
	case sevInfo, "any":
		return summary.Total > 0
	default:
		return false
	}
}

// buildHealthRemediationPlan groups findings into preview-first commands that
// already exist. Exact item remediation uses `--keys-from -` and carries Keys in
// the JSON payload so an agent can pipe them without broadening scope.
func buildHealthRemediationPlan(findings []healthFinding) []healthRemediationPlanStep {
	type bucket struct {
		command string
		keys    []string
		seen    map[string]bool
	}
	exact := map[string]*bucket{
		"missing_doi":      {command: "zotio items enrich --missing-doi --keys-from -", seen: map[string]bool{}},
		"missing_abstract": {command: "zotio items enrich --missing-abstract --keys-from -", seen: map[string]bool{}},
		"missing_pdf":      {command: "zotio items enrich --missing-pdf --keys-from -", seen: map[string]bool{}},
	}
	var hasDOIDups, hasTitleDups, hasTagDrift bool
	for _, f := range findings {
		if b, ok := exact[f.Kind]; ok && f.ItemKey != "" {
			if !b.seen[f.ItemKey] {
				b.seen[f.ItemKey] = true
				b.keys = append(b.keys, f.ItemKey)
			}
			continue
		}
		switch f.Kind {
		case "duplicate_candidates":
			switch sqlStringValue(f.Evidence["group"]) {
			case "doi":
				hasDOIDups = true
			case "title":
				hasTitleDups = true
			}
		case "tag_drift":
			hasTagDrift = true
		}
	}

	steps := make([]healthRemediationPlanStep, 0, 6)
	for _, kind := range []string{"missing_doi", "missing_abstract", "missing_pdf"} {
		b := exact[kind]
		if len(b.keys) == 0 {
			continue
		}
		sort.Strings(b.keys)
		steps = append(steps, healthRemediationPlanStep{
			Kind:    kind,
			Command: b.command,
			Keys:    append([]string(nil), b.keys...),
			Count:   len(b.keys),
			Scoped:  true,
			Preview: true,
			Notes:   "Pipe keys to stdin; add --yes only after reviewing the mutation preview.",
		})
	}
	if hasDOIDups {
		steps = append(steps, healthRemediationPlanStep{
			Kind:    "duplicate_candidates",
			Command: "zotio items duplicates resolve --doi",
			Count:   countDuplicateGroups(findings, "doi"),
			Scoped:  false,
			Preview: true,
			Notes:   "Delegates to the existing duplicate resolver; preview first and add --yes only after review.",
		})
	}
	if hasTitleDups {
		steps = append(steps, healthRemediationPlanStep{
			Kind:    "duplicate_candidates",
			Command: "zotio items duplicates resolve --title",
			Count:   countDuplicateGroups(findings, "title"),
			Scoped:  false,
			Preview: true,
			Notes:   "Title groups are riskier; preview carefully before applying.",
		})
	}
	if hasTagDrift {
		steps = append(steps, healthRemediationPlanStep{
			Kind:    "tag_drift",
			Command: "zotio tags audit fix",
			Count:   countFindingsByKind(findings, "tag_drift"),
			Scoped:  false,
			Preview: true,
			Notes:   "Global tag-normalization preview; add --yes only after reviewing aliases.",
		})
	}
	return steps
}

func countFindingsByKind(findings []healthFinding, kind string) int {
	n := 0
	for _, f := range findings {
		if f.Kind == kind {
			n++
		}
	}
	return n
}

func countDuplicateGroups(findings []healthFinding, group string) int {
	n := 0
	for _, f := range findings {
		if f.Kind == "duplicate_candidates" && sqlStringValue(f.Evidence["group"]) == group {
			n++
		}
	}
	return n
}

func printHealthReport(cmd *cobra.Command, report healthReport) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Health: %s\n", healthStatusWord(report.Summary))
	// PATCH(action-arc): surface the baseline delta immediately after the summary line.
	if report.Baseline != nil {
		if report.Baseline.Established {
			fmt.Fprintf(out, "New since baseline: %d (resolved %d)\n", len(report.Baseline.New), report.Baseline.ResolvedCount)
		} else {
			fmt.Fprintf(out, "Baseline established (%d findings recorded)\n", report.Baseline.RecordedCount)
		}
	}

	scopeLine := fmt.Sprintf("Scope: %s · %d items · source %s", report.Scope.Expr, report.Scope.Items, report.Scope.Source)
	if report.Scope.SyncedAt != nil {
		// PATCH(demo-mode): durationAgo returns "just now" for <1m — appending
		// " ago" produced "synced just now ago" in the scope line.
		age := durationAgo(time.Since(*report.Scope.SyncedAt))
		if age == "just now" {
			scopeLine += " · synced just now"
		} else {
			scopeLine += fmt.Sprintf(" · synced %s ago", age)
		}
	}
	fmt.Fprintf(out, "%s · preset %s\n\n", scopeLine, report.Preset)

	for _, sev := range []string{sevCritical, sevHigh, sevInfo} {
		group := findingsForSeverity(report.Findings, sev)
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(out, "%s (%d)\n", strings.ToUpper(sev[:1])+sev[1:], countForSeverity(report.Summary, sev))
		for _, f := range group {
			prefix := ""
			if healthFindingIsNew(report, f) {
				prefix = "NEW "
			}
			fmt.Fprintf(out, "  %s%s\n", prefix, formatFindingLine(f))
		}
		fmt.Fprintln(out)
	}

	if len(report.Skipped) > 0 {
		fmt.Fprintln(out, "Skipped (precondition unmet)")
		for _, s := range report.Skipped {
			fmt.Fprintf(out, "  %s — %s\n", s.Kind, s.Detail)
			for _, r := range s.Remediation {
				if r.Command != "" {
					fmt.Fprintf(out, "    Fix: %s\n", r.Command)
				} else if r.Text != "" {
					fmt.Fprintf(out, "    Fix: %s\n", r.Text)
				}
			}
		}
		fmt.Fprintln(out)
	}

	if len(report.RemediationPlan) > 0 {
		fmt.Fprintln(out, "Remediation plan (preview-first)")
		for _, step := range report.RemediationPlan {
			scope := "global"
			if step.Scoped {
				scope = fmt.Sprintf("%d exact keys via stdin", step.Count)
			}
			fmt.Fprintf(out, "  %s — %s (%s)\n", step.Kind, step.Command, scope)
			if step.Notes != "" {
				fmt.Fprintf(out, "    %s\n", step.Notes)
			}
		}
		fmt.Fprintln(out)
	}

	if report.Freshness != nil {
		if report.Freshness.Stale {
			fmt.Fprintf(out, "Freshness: STALE (%s) — run 'zotio sync' and retry\n", report.Freshness.Reason)
		} else {
			fmt.Fprintln(out, "Freshness: OK")
		}
	}
	if report.Gate != nil {
		line := fmt.Sprintf("Gate: fail-on %s -> %s", report.Gate.FailOn, strings.ToUpper(report.Gate.Status))
		if report.Gate.Reason != "" {
			line += fmt.Sprintf(" (%s)", report.Gate.Reason)
		}
		fmt.Fprintln(out, line)
	}
}

func healthStatusWord(s healthSummary) string {
	switch {
	case s.Critical > 0:
		return "critical"
	case s.High > 0:
		return "needs attention"
	case s.Info > 0:
		return "ok with notes"
	default:
		return "clean"
	}
}

func findingsForSeverity(findings []healthFinding, severity string) []healthFinding {
	out := make([]healthFinding, 0)
	for _, f := range findings {
		if f.Severity == severity {
			out = append(out, f)
		}
	}
	return out
}

func countForSeverity(s healthSummary, severity string) int {
	switch severity {
	case sevCritical:
		return s.Critical
	case sevHigh:
		return s.High
	case sevInfo:
		return s.Info
	default:
		return 0
	}
}

func formatFindingLine(f healthFinding) string {
	switch f.Kind {
	case "duplicate_candidates":
		return fmt.Sprintf("[%s] %s=%q (%v items)", f.Kind, sqlStringValue(f.Evidence["group"]), sqlStringValue(f.Evidence["value"]), f.Evidence["count"])
	case "tag_drift":
		return fmt.Sprintf("[%s] %q <- %v (%v items)", f.Kind, sqlStringValue(f.Evidence["canonical"]), f.Evidence["aliases"], f.Evidence["total_items"])
	case "retracted_item":
		// PATCH(marketing-heroes-2): surface CrossRef notice DOI/date in human health output.
		line := fmt.Sprintf("[%s] %s", f.Kind, f.ItemKey)
		if f.Title != "" {
			line += " " + f.Title
		}
		if notice := sqlStringValue(f.Evidence["notice_doi"]); notice != "" {
			line += " (notice: " + notice
			if date := sqlStringValue(f.Evidence["date"]); date != "" {
				line += " " + date
			}
			line += ")"
		}
		return line
	default:
		line := fmt.Sprintf("[%s] %s", f.Kind, f.ItemKey)
		if f.Title != "" {
			line += " " + f.Title
		}
		if m := sqlStringValue(f.Evidence["missing"]); m != "" {
			line += " (missing: " + m + ")"
		}
		return line
	}
}

func durationAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
