// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//
// A command annotated mcp:read-only=true is trusted in two independent places:
// the MCP surface exposes it to agents as safe to call, and workflow_run.go
// injects --dry-run into every workflow step EXCEPT the ones marked read-only
// (workflowRunStepIsReadOnlyWithRoot). --dry-run itself is honoured in exactly
// one place, internal/client, and only for mutating HTTP methods, so nothing
// outside the HTTP path is covered by it.
//
// A command that wrongly claims read-only therefore both invites an agent to
// run it and performs its writes for real inside a workflow preview. Nothing
// checked the claim: 85 commands asserted it and the only correctness test was
// one line about `items open`. An audit found agent-context creating ~/.zotio
// through loadProfileStore, which no grep would have caught, because the write
// was four calls away in a function named for computing a path.
//
// This test walks the package call graph from each read-only command's RunE and
// requires it to reach no write sink, unless it is named in readOnlyWriters with
// a reason. It fails in both directions, so the allowlist cannot quietly grow
// stale as commands change.
//
// Limits worth knowing: reachability through interface methods and function
// values is not resolved, so a pass is strong evidence rather than proof.

package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// readOnlyWriters are commands that reach a write sink and are still correctly
// marked read-only. Each entry says why. "Read-only" here means "does not change
// the user's library or durable state they did not name" — producing the export
// or report the command exists to produce, at a path the user supplied, counts
// as read-only.
var readOnlyWriters = map[string]string{
	"newAnnotationsExportCmd": "writes the annotations export to the path the user passes",
	"newCollectionsBundleCmd": "writes the bundle directory the user passes",
	"newCollectionsExportCmd": "writes the collections export to the path the user passes",
	"newDemoCmd":              "creates and cleans up its own sandbox directory",
	"newExportSnapshotCmd":    "writes the user's snapshot plus its own adjacent lock and checkpoint artifacts",
	"newImportDiscoverCmd":    "writes the discovery report to the path the user passes",
	"newLibraryWrappedCmd":    "writes the wrapped card to the path the user passes",
	"newTailCmd":              "builds and dispatches the command tree; the reachable sinks belong to the commands it runs, which carry their own annotations",

	// Known exception, not a settled one. `schema drift` writes
	// ~/.local/share/zotio/schema-baseline.json unconditionally on first run
	// (schema_drift.go, the !ok branch) and again under --update, to a default
	// path the user never names. It is mcp:hidden so it is not on the MCP tool
	// surface, but workflow_run.go keys on mcp:read-only alone, so a
	// `schema drift --update` step inside `workflow run --dry-run` rewrites the
	// baseline for real. Deciding whether a preview should capture a baseline is
	// a product call; until it is made, this entry documents the gap.
	"newSchemaDriftCmd": "writes the schema baseline to a default path; see the note above, this exception is unresolved",
}

// clientMutators are the *client.Client methods that reach the dry-run gate.
var clientMutators = map[string]bool{
	"Post": true, "PostWithHeaders": true, "PostFormWithHeaders": true,
	"Put": true, "PutWithHeaders": true,
	"Patch": true, "PatchWithHeaders": true,
	"Delete": true, "DeleteWithHeaders": true,
}

var osWriters = map[string]bool{
	"WriteFile": true, "Create": true, "OpenFile": true, "Remove": true,
	"RemoveAll": true, "Rename": true, "MkdirAll": true, "Mkdir": true,
	"Truncate": true, "Chmod": true, "Symlink": true, "Link": true,
	"MkdirTemp": true, "CreateTemp": true,
}

var statefulPkgs = map[string]bool{
	"store": true, "journal": true, "config": true, "cache": true,
	"mutation": true, "vault": true, "state": true,
}

var writeVerbs = []string{
	"Write", "Save", "Set", "Put", "Delete", "Remove", "Apply",
	"Insert", "Update", "Commit", "Append", "Record", "Persist", "Flush",
}

type cliPackage struct {
	fset  *token.FileSet
	files map[string]*ast.File
	funcs map[string]*ast.FuncDecl
	owner map[string]string
}

type readOnlyCmd struct {
	ctor string
	use  string
	file string
	run  *ast.FuncLit
}

type writeSink struct {
	fn   string
	file string
	line int
	kind string
	text string
}

func loadCLIPackage(t *testing.T) *cliPackage {
	t.Helper()
	p := &cliPackage{
		fset:  token.NewFileSet(),
		files: map[string]*ast.File{},
		funcs: map[string]*ast.FuncDecl{},
		owner: map[string]string{},
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(p.fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		p.files[name] = f
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			p.funcs[fn.Name.Name] = fn
			p.owner[fn.Name.Name] = name
		}
	}
	if len(p.funcs) < 500 {
		t.Fatalf("parsed only %d package functions; the walk is broken", len(p.funcs))
	}
	return p
}

// readOnlyCommands finds every cobra.Command literal annotated read-only.
// Annotations are not always inline: reading_list_state.go shares one through a
// helper, and a literal-only reader silently drops those commands.
func (p *cliPackage) readOnlyCommands() []readOnlyCmd {
	var out []readOnlyCmd
	for file, f := range p.files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !isCobraCommandLit(lit) {
					return true
				}
				use, annos := p.commandFacts(lit)
				if annos["mcp:read-only"] != "true" {
					return true
				}
				out = append(out, readOnlyCmd{ctor: fn.Name.Name, use: use, file: file, run: runEBody(lit)})
				return true
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ctor < out[j].ctor })
	return out
}

func isCobraCommandLit(lit *ast.CompositeLit) bool {
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "cobra" && sel.Sel.Name == "Command"
}

func (p *cliPackage) commandFacts(lit *ast.CompositeLit) (use string, annos map[string]string) {
	annos = map[string]string{}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "Use":
			if v := basicString(kv.Value); v != "" {
				use = v
			}
		case "Annotations":
			for k, v := range p.annotationMap(kv.Value) {
				annos[k] = v
			}
		}
	}
	return use, annos
}

func (p *cliPackage) annotationMap(e ast.Expr) map[string]string {
	switch v := e.(type) {
	case *ast.CompositeLit:
		out := map[string]string{}
		for _, el := range v.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k := basicString(kv.Key); k != "" {
				out[k] = basicString(kv.Value)
			}
		}
		return out
	case *ast.CallExpr:
		id, ok := v.Fun.(*ast.Ident)
		if !ok {
			return nil
		}
		fn := p.funcs[id.Name]
		if fn == nil || fn.Body == nil {
			return nil
		}
		var out map[string]string
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ret, ok := n.(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 || out != nil {
				return true
			}
			if lit, ok := ret.Results[0].(*ast.CompositeLit); ok {
				out = p.annotationMap(lit)
			}
			return true
		})
		return out
	}
	return nil
}

func basicString(e ast.Expr) string {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	return strings.Trim(lit.Value, "`\"")
}

// runEBody is the right entry point for reachability. A parent group's
// constructor calls its children's constructors, so entering at the constructor
// makes `journal` look like it reaches `journal undo`'s writes.
func runEBody(lit *ast.CompositeLit) *ast.FuncLit {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || (key.Name != "RunE" && key.Name != "Run") {
			continue
		}
		if fl, ok := kv.Value.(*ast.FuncLit); ok {
			return fl
		}
	}
	return nil
}

func (p *cliPackage) sinksFrom(c readOnlyCmd) []writeSink {
	if c.run == nil {
		return nil
	}
	var out []writeSink
	out = append(out, p.markers(c.run.Body, c.ctor+" (inline RunE)", c.file)...)

	seen := map[string]bool{}
	var walk func(ast.Node)
	walk = func(n ast.Node) {
		ast.Inspect(n, func(x ast.Node) bool {
			call, ok := x.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || seen[id.Name] {
				return true
			}
			fn := p.funcs[id.Name]
			if fn == nil || fn.Body == nil {
				return true
			}
			seen[id.Name] = true
			out = append(out, p.markers(fn.Body, id.Name, p.owner[id.Name])...)
			walk(fn.Body)
			return true
		})
	}
	walk(c.run.Body)

	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].line < out[j].line
	})
	return out
}

func (p *cliPackage) markers(body ast.Node, fnName, file string) []writeSink {
	var out []writeSink
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		line := p.fset.Position(call.Pos()).Line
		switch {
		case clientMutators[sel.Sel.Name]:
			out = append(out, writeSink{fnName, file, line, "HTTP", sel.Sel.Name})
		case recv.Name == "os" && osWriters[sel.Sel.Name]:
			out = append(out, writeSink{fnName, file, line, "LOCAL", "os." + sel.Sel.Name})
		case statefulPkgs[recv.Name] && hasWriteVerb(sel.Sel.Name):
			out = append(out, writeSink{fnName, file, line, "LOCAL", recv.Name + "." + sel.Sel.Name})
		}
		return true
	})
	return out
}

func hasWriteVerb(name string) bool {
	for _, v := range writeVerbs {
		if strings.HasPrefix(name, v) {
			return true
		}
	}
	return false
}

func TestReadOnlyCommandsReachNoWriteSink(t *testing.T) {
	p := loadCLIPackage(t)
	cmds := p.readOnlyCommands()
	if len(cmds) < 60 {
		t.Fatalf("found %d read-only commands, want the full set; annotation discovery is broken", len(cmds))
	}

	claimed := map[string]bool{}
	for _, c := range cmds {
		sinks := p.sinksFrom(c)
		reason, allowed := readOnlyWriters[c.ctor]
		if allowed {
			claimed[c.ctor] = true
		}
		switch {
		case len(sinks) > 0 && !allowed:
			t.Errorf("%s (%q, %s) is annotated mcp:read-only=true but reaches %d write sink(s):\n%s\n"+
				"  A read-only command is exempt from --dry-run injection in workflow previews and is\n"+
				"  offered to MCP agents as safe. Either stop the write, drop the annotation, or add\n"+
				"  %s to readOnlyWriters with the reason it is legitimate.",
				c.ctor, c.use, c.file, len(sinks), formatSinks(sinks), c.ctor)
		case len(sinks) == 0 && allowed:
			t.Errorf("%s no longer reaches any write sink, so readOnlyWriters[%q] (%s) is stale; drop the entry",
				c.ctor, c.ctor, reason)
		}
	}

	for ctor := range readOnlyWriters {
		if !claimed[ctor] {
			t.Errorf("readOnlyWriters names %s, which is not a read-only command in this package", ctor)
		}
	}
}

func formatSinks(sinks []writeSink) string {
	shown := sinks
	truncated := 0
	if len(shown) > 10 {
		truncated = len(shown) - 10
		shown = shown[:10]
	}
	var b strings.Builder
	for _, s := range shown {
		b.WriteString("    " + s.kind + " " + filepath.Base(s.file) + ":" +
			itoa(s.line) + "  " + s.text + "  (in " + s.fn + ")\n")
	}
	if truncated > 0 {
		b.WriteString("    … " + itoa(truncated) + " more\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
