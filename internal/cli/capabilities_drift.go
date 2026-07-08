// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// expose a read-only live API
// probe so agents can detect capability registry drift against Zotero endpoints.

package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

type capabilitiesDriftFinding struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

type capabilitiesDriftReport struct {
	Checked     int                        `json:"checked"`
	OK          int                        `json:"ok"`
	Drifted     []capabilitiesDriftFinding `json:"drifted"`
	GeneratedAt string                     `json:"generated_at"`
}

type capabilitiesDriftProbe struct {
	path   string
	params map[string]string
	global bool
}

func newCapabilitiesDriftCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "drift",
		Short: "Probe core read endpoints for capability/API drift",
		Long: `Probes core Zotero read endpoints declared in the capability registry and
reports any endpoint that now returns an API error or cannot be reached. This is
read-only and treats every non-nil GET error as drift, including HTTP 4xx/5xx
and network failures.`,
		// mcp:hidden — capability drift is a
		// maintenance/ops probe, not an agent tool.
		Annotations: map[string]string{"mcp:read-only": "true", "mcp:hidden": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			// the capabilities parent is
			// registered before root flags are threaded through it; hydrate the narrow
			// read/output fields from Cobra when invoked from the root command.
			hydrateCapabilitiesDriftFlags(cmd, flags)

			c, err := flags.newClient()
			if err != nil {
				return err
			}
			sc, err := newSchemaClient(flags)
			if err != nil {
				return err
			}
			c.NoCache = true
			sc.NoCache = true

			limitOne := map[string]string{"limit": "1"}
			probes := []capabilitiesDriftProbe{
				{path: "/items", params: limitOne},
				{path: "/collections", params: limitOne},
				{path: "/tags", params: limitOne},
				{path: "/searches", params: limitOne},
				{path: "/itemTypes", global: true},
				{path: "/itemFields", global: true},
			}

			report := capabilitiesDriftReport{
				Checked:     len(probes),
				Drifted:     make([]capabilitiesDriftFinding, 0, len(probes)),
				GeneratedAt: time.Now().UTC().Format(time.RFC3339),
			}
			for _, probe := range probes {
				getter := c
				if probe.global {
					getter = sc
				}
				if _, err := getter.Get(probe.path, probe.params); err != nil {
					report.Drifted = append(report.Drifted, capabilitiesDriftFinding{Path: probe.path, Error: err.Error()})
					continue
				}
				report.OK++
			}

			if flags.asJSON {
				data, err := json.Marshal(report)
				if err != nil {
					return err
				}
				return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
			}

			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "%d endpoints checked, %d ok, %d drifted\n", report.Checked, report.OK, len(report.Drifted))
			for _, drift := range report.Drifted {
				fmt.Fprintf(w, "%s: %s\n", drift.Path, drift.Error)
			}
			return nil
		},
	}
	return cmd
}

func hydrateCapabilitiesDriftFlags(cmd *cobra.Command, flags *rootFlags) {
	if cmd == nil || flags == nil {
		return
	}
	getBool := func(name string) (bool, bool) {
		flag := cmd.Flag(name)
		if flag == nil {
			return false, false
		}
		v, err := strconv.ParseBool(flag.Value.String())
		return v, err == nil
	}
	getString := func(name string) (string, bool) {
		flag := cmd.Flag(name)
		if flag == nil {
			return "", false
		}
		return flag.Value.String(), true
	}
	getFloat := func(name string) (float64, bool) {
		flag := cmd.Flag(name)
		if flag == nil {
			return 0, false
		}
		v, err := strconv.ParseFloat(flag.Value.String(), 64)
		return v, err == nil
	}
	getDuration := func(name string) (time.Duration, bool) {
		flag := cmd.Flag(name)
		if flag == nil {
			return 0, false
		}
		v, err := time.ParseDuration(flag.Value.String())
		return v, err == nil
	}

	if v, ok := getBool("json"); ok {
		flags.asJSON = v
	}
	if v, ok := getBool("compact"); ok {
		flags.compact = v
	}
	if v, ok := getBool("csv"); ok {
		flags.csv = v
	}
	if v, ok := getBool("quiet"); ok {
		flags.quiet = v
	}
	if v, ok := getBool("dry-run"); ok {
		flags.dryRun = v
	}
	if v, ok := getBool("no-cache"); ok {
		flags.noCache = v
	}
	if v, ok := getBool("agent"); ok {
		flags.agent = v
		if v {
			if flag := cmd.Flag("json"); flag != nil && !flag.Changed {
				flags.asJSON = true
			}
			if flag := cmd.Flag("compact"); flag != nil && !flag.Changed {
				flags.compact = true
			}
		}
	}
	if v, ok := getString("select"); ok {
		flags.selectFields = v
	}
	if v, ok := getString("config"); ok {
		flags.configPath = v
	}
	if v, ok := getString("group"); ok {
		flags.group = v
	}
	if v, ok := getDuration("timeout"); ok {
		flags.timeout = v
	}
	if v, ok := getFloat("rate-limit"); ok {
		flags.rateLimit = v
	}
}
