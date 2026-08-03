package supervisor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionGoFuncLiteralsHavePanicBoundary(t *testing.T) {
	root := repoRoot(t)
	for _, dir := range []string{filepath.Join(root, "internal"), filepath.Join(root, "cmd")} {
		if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				switch entry.Name() {
				case ".git", "node_modules":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			checkGoFuncBoundaries(t, root, path)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGoFuncBoundaryCheckerRequiresSupervisorQualifierOutsideSupervisor(t *testing.T) {
	body := parseFirstGoFuncBody(t, `package api

func run() {
	go func() {
		defer Recover("fake")
	}()
}`)
	if hasSupervisorPanicBoundary(body, false) {
		t.Fatal("unqualified Recover outside supervisor package must not satisfy the panic boundary")
	}
}

func TestGoFuncBoundaryCheckerAcceptsSupervisorQualifiedRecover(t *testing.T) {
	body := parseFirstGoFuncBody(t, `package api

func run() {
	go func() {
		defer supervisor.Recover("worker")
	}()
}`)
	if !hasSupervisorPanicBoundary(body, false) {
		t.Fatal("supervisor.Recover should satisfy the panic boundary outside supervisor package")
	}
}

func TestGoFuncBoundaryCheckerAcceptsPackageLocalRecoverInSupervisor(t *testing.T) {
	body := parseFirstGoFuncBody(t, `package supervisor

func run() {
	go func() {
		defer RecoverWithLogf("worker", logf)
	}()
}`)
	if !hasSupervisorPanicBoundary(body, true) {
		t.Fatal("package-local RecoverWithLogf should satisfy the panic boundary inside supervisor package")
	}
}

func TestGoFuncBoundaryCheckerAcceptsDirectTargetWithBoundary(t *testing.T) {
	file, goStmt := parseFirstGoStatement(t, `package supervisor

func run() {
	go worker()
}

func worker() {
	defer Recover("worker")
}`)
	body := sameFileGoTargetBody(file, goStmt)
	if body == nil {
		t.Fatal("direct go target body was not resolved")
	}
	if !hasSupervisorPanicBoundary(body, true) {
		t.Fatal("direct go target with supervisor boundary should be accepted")
	}
}

func TestGoFuncBoundaryCheckerRejectsDirectTargetWithoutBoundary(t *testing.T) {
	file, goStmt := parseFirstGoStatement(t, `package api

type Handler struct{}

func run(h *Handler) {
	go h.worker()
}

func (h *Handler) worker() {
	doWork()
}`)
	body := sameFileGoTargetBody(file, goStmt)
	if body == nil {
		t.Fatal("method go target body was not resolved")
	}
	if hasSupervisorPanicBoundary(body, false) {
		t.Fatal("direct go target without supervisor boundary must not be accepted")
	}
}

func TestTimerCallbackBoundaryCheckerAcceptsAfterFuncWithBoundary(t *testing.T) {
	file, call := parseFirstAfterFuncCall(t, `package api

import "time"

func run() {
	time.AfterFunc(time.Second, func() {
		defer supervisor.Recover("timer")
		doWork()
	})
}`)
	body := timerCallbackBody(file, call)
	if body == nil {
		t.Fatal("time.AfterFunc callback body was not resolved")
	}
	if !hasSupervisorPanicBoundary(body, false) {
		t.Fatal("time.AfterFunc callback with supervisor boundary should be accepted")
	}
}

func TestTimerCallbackBoundaryCheckerRejectsAfterFuncWithoutBoundary(t *testing.T) {
	file, call := parseFirstAfterFuncCall(t, `package api

import "time"

func run() {
	time.AfterFunc(time.Second, func() {
		doWork()
	})
}`)
	body := timerCallbackBody(file, call)
	if body == nil {
		t.Fatal("time.AfterFunc callback body was not resolved")
	}
	if hasSupervisorPanicBoundary(body, false) {
		t.Fatal("time.AfterFunc callback without supervisor boundary must not be accepted")
	}
}

func checkGoFuncBoundaries(t *testing.T, root, path string) {
	t.Helper()
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	allowPackageLocalSupervisorCalls := file.Name.Name == "supervisor"
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.GoStmt:
			fn, ok := n.Call.Fun.(*ast.FuncLit)
			if ok {
				if hasSupervisorPanicBoundary(fn.Body, allowPackageLocalSupervisorCalls) {
					return true
				}
				pos := fileSet.Position(n.Pos())
				t.Errorf("%s:%d go func literal must defer supervisor.Recover or log panic via supervisor.LogPanic", filepath.ToSlash(relativePath(t, root, path)), pos.Line)
				return true
			}
			target := sameFileGoTargetBody(file, n)
			if target != nil && hasSupervisorPanicBoundary(target, allowPackageLocalSupervisorCalls) {
				return true
			}
			pos := fileSet.Position(n.Pos())
			t.Errorf("%s:%d go call target must have a supervisor panic boundary", filepath.ToSlash(relativePath(t, root, path)), pos.Line)
			return true
		case *ast.CallExpr:
			if !isTimeAfterFuncCall(n) {
				return true
			}
			body := timerCallbackBody(file, n)
			if body == nil || hasSupervisorPanicBoundary(body, allowPackageLocalSupervisorCalls) {
				return true
			}
			pos := fileSet.Position(n.Pos())
			t.Errorf("%s:%d time.AfterFunc callback must defer supervisor.Recover or log panic via supervisor.LogPanic", filepath.ToSlash(relativePath(t, root, path)), pos.Line)
			return true
		default:
			return true
		}
	})
}

func sameFileGoTargetBody(file *ast.File, goStmt *ast.GoStmt) *ast.BlockStmt {
	return sameFileCallTargetBody(file, goStmt.Call.Fun)
}

func timerCallbackBody(file *ast.File, call *ast.CallExpr) *ast.BlockStmt {
	if len(call.Args) < 2 {
		return nil
	}
	if fn, ok := call.Args[1].(*ast.FuncLit); ok {
		return fn.Body
	}
	return sameFileCallTargetBody(file, call.Args[1])
}

func sameFileCallTargetBody(file *ast.File, target ast.Expr) *ast.BlockStmt {
	_, name, ok := callName(target)
	if !ok || name == "" {
		return nil
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != name {
			continue
		}
		return fn.Body
	}
	return nil
}

func isTimeAfterFuncCall(call *ast.CallExpr) bool {
	pkg, name, ok := callName(call.Fun)
	return ok && pkg == "time" && name == "AfterFunc"
}

func hasSupervisorPanicBoundary(body *ast.BlockStmt, allowPackageLocalSupervisorCalls bool) bool {
	if body == nil {
		return false
	}
	for _, stmt := range body.List {
		deferStmt, ok := stmt.(*ast.DeferStmt)
		if !ok || deferStmt.Call == nil {
			continue
		}
		if isSupervisorRecoverCall(deferStmt.Call, allowPackageLocalSupervisorCalls) {
			return true
		}
		if fn, ok := deferStmt.Call.Fun.(*ast.FuncLit); ok && deferredFuncLogsPanic(fn, allowPackageLocalSupervisorCalls) {
			return true
		}
	}
	return false
}

func deferredFuncLogsPanic(fn *ast.FuncLit, allowPackageLocalSupervisorCalls bool) bool {
	foundRecover := false
	foundLogPanic := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isIdentCall(call, "recover") {
			foundRecover = true
		}
		if isSupervisorLogPanicCall(call, allowPackageLocalSupervisorCalls) {
			foundLogPanic = true
		}
		return true
	})
	return foundRecover && foundLogPanic
}

func isSupervisorRecoverCall(call *ast.CallExpr, allowPackageLocalSupervisorCalls bool) bool {
	pkg, name, ok := callName(call.Fun)
	if !ok {
		return false
	}
	if pkg == "supervisor" || pkg == "" && allowPackageLocalSupervisorCalls {
		return name == "Recover" || name == "RecoverEvent" || name == "RecoverWithLogf" || name == "RecoverCallback"
	}
	return false
}

func isSupervisorLogPanicCall(call *ast.CallExpr, allowPackageLocalSupervisorCalls bool) bool {
	pkg, name, ok := callName(call.Fun)
	if !ok {
		return false
	}
	if pkg == "supervisor" || pkg == "" && allowPackageLocalSupervisorCalls {
		return name == "LogPanic" || name == "LogPanicWithLogf"
	}
	return false
}

func isIdentCall(call *ast.CallExpr, name string) bool {
	ident, ok := call.Fun.(*ast.Ident)
	return ok && ident.Name == name
}

func callName(expr ast.Expr) (pkg, name string, ok bool) {
	switch fn := expr.(type) {
	case *ast.Ident:
		return "", fn.Name, true
	case *ast.SelectorExpr:
		ident, ok := fn.X.(*ast.Ident)
		if !ok {
			return "", "", false
		}
		return ident.Name, fn.Sel.Name, true
	default:
		return "", "", false
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		next := filepath.Dir(dir)
		if next == dir {
			t.Fatal("go.mod not found")
		}
		dir = next
	}
}

func relativePath(t *testing.T, root, path string) string {
	t.Helper()
	rel, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

func parseFirstGoFuncBody(t *testing.T, src string) *ast.BlockStmt {
	t.Helper()
	_, goStmt := parseFirstGoStatement(t, src)
	var body *ast.BlockStmt
	fn, ok := goStmt.Call.Fun.(*ast.FuncLit)
	if !ok {
		t.Fatal("go func literal not found")
	}
	body = fn.Body
	return body
}

func parseFirstGoStatement(t *testing.T, src string) (*ast.File, *ast.GoStmt) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "snippet.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var goStmt *ast.GoStmt
	ast.Inspect(file, func(node ast.Node) bool {
		if goStmt != nil {
			return false
		}
		stmt, ok := node.(*ast.GoStmt)
		if !ok {
			return true
		}
		goStmt = stmt
		return false
	})
	if goStmt == nil {
		t.Fatal("go statement not found")
	}
	return file, goStmt
}

func parseFirstAfterFuncCall(t *testing.T, src string) (*ast.File, *ast.CallExpr) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "snippet.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var call *ast.CallExpr
	ast.Inspect(file, func(node ast.Node) bool {
		if call != nil {
			return false
		}
		candidate, ok := node.(*ast.CallExpr)
		if !ok || !isTimeAfterFuncCall(candidate) {
			return true
		}
		call = candidate
		return false
	})
	if call == nil {
		t.Fatal("time.AfterFunc call not found")
	}
	return file, call
}
