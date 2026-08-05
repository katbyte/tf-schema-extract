package src

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestModule(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, content := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestFindEntry(t *testing.T) {
	t.Parallel()

	root := writeTestModule(t, map[string]string{
		"go.mod": "module example.com/terraform-provider-test\n\ngo 1.21\n",
		"internal/provider/provider.go": `package provider

import "github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"

func TestProvider() *schema.Provider { return nil }

func AzureProvider() *schema.Provider { return nil }

func notExported() *schema.Provider { return nil }

func WithArgs(s string) *schema.Provider { return nil }
`,
		// aliased schema import must still be detected
		"other/other.go": `package other

import sdkschema "github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"

func OtherThing() *sdkschema.Provider { return nil }
`,
		// vendor dir must be skipped
		"vendor/example.com/x/x.go": `package x

import "github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"

func Provider() *schema.Provider { return nil }
`,
	})

	moduleRoot, modPath, err := FindModule(filepath.Join(root, "internal"))
	if err != nil {
		t.Fatalf("FindModule: %v", err)
	}
	if moduleRoot != root {
		t.Errorf("module root: got %s, want %s", moduleRoot, root)
	}
	if modPath != "example.com/terraform-provider-test" {
		t.Errorf("module path: got %s", modPath)
	}

	entry, err := findEntry(root, modPath)
	if err != nil {
		t.Fatalf("findEntry: %v", err)
	}

	// AzureProvider outranks TestProvider (Test* prefix) and OtherThing (no Provider suffix);
	// notExported/WithArgs/vendor funcs are not candidates at all
	want := "example.com/terraform-provider-test/internal/provider.AzureProvider"
	if entry.String() != want {
		t.Errorf("entry: got %s, want %s", entry, want)
	}
}

func TestFindEntryNone(t *testing.T) {
	t.Parallel()

	root := writeTestModule(t, map[string]string{
		"go.mod":  "module example.com/empty\n\ngo 1.21\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})

	if _, err := findEntry(root, "example.com/empty"); err == nil {
		t.Fatal("expected error for module with no provider entry point")
	}
}
