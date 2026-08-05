// Package schemajson defines the normalised JSON output model for tfschemaex.
//
// A Dump is the top-level envelope shared by both extraction modes. Plugin mode fills
// in the Plugin field with everything the terraform plugin protocol exposes; src mode
// fills in the Src field with the richer SDK-level detail (ForceNew, validators,
// defaults, source locations) that never crosses the wire.
package schemajson

import (
	"encoding/json"
	"fmt"
	"os"
)

const FormatVersion = "1.0"

// Dump is the top level JSON document produced by tfschemaex.
type Dump struct {
	FormatVersion   string          `json:"format_version"`
	Tool            string          `json:"tool"`
	ToolVersion     string          `json:"tool_version"`
	Mode            string          `json:"mode"`   // "plugin" or "src"
	Source          string          `json:"source"` // provider binary path (plugin) or repo path (src)
	ProtocolVersion int             `json:"protocol_version,omitempty"`
	Entry           string          `json:"entry,omitempty"`           // provider entry point func (src mode)
	FrameworkEntry  string          `json:"framework_entry,omitempty"` // plugin-framework constructor (src mode, muxed providers)
	SDK             string          `json:"sdk,omitempty"`             // "sdkv2" (src mode)
	SSA             bool            `json:"ssa,omitempty"`             // src payload was enriched with static call-site analysis
	Plugin          *PluginDump     `json:"plugin,omitempty"`
	Src             json.RawMessage `json:"src,omitempty"`
}

// PluginDump holds everything a provider exposes over the plugin protocol.
type PluginDump struct {
	Provider                *Schema                    `json:"provider,omitempty"`
	ProviderMeta            *Schema                    `json:"provider_meta,omitempty"`
	Resources               map[string]*Schema         `json:"resources,omitempty"`
	DataSources             map[string]*Schema         `json:"data_sources,omitempty"`
	EphemeralResources      map[string]*Schema         `json:"ephemeral_resources,omitempty"`
	ListResources           map[string]*Schema         `json:"list_resources,omitempty"`
	StateStores             map[string]*Schema         `json:"state_stores,omitempty"`
	Actions                 map[string]*Schema         `json:"actions,omitempty"`
	Functions               map[string]*Function       `json:"functions,omitempty"`
	ResourceIdentitySchemas map[string]*IdentitySchema `json:"resource_identity_schemas,omitempty"`
	ServerCapabilities      *ServerCapabilities        `json:"server_capabilities,omitempty"`
	Diagnostics             []Diagnostic               `json:"diagnostics,omitempty"`
}

// Schema is a versioned block (a resource, data source, ephemeral resource, etc).
type Schema struct {
	Version int64  `json:"version"`
	Block   *Block `json:"block,omitempty"`
}

type Block struct {
	Attributes         map[string]*Attribute   `json:"attributes,omitempty"`
	BlockTypes         map[string]*NestedBlock `json:"block_types,omitempty"`
	Description        string                  `json:"description,omitempty"`
	DescriptionKind    string                  `json:"description_kind,omitempty"`
	Deprecated         bool                    `json:"deprecated,omitempty"`
	DeprecationMessage string                  `json:"deprecation_message,omitempty"`
}

type Attribute struct {
	Type               json.RawMessage `json:"type,omitempty"` // cty type as JSON, e.g. "string" or ["map","string"]
	NestedType         *NestedObject   `json:"nested_type,omitempty"`
	Description        string          `json:"description,omitempty"`
	DescriptionKind    string          `json:"description_kind,omitempty"`
	Required           bool            `json:"required,omitempty"`
	Optional           bool            `json:"optional,omitempty"`
	Computed           bool            `json:"computed,omitempty"`
	Sensitive          bool            `json:"sensitive,omitempty"`
	WriteOnly          bool            `json:"write_only,omitempty"`
	Deprecated         bool            `json:"deprecated,omitempty"`
	DeprecationMessage string          `json:"deprecation_message,omitempty"`
}

type NestedBlock struct {
	Nesting  string `json:"nesting,omitempty"` // SINGLE, LIST, SET, MAP, GROUP
	Block    *Block `json:"block,omitempty"`
	MinItems int64  `json:"min_items,omitempty"`
	MaxItems int64  `json:"max_items,omitempty"`
}

// NestedObject is a plugin-framework object attribute type (protocol 6 only).
type NestedObject struct {
	Nesting    string                `json:"nesting,omitempty"` // SINGLE, LIST, SET, MAP
	Attributes map[string]*Attribute `json:"attributes,omitempty"`
}

type Function struct {
	Parameters         []*FunctionParameter `json:"parameters,omitempty"`
	VariadicParameter  *FunctionParameter   `json:"variadic_parameter,omitempty"`
	Return             json.RawMessage      `json:"return,omitempty"` // cty type as JSON
	Summary            string               `json:"summary,omitempty"`
	Description        string               `json:"description,omitempty"`
	DescriptionKind    string               `json:"description_kind,omitempty"`
	DeprecationMessage string               `json:"deprecation_message,omitempty"`
}

type FunctionParameter struct {
	Name               string          `json:"name,omitempty"`
	Type               json.RawMessage `json:"type,omitempty"`
	AllowNullValue     bool            `json:"allow_null_value,omitempty"`
	AllowUnknownValues bool            `json:"allow_unknown_values,omitempty"`
	Description        string          `json:"description,omitempty"`
	DescriptionKind    string          `json:"description_kind,omitempty"`
}

type IdentitySchema struct {
	Version    int64                         `json:"version"`
	Attributes map[string]*IdentityAttribute `json:"attributes,omitempty"`
}

type IdentityAttribute struct {
	Type              json.RawMessage `json:"type,omitempty"`
	RequiredForImport bool            `json:"required_for_import,omitempty"`
	OptionalForImport bool            `json:"optional_for_import,omitempty"`
	Description       string          `json:"description,omitempty"`
}

type ServerCapabilities struct {
	PlanDestroy               bool `json:"plan_destroy,omitempty"`
	GetProviderSchemaOptional bool `json:"get_provider_schema_optional,omitempty"`
	MoveResourceState         bool `json:"move_resource_state,omitempty"`
}

type Diagnostic struct {
	Severity string `json:"severity,omitempty"`
	Summary  string `json:"summary,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// WriteFile writes the dump as pretty-printed JSON to the given path, or stdout when path is "-" or "".
func (d *Dump) WriteFile(path string) error {
	out := os.Stdout
	if path != "" && path != "-" {
		f, err := os.Create(path) //nolint:gosec // the user-supplied output path is the entire point
		if err != nil {
			return fmt.Errorf("creating output file %s: %w", path, err)
		}
		defer func() { _ = f.Close() }()
		out = f
	}

	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d); err != nil {
		return fmt.Errorf("encoding schema dump: %w", err)
	}

	return nil
}
