// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestAttachmentsLegacyModeKeepsJSONStreamAttachments(t *testing.T) {
	for _, tc := range []struct {
		name, key      string
		quiet, failure bool
	}{
		{name: "preview", key: "ABCD2345"},
		{name: "usage", key: "ABCD1234", failure: true},
		{name: "quiet", key: "ABCD2345", quiet: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags := &rootFlags{asJSON: true, agent: true, dryRun: true, quiet: tc.quiet, maxChanges: -1}
			cmd := newAttachmentsAddCmd(flags)
			var stdout, stderr bytes.Buffer
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			counted := &stdoutCounter{w: &stdout}
			cmd.SetOut(counted)
			cmd.SetErr(&stderr)
			path := writeUploadFixture(t, "paper.pdf", []byte(uploadFixturePDF))
			cmd.SetArgs([]string{tc.key, path, "--mode", "linked-file"})
			err := cmd.ExecuteContext(context.Background())
			if (err != nil) != tc.failure {
				t.Fatalf("error=%v", err)
			}
			if err != nil {
				recordErrorOutputMode(errorOutputMode{json: true, quiet: tc.quiet, wroteStdout: counted.Wrote()})
				defer recordErrorOutputMode(errorOutputMode{})
				if code := ReportError(&stdout, &stderr, err); code != 2 {
					t.Fatalf("code=%d", code)
				}
			}
			if tc.quiet {
				if stdout.Len() != 0 || stderr.Len() != 0 {
					t.Fatalf("quiet output: stdout=%q stderr=%q", stdout.String(), stderr.String())
				}
				return
			}
			var result map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("stdout is not one JSON value: %q: %v", stdout.String(), err)
			}
			if tc.failure && result["kind"] != "usage" {
				t.Fatalf("missing usage envelope: %#v", result)
			}
			if !strings.Contains(stderr.String(), "deprecated") {
				t.Fatalf("stderr=%q", stderr.String())
			}
		})
	}
}
