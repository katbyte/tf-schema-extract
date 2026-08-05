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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
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

// Extract locates the provider entry point in the repo at path (or uses the explicit
// "import/path.Func" entry override), generates the reflection shim, runs it with the
// provider's own module context, and returns the raw JSON payload plus the entry used.
func Extract(ctx context.Context, repoPath, entryOverride string, keepShim bool) (json.RawMessage, Entry, error) {
	root, modPath, err := FindModule(repoPath)
	if err != nil {
		return nil, Entry{}, err
	}
	cout.Printf("  module <cyan>%s</> at <gray>%s</>\n", modPath, root)

	var entry Entry
	if entryOverride != "" {
		i := strings.LastIndex(entryOverride, ".")
		if i < 1 || i == len(entryOverride)-1 {
			return nil, Entry{}, fmt.Errorf("invalid --entry %q, expected import/path.FuncName", entryOverride)
		}
		entry = Entry{Package: entryOverride[:i], Func: entryOverride[i+1:]}
		if err := resolveEntry(root, modPath, &entry); err != nil {
			return nil, Entry{}, err
		}
	} else {
		entry, err = findEntry(root, modPath)
		if err != nil {
			return nil, Entry{}, err
		}
	}
	cout.Printf("  entry <green>%s</>\n", entry)

	payload, err := runShim(ctx, root, entry, keepShim)
	if err != nil {
		return nil, entry, err
	}

	return payload, entry, nil
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
func runShim(ctx context.Context, root string, entry Entry, keepShim bool) (json.RawMessage, error) {
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
