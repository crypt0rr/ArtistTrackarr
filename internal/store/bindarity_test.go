package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This package is where every SQL statement in the service lives, and the
// driver underneath it is unforgiving in one direction only: modernc.org/sqlite
// errors when a statement is given too FEW bind arguments and silently ignores
// surplus ones. A statement carrying one argument too many therefore executes,
// returns rows, and quietly answers a different question than the one written -
// which is exactly how #239 shipped, and why it took a review round to find.
//
// Placeholders are positional, so the count is a property a parser can check
// and a reviewer cannot. Measured on this package: 59 statements carry four or
// more placeholders and one carries 21.

// The database/sql entry points, plus this package's own write wrapper.
//
// execWriteContext matters more than the rest: it is the house-preferred write
// path, with 31 call sites, and leaving it out meant this test asserted a
// package-wide invariant while skipping the statements the codebase is
// actively being steered towards. A guard that covers some of the sites and
// claims all of them is the exact defect shape this file exists to catch, so
// it is worth naming here - any future wrapper of the same shape belongs in
// this map on the day it is written.
//
// Prepare and PrepareContext are deliberately absent. They take the SQL and no
// bind arguments at all, so a prepared statement with placeholders would be
// reported as a mismatch on completely correct code. Their arguments are bound
// later through the returned *sql.Stmt, which this walker does not follow.
var bindMethods = map[string]bool{
	"Query": true, "QueryContext": true, "QueryRow": true, "QueryRowContext": true,
	"Exec": true, "ExecContext": true,
	"execWriteContext": true,
}

// stringFunc is a helper of the shape `func(a, b string) string { return ... }`.
// Resolving these matters: followedReleasePredicate is interpolated into the
// statement that carried #239, and without following it that call site is
// invisible to this test.
type stringFunc struct {
	params []string
	body   ast.Expr
}

type sqlResolver struct {
	scope map[string]string
	funcs map[string]*stringFunc
}

// resolve folds an expression down to a constant string, or reports false when
// it depends on something only known at runtime.
func (r *sqlResolver) resolve(expr ast.Expr) (string, bool) {
	switch v := expr.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		unquoted, err := strconv.Unquote(v.Value)
		return unquoted, err == nil
	case *ast.ParenExpr:
		return r.resolve(v.X)
	case *ast.Ident:
		value, ok := r.scope[v.Name]
		return value, ok
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		left, leftOK := r.resolve(v.X)
		right, rightOK := r.resolve(v.Y)
		return left + right, leftOK && rightOK
	case *ast.CallExpr:
		name, ok := v.Fun.(*ast.Ident)
		if !ok {
			return "", false
		}
		fn, ok := r.funcs[name.Name]
		if !ok || len(fn.params) != len(v.Args) {
			return "", false
		}
		inner := &sqlResolver{scope: map[string]string{}, funcs: r.funcs}
		for i, param := range fn.params {
			value, ok := r.resolve(v.Args[i])
			if !ok {
				return "", false
			}
			inner.scope[param] = value
		}
		return inner.resolve(fn.body)
	}
	return "", false
}

// countPlaceholders counts bind markers, skipping the ones inside SQL string
// literals and comments. A '?' in a comment is not a placeholder, and counting
// it would produce a false mismatch on a statement that is perfectly correct.
func countPlaceholders(sql string) int {
	count := 0
	var inSingle, inDouble, inLineComment, inBlockComment bool
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		next := byte(0)
		if i+1 < len(sql) {
			next = sql[i+1]
		}
		switch {
		case inLineComment:
			if c == '\n' {
				inLineComment = false
			}
		case inBlockComment:
			if c == '*' && next == '/' {
				inBlockComment, i = false, i+1
			}
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
		case inDouble:
			if c == '"' {
				inDouble = false
			}
		case c == '-' && next == '-':
			inLineComment, i = true, i+1
		case c == '/' && next == '*':
			inBlockComment, i = true, i+1
		case c == '\'':
			inSingle = true
		case c == '"':
			inDouble = true
		case c == '?':
			count++
		}
	}
	return count
}

func collectStringFuncs(files []*ast.File) map[string]*stringFunc {
	funcs := map[string]*stringFunc{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil || len(fn.Body.List) != 1 {
				continue
			}
			ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 {
				continue
			}
			if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
				continue
			}
			if id, ok := fn.Type.Results.List[0].Type.(*ast.Ident); !ok || id.Name != "string" {
				continue
			}
			helper := &stringFunc{body: ret.Results[0]}
			allStrings := true
			for _, param := range fn.Type.Params.List {
				if id, ok := param.Type.(*ast.Ident); !ok || id.Name != "string" {
					allStrings = false
					break
				}
				for _, name := range param.Names {
					helper.params = append(helper.params, name.Name)
				}
			}
			if allStrings {
				funcs[fn.Name.Name] = helper
			}
		}
	}
	return funcs
}

// collectStringDeclarations folds package-level string declarations.
//
// Both const and var are collected: this package already keeps SQL in vars
// (evidenceIssueSelect, releaseInboxFrom), and skipping them would mean a
// one-token difference between `const` and `var` silently decided whether a
// statement was checked at all.
//
// It iterates to a fixed point because a declaration may be built from another
// one, and Go does not require them to appear in dependency order. Resolving
// in a single pass with an empty scope quietly dropped every such statement -
// including the ones interpolating followedReleasePredicate, which is where
// #239 lived.
func collectStringDeclarations(files []*ast.File, funcs map[string]*stringFunc) map[string]string {
	declared := map[string]string{}
	for {
		found := 0
		r := &sqlResolver{scope: declared, funcs: funcs}
		for _, file := range files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
					continue
				}
				for _, spec := range gen.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range value.Names {
						if i >= len(value.Values) {
							continue
						}
						if _, already := declared[name.Name]; already {
							continue
						}
						if resolved, ok := r.resolve(value.Values[i]); ok {
							declared[name.Name] = resolved
							found++
						}
					}
				}
			}
		}
		if found == 0 {
			return declared
		}
	}
}

// localStringVars folds function-local `x := "..."` assignments, dropping any
// name that is ever assigned something this resolver cannot fold. Keeping a
// stale value for a reassigned name would be worse than skipping the site.
func localStringVars(body *ast.BlockStmt, r *sqlResolver) {
	unresolvable := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != len(assign.Rhs) {
			return true
		}
		for i := range assign.Lhs {
			name, ok := assign.Lhs[i].(*ast.Ident)
			if !ok {
				continue
			}
			resolved, ok := r.resolve(assign.Rhs[i])
			if !ok {
				unresolvable[name.Name] = true
				continue
			}
			if previous, seen := r.scope[name.Name]; seen && previous != resolved {
				unresolvable[name.Name] = true
				continue
			}
			r.scope[name.Name] = resolved
		}
		return true
	})
	for name := range unresolvable {
		delete(r.scope, name)
	}
}

// executesSQL reports whether fn has the shape of a statement-executing
// helper: a string parameter followed by a final variadic one, which is how
// database/sql spells it and how this package's own wrapper does too.
func executesSQL(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil || len(fn.Type.Params.List) < 2 {
		return false
	}
	params := fn.Type.Params.List
	last := params[len(params)-1]
	ellipsis, ok := last.Type.(*ast.Ellipsis)
	if !ok {
		return false
	}
	// `...any` parses as an Ident, `...interface{}` as an InterfaceType. Match
	// both: accepting only the latter made this check silently vacuous, since
	// the wrapper it was written for is spelled `args ...any`.
	switch elt := ellipsis.Elt.(type) {
	case *ast.Ident:
		if elt.Name != "any" {
			return false
		}
	case *ast.InterfaceType:
		if elt.Methods != nil && len(elt.Methods.List) != 0 {
			return false
		}
	default:
		return false
	}
	// The parameter before the variadic one carries the statement.
	previous := params[len(params)-2]
	id, ok := previous.Type.(*ast.Ident)
	return ok && id.Name == "string"
}

func TestEverySQLStatementBindsAsManyArgumentsAsItHasPlaceholders(t *testing.T) {
	fset := token.NewFileSet()
	// Walk the directory rather than using parser.ParseDir, which is
	// deprecated, and go/packages, which would add a dependency to a test
	// whose whole point is to need no tooling.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, parsed)
	}
	if len(files) == 0 {
		t.Fatal("no non-test Go files found; the walker has stopped working")
	}

	var total, checked, dynamic, variadic int
	histogram := map[int]int{}
	seenMethod := map[string]int{}

	{
		funcs := collectStringFuncs(files)
		consts := collectStringDeclarations(files, funcs)

		for _, file := range files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				scope := make(map[string]string, len(consts))
				for name, value := range consts {
					scope[name] = value
				}
				r := &sqlResolver{scope: scope, funcs: funcs}
				localStringVars(fn.Body, r)

				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || !bindMethods[sel.Sel.Name] {
						return true
					}
					// Context variants take ctx first; the SQL is the next arg.
					sqlIndex := 0
					if strings.HasSuffix(sel.Sel.Name, "Context") {
						sqlIndex = 1
					}
					if sqlIndex >= len(call.Args) {
						return true
					}
					total++
					seenMethod[sel.Sel.Name]++
					if call.Ellipsis.IsValid() {
						variadic++
						return true
					}
					sql, static := r.resolve(call.Args[sqlIndex])
					if !static {
						dynamic++
						return true
					}
					checked++
					placeholders := countPlaceholders(sql)
					histogram[placeholders]++
					if args := len(call.Args) - sqlIndex - 1; placeholders != args {
						t.Errorf("%s: %s has %d placeholder(s) but is given %d argument(s)\n\t%.200s",
							fset.Position(call.Pos()), sel.Sel.Name, placeholders, args,
							strings.Join(strings.Fields(sql), " "))
					}
					return true
				})
			}
		}
	}

	if checked == 0 {
		t.Fatal("no SQL call sites were resolved; the walker has stopped working")
	}

	// Any wrapper this package defines that takes SQL and a variadic argument
	// list must be listed in bindMethods.
	//
	// This is the assertion that was missing. execWriteContext - the
	// house-preferred write path, 31 call sites - was absent from the map, so
	// a test named "every SQL statement" skipped every statement written the
	// way the codebase is being steered towards, and nothing said so. Listing a
	// method that has no call sites yet is fine and deliberate (Query and
	// QueryRow are there for the day someone uses them); defining one and
	// forgetting to list it is the failure that matters.
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !executesSQL(fn) || bindMethods[fn.Name.Name] {
				continue
			}
			t.Errorf("%s: %s takes SQL and a variadic argument list but is not in bindMethods, so its call sites go unchecked",
				fset.Position(fn.Pos()), fn.Name.Name)
		}
	}
	// Report the blind spot rather than leaving it implicit. If a refactor
	// pushes many sites into the dynamic or variadic buckets, this test
	// quietly stops covering them, and the numbers are how that becomes
	// visible.
	keys := make([]int, 0, len(histogram))
	for k := range histogram {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	var shape strings.Builder
	for _, k := range keys {
		shape.WriteString(strconv.Itoa(k) + ":" + strconv.Itoa(histogram[k]) + " ")
	}
	t.Logf("db call sites: %d total, %d checked, %d dynamic SQL, %d variadic spread", total, checked, dynamic, variadic)
	for method := range bindMethods {
		t.Logf("  %-18s %d call sites", method, seenMethod[method])
	}
	t.Logf("placeholders per checked statement: %s", strings.TrimSpace(shape.String()))
}
