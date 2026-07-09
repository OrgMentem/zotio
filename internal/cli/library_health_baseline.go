// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// healthBaselineFile is the persisted baseline identity schema used by CI delta gates.
type healthBaselineFile struct {
	SchemaVersion int      `json:"schema_version"`
	GeneratedAt   string   `json:"generated_at"`
	Preset        string   `json:"preset"`
	Identities    []string `json:"identities"`
}

// healthBaselineReport is the JSON report block for baseline-aware health runs.
type healthBaselineReport struct {
	Established   bool      `json:"established"`
	New           []Finding `json:"new"`
	ResolvedCount int       `json:"resolved_count"`
	BaselinePath  string    `json:"baseline_path"`

	NewIDs        map[string]struct{} `json:"-"`
	RecordedCount int                 `json:"-"`
}

// applyHealthBaseline diffs current findings against the watch --health stable identity key.
func applyHealthBaseline(report *healthReport, path string) error {
	baseline, established, err := readHealthBaseline(path)
	if err != nil {
		return usageErr(fmt.Errorf("reading --baseline %s: %w", path, err))
	}

	current := healthCurrentFindings(*report)
	currentIDs := make(map[string]struct{}, len(current))
	for _, finding := range current {
		currentIDs[watchHealthFindingKey(finding)] = struct{}{}
	}

	out := &healthBaselineReport{
		Established:   established,
		New:           make([]Finding, 0),
		BaselinePath:  path,
		NewIDs:        make(map[string]struct{}),
		RecordedCount: len(currentIDs),
	}
	if !established {
		report.Baseline = out
		return nil
	}

	baselineIDs := make(map[string]struct{}, len(baseline.Identities))
	for _, identity := range baseline.Identities {
		if identity == "" {
			continue
		}
		baselineIDs[identity] = struct{}{}
	}
	for _, finding := range current {
		identity := watchHealthFindingKey(finding)
		if _, seen := baselineIDs[identity]; seen {
			continue
		}
		if _, alreadyNew := out.NewIDs[identity]; alreadyNew {
			continue
		}
		out.New = append(out.New, finding)
		out.NewIDs[identity] = struct{}{}
	}
	for identity := range baselineIDs {
		if _, stillPresent := currentIDs[identity]; !stillPresent {
			out.ResolvedCount++
		}
	}
	sortHealthFindings(out.New)
	report.Baseline = out
	return nil
}

func readHealthBaseline(path string) (healthBaselineFile, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return healthBaselineFile{}, false, nil
		}
		return healthBaselineFile{}, false, err
	}
	var baseline healthBaselineFile
	if err := json.Unmarshal(data, &baseline); err != nil {
		return healthBaselineFile{}, false, err
	}
	if baseline.SchemaVersion != 1 {
		return healthBaselineFile{}, false, fmt.Errorf("unsupported schema_version %d", baseline.SchemaVersion)
	}
	return baseline, true, nil
}

// writeHealthBaseline atomically persists current watch-health identities for future deltas.
func writeHealthBaseline(path string, preset string, findings []Finding) error {
	identities := FindingIdentities(findings)
	payload := healthBaselineFile{
		SchemaVersion: 1,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Preset:        preset,
		Identities:    identities,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(path, data)
}

// write --report JSON sidecars in human and badge modes without changing stdout.
func writeHealthReportFile(path string, report healthReport) error {
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return nil
}

// baseline identity helpers reuse watch --health finding keys.
func healthCurrentFindings(report healthReport) []Finding {
	if len(report.allFindings) > 0 || report.Summary.Total == 0 {
		return report.allFindings
	}
	return report.Findings
}

func FindingIdentities(findings []Finding) []string {
	set := make(map[string]struct{}, len(findings))
	for _, finding := range findings {
		set[watchHealthFindingKey(finding)] = struct{}{}
	}
	identities := make([]string, 0, len(set))
	for identity := range set {
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	return identities
}

func healthSummaryForFindings(findings []Finding) healthSummary {
	var summary healthSummary
	for _, finding := range findings {
		summary.add(finding.Severity, 1)
	}
	summary.Total = summary.Critical + summary.High + summary.Info
	return summary
}

func FindingIsNew(report healthReport, finding Finding) bool {
	if report.Baseline == nil || len(report.Baseline.NewIDs) == 0 {
		return false
	}
	_, ok := report.Baseline.NewIDs[watchHealthFindingKey(finding)]
	return ok
}
