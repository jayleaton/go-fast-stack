// Command rename gives every single-character identifier that namelint
// reports a descriptive name, type-aware: it resolves each declaration and all
// its uses with go/types (across packages and their tests), picks a name from
// the type and the context (writer for http.ResponseWriter, test for
// *testing.T, user when ranging over users, service for a *Service
// receiver), and refuses any name that would collide with or shadow another
// identifier in scope. Names in templ components are renamed in the .templ
// source through templ's source map. What it cannot name safely it lists,
// for a name in -overrides or a hand edit.
//
//	go run ./scripts/namelint/rename [-apply] [-overrides file] [-only dir,...] ./...
//
// Without -apply it prints the plan. After -apply: gofmt, `make generate`
// (templ output and mocks follow the sources), then `make check-fast`.
//
// An overrides line is `<file>:<line>:<col> <name>` for the declaration at
// that position (as namelint reports it), or `<file> <old> <name>` for every
// declaration of <old> in that file. A position override also renames a longer name
// (a second pass over a name the first one chose).
package main

import (
	"bufio"
	"cmp"
	"flag"
	"fmt"
	"go/ast"
	"go/scanner"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/a-h/templ"
	"github.com/a-h/templ/generator"
	parser "github.com/a-h/templ/parser/v2"
	"golang.org/x/tools/go/packages"
)

// declaration is one single-character identifier, merged across the package
// variants (a package, its test build and its external tests) that see it.
type declaration struct {
	key      string // file:line:col of the declaring identifier
	file     string
	line     int
	column   int
	name     string
	objects  []types.Object // one per variant; nil entries for type-switch symbols
	variants []*packages.Package
	scopes   []*types.Scope
	uses     map[string]position
	rangeOf  ast.Expr // the ranged expression when this is a range variable
	isKey    bool     // the key (not the value) of a range statement
	callee   string   // the called function when declared as `x := f(...)`
	receiver *types.TypeName
	newName  string
	reason   string // why it was left alone
}

type position struct {
	file   string
	line   int
	column int
}

func (spot position) String() string {
	return fmt.Sprintf("%s:%d:%d", spot.file, spot.line, spot.column)
}

func main() {
	apply := flag.Bool("apply", false, "rewrite the files (default: print the plan)")
	overridesFile := flag.String("overrides", "", "file of names to use instead of the derived ones")
	only := flag.String("only", "", "comma-separated directory prefixes to rename in (default: all)")
	flag.Parse()

	overrides := readOverrides(*overridesFile)

	config := &packages.Config{
		Mode:  packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports,
		Tests: true,
	}
	loaded, err := packages.Load(config, flag.Args()...)
	check(err)
	if packages.PrintErrors(loaded) > 0 {
		fail("packages have errors; fix the build first")
	}

	declarations := collect(loaded, overrides)
	var selected []*declaration
	for _, decl := range declarations {
		if *only == "" || hasAnyPrefix(decl.file, strings.Split(*only, ",")) {
			selected = append(selected, decl)
		}
	}
	slices.SortFunc(selected, func(left, right *declaration) int {
		return cmp.Or(cmp.Compare(left.file, right.file), cmp.Compare(left.line, right.line), cmp.Compare(left.column, right.column))
	})

	planner := &planner{declarations: selected, receiverNames: map[*types.TypeName]string{}}
	for _, decl := range selected {
		planner.plan(decl, overrides)
	}

	renamed, skipped := 0, 0
	perDir := map[string]int{}
	for _, decl := range selected {
		if decl.newName == "" {
			skipped++
			fmt.Printf("SKIP  %s %s: %s\n", decl.key, decl.name, decl.reason)
			continue
		}
		renamed++
		perDir[filepath.Dir(decl.file)]++
		if !*apply {
			fmt.Printf("%s %s -> %s (%d uses)\n", decl.key, decl.name, decl.newName, len(decl.uses))
		}
	}
	dirs := slices.Sorted(maps.Keys(perDir))
	for _, dir := range dirs {
		fmt.Printf("renamed %4d  %s\n", perDir[dir], dir)
	}
	fmt.Printf("renamed %d, left %d\n", renamed, skipped)
	if *apply {
		check(applyEdits(selected))
	}
}

// collect finds every single-character declaration in the loaded packages and
// all of its uses.
func collect(loaded []*packages.Package, overrides map[string]string) map[string]*declaration {
	declarations := map[string]*declaration{}
	symbolOf := map[types.Object]string{} // a type switch's implicit objects -> its symbol's key
	for _, pkg := range loaded {
		skipped := generatedFiles(pkg)
		rangeVars := rangeVariables(pkg)
		callees := assignedCalls(pkg)
		for ident, object := range pkg.TypesInfo.Defs {
			spot := where(pkg.Fset, ident.Pos())
			_, named := overrides[spot.String()]
			if ident.Name == "_" || (utf8.RuneCountInString(ident.Name) != 1 && !named) {
				continue
			}
			if skipped[spot.file] {
				continue
			}
			key := spot.String()
			decl := declarations[key]
			if decl == nil {
				decl = &declaration{key: key, file: spot.file, line: spot.line, column: spot.column, name: ident.Name, uses: map[string]position{}}
				declarations[key] = decl
			}
			decl.objects = append(decl.objects, object)
			decl.variants = append(decl.variants, pkg)
			if object != nil {
				decl.scopes = append(decl.scopes, object.Parent())
			} else {
				for _, implicit := range typeSwitchImplicits(pkg, ident) {
					symbolOf[implicit] = key
				}
				decl.scopes = append(decl.scopes, nil)
			}
			if callee, ok := callees[ident]; ok {
				decl.callee = callee
			}
			if info, ok := rangeVars[ident]; ok {
				decl.rangeOf, decl.isKey = info.expr, info.isKey
			}
			if variable, ok := object.(*types.Var); ok {
				decl.receiver = receiverType(pkg, ident, variable)
			}
		}
	}
	for _, pkg := range loaded {
		for ident, object := range pkg.TypesInfo.Uses {
			key, ok := symbolOf[object]
			if !ok {
				key = where(pkg.Fset, object.Pos()).String()
			}
			if decl := declarations[key]; decl != nil && decl.name == ident.Name {
				spot := where(pkg.Fset, ident.Pos())
				decl.uses[spot.String()] = spot
			}
		}
	}
	return declarations
}

type rangeInfo struct {
	expr  ast.Expr
	isKey bool
}

func rangeVariables(pkg *packages.Package) map[*ast.Ident]rangeInfo {
	found := map[*ast.Ident]rangeInfo{}
	for _, file := range pkg.Syntax {
		ast.Inspect(file, func(node ast.Node) bool {
			statement, ok := node.(*ast.RangeStmt)
			if !ok {
				return true
			}
			if key, ok := statement.Key.(*ast.Ident); ok {
				found[key] = rangeInfo{statement.X, true}
			}
			if value, ok := statement.Value.(*ast.Ident); ok {
				found[value] = rangeInfo{statement.X, false}
			}
			return true
		})
	}
	return found
}

// assignedCalls maps the first identifier of `x := f(...)` / `x, err :=
// f(...)` / `var x = f(...)` to the called function's name.
func assignedCalls(pkg *packages.Package) map[*ast.Ident]string {
	found := map[*ast.Ident]string{}
	calleeName := func(expr ast.Expr) string {
		call, ok := expr.(*ast.CallExpr)
		if !ok {
			return ""
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			return fun.Name
		case *ast.SelectorExpr:
			// A standard library function names its result poorly
			// (strings.IndexByte, strconv.ParseBool): fall back to the type.
			if pkgIdent, ok := fun.X.(*ast.Ident); ok {
				if pkgName, ok := pkg.TypesInfo.Uses[pkgIdent].(*types.PkgName); ok && !strings.Contains(pkgName.Imported().Path(), ".") {
					return ""
				}
			}
			return fun.Sel.Name
		}
		return ""
	}
	for _, file := range pkg.Syntax {
		ast.Inspect(file, func(node ast.Node) bool {
			switch statement := node.(type) {
			case *ast.AssignStmt:
				if len(statement.Rhs) == 1 && len(statement.Lhs) > 0 {
					if ident, ok := statement.Lhs[0].(*ast.Ident); ok {
						if name := calleeName(statement.Rhs[0]); name != "" {
							found[ident] = name
						}
					}
				}
			case *ast.ValueSpec:
				if len(statement.Values) == 1 && len(statement.Names) > 0 {
					if name := calleeName(statement.Values[0]); name != "" {
						found[statement.Names[0]] = name
					}
				}
			}
			return true
		})
	}
	return found
}

func typeSwitchImplicits(pkg *packages.Package, symbol *ast.Ident) []types.Object {
	var implicits []types.Object
	for _, file := range pkg.Syntax {
		ast.Inspect(file, func(node ast.Node) bool {
			statement, ok := node.(*ast.TypeSwitchStmt)
			if !ok {
				return true
			}
			assign, ok := statement.Assign.(*ast.AssignStmt)
			if !ok || len(assign.Lhs) != 1 || assign.Lhs[0] != symbol {
				return true
			}
			for _, clause := range statement.Body.List {
				if implicit := pkg.TypesInfo.Implicits[clause]; implicit != nil {
					implicits = append(implicits, implicit)
				}
			}
			return false
		})
	}
	return implicits
}

// receiverType is the named type when variable is a method's receiver.
func receiverType(pkg *packages.Package, ident *ast.Ident, variable *types.Var) *types.TypeName {
	for _, file := range pkg.Syntax {
		for _, decl := range file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Recv == nil || len(function.Recv.List) == 0 {
				continue
			}
			for _, name := range function.Recv.List[0].Names {
				if name == ident {
					if named, ok := types.Unalias(derefType(variable.Type())).(*types.Named); ok {
						return named.Obj()
					}
				}
			}
		}
	}
	return nil
}

func generatedFiles(pkg *packages.Package) map[string]bool {
	skipped := map[string]bool{}
	for _, file := range pkg.Syntax {
		name := pkg.Fset.File(file.Pos()).Name()
		if ast.IsGenerated(file) && !strings.HasSuffix(name, "_templ.go") {
			skipped[relative(name)] = true
		}
	}
	return skipped
}

var workingDir, _ = os.Getwd()

func relative(name string) string {
	if rel, err := filepath.Rel(workingDir, name); err == nil {
		return rel
	}
	return name
}

func where(fset *token.FileSet, pos token.Pos) position {
	spot := fset.Position(pos)
	return position{file: relative(spot.Filename), line: spot.Line, column: spot.Column}
}

// planner chooses names, remembering what it already chose so that two
// renamed identifiers never end up shadowing each other.
type planner struct {
	declarations  []*declaration
	receiverNames map[*types.TypeName]string
	chosen        []*declaration
}

func (planner *planner) plan(decl *declaration, overrides map[string]string) {
	candidates := []string{}
	if name, ok := overrides[decl.key]; ok {
		candidates = append(candidates, name)
	} else if name, ok := overrides[decl.file+" "+decl.name]; ok {
		candidates = append(candidates, name)
	}
	overridden := len(candidates) > 0
	if decl.receiver != nil {
		if name, ok := planner.receiverNames[decl.receiver]; ok {
			candidates = append(candidates, name)
		}
	}
	candidates = append(candidates, derive(decl)...)
	if len(candidates) == 0 {
		decl.reason = "no name derived (add an override)"
		return
	}
	for _, candidate := range candidates {
		if problem := planner.conflict(decl, candidate); problem != "" {
			decl.reason = fmt.Sprintf("%s: %s", candidate, problem)
			if overridden && candidate == candidates[0] {
				fmt.Printf("OVERRIDE REFUSED  %s %s -> %s\n", decl.key, decl.name, decl.reason)
			}
			continue
		}
		decl.newName = candidate
		decl.reason = ""
		planner.chosen = append(planner.chosen, decl)
		if decl.receiver != nil {
			if _, ok := planner.receiverNames[decl.receiver]; !ok {
				planner.receiverNames[decl.receiver] = candidate
			}
		}
		return
	}
}

var reserved = func() map[string]bool {
	words := map[string]bool{}
	for _, keyword := range []string{"break", "case", "chan", "const", "continue", "default", "defer", "else", "fallthrough", "for", "func", "go", "goto", "if", "import", "interface", "map", "package", "range", "return", "select", "struct", "switch", "type", "var"} {
		words[keyword] = true
	}
	for _, name := range types.Universe.Names() {
		words[name] = true
	}
	return words
}()

// conflict says why name cannot replace decl's identifier ("" when it can).
func (planner *planner) conflict(decl *declaration, name string) string {
	if reserved[name] || !token.IsIdentifier(name) || utf8.RuneCountInString(name) < 2 {
		return "reserved or invalid"
	}
	for index, object := range decl.objects {
		pkg := decl.variants[index]
		scope := decl.scopes[index]
		if object != nil && scope == nil {
			// Struct fields and methods: the type's other members.
			if variable, ok := object.(*types.Var); ok && variable.IsField() {
				if problem := fieldConflict(pkg, variable, name); problem != "" {
					return problem
				}
				continue
			}
			if function, ok := object.(*types.Func); ok {
				if problem := methodConflict(function, name); problem != "" {
					return problem
				}
				continue
			}
			return "not a scoped name (rename by hand)"
		}
		if object == nil {
			scope = symbolScope(pkg, decl)
			if scope == nil {
				return "type switch scope not found"
			}
		}
		if scope.Lookup(name) != nil {
			return "already declared in the same scope"
		}
		for _, use := range decl.uses {
			inner := innermost(pkg, use)
			for current := inner; current != nil && current != scope; current = current.Parent() {
				if current.Lookup(name) != nil {
					return "shadowed at " + use.String()
				}
			}
		}
		for _, ident := range usesNamed(pkg)[name] {
			used := pkg.TypesInfo.Uses[ident]
			if used.Parent() == nil || ident.Pos() < bodyStart(pkg, scope) || ident.Pos() >= scope.End() {
				continue
			}
			if !within(used.Parent(), scope) {
				return "would capture " + name + " at " + where(pkg.Fset, ident.Pos()).String()
			}
		}
		// A package-level name must not take a name the package's files
		// import, or one they use from the universe (len, string, ...).
		if scope == pkg.Types.Scope() {
			for _, file := range pkg.Syntax {
				if fileScope := pkg.TypesInfo.Scopes[file]; fileScope != nil && fileScope.Lookup(name) != nil {
					return "a file of the package imports " + name
				}
			}
			for _, ident := range usesNamed(pkg)[name] {
				if used := pkg.TypesInfo.Uses[ident]; used.Parent() == types.Universe || used.Parent() != nil && used.Parent().Parent() == types.Universe {
					return "would capture " + name + " at " + where(pkg.Fset, ident.Pos()).String()
				}
			}
		}
	}
	for _, other := range planner.chosen {
		if other.newName != name || other.file != decl.file {
			continue
		}
		if other.name != decl.name && nested(other, decl) {
			return "another renamed identifier in an enclosing or enclosed scope takes it"
		}
	}
	return ""
}

var bodyStarts = map[*packages.Package]map[token.Pos]token.Pos{}

// bodyStart is where a function scope's names start to apply: its body (a
// parameter or receiver name never hides a type used in the signature).
// Other scopes start where they start.
func bodyStart(pkg *packages.Package, scope *types.Scope) token.Pos {
	starts, ok := bodyStarts[pkg]
	if !ok {
		starts = map[token.Pos]token.Pos{}
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(node ast.Node) bool {
				var funcType *ast.FuncType
				var body *ast.BlockStmt
				switch function := node.(type) {
				case *ast.FuncDecl:
					funcType, body = function.Type, function.Body
				case *ast.FuncLit:
					funcType, body = function.Type, function.Body
				}
				if funcType != nil && body != nil {
					if funcScope := pkg.TypesInfo.Scopes[funcType]; funcScope != nil {
						starts[funcScope.Pos()] = body.Lbrace
					}
				}
				return true
			})
		}
		bodyStarts[pkg] = starts
	}
	if start, ok := starts[scope.Pos()]; ok {
		return start
	}
	return scope.Pos()
}

var usesIndex = map[*packages.Package]map[string][]*ast.Ident{}

// usesNamed indexes a package's identifier uses by name.
func usesNamed(pkg *packages.Package) map[string][]*ast.Ident {
	if index, ok := usesIndex[pkg]; ok {
		return index
	}
	index := map[string][]*ast.Ident{}
	for ident := range pkg.TypesInfo.Uses {
		index[ident.Name] = append(index[ident.Name], ident)
	}
	usesIndex[pkg] = index
	return index
}

func fieldConflict(pkg *packages.Package, field *types.Var, name string) string {
	for _, typeName := range pkg.Types.Scope().Names() {
		named, ok := pkg.Types.Scope().Lookup(typeName).Type().(*types.Named)
		if !ok {
			continue
		}
		structure, ok := named.Underlying().(*types.Struct)
		if !ok {
			continue
		}
		for index := range structure.NumFields() {
			if structure.Field(index) == field {
				if found, _, _ := types.LookupFieldOrMethod(named, true, pkg.Types, name); found != nil {
					return "the struct already has " + name
				}
				return ""
			}
		}
	}
	for _, typed := range pkg.TypesInfo.Types {
		structure, ok := typed.Type.Underlying().(*types.Struct)
		if !ok {
			continue
		}
		for index := range structure.NumFields() {
			if structure.Field(index) != field {
				continue
			}
			for other := range structure.NumFields() {
				if structure.Field(other).Name() == name {
					return "the struct already has " + name
				}
			}
			return ""
		}
	}
	return "struct not found (rename by hand)"
}

func methodConflict(method *types.Func, name string) string {
	signature, ok := method.Type().(*types.Signature)
	if !ok || signature.Recv() == nil {
		return "not a method (rename by hand)"
	}
	if found, _, _ := types.LookupFieldOrMethod(signature.Recv().Type(), true, method.Pkg(), name); found != nil {
		return "the type already has " + name
	}
	return ""
}

func symbolScope(pkg *packages.Package, decl *declaration) *types.Scope {
	for _, file := range pkg.Syntax {
		if relative(pkg.Fset.File(file.Pos()).Name()) != decl.file {
			continue
		}
		var found *types.Scope
		ast.Inspect(file, func(node ast.Node) bool {
			statement, ok := node.(*ast.TypeSwitchStmt)
			if !ok || found != nil {
				return found == nil
			}
			if spot := where(pkg.Fset, statement.Pos()); spot.line <= decl.line {
				if scope := pkg.TypesInfo.Scopes[statement]; scope != nil && where(pkg.Fset, statement.End()).line >= decl.line {
					if assign, ok := statement.Assign.(*ast.AssignStmt); ok {
						if ident, ok := assign.Lhs[0].(*ast.Ident); ok && where(pkg.Fset, ident.Pos()).String() == decl.key {
							found = scope
						}
					}
				}
			}
			return true
		})
		return found
	}
	return nil
}

func innermost(pkg *packages.Package, spot position) *types.Scope {
	for _, file := range pkg.Syntax {
		tokenFile := pkg.Fset.File(file.Pos())
		if relative(tokenFile.Name()) != spot.file {
			continue
		}
		pos := tokenFile.LineStart(spot.line) + token.Pos(spot.column-1)
		return pkg.Types.Scope().Innermost(pos)
	}
	return nil
}

func within(scope, ancestor *types.Scope) bool {
	for current := scope; current != nil; current = current.Parent() {
		if current == ancestor {
			return true
		}
	}
	return false
}

// nested reports whether the two declarations' scopes overlap in the source.
func nested(left, right *declaration) bool {
	for _, leftScope := range left.scopes {
		for _, rightScope := range right.scopes {
			if leftScope == nil || rightScope == nil {
				return true
			}
			if leftScope.Pos() <= rightScope.End() && rightScope.Pos() <= leftScope.End() {
				return true
			}
		}
	}
	return false
}

// knownTypes names a value by its type, for types with one obvious name.
var knownTypes = map[string][]string{
	"net/http.Request":                     {"request"},
	"net/http.ResponseWriter":              {"writer"},
	"net/http.Handler":                     {"handler", "next"},
	"net/http.HandlerFunc":                 {"handler", "next"},
	"net/http.Client":                      {"client"},
	"net/http.Cookie":                      {"cookie"},
	"net/http.Header":                      {"header"},
	"net/http.Server":                      {"server"},
	"net/http.Response":                    {"response"},
	"net/http.ServeMux":                    {"mux"},
	"net/http/httptest.ResponseRecorder":   {"recorder"},
	"net/http/httptest.Server":             {"server"},
	"testing.T":                            {"test"},
	"testing.TB":                           {"test"},
	"testing.B":                            {"bench"},
	"testing.M":                            {"suite"},
	"testing.F":                            {"fuzz"},
	"context.Context":                      {"ctx"},
	"io.Writer":                            {"writer", "out"},
	"io.Reader":                            {"reader", "input"},
	"io.ReadCloser":                        {"body", "reader"},
	"os.File":                              {"file"},
	"bytes.Buffer":                         {"buffer", "buf"},
	"strings.Builder":                      {"builder"},
	"time.Time":                            {"moment", "when"},
	"time.Duration":                        {"duration"},
	"regexp.Regexp":                        {"pattern"},
	"net/url.URL":                          {"link", "address"},
	"net/url.Values":                       {"values", "query"},
	"sync.WaitGroup":                       {"group"},
	"log/slog.Logger":                      {"logger"},
	"log/slog.Attr":                        {"attr"},
	"log/slog.Record":                      {"record"},
	"log/slog.Value":                       {"value"},
	"log/slog.Level":                       {"level"},
	"encoding/json.Decoder":                {"decoder"},
	"encoding/json.Encoder":                {"encoder"},
	"github.com/jackc/pgx/v5.Rows":         {"rows"},
	"github.com/jackc/pgx/v5.Row":          {"row"},
	"github.com/jackc/pgx/v5.Tx":           {"tx"},
	"github.com/jackc/pgx/v5/pgxpool.Pool": {"pool"},
	"go.uber.org/mock/gomock.Controller":   {"ctrl"},
	"reflect.Value":                        {"value"},
	"reflect.Type":                         {"typ"},
	"net/netip.Addr":                       {"addr"},
	"net/netip.Prefix":                     {"prefix"},
	"net.IP":                               {"ip"},
	"hash.Hash":                            {"mac"},
	"bufio.Scanner":                        {"scanner"},
	"bufio.Reader":                         {"reader"},
	"bufio.Writer":                         {"writer"},
	"flag.FlagSet":                         {"flags"},
	"os/exec.Cmd":                          {"cmd"},
	"io/fs.FS":                             {"fsys"},
	"io/fs.DirEntry":                       {"entry"},
	"io/fs.FileInfo":                       {"info"},
	"os.Signal":                            {"sig"},
	"text/tabwriter.Writer":                {"table"},
	"github.com/a-h/templ.Component":       {"component"},
	"image.Image":                          {"img"},
	"image/color.Color":                    {"color"},
	"strings.Reader":                       {"reader"},
	"crypto/cipher.AEAD":                   {"aead"},
	"crypto/cipher.Block":                  {"block"},
}

// byLetter names basic values by their old letter, the most common meaning.
var byLetter = map[string]map[string][]string{
	"int": {
		"i": {"index", "position"}, "j": {"inner", "other"}, "k": {"index", "position"}, "n": {"count", "total"},
		"m": {"count"}, "l": {"length"}, "s": {"size"}, "c": {"count"}, "d": {"digit"}, "b": {"value"},
		"x": {"value"}, "v": {"value"}, "w": {"width"}, "h": {"height"}, "o": {"offset"}, "p": {"pos"},
		"t": {"total"}, "y": {"row"}, "e": {"exponent"}, "a": {"value"},
	},
	"string": {
		"s": {"text", "value"}, "k": {"key"}, "v": {"value"}, "n": {"name"}, "p": {"path"}, "e": {"email"},
		"u": {"raw"}, "c": {"code"}, "t": {"token"}, "q": {"query"}, "r": {"raw"}, "f": {"field"},
		"l": {"line"}, "h": {"header"}, "m": {"message"}, "a": {"addr"}, "d": {"dir"}, "g": {"group"},
		"x": {"text"}, "o": {"origin"}, "w": {"word"}, "b": {"text"}, "i": {"item"}, "y": {"text"},
	},
	"rune":  {"r": {"char"}, "c": {"char"}, "b": {"char"}},
	"byte":  {"b": {"char"}, "c": {"char"}, "r": {"char"}},
	"bool":  {"b": {"flag"}, "o": {"ok"}, "v": {"flag"}, "s": {"set"}, "e": {"enabled"}},
	"float": {"f": {"value"}, "x": {"value"}, "v": {"value"}, "d": {"delta"}},
}

// derive lists names for decl, best first.
func derive(decl *declaration) []string {
	var object types.Object
	for _, candidate := range decl.objects {
		if candidate != nil {
			object = candidate
			break
		}
	}
	var names []string
	if object == nil {
		// type switch symbol: the switched value
		return []string{"value", "typed"}
	}
	if _, ok := object.(*types.TypeName); ok {
		if _, ok := object.Type().(*types.TypeParam); ok {
			return []string{"Value", "Item", "Elem"}
		}
		return nil
	}
	if _, ok := object.(*types.Func); ok {
		return nil // a function or method: name it in -overrides
	}
	if _, ok := object.(*types.Label); ok {
		return []string{"loop"}
	}
	typ := object.Type()
	if pair := comparatorName(object, decl.name); pair != "" {
		names = append(names, pair)
	}
	if decl.rangeOf != nil {
		names = append(names, rangeNames(decl, typ)...)
	}
	if isError(typ) {
		names = append(names, "err", "cause")
	}
	if _, named := types.Unalias(derefType(typ)).(*types.Named); !named && decl.callee != "" {
		names = append(names, calleeNames(decl.callee)...)
	}
	names = append(names, typeNames(typ, decl.name)...)
	if decl.receiver != nil {
		names = append(names, words(decl.receiver.Name())...)
		if strings.HasSuffix(decl.receiver.Name(), "Error") {
			names = append(names, "err")
		}
	}
	for index, name := range names {
		if long, ok := abbreviations[name]; ok {
			names[index] = long
		}
	}
	return dedupe(names)
}

// abbreviations spells out the short forms a plural or type name can yield.
var abbreviations = map[string]string{
	"obj": "object", "cand": "candidate", "seg": "segment", "val": "value", "cfg": "config",
	"req": "request", "resp": "response", "msg": "message", "tok": "token", "sess": "session",
	"buf": "buffer", "addr": "address", "cmd": "command", "attr": "attribute", "typ": "kind",
}

// comparatorName is left or right for a and b (or x and y) declared side
// by side with the same type, as in a sort or compare func.
func comparatorName(object types.Object, letter string) string {
	pairs := map[string][2]string{"a": {"b", "left"}, "b": {"a", "right"}, "x": {"y", "left"}, "y": {"x", "right"}}
	pair, ok := pairs[letter]
	if !ok || object.Parent() == nil {
		return ""
	}
	partner := object.Parent().Lookup(pair[0])
	if partner == nil || !types.Identical(partner.Type(), object.Type()) {
		return ""
	}
	return pair[1]
}

func rangeNames(decl *declaration, typ types.Type) []string {
	ranged := ""
	switch expr := decl.rangeOf.(type) {
	case *ast.Ident:
		ranged = expr.Name
	case *ast.SelectorExpr:
		ranged = expr.Sel.Name
	case *ast.CallExpr:
		switch fun := expr.Fun.(type) {
		case *ast.Ident:
			ranged = fun.Name
		case *ast.SelectorExpr:
			ranged = fun.Sel.Name
		}
	}
	if decl.isKey {
		if basic, ok := typ.Underlying().(*types.Basic); ok && basic.Info()&types.IsInteger != 0 {
			return []string{"index", "position"}
		}
		return []string{"key", "name"}
	}
	var names []string
	if singular := singularOf(ranged); singular != "" {
		names = append(names, lowerFirst(singular))
	}
	if _, ok := typ.Underlying().(*types.Struct); ok && strings.HasSuffix(decl.file, "_test.go") {
		names = append(names, "testCase")
	}
	return names
}

// calleeNames names a value after the function that made it: jsonBody ->
// body, newHarness -> harness, scanUser -> user. Verbs alone name nothing.
func calleeNames(callee string) []string {
	parts := words(callee)
	if len(parts) == 0 {
		return nil
	}
	verbs := map[string]bool{"get": true, "new": true, "make": true, "load": true, "read": true, "parse": true, "find": true, "lookup": true, "build": true, "cut": true, "trim": true, "split": true, "join": true, "do": true, "run": true, "decode": true, "encode": true, "marshal": true, "unmarshal": true, "open": true, "atoi": true, "itoa": true, "sprintf": true, "len": true, "add": true, "of": true, "to": true, "from": true, "for": true, "with": true, "by": true}
	var names []string
	for _, part := range parts {
		if !verbs[strings.ToLower(part)] {
			names = append(names, part)
		}
	}
	return names
}

func singularOf(plural string) string {
	lower := strings.ToLower(plural)
	switch {
	case lower == "" || utf8.RuneCountInString(plural) < 3:
		return ""
	case strings.HasSuffix(lower, "cookies"), strings.HasSuffix(lower, "movies"):
		return plural[:len(plural)-1]
	case strings.HasSuffix(lower, "ies"):
		return plural[:len(plural)-3] + "y"
	case strings.HasSuffix(lower, "sses"), strings.HasSuffix(lower, "xes"), strings.HasSuffix(lower, "ches"), strings.HasSuffix(lower, "shes"):
		return plural[:len(plural)-2]
	case strings.HasSuffix(lower, "ss"), strings.HasSuffix(lower, "us"), strings.HasSuffix(lower, "is"):
		return "" // class, previous, status, analysis: not plurals
	case strings.HasSuffix(lower, "s"):
		return plural[:len(plural)-1]
	}
	return ""
}

func isError(typ types.Type) bool {
	return types.Identical(typ, types.Universe.Lookup("error").Type())
}

func typeNames(typ types.Type, letter string) []string {
	typ = types.Unalias(typ)
	if pointer, ok := typ.(*types.Pointer); ok {
		typ = types.Unalias(pointer.Elem())
	}
	switch concrete := typ.(type) {
	case *types.Named:
		object := concrete.Obj()
		if object.Pkg() != nil {
			if names, ok := knownTypes[object.Pkg().Path()+"."+object.Name()]; ok {
				return names
			}
		}
		if object.Pkg() == nil && object.Name() == "error" {
			return []string{"err"}
		}
		name := strings.TrimPrefix(object.Name(), "Mock")
		if basic, ok := concrete.Underlying().(*types.Basic); ok && !ast.IsExported(object.Name()) && len(words(name)) == 1 {
			return append(words(name), basicNames(basic, letter)...)
		}
		return words(name)
	case *types.Basic:
		return basicNames(concrete, letter)
	case *types.Slice:
		if elem, ok := concrete.Elem().Underlying().(*types.Basic); ok && elem.Kind() == types.Byte {
			return []string{"data", "buf"}
		}
		if names := typeNames(concrete.Elem(), letter); len(names) > 0 {
			return []string{plural(names[0]), "items"}
		}
		return []string{"items", "list"}
	case *types.Map:
		return []string{"lookup", "entries"}
	case *types.Signature:
		return []string{"fn", "callback"}
	case *types.Chan:
		return []string{"ch", "events"}
	case *types.Interface:
		return []string{"value"}
	case *types.Struct:
		return []string{"item", "entry"}
	case *types.TypeParam:
		return []string{"value", "item"}
	}
	return nil
}

func basicNames(basic *types.Basic, letter string) []string {
	info := basic.Info()
	switch {
	case basic.Kind() == types.Int32 && letter != "i" && letter != "n":
		return byLetter["rune"][letter]
	case basic.Kind() == types.Uint8 && letter != "i" && letter != "n":
		return byLetter["byte"][letter]
	case info&types.IsInteger != 0:
		return byLetter["int"][letter]
	case info&types.IsString != 0:
		return byLetter["string"][letter]
	case info&types.IsBoolean != 0:
		return byLetter["bool"][letter]
	case info&types.IsFloat != 0:
		return byLetter["float"][letter]
	}
	return nil
}

func plural(name string) string {
	switch {
	case strings.HasSuffix(name, "y") && !strings.HasSuffix(name, "ey"):
		return name[:len(name)-1] + "ies"
	case strings.HasSuffix(name, "s"), strings.HasSuffix(name, "x"), strings.HasSuffix(name, "ch"):
		return name + "es"
	}
	return name + "s"
}

// words splits a Go name into lower-camel words: "UserService" -> user, service
// joined back as needed ("userService"); initialisms stay together.
func words(name string) []string {
	var parts []string
	runes := []rune(name)
	start := 0
	for index := 1; index <= len(runes); index++ {
		boundary := index == len(runes) ||
			(unicode.IsUpper(runes[index]) && (unicode.IsLower(runes[index-1]) ||
				(index+1 < len(runes) && unicode.IsLower(runes[index+1]) && unicode.IsUpper(runes[index-1]))))
		if boundary {
			parts = append(parts, string(runes[start:index]))
			start = index
		}
	}
	for index, part := range parts {
		if index == 0 {
			parts[index] = lowerFirst(part)
		}
	}
	if len(parts) > 1 {
		last := lowerFirst(parts[len(parts)-1])
		joined := strings.Join(parts, "")
		return []string{last, joined}
	}
	return parts
}

func lowerFirst(word string) string {
	if word == "" {
		return word
	}
	if strings.ToUpper(word) == word {
		return strings.ToLower(word) // an initialism: ID -> id, URL -> url
	}
	first, size := utf8.DecodeRuneInString(word)
	return string(unicode.ToLower(first)) + word[size:]
}

func dedupe(names []string) []string {
	seen := map[string]bool{}
	var unique []string
	for _, name := range names {
		if name != "" && !seen[name] {
			seen[name] = true
			unique = append(unique, name)
		}
	}
	return unique
}

// edit replaces one identifier at a byte offset of a file.
type edit struct {
	offset  int
	oldName string
	newName string
}

// applyEdits rewrites the declarations and their uses. Positions in
// *_templ.go files are mapped to the .templ source and edited there.
func applyEdits(declarations []*declaration) error {
	edits := map[string]map[int]edit{}
	templMaps := map[string]*templOutput{}
	add := func(spot position, decl *declaration) error {
		file, line, column := spot.file, spot.line, spot.column
		if strings.HasSuffix(file, "_templ.go") {
			source := strings.TrimSuffix(file, "_templ.go") + ".templ"
			mapped, err := templPosition(file, line, column, source, templMaps)
			if err != nil {
				return fmt.Errorf("%s: no .templ position for %s: %w", spot, decl.name, err)
			}
			file, line, column = source, int(mapped.Line)+1, int(mapped.Col)+1
		}
		content, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		offset := lineOffset(content, line) + column - 1
		if offset >= len(content) || string(content[offset:offset+len(decl.name)]) != decl.name {
			return fmt.Errorf("%s:%d:%d: expected %q", file, line, column, decl.name)
		}
		if edits[file] == nil {
			edits[file] = map[int]edit{}
		}
		edits[file][offset] = edit{offset: offset, oldName: decl.name, newName: decl.newName}
		return nil
	}
	for _, decl := range declarations {
		if decl.newName == "" {
			continue
		}
		if err := add(position{decl.file, decl.line, decl.column}, decl); err != nil {
			return err
		}
		for _, use := range decl.uses {
			if err := add(use, decl); err != nil {
				return err
			}
		}
	}
	for file, byOffset := range edits {
		content, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		offsets := slices.Sorted(maps.Keys(byOffset))
		var out strings.Builder
		last := 0
		for _, offset := range offsets {
			change := byOffset[offset]
			out.Write(content[last:offset])
			out.WriteString(change.newName)
			last = offset + len(change.oldName)
		}
		out.Write(content[last:])
		info, err := os.Stat(file)
		if err != nil {
			return err
		}
		if err := os.WriteFile(file, []byte(out.String()), info.Mode()); err != nil {
			return err
		}
	}
	fmt.Printf("edited %d files\n", len(edits))
	return nil
}

// templOutput is what templ generates for a .templ file before gofmt runs
// over it, and its source map (which points into that unformatted text).
type templOutput struct {
	raw       []byte
	sourceMap *parser.SourceMap
}

// templPosition maps a position in a committed *_templ.go file to the
// .templ source: gofmt changes only whitespace, so the nth token of the
// committed file is the nth token of the unformatted output, whose position
// the source map knows.
func templPosition(generated string, line, column int, source string, cache map[string]*templOutput) (parser.Position, error) {
	output, ok := cache[source]
	if !ok {
		template, err := parser.Parse(source)
		if err != nil {
			return parser.Position{}, err
		}
		var raw strings.Builder
		result, err := generator.Generate(template, &raw, generator.WithVersion(templ.Version()))
		if err != nil {
			return parser.Position{}, err
		}
		output = &templOutput{raw: []byte(raw.String()), sourceMap: result.SourceMap}
		cache[source] = output
	}
	formatted, err := os.ReadFile(generated)
	if err != nil {
		return parser.Position{}, err
	}
	offset := lineOffset(formatted, line) + column - 1
	formattedTokens := tokenOffsets(formatted)
	rawTokens := tokenOffsets(output.raw)
	index, found := slices.BinarySearch(formattedTokens, offset)
	if !found || len(formattedTokens) != len(rawTokens) {
		return parser.Position{}, fmt.Errorf("token streams differ (%d vs %d tokens)", len(formattedTokens), len(rawTokens))
	}
	rawOffset := rawTokens[index]
	rawLine := strings.Count(string(output.raw[:rawOffset]), "\n")
	rawColumn := rawOffset - (strings.LastIndexByte(string(output.raw[:rawOffset]), '\n') + 1)
	mapped, ok := output.sourceMap.SourcePositionFromTarget(uint32(rawLine), uint32(rawColumn))
	if !ok {
		return parser.Position{}, fmt.Errorf("not in the source map")
	}
	return mapped, nil
}

// tokenOffsets lists the byte offset of every token except semicolons
// (which the scanner inserts at line ends, so gofmt can change them).
func tokenOffsets(content []byte) []int {
	var offsets []int
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(content))
	var tokens scanner.Scanner
	tokens.Init(file, content, nil, scanner.ScanComments)
	for {
		pos, kind, _ := tokens.Scan()
		if kind == token.EOF {
			return offsets
		}
		if kind != token.SEMICOLON {
			offsets = append(offsets, file.Offset(pos))
		}
	}
}

func lineOffset(content []byte, line int) int {
	offset := 0
	for current := 1; current < line; current++ {
		next := strings.IndexByte(string(content[offset:]), '\n')
		if next < 0 {
			return len(content)
		}
		offset += next + 1
	}
	return offset
}

// Fields of an overrides line: a position and a name, or a file, an old
// name and a new one.
const (
	positionFields = 2
	fileFields     = 3
)

func readOverrides(name string) map[string]string {
	overrides := map[string]string{}
	if name == "" {
		return overrides
	}
	file, err := os.Open(name)
	check(err)
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		switch len(fields) {
		case positionFields: // file:line:col name
			overrides[fields[0]] = fields[1]
		case fileFields: // file old name
			overrides[fields[0]+" "+fields[1]] = fields[2]
		default:
			fail("bad overrides line: " + line)
		}
	}
	check(scanner.Err())
	return overrides
}

func hasAnyPrefix(file string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(file, strings.TrimPrefix(prefix, "./")) {
			return true
		}
	}
	return false
}

func derefType(typ types.Type) types.Type {
	if pointer, ok := typ.(*types.Pointer); ok {
		return pointer.Elem()
	}
	return typ
}

func check(err error) {
	if err != nil {
		fail(err.Error())
	}
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, "rename:", message)
	os.Exit(1)
}
