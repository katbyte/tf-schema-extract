// Package src extracts a provider's full SDK-level schema from its source checkout.
//
// The plugin protocol never exposes ForceNew, defaults, validators, ConflictsWith or
// source locations, so instead of speaking the protocol this mode compiles a small
// generated shim *inside* the provider's own module (so its go.mod/vendor are used),
// calls the provider's entry func (e.g. provider.AzureProvider()) in-process, and walks
// the live *schema.Provider with reflection. runtime.FuncForPC turns every CRUD,
// validate, default and diff-suppress func into a name + file:line, which is how the
// dump gets per-resource filenames without any SSA.
package src

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/katbyte/tfschemaex/lib/cout"
	"golang.org/x/mod/modfile"
)

// Entry identifies a provider constructor function. Supported signatures are any
// combination of an optional context.Context parameter and an optional error result
// around *schema.Provider - e.g. azurerm's AzureProvider() *schema.Provider and aws's
// NewProvider(ctx) (*schema.Provider, error).
type Entry struct {
	Package    string // full import path, e.g. github.com/hashicorp/terraform-provider-azurerm/internal/provider
	Func       string // e.g. AzureProvider
	File       string // repo-relative file the func was found in
	TakesCtx   bool   // func takes a context.Context
	ReturnsErr bool   // func returns (*schema.Provider, error)
}

func (e Entry) String() string {
	return e.Package + "." + e.Func
}

// FrameworkEntry identifies a plugin-framework provider constructor in a muxed
// provider - e.g. aws's NewProvider(ctx, primary) (provider.Provider, error) or
// azurerm's NewFrameworkProvider(primary). Params records each parameter as "ctx"
// (a context.Context) or "primary" (anything else - the SDKv2 provider satisfies the
// anonymous interface{ Meta() any } both aws and azurerm use).
type FrameworkEntry struct {
	Package    string
	Func       string
	File       string
	Params     []string
	ReturnsErr bool
}

func (e FrameworkEntry) String() string {
	return e.Package + "." + e.Func
}

// Extract locates the provider entry point in the repo at path (or uses the explicit
// "import/path.Func" entry override), generates the reflection shim, runs it with the
// provider's own module context, and returns the raw JSON payload plus the entries
// used. When the module depends on terraform-plugin-framework, the framework provider
// constructor is located too and the shim dumps the framework side of the mux as well.
func Extract(ctx context.Context, repoPath, entryOverride, fwEntryOverride string, keepShim bool) (json.RawMessage, Entry, *FrameworkEntry, error) {
	root, modPath, err := FindModule(repoPath)
	if err != nil {
		return nil, Entry{}, nil, err
	}
	cout.Printf("  module <cyan>%s</> at <gray>%s</>\n", modPath, root)

	var entry Entry
	if entryOverride != "" {
		i := strings.LastIndex(entryOverride, ".")
		if i < 1 || i == len(entryOverride)-1 {
			return nil, Entry{}, nil, fmt.Errorf("invalid --entry %q, expected import/path.FuncName", entryOverride)
		}
		entry = Entry{Package: entryOverride[:i], Func: entryOverride[i+1:]}
		if err := resolveEntry(root, modPath, &entry); err != nil {
			return nil, Entry{}, nil, err
		}
	} else {
		entry, err = findEntry(root, modPath)
		if err != nil {
			return nil, Entry{}, nil, err
		}
	}
	cout.Printf("  entry <green>%s</>\n", entry)

	var fwEntry *FrameworkEntry
	if hasFrameworkDep(root) {
		if fe, ferr := findFrameworkEntry(root, modPath, fwEntryOverride); ferr == nil {
			fwEntry = &fe
			cout.Printf("  framework entry <green>%s</>\n", fe)
		} else {
			cout.Printf("  <yellow>module uses terraform-plugin-framework but no entry point was found (%v) - framework schemas skipped, use --framework-entry</>\n", ferr)
		}
	}

	payload, err := runShim(ctx, root, entry, fwEntry, keepShim)
	if err != nil {
		return nil, entry, fwEntry, err
	}

	return payload, entry, fwEntry, nil
}

// hasFrameworkDep reports whether the module requires terraform-plugin-framework.
func hasFrameworkDep(root string) bool {
	data, err := os.ReadFile(filepath.Join(root, "go.mod")) //nolint:gosec // reading the user-supplied repo's go.mod is the entire point
	if err != nil {
		return false
	}
	mf, err := modfile.ParseLax("go.mod", data, nil)
	if err != nil {
		return false
	}
	for _, r := range mf.Require {
		if r.Mod.Path == "github.com/hashicorp/terraform-plugin-framework" {
			return true
		}
	}
	return false
}

// FindModule walks up from path until it finds a go.mod and returns the module root and path.
func FindModule(path string) (root, modPath string, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("resolving %s: %w", path, err)
	}

	for dir := abs; ; dir = filepath.Dir(dir) {
		gomod := filepath.Join(dir, "go.mod")
		if data, rerr := os.ReadFile(gomod); rerr == nil { //nolint:gosec // walking up the user-supplied repo path for its go.mod
			mf, perr := modfile.ParseLax(gomod, data, nil)
			if perr != nil {
				return "", "", fmt.Errorf("parsing %s: %w", gomod, perr)
			}
			return dir, mf.Module.Mod.Path, nil
		}
		if dir == filepath.Dir(dir) {
			return "", "", fmt.Errorf("no go.mod found in or above %s", abs)
		}
	}
}

// findEntry scans the module for zero-arg functions returning *schema.Provider where
// schema resolves to the terraform-plugin-sdk helper/schema package. When there are
// multiple candidates it prefers ones named Provider/*Provider and skips Test* funcs.
func findEntry(root, modPath string) (Entry, error) {
	var candidates []Entry

	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == "testdata" || name == ".git" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return nil //nolint:nilerr // skip unparsable files, they can't contain our entry point
		}

		// find what the helper/schema package is imported as in this file
		schemaAlias := ""
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.HasSuffix(p, "terraform-plugin-sdk/v2/helper/schema") || strings.HasSuffix(p, "terraform-plugin-sdk/helper/schema") {
				schemaAlias = "schema"
				if imp.Name != nil {
					schemaAlias = imp.Name.Name
				}
			}
		}
		if schemaAlias == "" || schemaAlias == "_" {
			return nil
		}

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() {
				continue
			}
			takesCtx, returnsErr, ok := entrySignature(fn, schemaAlias)
			if !ok {
				continue
			}

			rel, _ := filepath.Rel(root, path)
			pkgPath := modPath
			if dir := filepath.Dir(rel); dir != "." {
				pkgPath = modPath + "/" + filepath.ToSlash(dir)
			}
			candidates = append(candidates, Entry{Package: pkgPath, Func: fn.Name.Name, File: rel, TakesCtx: takesCtx, ReturnsErr: returnsErr})
		}

		return nil
	})
	if err != nil {
		return Entry{}, fmt.Errorf("scanning %s for provider entry point: %w", root, err)
	}

	if len(candidates) == 0 {
		return Entry{}, fmt.Errorf("no zero-arg func returning *schema.Provider found in %s (use --entry import/path.FuncName)", root)
	}

	// rank: non-Test funcs named exactly Provider first, then *Provider, then the rest
	sort.SliceStable(candidates, func(i, j int) bool { return entryRank(candidates[i]) < entryRank(candidates[j]) })

	if len(candidates) > 1 {
		cout.Verbosef("  found <yellow>%d</> entry point candidates:\n", len(candidates))
		for _, c := range candidates {
			cout.Verbosef("    %s <gray>(%s)</>\n", c, c.File)
		}
	}

	return candidates[0], nil
}

// entrySignature reports whether fn is a provider constructor: an optional
// context.Context parameter and *schema.Provider with an optional error result.
func entrySignature(fn *ast.FuncDecl, schemaAlias string) (takesCtx, returnsErr, ok bool) {
	switch fn.Type.Params.NumFields() {
	case 0:
	case 1:
		sel, isSel := fn.Type.Params.List[0].Type.(*ast.SelectorExpr)
		if !isSel || sel.Sel.Name != "Context" {
			return false, false, false
		}
		takesCtx = true
	default:
		return false, false, false
	}

	results := fn.Type.Results
	switch results.NumFields() {
	case 1:
	case 2:
		id, isIdent := results.List[1].Type.(*ast.Ident)
		if !isIdent || id.Name != "error" {
			return false, false, false
		}
		returnsErr = true
	default:
		return false, false, false
	}

	star, isStar := results.List[0].Type.(*ast.StarExpr)
	if !isStar {
		return false, false, false
	}
	sel, isSel := star.X.(*ast.SelectorExpr)
	if !isSel || sel.Sel.Name != "Provider" {
		return false, false, false
	}
	pkgIdent, isIdent := sel.X.(*ast.Ident)
	if !isIdent || pkgIdent.Name != schemaAlias {
		return false, false, false
	}

	return takesCtx, returnsErr, true
}

// resolveEntry fills in the signature details (and file) for an explicit --entry
// override by parsing the named package.
func resolveEntry(root, modPath string, entry *Entry) error {
	if entry.Package != modPath && !strings.HasPrefix(entry.Package, modPath+"/") {
		return fmt.Errorf("--entry package %s is not part of module %s", entry.Package, modPath)
	}
	dir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(entry.Package, modPath), "/")))

	files, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading --entry package dir %s: %w", dir, err)
	}

	fset := token.NewFileSet()
	for _, de := range files {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".go") || strings.HasSuffix(de.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, de.Name())
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			continue
		}

		schemaAlias := ""
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.HasSuffix(p, "terraform-plugin-sdk/v2/helper/schema") || strings.HasSuffix(p, "terraform-plugin-sdk/helper/schema") {
				schemaAlias = "schema"
				if imp.Name != nil {
					schemaAlias = imp.Name.Name
				}
			}
		}
		if schemaAlias == "" || schemaAlias == "_" {
			continue
		}

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != entry.Func {
				continue
			}
			takesCtx, returnsErr, ok := entrySignature(fn, schemaAlias)
			if !ok {
				return fmt.Errorf("--entry %s has an unsupported signature (want [ctx] (*schema.Provider[, error]))", entry)
			}
			entry.TakesCtx, entry.ReturnsErr = takesCtx, returnsErr
			entry.File, _ = filepath.Rel(root, path)
			return nil
		}
	}

	return fmt.Errorf("--entry func %s not found in %s", entry.Func, dir)
}

// findFrameworkEntry scans for exported constructors returning the plugin-framework
// provider.Provider. Parameters are classified as "ctx" (context.Context) or "primary"
// (anything else, passed the SDKv2 provider). With an override of the form
// import/path.FuncName only that func is accepted.
func findFrameworkEntry(root, modPath, override string) (FrameworkEntry, error) {
	wantPkg, wantFunc := "", ""
	if override != "" {
		i := strings.LastIndex(override, ".")
		if i < 1 || i == len(override)-1 {
			return FrameworkEntry{}, fmt.Errorf("invalid --framework-entry %q, expected import/path.FuncName", override)
		}
		wantPkg, wantFunc = override[:i], override[i+1:]
	}

	var candidates []FrameworkEntry

	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == "testdata" || name == ".git" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return nil //nolint:nilerr // skip unparsable files, they can't contain our entry point
		}

		providerAlias := ""
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.HasSuffix(p, "terraform-plugin-framework/provider") {
				providerAlias = "provider"
				if imp.Name != nil {
					providerAlias = imp.Name.Name
				}
			}
		}
		if providerAlias == "" || providerAlias == "_" {
			return nil
		}

		rel, _ := filepath.Rel(root, path)
		pkgPath := modPath
		if dir := filepath.Dir(rel); dir != "." {
			pkgPath = modPath + "/" + filepath.ToSlash(dir)
		}
		if wantPkg != "" && pkgPath != wantPkg {
			return nil
		}

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() {
				continue
			}
			if wantFunc != "" && fn.Name.Name != wantFunc {
				continue
			}

			params, ok := frameworkParams(fn)
			if !ok {
				continue
			}
			returnsErr, ok := frameworkResults(fn, providerAlias)
			if !ok {
				continue
			}

			candidates = append(candidates, FrameworkEntry{
				Package: pkgPath, Func: fn.Name.Name, File: rel,
				Params: params, ReturnsErr: returnsErr,
			})
		}

		return nil
	})
	if err != nil {
		return FrameworkEntry{}, fmt.Errorf("scanning %s for framework entry point: %w", root, err)
	}

	if len(candidates) == 0 {
		if override != "" {
			return FrameworkEntry{}, fmt.Errorf("--framework-entry %s not found or has an unsupported signature", override)
		}
		return FrameworkEntry{}, errors.New("no constructor returning a framework provider.Provider found")
	}

	sort.SliceStable(candidates, func(i, j int) bool { return frameworkRank(candidates[i]) < frameworkRank(candidates[j]) })

	if len(candidates) > 1 {
		cout.Verbosef("  found <yellow>%d</> framework entry point candidates:\n", len(candidates))
		for _, c := range candidates {
			cout.Verbosef("    %s <gray>(%s)</>\n", c, c.File)
		}
	}

	return candidates[0], nil
}

// frameworkParams classifies constructor parameters: context.Context -> "ctx", one
// anything-else -> "primary" (it receives the SDKv2 provider).
func frameworkParams(fn *ast.FuncDecl) ([]string, bool) {
	var params []string
	primaries := 0
	for _, field := range fn.Type.Params.List {
		n := max(len(field.Names), 1)
		for range n {
			if sel, ok := field.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Context" {
				params = append(params, "ctx")
				continue
			}
			primaries++
			params = append(params, "primary")
		}
	}
	if len(params) > 2 || primaries > 1 {
		return nil, false
	}
	return params, true
}

// frameworkResults accepts (provider.Provider) or (provider.Provider, error).
func frameworkResults(fn *ast.FuncDecl, providerAlias string) (returnsErr, ok bool) {
	results := fn.Type.Results
	switch results.NumFields() {
	case 1:
	case 2:
		id, isIdent := results.List[1].Type.(*ast.Ident)
		if !isIdent || id.Name != "error" {
			return false, false
		}
		returnsErr = true
	default:
		return false, false
	}

	sel, isSel := results.List[0].Type.(*ast.SelectorExpr)
	if !isSel || sel.Sel.Name != "Provider" {
		return false, false
	}
	pkgIdent, isIdent := sel.X.(*ast.Ident)
	if !isIdent || pkgIdent.Name != providerAlias {
		return false, false
	}

	return returnsErr, true
}

// frameworkRank prefers constructors wired to the SDKv2 provider (aws's
// NewProvider(ctx, primary), azurerm's NewFrameworkProvider(primary)) over standalone
// ones, and skips Test* helpers.
func frameworkRank(e FrameworkEntry) int {
	switch {
	case strings.HasPrefix(e.Func, "Test"):
		return 3
	case slices.Contains(e.Params, "primary"):
		return 0
	default:
		return 1
	}
}

func entryRank(e Entry) int {
	switch {
	case strings.HasPrefix(e.Func, "Test"):
		return 3
	case e.Func == "Provider":
		return 0
	case strings.HasSuffix(e.Func, "Provider"):
		return 1
	default:
		return 2
	}
}

// runShim writes the generated shim package into the module root (so imports resolve
// against the provider's go.mod and vendor directory), runs it, and returns its JSON.
func runShim(ctx context.Context, root string, entry Entry, fwEntry *FrameworkEntry, keepShim bool) (json.RawMessage, error) {
	shimDir := filepath.Join(root, shimDirName)
	if err := os.MkdirAll(shimDir, 0o750); err != nil {
		return nil, fmt.Errorf("creating shim dir %s: %w", shimDir, err)
	}
	if !keepShim {
		defer func() { _ = os.RemoveAll(shimDir) }()
	}

	call, ctxImport := "p := entry."+entry.Func+"()", ""
	arg := ""
	if entry.TakesCtx {
		arg, ctxImport = "context.Background()", "\"context\"\n"
	}
	if entry.ReturnsErr {
		call = "p, err := entry." + entry.Func + "(" + arg + ")\n" +
			"\tif err != nil {\n" +
			"\t\tfmt.Fprintf(os.Stderr, \"shim: constructing provider: %v\\n\", err)\n" +
			"\t\tos.Exit(1)\n" +
			"\t}"
	} else if entry.TakesCtx {
		call = "p := entry." + entry.Func + "(" + arg + ")"
	}

	shimSrc := strings.NewReplacer(
		"__ENTRY_PACKAGE__", entry.Package,
		"__IMPORT_CONTEXT__", ctxImport,
		"__ENTRY_CALL__", call,
	).Replace(shimTemplate)

	if err := os.WriteFile(filepath.Join(shimDir, "main.go"), []byte(shimSrc), 0o644); err != nil { //nolint:gosec // generated source, not sensitive
		return nil, fmt.Errorf("writing shim: %w", err)
	}

	fwSrc := shimFrameworkStub
	if fwEntry != nil {
		args := make([]string, 0, len(fwEntry.Params))
		for _, p := range fwEntry.Params {
			if p == "ctx" {
				args = append(args, "context.Background()")
			} else {
				args = append(args, "p")
			}
		}
		fwCall := "fwp := fwentry." + fwEntry.Func + "(" + strings.Join(args, ", ") + ")"
		if fwEntry.ReturnsErr {
			fwCall = "fwp, err := fwentry." + fwEntry.Func + "(" + strings.Join(args, ", ") + ")\n" +
				"\tif err != nil {\n" +
				"\t\tfmt.Fprintf(os.Stderr, \"shim: constructing framework provider: %v\\n\", err)\n" +
				"\t\treturn nil\n" +
				"\t}"
		}
		fwSrc = strings.NewReplacer(
			"__FW_ENTRY_PACKAGE__", fwEntry.Package,
			"__FW_CALL__", fwCall,
		).Replace(shimFrameworkTemplate)
	}
	if err := os.WriteFile(filepath.Join(shimDir, "framework.go"), []byte(fwSrc), 0o644); err != nil { //nolint:gosec // generated source, not sensitive
		return nil, fmt.Errorf("writing framework shim: %w", err)
	}

	outFile, err := os.CreateTemp("", "tfschemaex-src-*.json")
	if err != nil {
		return nil, fmt.Errorf("creating shim output file: %w", err)
	}
	outPath := outFile.Name()
	_ = outFile.Close()
	defer func() { _ = os.Remove(outPath) }()

	cout.Printf("  compiling and running shim <gray>(first run against a large provider can take a few minutes)</>...\n")
	cmd := exec.CommandContext(ctx, "go", "run", "./"+shimDirName, outPath) //nolint:gosec // running the generated shim inside the user-supplied repo is the entire point
	cmd.Dir = root
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stderr // provider init noise must not pollute our stdout JSON
	if err := cmd.Run(); err != nil {
		if keepShim {
			cout.Printf("  shim kept at <gray>%s</>\n", shimDir)
		}
		return nil, fmt.Errorf("running schema shim (go run ./%s): %w", shimDirName, err)
	}

	payload, err := os.ReadFile(outPath) //nolint:gosec // reading back the temp file we created above
	if err != nil {
		return nil, fmt.Errorf("reading shim output: %w", err)
	}
	if !json.Valid(payload) {
		return nil, fmt.Errorf("shim produced invalid JSON (%d bytes)", len(payload))
	}

	return payload, nil
}
