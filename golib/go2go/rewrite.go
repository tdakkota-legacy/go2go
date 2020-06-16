// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package go2go

import (
	"bufio"
	"fmt"
	"github.com/tdakkota/go2go/golib/ast"
	"github.com/tdakkota/go2go/golib/printer"
	"github.com/tdakkota/go2go/golib/token"
	"github.com/tdakkota/go2go/golib/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

var config = printer.Config{
	Mode:     printer.UseSpaces | printer.TabIndent | printer.SourcePos,
	Tabwidth: 8,
}

// isParameterizedFuncDecl reports whether fd is a parameterized function.
func isParameterizedFuncDecl(fd *ast.FuncDecl, info *types.Info) bool {
	if fd.Type.TParams != nil {
		return true
	}
	if fd.Recv != nil {
		rtyp := info.TypeOf(fd.Recv.List[0].Type)
		if rtyp == nil {
			// Already instantiated.
			return false
		}
		if p, ok := rtyp.(*types.Pointer); ok {
			rtyp = p.Elem()
		}
		if named, ok := rtyp.(*types.Named); ok {
			if named.TParams() != nil {
				return true
			}
		}
	}
	return false
}

// isParameterizedTypeDecl reports whether s is a parameterized type.
func isParameterizedTypeDecl(s ast.Spec) bool {
	ts := s.(*ast.TypeSpec)
	return ts.TParams != nil
}

// A translator is used to translate a file from Go with contracts to Go 1.
type translator struct {
	fset               *token.FileSet
	importer           *Importer
	tpkg               *types.Package
	types              map[ast.Expr]types.Type
	instantiations     map[string][]*instantiation
	newDecls           []ast.Decl
	typeInstantiations map[types.Type][]*typeInstantiation

	// err is set if we have seen an error during this translation.
	// This is used by the rewrite methods.
	err error
}

// An instantiation is a single instantiation of a function.
type instantiation struct {
	types []types.Type
	decl  *ast.Ident
}

// A typeInstantiation is a single instantiation of a type.
type typeInstantiation struct {
	types []types.Type
	decl  *ast.Ident
	typ   types.Type
}

// rewrite rewrites the contents of one file.
func rewriteFile(dir string, fset *token.FileSet, importer *Importer, importPath string, tpkg *types.Package, filename string, file *ast.File, addImportableName bool) (err error) {
	if err := rewriteAST(fset, importer, importPath, tpkg, file, addImportableName); err != nil {
		return err
	}

	filename = filepath.Base(filename)
	goFile := strings.TrimSuffix(filename, filepath.Ext(filename)) + ".go"
	o, err := os.Create(filepath.Join(dir, goFile))
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := o.Close(); err == nil {
			err = closeErr
		}
	}()

	w := bufio.NewWriter(o)
	defer func() {
		if flushErr := w.Flush(); err == nil {
			err = flushErr
		}
	}()
	fmt.Fprintln(w, rewritePrefix)

	return config.Fprint(w, fset, file)
}

// rewriteAST rewrites the AST for a file.
func rewriteAST(fset *token.FileSet, importer *Importer, importPath string, tpkg *types.Package, file *ast.File, addImportableName bool) (err error) {
	t := translator{
		fset:               fset,
		importer:           importer,
		tpkg:               tpkg,
		types:              make(map[ast.Expr]types.Type),
		instantiations:     make(map[string][]*instantiation),
		typeInstantiations: make(map[types.Type][]*typeInstantiation),
	}
	t.translate(file)

	// Add all the transitive imports. This is more than we need,
	// but we're not trying to be elegant here.
	imps := make(map[string]bool)

	for _, p := range importer.transitiveImports(importPath) {
		imps[p] = true
	}

	decls := make([]ast.Decl, 0, len(file.Decls))
	var specs []ast.Spec
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.IMPORT {
			decls = append(decls, decl)
			continue
		}
		for _, spec := range gen.Specs {
			imp := spec.(*ast.ImportSpec)
			if imp.Name != nil {
				specs = append(specs, imp)
			}
			// We picked up Go 2 imports above, but we still
			// need to pick up Go 1 imports here.
			path := strings.TrimPrefix(strings.TrimSuffix(imp.Path.Value, `"`), `"`)
			if imps[path] {
				continue
			}
			imps[path] = true
			for _, p := range importer.transitiveImports(path) {
				imps[p] = true
			}
		}
	}
	file.Decls = decls

	paths := make([]string, 0, len(imps))
	for p := range imps {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		specs = append(specs, ast.Spec(&ast.ImportSpec{
			Path: &ast.BasicLit{
				Kind:  token.STRING,
				Value: strconv.Quote(p),
			},
		}))
	}
	if len(specs) > 0 {
		first := &ast.GenDecl{
			Tok:   token.IMPORT,
			Specs: specs,
		}
		file.Decls = append([]ast.Decl{first}, file.Decls...)
	}

	// Add a name that other packages can reference to avoid an error
	// about an unused package.
	if addImportableName {
		file.Decls = append(file.Decls,
			&ast.GenDecl{
				Tok: token.TYPE,
				Specs: []ast.Spec{
					&ast.TypeSpec{
						Name: ast.NewIdent(t.importableName()),
						Type: ast.NewIdent("int"),
					},
				},
			})
	}

	// Add a reference for each imported package to avoid an error
	// about an unused package.
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.IMPORT {
			continue
		}
		for _, spec := range gen.Specs {
			imp := spec.(*ast.ImportSpec)
			if imp.Name != nil && imp.Name.Name == "_" {
				continue
			}
			path := strings.TrimPrefix(strings.TrimSuffix(imp.Path.Value, `"`), `"`)

			var tok token.Token
			var importableName string
			if _, ok := importer.lookupPackage(path); ok {
				tok = token.TYPE
				importableName = t.importableName()
			} else {
				fileDir := filepath.Dir(fset.Position(file.Name.Pos()).Filename)
				pkg, err := importer.ImportFrom(path, fileDir, 0)
				if err != nil {
					return err
				}
				scope := pkg.Scope()
				names := scope.Names()
			nameLoop:
				for _, name := range names {
					if !token.IsExported(name) {
						continue
					}
					obj := scope.Lookup(name)
					switch obj.(type) {
					case *types.TypeName:
						tok = token.TYPE
						importableName = name
						break nameLoop
					case *types.Var, *types.Func:
						tok = token.VAR
						importableName = name
						break nameLoop
					case *types.Const:
						tok = token.CONST
						importableName = name
						break nameLoop
					}
				}
				if importableName == "" {
					return fmt.Errorf("can't find any importable name in package %q", path)
				}
			}

			var name string
			if imp.Name != nil {
				name = imp.Name.Name
			} else {
				name = filepath.Base(path)
			}
			var spec ast.Spec
			switch tok {
			case token.CONST, token.VAR:
				spec = &ast.ValueSpec{
					Names: []*ast.Ident{
						ast.NewIdent("_"),
					},
					Values: []ast.Expr{
						&ast.SelectorExpr{
							X:   ast.NewIdent(name),
							Sel: ast.NewIdent(importableName),
						},
					},
				}
			case token.TYPE:
				spec = &ast.TypeSpec{
					Name: ast.NewIdent("_"),
					Type: &ast.SelectorExpr{
						X:   ast.NewIdent(name),
						Sel: ast.NewIdent(importableName),
					},
				}
			default:
				panic("can't happen")
			}
			file.Decls = append(file.Decls,
				&ast.GenDecl{
					Tok:   tok,
					Specs: []ast.Spec{spec},
				})
		}
	}

	return t.err
}

// translate translates the AST for a file from Go with contracts to Go 1.
func (t *translator) translate(file *ast.File) {
	declsToDo := file.Decls
	file.Decls = nil
	for len(declsToDo) > 0 {
		newDecls := make([]ast.Decl, 0, len(declsToDo))
		for i, decl := range declsToDo {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if !isParameterizedFuncDecl(decl, t.importer.info) {
					t.translateFuncDecl(&declsToDo[i])
					newDecls = append(newDecls, decl)
				}
			case *ast.GenDecl:
				switch decl.Tok {
				case token.TYPE:
					newSpecs := make([]ast.Spec, 0, len(decl.Specs))
					for j := range decl.Specs {
						if !isParameterizedTypeDecl(decl.Specs[j]) {
							t.translateTypeSpec(&decl.Specs[j])
							newSpecs = append(newSpecs, decl.Specs[j])
						}
					}
					if len(newSpecs) == 0 {
						decl = nil
					} else {
						decl.Specs = newSpecs
					}
				case token.VAR, token.CONST:
					for j := range decl.Specs {
						t.translateValueSpec(&decl.Specs[j])
					}
				case token.IDENT:
					// A contract.
					decl = nil
				}
				if decl != nil {
					newDecls = append(newDecls, decl)
				}
			default:
				newDecls = append(newDecls, decl)
			}
		}
		file.Decls = append(file.Decls, newDecls...)
		declsToDo = t.newDecls
		t.newDecls = nil
	}
}

// translateTypeSpec translates a type from Go with contracts to Go 1.
func (t *translator) translateTypeSpec(ps *ast.Spec) {
	ts := (*ps).(*ast.TypeSpec)
	if ts.TParams != nil {
		panic("parameterized type")
	}
	t.translateExpr(&ts.Type)
}

// translateValueSpec translates a variable or constant from Go with
// contracts to Go 1.
func (t *translator) translateValueSpec(ps *ast.Spec) {
	vs := (*ps).(*ast.ValueSpec)
	t.translateExpr(&vs.Type)
	for i := range vs.Values {
		t.translateExpr(&vs.Values[i])
	}
}

// translateFuncDecl translates a function from Go with contracts to Go 1.
func (t *translator) translateFuncDecl(pd *ast.Decl) {
	if t.err != nil {
		return
	}
	fd := (*pd).(*ast.FuncDecl)
	if fd.Type.TParams != nil {
		panic("parameterized function")
	}
	if fd.Recv != nil {
		t.translateFieldList(fd.Recv)
	}
	t.translateFieldList(fd.Type.Params)
	t.translateFieldList(fd.Type.Results)
	t.translateBlockStmt(fd.Body)
}

// translateBlockStmt translates a block statement from Go with
// contracts to Go 1.
func (t *translator) translateBlockStmt(pbs *ast.BlockStmt) {
	for i := range pbs.List {
		t.translateStmt(&pbs.List[i])
	}
}

// translateStmt translates a statement from Go with contracts to Go 1.
func (t *translator) translateStmt(ps *ast.Stmt) {
	if t.err != nil {
		return
	}
	if *ps == nil {
		return
	}
	switch s := (*ps).(type) {
	case *ast.DeclStmt:
		d := s.Decl.(*ast.GenDecl)
		switch d.Tok {
		case token.TYPE:
			for i := range d.Specs {
				t.translateTypeSpec(&d.Specs[i])
			}
		case token.CONST, token.VAR:
			for i := range d.Specs {
				t.translateValueSpec(&d.Specs[i])
			}
		default:
			panic(fmt.Sprintf("unknown decl type %v", d.Tok))
		}
	case *ast.EmptyStmt:
	case *ast.LabeledStmt:
		t.translateStmt(&s.Stmt)
	case *ast.ExprStmt:
		t.translateExpr(&s.X)
	case *ast.SendStmt:
		t.translateExpr(&s.Chan)
		t.translateExpr(&s.Value)
	case *ast.IncDecStmt:
		t.translateExpr(&s.X)
	case *ast.AssignStmt:
		t.translateExprList(s.Lhs)
		t.translateExprList(s.Rhs)
	case *ast.GoStmt:
		e := ast.Expr(s.Call)
		t.translateExpr(&e)
		s.Call = e.(*ast.CallExpr)
	case *ast.DeferStmt:
		e := ast.Expr(s.Call)
		t.translateExpr(&e)
		s.Call = e.(*ast.CallExpr)
	case *ast.ReturnStmt:
		t.translateExprList(s.Results)
	case *ast.BranchStmt:
	case *ast.BlockStmt:
		t.translateBlockStmt(s)
	case *ast.IfStmt:
		t.translateStmt(&s.Init)
		t.translateExpr(&s.Cond)
		t.translateBlockStmt(s.Body)
		t.translateStmt(&s.Else)
	case *ast.CaseClause:
		t.translateExprList(s.List)
		t.translateStmtList(s.Body)
	case *ast.SwitchStmt:
		t.translateStmt(&s.Init)
		t.translateExpr(&s.Tag)
		t.translateBlockStmt(s.Body)
	case *ast.TypeSwitchStmt:
		t.translateStmt(&s.Init)
		t.translateStmt(&s.Assign)
		t.translateBlockStmt(s.Body)
	case *ast.CommClause:
		t.translateStmt(&s.Comm)
		t.translateStmtList(s.Body)
	case *ast.SelectStmt:
		t.translateBlockStmt(s.Body)
	case *ast.ForStmt:
		t.translateStmt(&s.Init)
		t.translateExpr(&s.Cond)
		t.translateStmt(&s.Post)
		t.translateBlockStmt(s.Body)
	case *ast.RangeStmt:
		t.translateExpr(&s.Key)
		t.translateExpr(&s.Value)
		t.translateExpr(&s.X)
		t.translateBlockStmt(s.Body)
	default:
		panic(fmt.Sprintf("unimplemented Stmt %T", s))
	}
}

// translateStmtList translates a list of statements from Go with
// contracts to Go 1.
func (t *translator) translateStmtList(sl []ast.Stmt) {
	for i := range sl {
		t.translateStmt(&sl[i])
	}
}

// translateExpr translates an expression from Go with contracts to Go 1.
func (t *translator) translateExpr(pe *ast.Expr) {
	if t.err != nil {
		return
	}
	if *pe == nil {
		return
	}
	switch e := (*pe).(type) {
	case *ast.Ident:
	case *ast.Ellipsis:
		t.translateExpr(&e.Elt)
	case *ast.BasicLit:
	case *ast.FuncLit:
		t.translateFieldList(e.Type.TParams)
		t.translateFieldList(e.Type.Params)
		t.translateFieldList(e.Type.Results)
		t.translateBlockStmt(e.Body)
	case *ast.CompositeLit:
		t.translateExpr(&e.Type)
		t.translateExprList(e.Elts)
	case *ast.ParenExpr:
		t.translateExpr(&e.X)
	case *ast.SelectorExpr:
		t.translateExpr(&e.X)
	case *ast.IndexExpr:
		t.translateExpr(&e.X)
		t.translateExpr(&e.Index)
	case *ast.SliceExpr:
		t.translateExpr(&e.X)
		t.translateExpr(&e.Low)
		t.translateExpr(&e.High)
		t.translateExpr(&e.Max)
	case *ast.TypeAssertExpr:
		t.translateExpr(&e.X)
		t.translateExpr(&e.Type)
	case *ast.CallExpr:
		t.translateExprList(e.Args)
		if ftyp, ok := t.lookupType(e.Fun).(*types.Signature); ok && len(ftyp.TParams()) > 0 {
			t.translateFunctionInstantiation(pe)
		} else if ntyp, ok := t.lookupType(e.Fun).(*types.Named); ok && len(ntyp.TParams()) > 0 && len(ntyp.TArgs()) == 0 {
			t.translateTypeInstantiation(pe)
		}
		t.translateExpr(&e.Fun)
	case *ast.StarExpr:
		t.translateExpr(&e.X)
	case *ast.UnaryExpr:
		t.translateExpr(&e.X)
	case *ast.BinaryExpr:
		t.translateExpr(&e.X)
		t.translateExpr(&e.Y)
	case *ast.KeyValueExpr:
		t.translateExpr(&e.Key)
		t.translateExpr(&e.Value)
	case *ast.ArrayType:
		t.translateExpr(&e.Elt)
	case *ast.StructType:
		t.translateFieldList(e.Fields)
	case *ast.FuncType:
		t.translateFieldList(e.TParams)
		t.translateFieldList(e.Params)
		t.translateFieldList(e.Results)
	case *ast.InterfaceType:
		methods, types := splitFieldList(e.Methods)
		t.translateFieldList(methods)
		t.translateExprList(types)
	case *ast.MapType:
		t.translateExpr(&e.Key)
		t.translateExpr(&e.Value)
	case *ast.ChanType:
		t.translateExpr(&e.Value)
	default:
		panic(fmt.Sprintf("unimplemented Expr %T", e))
	}
}

// TODO(iant) refactor code and get rid of this?
func splitFieldList(fl *ast.FieldList) (methods *ast.FieldList, types []ast.Expr) {
	if fl == nil {
		return
	}
	var mfields []*ast.Field
	for _, f := range fl.List {
		if len(f.Names) > 0 && f.Names[0].Name == "type" {
			// type list type
			types = append(types, f.Type)
		} else {
			mfields = append(mfields, f)
		}
	}
	copy := *fl
	copy.List = mfields
	methods = &copy
	return
}

// TODO(iant) refactor code and get rid of this?
func mergeFieldList(methods *ast.FieldList, types []ast.Expr) (fl *ast.FieldList) {
	fl = methods
	if len(types) == 0 {
		return
	}
	if fl == nil {
		fl = new(ast.FieldList)
	}
	name := []*ast.Ident{ast.NewIdent("type")}
	for _, typ := range types {
		fl.List = append(fl.List, &ast.Field{Names: name, Type: typ})
	}
	return
}

// translateExprList translate an expression list from Go with
// contracts to Go 1.
func (t *translator) translateExprList(el []ast.Expr) {
	for i := range el {
		t.translateExpr(&el[i])
	}
}

// translateFieldList translates a field list from Go with contracts to Go 1.
func (t *translator) translateFieldList(fl *ast.FieldList) {
	if fl == nil {
		return
	}
	for _, f := range fl.List {
		t.translateField(f)
	}
}

// translateField translates a field from Go with contracts to Go 1.
func (t *translator) translateField(f *ast.Field) {
	t.translateExpr(&f.Type)
}

// translateFunctionInstantiation translates an instantiated function
// to Go 1.
func (t *translator) translateFunctionInstantiation(pe *ast.Expr) {
	call := (*pe).(*ast.CallExpr)
	qid := t.instantiatedIdent(call)
	argList, typeList, typeArgs := t.instantiationTypes(call)

	var instIdent *ast.Ident
	key := qid.String()
	instantiations := t.instantiations[key]
	for _, inst := range instantiations {
		if t.sameTypes(typeList, inst.types) {
			instIdent = inst.decl
			break
		}
	}

	if instIdent == nil {
		var err error
		instIdent, err = t.instantiateFunction(qid, argList, typeList)
		if err != nil {
			t.err = err
			return
		}

		n := &instantiation{
			types: typeList,
			decl:  instIdent,
		}
		t.instantiations[key] = append(instantiations, n)
	}

	if typeArgs {
		*pe = instIdent
	} else {
		newCall := *call
		newCall.Fun = instIdent
		*pe = &newCall
	}
}

// translateTypeInstantiation translates an instantiated type to Go 1.
func (t *translator) translateTypeInstantiation(pe *ast.Expr) {
	call := (*pe).(*ast.CallExpr)
	qid := t.instantiatedIdent(call)
	typ := t.lookupType(call.Fun).(*types.Named)
	argList, typeList, typeArgs := t.instantiationTypes(call)
	if !typeArgs {
		panic("no type arguments for type")
	}

	instantiations := t.typeInstantiations[typ]
	for _, inst := range instantiations {
		if t.sameTypes(typeList, inst.types) {
			*pe = inst.decl
			return
		}
	}

	instIdent, instType, err := t.instantiateTypeDecl(qid, typ, argList, typeList)
	if err != nil {
		t.err = err
		return
	}

	n := &typeInstantiation{
		types: typeList,
		decl:  instIdent,
		typ:   instType,
	}
	t.typeInstantiations[typ] = append(instantiations, n)

	*pe = instIdent
}

// instantiatedIdent returns the qualified identifer that is being
// instantiated.
func (t *translator) instantiatedIdent(call *ast.CallExpr) qualifiedIdent {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return qualifiedIdent{ident: fun}
	case *ast.SelectorExpr:
		pkgname, ok := fun.X.(*ast.Ident)
		if !ok {
			break
		}
		pkgobj, ok := t.importer.info.Uses[pkgname]
		if !ok {
			break
		}
		pn, ok := pkgobj.(*types.PkgName)
		if !ok {
			break
		}
		return qualifiedIdent{pkg: pn.Imported(), ident: fun.Sel}
	}
	panic(fmt.Sprintf("instantiated object %T %v is not an identifier", call.Fun, call.Fun))
}

// instantiationTypes returns the type arguments of an instantiation.
// It also returns the AST arguments if they are present.
// The typeArgs result reports whether the AST arguments are types.
func (t *translator) instantiationTypes(call *ast.CallExpr) (argList []ast.Expr, typeList []types.Type, typeArgs bool) {
	inferred, haveInferred := t.importer.info.Inferred[call]

	if !haveInferred {
		argList = call.Args
		typeList = make([]types.Type, 0, len(argList))
		for _, arg := range argList {
			if at := t.lookupType(arg); at == nil {
				panic(fmt.Sprintf("%s: no type found for %T %v", t.fset.Position(arg.Pos()), arg, arg))
			} else {
				typeList = append(typeList, at)
			}
		}
		typeArgs = true
	} else {
		for _, typ := range inferred.Targs {
			arg := ast.NewIdent(typ.String())
			if named, ok := typ.(*types.Named); ok {
				if len(named.TArgs()) > 0 {
					var narg *ast.Ident
					typ, narg = t.lookupInstantiatedType(named)
					if narg != nil {
						arg = ast.NewIdent(narg.Name)
					}
				}
				if named.Obj().Pkg() == t.tpkg {
					fields := strings.Split(arg.Name, ".")
					if len(fields) > 1 {
						arg = ast.NewIdent(fields[1])
					}
				}
			}
			typeList = append(typeList, typ)
			argList = append(argList, arg)
			t.setType(arg, typ)
		}
	}

	return
}

// lookupInstantiatedType looks for an existing instantiation of an
// instantiated type.
func (t *translator) lookupInstantiatedType(typ *types.Named) (types.Type, *ast.Ident) {
	name := typ.Obj().Name()
	fields := strings.Split(name, ".")
	if len(fields) > 2 {
		panic(fmt.Sprintf("unparseable instantiated name %q", name))
	}
	if len(fields) > 1 {
		name = fields[1]
	}

	tpkg := typ.Obj().Pkg()
	nobj := tpkg.Scope().Lookup(name)
	if nobj == nil {
		panic(fmt.Sprintf("can't find %q in scope of package %q", name, tpkg.Name()))
	}

	targs := typ.TArgs()
	instantiations := t.typeInstantiations[nobj.Type()]
	for _, inst := range instantiations {
		if t.sameTypes(targs, inst.types) {
			newName := inst.decl.Name
			nm := typ.NumMethods()
			methods := make([]*types.Func, 0, nm)
			for i := 0; i < nm; i++ {
				methods = append(methods, typ.Method(i))
			}
			obj := typ.Obj()
			obj = types.NewTypeName(obj.Pos(), obj.Pkg(), newName, nil)
			nt := types.NewNamed(obj, typ.Underlying(), methods)
			nt.SetTArgs(targs)
			return nt, inst.decl
		}
	}

	panic(fmt.Sprintf("did not find instantiation for %v %v\n", typ, typ.Underlying()))
}

// sameTypes reports whether two type slices are the same.
func (t *translator) sameTypes(a, b []types.Type) bool {
	if len(a) != len(b) {
		return false
	}
	for i, x := range a {
		if !types.Identical(x, b[i]) {
			return false
		}
	}
	return true
}

// qualifiedIdent is an identifier possibly qualified with a package.
type qualifiedIdent struct {
	pkg   *types.Package // identifier's package; nil for current package
	ident *ast.Ident
}

// String returns a printable name for qid.
func (qid qualifiedIdent) String() string {
	if qid.pkg == nil {
		return qid.ident.Name
	}
	return qid.pkg.Path() + "." + qid.ident.Name
}
