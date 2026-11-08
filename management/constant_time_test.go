package management

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// No test can see a comparison's timing, so the constant-time property is
// held by review and by this test, which reads the package's own source:
//
//   - subtle.ConstantTimeCompare is called exactly once, in Secrets.Accepts;
//   - Accepts's loop over the secrets never returns, breaks or jumps early,
//     and Accepts compares nothing but its match count with ==;
//   - nothing in the package calls an equality function (bytes.Equal,
//     hmac.Equal, reflect.DeepEqual, ...), and no ==, !=, <, >, switch, map
//     index or strings.HasPrefix-style call takes a value whose name says it
//     holds a token or a secret, except to ask whether it is empty or how
//     long it is;
//   - the digests are touched only in secrets.go.

// secretBearing matches the names of values that hold a presented token or
// a secret (or its digest).
var secretBearing = regexp.MustCompile(`(?i)token|secret|digest|presented|current|previous|accepted`)

// equalityFunctions compare their arguments, in variable time or not; none
// belongs in this package.
var equalityFunctions = map[string]bool{
	"bytes.Equal": true, "bytes.Compare": true, "bytes.EqualFold": true,
	"strings.Compare": true, "hmac.Equal": true, "reflect.DeepEqual": true,
	"slices.Equal": true, "slices.Compare": true, "maps.Equal": true,
	"subtle.ConstantTimeEq": true, "subtle.ConstantTimeByteEq": true,
}

// prefixFunctions compare part of a value: allowed, but never on a secret.
var prefixFunctions = map[string]bool{
	"strings.EqualFold": true, "strings.HasPrefix": true, "strings.HasSuffix": true,
	"strings.Contains": true, "strings.Index": true, "strings.LastIndex": true,
	"bytes.HasPrefix": true, "bytes.HasSuffix": true, "bytes.Contains": true, "bytes.Index": true,
}

type sourceFile struct {
	name string
	file *ast.File
}

func packageSource(t *testing.T, dir string) (*token.FileSet, []sourceFile) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []sourceFile
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, sourceFile{name: name, file: file})
	}
	if len(files) == 0 {
		t.Fatalf("no Go source in %s", dir)
	}
	return fset, files
}

// selectorName is "pkg.Func" for a call like pkg.Func(...), else "".
func selectorName(call *ast.CallExpr) string {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return pkg.Name + "." + selector.Sel.Name
}

// collectConstants names every constant the files declare (MinSecretBytes,
// SecretVariable, ...): their names say "secret", but they hold none.
func collectConstants(files []sourceFile) map[string]bool {
	constants := map[string]bool{}
	for _, source := range files {
		for _, decl := range source.file.Decls {
			general, ok := decl.(*ast.GenDecl)
			if !ok || general.Tok != token.CONST {
				continue
			}
			for _, spec := range general.Specs {
				for _, name := range spec.(*ast.ValueSpec).Names {
					constants[name.Name] = true
				}
			}
		}
	}
	return constants
}

// mentionsSecret reports whether expr names a secret-bearing value outside
// a len(...) call (a secret's length is not its value).
func mentionsSecret(expr ast.Node, constants map[string]bool) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		switch n := n.(type) {
		case *ast.CallExpr:
			if fun, ok := n.Fun.(*ast.Ident); ok && fun.Name == "len" {
				return false
			}
		case *ast.Ident:
			if secretBearing.MatchString(n.Name) && !constants[n.Name] {
				found = true
			}
		}
		return true
	})
	return found
}

// isEmptyOrNil reports whether expr is "" or nil: comparing with either
// says only whether a value is there.
func isEmptyOrNil(expr ast.Expr) bool {
	if literal, ok := expr.(*ast.BasicLit); ok {
		return literal.Value == `""`
	}
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == "nil"
}

// constantTimeViolations lists every breach of the rules above in the Go
// source in dir. It is a function of the source alone, so a scratch copy
// with a deliberate break shows it failing.
func constantTimeViolations(t *testing.T, dir string) []string {
	fset, files := packageSource(t, dir)
	constants := collectConstants(files)
	var violations []string
	report := func(node ast.Node, format string) {
		violations = append(violations, fset.Position(node.Pos()).String()+": "+format)
	}

	compares := 0
	for _, source := range files {
		ast.Inspect(source.file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Name.Name == "Accepts" && n.Recv != nil {
					violations = append(violations, acceptsViolations(fset, n)...)
				}
			case *ast.CallExpr:
				name := selectorName(n)
				switch {
				case name == "subtle.ConstantTimeCompare":
					compares++
				case equalityFunctions[name]:
					report(n, name+" is called; only subtle.ConstantTimeCompare may compare secrets")
				case prefixFunctions[name]:
					for _, argument := range n.Args {
						if mentionsSecret(argument, constants) {
							report(n, name+" is called on a secret or a token")
						}
					}
				}
			case *ast.BinaryExpr:
				switch n.Op {
				case token.EQL, token.NEQ, token.LSS, token.GTR, token.LEQ, token.GEQ:
					if isEmptyOrNil(n.X) || isEmptyOrNil(n.Y) {
						return true
					}
					if mentionsSecret(n.X, constants) || mentionsSecret(n.Y, constants) {
						report(n, "a secret or a token is compared with "+n.Op.String())
					}
				}
			case *ast.SwitchStmt:
				if n.Tag != nil && mentionsSecret(n.Tag, constants) {
					report(n, "a switch on a secret or a token")
				}
			case *ast.IndexExpr:
				if mentionsSecret(n.Index, constants) {
					report(n, "a secret or a token is used as an index or a map key")
				}
			case *ast.SelectorExpr:
				if n.Sel.Name == "digests" && source.name != "secrets.go" {
					report(n, "the digests are read outside secrets.go")
				}
			}
			return true
		})
	}
	if compares != 1 {
		violations = append(violations, fmt.Sprintf(
			"subtle.ConstantTimeCompare is called %d times, want exactly once, in Secrets.Accepts", compares))
	}
	return violations
}

// acceptsViolations checks Secrets.Accepts itself: ConstantTimeCompare is
// called inside its loop, the loop never leaves early, and == compares only
// with a number.
func acceptsViolations(fset *token.FileSet, accepts *ast.FuncDecl) []string {
	var violations []string
	report := func(node ast.Node, message string) {
		violations = append(violations, fset.Position(node.Pos()).String()+": "+message)
	}
	loops, comparesInLoop := 0, 0
	ast.Inspect(accepts.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.RangeStmt, *ast.ForStmt:
			loops++
			ast.Inspect(n, func(inner ast.Node) bool {
				switch inner := inner.(type) {
				case *ast.ReturnStmt:
					report(inner, "Accepts returns from inside its loop")
				case *ast.BranchStmt:
					if inner.Tok != token.CONTINUE || inner.Label != nil {
						report(inner, "Accepts leaves its loop early ("+inner.Tok.String()+")")
					}
				case *ast.CallExpr:
					if selectorName(inner) == "subtle.ConstantTimeCompare" {
						comparesInLoop++
					}
				}
				return true
			})
			return false
		case *ast.BinaryExpr:
			if n.Op == token.EQL || n.Op == token.NEQ {
				_, xNumber := n.X.(*ast.BasicLit)
				_, yNumber := n.Y.(*ast.BasicLit)
				if !xNumber && !yNumber {
					report(n, "Accepts compares with "+n.Op.String()+" something other than a number")
				}
			}
		}
		return true
	})
	if loops != 1 || comparesInLoop != 1 {
		report(accepts, "Accepts must compare with subtle.ConstantTimeCompare inside one loop over every secret")
	}
	return violations
}

func TestSubtleConstantTimeCompareIsTheOnlySecretComparison(t *testing.T) {
	if violations := constantTimeViolations(t, "."); len(violations) > 0 {
		t.Fatalf("the secret comparison isn't constant-time only:\n%s", strings.Join(violations, "\n"))
	}
}

// The scan sees the breaks it exists for, written into a scratch package.
func TestTheConstantTimeScanSeesAnEarlyReturnAndAPlainComparison(t *testing.T) {
	cases := map[string]string{
		"== with an early return": `package management
func (s *Secrets) Accepts(token string) bool {
	for _, accepted := range s.digests {
		if token == string(accepted[:]) {
			return true
		}
	}
	return false
}`,
		"ConstantTimeCompare with an early return": `package management
import "crypto/subtle"
func (s *Secrets) Accepts(token string) bool {
	presented := []byte(token)
	for i := range s.digests {
		if subtle.ConstantTimeCompare(presented, s.digests[i][:]) == 1 {
			return true
		}
	}
	return false
}`,
		"a second comparison in the middleware": `package management
import "crypto/subtle"
func (s *Secrets) Accepts(token string) bool {
	matched := 0
	for i := range s.digests {
		matched |= subtle.ConstantTimeCompare([]byte(token), s.digests[i][:])
	}
	return matched == 1
}
func shortcut(token, secret string) bool { return strings.HasPrefix(secret, token) || token == secret }`,
		"hmac.Equal": `package management
import ("crypto/hmac"; "crypto/subtle")
func (s *Secrets) Accepts(token string) bool {
	matched := 0
	for i := range s.digests {
		matched |= subtle.ConstantTimeCompare([]byte(token), s.digests[i][:])
	}
	return matched == 1 && hmac.Equal([]byte(token), s.digests[0][:])
}`,
	}
	for label, source := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "secrets.go"), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		if violations := constantTimeViolations(t, dir); len(violations) == 0 {
			t.Errorf("%s: the scan saw nothing wrong", label)
		}
	}
}
