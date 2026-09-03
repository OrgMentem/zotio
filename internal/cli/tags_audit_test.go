// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zotio/internal/mutation"
	"zotio/internal/store"
)

// tagAuditFixtureRows builds the tagRows/countRows shape QueryRaw returns for
// tagAuditDistinctQuery/tagAuditCountQuery from a set of (name, count) pairs.
func tagAuditFixtureRows(counted map[string]int) (tagRows, countRows []map[string]any) {
	for name, count := range counted {
		tagRows = append(tagRows, map[string]any{"tag_name": name})
		countRows = append(countRows, map[string]any{"tag_name": name, "item_count": count})
	}
	return tagRows, countRows
}

// TestBuildTagAuditPlansFrequencyDefaultIsByteIdentical pins the frequency
// policy -- the default, and the only policy that existed before --prefer --
// to the exact plan the pre---prefer implementation produced for this
// fixture (mirrors duplicateTagAuditItems in tags_audit_fix_test.go: "Data
// Science" x2, "data science" x1, "Data  Science" x1). Any drift here means
// --prefer changed today's default behavior, which the finding forbids.
func TestBuildTagAuditPlansFrequencyDefaultIsByteIdentical(t *testing.T) {
	tagRows, countRows := tagAuditFixtureRows(map[string]int{
		"Data Science":  2,
		"data science":  1,
		"Data  Science": 1,
	})
	plans := buildTagAuditPlans(tagRows, countRows, tagAuditPreferFrequency, nil)
	if len(plans) != 1 {
		t.Fatalf("plans = %+v, want exactly 1 group", plans)
	}
	p := plans[0]
	if p.Canonical != "Data Science" {
		t.Errorf("canonical = %q, want %q", p.Canonical, "Data Science")
	}
	wantAliases := []string{"Data  Science", "data science"}
	if strings.Join(p.Aliases, "|") != strings.Join(wantAliases, "|") {
		t.Errorf("aliases = %v, want %v", p.Aliases, wantAliases)
	}
	wantCommands := []string{
		`zotio tags rename --from 'Data  Science' --to 'Data Science'`,
		`zotio tags rename --from 'data science' --to 'Data Science'`,
	}
	if strings.Join(p.RenameCommands, "\n") != strings.Join(wantCommands, "\n") {
		t.Errorf("rename commands = %v, want %v", p.RenameCommands, wantCommands)
	}
	if p.TotalItems != 4 {
		t.Errorf("total items = %d, want 4", p.TotalItems)
	}
	if p.AutomaticSkipped {
		t.Errorf("automatic_skipped = true, want false: frequency never falls back")
	}
}

// TestBuildTagAuditPlansPreferPolicies exercises the three case policies
// against the exact groups the field report calls out as internally
// inconsistent under frequency alone. Counts are deliberately stacked
// against the case-correct spelling so a passing test proves the policy
// synthesizes the canonical from the group's normalized name rather than
// just picking whichever variant happens to be most common.
func TestBuildTagAuditPlansPreferPolicies(t *testing.T) {
	tests := []struct {
		name    string
		prefer  tagAuditPrefer
		counted map[string]int
		want    string
	}{
		{
			name:   "lower",
			prefer: tagAuditPreferLower,
			// "Children" is used more, but --prefer lower must still pick
			// the all-lowercase spelling.
			counted: map[string]int{"Children": 5, "children": 2},
			want:    "children",
		},
		{
			name:   "sentence",
			prefer: tagAuditPreferSentence,
			// "Cognitive Psychology" (title case) is used more, but
			// --prefer sentence must pick the sentence-case spelling.
			counted: map[string]int{"Cognitive Psychology": 4, "Cognitive psychology": 1},
			want:    "Cognitive psychology",
		},
		{
			name:   "title",
			prefer: tagAuditPreferTitle,
			// "Developmental psychology" (sentence case) is used more, but
			// --prefer title must pick the title-case spelling.
			counted: map[string]int{"Developmental psychology": 3, "Developmental Psychology": 1},
			want:    "Developmental Psychology",
		},
		{
			name:   "title synthesizes when no existing variant matches",
			prefer: tagAuditPreferTitle,
			counted: map[string]int{
				"MAGNETIC RESONANCE IMAGING": 2,
				"magnetic resonance imaging": 2,
			},
			want: "Magnetic Resonance Imaging",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tagRows, countRows := tagAuditFixtureRows(tc.counted)
			plans := buildTagAuditPlans(tagRows, countRows, tc.prefer, nil)
			if len(plans) != 1 {
				t.Fatalf("plans = %+v, want exactly 1 group", plans)
			}
			if got := plans[0].Canonical; got != tc.want {
				t.Errorf("canonical = %q, want %q", got, tc.want)
			}
			if plans[0].AutomaticSkipped {
				t.Errorf("automatic_skipped = true, want false: no type-1 tags in this fixture")
			}
			for _, alias := range plans[0].Aliases {
				if alias == tc.want {
					t.Errorf("aliases = %v, canonical %q must not also be listed as its own alias", plans[0].Aliases, tc.want)
				}
			}
		})
	}
}

func TestTagAuditCasePoliciesRespectIntraTokenBoundaries(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantTitle    string
		wantSentence string
	}{
		{name: "wt2 hyphen", input: "wt2-Case", wantTitle: "Wt2-Case", wantSentence: "Wt2-case"},
		{name: "meta analysis", input: "meta-analysis", wantTitle: "Meta-Analysis", wantSentence: "Meta-analysis"},
		{name: "Carhart Harris", input: "Carhart-Harris", wantTitle: "Carhart-Harris", wantSentence: "Carhart-harris"},
		{name: "double blind", input: "double-blind", wantTitle: "Double-Blind", wantSentence: "Double-blind"},
		{name: "fMRI adaptation", input: "fMRI-adaptation", wantTitle: "Fmri-Adaptation", wantSentence: "Fmri-adaptation"},
		{name: "O'Brien", input: "O'Brien", wantTitle: "O'Brien", wantSentence: "O'brien"},
		{name: "don't", input: "don't", wantTitle: "Don't", wantSentence: "Don't"},
		{name: "trailing hyphen", input: "meta-", wantTitle: "Meta-", wantSentence: "Meta-"},
		{name: "doubled hyphen", input: "a--b", wantTitle: "A--B", wantSentence: "A--b"},
		{name: "slash separator", input: "dose-response/side-effect", wantTitle: "Dose-Response/Side-Effect", wantSentence: "Dose-response/side-effect"},
		{name: "multi-word tag", input: "multi word tag", wantTitle: "Multi Word Tag", wantSentence: "Multi word tag"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			normalized := normalizeTagAuditName(tc.input)
			if got := tagAuditTitleCase(normalized); got != tc.wantTitle {
				t.Errorf("title case = %q, want %q", got, tc.wantTitle)
			}
			if got := tagAuditSentenceCase(normalized); got != tc.wantSentence {
				t.Errorf("sentence case = %q, want %q", got, tc.wantSentence)
			}
		})
	}
}

func TestTagAuditCasePoliciesPreserveExistingInternalCase(t *testing.T) {
	tests := []struct {
		input        string
		wantTitle    string
		wantSentence string
	}{
		{input: "McDonald", wantTitle: "McDonald", wantSentence: "McDonald"},
		{input: "Parkinson's", wantTitle: "Parkinson's", wantSentence: "Parkinson's"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			if got := tagAuditTitleCase(tc.input); got != tc.wantTitle {
				t.Errorf("title case = %q, want %q", got, tc.wantTitle)
			}
			if got := tagAuditSentenceCase(tc.input); got != tc.wantSentence {
				t.Errorf("sentence case = %q, want %q", got, tc.wantSentence)
			}
		})
	}
}

// TestBuildTagAuditPlansSkipsPreferForAutomaticTags is the type-1 decision:
// a group containing an automatic (type 1) tag -- e.g. a MeSH term a
// translator imported -- must not have a case policy imposed on it, because
// the source casing is frequently the correct one (Title Case for MeSH) and
// a blanket rewrite would corrupt it. The group falls back to the frequency
// canonical instead, and AutomaticSkipped flags that to the caller.
func TestBuildTagAuditPlansSkipsPreferForAutomaticTags(t *testing.T) {
	tagRows, countRows := tagAuditFixtureRows(map[string]int{
		"Magnetic Resonance Imaging": 3,
		"magnetic resonance imaging": 1,
	})
	automaticTags := map[string]bool{"Magnetic Resonance Imaging": true}

	// --prefer lower would normally force "magnetic resonance imaging".
	plans := buildTagAuditPlans(tagRows, countRows, tagAuditPreferLower, automaticTags)
	if len(plans) != 1 {
		t.Fatalf("plans = %+v, want exactly 1 group", plans)
	}
	p := plans[0]
	if !p.AutomaticSkipped {
		t.Fatalf("automatic_skipped = false, want true for a group carrying a type-1 tag")
	}
	// Falls back to the frequency canonical (most-used spelling), NOT the
	// --prefer lower target.
	if p.Canonical != "Magnetic Resonance Imaging" {
		t.Errorf("canonical = %q, want frequency fallback %q", p.Canonical, "Magnetic Resonance Imaging")
	}

	titlePlans := buildTagAuditPlans(tagRows, countRows, tagAuditPreferTitle, automaticTags)
	if len(titlePlans) != 1 || !titlePlans[0].AutomaticSkipped {
		t.Fatalf("title plans = %+v, want one automatic-skipped group", titlePlans)
	}
	if titlePlans[0].Canonical != p.Canonical {
		t.Errorf("title automatic fallback %q != frequency canonical %q", titlePlans[0].Canonical, p.Canonical)
	}

	// The same group under the default frequency policy never needed to
	// "skip" anything -- it was always going to use frequency.
	freqPlans := buildTagAuditPlans(tagRows, countRows, tagAuditPreferFrequency, automaticTags)
	if freqPlans[0].AutomaticSkipped {
		t.Errorf("frequency policy reported automatic_skipped = true, want false (nothing to skip)")
	}
	if freqPlans[0].Canonical != p.Canonical {
		t.Errorf("frequency canonical %q != automatic-fallback canonical %q, fix and audit would disagree", freqPlans[0].Canonical, p.Canonical)
	}
}

func TestParseTagAuditPreferRejectsUnknownValue(t *testing.T) {
	if _, err := parseTagAuditPrefer("shout"); err == nil {
		t.Fatal("parseTagAuditPrefer(\"shout\") = nil error, want a validation error")
	}
	for _, v := range []string{"frequency", "sentence", "title", "lower"} {
		if _, err := parseTagAuditPrefer(v); err != nil {
			t.Errorf("parseTagAuditPrefer(%q) = %v, want nil", v, err)
		}
	}
}

// runTagsAuditReportCmd runs `zotio tags audit` (no subcommand) against a
// seeded local store and returns the plain-text human report.
func runTagsAuditReportCmd(t *testing.T, args ...string) string {
	t.Helper()
	cmd := newTagsAuditCmd(&rootFlags{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("tags audit: %v", err)
	}
	return out.String()
}

// TestTagsAuditReportNamesFixCommand is finding #2: the report used to hand
// back nothing but copy-pasteable `zotio tags rename` lines with no mention
// that `tags audit fix` batches them all. The report must lead with that
// pointer, and carry --prefer through it when set, so the batch reproduces
// exactly what was shown.
func TestTagsAuditReportNamesFixCommand(t *testing.T) {
	seedTagsAuditFixStore(t, duplicateTagAuditItems())

	out := runTagsAuditReportCmd(t)
	if !strings.Contains(out, "tags audit fix") {
		t.Fatalf("report does not mention `tags audit fix`:\n%s", out)
	}
	if strings.Contains(out, "--prefer") {
		t.Errorf("default-prefer report should not mention --prefer:\n%s", out)
	}

	outPrefer := runTagsAuditReportCmd(t, "--prefer", "title")
	if !strings.Contains(outPrefer, "tags audit fix") {
		t.Fatalf("report does not mention `tags audit fix`:\n%s", outPrefer)
	}
	if !strings.Contains(outPrefer, "--prefer title") {
		t.Fatalf("report with --prefer title does not carry it into the fix pointer:\n%s", outPrefer)
	}
}

func TestTagsAuditResultsIsArrayAndHumanPlanStaysIntact(t *testing.T) {
	seedTagsAuditFixStore(t, duplicateTagAuditItems())

	jsonFlags := &rootFlags{asJSON: true}
	jsonCmd := newTagsAuditCmd(jsonFlags)
	var jsonOut bytes.Buffer
	jsonCmd.SetOut(&jsonOut)
	jsonCmd.SetErr(io.Discard)
	if err := jsonCmd.Execute(); err != nil {
		t.Fatalf("tags audit --json: %v", err)
	}
	env := decodeResultsArrayEnvelope(t, jsonOut.Bytes())
	if len(env.Results) == 0 {
		t.Fatalf("tags audit results is empty: %s", jsonOut.String())
	}
	if _, ok := env.Results[0]["canonical"]; !ok {
		t.Fatalf("tags audit results[0] missing canonical plan: %s", jsonOut.String())
	}

	human := runTagsAuditReportCmd(t)
	for _, want := range []string{
		"Summary",
		"Merge plan",
		"Run zotio tags audit fix --yes to apply every rename below in one batch",
		"zotio tags rename",
	} {
		if !strings.Contains(human, want) {
			t.Fatalf("human tags audit output missing %q:\n%s", want, human)
		}
	}
}

// runTagsAuditFixCmdArgs is runTagsAuditFixCmd (tags_audit_fix_test.go) but
// accepts extra args after "fix", to exercise --prefer.
func runTagsAuditFixCmdArgs(t *testing.T, flags *rootFlags, baseURL string, extra ...string) (mutation.Envelope, string, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", baseURL+"/users/0")
	cmd := newTagsAuditCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(append([]string{"fix"}, extra...))
	err := cmd.Execute()
	var env mutation.Envelope
	if out.Len() > 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
			t.Fatalf("decode envelope %q: %v", out.String(), decodeErr)
		}
	}
	return env, out.String(), err
}

// TestTagsAuditFixAppliesSamePreferCanonicalAsAudit closes finding #3's
// closing requirement: the audit report and `tags audit fix` must never
// disagree about the outcome. Frequency alone would canonicalize on
// "Cognitive Psychology" (2 items vs 1), but --prefer sentence must steer
// both the audit plan and the applied batch to "Cognitive psychology".
func TestTagsAuditFixAppliesSamePreferCanonicalAsAudit(t *testing.T) {
	items := []jsonRM{
		{"K1", `{"key":"K1","version":1,"data":{"key":"K1","tags":[{"tag":"Cognitive Psychology","type":0}]}}`},
		{"K2", `{"key":"K2","version":2,"data":{"key":"K2","tags":[{"tag":"Cognitive Psychology","type":0}]}}`},
		{"K3", `{"key":"K3","version":3,"data":{"key":"K3","tags":[{"tag":"Cognitive psychology","type":0}]}}`},
	}
	seedTagsAuditFixStore(t, jsonRMList(items))

	// The audit report (as the user would read it) must show the same
	// canonical the batch fix is about to apply.
	report := runTagsAuditReportCmd(t, "--prefer", "sentence")
	if !strings.Contains(report, "--to 'Cognitive psychology'") {
		t.Fatalf("audit report canonical != expected sentence-case target:\n%s", report)
	}
	if strings.Contains(report, "--to 'Cognitive Psychology'") {
		t.Fatalf("audit report picked the frequency (title-case) canonical instead of --prefer sentence:\n%s", report)
	}

	patches := map[string]map[string]any{}
	raw := jsonRMList(items)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveTagAuditFixItem(w, r, raw) {
			return
		}
		if r.Method != http.MethodPatch {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		patches[r.URL.Path] = body
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	env, _, err := runTagsAuditFixCmdArgs(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, srv.URL, "--prefer", "sentence")
	if err != nil {
		t.Fatalf("tags audit fix --prefer sentence: %v", err)
	}
	if !env.OK || env.Mode != "apply" {
		t.Fatalf("apply envelope = %+v, want ok apply", env)
	}
	// Only K1 and K2 carry the alias "Cognitive Psychology"; K3 is already
	// the sentence-case canonical and needs no write.
	for _, path := range []string{"/users/0/items/K1", "/users/0/items/K2"} {
		body, ok := patches[path]
		if !ok {
			t.Fatalf("missing PATCH for %s; got %#v", path, patches)
		}
		tags, _ := body["tags"].([]any)
		if len(tags) != 1 {
			t.Fatalf("PATCH %s tags = %#v, want one tag", path, body["tags"])
		}
		tag, _ := tags[0].(map[string]any)
		if tag["tag"] != "Cognitive psychology" {
			t.Errorf("PATCH %s renamed to %v, want sentence-case canonical %q", path, tag["tag"], "Cognitive psychology")
		}
	}
	if _, wrote := patches["/users/0/items/K3"]; wrote {
		t.Errorf("K3 already matched the canonical spelling and should not have been PATCHed")
	}
}

type jsonRM struct {
	key string
	raw string
}

func jsonRMList(items []jsonRM) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(items))
	for _, it := range items {
		out = append(out, json.RawMessage(it.raw))
	}
	return out
}

// runTagsAuditJSONCmd runs `zotio tags audit --json` against a seeded store
// and returns the raw envelope bytes.
func runTagsAuditJSONCmd(t *testing.T, args ...string) []byte {
	t.Helper()
	cmd := newTagsAuditCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("tags audit --json: %v", err)
	}
	return out.Bytes()
}

// tagAuditEnvelope decodes both halves of the `tags audit --json` output: the
// merge plan its existing consumers read from .results, and the finding
// envelope --keys-from ingests from .findings.
type tagAuditEnvelope struct {
	Results  []map[string]any `json:"results"`
	Findings []Finding        `json:"findings"`
}

func decodeTagAuditEnvelope(t *testing.T, out []byte) tagAuditEnvelope {
	t.Helper()
	var env tagAuditEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("decode tags audit envelope %s: %v", out, err)
	}
	return env
}

// TestTagsAuditEmitsTagDriftFindingsBesidePlan is the migration contract:
// `tags audit --json` used to emit only the plan shape, which carried no
// kind, no severity and no recommended action, so nothing downstream could
// treat it as a finding. The findings must appear WITHOUT displacing the
// plan: .results is the documented list-read shape (walktest N-1) and
// rename_commands inside it is the manual escape hatch users copy.
func TestTagsAuditEmitsTagDriftFindingsBesidePlan(t *testing.T) {
	seedTagsAuditFixStore(t, duplicateTagAuditItems())

	env := decodeTagAuditEnvelope(t, runTagsAuditJSONCmd(t))
	if len(env.Results) != 1 {
		t.Fatalf("results = %+v, want the one duplicate group", env.Results)
	}
	if env.Results[0]["canonical"] != "Data Science" {
		t.Fatalf("results[0] canonical = %v, want Data Science", env.Results[0]["canonical"])
	}
	if commands, ok := env.Results[0]["rename_commands"].([]any); !ok || len(commands) != 2 {
		t.Fatalf("results[0] rename_commands = %v, want the two rename commands preserved", env.Results[0]["rename_commands"])
	}

	// K2 carries the alias "data science", K3 carries "Data  Science"; K1
	// already carries the canonical spelling and is not drifted.
	wantKeys := map[string]string{"K2": "data science", "K3": "Data  Science"}
	if len(env.Findings) != len(wantKeys) {
		t.Fatalf("findings = %+v, want one per drifted item", env.Findings)
	}
	identities := map[string]bool{}
	for _, finding := range env.Findings {
		wantTag, ok := wantKeys[finding.ItemKey]
		if !ok {
			t.Fatalf("finding on unexpected item %q: %+v", finding.ItemKey, finding)
		}
		if finding.Kind == "" {
			t.Fatalf("finding %+v has an empty kind", finding)
		}
		if got := sqlStringValue(finding.Evidence["tag"]); got != wantTag {
			t.Errorf("finding %s evidence tag = %q, want %q", finding.ItemKey, got, wantTag)
		}
		if got := sqlStringValue(finding.Evidence["canonical"]); got != "Data Science" {
			t.Errorf("finding %s evidence canonical = %q, want Data Science", finding.ItemKey, got)
		}
		// Identity is (kind, item_key): stable across runs even when the
		// group's item counts move, which library_health_baseline.go diffs.
		identity := watchHealthFindingKey(finding)
		if identity != "tag_drift\x00item\x00"+finding.ItemKey {
			t.Errorf("finding identity = %q, want the (kind, item_key) key", identity)
		}
		if identities[identity] {
			t.Errorf("duplicate identity %q: cross-run diffs would collapse two findings", identity)
		}
		identities[identity] = true
	}
}

// TestTagsAuditFindingsMatchLibraryHealthVocabulary holds the two commands to
// one vocabulary for one condition. `library health` groups tag drift by
// canonical name and `tags audit` fans it out per item, but the kind,
// severity and recommended action must be identical, or an agent would have
// to learn two names for the same problem and two fixers for it.
func TestTagsAuditFindingsMatchLibraryHealthVocabulary(t *testing.T) {
	seedTagsAuditFixStore(t, duplicateTagAuditItems())
	db := openTagAuditTestStore(t)

	tagRows, err := db.QueryRaw(tagAuditDistinctQuery)
	if err != nil {
		t.Fatalf("tag distinct query: %v", err)
	}
	countRows, err := db.QueryRaw(tagAuditCountQuery)
	if err != nil {
		t.Fatalf("tag count query: %v", err)
	}
	plans := buildTagAuditPlans(tagRows, countRows, tagAuditPreferFrequency, nil)

	auditFindings, err := tagAuditFindings(db, plans, FindingSource{Kind: "local"})
	if err != nil {
		t.Fatalf("tagAuditFindings: %v", err)
	}
	healthFindings, _, err := runTagDrift(db, newHealthCtx("quick", false))
	if err != nil {
		t.Fatalf("runTagDrift: %v", err)
	}
	if len(auditFindings) == 0 || len(healthFindings) == 0 {
		t.Fatalf("need findings from both sides, got %d audit and %d health", len(auditFindings), len(healthFindings))
	}

	want := healthFindings[0]
	for _, finding := range auditFindings {
		if finding.Kind != want.Kind || finding.Severity != want.Severity {
			t.Fatalf("audit finding kind/severity = %q/%q, want library health's %q/%q",
				finding.Kind, finding.Severity, want.Kind, want.Severity)
		}
		if finding.Autofixable != want.Autofixable {
			t.Fatalf("audit finding autofixable = %v, want library health's %v", finding.Autofixable, want.Autofixable)
		}
		if finding.RecommendedAction == nil || want.RecommendedAction == nil ||
			*finding.RecommendedAction != *want.RecommendedAction {
			t.Fatalf("audit finding action = %+v, want library health's %+v", finding.RecommendedAction, want.RecommendedAction)
		}
	}
}

func openTagAuditTestStore(t *testing.T) localQueryStore {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open seeded store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return localQueryStore{db}
}

// TestTagsAuditFindingPipesIntoItsFixer closes the loop the finding envelope
// exists for: the diagnostic names the fixer in recommended_action, and its
// findings feed that fixer's --keys-from directly. Piping ONE finding must
// also bound the batch to that item -- `tags audit fix` otherwise applies the
// whole library-wide plan, so the pipe is the only item selector it has.
func TestTagsAuditFindingPipesIntoItsFixer(t *testing.T) {
	items := duplicateTagAuditItems()
	seedTagsAuditFixStore(t, items)

	env := decodeTagAuditEnvelope(t, runTagsAuditJSONCmd(t))
	var selected Finding
	for _, finding := range env.Findings {
		if finding.ItemKey == "K3" {
			selected = finding
		}
	}
	if selected.ItemKey == "" {
		t.Fatalf("no finding for K3 to pipe: %+v", env.Findings)
	}
	if selected.RecommendedAction == nil || selected.RecommendedAction.Command != "zotio tags audit fix" {
		t.Fatalf("finding action = %+v, want the tags audit fix command", selected.RecommendedAction)
	}
	piped, err := json.Marshal(FindingsReport{Findings: []Finding{selected}})
	if err != nil {
		t.Fatalf("marshal piped finding: %v", err)
	}

	patches := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveTagAuditFixItem(w, r, items) {
			return
		}
		if r.Method != http.MethodPatch {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		patches[r.URL.Path] = body
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	mutationEnv, out, err := runTagsAuditFixCmdStdin(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, srv.URL, piped, "--keys-from", "-")
	if err != nil {
		t.Fatalf("tags audit fix --keys-from -: %v\n%s", err, out)
	}
	if !mutationEnv.OK || mutationEnv.Mode != "apply" {
		t.Fatalf("envelope = %+v, want ok apply", mutationEnv)
	}
	if mutationEnv.Plan.Summary.Planned != 1 {
		t.Fatalf("planned = %d, want only the piped item (the unfiltered plan writes 2)", mutationEnv.Plan.Summary.Planned)
	}
	if _, ok := patches["/users/0/items/K3"]; !ok {
		t.Fatalf("piped finding did not reach its fixer; patches = %#v", patches)
	}
	if _, ok := patches["/users/0/items/K2"]; ok {
		t.Fatalf("--keys-from did not bound the batch: K2 was written too; patches = %#v", patches)
	}
	tags, _ := patches["/users/0/items/K3"]["tags"].([]any)
	if len(tags) != 1 {
		t.Fatalf("K3 tags = %#v, want the single canonical tag", patches["/users/0/items/K3"]["tags"])
	}
	if tag, _ := tags[0].(map[string]any); tag["tag"] != "Data Science" {
		t.Fatalf("K3 renamed to %v, want the canonical Data Science", tag["tag"])
	}
}

// runTagsAuditFixCmdStdin is runTagsAuditFixCmdArgs with a piped stdin, so a
// test can hand `tags audit fix` the finding envelope on `--keys-from -`.
func runTagsAuditFixCmdStdin(t *testing.T, flags *rootFlags, baseURL string, stdin []byte, extra ...string) (mutation.Envelope, string, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", baseURL+"/users/0")
	cmd := newTagsAuditCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetIn(bytes.NewReader(stdin))
	cmd.SetArgs(append([]string{"fix"}, extra...))
	err := cmd.Execute()
	var env mutation.Envelope
	if out.Len() > 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
			t.Fatalf("decode envelope %q: %v", out.String(), decodeErr)
		}
	}
	return env, out.String(), err
}
