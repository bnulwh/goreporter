// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build go1.5

package ssa_test

// This file defines tests of source-level debugging utilities.

import (
	"fmt"
	"go/ast"
	exact "go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/bnulwh/goreporter/linters/simpler/ssa"
	"github.com/bnulwh/goreporter/linters/simpler/ssa/ssautil"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/loader"
)

func TestObjValueLookup(t *testing.T) {
	if runtime.GOOS == "android" {
		t.Skipf("no testdata directory on %s", runtime.GOOS)
	}

	conf := loader.Config{ParserMode: parser.ParseComments}
	f, err := conf.ParseFile("testdata/objlookup.go", nil)
	if err != nil {
		t.Error(err)
		return
	}
	conf.CreateFromFiles("main", f)

	// Maps each var Ident (represented "name:linenum") to the
	// kind of ssa.Value we expect (represented "Constant", "&Alloc").
	expectations := make(map[string]string)

	// Find all annotations of form x::BinOp, &y::Alloc, etc.
	re := regexp.MustCompile(`(\b|&)?(\w*)::(\w*)\b`)
	for _, c := range f.Comments {
		text := c.Text()
		pos := conf.Fset.Position(c.Pos())
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			key := fmt.Sprintf("%s:%d", m[2], pos.Line)
			value := m[1] + m[3]
			expectations[key] = value
		}
	}

	iprog, err := conf.Load()
	if err != nil {
		t.Error(err)
		return
	}

	prog := ssautil.CreateProgram(iprog, 0 /*|ssa.PrintFunctions*/)
	mainInfo := iprog.Created[0]
	mainPkg := prog.Package(mainInfo.Pkg)
	mainPkg.SetDebugMode(true)
	mainPkg.Build()

	var varIds []*ast.Ident
	var varObjs []*types.Var
	for id, obj := range mainInfo.Defs {
		// Check invariants for func and const objects.
		switch obj := obj.(type) {
		case *types.Func:
			checkFuncValue(t, prog, obj)

		case *types.Const:
			checkConstValue(t, prog, obj)

		case *types.Var:
			if id.Name == "_" {
				continue
			}
			varIds = append(varIds, id)
			varObjs = append(varObjs, obj)
		}
	}
	for id, obj := range mainInfo.Uses {
		if obj, ok := obj.(*types.Var); ok {
			varIds = append(varIds, id)
			varObjs = append(varObjs, obj)
		}
	}

	// Check invariants for var objects.
	// The result varies based on the specific Ident.
	for i, id := range varIds {
		obj := varObjs[i]
		ref, _ := astutil.PathEnclosingInterval(f, id.Pos(), id.Pos())
		pos := prog.Fset.Position(id.Pos())
		exp := expectations[fmt.Sprintf("%s:%d", id.Name, pos.Line)]
		if exp == "" {
			t.Errorf("%s: no expectation for var ident %s ", pos, id.Name)
			continue
		}
		wantAddr := false
		if exp[0] == '&' {
			wantAddr = true
			exp = exp[1:]
		}
		checkVarValue(t, prog, mainPkg, ref, obj, exp, wantAddr)
	}
}

func checkFuncValue(t *testing.T, prog *ssa.Program, obj *types.Func) {
	fn := prog.FuncValue(obj)
	// fmt.Printf("FuncValue(%s) = %s\n", obj, fn) // debugging
	if fn == nil {
		if obj.Name() != "interfaceMethod" {
			t.Errorf("FuncValue(%s) == nil", obj)
		}
		return
	}
	if fnobj := fn.Object(); fnobj != obj {
		t.Errorf("FuncValue(%s).Object() == %s; value was %s",
			obj, fnobj, fn.Name())
		return
	}
	if !types.Identical(fn.Type(), obj.Type()) {
		t.Errorf("FuncValue(%s).Type() == %s", obj, fn.Type())
		return
	}
}

func checkConstValue(t *testing.T, prog *ssa.Program, obj *types.Const) {
	c := prog.ConstValue(obj)
	// fmt.Printf("ConstValue(%s) = %s\n", obj, c) // debugging
	if c == nil {
		t.Errorf("ConstValue(%s) == nil", obj)
		return
	}
	if !types.Identical(c.Type(), obj.Type()) {
		t.Errorf("ConstValue(%s).Type() == %s", obj, c.Type())
		return
	}
	if obj.Name() != "nil" {
		if !exact.Compare(c.Value, token.EQL, obj.Val()) {
			t.Errorf("ConstValue(%s).Value (%s) != %s",
				obj, c.Value, obj.Val())
			return
		}
	}
}

func checkVarValue(t *testing.T, prog *ssa.Program, pkg *ssa.Package, ref []ast.Node, obj *types.Var, expKind string, wantAddr bool) {
	// The prefix of all assertions messages.
	prefix := fmt.Sprintf("VarValue(%s @ L%d)",
		obj, prog.Fset.Position(ref[0].Pos()).Line)

	v, gotAddr := prog.VarValue(obj, pkg, ref)

	// Kind is the concrete type of the ssa Value.
	gotKind := "nil"
	if v != nil {
		gotKind = fmt.Sprintf("%T", v)[len("*ssa."):]
	}

	// fmt.Printf("%s = %v (kind %q; expect %q) wantAddr=%t gotAddr=%t\n", prefix, v, gotKind, expKind, wantAddr, gotAddr) // debugging

	// Check the kinds match.
	// "nil" indicates expected failure (e.g. optimized away).
	if expKind != gotKind {
		t.Errorf("%s concrete type == %s, want %s", prefix, gotKind, expKind)
	}

	// Check the types match.
	// If wantAddr, the expected type is the object's address.
	if v != nil {
		expType := obj.Type()
		if wantAddr {
			expType = types.NewPointer(expType)
			if !gotAddr {
				t.Errorf("%s: got value, want address", prefix)
			}
		} else if gotAddr {
			t.Errorf("%s: got address, want value", prefix)
		}
		if !types.Identical(v.Type(), expType) {
			t.Errorf("%s.Type() == %s, want %s", prefix, v.Type(), expType)
		}
	}
}

// Ensure that, in debug mode, we can determine the ssa.Value
// corresponding to every ast.Expr.
func TestValueForExpr(t *testing.T) {
	if runtime.GOOS == "android" {
		t.Skipf("no testdata dir on %s", runtime.GOOS)
	}

	conf := loader.Config{ParserMode: parser.ParseComments}
	f, err := conf.ParseFile("testdata/valueforexpr.go", nil)
	if err != nil {
		t.Error(err)
		return
	}
	conf.CreateFromFiles("main", f)

	iprog, err := conf.Load()
	if err != nil {
		t.Error(err)
		return
	}

	mainInfo := iprog.Created[0]

	prog := ssautil.CreateProgram(iprog, 0)
	mainPkg := prog.Package(mainInfo.Pkg)
	mainPkg.SetDebugMode(true)
	mainPkg.Build()

	if false {
		// debugging
		for _, mem := range mainPkg.Members {
			if fn, ok := mem.(*ssa.Function); ok {
				fn.WriteTo(os.Stderr)
			}
		}
	}

	// Find the actual AST node for each canonical position.
	parenExprByPos := make(map[token.Pos]*ast.ParenExpr)
	ast.Inspect(f, func(n ast.Node) bool {
		if n != nil {
			if e, ok := n.(*ast.ParenExpr); ok {
				parenExprByPos[e.Pos()] = e
			}
		}
		return true
	})

	// Find all annotations of form /*@kind*/.
	for _, c := range f.Comments {
		text := strings.TrimSpace(c.Text())
		if text == "" || text[0] != '@' {
			continue
		}
		text = text[1:]
		pos := c.End() + 1
		position := prog.Fset.Position(pos)
		var e ast.Expr
		if target := parenExprByPos[pos]; target == nil {
			t.Errorf("%s: annotation doesn't precede ParenExpr: %q", position, text)
			continue
		} else {
			e = target.X
		}

		path, _ := astutil.PathEnclosingInterval(f, pos, pos)
		if path == nil {
			t.Errorf("%s: can't find AST path from root to comment: %s", position, text)
			continue
		}

		fn := ssa.EnclosingFunction(mainPkg, path)
		if fn == nil {
			t.Errorf("%s: can't find enclosing function", position)
			continue
		}

		v, gotAddr := fn.ValueForExpr(e) // (may be nil)
		got := strings.TrimPrefix(fmt.Sprintf("%T", v), "*ssa.")
		if want := text; got != want {
			t.Errorf("%s: got value %q, want %q", position, got, want)
		}
		if v != nil {
			T := v.Type()
			if gotAddr {
				T = T.Underlying().(*types.Pointer).Elem() // deref
			}
			if !types.Identical(T, mainInfo.TypeOf(e)) {
				t.Errorf("%s: got type %s, want %s", position, mainInfo.TypeOf(e), T)
			}
		}
	}
}

// findInterval parses input and returns the [start, end) positions of
// the first occurrence of substr in input.  f==nil indicates failure;
// an error has already been reported in that case.
//
func findInterval(t *testing.T, fset *token.FileSet, input, substr string) (f *ast.File, start, end token.Pos) {
	f, err := parser.ParseFile(fset, "<input>", input, 0)
	if err != nil {
		t.Errorf("parse error: %s", err)
		return
	}

	i := strings.Index(input, substr)
	if i < 0 {
		t.Errorf("%q is not a substring of input", substr)
		f = nil
		return
	}

	filePos := fset.File(f.Package)
	return f, filePos.Pos(i), filePos.Pos(i + len(substr))
}

func TestEnclosingFunction(t *testing.T) {
	tests := []struct {
		input  string // the input file
		substr string // first occurrence of this string denotes interval
		fn     string // name of expected containing function
	}{
		// We use distinctive numbers as syntactic landmarks.

		// Ordinary function:
		{`package main
		  func f() { println(1003) }`,
			"100", "main.f"},
		// Methods:
		{`package main
                  type T int
		  func (t T) f() { println(200) }`,
			"200", "(main.T).f"},
		// Function literal:
		{`package main
		  func f() { println(func() { print(300) }) }`,
			"300", "main.f$1"},
		// Doubly nested
		{`package main
		  func f() { println(func() { print(func() { print(350) })})}`,
			"350", "main.f$1$1"},
		// Implicit init for package-level var initializer.
		{"package main; var a = 400", "400", "main.init"},
		// No code for constants:
		{"package main; const a = 500", "500", "(none)"},
		// Explicit init()
		{"package main; func init() { println(600) }", "600", "main.init#1"},
		// Multiple explicit init functions:
		{`package main
		  func init() { println("foo") }
		  func init() { println(800) }`,
			"800", "main.init#2"},
		// init() containing FuncLit.
		{`package main
		  func init() { println(func(){print(900)}) }`,
			"900", "main.init#1$1"},
	}
	for _, test := range tests {
		conf := loader.Config{Fset: token.NewFileSet()}
		f, start, end := findInterval(t, conf.Fset, test.input, test.substr)
		if f == nil {
			continue
		}
		path, exact := astutil.PathEnclosingInterval(f, start, end)
		if !exact {
			t.Errorf("EnclosingFunction(%q) not exact", test.substr)
			continue
		}

		conf.CreateFromFiles("main", f)

		iprog, err := conf.Load()
		if err != nil {
			t.Error(err)
			continue
		}
		prog := ssautil.CreateProgram(iprog, 0)
		pkg := prog.Package(iprog.Created[0].Pkg)
		pkg.Build()

		name := "(none)"
		fn := ssa.EnclosingFunction(pkg, path)
		if fn != nil {
			name = fn.String()
		}

		if name != test.fn {
			t.Errorf("EnclosingFunction(%q in %q) got %s, want %s",
				test.substr, test.input, name, test.fn)
			continue
		}

		// While we're here: test HasEnclosingFunction.
		if has := ssa.HasEnclosingFunction(pkg, path); has != (fn != nil) {
			t.Errorf("HasEnclosingFunction(%q in %q) got %v, want %v",
				test.substr, test.input, has, fn != nil)
			continue
		}
	}
}
