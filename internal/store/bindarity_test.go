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

var bindMethods = map[string]bool{
	"Query": true, "QueryContext": true, "QueryRow": true, "QueryRowContext": true,
	"Exec": true, "ExecContext": true, "Prepare": true, "PrepareContext": true,
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

func collectStringFuncs(pkg *ast.Package) map[string]*stringFunc {
	funcs := map[string]*stringFunc{}
	for _, file := range pkg.Files {
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

func collectStringConsts(pkg *ast.Package, base *sqlResolver) map[string]string {
	consts := map[string]string{}
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
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
					if resolved, ok := base.resolve(value.Values[i]); ok {
						consts[name.Name] = resolved
					}
				}
			}
		}
	}
	return consts
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

func TestEverySQLStatementBindsAsManyArgumentsAsItHasPlaceholders(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	var total, checked, dynamic, variadic int
	histogram := map[int]int{}

	for _, pkg := range pkgs {
		funcs := collectStringFuncs(pkg)
		consts := collectStringConsts(pkg, &sqlResolver{scope: map[string]string{}, funcs: funcs})

		for _, file := range pkg.Files {
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
	t.Logf("placeholders per checked statement: %s", strings.TrimSpace(shape.String()))
}
