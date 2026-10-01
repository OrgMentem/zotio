// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/cliutil"
)

// FeedbackEntry is one line in the local feedback ledger. Every run of
// the feedback command appends one entry; upstream POST is a separate,
// optional step that sends the same entry unchanged. AgentID is set only
// from the explicit --agent-id flag, never from the ambient environment.
type FeedbackEntry struct {
	Text      string    `json:"text"`
	CLI       string    `json:"cli"`
	Version   string    `json:"version"`
	AgentID   string    `json:"agent_id,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

const feedbackMaxTextLen = 4096

func feedbackFilePath() (string, error) {
	home, err := cliutil.HomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	dir := filepath.Join(home, ".zotio")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating state dir: %w", err)
	}
	return filepath.Join(dir, "feedback.jsonl"), nil
}

// FeedbackEndpointConfigured reports whether an upstream feedback URL
// is available. Surfaced via agent-context so introspecting agents know
// whether their feedback will ship upstream.
func FeedbackEndpointConfigured() bool {
	endpoint, err := feedbackEndpoint()
	return err == nil && endpoint != ""
}

func feedbackEndpoint() (string, error) {
	endpoint := strings.TrimSpace(os.Getenv("ZOTERO_FEEDBACK_ENDPOINT"))
	if endpoint == "" {
		return "", nil
	}
	// validateExternalHTTPURL quotes an unparseable URL in its error, and
	// feedback receivers often carry their credential in the path or query,
	// so reject a malformed value without echoing it.
	if _, err := url.Parse(endpoint); err != nil {
		return "", errors.New("not a valid URL")
	}
	// feedback sends may contain
	// private CLI context, so only HTTPS public endpoints are accepted.
	if err := validateExternalHTTPURL(endpoint, true); err != nil {
		return "", err
	}
	return endpoint, nil
}

func feedbackAutoSend() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("ZOTERO_FEEDBACK_AUTO_SEND")))
	return v == "1" || v == "true" || v == "yes"
}

func appendFeedback(entry FeedbackEntry) error {
	p, err := feedbackFilePath()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening feedback ledger: %w", err)
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(entry)
}

func postFeedback(endpoint string, entry FeedbackEntry) error {
	// keep direct helper calls as
	// constrained as feedbackEndpoint().
	if err := validateExternalHTTPURL(endpoint, true); err != nil {
		return err
	}
	body, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building feedback request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "zotio/feedback")
	client := externalFetchHTTPClient(&http.Client{Timeout: 15 * time.Second}, true)
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		// keep a trusted HTTPS
		// endpoint from redirecting feedback into an internal target.
		return http.ErrUseLastResponse
	}
	resp, err := client.Do(req)
	if err != nil {
		// net/http strips only the password from the URL it reports; the
		// path and query, where receivers carry their credential, would
		// otherwise reach stderr and the JSON result.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			urlErr.URL = webhookOrigin(urlErr.URL)
		}
		return fmt.Errorf("posting feedback: %w", err)
	}
	defer resp.Body.Close()
	return externalHTTPPostStatusError("feedback endpoint", resp)
}

func newFeedbackCmd(flags *rootFlags) *cobra.Command {
	var useStdin bool
	var send bool
	var agentID string
	cmd := &cobra.Command{
		Use:   "feedback [text]",
		Short: "Record feedback about this CLI (local by default; upstream opt-in)",
		Long: `Feedback is captured locally first at ~/.zotio/feedback.jsonl.
When ` + "`ZOTERO_FEEDBACK_ENDPOINT`" + ` is set and either --send is
passed or ` + "`ZOTERO_FEEDBACK_AUTO_SEND=true`" + `, the entry is
POSTed as JSON after the local write.

The entry, stored locally and POSTed unchanged, holds exactly these
fields: text, cli ("zotio"), version, timestamp (UTC), and agent_id
only when --agent-id is passed. No environment variables, config,
credentials, or library data are added.

--send makes delivery required. If the endpoint is unset or unusable
the command exits 10; if the POST fails it exits 5. The local entry is
kept in both cases. An automatic send (ZOTERO_FEEDBACK_AUTO_SEND) stays
best-effort: a failure is a warning and the command exits 0. Results
and errors name only the endpoint's scheme and host.

Write what surprised you or tripped you up, not a bug report. The
loop is: agent notices friction -> one invocation -> captured -> the
maintainer sees it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var text string
			if useStdin {
				data, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("reading stdin: %w", err)
				}
				text = strings.TrimSpace(string(data))
			} else if len(args) > 0 {
				text = strings.Join(args, " ")
			}
			text = strings.TrimSpace(text)
			if text == "" {
				return fmt.Errorf("feedback text is empty (pass arguments or --stdin)")
			}
			truncated := false
			if len(text) > feedbackMaxTextLen {
				text = text[:feedbackMaxTextLen]
				truncated = true
			}

			entry := FeedbackEntry{
				Text:      text,
				CLI:       "zotio",
				Version:   version,
				AgentID:   strings.TrimSpace(agentID),
				Timestamp: time.Now().UTC(),
			}
			if err := withInstallationWriterLock(cmd, flags, "feedback", func() error {
				return appendFeedback(entry)
			}); err != nil {
				return err
			}

			upstreamResult := map[string]any{"sent": false}
			var upstreamErr error
			if send || feedbackAutoSend() {
				endpoint, endpointErr := feedbackEndpoint()
				switch {
				case endpointErr != nil:
					upstreamErr = configErr(fmt.Errorf("feedback recorded locally but not sent: ZOTERO_FEEDBACK_ENDPOINT is unusable: %w", endpointErr))
				case endpoint == "":
					if send {
						upstreamErr = configErr(errors.New("feedback recorded locally but not sent: --send requires ZOTERO_FEEDBACK_ENDPOINT set to a public https:// URL"))
					}
				default:
					if err := postFeedback(endpoint, entry); err != nil {
						upstreamErr = apiErr(fmt.Errorf("feedback recorded locally but upstream POST failed: %w", err))
					} else {
						upstreamResult["sent"] = true
						upstreamResult["endpoint"] = webhookOrigin(endpoint)
					}
				}
				if upstreamErr != nil {
					upstreamResult["error"] = upstreamErr.Error()
					// Only an explicit --send is a required delivery; the
					// ambient auto-send stays a best-effort warning.
					if !send {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", upstreamErr)
						upstreamErr = nil
					}
				}
			}

			if flags.asJSON {
				if err := printJSONFiltered(cmd.OutOrStdout(), map[string]any{
					"recorded":  true,
					"truncated": truncated,
					"upstream":  upstreamResult,
					"entry":     entry,
				}, flags); err != nil {
					return err
				}
				return upstreamErr
			}
			fmt.Fprintf(cmd.OutOrStdout(), "feedback recorded locally (%d chars%s)\n", len(text), func() string {
				if truncated {
					return ", truncated"
				}
				return ""
			}())
			if sent, _ := upstreamResult["sent"].(bool); sent {
				fmt.Fprintf(cmd.OutOrStdout(), "upstream POST: %v\n", upstreamResult["endpoint"])
			}
			return upstreamErr
		},
	}
	cmd.Flags().BoolVar(&useStdin, "stdin", false, "Read feedback body from stdin rather than arguments")
	cmd.Flags().BoolVar(&send, "send", false, "POST to the configured feedback endpoint in addition to local write; exits non-zero if the POST does not happen")
	cmd.Flags().StringVar(&agentID, "agent-id", "", "Identifier to record (and send) as agent_id; omitted unless passed")

	cmd.AddCommand(newFeedbackListCmd(flags))
	return cmd
}

type feedbackListResult struct {
	Entries             []FeedbackEntry `json:"entries"`
	SkippedCorruptLines int             `json:"skipped_corrupt_lines"`
}

func newFeedbackListCmd(flags *rootFlags) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recent feedback entries",
		Example: `  zotio feedback list
  zotio feedback list --limit 5
  zotio feedback list --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := feedbackFilePath()
			if err != nil {
				return err
			}
			data, err := os.ReadFile(p)
			if err != nil {
				if os.IsNotExist(err) {
					if flags.asJSON {
						return printJSONFiltered(cmd.OutOrStdout(), []FeedbackEntry{}, flags)
					}
					return nil
				}
				return err
			}
			var entries []FeedbackEntry
			skippedCorruptLines := 0
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				var e FeedbackEntry
				if err := json.Unmarshal([]byte(line), &e); err != nil {
					skippedCorruptLines++
					continue
				}
				entries = append(entries, e)
			}
			if limit > 0 && limit < len(entries) {
				entries = entries[len(entries)-limit:]
			}
			if skippedCorruptLines > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: skipped %d corrupt feedback journal line(s)\n", skippedCorruptLines)
				if flags.asJSON {
					if err := printJSONFiltered(cmd.OutOrStdout(), feedbackListResult{
						Entries:             entries,
						SkippedCorruptLines: skippedCorruptLines,
					}, flags); err != nil {
						return err
					}
				} else if err := printJSONFiltered(cmd.OutOrStdout(), entries, flags); err != nil {
					return err
				}
				return degradedErr(fmt.Errorf("feedback list: skipped %d corrupt journal line(s); results incomplete", skippedCorruptLines))
			}
			return printJSONFiltered(cmd.OutOrStdout(), entries, flags)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "Maximum number of recent entries to return")
	return cmd
}
