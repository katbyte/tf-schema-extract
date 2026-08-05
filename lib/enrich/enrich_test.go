package enrich

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const testProviderSrc = `package things

import (
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
)

func resourceThingSchema() map[string]*schema.Schema {
	return map[string]*schema.Schema{
		"port": {
			Type:         schema.TypeInt,
			Optional:     true,
			ValidateFunc: validation.IntBetween(1, 65535),
		},
		"kind": {
			Type:         schema.TypeString,
			Required:     true,
			ValidateFunc: validation.StringInSlice([]string{"alpha", "beta"}, false),
		},
		"count": {
			Type:         schema.TypeInt,
			Optional:     true,
			ValidateFunc: validation.IntBetween(0, 10),
		},
	}
}

func PortNumber() schema.SchemaValidateFunc {
	return validation.IntAtLeast(1024)
}
`

func as[T any](t *testing.T, v any) T {
	t.Helper()
	tv, ok := v.(T)
	if !ok {
		t.Fatalf("unexpected type %T", v)
	}
	return tv
}

func testPayload() json.RawMessage {
	return json.RawMessage(`{
		"resources": {
			"test_thing": {
				"schema": {
					"port": {
						"type": "TypeInt",
						"validate_func": {
							"name": "example.com/p/internal/things.resourceThingSchema.IntBetween.func1",
							"origin": "example.com/p/internal/things.resourceThingSchema.IntBetween",
							"file": "helper/validation/int.go", "line": 12
						}
					},
					"count": {
						"type": "TypeInt",
						"validate_func": {
							"name": "example.com/p/internal/things.resourceThingSchema.IntBetween.func2",
							"origin": "example.com/p/internal/things.resourceThingSchema.IntBetween",
							"file": "helper/validation/int.go", "line": 12
						}
					},
					"kind": {
						"type": "TypeString",
						"validate_func": {
							"name": "github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation.StringInSlice.func1",
							"origin": "github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation.StringInSlice",
							"file": "helper/validation/strings.go", "line": 30
						}
					},
					"limit": {
						"type": "TypeInt",
						"validate_func": {
							"name": "example.com/p/internal/things.PortNumber.IntAtLeast.func1",
							"origin": "example.com/p/internal/things.PortNumber.IntAtLeast",
							"file": "helper/validation/int.go", "line": 40
						}
					}
				}
			}
		}
	}`)
}

func TestPayload(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	dir := filepath.Join(root, "internal", "things")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/p\n\ngo 1.21\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "thing.go"), []byte(testProviderSrc), 0o600); err != nil {
		t.Fatal(err)
	}

	out, stats, err := Payload(testPayload(), root, "example.com/p")
	if err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}

	call := func(name string) map[string]any {
		t.Helper()
		s := as[map[string]any](t, as[map[string]any](t, as[map[string]any](t, doc["resources"])["test_thing"])["schema"])
		vf := as[map[string]any](t, as[map[string]any](t, s[name])["validate_func"])
		c, ok := vf["call"].(map[string]any)
		if !ok {
			t.Fatalf("attribute %s: no call attached: %v", name, vf)
		}
		return c
	}
	args := func(name string) []any {
		t.Helper()
		return as[[]any](t, call(name)["args"])
	}

	// two IntBetween calls in the same function disambiguated by attribute key
	if a := args("port"); a[0] != 1.0 || a[1] != 65535.0 {
		t.Errorf("port args: %v", a)
	}
	if a := args("count"); a[0] != 0.0 || a[1] != 10.0 {
		t.Errorf("count args: %v", a)
	}

	// caller-less origin resolved via the repo-wide index + attribute key
	kindArgs := args("kind")
	if list := as[[]any](t, kindArgs[0]); list[0] != "alpha" || list[1] != "beta" {
		t.Errorf("kind list: %v", list)
	}
	if kindArgs[1] != false {
		t.Errorf("kind ignoreCase: %v", kindArgs[1])
	}

	// wrapper constructor matched via its return statement
	if a := args("limit"); a[0] != 1024.0 {
		t.Errorf("limit args: %v", a)
	}

	if stats.Matched != 4 {
		t.Errorf("stats: %+v", stats)
	}
}
