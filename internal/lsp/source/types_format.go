// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package source

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/printer"
	"go/types"
	"strings"
	"unicode"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/internal/event"
	"golang.org/x/tools/internal/lsp/debug/tag"
	"golang.org/x/tools/internal/lsp/protocol"
)

// formatType returns the detail and kind for a types.Type.
func formatType(typ types.Type, qf types.Qualifier) (detail string, kind protocol.CompletionItemKind) {
	if types.IsInterface(typ) {
		detail = "interface{...}"
		kind = protocol.InterfaceCompletion
	} else if _, ok := typ.(*types.Struct); ok {
		detail = "struct{...}"
		kind = protocol.StructCompletion
	} else if typ != typ.Underlying() {
		detail, kind = formatType(typ.Underlying(), qf)
	} else {
		detail = types.TypeString(typ, qf)
		kind = protocol.ClassCompletion
	}
	return detail, kind
}

type signature struct {
	name, doc        string
	params, results  []string
	variadic         bool
	needResultParens bool
}

func (s *signature) format() string {
	var b strings.Builder
	b.WriteByte('(')
	for i, p := range s.params {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(p)
	}
	b.WriteByte(')')

	// Add space between parameters and results.
	if len(s.results) > 0 {
		b.WriteByte(' ')
	}
	if s.needResultParens {
		b.WriteByte('(')
	}
	for i, r := range s.results {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(r)
	}
	if s.needResultParens {
		b.WriteByte(')')
	}
	return b.String()
}

func newBuiltinSignature(ctx context.Context, view View, name string) (*signature, error) {
	astObj, err := view.LookupBuiltin(ctx, name)
	if err != nil {
		return nil, err
	}
	decl, ok := astObj.Decl.(*ast.FuncDecl)
	if !ok {
		return nil, fmt.Errorf("no function declaration for builtin: %s", name)
	}
	if decl.Type == nil {
		return nil, fmt.Errorf("no type for builtin decl %s", decl.Name)
	}
	var variadic bool
	if decl.Type.Params.List != nil {
		numParams := len(decl.Type.Params.List)
		lastParam := decl.Type.Params.List[numParams-1]
		if _, ok := lastParam.Type.(*ast.Ellipsis); ok {
			variadic = true
		}
	}
	params, _ := formatFieldList(ctx, view, decl.Type.Params, variadic)
	results, needResultParens := formatFieldList(ctx, view, decl.Type.Results, false)
	return &signature{
		doc:              decl.Doc.Text(),
		name:             name,
		needResultParens: needResultParens,
		params:           params,
		results:          results,
		variadic:         variadic,
	}, nil
}

var replacer = strings.NewReplacer(
	`ComplexType`, `complex128`,
	`FloatType`, `float64`,
	`IntegerType`, `int`,
)

func formatFieldList(ctx context.Context, view View, list *ast.FieldList, variadic bool) ([]string, bool) {
	if list == nil {
		return nil, false
	}
	var writeResultParens bool
	var result []string
	for i := 0; i < len(list.List); i++ {
		if i >= 1 {
			writeResultParens = true
		}
		p := list.List[i]
		cfg := printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 4}
		b := &bytes.Buffer{}
		if err := cfg.Fprint(b, view.Session().Cache().FileSet(), p.Type); err != nil {
			event.Error(ctx, "unable to print type", nil, tag.Type.Of(p.Type))
			continue
		}
		typ := replacer.Replace(b.String())
		if len(p.Names) == 0 {
			result = append(result, typ)
		}
		for _, name := range p.Names {
			if name.Name != "" {
				if i == 0 {
					writeResultParens = true
				}
				result = append(result, fmt.Sprintf("%s %s", name.Name, typ))
			} else {
				result = append(result, typ)
			}
		}
	}
	if variadic {
		result[len(result)-1] = strings.Replace(result[len(result)-1], "[]", "...", 1)
	}
	return result, writeResultParens
}

func newSignature(ctx context.Context, s Snapshot, pkg Package, name string, sig *types.Signature, comment *ast.CommentGroup, qf types.Qualifier) (*signature, error) {
	params := make([]string, 0, sig.Params().Len())
	for i := 0; i < sig.Params().Len(); i++ {
		el := sig.Params().At(i)
		typ := formatVarType(ctx, s, pkg, el, qf)
		p := typ
		if el.Name() != "" {
			p = el.Name() + " " + typ
		}
		params = append(params, p)
	}
	var needResultParens bool
	results := make([]string, 0, sig.Results().Len())
	for i := 0; i < sig.Results().Len(); i++ {
		if i >= 1 {
			needResultParens = true
		}
		el := sig.Results().At(i)
		typ := formatVarType(ctx, s, pkg, el, qf)
		if el.Name() == "" {
			results = append(results, typ)
		} else {
			if i == 0 {
				needResultParens = true
			}
			results = append(results, el.Name()+" "+typ)
		}
	}
	var c string
	if comment != nil {
		c = comment.Text()
	}
	return &signature{
		doc:              c,
		params:           params,
		results:          results,
		variadic:         sig.Variadic(),
		needResultParens: needResultParens,
	}, nil
}

// formatVarType formats a *types.Var, accounting for type aliases.
// To do this, it looks in the AST of the file in which the object is declared.
// On any errors, it always fallbacks back to types.TypeString.
func formatVarType(ctx context.Context, s Snapshot, srcpkg Package, obj *types.Var, qf types.Qualifier) string {
	file, pkg, err := findPosInPackage(s.View(), srcpkg, obj.Pos())
	if err != nil {
		return types.TypeString(obj.Type(), qf)
	}
	// Named and unnamed variables must be handled differently.
	// Unnamed variables appear in the result values of a function signature.
	var expr ast.Expr
	if obj.Name() != "" {
		expr, err = namedVarType(ctx, s, pkg, file, obj)
	} else {
		expr, err = unnamedVarType(file, obj)
	}
	if err != nil {
		return types.TypeString(obj.Type(), qf)
	}
	// The type names in the AST may not be correctly qualified.
	// Determine the package name to use based on the package that originated
	// the query and the package in which the type is declared.
	// We then qualify the value by cloning the AST node and editing it.
	pkgName := importedPkgName(s, srcpkg, pkg, file)
	cloned := cloneExpr(expr)
	qualified := qualifyExpr(cloned, pkgName)
	fmted := formatNode(s.View().Session().Cache().FileSet(), qualified)
	return fmted
}

// unnamedVarType finds the type for an unnamed variable.
func unnamedVarType(file *ast.File, obj *types.Var) (ast.Expr, error) {
	path, _ := astutil.PathEnclosingInterval(file, obj.Pos(), obj.Pos())
	var expr ast.Expr
	for _, p := range path {
		e, ok := p.(ast.Expr)
		if !ok {
			break
		}
		expr = e
	}
	typ, ok := expr.(ast.Expr)
	if !ok {
		return nil, fmt.Errorf("unexpected type for node (%T)", path[0])
	}
	return typ, nil
}

// namedVarType returns the type for a named variable.
func namedVarType(ctx context.Context, s Snapshot, pkg Package, file *ast.File, obj *types.Var) (ast.Expr, error) {
	ident, err := findIdentifier(ctx, s, pkg, file, obj.Pos())
	if err != nil {
		return nil, err
	}
	if ident.Declaration.obj != obj {
		return nil, fmt.Errorf("expected the ident's declaration %v to be equal to obj %v", ident.Declaration.obj, obj)
	}
	if i := ident.ident; i == nil || i.Obj == nil || i.Obj.Decl == nil {
		return nil, fmt.Errorf("no declaration for object %s", obj.Name())
	}
	f, ok := ident.ident.Obj.Decl.(*ast.Field)
	if !ok {
		return nil, fmt.Errorf("declaration of object %v is %T, not *ast.Field", obj.Name(), ident.ident.Obj.Decl)
	}
	typ, ok := f.Type.(ast.Expr)
	if !ok {
		return nil, fmt.Errorf("unexpected type for node (%T)", f.Type)
	}
	return typ, nil
}

// importedPkgName returns the package name used for pkg in srcpkg.
func importedPkgName(s Snapshot, srcpkg, pkg Package, file *ast.File) string {
	if srcpkg == pkg {
		return ""
	}
	// If the file already imports the package under another name, use that.
	for _, group := range astutil.Imports(s.View().Session().Cache().FileSet(), file) {
		for _, cand := range group {
			if strings.Trim(cand.Path.Value, `"`) == pkg.PkgPath() {
				if cand.Name != nil && cand.Name.Name != "" {
					return cand.Name.Name
				}
			}
		}
	}
	return pkg.GetTypes().Name()
}

// qualifyExpr applies the "pkgName." prefix to any *ast.Ident in the expr.
func qualifyExpr(expr ast.Expr, pkgName string) ast.Expr {
	if pkgName == "" {
		return expr
	}
	ast.Inspect(expr, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ArrayType, *ast.ChanType, *ast.Ellipsis,
			*ast.FuncType, *ast.MapType, *ast.ParenExpr,
			*ast.StarExpr, *ast.StructType:
			// These are the only types that are cloned by cloneExpr below,
			// so these are the only types that we can traverse and potentially
			// modify. This is not an ideal approach, but it works for now.
			return true
		case *ast.SelectorExpr:
			// Don't add qualifiers to selectors.
			return false
		case *ast.Ident:
			// Only add the qualifier if the identifier is exported.
			if unicode.IsUpper(rune(n.Name[0])) {
				n.Name = pkgName + "." + n.Name
			}
		}
		return false
	})
	return expr
}

// cloneExpr only clones expressions that appear in the parameters or return
// values of a function declaration. The original expression may be returned
// to the caller in 2 cases:
//    (1) The expression has no pointer fields.
//    (2) The expression cannot appear in an *ast.FuncType, making it
//        unnecessary to clone.
// NOTE: This function is tailored to the use case of qualifyExpr, and should
// be used with caution.
func cloneExpr(expr ast.Expr) ast.Expr {
	switch expr := expr.(type) {
	case *ast.ArrayType:
		return &ast.ArrayType{
			Lbrack: expr.Lbrack,
			Elt:    cloneExpr(expr.Elt),
			Len:    expr.Len,
		}
	case *ast.ChanType:
		return &ast.ChanType{
			Arrow: expr.Arrow,
			Begin: expr.Begin,
			Dir:   expr.Dir,
			Value: cloneExpr(expr.Value),
		}
	case *ast.Ellipsis:
		return &ast.Ellipsis{
			Ellipsis: expr.Ellipsis,
			Elt:      cloneExpr(expr.Elt),
		}
	case *ast.FuncType:
		return &ast.FuncType{
			Func:    expr.Func,
			Params:  cloneFieldList(expr.Params),
			Results: cloneFieldList(expr.Results),
		}
	case *ast.Ident:
		return cloneIdent(expr)
	case *ast.MapType:
		return &ast.MapType{
			Map:   expr.Map,
			Key:   cloneExpr(expr.Key),
			Value: cloneExpr(expr.Value),
		}
	case *ast.ParenExpr:
		return &ast.ParenExpr{
			Lparen: expr.Lparen,
			Rparen: expr.Rparen,
			X:      cloneExpr(expr.X),
		}
	case *ast.SelectorExpr:
		return &ast.SelectorExpr{
			Sel: cloneIdent(expr.Sel),
			X:   cloneExpr(expr.X),
		}
	case *ast.StarExpr:
		return &ast.StarExpr{
			Star: expr.Star,
			X:    cloneExpr(expr.X),
		}
	case *ast.StructType:
		return &ast.StructType{
			Struct:     expr.Struct,
			Fields:     cloneFieldList(expr.Fields),
			Incomplete: expr.Incomplete,
		}
	default:
		// Not in function literals or don't need to be cloned.
		return expr
	}
}

func cloneFieldList(fl *ast.FieldList) *ast.FieldList {
	if fl == nil {
		return nil
	}
	if fl.List == nil {
		return &ast.FieldList{
			Closing: fl.Closing,
			Opening: fl.Opening,
		}
	}
	list := make([]*ast.Field, 0, len(fl.List))
	for _, f := range fl.List {
		var names []*ast.Ident
		for _, n := range f.Names {
			names = append(names, cloneIdent(n))
		}
		list = append(list, &ast.Field{
			Comment: f.Comment,
			Doc:     f.Doc,
			Names:   names,
			Tag:     f.Tag,
			Type:    cloneExpr(f.Type),
		})
	}
	return &ast.FieldList{
		Closing: fl.Closing,
		Opening: fl.Opening,
		List:    list,
	}
}

func cloneIdent(ident *ast.Ident) *ast.Ident {
	return &ast.Ident{
		NamePos: ident.NamePos,
		Name:    ident.Name,
		Obj:     ident.Obj,
	}
}