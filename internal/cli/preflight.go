// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Central capability-registry preflight enforcement for cheap, declared setup
// requirements. The registry is the policy surface; this file is the runtime
// guard that turns unmet preconditions into uniform exit-9 refusals.

package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"zotio/internal/config"

	"github.com/spf13/cobra"
)

const (
	preflightAnnotationKey  = "zotio:preflight"
	preflightAnnotationSkip = "skip"
)

type preconditionUnmetEnvelope struct {
	Kind                  string   `json:"kind"`
	Capability            string   `json:"capability"`
	Precondition          string   `json:"precondition"`
	Detail                string   `json:"detail"`
	Remediation           []string `json:"remediation"`
	RetryAfterRemediation bool     `json:"retry_after_remediation"`
}

type preconditionChecker func(context.Context, *rootFlags, *cobra.Command, capabilityEntry) (bool, string, error)

var preconditionCheckers = map[string]preconditionChecker{
	preconditionSyncedStore:       checkSyncedStorePrecondition,
	preconditionWebAPIKey:         checkWebAPIKeyPrecondition,
	preconditionLiveLocalAPI:      checkLiveLocalAPIPrecondition,
	preconditionBetterBibTeX:      checkBetterBibTeXPrecondition,
	preconditionDesktopConnector:  checkDesktopConnectorPrecondition,
	preconditionZoteroFileStorage: checkZoteroFileStoragePrecondition,
}

// preconditionRemediators supply remediation that depends on WHY a
// precondition failed rather than on the precondition alone. A precondition
// without an entry here falls back to preconditionRemediation's static list.
//
// The storage guard needs this: it refuses for several distinct reasons, and
// the useful next step differs for each, down to naming the specific Zotero
// profile whose configuration produced the verdict.
var preconditionRemediators = map[string]func(context.Context, *rootFlags) []string{
	preconditionZoteroFileStorage: func(ctx context.Context, flags *rootFlags) []string {
		return storedUploadRefusalSteps(storedUploadDecision(ctx, flags, storedUploadRouteUnknown))
	},
}

// remediationFor resolves the remediation for one failed precondition.
func remediationFor(ctx context.Context, flags *rootFlags, precondition string) []string {
	if resolve := preconditionRemediators[precondition]; resolve != nil {
		if steps := resolve(ctx, flags); len(steps) > 0 {
			return steps
		}
	}
	return preconditionRemediation(precondition)
}

func init() {
	validateCapabilityPreconditions()
}

func validateCapabilityPreconditions() {
	for path, entry := range capabilityOverrides {
		for _, req := range entry.Requires {
			if _, ok := preconditionCheckers[req]; !ok {
				panic(fmt.Sprintf("capabilityOverrides[%q] declares unknown precondition %q", path, req))
			}
		}
	}
}

func runCapabilityPreflight(cmd *cobra.Command, flags *rootFlags) error {
	if cmd == nil || flags == nil || commandPreflightSkipped(cmd) {
		return nil
	}
	path := commandRegistryPath(cmd)
	if path == "" {
		return nil
	}
	entry, ok := capabilityOverrides[path]
	if !ok || len(entry.Requires) == 0 {
		return nil
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	for _, req := range entry.Requires {
		if !preconditionEnforcedAtPreflight(entry, req) {
			continue
		}
		checker := preconditionCheckers[req]
		ok, detail, err := checker(ctx, flags, cmd, entry)
		if err != nil {
			return err
		}
		if !ok {
			return emitPreconditionUnmetWithRemediation(cmd.OutOrStdout(), flags, path, req, detail,
				remediationFor(ctx, flags, req))
		}
	}
	return nil
}

func preconditionEnforcedAtPreflight(entry capabilityEntry, precondition string) bool {
	if precondition == preconditionWebAPIKey && entry.Operation == "write" {
		// Mutating commands must remain preview-first: a missing key is enforced by
		// the apply-time write guard, not before a --dry-run plan can be produced.
		return false
	}
	return true
}

func commandPreflightSkipped(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Annotations != nil && c.Annotations[preflightAnnotationKey] == preflightAnnotationSkip {
			return true
		}
	}
	return false
}

func commandRegistryPath(cmd *cobra.Command) string {
	if cmd == nil || cmd.Root() == nil || cmd == cmd.Root() {
		return ""
	}
	return strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
}

// emitPreconditionUnmetWithRemediation is emitPreconditionUnmet for callers
// whose remediation depends on WHY the precondition failed rather than on the
// precondition alone. The storage guard refuses for several distinct reasons
// and the useful next step differs for each.
func emitPreconditionUnmetWithRemediation(w io.Writer, flags *rootFlags, capability, precondition, detail string, remediation []string) error {
	detail = sanitizeForTerminal(detail)
	env := preconditionUnmetEnvelope{
		Kind:                  "precondition_unmet",
		Capability:            capability,
		Precondition:          precondition,
		Detail:                detail,
		Remediation:           remediation,
		RetryAfterRemediation: true,
	}
	if writePreconditionUnmetEnvelope(w, flags, env) {
		// The machine-readable envelope already carries the remediation, so the
		// error stays a single line for logs.
		return preconditionErr(fmt.Errorf("%s requires %s: %s", capability, precondition, detail))
	}
	return preconditionErr(errors.New(humanPreconditionMessage(capability, precondition, detail, remediation)))
}

// humanPreconditionMessage renders the failure the way a person needs to read
// it: what is wrong, then what to do about it. The remediation used to be
// reachable only under --json, which meant the terminal told operators they
// were blocked and never how to proceed.
func humanPreconditionMessage(capability, precondition, detail string, remediation []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s requires %s.\n\n%s.", capability, precondition, strings.TrimRight(detail, "."))
	if len(remediation) == 0 {
		return b.String()
	}
	b.WriteString("\n\nWhat to do instead:")
	for _, step := range remediation {
		fmt.Fprintf(&b, "\n  - %s", sanitizeForTerminal(step))
	}
	return b.String()
}

// writePreconditionUnmetEnvelope reports whether it wrote the machine-readable
// envelope, so the caller knows whether the remediation still needs saying.
func writePreconditionUnmetEnvelope(w io.Writer, flags *rootFlags, env preconditionUnmetEnvelope) bool {
	if flags == nil || !flags.asJSON || w == nil {
		return false
	}
	_ = json.NewEncoder(w).Encode(env)
	return true
}

func preconditionRemediation(precondition string) []string {
	switch precondition {
	case preconditionSyncedStore:
		return []string{
			"Run 'zotio sync' to populate the local store.",
			"If this is a group library, pass --group <id> or set ZOTERO_GROUP before syncing and retrying.",
		}
	case preconditionWebAPIKey:
		return []string{
			"Create a Zotero API key at https://www.zotero.org/settings/keys.",
			"Set it with: export ZOTERO_API_KEY=<key>, or save it with: printf %s \"$ZOTERO_API_KEY\" | zotio auth set-token --stdin.",
			"Run 'zotio doctor' to verify authentication, then retry.",
		}
	case preconditionLiveLocalAPI:
		return []string{
			"Open Zotero desktop and enable Settings -> Advanced -> 'Allow other applications to communicate with Zotero'.",
			"Run 'zotio doctor --ensure-live --launch' to start Zotero and verify the local API, then retry.",
		}
	case preconditionBetterBibTeX:
		return []string{
			"Install the Zotero Better BibTeX extension and let Zotero assign citation keys.",
			"Run 'zotio sync --resources items --full' after Better BibTeX updates the library, then retry.",
		}
	case preconditionDesktopConnector:
		return []string{
			"Open Zotero desktop and enable Settings -> Advanced -> 'Allow other applications to communicate with Zotero'.",
			"Use a local Zotero base URL (the default http://localhost:23119/api/users/0) and retry after 'zotio doctor' reports the desktop connector reachable.",
		}
	case preconditionZoteroFileStorage:
		return storedUploadRefusalRemediation(storedUploadReasonWebDAV)
	default:
		return []string{"Fix the declared setup precondition, then retry."}
	}
}

func checkSyncedStorePrecondition(ctx context.Context, _ *rootFlags, _ *cobra.Command, _ capabilityEntry) (bool, string, error) {
	db, err := defaultDBPath("zotio")
	if err != nil {
		return false, fmt.Sprintf("resolving local store path: %v", err), nil
	}
	s, err := openStoreForRead(ctx, "zotio")
	if err != nil {
		return false, fmt.Sprintf("local store at %s cannot be opened: %v", db, err), nil
	}
	if s == nil {
		return false, fmt.Sprintf("local store not found at %s", db), nil
	}
	defer s.Close()
	state, err := readSyncHintState(s, "")
	if err != nil {
		return false, fmt.Sprintf("local store sync state cannot be read: %v", err), nil
	}
	if !state.hasState {
		return false, "local store has no completed sync checkpoint", nil
	}
	status, err := s.Status()
	if err != nil {
		return false, fmt.Sprintf("local store contents cannot be counted: %v", err), nil
	}
	total := 0
	for _, count := range status {
		total += count
	}
	if total == 0 {
		return false, "local store is present but contains no synced resources", nil
	}
	return true, "", nil
}

func checkWebAPIKeyPrecondition(_ context.Context, flags *rootFlags, _ *cobra.Command, _ capabilityEntry) (bool, string, error) {
	cfg, err := config.Load(flags.configPath)
	if err != nil {
		return false, "", configErr(err)
	}
	if cfg == nil || strings.TrimSpace(cfg.AuthHeader()) == "" {
		return false, "no Zotero Web API key is configured in config or ZOTERO_API_KEY", nil
	}
	return true, "", nil
}

func checkLiveLocalAPIPrecondition(_ context.Context, flags *rootFlags, _ *cobra.Command, _ capabilityEntry) (bool, string, error) {
	cfg, err := config.Load(flags.configPath)
	if err != nil {
		return false, "", configErr(err)
	}
	if cfg == nil || !isLocalZoteroAPI(cfg.BaseURL) {
		base := ""
		if cfg != nil {
			base = redactURL(cfg.BaseURL)
		}
		return false, fmt.Sprintf("configured base URL %q is not the Zotero desktop local API", base), nil
	}
	c, err := flags.newClient()
	if err != nil {
		return false, "", err
	}
	if !localAPIReachable(c) {
		return false, "Zotero desktop local API is not reachable", nil
	}
	return true, "", nil
}

func checkBetterBibTeXPrecondition(ctx context.Context, _ *rootFlags, _ *cobra.Command, _ capabilityEntry) (bool, string, error) {
	s, err := openStoreForRead(ctx, "zotio")
	if err != nil {
		return false, fmt.Sprintf("local store cannot be opened: %v", err), nil
	}
	if s == nil {
		return false, "local store is not synced, so Better BibTeX citation keys cannot be inspected", nil
	}
	defer s.Close()
	var citeable int
	var keyed int
	err = s.DB().QueryRowContext(ctx, `
SELECT
	COUNT(*) AS citeable,
	COUNT(CASE WHEN instr(COALESCE(json_extract(data,'$.data.extra'), ''), 'Citation Key: ') > 0
		OR COALESCE(json_extract(data,'$.data.citationKey'), '') != '' THEN 1 END) AS keyed
FROM resources
WHERE resource_type='items'
	AND json_extract(data,'$.data.itemType') NOT IN ('attachment','note','annotation')`).Scan(&citeable, &keyed)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, "no citeable items are available for Better BibTeX inspection", nil
		}
		return false, "", err
	}
	if citeable == 0 {
		return false, "the synced store has no citeable items for Better BibTeX inspection", nil
	}
	if keyed == 0 {
		return false, "no Better BibTeX citation keys were found in synced items (neither citationKey fields nor 'Citation Key:' Extra lines)", nil
	}
	return true, "", nil
}

func checkDesktopConnectorPrecondition(ctx context.Context, flags *rootFlags, cmd *cobra.Command, _ capabilityEntry) (bool, string, error) {
	conn, err := flags.newConnector()
	if err != nil {
		return false, err.Error(), nil
	}
	if err := connectorPing(ctx, conn); err != nil {
		detail := fmt.Sprintf("Zotero desktop connector is not reachable: %v", err)
		// import pdf's plan phase probes here too (field-report-2026-08-08
		// finding 8): recognition only exists in Zotero desktop, so name the
		// one alternative that needs no desktop instead of a dead end.
		if commandRegistryPath(cmd) == "import pdf" {
			detail += "; start Zotero desktop, or use 'zotio import doi <DOI>' which needs no desktop"
		}
		return false, detail, nil
	}
	return true, "", nil
}

// checkZoteroFileStoragePrecondition refuses a Web API stored-file upload that
// would silently bypass the operator's configured Zotero file store.
//
// It is scoped to invocations that actually request a stored attachment: the
// mode flag is read from the command so that --mode linked-file, which never
// uploads bytes, is unaffected.
func checkZoteroFileStoragePrecondition(ctx context.Context, flags *rootFlags, cmd *cobra.Command, _ capabilityEntry) (bool, string, error) {
	if !commandRequestsStoredUpload(cmd) {
		return true, "", nil
	}
	// --via connector never touches the Web API file uploader: the bytes go
	// through the running desktop into whatever file store it uses, which is
	// exactly the outcome this precondition exists to protect. Refusing that
	// route would refuse the fix.
	// Ask the same function the command asks, so this waiver cannot outlive the
	// behaviour it is waived for. commandRequestsStoredUpload above has already
	// established the stored mode.
	if usesConnectorReparentRoute("stored", flags) {
		return true, "", nil
	}
	// Only `attachments add` declares this precondition, and it always targets
	// an item that is already in the library.
	detail := storedUploadRefusal(ctx, flags, storedUploadToExistingItem)
	if detail == "" {
		return true, "", nil
	}
	return false, detail, nil
}

// commandRequestsStoredUpload reports whether this invocation asked for a
// stored attachment. Commands carrying no mode flag are treated as requesting
// one, since they would not declare the precondition otherwise.
func commandRequestsStoredUpload(cmd *cobra.Command) bool {
	if cmd == nil {
		return true
	}
	for _, name := range []string{"mode", "attach-mode"} {
		if f := cmd.Flags().Lookup(name); f != nil {
			return strings.TrimSpace(f.Value.String()) == "stored"
		}
	}
	return true
}
