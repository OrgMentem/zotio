// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(marketing-heroes): preview-first remediation for arXiv preprints that now have published DOIs.

package cli

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

// PATCH(marketing-heroes): shape the fix command's visible skip records after enrich skips.
type preprintCheckFixSkip struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Category string `json:"category"`
	Reason   string `json:"reason"`
}

// PATCH(marketing-heroes): carry a single schema-aware PATCH proposal into the mutation engine.
type preprintCheckFixProposal struct {
	Key     string         `json:"key"`
	Title   string         `json:"title"`
	DOI     string         `json:"doi"`
	Venue   string         `json:"venue,omitempty"`
	Year    int            `json:"year,omitempty"`
	Fields  map[string]any `json:"fields"`
	version any
}

// PATCH(marketing-heroes): add the write-capable child while preserving preview-by-default mutation semantics.
func newItemsPreprintCheckFixCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int

	cmd := &cobra.Command{
		Use:   "fix",
		Short: "Apply published DOIs to preprints that CrossRef reports as published",
		Annotations: map[string]string{
			"mcp:read-only":                 "false",
			"pp:destructive":                "false",
			"pp:supports-dry-run":           "true",
			"pp:requires-allow-destructive": "false",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			readClient, err := flags.newClient()
			if err != nil {
				return err
			}
			// PATCH(marketing-heroes): detection reads must execute even under
			// --dry-run, which only previews the write (same convention as
			// items new's /items/new fetch); otherwise the client prints the
			// GET instead of running it and the plan can never be built.
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
			renderErr := renderPreprintCheckFixMutation(cmd, flags, env, skipped)
			if renderErr != nil {
				return renderErr
			}
			return runErr
		},
	}
	cmd.Flags().IntVar(&flagLimit, "limit", 20, "Maximum number of preprints to check")
	return cmd
}

// PATCH(marketing-heroes): reuse the detection flow and convert published matches into mutation ops.
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
			return nil, nil, err
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

	return preprintCheckFixPlannedOps(proposals, writeClient), skipped, nil
}

// PATCH(marketing-heroes): decide DOI-vs-Extra field changes without promoting the Zotero item type.
func preprintCheckFixProposalForItem(item map[string]any, match crossrefMatch) (preprintCheckFixProposal, preprintCheckFixSkip, bool) {
	key := zoteroString(item, "key")
	title := zoteroString(item, "title")
	data := zoteroData(item)
	_, supportsDOI := data["DOI"]
	fields := map[string]any{}
	if supportsDOI {
		currentDOI := normalizeDOI(stringFromMap(data, "DOI"))
		if currentDOI == "" || isArxivSelfDOI(currentDOI) {
			fields["DOI"] = match.DOI
		} else {
			return preprintCheckFixProposal{}, preprintCheckFixSkip{Key: key, Title: title, Category: "preprint_check_fix", Reason: "doi_conflict"}, false
		}
	}
	fields["extra"] = appendPreprintCheckFixProvenance(zoteroString(item, "extra"), match)
	return preprintCheckFixProposal{
		Key:     key,
		Title:   title,
		DOI:     match.DOI,
		Venue:   match.Venue,
		Year:    match.Year,
		Fields:  fields,
		version: preprintCheckItemVersion(item),
	}, preprintCheckFixSkip{}, true
}

// PATCH(marketing-heroes): preserve existing Extra content while adding a dated publication provenance line.
func appendPreprintCheckFixProvenance(existing string, match crossrefMatch) string {
	line := fmt.Sprintf("zotio preprint-check: published as doi:%s%s on %s", match.DOI, preprintCheckPublicationSuffix(match), time.Now().UTC().Format("2006-01-02"))
	existing = strings.TrimRight(existing, "\n")
	if existing == "" {
		return line
	}
	return existing + "\n" + line
}

// PATCH(marketing-heroes): format optional CrossRef venue/year details in the provenance line.
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

// PATCH(marketing-heroes): keep the Zotero version on each planned op for 412-safe PATCHes.
func preprintCheckItemVersion(item map[string]any) any {
	if version, ok := item["version"]; ok {
		return version
	}
	if data := zoteroData(item); data != nil {
		return data["version"]
	}
	return nil
}

// PATCH(marketing-heroes): convert proposals into shared mutation-engine operations.
func preprintCheckFixPlannedOps(proposals []preprintCheckFixProposal, writeClient func() apiMutator) []mutation.Op {
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
				return applyPreprintCheckFixProposal(writeClient(), proposal)
			},
		})
	}
	return ops
}

// PATCH(marketing-heroes): expose field-level DOI/Extra changes in preview output.
func preprintCheckFixChanges(proposal preprintCheckFixProposal) []mutation.Change {
	changes := make([]mutation.Change, 0, len(proposal.Fields))
	if doi, ok := proposal.Fields["DOI"]; ok {
		changes = append(changes, mutation.Change{Field: "DOI", Add: doi})
	}
	if extra, ok := proposal.Fields["extra"]; ok {
		changes = append(changes, mutation.Change{Field: "extra", Add: extra})
	}
	return changes
}

// PATCH(marketing-heroes): apply the item PATCH using enrich's typed API-error statuses.
func applyPreprintCheckFixProposal(c apiMutator, proposal preprintCheckFixProposal) (string, any, error) {
	if c == nil {
		err := errors.New("write client not initialized")
		return "failed", err.Error(), err
	}
	body := map[string]any{"version": proposal.version}
	for key, value := range proposal.Fields {
		body[key] = value
	}
	path := replacePathParam("/items/{itemKey}", "itemKey", proposal.Key)
	if _, _, err := c.Patch(path, body); err != nil {
		return enrichErrorStatus(err)
	}
	return "applied", nil, nil
}

// PATCH(marketing-heroes): keep skips visible in JSON/non-TTY envelopes and human terminal output.
func renderPreprintCheckFixMutation(cmd *cobra.Command, flags *rootFlags, env mutation.Envelope, skipped []preprintCheckFixSkip) error {
	if len(skipped) > 0 {
		env.Journal = map[string]any{"skipped": skipped}
	}
	if err := renderMutation(cmd, flags, env, nil); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if len(skipped) == 0 || flags != nil && flags.asJSON || !isTerminal(out) {
		return nil
	}
	fmt.Fprintln(out, "Skips:")
	for _, skip := range skipped {
		fmt.Fprintf(out, "- %s %s (%s)\n", skip.Key, skip.Title, skip.Reason)
	}
	return nil
}
