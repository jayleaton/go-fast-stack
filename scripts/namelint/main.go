// Command namelint fails on single-character names (AGENTS.md "Taste"):
// every identifier a package declares, in production code and tests alike:
// receivers, parameters, named results, locals, loop and closure variables,
// type-switch variables, constants, types, type parameters, struct fields,
// labels and import aliases. `_` is not a name. Generated files (sqlc output,
// mocks) are skipped, except templ output: templ copies the Go in a .templ
// file verbatim and names its own variables templ_7745c5c3_*, so a short
// name there is the .templ source's, and the report says to fix it there.
//
// golangci-lint has no linter that catches all of these: varnamelen lets a
// name through when its uses are within a line of the declaration and always
// allows t, b, m and the like. `make lint`, `make check-fast` and CI run this
// next to golangci-lint:
//
//	go run ./scripts/namelint ./...
//
// There is no nolint escape: rename the identifier.
package main

import (
	"go/ast"
	"go/token"
	"strings"
	"unicode/utf8"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/singlechecker"
)

// hint is what every report tells the reader to do.
const hint = "use a descriptive name: writer, request, test, index, service, caller (AGENTS.md \"Taste\")"

var analyzer = &analysis.Analyzer{
	Name: "namelint",
	Doc:  "reports single-character identifiers",
	Run:  run,
}

func main() { singlechecker.Main(analyzer) }

func run(pass *analysis.Pass) (any, error) {
	skipped := map[*token.File]bool{}
	templ := map[*token.File]bool{}
	for _, file := range pass.Files {
		tokenFile := pass.Fset.File(file.Pos())
		if strings.HasSuffix(tokenFile.Name(), "_templ.go") {
			templ[tokenFile] = true
		} else if ast.IsGenerated(file) {
			skipped[tokenFile] = true
		}
	}
	// Defs holds every declared identifier; a type switch's symbolic
	// variable (`switch v := x.(type)`) maps to nil but is still a name.
	for ident := range pass.TypesInfo.Defs {
		if ident.Name == "_" || utf8.RuneCountInString(ident.Name) != 1 {
			continue
		}
		tokenFile := pass.Fset.File(ident.Pos())
		switch {
		case skipped[tokenFile]:
		case templ[tokenFile]:
			pass.Reportf(ident.Pos(), "single-character name %q in the .templ source of this file: %s", ident.Name, hint)
		default:
			pass.Reportf(ident.Pos(), "single-character name %q: %s", ident.Name, hint)
		}
	}
	return nil, nil
}
