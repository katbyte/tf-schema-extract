# tfschemaex

A tool — and an experiment: **how much information about a terraform provider is
available depending on how you analyze it?**

The same provider can be interrogated three ways, each seeing strictly more than the
last, and tfschemaex implements all three so the difference can be measured directly:

1. **`plugin` mode — ask the provider over the wire.** Launches any compiled provider
   binary exactly like terraform does (go-plugin handshake, AutoMTLS, gRPC protocol 5/6
   negotiation) and dumps everything the plugin protocol exposes. This is the ceiling of
   what terraform itself can ever know.
2. **`src` mode — run the provider's schema code in-process.** Points at a source
   checkout, auto-detects the entry point, generates a shim inside the provider module,
   compiles it with the provider's own go.mod/vendor, and walks the live
   `*schema.Provider` with reflection. Sees everything the SDK knows — ForceNew,
   defaults, validators, conflicts — plus the source file:line of every schema function
   via `runtime.FuncForPC`.
3. **`src --ssa` — read the source itself.** A static call-site analysis pass on top of
   src mode that recovers the one thing reflection cannot see: the literal arguments
   captured inside validator/default closures — the `1, 100` in
   `validation.IntBetween(1, 100)`.

```sh
tfschemaex plugin ~/go/bin/terraform-provider-azurerm   # -> results/azurerm.<date>.binary.json
tfschemaex src ~/hashi/azure/azurerm                    # -> results/azurerm.<date>.src.json
tfschemaex src ~/hashi/azure/azurerm --ssa              # -> results/azurerm.<date>.src-ssa.json
```

Dumps land in a self-gitignored `results/` directory by default (override with `-o`,
`-o -` for stdout).

## what each rung of the ladder sees

The terraform plugin protocol only carries what terraform core needs to plan; a lot of
schema detail is enforced provider-side and **never crosses the wire** — which is why
azurerm's own release process uses an in-process dumper (`internal/tools/schema-api`)
rather than the protocol.

| detail                                                        | plugin | src | src --ssa |
|---------------------------------------------------------------|:------:|:---:|:---------:|
| resources / data sources / attributes / blocks / types        | ✔      | ✔   | ✔         |
| required / optional / computed / sensitive / deprecated       | ✔      | ✔   | ✔         |
| ephemeral + list resources, actions, provider functions       | ✔      | ✔   | ✔         |
| resource identity schemas                                     | ✔      | ✔   | ✔         |
| plugin-framework resources (muxed providers)                  | ✔      | ✔   | ✔         |
| framework validators / defaults / plan modifiers              |        | ✔   | ✔         |
| **ForceNew**                                                  |        | ✔   | ✔         |
| defaults + DefaultFunc                                        |        | ✔   | ✔         |
| which validator is attached (name + origin + file:line)       |        | ✔   | ✔         |
| ConflictsWith / ExactlyOneOf / AtLeastOneOf / RequiredWith    |        | ✔   | ✔         |
| DiffSuppressFunc, StateFunc, Set, CustomizeDiff               |        | ✔   | ✔         |
| CRUD/validate func names + source file:line                   |        | ✔   | ✔         |
| timeouts, state upgraders, importer                           |        | ✔   | ✔         |
| **literal validator/default args** (`IntBetween(1, 100)`)     |        |     | ✔         |
| attribute declaration call site (file:line)                   |        |     | ✔         |

For the complete picture of a provider, run plugin mode and `src --ssa` and join on
resource name.

## measured: three providers × three modes

Snapshot from 2026-08-05: azuread and azurerm (SDKv2 with a small framework side) and
aws (heavily muxed), extracted with all three modes. Counts below are managed-resource
attributes unless noted; "—" means the mode cannot see that fact at all.

### azuread

| | plugin | src | src --ssa |
|---|---:|---:|---:|
| dump size | 0.2 MB | 0.6 MB | 0.6 MB |
| resources / data sources | 35 / 17 | 54 / 20 | 54 / 20 |
| attributes with ForceNew | — | 130 | 130 |
| attributes with validators | — | 332 | 332 |
| └ literal args recovered | — | — | 132 + 6 candidate |
| defaults / constraint groups / diff-suppress | — | 37 / 57 / 19 | 37 / 57 / 19 |
| CRUD funcs with file:line | — | 54 | 54 |

The 35 vs 54 resource gap is not a mode difference — the installed binary predates the
checkout. It illustrates what each mode actually measures: plugin mode describes **the
artifact you ship**, src mode describes **the code as it is now**.

### azurerm

| | plugin | src | src --ssa |
|---|---:|---:|---:|
| dump size | 6.9 MB | 13.4 MB | 17.2 MB |
| resources / data sources | 1103 / 394 | 1103 / 394 | 1103 / 394 |
| ephemeral / list / actions / functions | 2 / 83 / 7 / 2 | 2 / 83 / 7 / 2 | 2 / 83 / 7 / 2 |
| resource identity schemas | 172 | 172 | 172 |
| attributes with ForceNew | — | 5347 | 5347 |
| attributes with validators | — | 12322 | 12322 |
| └ literal args recovered | — | — | 3802 + 299 candidate |
| defaults / constraint groups / diff-suppress | — | 2436 / 1397 / 802 | 2436 / 1397 / 802 |
| CRUD funcs with file:line | — | 1103 | 1103 |

Everything agrees mode-to-mode — and note what the protocol hides: 5,347 ForceNew
markers and 12,322 attached validators exist only below the wire. Only ~8% of azurerm's
attributes carry descriptions at the protocol level (docs live in the website, not the
schema), so for azurerm the schema alone is a poor documentation source in every mode.

### aws

| | plugin | src | src --ssa |
|---|---:|---:|---:|
| dump size | 102 MB | 159 MB | 221 MB |
| resources | 1687 | 1240 sdkv2 + 447 fw = 1687 | 1687 |
| data sources | 673 | 514 sdkv2 + 159 fw = 673 | 673 |
| ephemeral / list / actions / functions | 10 / 177 / 12 / 4 | 10 / 177 / 12 / 4 | 10 / 177 / 12 / 4 |
| resource identity schemas | 463 | 292 sdkv2 + 171 fw = 463 | 463 |
| attributes with ForceNew | — | 4672 sdkv2 + 1293 fw | 5965 |
| attributes with validators (sdkv2) | — | 58433 | 58433 |
| └ literal args / enum type params recovered | — | — | 42136 + 4888 candidate |
| framework attrs with validators (described) | — | 1522 | 1522 |
| defaults / constraint groups / diff-suppress (sdkv2) | — | 2980 / 1103 / 720 | 2980 / 1103 / 720 |
| CRUD funcs with file:line | — | 1240 | 1240 |

The totals cross-check exactly: 1240+447 resources, 514+159 data sources and 292+171
identity schemas each reproduce the protocol's numbers while exposing the mux split the
protocol erases. aws also shows the scale difference in validation styles: ~58k
validator attachments on the SDKv2 side (mostly `validation.*` constructors, args
recoverable by --ssa) vs ~1.5k on the framework side (self-describing validator
objects, values already in their descriptions).

## how src mode gets filenames without SSA

The shim resolves every function in the schema (CRUD, validators, defaults, diff
suppression, state upgraders) through `runtime.FuncForPC`, which yields the fully
qualified function name and its `file:line`:

```json
"create": {
  "name": "github.com/hashicorp/terraform-provider-azurerm/internal/services/compute.resourceLinuxVirtualMachineCreate",
  "file": "internal/services/compute/linux_virtual_machine_resource.go",
  "line": 488
}
```

Validators built by constructors are closures; their runtime name keeps the constructor
(and, thanks to inlining, usually the provider function that called it). The dump adds
an `origin` field with the `.funcN` suffixes stripped:

```json
"validate_func": {
  "name": "github.com/hashicorp/terraform-provider-azurerm/internal/services/apimanagement.resourceApiManagementService.IntBetween.func1",
  "origin": "github.com/hashicorp/terraform-provider-azurerm/internal/services/apimanagement.resourceApiManagementService.IntBetween",
  "file": "helper/validation/int.go",
  "line": 42
}
```

## how --ssa recovers the arguments

The values a validator closure captured (`1, 100`) exist only in source, so `--ssa`
parses the provider repo — plus any vendored packages the schema references — and
indexes every constructor call sitting in a schema-relevant position (`ValidateFunc:`,
`DefaultFunc:`, `DiffSuppressFunc:`, ... or the return statement of a wrapper like
`func PortNumber() pluginsdk.SchemaValidateFunc`). Each call site records its enclosing
function, receiver, nearest attribute key, and evaluated arguments. Every func ref with
an `origin` is then matched against the index, narrowing by enclosing function → receiver
→ attribute name → schema field until a unique site remains:

```json
"validate_func": {
  "origin": "...apimanagement.resourceApiManagementService.IntBetween",
  "call": {
    "file": "internal/services/apimanagement/api_management_resource.go",
    "line": 220,
    "args": [0, 50]
  }
}
```

Argument encoding: literals become native JSON values (`[0, 50]`,
`[["base64", "hex", "pem"], false]`); nested constructor calls become
`{"func": "string", "args": [...]}`; anything non-literal falls back honestly to its
source text as `{"expr": "possibleValuesForSignInAudience"}`. When several differing
call sites survive filtering the ref gets `"call_candidates": [...]` instead of a
guess, and when multiple sites agree on identical args the call carries
`"sites": N`. The `call.file`/`call.line` point at the attribute's declaration, which is
how `--ssa` also yields per-attribute source positions.

Two provider patterns need extra machinery, both discovered via aws:

- **Generic validators.** aws validates enums with `enum.Validate[awstypes.X]()`; the
  runtime erases the type param, collapsing every enum validator in the provider into
  one origin (`enum.validateSlow[...]`). The scanner therefore indexes generic
  instantiations (their type arguments are the payload — emitted as `"type_args":
  ["awstypes.AlternateContactType"]`), and matching falls back from the callee name to
  any call whose package qualifier matches the origin's package.
- **Anchoring.** Refs without caller context can't be matched repo-wide without risking
  confident wrong answers. Each resource's caller-scoped matches vote for its anchor
  file (CRUD refs can't be used — aws routes every resource through one shared
  interceptor), and anchor-less refs are matched in that file, then its directory, then
  globally only on an exact attribute-name hit.

(Naming honesty: the current pass is AST-based call-site analysis, not full SSA
value-tracing — the flag is named for where it's headed. Constants like
`math.MaxInt32` or values threaded through variables surface as `expr` text today;
folding those needs `go/types`/SSA.)

## usage

```
tfschemaex plugin <provider-binary>   [-o out.json]
tfschemaex src    <provider-repo>     [-o out.json] [--ssa] [--entry import/path.FuncName] [--keep-shim]
tfschemaex version
```

| flag              | env                  | description                                              |
|-------------------|----------------------|----------------------------------------------------------|
| `-o`, `--out`     | `TFSCHEMAEX_OUT`     | output file, `-` for stdout (default `results/<provider>.<date>.<mode>.json`) |
| `--ssa`           | `TFSCHEMAEX_SSA`     | src mode: recover literal args via call-site analysis    |
| `--entry`         | `TFSCHEMAEX_ENTRY`   | src mode: override the auto-detected entry point         |
| `--keep-shim`     |                      | src mode: leave the generated shim behind to debug       |
| `--timeout`       | `TFSCHEMAEX_TIMEOUT` | plugin mode extraction timeout (default 5m)              |
| `--silent`        | `TFSCHEMAEX_SILENT`  | suppress progress output                                 |
| `-v`, `--verbose` |                      | detailed progress (entry point candidates, etc)          |

Progress goes to stderr; with `-o -` only the JSON goes to stdout, so
`tfschemaex plugin p -o - > s.json` is always clean.

Src mode entry detection scans for exported constructor functions returning
`*schema.Provider` (terraform-plugin-sdk v2) — with or without a `context.Context`
parameter and an `error` result — preferring `Provider`/`*Provider` names and skipping
`Test*`. azurerm's `AzureProvider()`, azuread's `AzureADProvider()` and aws's
`sdkv2.NewProvider(ctx) (*schema.Provider, error)` are all found automatically.

When the module also uses terraform-plugin-framework, src mode locates the framework
provider constructor the same way (preferring ones wired to the SDKv2 provider, like
aws's `NewProvider(ctx, primary)` and azurerm's `NewFrameworkProvider(primary)`;
override with `--framework-entry`) and walks the framework side of the mux entirely via
reflection — so it works across framework versions and captures framework resources,
data sources, ephemeral + list resources, actions, functions and identity schemas under
a `framework` key. Framework attributes carry more than the protocol exposes:
validators, plan modifiers and defaults appear with their human-readable descriptions
and internal parameters, and `RequiresReplace` plan modifiers are folded into a
`force_new` flag to mirror the SDKv2 side. (`--ssa` literal-arg recovery targets the
SDKv2 side; framework validator values are usually already visible in their
descriptions.)

## output

The envelope is shared; the payload shape depends on the mode:

```jsonc
{
  "format_version": "1.0",
  "tool": "tfschemaex", "tool_version": "...",
  "mode": "plugin",                       // or "src"
  "source": "...", "protocol_version": 5, // plugin mode
  "entry": "...", "sdk": "sdkv2",         // src mode
  "ssa": true,                            // src mode payload was enriched
  "plugin": { "resources": {...}, "data_sources": {...}, "ephemeral_resources": {...},
              "functions": {...}, "resource_identity_schemas": {...}, ... },
  "src":    { "provider": {...}, "resources": {...}, "data_sources": {...} }
}
```

## development

```sh
make          # fmt + build
make install  # install into GOPATH/bin
make test     # unit tests
make lint     # golangci-lint
make proto    # regenerate the tfplugin5/tfplugin6 stubs from terraform-plugin-go
```

## limitations / future work

- azurerm's typed (`internal/sdk`) resources report wrapper functions for CRUD; their
  schemas are still fully captured.
- `--ssa` resolves external packages via the provider's `vendor/` directory; providers
  that don't vendor lose the call sites living in dependencies (they count as
  unlocated).
- true SSA value-tracing (constants, args threaded through variables, non-literal
  expressions) would push the ~85-90% arg-recovery rate higher — those currently fall
  back to `expr` source text.
- `--ssa` call-site enrichment does not yet cover framework validator constructors
  (their values are usually visible in validator descriptions already).
