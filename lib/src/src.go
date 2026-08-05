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

// Entry identifies a zero-argument function returning *schema.Provider.
type Entry struct {
	Package string // full import path, e.g. github.com/hashicorp/terraform-provider-azurerm/internal/provider
	Func    string // e.g. AzureProvider
	File    string // repo-relative file the func was found in
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
			if fn.Type.Params.NumFields() != 0 || fn.Type.Results.NumFields() != 1 {
				continue
			}
			star, ok := fn.Type.Results.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			sel, ok := star.X.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Provider" {
				continue
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok || pkgIdent.Name != schemaAlias {
				continue
			}

			rel, _ := filepath.Rel(root, path)
			pkgPath := modPath
			if dir := filepath.Dir(rel); dir != "." {
				pkgPath = modPath + "/" + filepath.ToSlash(dir)
			}
			candidates = append(candidates, Entry{Package: pkgPath, Func: fn.Name.Name, File: rel})
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

	shimSrc := strings.NewReplacer(
		"__ENTRY_PACKAGE__", entry.Package,
		"__ENTRY_FUNC__", entry.Func,
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
