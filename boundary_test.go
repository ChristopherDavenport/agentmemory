package agentmemory

import (
	"go/build"
	"strings"
	"testing"
)

const (
	modulePath   = "github.com/ChristopherDavenport/agentmemory"
	agenttoolPkg = "github.com/ChristopherDavenport/agenttool"
	openresp     = "github.com/ChristopherDavenport/openresponses"
	agentturnPkg = "github.com/ChristopherDavenport/agentturn"
)

// TestImportBoundary enforces the module's dependency rules: the root
// package imports openresponses, agenttool and the standard library
// only; agentturn appears in test files alone; filestore and storetest
// import this module's root package and the standard library alone.
func TestImportBoundary(t *testing.T) {
	root, err := build.Default.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range root.Imports {
		switch {
		case isStandard(imp):
		case imp == agenttoolPkg, imp == openresp:
		default:
			t.Errorf("agentmemory imports %q; only agenttool, openresponses and the standard library are allowed", imp)
		}
	}
	for _, imp := range append(root.TestImports, root.XTestImports...) {
		switch {
		case isStandard(imp):
		case strings.HasPrefix(imp, modulePath):
		case strings.HasPrefix(imp, agenttoolPkg), strings.HasPrefix(imp, openresp):
		case imp == agentturnPkg:
			// The tools' integration test drives the loop.
		default:
			t.Errorf("agentmemory tests import %q; that is not an allowed test dependency", imp)
		}
	}
	for _, dir := range []string{"filestore", "storetest"} {
		pkg, err := build.Default.ImportDir(dir, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range append(append(pkg.Imports, pkg.TestImports...), pkg.XTestImports...) {
			if !isStandard(imp) && !strings.HasPrefix(imp, modulePath) {
				t.Errorf("%s imports %q; only this module and the standard library are allowed", dir, imp)
			}
		}
	}
}

func isStandard(imp string) bool {
	first, _, _ := strings.Cut(imp, "/")
	return !strings.Contains(first, ".")
}
