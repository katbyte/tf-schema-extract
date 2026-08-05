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
tfschemaex plugin ~/go/bin/terraform-provider-azurerm -o azurerm-plugin.json
tfschemaex src ~/hashi/azure/azurerm -o azurerm-src.json
tfschemaex src ~/hashi/azure/azurerm --ssa -o azurerm-full.json
```

## what each rung of the ladder sees

The terraform plugin protocol only carries what terraform core needs to plan; a lot of
schema detail is enforced provider-side and **never crosses the wire** — which is why
azurerm's own release process uses an in-process dumper (`internal/tools/schema-api`)
rather than the protocol.

| detail                                                        | plugin | src | src --ssa |
|---------------------------------------------------------------|:------:|:---:|:---------:|
| resources / data sources / attributes / blocks / types        | ✔      | ✔   | ✔         |
| required / optional / computed / sensitive / deprecated       | ✔      | ✔   | ✔         |
| ephemeral + list resources, actions, provider functions       | ✔      |     |           |
| resource identity schemas                                     | ✔      |     |           |
| plugin-framework resources (muxed providers)                  | ✔      |     |           |
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
resource name. Measured against terraform-provider-azurerm: plugin mode dumps 1103
resources / 394 data sources / 172 identity schemas; `--ssa` recovers exact literal
args for ~90% of the ~4,500 constructor-built validator/default funcs in about ten
seconds of analysis.

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
| `-o`, `--out`     | `TFSCHEMAEX_OUT`     | output file, `-` for stdout (default)                    |
| `--ssa`           | `TFSCHEMAEX_SSA`     | src mode: recover literal args via call-site analysis    |
| `--entry`         | `TFSCHEMAEX_ENTRY`   | src mode: override the auto-detected entry point         |
| `--keep-shim`     |                      | src mode: leave the generated shim behind to debug       |
| `--timeout`       | `TFSCHEMAEX_TIMEOUT` | plugin mode extraction timeout (default 5m)              |
| `--silent`        | `TFSCHEMAEX_SILENT`  | suppress progress output                                 |
| `-v`, `--verbose` |                      | detailed progress (entry point candidates, etc)          |

Progress goes to stderr; only the JSON goes to stdout, so `tfschemaex plugin p > s.json`
is always clean.

Src mode entry detection scans for zero-arg exported functions returning
`*schema.Provider` (terraform-plugin-sdk v2), preferring `Provider`/`*Provider` names and
skipping `Test*` — azurerm's `internal/provider.AzureProvider` and azuread's
`AzureADProvider` are found automatically.

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

- src mode covers terraform-plugin-sdk v2 providers only; plugin-framework resources in
  muxed providers (and functions/ephemeral resources) are captured by plugin mode.
- azurerm's typed (`internal/sdk`) resources report wrapper functions for CRUD; their
  schemas are still fully captured.
- `--ssa` resolves external packages via the provider's `vendor/` directory; providers
  that don't vendor lose the call sites living in dependencies (they count as
  unlocated).
- true SSA value-tracing (constants, args threaded through variables, non-literal
  expressions) would push the ~90% arg-recovery rate higher — those currently fall back
  to `expr` source text.
- SDKv2 resource identity schemas are currently only captured via plugin mode.
