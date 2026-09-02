package client

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeMethods are the session methods that put bytes on the connection.
var writeMethods = map[string]bool{"write": true, "writeWithDeadline": true}

// TestUnit_EveryWriteSiteClassifiesAuthRejection is a source-level guard, and
// it exists because this bug has now shipped twice by the same mechanism.
//
// A write failure returns before either read-path rejection detector runs, so
// EVERY site that writes to the connection and HANDLES the resulting error has
// to route it through classifyWriteErr, or a rejected token gets retried. #238
// covered two sites and left the SyncStep2 reply uncovered, which kept
// TestClient_Auth_WrongTokenIsTerminal flaking; the awareness reply and
// flushLane's own write were uncovered too, and the last of those was found by
// an earlier revision of THIS test rather than by review.
//
// Behavioural tests can only cover the sites someone thought to write a test
// for — which is exactly how the gap survived. Counting the sites catches a NEW
// one on the day it is added.
//
// It walks the AST rather than matching source text. A regex keyed to
// `if err := s.write(...)` would miss an equally idiomatic
// `err := s.write(...); if err != nil { ... }` — the count would simply not
// rise, the assertion would still balance, and an unclassified site would pass
// unnoticed. For a guard whose whole purpose is to catch the site nobody
// thought about, recognising only one spelling is no guard at all
// (raised in review on #242).
//
// If this fails because you added a write: route its error through
// s.classifyWriteErr(...). If your write genuinely cannot be in the rejection
// window, say why here and adjust deliberately rather than by reflex.
func TestUnit_EveryWriteSiteClassifiesAuthRejection(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return strings.HasSuffix(fi.Name(), ".go") && !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	require.NoError(t, err)
	require.NotEmpty(t, pkgs, "parsed no package: the scan has gone stale")

	var handled, classified []string
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			// Calls that ARE the whole of a `return <call>` merely forward the
			// error to their own caller; there is nothing to classify at such a
			// site (this is what session.write does with writeWithDeadline).
			forwarded := map[ast.Node]bool{}
			ast.Inspect(file, func(n ast.Node) bool {
				ret, ok := n.(*ast.ReturnStmt)
				if !ok {
					return true
				}
				for _, r := range ret.Results {
					if call, ok := r.(*ast.CallExpr); ok {
						forwarded[call] = true
					}
				}
				return true
			})

			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				recv, ok := sel.X.(*ast.Ident)
				if !ok || recv.Name != "s" {
					return true
				}
				pos := fset.Position(call.Pos())
				where := fmt.Sprintf("%s:%d", filepath.Base(name), pos.Line)
				switch {
				case sel.Sel.Name == "classifyWriteErr":
					classified = append(classified, where)
				case writeMethods[sel.Sel.Name] && !forwarded[call]:
					handled = append(handled, where)
				}
				return true
			})
		}
	}

	require.NotEmpty(t, handled, "found no error-handling write sites at all: the scan has gone stale")
	require.Len(t, classified, len(handled),
		"every connection write whose error is handled must be routed through s.classifyWriteErr, "+
			"or a rejected auth token can be retried.\n  write sites (%d): %v\n  classified (%d): %v",
		len(handled), handled, len(classified), classified)
}
