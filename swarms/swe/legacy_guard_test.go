package swe

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const (
	loopImportPath    = "github.com/looprig/harness/pkg/loop"
	sessionImportPath = "github.com/looprig/harness/pkg/session"
	toolsImportPath   = "github.com/looprig/harness/pkg/tools"
	journalImportPath = "github.com/looprig/harness/pkg/journal"
	sessionStorePath  = "github.com/looprig/harness/pkg/sessionstore"
	storageImportPath = "github.com/looprig/storage"
)

var forbiddenIdentifiers = map[string]string{
	"swarmSpawner":        "custom delegation spawner",
	"subagentRunner":      "custom delegation runner",
	"RunSubagent":         "custom delegation entry point",
	"NewSubagent":         "custom Subagent constructor",
	"watchSessionEvents":  "manual session event watcher",
	"CheckpointWorkspace": "manual idle checkpoint wiring",
	"scheduleGC":          "manual session object-GC scheduler",
	"stopGCTeardown":      "manual session object-GC teardown",
	"PrimaryLoopID":       "legacy primary-loop identity",
	"RootLoopID":          "removed root-loop identity",
	"rootLoopID":          "removed root-loop state",
}

var forbiddenPackageMembers = map[string]map[string]string{
	loopImportPath: {
		"Config":  "loop.Config",
		"ToolSet": "loop.ToolSet",
	},
	sessionImportPath: {
		"New":                         "session.New",
		"Restore":                     "session.Restore",
		"Option":                      "session.Option",
		"Limits":                      "session.Limits",
		"ConfigFingerprintFields":     "session.ConfigFingerprintFields",
		"WithLimits":                  "manual session.WithLimits option",
		"WithCeiling":                 "manual session.WithCeiling option",
		"WithConfigFingerprintFields": "manual session.WithConfigFingerprintFields option",
		"WithWorkspaceStore":          "manual session.WithWorkspaceStore option",
		"WithWorkspaceCheckpointing":  "manual session.WithWorkspaceCheckpointing option",
		"WithSessionID":               "manual session.WithSessionID option",
		"WithEventAppender":           "manual session.WithEventAppender option",
		"WithCommandAppender":         "manual session.WithCommandAppender option",
		"WithLeaseRelease":            "manual session.WithLeaseRelease option",
		"WithGateAppender":            "manual session.WithGateAppender option",
	},
	journalImportPath: {
		"NewJournalEventAppenderChecked":   "manual journal event-appender wiring",
		"NewJournalCommandAppenderChecked": "manual journal command-appender wiring",
	},
	sessionStorePath: {
		"OpenObjectGC": "manual session object-GC wiring",
	},
	toolsImportPath: {
		"NewSubagent": "custom Subagent constructor",
	},
}

func legacyProductionDiagnostics() ([]string, error) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return nil, fmt.Errorf("locate legacy guard source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	var diagnostics []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == "vendor" || strings.HasPrefix(entry.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		diagnostics = append(diagnostics, legacySourceDiagnostics(path, source)...)
		return nil
	})
	return diagnostics, err
}

func legacySourceDiagnostics(filename string, source []byte) []string {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, source, 0)
	if err != nil {
		return []string{fmt.Sprintf("%s: parse: %v", filename, err)}
	}
	typeInfo := guardTypeInfo(fset, file)

	imports := make(map[string]string)
	dotImports := make(map[string]bool)
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := filepath.Base(path)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		switch name {
		case "_":
			continue
		case ".":
			dotImports[path] = true
		default:
			imports[name] = path
		}
	}

	seen := make(map[token.Pos]bool)
	var diagnostics []string
	report := func(pos token.Pos, legacy string) {
		if seen[pos] {
			return
		}
		seen[pos] = true
		position := fset.Position(pos)
		diagnostics = append(diagnostics, fmt.Sprintf("%s:%d:%d: %s", position.Filename, position.Line, position.Column, legacy))
	}

	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.Ident:
			if legacy, ok := forbiddenIdentifiers[n.Name]; ok {
				report(n.Pos(), legacy)
			}
			if n.Name == "AcceptsImages" {
				if obj := typeInfo.Defs[n]; obj != nil && isZeroArgumentCallable(obj.Type()) {
					report(n.Pos(), "static zero-argument AcceptsImages declaration")
				}
			}
			if n.Obj == nil {
				for path := range dotImports {
					if legacy, ok := forbiddenPackageMembers[path][n.Name]; ok {
						report(n.Pos(), legacy)
					}
				}
			}
		case *ast.SelectorExpr:
			qualifier, ok := n.X.(*ast.Ident)
			if ok && qualifier.Obj == nil {
				if legacy, found := forbiddenPackageMembers[imports[qualifier.Name]][n.Sel.Name]; found {
					report(n.Sel.Pos(), legacy)
				}
			}
			if n.Sel.Name == "Acquire" && isManualLeaseCollaborator(typeInfo.TypeOf(n.X)) {
				report(n.Sel.Pos(), "manual session lease acquisition")
			}
		case *ast.StarExpr:
			if selectorFromPackage(n.X, imports, sessionImportPath, "Session") {
				report(n.Pos(), "concrete *session.Session")
			} else if ident, ok := n.X.(*ast.Ident); ok && ident.Obj == nil && ident.Name == "Session" && dotImports[sessionImportPath] {
				report(n.Pos(), "concrete *session.Session")
			}
		case *ast.CallExpr:
			if len(n.Args) == 0 && calledName(n.Fun) == "AcceptsImages" {
				report(n.Fun.Pos(), "static zero-argument AcceptsImages")
			}
		case *ast.FuncDecl:
			if n.Name.Name == "AcceptsImages" && fieldCount(n.Type.Params) == 0 {
				report(n.Name.Pos(), "static zero-argument AcceptsImages declaration")
			}
		case *ast.ValueSpec:
			for i, name := range n.Names {
				if name.Name != "AcceptsImages" {
					continue
				}
				if fn, ok := n.Type.(*ast.FuncType); ok && fieldCount(fn.Params) == 0 {
					report(name.Pos(), "static zero-argument AcceptsImages variable")
					continue
				}
				if i < len(n.Values) {
					if fn, ok := n.Values[i].(*ast.FuncLit); ok && fieldCount(fn.Type.Params) == 0 {
						report(name.Pos(), "static zero-argument AcceptsImages variable")
					}
				}
			}
		case *ast.TypeSpec:
			if n.Name.Name == "AcceptsImages" {
				if fn, ok := n.Type.(*ast.FuncType); ok && fieldCount(fn.Params) == 0 {
					report(n.Name.Pos(), "static zero-argument AcceptsImages function type")
				}
			}
		case *ast.Field:
			for _, name := range n.Names {
				if name.Name == "AcceptsImages" {
					if fn, ok := n.Type.(*ast.FuncType); ok && fieldCount(fn.Params) == 0 {
						report(name.Pos(), "static zero-argument AcceptsImages contract")
					}
				}
			}
		}
		return true
	})
	return diagnostics
}

func selectorFromPackage(expr ast.Expr, imports map[string]string, importPath, member string) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != member {
		return false
	}
	qualifier, ok := selector.X.(*ast.Ident)
	return ok && qualifier.Obj == nil && imports[qualifier.Name] == importPath
}

type guardImporter struct{ standard types.Importer }

func (i guardImporter) Import(path string) (*types.Package, error) {
	switch path {
	case storageImportPath:
		pkg := types.NewPackage(path, "storage")
		anyType := types.Universe.Lookup("any").Type()
		params := types.NewTuple(types.NewVar(token.NoPos, pkg, "ctx", anyType), types.NewVar(token.NoPos, pkg, "name", anyType))
		results := types.NewTuple(types.NewVar(token.NoPos, pkg, "lease", anyType), types.NewVar(token.NoPos, pkg, "err", anyType))
		acquire := types.NewFunc(token.NoPos, pkg, "Acquire", types.NewSignatureType(nil, nil, nil, params, results, false))
		iface := types.NewInterfaceType([]*types.Func{acquire}, nil)
		iface.Complete()
		obj := types.NewTypeName(token.NoPos, pkg, "Leaser", nil)
		types.NewNamed(obj, iface, nil)
		pkg.Scope().Insert(obj)
		pkg.MarkComplete()
		return pkg, nil
	case journalImportPath:
		pkg := types.NewPackage(path, "journal")
		obj := types.NewTypeName(token.NoPos, pkg, "LeaseManager", nil)
		types.NewNamed(obj, types.NewStruct(nil, nil), nil)
		pkg.Scope().Insert(obj)
		pkg.MarkComplete()
		return pkg, nil
	default:
		if i.standard != nil {
			if pkg, err := i.standard.Import(path); err == nil {
				return pkg, nil
			}
		}
		pkg := types.NewPackage(path, filepath.Base(path))
		pkg.MarkComplete()
		return pkg, nil
	}
}

func guardTypeInfo(fset *token.FileSet, file *ast.File) *types.Info {
	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Uses:       make(map[*ast.Ident]types.Object),
		Defs:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}
	config := &types.Config{Importer: guardImporter{standard: importer.Default()}, Error: func(error) {}}
	_, _ = config.Check("guard/fixture", fset, []*ast.File{file}, info)
	return info
}

func isManualLeaseCollaborator(t types.Type) bool {
	if pointer, ok := t.(*types.Pointer); ok {
		t = pointer.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	path := named.Obj().Pkg().Path()
	name := named.Obj().Name()
	return (path == storageImportPath && name == "Leaser") || (path == journalImportPath && name == "LeaseManager")
}

func isZeroArgumentCallable(t types.Type) bool {
	if t == nil {
		return false
	}
	t = types.Unalias(t)
	sig, ok := t.Underlying().(*types.Signature)
	return ok && sig.Params().Len() == 0
}

func calledName(expr ast.Expr) string {
	switch expr := expr.(type) {
	case *ast.Ident:
		return expr.Name
	case *ast.SelectorExpr:
		return expr.Sel.Name
	default:
		return ""
	}
}

func fieldCount(fields *ast.FieldList) int {
	if fields == nil {
		return 0
	}
	count := 0
	for _, field := range fields.List {
		if len(field.Names) == 0 {
			count++
		} else {
			count += len(field.Names)
		}
	}
	return count
}

func TestLegacySourceGuardRecognizesImportsAndShadowing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		source     string
		wantLegacy bool
	}{
		{
			name: "aliased harness loop import",
			source: `package fixture
import hloop "github.com/looprig/harness/pkg/loop"
var _ hloop.Config
`,
			wantLegacy: true,
		},
		{
			name: "shadowed import alias is ordinary field selection",
			source: `package fixture
import loop "github.com/looprig/harness/pkg/loop"
var _ = loop.Define
func f() { loop := struct{ Config int }{}; _ = loop.Config }
`,
		},
		{
			name: "aliased concrete session pointer",
			source: `package fixture
import runtimeSession "github.com/looprig/harness/pkg/session"
var _ *runtimeSession.Session
`,
			wantLegacy: true,
		},
		{
			name: "dot imported concrete session pointer",
			source: `package fixture
import . "github.com/looprig/harness/pkg/session"
var _ *Session
`,
			wantLegacy: true,
		},
		{
			name: "comments and strings are inert",
			source: `package fixture
// session.New and loop.ToolSet are migration history.
const history = "PrimaryLoopID AcceptsImages()"
`,
		},
		{
			name: "legacy declaration",
			source: `package fixture
type swarmSpawner struct{}
`,
			wantLegacy: true,
		},
		{
			name: "zero argument image capability",
			source: `package fixture
func f(agent interface{ AcceptsImages() bool }) { _ = agent.AcceptsImages() }
`,
			wantLegacy: true,
		},
		{
			name: "loop targeted image capability",
			source: `package fixture
func f(agent interface{ AcceptsImages(string) bool }) { _ = agent.AcceptsImages("child") }
`,
		},
		{
			name: "unrelated shadowed leases acquire",
			source: `package fixture
type unrelated struct{}
func (*unrelated) Acquire() {}
func f() { leases := &unrelated{}; leases.Acquire() }
`,
		},
		{
			name: "renamed storage leaser acquire",
			source: `package fixture
import store "github.com/looprig/storage"
func f(leaseStore store.Leaser) { _, _ = leaseStore.Acquire(nil, "session") }
`,
			wantLegacy: true,
		},
		{
			name: "dot imported storage leaser acquire",
			source: `package fixture
import . "github.com/looprig/storage"
func f(leaseStore Leaser) { _, _ = leaseStore.Acquire(nil, "session") }
`,
			wantLegacy: true,
		},
		{
			name: "zero argument image capability variable",
			source: `package fixture
var AcceptsImages = func() bool { return true }
`,
			wantLegacy: true,
		},
		{
			name: "zero argument image capability function type",
			source: `package fixture
type AcceptsImages func() bool
`,
			wantLegacy: true,
		},
		{
			name: "inferred existing zero argument image capability",
			source: `package fixture
func existingZeroArgFunc() bool { return true }
var AcceptsImages = existingZeroArgFunc
`,
			wantLegacy: true,
		},
		{
			name: "named zero argument image capability variable",
			source: `package fixture
type ZeroArgFunc func() bool
var AcceptsImages ZeroArgFunc
`,
			wantLegacy: true,
		},
		{
			name: "defined from zero argument image capability type",
			source: `package fixture
type ZeroArgFunc func() bool
type AcceptsImages ZeroArgFunc
`,
			wantLegacy: true,
		},
		{
			name: "alias of zero argument image capability type",
			source: `package fixture
type ZeroArgFunc func() bool
type AcceptsImages = ZeroArgFunc
`,
			wantLegacy: true,
		},
		{
			name: "inferred one argument image capability is allowed",
			source: `package fixture
func existingOneArgFunc(string) bool { return true }
var AcceptsImages = existingOneArgFunc
`,
		},
		{
			name: "named one argument image capability declarations are allowed",
			source: `package fixture
type OneArgFunc func(string) bool
var AcceptsImages OneArgFunc
type ImageCapability OneArgFunc
`,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			diagnostics := legacySourceDiagnostics("fixture.go", []byte(tt.source))
			if got := len(diagnostics) > 0; got != tt.wantLegacy {
				t.Fatalf("legacy diagnostics = %v, wantLegacy %v", diagnostics, tt.wantLegacy)
			}
		})
	}
}

func TestNoLegacySessionWiringInProduction(t *testing.T) {
	t.Parallel()

	diagnostics, err := legacyProductionDiagnostics()
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("legacy session wiring remains in production:\n%s", strings.Join(diagnostics, "\n"))
	}
}
