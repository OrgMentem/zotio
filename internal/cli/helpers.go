// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"
	"unicode"
	"zotio/internal/client"
	"zotio/internal/cliutil"
	"zotio/internal/config"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var As = errors.As

// noColor is set by the --no-color flag
var noColor bool

// humanFriendly is set by the --human-friendly flag; it forces colored output
// on even when stdout is not a terminal (e.g. when piping into a renderer).
var humanFriendly bool

// activeGroupIDValue holds the numeric Zotero group ID selected via --group, or
// "" for the personal library. It scopes both the API library prefix and the
// on-disk DB file to a group library.
//
// Access it ONLY through activeGroupIDLocked/setActiveGroupID. Cobra's root
// PersistentPreRunE writes it on every in-process command execution, while the
// MCP server reads it from its own dispatch goroutines: under --transport http
// a native sql/search/archive-status/schema handler can read while a concurrent
// command_run writes. The mirrored-command mutex serializes only mirrored runs,
// so it cannot stand in for this lock. The raw variable is deliberately named
// so that direct access does not compile.
var (
	activeGroupMu      sync.RWMutex
	activeGroupIDValue string
)

// activeGroupIDLocked returns the group scope under a read lock.
func activeGroupIDLocked() string {
	activeGroupMu.RLock()
	defer activeGroupMu.RUnlock()
	return activeGroupIDValue
}

// setActiveGroupID replaces the group scope under a write lock.
func setActiveGroupID(id string) {
	activeGroupMu.Lock()
	activeGroupIDValue = id
	activeGroupMu.Unlock()
}

// snapshotCLIGlobals captures the CLI state mutated by root command flags.
func snapshotCLIGlobals() func() {
	savedNoColor := noColor
	savedHumanFriendly := humanFriendly
	savedGroupID := activeGroupIDLocked()
	return func() {
		noColor = savedNoColor
		humanFriendly = savedHumanFriendly
		setActiveGroupID(savedGroupID)
	}
}

// SnapshotGlobals guards process-global CLI state around in-process command execution.
func SnapshotGlobals() func() {
	return snapshotCLIGlobals()
}

// colorEnabled reports whether output should carry ANSI styling.
// Kill switches (--no-color, NO_COLOR, TERM=dumb) always win; --human-friendly
// forces color on; otherwise color follows terminal auto-detection. Piped
// output already switches to JSON via wantsHumanTable, so agents never see
// styled tables unless they allocate a pty on purpose.
func colorEnabled() bool {
	if noColor {
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	if humanFriendly {
		return true
	}
	return isTerminal(os.Stdout)
}

func isTerminal(w io.Writer) bool {
	for {
		u, ok := w.(interface{ Unwrap() io.Writer })
		if !ok {
			break
		}
		w = u.Unwrap()
	}
	if f, ok := w.(*os.File); ok {
		fi, err := f.Stat()
		if err != nil {
			return true
		}
		return (fi.Mode() & os.ModeCharDevice) != 0
	}
	return false
}

func bold(s string) string {
	if !colorEnabled() {
		return s
	}
	return "\033[1m" + s + "\033[0m"
}

func green(s string) string {
	if !colorEnabled() {
		return s
	}
	return "\033[32m" + s + "\033[0m"
}

func red(s string) string {
	if !colorEnabled() {
		return s
	}
	return "\033[31m" + s + "\033[0m"
}

func yellow(s string) string {
	if !colorEnabled() {
		return s
	}
	return "\033[33m" + s + "\033[0m"
}

func dim(s string) string {
	if !colorEnabled() {
		return s
	}
	return "\033[2m" + s + "\033[0m"
}

func cyan(s string) string {
	if !colorEnabled() {
		return s
	}
	return "\033[36m" + s + "\033[0m"
}

type cliError struct {
	code int
	err  error
	// kind overrides the kind derived from code in the JSON error object.
	kind string
	// fields are extra kind-specific keys merged into the JSON error object.
	fields map[string]any
}

func (e *cliError) Error() string { return e.err.Error() }
func (e *cliError) Unwrap() error { return e.err }

func usageErr(err error) error     { return &cliError{code: 2, err: err} }
func notFoundErr(err error) error  { return &cliError{code: 3, err: err} }
func authErr(err error) error      { return &cliError{code: 4, err: err} }
func apiErr(err error) error       { return &cliError{code: 5, err: err} }
func configErr(err error) error    { return &cliError{code: 10, err: err} }
func rateLimitErr(err error) error { return &cliError{code: 7, err: err} }

// redactedError wraps an error so error-chain inspection (e.g. errors.As to
// *client.APIError) keeps working while Error() returns only pre-sanitized
// text. This stops the raw response body from reaching stderr / the --json
// envelope via a %w-wrapped APIError.Error().
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

func liveSecrets(flags *rootFlags) []string {
	envKey := os.Getenv("ZOTERO_API_KEY")
	secrets := make([]string, 0, 2)
	configPath := ""
	if flags != nil {
		configPath = flags.configPath
	}
	if cfg, err := config.Load(configPath); err == nil && cfg != nil {
		secrets = appendUniqueSecret(secrets, cfg.AuthHeader())
	}
	return appendUniqueSecret(secrets, envKey)
}

func appendUniqueSecret(secrets []string, secret string) []string {
	if secret == "" {
		return secrets
	}
	for _, existing := range secrets {
		if existing == secret {
			return secrets
		}
	}
	return append(secrets, secret)
}

func redactedAPIError(err error, msg string, secrets []string, suffix string) *redactedError {
	return &redactedError{
		err: err,
		msg: cliutil.SanitizeErrorBodyWithSecrets(msg, secrets...) + suffix,
	}
}

// library health quality gate (--fail-on) failed —
// the tool ran fine but the library did not meet the requested bar. Distinct
// from error codes so CI can branch on "needs fixing" vs "tool broke".
func gateErr(err error) error { return &cliError{code: 11, err: err} }

// a declared precondition was unmet (e.g. Zotero
// desktop not running for a live-local-API capability). Loud, not silent: the
// agent's remedy is to provision the environment (launch the app, set a key,
// sync) and retry — categorically different from a content gate or a hard error.
func preconditionErr(err error) error { return &cliError{code: 9, err: err} }

// the local store is staler than --require-fresh
// allows, so a read cannot be certified. Distinct from a precondition (9) and a
// quality gate (11): the remedy is `sync` + retry, which CI can automate.
func freshnessErr(err error) error { return &cliError{code: 12, err: err} }

// a read/report command produced output but could not read part of its inputs
// (permission/corruption/transport failure on a note, row, attachment, or
// subtree). The partial result and a machine-readable `warnings` list are still
// emitted; a non-zero exit lets automation detect the incompleteness instead of
// mistaking a degraded run for a complete one. Distinct from a hard error (no
// output), a quality gate (11), a precondition (9), and freshness (12).
func degradedErr(err error) error { return &cliError{code: 13, err: err} }

// a bounded wait (`desktop wait --timeout`) ran out before the awaited
// condition held. Nothing failed and nothing needs fixing: the remedy is to
// wait again, unlike a precondition (9), whose remedy is to change the
// environment first.
func timeoutErr(err error) error { return &cliError{code: 14, err: err} }

// Zotero desktop is running past its startup window and its connector cannot
// take requests (`desktop wait`: unresponsive after a sustained stall, or
// connector_off). Unlike a
// timeout (14), waiting longer does not help until something changes, and
// unlike a missing precondition (9) the app is not closed: the remedy is to
// tell the user, whose Zotero is open but hung or has its connector off.
func stuckErr(err error) error { return &cliError{code: 15, err: err} }

// accessWarning describes an API access-denial that sync converts into a
// non-fatal warning. It carries enough structured data for the sync_warning
// JSON event without parsing free-form error strings downstream.
type accessWarning struct {
	Status  int    // HTTP status when applicable; 0 for GraphQL field-level denials.
	Reason  string // "forbidden" | "insufficient_access" | "unauthenticated"
	Message string // human-readable detail (the API's body or GraphQL error message)
}

// accessDenialPatterns matches API error bodies that indicate the request was
// rejected for access-policy reasons rather than for input validity. Matching
// is case-insensitive and uses word boundaries so common substrings inside
// unrelated tokens (e.g. "author", "pagination_token", "insufficient_funds")
// do not produce false positives. The set deliberately excludes brand names —
// vendor-specific phrasings should be addressed at the spec/profiler level,
// not in this universal classifier.
var accessDenialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bforbidden\b`),
	regexp.MustCompile(`\bunauthorized\b`),
	regexp.MustCompile(`\bnot[\s_-]?authorized\b`),
	regexp.MustCompile(`\bpermission[\s_-]?denied\b`),
	regexp.MustCompile(`\baccess[\s_-]?denied\b`),
	regexp.MustCompile(`\binsufficient[\s_-]?(scope|permission|privilege)`),
	regexp.MustCompile(`\binvalid[\s_-]?scope\b`),
	regexp.MustCompile(`\bmissing[\s_-]?scope\b`),
	regexp.MustCompile(`\brequires?\s+(elevated|admin|enterprise|business|workspace|enterprise[\s_-]?tier)`),
}

// looksLikeAccessDenial reports whether body text describes an access-policy
// rejection. Use it on response-body content (apiErr.Body), not on the full
// error string — the request path can contain words like "auth" or "tokens"
// that would produce false positives if the whole error message were scanned.
func looksLikeAccessDenial(body string) bool {
	lower := strings.ToLower(body)
	for _, p := range accessDenialPatterns {
		if p.MatchString(lower) {
			return true
		}
	}
	return false
}

// isSyncAccessWarning classifies err as an access-denial warning suitable for
// sync's warn-and-continue path. It returns nil, false for any error that
// should remain a hard sync failure: HTTP 401 (token-level auth failure
// requiring re-auth), 5xx, network errors, and HTTP 400 responses whose
// bodies do not match an access-policy pattern.
//
// Recognized warning shapes:
//   - HTTP 403 (per-resource ACL rejection)
//   - HTTP 400 + access-denial body keyword (insufficient scope, etc.)
//   - GraphQL response carrying only access-denial extension codes
func isSyncAccessWarning(err error) (*accessWarning, bool) {
	if err == nil {
		return nil, false
	}

	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 403:
			return &accessWarning{Status: 403, Reason: "forbidden", Message: apiErr.Body}, true
		case 400:
			if looksLikeAccessDenial(apiErr.Body) {
				return &accessWarning{Status: 400, Reason: "insufficient_access", Message: apiErr.Body}, true
			}
		}
	}

	return nil, false
}

type noopResult struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

func writeNoop(out, errOut io.Writer, flags *rootFlags, reason, prose string) error {
	if flags != nil && flags.asJSON {
		return json.NewEncoder(out).Encode(noopResult{Status: "noop", Reason: reason})
	}
	fmt.Fprintln(errOut, prose)
	return nil
}

// classifyAPIError maps API errors to structured exit codes with actionable hints.
func classifyAPIError(err error, flags *rootFlags) error {
	// An already-typed error keeps its exit code: re-classifying by message
	// would downgrade a typed not-found/precondition/usage error to upstream.
	var typed *cliError
	if errors.As(err, &typed) {
		return err
	}
	msg := err.Error()
	secrets := liveSecrets(flags)
	// A request that got no HTTP response at all (dial refused or unreachable,
	// DNS, timeout, TLS) is a missing precondition, not an upstream fault. A
	// response whose body failed to read did get a response, so it keeps the
	// generic mapping. Auto-mode read fallback is handled before classification.
	var bodyReadErr *client.BodyReadError
	if isNetworkError(err) && !errors.As(err, &bodyReadErr) {
		if isLocalZoteroTransportError(err) {
			return preconditionErr(redactedAPIError(err, msg, secrets, "\nhint: Zotero desktop is not running or its local API is disabled; start Zotero or run: zotio doctor --ensure-live --launch"))
		}
		return preconditionErr(redactedAPIError(err, msg, secrets, "\nhint: cannot reach the Zotero Web API; check the network connection and retry"))
	}
	// the Zotero local API is GET-only and rejects writes with
	// "Endpoint does not support method" (400, POST) or "Method not implemented"
	// (501, PUT/PATCH). Without this, those map to a misleading "fix your auth"
	// hint. Surface the real cause and the Web API path forward instead.
	if isLocalWriteRejection(msg) {
		return apiErr(redactedAPIError(err, msg, secrets, "\nhint: the Zotero local API is read-only — writes are not supported there."+
			"\n      Point the CLI at the Web API and provide a write-capable key:"+
			"\n        export ZOTERO_BASE_URL=\"https://api.zotero.org/users/<your-user-id>\""+
			"\n        export ZOTERO_API_KEY=\"<key>\"   (create at https://www.zotero.org/settings/keys)"+
			"\n      Changes made via the Web API sync down to your desktop Zotero."+
			"\n      Run 'zotio doctor' to check writability."))
	}
	// a 412 on a write means the object's version changed on the Web API between
	// the read that supplied the precondition and the write. Guarded writes read
	// that version from the write plane (GetFromWriteBaseWithVersionContext) or
	// take it from the operator's explicit "version", never from the local API
	// or store, so a local sync cannot fix it. Re-running re-reads the version.
	if strings.Contains(msg, "HTTP 412") {
		return apiErr(redactedAPIError(err, msg, secrets, "\nhint: the object changed on the Zotero Web API after this command read its version (version conflict)."+
			"\n      Review the concurrent change, then re-run the command; it re-reads the current version from the Web API."+
			"\n      If you supplied an explicit \"version\", replace it with the object's current Web API version."))
	}
	// classify via the shared cliutil helper so the
	// HTTP-status detection isn't duplicated with the MCP layer; hint text and
	// exit-code wrapping stay CLI-specific.
	switch cliutil.ClassifyHTTPError(msg) {
	case cliutil.HTTPErrConflict:
		if flags != nil && flags.idempotent {
			return writeNoop(flags.out(), flags.errOut(), flags, "already_exists", "already exists (no-op)")
		}
		classified := apiErr(redactedAPIError(err, msg, secrets, ""))
		return classified
	case cliutil.HTTPErrBadRequestAuth:
		return authErr(redactedAPIError(err, msg, secrets, "\nhint: the API rejected the request — this usually means auth is missing or invalid."+
			"\n      Set your API key: export ZOTERO_API_KEY=<your-key> or run zotio auth set-token --stdin"+
			"\n      Run 'zotio doctor' to check auth status."))
	case cliutil.HTTPErrBadRequest:
		// An access-policy 400 (e.g. "missing scope") is an auth problem, not a
		// bad client value.
		var bodyErr *client.APIError
		if errors.As(err, &bodyErr) && looksLikeAccessDenial(bodyErr.Body) {
			return authErr(redactedAPIError(err, msg, secrets, authHint(msg, len(secrets) > 0, true)))
		}
		return usageErr(redactedAPIError(err, msg, secrets, "\nhint: the server rejected a value in the request; check the arguments and flag values."))
	case cliutil.HTTPErrUnauthorized:
		return authErr(redactedAPIError(err, msg, secrets, authHint(msg, len(secrets) > 0, false)))
	case cliutil.HTTPErrForbidden:
		return authErr(redactedAPIError(err, msg, secrets, authHint(msg, len(secrets) > 0, true)))
	case cliutil.HTTPErrNotFound:
		return notFoundErr(redactedAPIError(err, msg, secrets, notFoundHint(msg)))
	case cliutil.HTTPErrRateLimited:
		return rateLimitErr(redactedAPIError(err, msg, secrets, ""))
	default:
		return apiErr(redactedAPIError(err, msg, secrets, ""))
	}
}

// isLocalWriteRejection detects the Zotero local API's verbatim responses to write
// attempts — POST → "Endpoint does not support method", PUT/PATCH → "Method not
// implemented" — which signal a read-only target, not an auth or client error. A
// community local-write plugin returns 2xx instead, so this never fires for it.
func isLocalWriteRejection(msg string) bool {
	return strings.Contains(msg, "Endpoint does not support method") ||
		strings.Contains(msg, "Method not implemented")
}

// sanitizeForTerminal replaces ESC and the other control bytes, including
// newline, with U+FFFD so a hostile library or remote value cannot drive the
// terminal or forge extra output lines. Tab is kept.
func sanitizeForTerminal(s string) string {
	return replaceControlBytes(s, terminalControlByte)
}

// sanitizeErrorForTerminal is for zotio's own error and hint text: it keeps
// newline, which multi-line hints depend on.
func sanitizeErrorForTerminal(s string) string {
	return replaceControlBytes(s, errorControlByte)
}

// SanitizeErrorForTerminal exposes sanitizeErrorForTerminal to the binary entry
// point, which prints zotio's own multi-line error and hint text.
func SanitizeErrorForTerminal(s string) string {
	return sanitizeErrorForTerminal(s)
}

func replaceControlBytes(s string, bad func(byte) bool) string {
	for i := range len(s) {
		if bad(s[i]) {
			var out strings.Builder
			out.Grow(len(s))
			out.WriteString(s[:i])
			out.WriteRune('\uFFFD')
			for j := i + 1; j < len(s); j++ {
				b := s[j]
				if bad(b) {
					out.WriteRune('\uFFFD')
					continue
				}
				out.WriteByte(b)
			}
			return out.String()
		}
	}
	return s
}

func errorControlByte(b byte) bool {
	return (b < 0x20 && b != '\t' && b != '\n') || b == 0x7f
}

func terminalControlByte(b byte) bool {
	return (b < 0x20 && b != '\t') || b == 0x7f
}

// newTabWriter returns a tabwriter for plain (unstyled) tab-separated output.
// NEVER feed it cells containing ANSI codes or East Asian wide runes — it
// counts bytes, so styled cells skew every column. Styled or wide-rune tables
// go through renderColumns (render.go) instead.
func newTabWriter(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
}

// percent-encode user-controlled path
// parameters before splicing them into endpoint templates. Raw replacement let
// "/" and "?" retarget generated commands to sibling resources.
func replacePathParam(path, name, value string) string {
	return strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(value))
}

func extractPaginatedItems(obj map[string]json.RawMessage) ([]json.RawMessage, bool) {
	for _, field := range []string{"data", "items", "results", "messages", "members", "values"} {
		if arr, ok := obj[field]; ok {
			var nested []json.RawMessage
			if json.Unmarshal(arr, &nested) == nil {
				return nested, true
			}
		}
	}

	var onlyArray []json.RawMessage
	arrayCount := 0
	for _, raw := range obj {
		var candidate []json.RawMessage
		if json.Unmarshal(raw, &candidate) == nil {
			onlyArray = candidate
			arrayCount++
		}
	}
	if arrayCount == 1 {
		return onlyArray, true
	}
	return nil, false
}

// printJSONFiltered marshals a Go-typed value through the same output
// pipeline endpoint-mirror commands use. Hand-written novel commands that
// build a typed slice/struct call this so --select, --compact, --csv, and
// --quiet all behave the same way as on generator-emitted commands.
func printJSONFiltered(w io.Writer, v any, flags *rootFlags) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return printOutputWithFlags(w, json.RawMessage(raw), flags)
}

// filterFields keeps only the specified fields (comma-separated) from JSON objects/arrays.
// Supports dotted paths like "events.shortName" to descend into nested structures.
// Arrays are traversed element-wise: "events.shortName" keeps shortName on each event.
func filterFields(data json.RawMessage, fields string) json.RawMessage {
	_, paths := parseSelectPaths(fields)
	if len(paths) == 0 {
		return data
	}
	return filterFieldsRec(data, paths)
}

// filterFieldsRec applies path filters to a JSON value. Each path is a list of
// lowercase segments; arrays descend element-wise.
func filterFieldsRec(data json.RawMessage, paths [][]string) json.RawMessage {
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err == nil {
		out := make([]json.RawMessage, len(arr))
		for i, el := range arr {
			out[i] = filterFieldsRec(el, paths)
		}
		result, _ := json.Marshal(out)
		return result
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err == nil {
		keepWhole := map[string]bool{}
		subPaths := map[string][][]string{}
		for _, p := range paths {
			if len(p) == 0 {
				continue
			}
			head := p[0]
			if len(p) == 1 {
				keepWhole[head] = true
			} else {
				subPaths[head] = append(subPaths[head], p[1:])
			}
		}
		filtered := map[string]json.RawMessage{}
		matchedAny := false
		for k, v := range obj {
			matched := matchSelectSegment(k, keepWhole, subPaths)
			if matched == "" {
				continue
			}
			matchedAny = true
			if keepWhole[matched] {
				filtered[k] = v
				continue
			}
			if subs := subPaths[matched]; subs != nil {
				filtered[k] = filterFieldsRec(v, subs)
			}
		}
		// Envelope fallback: when no top-level keys matched but at least one
		// sibling is a non-null array, treat the object as a list envelope
		// (`{"items":[...]}`, `{"data":[...]}`, `{"total_count":N,"items":[...]}`)
		// and apply the selector inside the array(s). Non-array siblings pass
		// through verbatim so envelope metadata (counts, null pagination
		// cursors) stays visible.
		if !matchedAny {
			pending := map[string]json.RawMessage{}
			foundArray := false
			for k, v := range obj {
				var arr []json.RawMessage
				if json.Unmarshal(v, &arr) == nil && arr != nil {
					foundArray = true
					pending[k] = filterFieldsRec(v, paths)
				} else {
					pending[k] = v
				}
			}
			if foundArray {
				for k, v := range pending {
					filtered[k] = v
				}
			}
		}
		result, _ := json.Marshal(filtered)
		return result
	}

	return data
}

// matchSelectSegment returns the matching normalized segment, or "" if no match.
// Matching ignores case, '-' and '_', so date_added, dateAdded and date-added
// all select the same field.
func matchSelectSegment(fieldName string, keepWhole map[string]bool, subPaths map[string][][]string) string {
	n := normSelectSegment(fieldName)
	if keepWhole[n] || subPaths[n] != nil {
		return n
	}
	for k := range keepWhole {
		if normSelectSegment(k) == n {
			return k
		}
	}
	for k := range subPaths {
		if normSelectSegment(k) == n {
			return k
		}
	}
	return ""
}

// normSelectSegment folds a field name to its comparison form.
func normSelectSegment(s string) string {
	s = strings.ToLower(s)
	if strings.ContainsAny(s, "-_") {
		s = strings.NewReplacer("-", "", "_", "").Replace(s)
	}
	return s
}

// parseSelectPaths splits a --select list into normalized dotted paths.
func parseSelectPaths(fields string) (names []string, paths [][]string) {
	for _, f := range strings.Split(fields, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		parts := strings.Split(f, ".")
		for i := range parts {
			parts[i] = normSelectSegment(parts[i])
		}
		names = append(names, f)
		paths = append(paths, parts)
	}
	return names, paths
}

// checkSelectFields returns a usage error when no field in the --select list
// matches anything in any row or object of data. Empty results are not an error.
func checkSelectFields(data json.RawMessage, fields string) error {
	names, paths := parseSelectPaths(fields)
	if len(paths) == 0 {
		return nil
	}
	sawObject := false
	for _, p := range paths {
		matched, saw := selectPathMatches(data, p)
		if matched {
			return nil
		}
		sawObject = sawObject || saw
	}
	if !sawObject {
		return nil
	}
	return usageErr(fmt.Errorf("--select: no field matches %s in the output (snake_case and camelCase names are both accepted)", strings.Join(names, ", ")))
}

// selectPathMatches mirrors filterFieldsRec's traversal: arrays descend
// element-wise, and an object with no matching top-level key falls back to its
// array-valued siblings.
func selectPathMatches(data json.RawMessage, path []string) (matched, sawObject bool) {
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err == nil {
		for _, el := range arr {
			m, s := selectPathMatches(el, path)
			matched = matched || m
			sawObject = sawObject || s
		}
		return matched, sawObject
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return false, false
	}
	topMatched := false
	for k, v := range obj {
		if normSelectSegment(k) != path[0] {
			continue
		}
		topMatched = true
		if len(path) == 1 {
			matched = true
			continue
		}
		if m, _ := selectPathMatches(v, path[1:]); m {
			matched = true
		}
	}
	foundArray := false
	if !topMatched {
		for _, v := range obj {
			var a []json.RawMessage
			if json.Unmarshal(v, &a) == nil && a != nil {
				foundArray = true
				m, s := selectPathMatches(v, path)
				matched = matched || m
				sawObject = sawObject || s
			}
		}
	}
	// A list envelope is only a wrapper: its records decide, so an empty
	// results array is not a candidate record.
	return matched, sawObject || topMatched || !foundArray
}

// camelToKebab converts "orderDate" or "orderdate" to "order-date" by splitting on
// uppercase boundaries. For already-lowercase input, splits on known word boundaries.
func camelToKebab(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) && unicode.IsLower(runes[i-1]) {
			b.WriteByte('-')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// printOutputWithFlags routes output through the right format based on flags.
func printOutputWithFlags(w io.Writer, data json.RawMessage, flags *rootFlags) error {
	// --quiet: print nothing for every command routed through here; the exit
	// code is the answer.
	if flags.quiet {
		return nil
	}
	data, err := applyFieldFlags(data, flags)
	if err != nil {
		return err
	}
	// --csv: render as CSV
	if flags.csv {
		return printCSV(w, data, flags.selectFields != "")
	}
	// --plain: tab-separated text
	if flags.plain {
		// An explicit --select means the caller chose the columns; otherwise the
		// structural wrapper fields are dropped so a row stays readable.
		return printPlain(w, data, flags.selectFields != "")
	}
	return printOutput(w, data, flags.asJSON)
}

// applyFieldFlags applies --select (or, when no field list was given,
// --compact). --select wins over --compact when both are set: an explicit
// field list is the user's authoritative request, so the high-gravity
// allow-list must not strip those fields out before --select can pick them.
// A field list that matches nothing in the data is a usage error (exit 2).
func applyFieldFlags(data json.RawMessage, flags *rootFlags) (json.RawMessage, error) {
	if flags.selectFields != "" {
		if err := checkSelectFields(data, flags.selectFields); err != nil {
			return nil, err
		}
		return filterFields(data, flags.selectFields), nil
	}
	if flags.compact {
		return compactFields(data), nil
	}
	return data, nil
}

// compactFields keeps only the most important fields for agent consumption.
// For arrays: allowlist of high-gravity fields (no descriptions).
// For single objects: blocklist that strips known-verbose fields (descriptions, comments, etc.).
func compactFields(data json.RawMessage) json.RawMessage {
	// Try array first
	var items []map[string]any
	if err := json.Unmarshal(data, &items); err == nil {
		return compactListFields(items)
	}

	// Single object — use blocklist
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err == nil {
		return compactObjectFields(obj)
	}

	return data
}

// compactListFields keeps only high-gravity fields for array responses.
func compactListFields(items []map[string]any) json.RawMessage {
	keepFields := map[string]bool{
		"id": true, "name": true, "title": true, "identifier": true,
		"status": true, "state": true, "type": true, "priority": true,
		"url": true, "email": true, "key": true, "keys": true,
		"created_at": true, "updated_at": true, "createdAt": true, "updatedAt": true,
		// Preserve hand-written Zotero analytics fields under --agent compact output.
		// resource_type and value carry the analytics row's grouping key: without
		// them a row compacts to a bare {"count":N} and the data is gone.
		"cite_key": true, "display_name": true, "creator_type": true, "item_count": true,
		"venue": true, "item_type": true, "min_year": true, "max_year": true,
		"count": true, "date_added": true, "uri": true, "launched": true,
		"resource_type": true, "value": true,
		"parent_item": true, "color": true, "text": true, "comment": true,
		"page": true, "tag": true, "collection_count": true, "library_count": true,
		"collection_only": true, "queue_tag": true, "oldest": true, "items": true,
		"author": true, "year": true, "arxiv_id": true, "doi": true,
		"item_key": true, "item_title": true,
	}

	filtered := make([]map[string]any, 0, len(items))
	for _, item := range items {
		//
		// resource-envelope items (Zotero/JSON:API shape {key/id, data:{...}})
		// keep nothing under the flat allowlist because their real fields are
		// nested. Detect the envelope and compact the nested object instead, so
		// --agent/--compact output stays useful rather than collapsing to {key}.
		if data, ok := item["data"].(map[string]any); ok {
			compact := map[string]any{}
			for k, v := range item {
				if k != "data" && keepFields[k] {
					compact[k] = v
				}
			}
			var inner map[string]any
			if json.Unmarshal(compactObjectFields(data), &inner) == nil {
				compact["data"] = inner
			}
			filtered = append(filtered, compact)
			continue
		}
		compact := map[string]any{}
		for k, v := range item {
			if keepFields[k] {
				compact[k] = v
			}
		}
		// A row that matched nothing is not a Zotero item record: the allowlist
		// names item-ish fields, so a hand-written report row (e.g. `journal
		// list`, whose fields are run_id/operation/timestamp/summary/ops)
		// compacts to {} and the caller loses the entire row under --agent.
		// Strip nothing rather than everything — same reasoning as the nested
		// envelope branch above, which exists so compaction cannot empty a row
		// whose real fields the allowlist does not name.
		if len(compact) == 0 && len(item) > 0 {
			filtered = append(filtered, item)
			continue
		}
		filtered = append(filtered, compact)
	}
	result, _ := json.Marshal(filtered)
	return result
}

// compactObjectFields strips known-verbose fields from single-object responses.
// Uses a blocklist so it works across all API domains (project management, payments, CRM, etc.).
func compactObjectFields(obj map[string]any) json.RawMessage {
	stripFields := map[string]bool{
		"description": true, "body": true, "content": true,
		"comments": true, "attachments": true, "html": true, "markdown": true,
		// verbose Zotero item fields.
		"abstractNote": true, "relations": true,
	}

	compact := map[string]any{}
	for k, v := range obj {
		if !stripFields[k] {
			compact[k] = v
		}
	}
	result, _ := json.Marshal(compact)
	return result
}

func csvSafeCell(s string) string {
	if s == "" {
		return s
	}
	// prevent spreadsheet formula
	// execution when Zotero-controlled values are opened as CSV.
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	default:
		return s
	}
}

// printPlain renders tab-separated records for --plain: a header line, then one
// line per record. Before this the flag only suppressed the human table and fell
// through to the shared JSON path, so --plain silently emitted JSON.
//
// Cells are NOT truncated the way table cells are: --plain is a machine format
// feeding cut/awk, so a 60-character display truncation would hand the caller
// quietly wrong values.
func printPlain(w io.Writer, data json.RawMessage, explicitFields bool) error {
	return printTabular(w, data, explicitFields, false)
}

// printCSV renders the same rows as printPlain, comma-separated and quoted. It
// shares the envelope unwrap, resource flattening and cell encoding with
// --plain, so nested values are JSON-encoded in both formats and an empty
// result prints nothing in both.
func printCSV(w io.Writer, data json.RawMessage, explicitFields bool) error {
	return printTabular(w, data, explicitFields, true)
}

// printTabular is the shared --plain/--csv renderer.
func printTabular(w io.Writer, data json.RawMessage, explicitFields, asCSV bool) error {
	var records []json.RawMessage
	if err := json.Unmarshal(data, &records); err == nil {
		return printTabularRecords(w, records, explicitFields, asCSV)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err == nil {
		// Read commands wrap results in a provenance envelope
		// ({"results": [...], "meta": {...}}); render the records, not the wrapper.
		if nested, ok := extractPaginatedItems(obj); ok {
			return printTabularRecords(w, nested, explicitFields, asCSV)
		}
		// An object holding several sibling arrays (e.g. items audit with more
		// than one check) has no single record list. Rendering it as one row
		// would put a whole JSON array in each cell, which is worse than the
		// format the caller asked for being unavailable: emit JSON and say so.
		if countArrayFields(obj) > 1 {
			format := "--plain"
			if asCSV {
				format = "--csv"
			}
			fmt.Fprintf(os.Stderr, "note: %s has no tabular form for this response; emitting JSON\n", format)
			return printOutput(w, data, true)
		}
		return printTabularRecords(w, []json.RawMessage{data}, explicitFields, asCSV)
	}
	// Scalar or non-JSON payload (e.g. a plain-text file URL): pass it through.
	var scalar any
	if err := json.Unmarshal(data, &scalar); err == nil {
		// Decode first so a JSON string does not print its own quotes.
		return writeTabularRow(w, []string{plainCell(scalar)}, asCSV)
	}
	return writeTabularRow(w, []string{plainCell(strings.TrimSpace(string(data)))}, asCSV)
}

// writeTabularRow writes one record: tab-joined for --plain, RFC 4180 quoting
// with spreadsheet-formula neutralisation for --csv.
func writeTabularRow(w io.Writer, cells []string, asCSV bool) error {
	if !asCSV {
		_, err := fmt.Fprintln(w, strings.Join(cells, "\t"))
		return err
	}
	safe := make([]string, len(cells))
	for i, cell := range cells {
		safe[i] = csvSafeCell(cell)
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(safe); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

// countArrayFields reports how many top-level fields hold a JSON array.
func countArrayFields(obj map[string]json.RawMessage) int {
	count := 0
	for _, value := range obj {
		var arr []json.RawMessage
		if json.Unmarshal(value, &arr) == nil {
			count++
		}
	}
	return count
}

func printTabularRecords(w io.Writer, records []json.RawMessage, explicitFields, asCSV bool) error {
	if len(records) == 0 {
		return nil
	}
	items := make([]map[string]any, 0, len(records))
	scalars := 0
	for _, record := range records {
		var obj map[string]any
		if err := json.Unmarshal(record, &obj); err != nil {
			scalars++
			continue
		}
		items = append(items, obj)
	}
	// An array of scalars (e.g. collection keys) has no columns: one value per
	// line is the useful shape. A mixed array cannot be rendered as records
	// without silently dropping the scalars, so it takes the same path.
	if len(items) == 0 || scalars > 0 {
		for _, record := range records {
			var value any
			if err := json.Unmarshal(record, &value); err != nil {
				return fmt.Errorf("rendering tabular output: %w", err)
			}
			if err := writeTabularRow(w, []string{plainCell(value)}, asCSV); err != nil {
				return err
			}
		}
		return nil
	}

	items = flattenResourceEnvelopesForTable(items)
	headers := plainHeaders(items, explicitFields)
	if len(headers) == 0 {
		return nil
	}
	if err := writeTabularRow(w, headers, asCSV); err != nil {
		return err
	}
	cells := make([]string, len(headers))
	for _, item := range items {
		for i, header := range headers {
			cells[i] = plainCell(item[header])
		}
		if err := writeTabularRow(w, cells, asCSV); err != nil {
			return err
		}
	}
	return nil
}

// plainHeaders keeps the table column order for the fields the first record has,
// then appends any field only later records carry so every row stays rectangular.
func plainHeaders(items []map[string]any, explicitFields bool) []string {
	headers := plainBibliographicFields(prioritizeAllHeaders(items[0]), explicitFields)
	seen := make(map[string]bool, len(headers))
	for _, header := range headers {
		seen[header] = true
	}
	var extra []string
	// An explicit --select keeps every selected column even when its value is
	// empty, null or [] (the card renderer's empty-value filter would drop it),
	// so the first record contributes all of its keys too.
	rest := items[1:]
	if explicitFields {
		rest = items
	}
	for _, item := range rest {
		for field := range item {
			if !seen[field] {
				seen[field] = true
				extra = append(extra, field)
			}
		}
	}
	sort.Strings(extra)
	return append(headers, plainBibliographicFields(extra, explicitFields)...)
}

// plainStructuralFields are the Zotero response wrappers that carry no
// bibliographic data. In --plain they rendered as whole JSON objects inside a
// single cell, pushing item rows past 2 KB across ~35 columns. They are dropped
// unless the caller named fields explicitly with --select, which always wins.
var plainStructuralFields = map[string]bool{
	"library":   true,
	"links":     true,
	"meta":      true,
	"relations": true,
}

func plainBibliographicFields(fields []string, explicitFields bool) []string {
	if explicitFields {
		return fields
	}
	kept := fields[:0]
	for _, field := range fields {
		if plainStructuralFields[field] {
			continue
		}
		kept = append(kept, field)
	}
	return kept
}

// plainCell renders one value losslessly on a single line. Tabs and newlines
// inside a value would shift columns or split the record, so they collapse to
// spaces before the control-character sanitizer runs.
func plainCell(v any) string {
	var s string
	switch val := v.(type) {
	case nil:
		s = ""
	case string:
		s = val
	case float64:
		if val == float64(int64(val)) {
			s = strconv.FormatInt(int64(val), 10)
		} else {
			s = strconv.FormatFloat(val, 'f', -1, 64)
		}
	case bool:
		s = strconv.FormatBool(val)
	default:
		encoded, err := json.Marshal(val)
		if err != nil {
			s = fmt.Sprintf("%v", val)
		} else {
			s = string(encoded)
		}
	}
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\t', '\n', '\r':
			return ' '
		}
		return r
	}, s)
	return sanitizeForTerminal(s)
}

// printOutput auto-detects arrays and renders as tables, or prints raw JSON for objects.
func printOutput(w io.Writer, data json.RawMessage, asJSON bool) error {
	if !asJSON && !isTerminal(w) {
		asJSON = true
	}

	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(data)
	}

	// Try to detect if response is an array
	var items []map[string]any
	if err := json.Unmarshal(data, &items); err == nil && len(items) > 0 {
		if err := printAutoTable(w, items); err != nil {
			return err
		}
		// Agent-friendly: show count and suggest narrowing when results are large
		if len(items) >= 25 {
			fmt.Fprintf(os.Stderr, "\nShowing %d results. To narrow: add --limit, --json --select, or filter flags.\n", len(items))
		}
		return nil
	}

	// Single object - pretty print
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err == nil {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(obj)
	}

	// Fallback: print raw
	fmt.Fprintln(w, string(data))
	return nil
}

// levenshteinDistance computes the edit distance between two strings using a two-row DP approach.
func levenshteinDistance(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	if len(a) < len(b) {
		a, b = b, a
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			ins := curr[j-1] + 1
			del := prev[j] + 1
			sub := prev[j-1] + cost
			min := ins
			if del < min {
				min = del
			}
			if sub < min {
				min = sub
			}
			curr[j] = min
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

// suggestFlag returns the closest known flag name to the unknown string, or "" if none is close enough.
func suggestFlag(unknown string, cmd *cobra.Command) string {
	unknown = strings.TrimLeft(unknown, "-")
	best := ""
	bestDist := 4 // only consider distance <= 3
	check := func(name string) {
		d := levenshteinDistance(unknown, name)
		if d < bestDist && d*5 <= len(unknown)*2 {
			bestDist = d
			best = name
		}
	}
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		check(f.Name)
	})
	cmd.InheritedFlags().VisitAll(func(f *pflag.Flag) {
		check(f.Name)
	})
	return best
}

// wantsHumanTable returns true when output should be a human-friendly table.
// Smart default: terminal=table, pipe=JSON.
// - Human in terminal: isTerminal()=true → table
// - Claude Code/Codex bash tool: stdout piped → JSON
// - --json/--csv/--compact/--agent: machine format → JSON
func wantsHumanTable(w io.Writer, flags *rootFlags) bool {
	if flags.asJSON || flags.csv || flags.compact || flags.quiet || flags.plain {
		return false
	}
	if flags.selectFields != "" {
		return false
	}
	return isTerminal(w)
}

// wantsJSONEnvelope reports whether a read command should emit its JSON
// provenance envelope: requested with --json, or implied because stdout is not a
// terminal.
//
// An explicitly named competing format wins over the piped-stdout default.
// --plain and --csv ARE machine formats, so treating "not a terminal" as "must
// be JSON" silently substituted JSON for the format the caller asked for — the
// worst of the three options.
func wantsJSONEnvelope(w io.Writer, flags *rootFlags) bool {
	if flags == nil {
		return !isTerminal(w)
	}
	if flags.asJSON {
		return true
	}
	if flags.plain || flags.csv {
		return false
	}
	return !isTerminal(w)
}

func printAutoTable(w io.Writer, items []map[string]any) error {
	if len(items) == 0 {
		return nil
	}

	// Zotero item rows are
	// resource envelopes ({key, version, data:{...}}). Flatten nested data for
	// human tables so users see title/type/date instead of a single opaque map.
	items = flattenResourceEnvelopesForTable(items)

	// Count scalar vs complex fields to decide format
	scalarCount := 0
	for _, v := range items[0] {
		switch v.(type) {
		case string, float64, bool, nil:
			scalarCount++
		}
	}

	// Use sectional/card layout for complex items (many fields or nested data)
	if len(items[0]) > 8 || scalarCount < len(items[0])-2 {
		return printAutoCards(w, items)
	}

	headers := prioritizeHeaders(items[0])

	// Limit to 6 columns max for readability
	if len(headers) > 6 {
		headers = headers[:6]
	}

	// Build rows
	rows := make([][]string, 0, len(items))
	for _, item := range items {
		row := make([]string, len(headers))
		for i, h := range headers {
			row[i] = formatCellValue(item[h])
		}
		rows = append(rows, row)
	}

	// Aligned by display width (ANSI- and wide-rune-aware); see render.go.
	return renderColumns(w, headers, rows)
}

func flattenResourceEnvelopesForTable(items []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		data, ok := item["data"].(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		flat := make(map[string]any, len(data)+len(item))
		for k, v := range data {
			flat[k] = v
		}
		for k, v := range item {
			if k != "data" {
				flat[k] = v
			}
		}
		out = append(out, flat)
	}
	return out
}

// prioritizeHeaders orders scalar fields by importance for table display.
func prioritizeHeaders(item map[string]any) []string {
	return prioritizeFields(item, false)
}

// prioritizeAllHeaders orders all fields (including arrays) by importance for card display.
func prioritizeAllHeaders(item map[string]any) []string {
	return prioritizeFields(item, true)
}

// prioritizeFields orders fields by importance: identity → temporal → status → other.
// When includeComplex is true, arrays and objects are included (for card layout).
//
// Uses exact-or-suffix matching to avoid false positives: "name" matches "Name" and
// "UserName" but not "BuildingName" (because "Building" is not a known prefix that
// indicates identity). The field is split on camelCase/snake_case boundaries and the
// LAST segment is matched against patterns.
func prioritizeFields(item map[string]any, includeComplex bool) []string {
	// Priority tiers — matched against the last segment of the field name.
	// "OrderDate" → last segment "date" → tier 1 (temporal).
	// "BuildingName" → last segment "name" → tier 0... but we want to avoid this.
	// Solution: exact match on the full lowered name OR suffix segment match,
	// with a penalty for compound names that have a non-identity prefix.

	// Exact matches (full field name, case-insensitive) — highest confidence
	exactMatches := map[string]int{
		"id": 0, "name": 0, "title": 0, "slug": 0, "key": 0,
		"date": 1, "created": 1, "updated": 1, "createdat": 1, "updatedat": 1,
		"status": 2, "state": 2, "statuscode": 2,
		"summary": 3, "description": 3, "price": 3, "amount": 3, "total": 3,
		"cost": 3, "points": 3, "score": 3,
		"type": 4, "kind": 4, "category": 4, "email": 4, "phone": 4, "url": 4,
	}
	// Suffix patterns — match when the field ends with this word (after splitting)
	suffixMatches := map[string]int{
		"id": 0, "name": 0, "title": 0,
		"date": 1, "time": 1,
		"status": 2, "state": 2, "code": 2,
		"price": 3, "amount": 3, "total": 3, "cost": 3,
		"summary": 3, "description": 3, "points": 3, "score": 3,
		"type": 4, "kind": 4, "category": 4, "method": 4,
	}

	numTiers := 5

	type scored struct {
		name string
		tier int
	}

	var all []scored
	for k, v := range item {
		if !includeComplex {
			switch v.(type) {
			case []any, map[string]any:
				continue
			}
		}
		// Skip values that won't render usefully in cards
		if includeComplex {
			formatted := formatCellValue(v)
			if formatted == "" {
				continue
			}
		}

		tier := numTiers // default: unclassified
		lower := strings.ToLower(k)

		// 1. Exact match on full field name
		if t, ok := exactMatches[lower]; ok {
			tier = t
		} else {
			// 2. Split camelCase into segments and match the last one
			segments := splitCamelCase(lower)
			if len(segments) > 0 {
				lastSeg := segments[len(segments)-1]
				if t, ok := suffixMatches[lastSeg]; ok {
					// Compound names with identity suffixes (BuildingName, TipTime)
					// get demoted one tier because the prefix dilutes the signal
					if len(segments) > 1 {
						tier = t + 1
					} else {
						tier = t
					}
				}
			}
		}

		// Demote booleans to last
		if _, ok := v.(bool); ok && tier >= numTiers {
			tier = numTiers + 1
		}
		all = append(all, scored{name: k, tier: tier})
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].tier != all[j].tier {
			return all[i].tier < all[j].tier
		}
		// Alphabetical within a tier: the source is map iteration, so the
		// insertion index is random per run — names keep output stable.
		return all[i].name < all[j].name
	})

	headers := make([]string, len(all))
	for i, s := range all {
		headers[i] = s.name
	}
	return headers
}

// splitCamelCase splits "OrderDate" → ["order", "date"], "statusCode" → ["status", "code"],
// "page_size" → ["page", "size"].
func splitCamelCase(s string) []string {
	var segments []string
	var current strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if r == '_' || r == '-' {
			if current.Len() > 0 {
				segments = append(segments, current.String())
				current.Reset()
			}
			continue
		}
		if i > 0 && unicode.IsUpper(r) && unicode.IsLower(runes[i-1]) {
			if current.Len() > 0 {
				segments = append(segments, current.String())
				current.Reset()
			}
		}
		current.WriteRune(unicode.ToLower(r))
	}
	if current.Len() > 0 {
		segments = append(segments, current.String())
	}
	return segments
}

// printAutoCards renders items as labeled cards — one block per item.
// Used for complex responses with many fields or nested data.
func printAutoCards(w io.Writer, items []map[string]any) error {
	headers := prioritizeAllHeaders(items[0])

	// Longest label ("header:") among the fields shown in card bodies,
	// so every value starts at the same column.
	maxLen := 0
	for _, h := range headers {
		if l := len(h) + 1; l > maxLen {
			maxLen = l
		}
	}

	for i, item := range items {
		if i > 0 {
			fmt.Fprintln(w)
		}

		// Card header: identity label + key recede, the human-readable
		// second field (usually the title) is the anchor.
		titleVal := formatCellValue(item[headers[0]])
		head := bold(strings.ToUpper(headers[0])) + " " + titleVal
		if len(headers) > 1 {
			if secondVal := formatCellValue(item[headers[1]]); secondVal != "" {
				head += dim(" — ") + bold(secondVal)
			}
		}
		fmt.Fprintln(w, head)

		// Remaining fields indented — skip empty, zero, and false values
		for _, h := range headers[2:] {
			v := formatCellValue(item[h])
			if t := strings.TrimSpace(v); t == "" || t == "false" || t == "0" || t == "[]" || t == "null" {
				continue
			}
			label := dim(padRight(h+":", maxLen))
			v = columnStyle(h)(v)
			// Multi-line values (nested arrays) start with \n
			if strings.HasPrefix(v, "\n") {
				fmt.Fprintf(w, "  %s%s\n", dim(h+":"), strings.ReplaceAll(v, "\n", "\n    "))
			} else {
				fmt.Fprintf(w, "  %s  %s\n", label, v)
			}
		}
	}
	return nil
}

func formatCellValue(v any) string {
	switch val := v.(type) {
	case string:
		safe := sanitizeForTerminal(val)
		// Format ISO dates as just the date portion
		if len(safe) >= 19 && safe[4] == '-' && safe[7] == '-' && safe[10] == 'T' {
			return safe[:10]
		}
		return truncate(safe, 60)
	case float64:
		if val == float64(int64(val)) {
			return fmt.Sprintf("%d", int64(val))
		}
		return fmt.Sprintf("%.2f", val)
	case bool:
		return fmt.Sprintf("%t", val)
	case nil:
		return ""
	case []any:
		if len(val) == 0 {
			return ""
		}
		// If array contains objects, format each as a summary line
		if _, isObj := val[0].(map[string]any); isObj {
			return formatObjectArray(val)
		}
		// Flatten simple arrays into comma-separated string
		parts := make([]string, 0, len(val))
		for _, item := range val {
			if s, ok := item.(string); ok {
				parts = append(parts, sanitizeForTerminal(s))
			} else {
				b, _ := json.Marshal(item)
				parts = append(parts, string(b))
			}
		}
		return truncate(strings.Join(parts, ", "), 60)
	case map[string]any:
		return formatSingleObject(val)
	default:
		b, _ := json.Marshal(val)
		return truncate(string(b), 60)
	}
}

// formatObjectArray renders an array of objects as multi-line summary.
// Each object is summarized by its most descriptive fields: name/title, qty, size, price.
func formatObjectArray(items []any) string {
	var lines []string
	for _, raw := range items {
		obj, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		lines = append(lines, formatObjectSummary(obj))
	}
	if len(lines) == 0 {
		return ""
	}
	// Multi-line: newline-prefixed so the card renderer can indent
	return "\n" + strings.Join(lines, "\n")
}

// formatObjectSummary renders one nested object as a one-line summary using
// the shapes that actually occur in Zotero payloads: tags ({tag, type}),
// creators ({creatorType, firstName, lastName} or {creatorType, name}), and
// generic named objects. Unknown shapes fall back to compact JSON.
func formatObjectSummary(obj map[string]any) string {
	// Tag: {"tag": "/unread", "type": 1}
	if tag := findField(obj, "tag"); tag != "" {
		return tag
	}

	// Creator: {"creatorType": "author", "firstName": "…", "lastName": "…"}
	first := findField(obj, "firstName")
	last := findField(obj, "lastName")
	name := strings.TrimSpace(strings.TrimSpace(first) + " " + strings.TrimSpace(last))
	if name == "" {
		name = findField(obj, "name", "title", "label", "description")
	}
	if name != "" {
		// Annotate non-author roles (editor, translator, …); "author" is the
		// overwhelming default and would only add noise.
		if role := findField(obj, "creatorType"); role != "" && role != "author" {
			return name + " (" + role + ")"
		}
		return name
	}

	// Fallback: compact JSON summary
	b, _ := json.Marshal(obj)
	return truncate(string(b), 80)
}

// formatSingleObject renders a single object by its most descriptive fields.
func formatSingleObject(obj map[string]any) string {
	name := findField(obj, "name", "title", "label", "description")
	if name != "" {
		return name
	}
	id := findField(obj, "id", "key", "code")
	if id != "" {
		return id
	}
	return ""
}

// findField searches an object for a field name (case-insensitive) and returns its formatted value.
func findField(obj map[string]any, names ...string) string {
	for _, name := range names {
		for k, v := range obj {
			if strings.EqualFold(k, name) {
				return formatCellValue(v)
			}
		}
	}
	return ""
}

// DataProvenance describes where data came from and when it was last synced.
type DataProvenance struct {
	Source       string     `json:"source"`                  // "live" or "local"
	SyncedAt     *time.Time `json:"synced_at,omitempty"`     // when local data was last synced
	Reason       string     `json:"reason,omitempty"`        // why local was used: "user_requested", "api_unreachable", "no_search_endpoint"
	ResourceType string     `json:"resource_type,omitempty"` // which resource type was queried
	Freshness    any        `json:"freshness,omitempty"`     // optional machine-owned freshness metadata for covered command paths
	// true when a local read applied the request's scopes
	// (itemType/tag/collection/sort/limit) rather than dumping all synced rows.
	Scoped bool `json:"scoped,omitempty"`
	// GroupBy names the analytics grouping field, so a consumer receiving
	// {value, count} rows can tell whether value is a year, a tag, or a
	// creator without re-reading its own command line.
	GroupBy string `json:"group_by,omitempty"`
	// MetaExtra carries command-specific annotations that belong INSIDE meta
	// rather than beside it: a statement about how this answer was produced,
	// never data the command matched. It exists because `extra` in
	// wrapWithProvenanceExtra means "sibling TOP-LEVEL key", and a command
	// that needs one meta field should not have to open a second top-level
	// key on the shared read envelope to get it.
	//
	// Not serialized under its own name: wrapWithProvenanceExtra merges the
	// keys into meta, and refuses a key provenance already owns rather than
	// letting a caller relabel where the data came from.
	MetaExtra map[string]any `json:"-"`
}

// printProvenance writes a one-line provenance message to stderr for TTY users.
// Suppressed when stdout is piped or redirected — the JSON response envelope
// already carries meta.source, so the stderr line is redundant and becomes
// noise in agent flows that merge stderr into stdout.
func printProvenance(cmd *cobra.Command, count int, prov DataProvenance) {
	if !isTerminal(cmd.OutOrStdout()) {
		return
	}
	noun := "results"
	if count == 1 {
		noun = "result"
	}
	if prov.Source == "live" {
		fmt.Fprintln(cmd.ErrOrStderr(), dim(fmt.Sprintf("%d %s (live)", count, noun)))
		return
	}
	age := "unknown"
	if prov.SyncedAt != nil {
		d := time.Since(*prov.SyncedAt)
		switch {
		case d < time.Minute:
			age = "just now"
		case d < time.Hour:
			age = fmt.Sprintf("%d minutes ago", int(d.Minutes()))
		case d < 24*time.Hour:
			age = fmt.Sprintf("%d hours ago", int(d.Hours()))
		default:
			age = fmt.Sprintf("%d days ago", int(d.Hours()/24))
		}
	}
	prefix := ""
	if prov.Reason == "api_unreachable" {
		prefix = "API unreachable. "
	}
	fmt.Fprintln(cmd.ErrOrStderr(), dim(fmt.Sprintf("%s%d %s (cached, synced %s)", prefix, count, noun, age)))
}

// wrapWithProvenance wraps response data in a provenance envelope:
// {"results": [...], "meta": {...}}. The envelope invariant is that
// .results is ALWAYS a JSON array — a jq pipeline written for one read
// command works against every other read command:
//
//   - A JSON array (list/search reads) passes through unchanged.
//   - A JSON object (single-resource reads like `items get`) becomes a
//     one-element array, not a bare object.
//   - Text formats (e.g. XML/RSS, BibTeX) become a one-element array
//     holding the raw text as a JSON string. Live reads expecting JSON
//     reject malformed bodies in resolveRead before reaching this wrapper.
//     Direct callers also retain the text fallback.
func wrapWithProvenance(data json.RawMessage, prov DataProvenance) (json.RawMessage, error) {
	return wrapWithProvenanceExtra(data, prov, nil)
}

// envelopeReservedKeys are the top-level keys the read envelope owns. They are
// checked in a fixed order so two colliding keys still produce the same error
// on every run, which is what makes the failure reproducible from a report.
var envelopeReservedKeys = [2]string{"meta", "results"}

// wrapWithProvenanceExtra wraps data in the same envelope and adds sibling
// top-level keys. Sibling, never inside .results: extra keys exist for data
// that is NOT what the command matched — advisory rows a caller must opt into
// reading — so folding them into .results would break the one invariant every
// jq pipeline depends on.
//
// "results" and "meta" are reserved: supplying either in extra returns an
// error naming it. This used to skip the key silently, which turned a caller
// bug into missing output with no signal on any surface — the reserved key was
// dropped, the command still exited 0, and the data the caller asked to publish
// simply was not there. A command that needs a meta field passes it through
// DataProvenance.MetaExtra, which merges into meta by design.
func wrapWithProvenanceExtra(data json.RawMessage, prov DataProvenance, extra map[string]any) (json.RawMessage, error) {
	meta := map[string]any{"source": prov.Source}
	if prov.SyncedAt != nil {
		meta["synced_at"] = prov.SyncedAt.UTC().Format(time.RFC3339)
	}
	if prov.Reason != "" {
		meta["reason"] = prov.Reason
	}
	if prov.ResourceType != "" {
		meta["resource_type"] = prov.ResourceType
	}
	if prov.Freshness != nil {
		meta["freshness"] = prov.Freshness
	}
	// advertise that local filters were applied.
	if prov.Scoped {
		meta["scoped"] = true
	}
	if prov.GroupBy != "" {
		meta["group_by"] = prov.GroupBy
	}
	// Command-specific meta annotations, merged last. Provenance keys are
	// authoritative: a caller colliding on one is trying to relabel where the
	// data came from, which is a bug in the caller and not a value to merge.
	if len(prov.MetaExtra) > 0 {
		keys := make([]string, 0, len(prov.MetaExtra))
		for key := range prov.MetaExtra {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if _, taken := meta[key]; taken {
				return nil, fmt.Errorf("meta key %q is owned by the provenance envelope and cannot be overridden by a command annotation", key)
			}
			meta[key] = prov.MetaExtra[key]
		}
	}
	envelope := map[string]any{
		"results": normalizeResultsArray(data),
		"meta":    meta,
	}
	for _, reserved := range envelopeReservedKeys {
		if _, taken := extra[reserved]; taken {
			return nil, fmt.Errorf("envelope key %q is reserved: pass a meta annotation through DataProvenance.MetaExtra, or name the sibling key after the data it carries", reserved)
		}
	}
	for key, value := range extra {
		envelope[key] = value
	}
	return json.Marshal(envelope)
}

// normalizeResultsArray enforces the read-envelope invariant that .results
// is always a JSON array (see wrapWithProvenance). Non-JSON data can still
// arrive from a requested text format or a direct caller; resolveRead rejects
// malformed live JSON before it reaches this function. Valid arrays need only
// a leading-byte check after trimming whitespace, not another full decode.
func normalizeResultsArray(data json.RawMessage) any {
	if !json.Valid(data) {
		return []any{string(data)}
	}
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '[' {
		return json.RawMessage(data)
	}
	return [1]json.RawMessage{json.RawMessage(data)}
}

// truncateJSONArray returns a JSON array containing at most n elements
// from the input. When n <= 0, when the input isn't a JSON array, or
// when the array is already at-or-below the limit, the input is
// returned unchanged.
//
// Used by list-endpoint commands whose API ignores the ?limit=N query
// param (e.g. Firebase-style endpoints that return the full collection
// regardless of the param). The truncation is idempotent — calling it
// when the API already honored the limit is a no-op. The generator
// only emits the call when the spec declares a `limit` param without a
// Pagination block, so paginated APIs are unaffected.
func truncateJSONArray(data json.RawMessage, n int) json.RawMessage {
	if n <= 0 {
		return data
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err != nil {
		return data
	}
	if len(arr) <= n {
		return data
	}
	out, err := json.Marshal(arr[:n])
	if err != nil {
		return data
	}
	return out
}

// defaultDBPath returns the canonical path for the local SQLite database.
// group libraries get their own data-group-<id>.db file so
// a group sync never mixes into the personal data.db; personal stays data.db.
func defaultDBPathFor(gid, name string) (string, error) {
	// the demo sandbox uses a separate demo.db in the same
	// directory (group suffix is irrelevant in demo mode).
	if demoActive() {
		return demoDBPath(name)
	}
	dataDir, err := cliutil.KindDir(cliutil.PathKindData)
	if err != nil {
		return "", fmt.Errorf("resolving data directory: %w", err)
	}
	file := "data.db"
	if gid != "" {
		file = "data-group-" + gid + ".db"
	}
	return filepath.Join(dataDir, file), nil
}

func defaultDBPath(name string) (string, error) {
	return defaultDBPathFor(activeGroupIDLocked(), name)
}

// resolveDBPath returns the mirror path for THIS invocation: the --db value
// the caller named, or the active library's own mirror when they named none.
//
// Every command with a --db flag must resolve through this function from
// inside RunE and keep the result in a local, because the shorter-looking
// alternative is silently wrong under `--group all`:
//
//	if dbPath == "" { dbPath, err = defaultDBPath("zotio") }   // WRONG
//
// That writes the resolved path back into the closure variable cobra binds to
// --db. The fan-out reuses ONE cobra.Command instance and re-enters its RunE
// once per library (withLibraryScope, group_fanout.go), so on the second
// library the variable is no longer empty: the command reads the FIRST
// library's database while the aggregate labels those rows with the second
// library's name. Two independent reviews found that shape at six call sites,
// which is why the resolution lives here instead of being repeated per command.
func resolveDBPath(dbFlag, name string) (string, error) {
	if dbFlag != "" {
		return dbFlag, nil
	}
	return defaultDBPath(name)
}

// rewriteLibraryPrefix rewrites a Zotero API base URL's library prefix to a
// group library. It replaces an existing /users/<id> or /groups/<id> segment
// with /groups/<groupID>; when no such segment exists it appends /groups/<id>
// to the URL.
func rewriteLibraryPrefix(baseURL, groupID string) string {
	prefix := regexp.MustCompile(`/(users|groups)/[^/]+`)
	if prefix.MatchString(baseURL) {
		return prefix.ReplaceAllString(baseURL, "/groups/"+groupID)
	}
	return strings.TrimRight(baseURL, "/") + "/groups/" + groupID
}

// userIDFromBaseURL extracts the numeric user ID from a personal-library base
// URL's /users/<id> segment. Returns false when the URL targets a group
// library or has no user segment.
func userIDFromBaseURL(baseURL string) (string, bool) {
	m := regexp.MustCompile(`/users/([^/]+)`).FindStringSubmatch(baseURL)
	if len(m) != 2 || m[1] == "" {
		return "", false
	}
	return m[1], true
}

// isAllDigits reports whether s is non-empty and contains only ASCII digits.
// guards the numeric --group value.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// authHint tells the three 401/403 causes apart: no key configured, a key the
// server rejected, and a valid key that lacks permission for the resource.
func authHint(msg string, haveKey, forbidden bool) string {
	const doctor = "\n      Run 'zotio doctor' to check auth status."
	switch {
	case !haveKey:
		return "\nhint: no API key is configured; check your API key setup. Set ZOTERO_API_KEY or run zotio auth set-token --stdin" + doctor
	case strings.Contains(strings.ToLower(msg), "invalid key"):
		return "\nhint: the Zotero API rejected your key (Invalid key). Create a new key at https://www.zotero.org/settings/keys, then set ZOTERO_API_KEY or run zotio auth set-token --stdin" + doctor
	case forbidden:
		return "\nhint: your API key is valid but has no permission for this resource. Edit the key's library access at https://www.zotero.org/settings/keys" + doctor
	default:
		return "\nhint: the Zotero API rejected your credentials. Set ZOTERO_API_KEY or run zotio auth set-token --stdin" + doctor
	}
}

var apiRequestPathPattern = regexp.MustCompile(`\b(?:GET|HEAD|POST|PUT|PATCH|DELETE) (/\S*) returned HTTP`)

// notFoundHint names the missing noun and a real command that lists the
// valid values, derived from the failing request path.
func notFoundHint(msg string) string {
	m := apiRequestPathPattern.FindStringSubmatch(msg)
	if m == nil {
		return "\nhint: not found. Check the key or name and try again."
	}
	segs := strings.Split(strings.Trim(m[1], "/"), "/")
	if len(segs) >= 2 && (segs[0] == "users" || segs[0] == "groups") {
		segs = segs[2:]
	}
	if len(segs) == 0 {
		return "\nhint: not found. Check the key or name and try again."
	}
	var noun, list string
	switch segs[0] {
	case "items":
		noun, list = "item", "zotio items list"
	case "collections":
		noun, list = "collection", "zotio collections list"
	case "searches":
		noun, list = "saved search", "zotio searches list"
	case "tags":
		noun, list = "tag", "zotio tags list"
	case "groups":
		noun, list = "group", "zotio groups list"
	default:
		return "\nhint: not found. Check the key or name and try again."
	}
	return fmt.Sprintf("\nhint: that %s does not exist. To see valid ones, run: %s", noun, list)
}

// renderedError marks an error whose JSON has already been printed.
type renderedError struct{ err error }

func (e *renderedError) Error() string { return e.err.Error() }
func (e *renderedError) Unwrap() error { return e.err }

// markRendered wraps an error whose JSON (for example a mutation envelope
// with ok:false) was already printed, so the central printer skips it. The
// exit code is preserved through errors.As.
func markRendered(err error) error {
	if err == nil {
		return nil
	}
	return &renderedError{err: err}
}

// errorKindForCode names the JSON error kind for an exit code.
func errorKindForCode(code int) string {
	switch code {
	case 2:
		return "usage"
	case 3:
		return "not_found"
	case 4:
		return "auth"
	case 5:
		return "upstream"
	case 7:
		return "rate_limited"
	case 9:
		return "precondition_unmet"
	case 10:
		return "config"
	case 11:
		return "gate"
	case 12:
		return "freshness"
	case 13:
		return "degraded"
	case 14:
		return "timeout"
	default:
		return "general"
	}
}

// splitErrorHint separates the message from its trailing "hint:" block.
func splitErrorHint(text string) (message, hint string) {
	idx := strings.Index(text, "\nhint:")
	if idx < 0 {
		return strings.TrimSpace(text), ""
	}
	message = strings.TrimSpace(text[:idx])
	lines := strings.Split(strings.TrimPrefix(text[idx+1:], "hint:"), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			kept = append(kept, line)
		}
	}
	return message, strings.Join(kept, "\n")
}

// errorJSONObject builds the flat JSON error object for one non-zero exit:
// {kind, code, message, hint?} plus any kind-specific fields the error carries.
func errorJSONObject(err error) map[string]any {
	code := ExitCode(err)
	message, hint := splitErrorHint(sanitizeErrorForTerminal(err.Error()))
	obj := map[string]any{"code": code, "message": message}
	var coded *cliError
	if errors.As(err, &coded) {
		for k, v := range coded.fields {
			obj[k] = v
		}
	}
	if hint != "" {
		obj["hint"] = hint
	}
	if _, ok := obj["kind"]; !ok {
		kind := ""
		if coded != nil {
			kind = coded.kind
		}
		if kind == "" {
			kind = errorKindForCode(code)
		}
		obj["kind"] = kind
	}
	var apiError *client.APIError
	if errors.As(err, &apiError) {
		if _, ok := obj["http_status"]; !ok {
			obj["http_status"] = apiError.StatusCode
		}
	}
	return obj
}

// errorOutputMode is the effective output mode of the failed invocation.
type errorOutputMode struct {
	json  bool
	quiet bool
	set   bool
	// wroteStdout is true when the command already wrote to stdout.
	wroteStdout bool
}

var (
	errorModeMu   sync.Mutex
	lastErrorMode errorOutputMode
)

func recordErrorOutputMode(m errorOutputMode) {
	m.set = true
	errorModeMu.Lock()
	lastErrorMode = m
	errorModeMu.Unlock()
}

// effectiveOutputMode reads the mode from the bound flag values, which already
// include saved-profile values and --agent defaults.
func effectiveOutputMode(root *cobra.Command, flags *rootFlags) errorOutputMode {
	jsonSet := root.PersistentFlags().Changed("json")
	return errorOutputMode{
		json:  flags.asJSON || (flags.agent && !jsonSet),
		quiet: flags.quiet,
	}
}

// argvOutputMode derives the mode from the argument list for failures that
// happen before flag parsing completes. It stops at "--", honours "=false" on
// boolean flags, and skips the value of root string flags.
func argvOutputMode(root *cobra.Command, args []string) errorOutputMode {
	var asJSON, agent, quiet, jsonSet bool
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "--") {
			continue
		}
		name, value, hasValue := strings.Cut(arg[2:], "=")
		f := root.PersistentFlags().Lookup(name)
		isBool := f == nil || f.NoOptDefVal != ""
		if !isBool && !hasValue {
			i++ // the next argument is this flag's value
			continue
		}
		on := true
		if hasValue && isBool {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				continue
			}
			on = parsed
		}
		switch name {
		case "json":
			asJSON, jsonSet = on, true
		case "agent":
			agent = on
		case "quiet":
			quiet = on
		}
	}
	return errorOutputMode{json: asJSON || (agent && !jsonSet), quiet: quiet}
}

// stdoutCounter wraps the stdout the CLI hands to commands and remembers
// whether anything was written, so the central error printer never appends a
// JSON error object to a document the command already printed.
type stdoutCounter struct {
	w io.Writer
	n atomic.Int64
}

func (c *stdoutCounter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// Unwrap lets isTerminal see the real file behind the counter.
func (c *stdoutCounter) Unwrap() io.Writer { return c.w }

// Wrote reports whether any byte reached stdout.
func (c *stdoutCounter) Wrote() bool { return c.n.Load() > 0 }

// ReportError renders the failure of the last Execute call and returns the
// process exit code. The human line goes to stderr. The flat JSON error object
// goes to stdout only under the effective --json/--agent (or when stdout is not
// a terminal), and only when the command wrote nothing to stdout and did not
// mark its own JSON as rendered. --quiet prints nothing: the exit code is the
// answer.
func ReportError(stdout, stderr io.Writer, err error) int {
	errorModeMu.Lock()
	m := lastErrorMode
	errorModeMu.Unlock()
	if !m.quiet {
		if (m.json || !isTerminal(stdout)) && !m.wroteStdout {
			PrintErrorJSON(stdout, err)
		}
		fmt.Fprintf(stderr, "Error: %s\n", sanitizeErrorForTerminal(err.Error()))
	}
	return ExitCode(err)
}

// PrintErrorJSON is the central JSON error printer: exactly one flat object on
// w per failure, skipped when the command already rendered its own JSON.
func PrintErrorJSON(w io.Writer, err error) {
	if err == nil {
		return
	}
	var rendered *renderedError
	if errors.As(err, &rendered) {
		return
	}
	_ = json.NewEncoder(w).Encode(errorJSONObject(err))
}

// parseFailOnSeverity validates a --fail-on severity gate value. The one
// vocabulary is critical|high|info|any|none; anything else is a usage error.
func parseFailOnSeverity(flag, value string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	switch v {
	case "critical", "high", "info", "any", "none":
		return v, nil
	}
	return "", usageErr(fmt.Errorf("invalid --%s value %q: want one of critical, high, info, any, none", strings.TrimLeft(flag, "-"), value))
}

// requireTabularOutput rejects --plain and --csv on a command whose output has
// no tabular form.
func requireTabularOutput(flags *rootFlags, cmdPath string) error {
	if flags == nil || (!flags.plain && !flags.csv) {
		return nil
	}
	format := "--plain"
	if flags.csv {
		format = "--csv"
	}
	return usageErr(fmt.Errorf("%s has no tabular form, so %s is not supported; use --json", cmdPath, format))
}

// isLocalZoteroTransportError reports whether a transport error targeted the
// Zotero desktop local API (loopback port 23119), judged by the failing URL or
// dial address the error carries.
func isLocalZoteroTransportError(err error) bool {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && isLocalZoteroAPI(urlErr.URL) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Addr != nil {
		host, port, splitErr := net.SplitHostPort(opErr.Addr.String())
		if splitErr == nil && port == "23119" {
			switch strings.ToLower(host) {
			case "localhost", "127.0.0.1", "::1":
				return true
			}
		}
	}
	return false
}
