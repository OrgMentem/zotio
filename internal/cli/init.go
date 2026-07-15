// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Guided first-run initializer composed from existing doctor/auth/sync/health seams.

package cli

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/config"
	"zotio/internal/store"
)

const (
	initStepLocalAPI = "local_api"
	initStepAPIKey   = "api_key"
	initStepSync     = "sync"
	initStepHealth   = "health"

	initLocalAPIRemediation = "open Zotero → Settings → Advanced → 'Allow other applications…'"
)

type initStepReport struct {
	Step        string `json:"step"`
	OK          bool   `json:"ok"`
	Status      string `json:"status,omitempty"`
	Remediation string `json:"remediation,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

type initSyncResourceReport struct {
	Resource string `json:"resource"`
	Count    int    `json:"count"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}

type initReport struct {
	OK                bool                     `json:"ok"`
	Steps             []initStepReport         `json:"steps"`
	Sync              []initSyncResourceReport `json:"sync,omitempty"`
	HealthVerdict     string                   `json:"health_verdict,omitempty"`
	SuggestedCommands []string                 `json:"suggested_commands,omitempty"`
}

func newInitCmd(flags *rootFlags) *cobra.Command {
	var launch bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Guided first-run setup using doctor, auth, sync, and health checks",
		Long: `Guided first-run setup for Zotero automation.

Checks the local Zotero API, stores an API key when one is provided interactively,
runs the first local sync when the store is missing or empty, and finishes with the
quick library-health preset plus suggested next commands.`,
		Args:        cobra.NoArgs,
		Annotations: map[string]string{"mcp:read-only": "false", "zotio:preflight": "skip"},
		RunE: func(cmd *cobra.Command, args []string) error {
			report, exitErr := runInit(cmd, flags, launch)
			if renderErr := renderInitReport(cmd, flags, report); renderErr != nil {
				return renderErr
			}
			return exitErr
		},
	}
	cmd.Flags().BoolVar(&launch, "launch", false, "Launch Zotero and wait for the local API when it is not reachable")
	return cmd
}

// Orchestrate first-run setup through existing local API, auth, sync, and health seams.
func runInit(cmd *cobra.Command, flags *rootFlags, launch bool) (initReport, error) {
	report := initReport{
		OK: true,
		SuggestedCommands: []string{
			`zotio which "<goal>"`,
			"zotio library health --for citation",
			"zotio search '<term>' --data-source local",
		},
	}
	setupRequired := false

	cfg, err := config.Load(flags.configPath)
	if err != nil {
		report.OK = false
		report.Steps = append(report.Steps, initStepReport{Step: "config", OK: false, Status: "error", Detail: err.Error()})
		return report, configErr(err)
	}

	localOK, localStep := runInitLocalAPIStep(cmd, flags, launch)
	report.Steps = append(report.Steps, localStep)
	if !localOK {
		setupRequired = true
	}

	keyOK, keyStep := runInitAPIKeyStep(cmd, flags, cfg)
	report.Steps = append(report.Steps, keyStep)
	if !keyOK {
		setupRequired = true
	}

	syncOK, syncStep, syncResources, syncErr := runInitSyncStep(cmd, flags, localOK)
	report.Steps = append(report.Steps, syncStep)
	report.Sync = syncResources
	if syncErr != nil {
		report.OK = false
		return report, syncErr
	}
	if !syncOK {
		setupRequired = true
	}

	healthOK, healthStep, verdict, healthErr := runInitHealthStep(cmd, flags)
	report.Steps = append(report.Steps, healthStep)
	report.HealthVerdict = verdict
	if healthErr != nil {
		report.OK = false
		return report, healthErr
	}
	if !healthOK {
		setupRequired = true
	}

	if setupRequired {
		report.OK = false
		return report, preconditionErr(fmt.Errorf("zotio init: setup required; follow the remediation in the step report"))
	}
	report.OK = true
	return report, nil
}

// Reuse doctor's local API reachability check and ensure-live launch primitive.
func runInitLocalAPIStep(cmd *cobra.Command, flags *rootFlags, launch bool) (bool, initStepReport) {
	c, err := flags.newClient()
	if err != nil {
		return false, initStepReport{Step: initStepLocalAPI, OK: false, Status: "client_error", Remediation: initLocalAPIRemediation, Detail: err.Error()}
	}
	if localAPIReachable(c) {
		return true, initStepReport{Step: initStepLocalAPI, OK: true, Status: "reachable"}
	}
	if launch {
		if err := runEnsureLiveForInit(cmd, flags); err != nil {
			return false, initStepReport{Step: initStepLocalAPI, OK: false, Status: "unreachable", Remediation: initLocalAPIRemediation, Detail: err.Error()}
		}
		if localAPIReachable(c) {
			return true, initStepReport{Step: initStepLocalAPI, OK: true, Status: "reachable_after_launch"}
		}
	}
	return false, initStepReport{Step: initStepLocalAPI, OK: false, Status: "unreachable", Remediation: initLocalAPIRemediation}
}

// Call ensureLive without letting its standalone renderer corrupt init's single report.
func runEnsureLiveForInit(cmd *cobra.Command, flags *rootFlags) error {
	quietFlags := *flags
	quietFlags.asJSON = false
	quietCmd := &cobra.Command{Use: "doctor --ensure-live"}
	quietCmd.SetContext(cmd.Context())
	quietCmd.SetOut(io.Discard)
	quietCmd.SetErr(io.Discard)
	return ensureLive(quietCmd, &quietFlags, true)
}

// Store prompted API keys through the same Config.SaveCredential path as auth set-token.
func runInitAPIKeyStep(cmd *cobra.Command, flags *rootFlags, cfg *config.Config) (bool, initStepReport) {
	if cfg.AuthHeader() != "" {
		return true, initStepReport{Step: initStepAPIKey, OK: true, Status: "configured"}
	}
	remediation := "create a Zotero API key at https://www.zotero.org/settings/keys, then run: printf %s \"$ZOTERO_API_KEY\" | zotio auth set-token --stdin"
	if flags.noInput || flags.agent {
		return false, initStepReport{Step: initStepAPIKey, OK: false, Status: "missing", Remediation: remediation}
	}

	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "Zotero reads work through the local API without a key; writes need a Zotero Web API key.")
	fmt.Fprintln(out, "Create one at https://www.zotero.org/settings/keys and paste it here.")
	fmt.Fprint(out, "API key: ")
	reader := bufio.NewReader(cmd.InOrStdin())
	token, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, initStepReport{Step: initStepAPIKey, OK: false, Status: "read_error", Remediation: remediation, Detail: err.Error()}
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return false, initStepReport{Step: initStepAPIKey, OK: false, Status: "missing", Remediation: remediation}
	}
	cfg.AuthHeaderVal = ""
	if err := cfg.SaveCredential(token); err != nil {
		return false, initStepReport{Step: initStepAPIKey, OK: false, Status: "save_error", Remediation: remediation, Detail: err.Error()}
	}
	return true, initStepReport{Step: initStepAPIKey, OK: true, Status: "saved"}
}

// Detect empty local stores and run the syncResource core path for first syncs.
func runInitSyncStep(cmd *cobra.Command, flags *rootFlags, localAPIOK bool) (bool, initStepReport, []initSyncResourceReport, error) {
	empty, err := localStoreNeedsFirstSync(cmd, "zotio")
	if err != nil {
		return false, initStepReport{Step: initStepSync, OK: false, Status: "store_error", Detail: err.Error()}, nil, nil
	}
	if !empty {
		return true, initStepReport{Step: initStepSync, OK: true, Status: "local_store_ready"}, nil, nil
	}
	if !localAPIOK {
		return false, initStepReport{Step: initStepSync, OK: false, Status: "blocked", Remediation: initLocalAPIRemediation}, nil, nil
	}

	resources, syncErr := runInitialSync(cmd, flags)
	if syncErr != nil {
		return false, initStepReport{Step: initStepSync, OK: false, Status: "failed", Remediation: "rerun zotio sync after fixing the reported API error", Detail: syncErr.Error()}, resources, apiErr(syncErr)
	}
	return true, initStepReport{Step: initStepSync, OK: true, Status: "synced"}, resources, nil
}

// Consider a missing DB or zero synced items as needing the first sync.
func localStoreNeedsFirstSync(cmd *cobra.Command, cliName string) (bool, error) {
	rawDB, err := openStoreForRead(cmd.Context(), cliName)
	if err != nil {
		return false, err
	}
	if rawDB == nil {
		return true, nil
	}
	defer rawDB.Close()
	count, err := rawDB.Count("items")
	if err != nil {
		return false, err
	}
	return count == 0, nil
}

// Reuse syncResource/defaultSyncResources while forcing progress away from JSON stdout.
func runInitialSync(cmd *cobra.Command, flags *rootFlags) ([]initSyncResourceReport, error) {
	c, err := flags.newClient()
	if err != nil {
		return nil, err
	}
	c.NoCache = true
	db, err := store.OpenWithContext(cmd.Context(), defaultDBPath("zotio"))
	if err != nil {
		return nil, fmt.Errorf("opening local database: %w", err)
	}
	defer db.Close()

	resources := defaultSyncResources()
	reports := make([]initSyncResourceReport, 0, len(resources))
	started := time.Now()
	fmt.Fprintln(cmd.ErrOrStderr(), "Running first sync...")
	savedHumanFriendly := humanFriendly
	humanFriendly = true
	defer func() { humanFriendly = savedHumanFriendly }()

	var failures []string
	for _, resource := range resources {
		res := syncResource(cmd.Context(), c, db, resource, 0, false, 100, true)
		r := initSyncResourceReport{Resource: res.Resource, Count: res.Count, Status: "ok"}
		switch {
		case res.Err != nil:
			r.Status = "error"
			r.Error = res.Err.Error()
			failures = append(failures, fmt.Sprintf("%s: %v", resource, res.Err))
		case res.Warn != nil:
			r.Status = "warning"
			r.Error = res.Warn.Error()
		}
		reports = append(reports, r)
		fmt.Fprintf(cmd.ErrOrStderr(), "  %s: %d synced (%s)\n", resource, res.Count, r.Status)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "First sync finished in %.1fs.\n", time.Since(started).Seconds())
	if len(failures) > 0 {
		return reports, fmt.Errorf("%d resource(s) failed to sync: %s", len(failures), strings.Join(failures, "; "))
	}
	return reports, nil
}

// Run the library health quick preset in-process and reduce it to a first-run verdict.
func runInitHealthStep(cmd *cobra.Command, flags *rootFlags) (bool, initStepReport, string, error) {
	rawDB, err := openStoreForRead(cmd.Context(), "zotio")
	if err != nil {
		return false, initStepReport{Step: initStepHealth, OK: false, Status: "store_error", Detail: err.Error()}, "", nil
	}
	if rawDB == nil {
		return false, initStepReport{Step: initStepHealth, OK: false, Status: "not_synced", Remediation: "run zotio sync first"}, "", nil
	}
	defer rawDB.Close()
	db := localQueryStore{rawDB}

	var syncedAt *time.Time
	if _, lastSynced, _, e := db.GetSyncState("items"); e == nil && !lastSynced.IsZero() {
		ls := lastSynced
		syncedAt = &ls
	}
	healthCtx := &healthContext{
		src:         FindingSource{Kind: "local", SyncedAt: syncedAt},
		preset:      "quick",
		flags:       flags,
		verifyFiles: false,
	}
	report, err := assembleHealthReport(db, healthCtx, "quick", healthPresets["quick"], healthPresetFailOn["quick"], scopeResult{All: true, Expr: "library"})
	if err != nil {
		return false, initStepReport{Step: initStepHealth, OK: false, Status: "failed", Detail: err.Error()}, "", nil
	}
	verdict := initHealthVerdict(report)
	return true, initStepReport{Step: initStepHealth, OK: true, Status: verdict}, verdict, nil
}

// Keep init's finale to the requested one-line health verdict.
func initHealthVerdict(report healthReport) string {
	if report.Summary.Total == 0 {
		if len(report.Skipped) > 0 {
			return fmt.Sprintf("quick health: no findings (%d check(s) skipped)", len(report.Skipped))
		}
		return "quick health: no findings"
	}
	parts := make([]string, 0, 3)
	if report.Summary.Critical > 0 {
		parts = append(parts, fmt.Sprintf("%d critical", report.Summary.Critical))
	}
	if report.Summary.High > 0 {
		parts = append(parts, fmt.Sprintf("%d high", report.Summary.High))
	}
	if report.Summary.Info > 0 {
		parts = append(parts, fmt.Sprintf("%d info", report.Summary.Info))
	}
	return fmt.Sprintf("quick health: %d finding(s): %s", report.Summary.Total, strings.Join(parts, ", "))
}

// Render either a machine step report or the guided human checklist.
func renderInitReport(cmd *cobra.Command, flags *rootFlags, report initReport) error {
	if flags.asJSON || flags.agent {
		return printJSONFiltered(cmd.OutOrStdout(), report, flags)
	}
	out := cmd.OutOrStdout()
	for _, step := range report.Steps {
		marker := green("OK")
		if !step.OK {
			marker = yellow("SETUP")
		}
		fmt.Fprintf(out, "%s %s: %s\n", marker, step.Step, step.Status)
		if step.Remediation != "" {
			fmt.Fprintf(out, "  fix: %s\n", step.Remediation)
		}
		if step.Detail != "" {
			fmt.Fprintf(out, "  detail: %s\n", step.Detail)
		}
	}
	if report.HealthVerdict != "" {
		fmt.Fprintf(out, "%s\n", report.HealthVerdict)
	}
	fmt.Fprintln(out, "Next commands:")
	for _, command := range report.SuggestedCommands {
		fmt.Fprintf(out, "  %s\n", command)
	}
	return nil
}
