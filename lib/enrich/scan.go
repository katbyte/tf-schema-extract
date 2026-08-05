package enrich

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// argsKey is the JSON key call arguments are emitted under.
const argsKey = "args"

// callSite is one constructor call found in a schema-relevant position, e.g. the
// validation.IntBetween(1, 65535) inside ValidateFunc: validation.IntBetween(1, 65535).
type callSite struct {
	PkgPath       string // import path of the package the call appears in
	File          string // repo-relative file
	Line          int
	Callee        string // short func name being called, e.g. IntBetween
	EnclosingFunc string // named function the call appears in
	Receiver      string // receiver type name when EnclosingFunc is a method
	Field         string // schema field the call value feeds: ValidateFunc, DefaultFunc, ... or "return"
	AttrKey       string // nearest enclosing string map key, i.e. the attribute name
	Args          []any
}

// schemaFields are the schema.Schema func-valued fields whose call expressions are worth
// indexing; a call in a return statement is indexed too to catch wrapper constructors.
var schemaFields = map[string]bool{
	"ValidateFunc": true, "ValidateDiagFunc": true, "DefaultFunc": true,
	"DiffSuppressFunc": true, "StateFunc": true, "Set": true,
}

// scanMarkers pre-filters files: no marker bytes means nothing indexable inside. Wrapper
// constructors (func X() pluginsdk.SchemaValidateFunc { return validation.IntBetween(...) })
// are still caught because their signatures contain a *Func marker.
var scanMarkers = [][]byte{
	[]byte("ValidateFunc"), []byte("ValidateDiagFunc"), []byte("DefaultFunc"),
	[]byte("DiffSuppressFunc"), []byte("StateFunc"), []byte("Set:"),
}

// index holds every discovered call site keyed by short callee name.
type index struct {
	root    string
	modPath string

	mu           sync.Mutex
	byCallee     map[string][]*callSite
	extraPkgs    map[string]bool // vendor packages scanned on demand
	filesScanned int
}

// buildIndex scans every module .go file (skipping vendor, which is scanned lazily per
// package as origins reference it).
func buildIndex(root, modPath string) (*index, error) {
	ix := &index{
		root:      root,
		modPath:   modPath,
		byCallee:  map[string][]*callSite{},
		extraPkgs: map[string]bool{},
	}

	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == "testdata" || name == shimIgnoreDir || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// parse in parallel, the pre-filter makes most files a single read
	var wg sync.WaitGroup
	work := make(chan string)
	for range 8 {
		wg.Go(func() {
			for path := range work {
				ix.scanFile(path)
			}
		})
	}
	for _, f := range files {
		work <- f
	}
	close(work)
	wg.Wait()

	return ix, nil
}

// shimIgnoreDir keeps a leftover --keep-shim dir out of the scan.
const shimIgnoreDir = "tfschemaex-shim"

// ensurePkg lazily scans a package outside the module (vendored dependencies like
// go-azure-helpers commonschema) when an origin references it.
func (ix *index) ensurePkg(pkgPath string) {
	ix.mu.Lock()
	if strings.HasPrefix(pkgPath+"/", ix.modPath+"/") || ix.extraPkgs[pkgPath] {
		ix.mu.Unlock()
		return
	}
	ix.extraPkgs[pkgPath] = true
	ix.mu.Unlock()

	dir := filepath.Join(ix.root, "vendor", filepath.FromSlash(pkgPath))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // not vendored, nothing we can do
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			ix.scanFile(filepath.Join(dir, e.Name()))
		}
	}
}

// pkgPathOf maps a file's directory to its import path: module-relative for module
// files, the vendored path for vendor files.
func (ix *index) pkgPathOf(path string) string {
	rel, err := filepath.Rel(ix.root, filepath.Dir(path))
	if err != nil {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if after, ok := strings.CutPrefix(rel, "vendor/"); ok {
		return after
	}
	if rel == "." {
		return ix.modPath
	}
	return ix.modPath + "/" + rel
}

func (ix *index) scanFile(path string) {
	src, err := os.ReadFile(path) //nolint:gosec // scanning the user-supplied repo is the entire point
	if err != nil {
		return
	}

	interesting := false
	for _, m := range scanMarkers {
		if bytes.Contains(src, m) {
			interesting = true
			break
		}
	}
	if !interesting {
		return
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return
	}

	pkgPath := ix.pkgPathOf(path)
	relFile, _ := filepath.Rel(ix.root, path)
	relFile = filepath.ToSlash(relFile)

	var sites []*callSite
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)

		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		var callee string
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			callee = fn.Sel.Name
		case *ast.Ident:
			callee = fn.Name
		default:
			return true
		}

		var decl *ast.FuncDecl
		for _, sn := range stack {
			if fd, ok := sn.(*ast.FuncDecl); ok {
				decl = fd
				break
			}
		}

		field, attrKey := "", ""
		for i := len(stack) - 2; i >= 0; i-- {
			switch p := stack[i].(type) {
			case *ast.KeyValueExpr:
				if id, ok := p.Key.(*ast.Ident); ok && field == "" && schemaFields[id.Name] {
					field = id.Name
				}
				if lit, ok := p.Key.(*ast.BasicLit); ok && attrKey == "" && lit.Kind == token.STRING {
					attrKey, _ = strconv.Unquote(lit.Value)
				}
			case *ast.ReturnStmt:
				// only index return-position calls in wrapper constructors, i.e. funcs
				// returning a func type - otherwise every return foo() would be indexed
				if field == "" && returnsFuncType(decl) {
					field = "return"
				}
			}
		}
		if field == "" {
			return true // not in a schema-relevant position, keep the index small
		}

		enclosing, receiver := "", ""
		if decl != nil {
			enclosing = decl.Name.Name
			receiver = receiverName(decl)
		}

		sites = append(sites, &callSite{
			PkgPath:       pkgPath,
			File:          relFile,
			Line:          fset.Position(call.Pos()).Line,
			Callee:        callee,
			EnclosingFunc: enclosing,
			Receiver:      receiver,
			Field:         field,
			AttrKey:       attrKey,
			Args:          evalArgs(call.Args, fset, src),
		})
		return true
	})

	if len(sites) == 0 {
		return
	}

	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.filesScanned++
	for _, s := range sites {
		ix.byCallee[s.Callee] = append(ix.byCallee[s.Callee], s)
	}
}

// returnsFuncType reports whether the func returns a single func-shaped value: either a
// literal func type or a named type ending in Func (schema.SchemaValidateFunc, ...).
func returnsFuncType(fd *ast.FuncDecl) bool {
	if fd == nil || fd.Type.Results == nil || len(fd.Type.Results.List) != 1 {
		return false
	}
	switch t := fd.Type.Results.List[0].Type.(type) {
	case *ast.FuncType:
		return true
	case *ast.Ident:
		return strings.HasSuffix(t.Name, "Func")
	case *ast.SelectorExpr:
		return strings.HasSuffix(t.Sel.Name, "Func")
	}
	return false
}

func receiverName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	t := fd.Recv.List[0].Type
	for {
		switch tt := t.(type) {
		case *ast.StarExpr:
			t = tt.X
		case *ast.IndexExpr:
			t = tt.X
		case *ast.Ident:
			return tt.Name
		default:
			return ""
		}
	}
}

// evalArgs turns call arguments into JSON-friendly values: literals become native
// values, nested constructor calls become {func, args}, and anything else falls back to
// its source text as {expr}.
func evalArgs(args []ast.Expr, fset *token.FileSet, src []byte) []any {
	out := make([]any, 0, len(args))
	for _, a := range args {
		out = append(out, evalExpr(a, fset, src))
	}
	return out
}

func evalExpr(e ast.Expr, fset *token.FileSet, src []byte) any {
	switch v := e.(type) {
	case *ast.BasicLit:
		switch v.Kind {
		case token.INT:
			if i, err := strconv.ParseInt(v.Value, 0, 64); err == nil {
				return i
			}
		case token.FLOAT:
			if f, err := strconv.ParseFloat(v.Value, 64); err == nil {
				return f
			}
		case token.STRING, token.CHAR:
			if s, err := strconv.Unquote(v.Value); err == nil {
				return s
			}
		default:
		}
	case *ast.Ident:
		switch v.Name {
		case "true":
			return true
		case "false":
			return false
		case "nil":
			return nil
		}
	case *ast.UnaryExpr:
		if v.Op == token.SUB {
			switch inner := evalExpr(v.X, fset, src).(type) {
			case int64:
				return -inner
			case float64:
				return -inner
			}
		}
	case *ast.CompositeLit:
		// []string{...} and friends: only fold when every element is a plain value
		vals := make([]any, 0, len(v.Elts))
		for _, el := range v.Elts {
			ev := evalExpr(el, fset, src)
			if m, isMap := ev.(map[string]any); isMap {
				if _, isExpr := m["expr"]; isExpr {
					return exprText(e, fset, src)
				}
			}
			vals = append(vals, ev)
		}
		return vals
	case *ast.CallExpr:
		callee := ""
		switch fn := v.Fun.(type) {
		case *ast.SelectorExpr:
			callee = fn.Sel.Name
		case *ast.Ident:
			callee = fn.Name
		}
		if callee != "" {
			return map[string]any{"func": callee, argsKey: evalArgs(v.Args, fset, src)}
		}
	}
	return exprText(e, fset, src)
}

func exprText(e ast.Expr, fset *token.FileSet, src []byte) map[string]any {
	start := fset.Position(e.Pos()).Offset
	end := fset.Position(e.End()).Offset
	if start < 0 || end > len(src) || start >= end {
		return map[string]any{"expr": "?"}
	}
	return map[string]any{"expr": string(src[start:end])}
}
