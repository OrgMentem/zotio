// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

// preprintCheckFixSkip shapes the fix command's visible skip records after enrich skips.
type preprintCheckFixSkip struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Category string `json:"category"`
	Reason   string `json:"reason"`
	Error    string `json:"error,omitempty"`
}

const preprintFixLookupFailed = "lookup_failed"

// preprintCheckFixProposal carries a single schema-aware item update into the mutation engine.
// Fields and version come from the read plane and exist for the preview only:
// apply re-derives the write from the write plane's current copy of the item,
// using match, so a stale read cannot decide what is overwritten.
type preprintCheckFixProposal struct {
	Key     string         `json:"key"`
	Title   string         `json:"title"`
	DOI     string         `json:"doi"`
	Venue   string         `json:"venue,omitempty"`
	Year    int            `json:"year,omitempty"`
	Fields  map[string]any `json:"fields"`
	match   crossrefMatch
	version any
	// landed is what apply actually wrote, derived from the live item. The
	// mirror replays it instead of the planned Fields, whose Extra came from
	// the possibly older read plane.
	landed []mutation.Change
}

// newItemsPreprintCheckFixCmd adds the write-capable child while preserving preview-by-default mutation semantics.
func newItemsPreprintCheckFixCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int

	cmd := &cobra.Command{
		Use:   "fix",
		Short: "Apply published DOIs to preprints that CrossRef reports as published",
		Annotations: map[string]string{
			"mcp:read-only": "false",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateLimitFlag("--limit", flagLimit); err != nil {
				return err
			}
			readClient, err := flags.newClient()
			if err != nil {
				return err
			}
			// Detection reads must execute even under --dry-run, which only previews the
			// write (same convention as items new's /items/new fetch); otherwise the
			// client prints the GET instead of running it and the plan can never be built.
			readClient.DryRun = false

			var writeClient apiMutator
			ops, skipped, err := buildPreprintCheckFixOps(cmd, flags, readClient, func() apiMutator { return writeClient }, flagLimit)
			if err != nil {
				return err
			}

			if resolveMutationMode(flags).Apply && len(ops) > 0 {
				c, err := flags.newWriteClient()
				if err != nil {
					return err
				}
				writeClient = c
			}

			env, runErr := runMutation(cmd.Context(), flags, "items preprint-check fix", ops)
			return renderPreprintCheckFixMutation(cmd, flags, env, skipped, runErr)
		},
	}
	cmd.Flags().IntVar(&flagLimit, "limit", 20, "Maximum number of preprints to check")
	return cmd
}

// buildPreprintCheckFixOps reuses the detection flow and converts published matches into mutation ops.
func buildPreprintCheckFixOps(cmd *cobra.Command, flags *rootFlags, readClient zoteroGetter, writeClient func() apiMutator, limit int) ([]mutation.Op, []preprintCheckFixSkip, error) {
	candidates, err := fetchPreprintCheckFixCandidates(readClient, limit)
	if err != nil {
		return nil, nil, classifyAPIError(err, flags)
	}

	httpClient := &http.Client{Timeout: flags.timeout}
	proposals := make([]preprintCheckFixProposal, 0, len(candidates))
	skipped := make([]preprintCheckFixSkip, 0)
	for i, item := range candidates {
		if i > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		key := zoteroString(item, "key")
		title := zoteroString(item, "title")
		arxivID := extractArxivID(item)
		if arxivID == "" {
			skipped = append(skipped, preprintCheckFixSkip{Key: key, Title: title, Category: "preprint_check_fix", Reason: "no_arxiv_id"})
			continue
		}

		match, found, err := lookupCrossrefArxiv(cmd.Context(), httpClient, arxivID)
		if err != nil {
			if cmd.Context().Err() != nil {
				return nil, nil, err
			}
			skipped = append(skipped, preprintCheckFixSkip{Key: key, Title: title, Category: "preprint_check_fix", Reason: preprintFixLookupFailed, Error: err.Error()})
			continue
		}
		if !found {
			skipped = append(skipped, preprintCheckFixSkip{Key: key, Title: title, Category: "preprint_check_fix", Reason: "still_preprint"})
			continue
		}

		proposal, skip, ok := preprintCheckFixProposalForItem(item, match)
		if !ok {
			skipped = append(skipped, skip)
			continue
		}
		proposals = append(proposals, proposal)
	}

	return preprintCheckFixPlannedOps(cmd.Context(), proposals, writeClient), skipped, nil
}

// preprintCheckFixProposalForItem decides DOI-vs-Extra field changes without promoting the Zotero item type.
func preprintCheckFixProposalForItem(item map[string]any, match crossrefMatch) (preprintCheckFixProposal, preprintCheckFixSkip, bool) {
	key := zoteroString(item, "key")
	title := zoteroString(item, "title")
	fields, ok := preprintCheckFixFields(zoteroData(item), zoteroString(item, "extra"), match)
	if !ok {
		return preprintCheckFixProposal{}, preprintCheckFixSkip{Key: key, Title: title, Category: "preprint_check_fix", Reason: "doi_conflict"}, false
	}
	return preprintCheckFixProposal{
		Key:     key,
		Title:   title,
		DOI:     match.DOI,
		Venue:   match.Venue,
		Year:    match.Year,
		Fields:  fields,
		match:   match,
		version: preprintCheckItemVersion(item),
	}, preprintCheckFixSkip{}, true
}

// preprintCheckFixFields derives the DOI and Extra values to write from one
// snapshot of an item. ok is false when the item already carries a DOI that is
// neither empty nor an arXiv self DOI: that value is a published DOI (or the
// operator's own) and is never replaced. Extra is a wholesale replace, so
// extra must be the value the write will race against.
func preprintCheckFixFields(data map[string]any, extra string, match crossrefMatch) (fields map[string]any, ok bool) {
	fields = map[string]any{}
	if _, supportsDOI := data["DOI"]; supportsDOI {
		currentDOI := normalizeDOI(stringFromMap(data, "DOI"))
		if currentDOI != "" && !isArxivSelfDOI(currentDOI) {
			return nil, false
		}
		fields["DOI"] = match.DOI
	}
	fields["extra"] = appendPreprintCheckFixProvenance(extra, match)
	return fields, true
}

// appendPreprintCheckFixProvenance preserves existing Extra content while adding a dated publication provenance line.
func appendPreprintCheckFixProvenance(existing string, match crossrefMatch) string {
	line := fmt.Sprintf("zotio preprint-check: published as doi:%s%s on %s", match.DOI, preprintCheckPublicationSuffix(match), time.Now().UTC().Format("2006-01-02"))
	existing = strings.TrimRight(existing, "\n")
	if existing == "" {
		return line
	}
	return existing + "\n" + line
}

// preprintCheckPublicationSuffix formats optional CrossRef venue/year details in the provenance line.
func preprintCheckPublicationSuffix(match crossrefMatch) string {
	venue := strings.TrimSpace(match.Venue)
	switch {
	case venue != "" && match.Year > 0:
		return fmt.Sprintf(" (%s, %d)", venue, match.Year)
	case venue != "":
		return fmt.Sprintf(" (%s)", venue)
	case match.Year > 0:
		return fmt.Sprintf(" (%d)", match.Year)
	default:
		return ""
	}
}

// preprintCheckItemVersion keeps the Zotero version on each planned op for 412-safe PATCHes.
func preprintCheckItemVersion(item map[string]any) any {
	if version, ok := item["version"]; ok {
		return version
	}
	if data := zoteroData(item); data != nil {
		return data["version"]
	}
	return nil
}

// preprintCheckFixPlannedOps converts proposals into shared mutation-engine operations.
func preprintCheckFixPlannedOps(ctx context.Context, proposals []preprintCheckFixProposal, writeClient func() apiMutator) []mutation.Op {
	ops := make([]mutation.Op, 0, len(proposals))
	for i := range proposals {
		proposal := proposals[i]
		ops = append(ops, mutation.Op{
			ID:              "preprint_check_fix:" + proposal.Key,
			Key:             proposal.Key,
			Kind:            "preprint_check_fix",
			ExpectedVersion: mutationExpectedVersion(proposal.version),
			Changes:         preprintCheckFixChanges(proposal),
			Apply: func() (string, any, error) {
				return applyPreprintCheckFixProposal(ctx, writeClient(), &proposal)
			},
			// The planned Extra came from the read plane; the mirror must
			// replay the value apply derived from the live item instead.
			Effective: func() []mutation.Change { return proposal.landed },
		})
	}
	return ops
}

// preprintCheckFixChanges exposes field-level DOI/Extra changes in preview output.
func preprintCheckFixChanges(proposal preprintCheckFixProposal) []mutation.Change {
	return preprintCheckFixFieldChanges(proposal.Fields)
}

// preprintCheckFixFieldChanges lists the DOI and Extra values of one field set as changes.
func preprintCheckFixFieldChanges(fields map[string]any) []mutation.Change {
	changes := make([]mutation.Change, 0, len(fields))
	if doi, ok := fields["DOI"]; ok {
		changes = append(changes, mutation.Change{Field: "DOI", Add: doi})
	}
	if extra, ok := fields["extra"]; ok {
		changes = append(changes, mutation.Change{Field: "extra", Add: extra})
	}
	return changes
}

// applyPreprintCheckFixProposal PATCHes the item using enrich's typed API-error statuses.
//
// The write is derived here, from the write plane's current copy of the item,
// not taken from proposal.Fields. Planning read the item through the read plane
// (the local desktop API under hybrid routing), which lags the Web API: its
// Extra can miss a citation-key line or note written since the last sync, and
// its DOI can miss a published DOI added on the Web. Extra is replaced
// wholesale, so patching with the planned value would delete that content, and
// a fresh version alone would only authorize the overwrite. The DOI rule is
// therefore re-evaluated against the live item, and the PATCH is guarded by the
// version that same read returned: a later edit conflicts instead of being
// overwritten. The two planes version objects independently, so the proposal's
// read-plane version is never used as the precondition.
func applyPreprintCheckFixProposal(ctx context.Context, c apiMutator, proposal *preprintCheckFixProposal) (string, any, error) {
	if c == nil {
		err := errors.New("write client not initialized")
		return "failed", err.Error(), err
	}
	path := replacePathParam("/items/{itemKey}", "itemKey", proposal.Key)
	reader, ok := c.(writePlaneReader)
	if !ok {
		err := fmt.Errorf("client for %s cannot read the write plane; refusing to write without an If-Unmodified-Since-Version precondition", path)
		return "failed", err.Error(), err
	}
	currentData, version, err := reader.GetFromWriteBaseWithVersionContext(ctx, path, nil)
	if err != nil {
		return classifyWriteError(err)
	}
	var current map[string]any
	if err := json.Unmarshal(currentData, &current); err != nil {
		err = fmt.Errorf("decoding write-plane item %s: %w", path, err)
		return "failed", err.Error(), err
	}
	liveData, ok := current["data"].(map[string]any)
	if !ok {
		// Without the live data the Extra to append to is unknown, and a
		// wholesale replace would erase it.
		err := fmt.Errorf("write-plane item %s has no data object; refusing to rewrite Extra without it", path)
		return "failed", err.Error(), err
	}
	fields, ok := preprintCheckFixFields(liveData, stringFromMap(liveData, "extra"), proposal.match)
	if !ok {
		return "skipped", map[string]any{
			"reason":  "doi_conflict",
			"message": fmt.Sprintf("item already has DOI %s on the write plane; left unchanged", stringFromMap(liveData, "DOI")),
		}, nil
	}
	snapshot := &enrichWriteSnapshotMutator{
		apiMutator: c,
		reader:     reader,
		path:       path,
		data:       currentData,
		version:    version,
		pending:    true,
	}
	status, detail, err := patchWithWritePlaneVersion(ctx, snapshot, path, fields)
	if status == "applied" {
		proposal.landed = preprintCheckFixFieldChanges(fields)
	}
	return status, detail, err
}

// renderPreprintCheckFixMutation keeps skips visible in JSON/non-TTY envelopes and human terminal output.
// It returns runErr marked as rendered, so no second error object is printed.
func renderPreprintCheckFixMutation(cmd *cobra.Command, flags *rootFlags, env mutation.Envelope, skipped []preprintCheckFixSkip, runErr error) error {
	if len(skipped) > 0 {
		// Merge, never replace: an applied run keeps its run_id here.
		journal, _ := env.Journal.(map[string]any)
		if journal == nil {
			journal = map[string]any{}
		}
		journal["skipped"] = skipped
		env.Journal = journal
	}
	if err := renderMutation(cmd, flags, env, nil); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if len(skipped) > 0 && (flags == nil || !flags.asJSON) && isTerminal(out) {
		fmt.Fprintln(out, "Skips:")
		for _, skip := range skipped {
			fmt.Fprintf(out, "- %s %s (%s)\n", skip.Key, skip.Title, skip.Reason)
		}
	}
	if runErr == nil {
		failed := 0
		for _, skip := range skipped {
			if skip.Reason == preprintFixLookupFailed {
				failed++
			}
		}
		if failed > 0 {
			runErr = degradedErr(fmt.Errorf("items preprint-check fix: %d lookup(s) failed; plan incomplete", failed))
		}
	}
	return markRendered(runErr)
}
