package repository

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// sqlPlaceholderPattern matches positional placeholders such as $1 or $10.
var sqlPlaceholderPattern = regexp.MustCompile(`\$([0-9]+)`)

// sqlCallSignatures lists the database/sql style helpers. The query text is the
// first argument, except for the *Context variants where ctx comes first.
var sqlCallNames = map[string]bool{
	"Exec":            true,
	"Query":           true,
	"QueryRow":        true,
	"ExecContext":     true,
	"QueryContext":    true,
	"QueryRowContext": true,
}

// sqlText mirrors a query expression: the constant fragments we could prove, plus
// whether the whole expression was provable. When complete is false some operand
// (a variable, a helper call, a shared snippet) may hide additional placeholders,
// so only the "too few arguments" direction can be asserted.
type sqlText struct {
	parts    []string
	complete bool
}

// TestSQLPlaceholdersMatchArgumentCount is a static guard against a class of bug
// that mock-style tests cannot see: passing more arguments than the query
// references. PostgreSQL rejects the *entire* statement with
// "could not determine data type of parameter $N" when a parameter is never
// referenced, because it has no type context to infer from — and sqlmock happily
// accepts whatever argument list the test author wrote, so a shape test can
// encode the broken numbering and stay green.
//
// 2026-11-13: updateUpstreamBillingProbeSnapshotInTx passed 11 arguments while
// the SQL referenced only 10 of them (the leftover `string(credentials)` after
// credentials_hash replaced the raw credentials column). The upstream billing
// probe therefore failed on every run since it shipped, logging
// `probe_due_failed: ... could not determine data type of parameter $5`.
//
// The guard walks the AST rather than grepping. Queries assembled partly from
// variables or helper calls are only checked in the direction that stays sound
// without resolving them.
func TestSQLPlaceholdersMatchArgumentCount(t *testing.T) {
	type violation struct {
		file    string
		line    int
		max     int
		args    int
		missing []int
	}
	var violations []violation
	strictQueries := 0
	partialQueries := 0

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read repository package dir: %v", err)
	}

	// Snippet constants are routinely declared in a sibling file of the same
	// package (e.g. ollamaCloudUsageEligibleSQL lives next to, not inside, the
	// query that concatenates it). Collect them package-wide first, otherwise
	// those queries look "partially assembled" and escape the strict check.
	packageConsts := make(map[string]string)
	parsed := make(map[string]*ast.File)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed[name] = file
	}
	// Iterate to a fixed point: snippets are built from other snippets.
	for pass := 0; pass < 5; pass++ {
		grew := false
		for _, file := range parsed {
			for constName, value := range packageStringConsts(file, packageConsts) {
				if _, seen := packageConsts[constName]; !seen {
					packageConsts[constName] = value
					grew = true
				}
			}
		}
		if !grew {
			break
		}
	}

	for _, entry := range entries {
		name := entry.Name()
		file, ok := parsed[name]
		if !ok {
			continue
		}

		fset := token.NewFileSet()

		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !sqlCallNames[sel.Sel.Name] {
				return true
			}

			queryIdx := 0
			if strings.HasSuffix(sel.Sel.Name, "Context") {
				queryIdx = 1
			}
			if len(call.Args) <= queryIdx {
				return true
			}

			// A trailing `args...` makes the argument count unknowable here.
			if call.Ellipsis.IsValid() {
				return true
			}

			text := resolveSQLText(call.Args[queryIdx], packageConsts)
			query := strings.Join(text.parts, "")
			matches := sqlPlaceholderPattern.FindAllStringSubmatch(query, -1)
			if len(matches) == 0 {
				return true
			}

			seen := make(map[int]bool, len(matches))
			maxIndex := 0
			for _, match := range matches {
				index, err := strconv.Atoi(match[1])
				if err != nil || index <= 0 {
					continue
				}
				seen[index] = true
				if index > maxIndex {
					maxIndex = index
				}
			}
			if maxIndex == 0 {
				return true
			}

			args := len(call.Args) - queryIdx - 1
			var missing []int
			for index := 1; index <= maxIndex; index++ {
				if !seen[index] {
					missing = append(missing, index)
				}
			}

			if !text.complete {
				partialQueries++
				// Hidden fragments may hold higher indices, so only a shortfall
				// against the visible ones is provable.
				if args < maxIndex {
					violations = append(violations, violation{
						file: name, line: fset.Position(call.Pos()).Line,
						max: maxIndex, args: args, missing: missing,
					})
				}
				return true
			}

			strictQueries++
			if args != maxIndex || len(missing) > 0 {
				violations = append(violations, violation{
					file: name, line: fset.Position(call.Pos()).Line,
					max: maxIndex, args: args, missing: missing,
				})
			}
			return true
		})
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].file != violations[j].file {
			return violations[i].file < violations[j].file
		}
		return violations[i].line < violations[j].line
	})

	for _, v := range violations {
		switch {
		case v.args > v.max && len(v.missing) > 0:
			t.Errorf("%s:%d: query references $%d and skips %v, but %d argument(s) were passed — PostgreSQL rejects an unreferenced parameter with \"could not determine data type\"; remove the extra argument or use the placeholder",
				v.file, v.line, v.max, v.missing, v.args)
		case v.args > v.max:
			t.Errorf("%s:%d: query references up to $%d but %d argument(s) were passed — the extra parameter(s) are unreferenced",
				v.file, v.line, v.max, v.args)
		case v.args < v.max:
			t.Errorf("%s:%d: query references up to $%d but only %d argument(s) were passed",
				v.file, v.line, v.max, v.args)
		default:
			t.Errorf("%s:%d: query references up to $%d but never uses %v — PostgreSQL cannot infer their type",
				v.file, v.line, v.max, v.missing)
		}
	}

	t.Logf("checked %d fully-literal queries strictly, %d partially-assembled queries in the sound direction only",
		strictQueries, partialQueries)
}

// resolveSQLText flattens a query expression into its constant string fragments.
// complete is true only when every operand was provable, which is the sole case
// where the argument count can be compared exactly.
func resolveSQLText(expr ast.Expr, consts map[string]string) sqlText {
	switch typed := expr.(type) {
	case *ast.BasicLit:
		if typed.Kind != token.STRING {
			return sqlText{}
		}
		value, err := strconv.Unquote(typed.Value)
		if err != nil {
			return sqlText{}
		}
		return sqlText{parts: []string{value}, complete: true}
	case *ast.BinaryExpr:
		if typed.Op != token.ADD {
			return sqlText{}
		}
		left := resolveSQLText(typed.X, consts)
		right := resolveSQLText(typed.Y, consts)
		return sqlText{
			parts:    append(left.parts, right.parts...),
			complete: left.complete && right.complete,
		}
	case *ast.ParenExpr:
		return resolveSQLText(typed.X, consts)
	case *ast.Ident:
		if value, ok := consts[typed.Name]; ok {
			return sqlText{parts: []string{value}, complete: true}
		}
		return sqlText{}
	default:
		return sqlText{}
	}
}

// packageStringConsts collects package-level const values that are fully
// resolvable given the consts learned so far, so that queries stitched from
// shared snippets stay provable. Snippets are routinely built from other
// snippets, hence the caller iterates this to a fixed point.
func packageStringConsts(file *ast.File, known map[string]string) map[string]string {
	consts := make(map[string]string)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != len(value.Values) {
				continue
			}
			for i, name := range value.Names {
				text := resolveSQLText(value.Values[i], known)
				if text.complete && len(text.parts) > 0 {
					consts[name.Name] = strings.Join(text.parts, "")
				}
			}
		}
	}
	return consts
}

// guard against the guard silently rotting: it must find the package files at all.
func TestSQLPlaceholderGuardSeesRepositorySources(t *testing.T) {
	matches, err := filepath.Glob("account_repo.go")
	if err != nil || len(matches) == 0 {
		t.Fatalf("guard is running outside the repository package dir: %v %v", matches, err)
	}
}
