package swe

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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
			if n.Sel.Name == "Acquire" && selectorEndsWith(n.X, "leases") {
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

func selectorEndsWith(expr ast.Expr, name string) bool {
	switch expr := expr.(type) {
	case *ast.Ident:
		return expr.Name == name
	case *ast.SelectorExpr:
		return expr.Sel.Name == name
	default:
		return false
	}
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
