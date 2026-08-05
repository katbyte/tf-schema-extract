package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/katbyte/tfschemaex/lib/schemajson"
	"github.com/katbyte/tfschemaex/lib/tfplugin6"
)

func extractV6(ctx context.Context, c tfplugin6.ProviderClient) (*schemajson.PluginDump, error) {
	resp, err := c.GetProviderSchema(ctx, &tfplugin6.GetProviderSchema_Request{})
	if err != nil {
		return nil, fmt.Errorf("GetProviderSchema: %w", err)
	}

	dump := &schemajson.PluginDump{
		Provider:           schema6(resp.GetProvider()),
		ProviderMeta:       schema6(resp.GetProviderMeta()),
		Resources:          schemaMap6(resp.GetResourceSchemas()),
		DataSources:        schemaMap6(resp.GetDataSourceSchemas()),
		EphemeralResources: schemaMap6(resp.GetEphemeralResourceSchemas()),
		ListResources:      schemaMap6(resp.GetListResourceSchemas()),
		StateStores:        schemaMap6(resp.GetStateStoreSchemas()),
		Functions:          functions6(resp.GetFunctions()),
		ServerCapabilities: capabilities6(resp.GetServerCapabilities()),
		Diagnostics:        diagnostics6(resp.GetDiagnostics()),
	}

	if actions := resp.GetActionSchemas(); len(actions) > 0 {
		dump.Actions = make(map[string]*schemajson.Schema, len(actions))
		for name, a := range actions {
			dump.Actions[name] = schema6(a.GetSchema())
		}
	}

	// identity schemas arrived in protocol 6.7 — older providers return Unimplemented
	if ident, err := c.GetResourceIdentitySchemas(ctx, &tfplugin6.GetResourceIdentitySchemas_Request{}); err == nil {
		dump.ResourceIdentitySchemas = identities6(ident.GetIdentitySchemas())
	} else if !unimplemented(err) {
		return nil, fmt.Errorf("GetResourceIdentitySchemas: %w", err)
	}

	return dump, nil
}

func schemaMap6(in map[string]*tfplugin6.Schema) map[string]*schemajson.Schema {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*schemajson.Schema, len(in))
	for name, s := range in {
		out[name] = schema6(s)
	}
	return out
}

func schema6(in *tfplugin6.Schema) *schemajson.Schema {
	if in == nil {
		return nil
	}
	return &schemajson.Schema{
		Version: in.GetVersion(),
		Block:   block6(in.GetBlock()),
	}
}

func block6(in *tfplugin6.Schema_Block) *schemajson.Block {
	if in == nil {
		return nil
	}

	out := &schemajson.Block{
		Description:        in.GetDescription(),
		DescriptionKind:    kind6(in.GetDescriptionKind()),
		Deprecated:         in.GetDeprecated(),
		DeprecationMessage: in.GetDeprecationMessage(),
	}

	if attrs := in.GetAttributes(); len(attrs) > 0 {
		out.Attributes = make(map[string]*schemajson.Attribute, len(attrs))
		for _, a := range attrs {
			out.Attributes[a.GetName()] = attribute6(a)
		}
	}

	if blocks := in.GetBlockTypes(); len(blocks) > 0 {
		out.BlockTypes = make(map[string]*schemajson.NestedBlock, len(blocks))
		for _, b := range blocks {
			out.BlockTypes[b.GetTypeName()] = &schemajson.NestedBlock{
				Nesting:  strings.ToLower(b.GetNesting().String()),
				Block:    block6(b.GetBlock()),
				MinItems: b.GetMinItems(),
				MaxItems: b.GetMaxItems(),
			}
		}
	}

	return out
}

func attribute6(in *tfplugin6.Schema_Attribute) *schemajson.Attribute {
	out := &schemajson.Attribute{
		Type:               json.RawMessage(in.GetType()),
		Description:        in.GetDescription(),
		DescriptionKind:    kind6(in.GetDescriptionKind()),
		Required:           in.GetRequired(),
		Optional:           in.GetOptional(),
		Computed:           in.GetComputed(),
		Sensitive:          in.GetSensitive(),
		WriteOnly:          in.GetWriteOnly(),
		Deprecated:         in.GetDeprecated(),
		DeprecationMessage: in.GetDeprecationMessage(),
	}

	if nt := in.GetNestedType(); nt != nil {
		out.NestedType = &schemajson.NestedObject{
			Nesting: strings.ToLower(nt.GetNesting().String()),
		}
		if attrs := nt.GetAttributes(); len(attrs) > 0 {
			out.NestedType.Attributes = make(map[string]*schemajson.Attribute, len(attrs))
			for _, a := range attrs {
				out.NestedType.Attributes[a.GetName()] = attribute6(a)
			}
		}
	}

	return out
}

func functions6(in map[string]*tfplugin6.Function) map[string]*schemajson.Function {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*schemajson.Function, len(in))
	for name, f := range in {
		fn := &schemajson.Function{
			VariadicParameter:  functionParameter6(f.GetVariadicParameter()),
			Return:             json.RawMessage(f.GetReturn().GetType()),
			Summary:            f.GetSummary(),
			Description:        f.GetDescription(),
			DescriptionKind:    kind6(f.GetDescriptionKind()),
			DeprecationMessage: f.GetDeprecationMessage(),
		}
		for _, p := range f.GetParameters() {
			fn.Parameters = append(fn.Parameters, functionParameter6(p))
		}
		out[name] = fn
	}
	return out
}

func functionParameter6(in *tfplugin6.Function_Parameter) *schemajson.FunctionParameter {
	if in == nil {
		return nil
	}
	return &schemajson.FunctionParameter{
		Name:               in.GetName(),
		Type:               json.RawMessage(in.GetType()),
		AllowNullValue:     in.GetAllowNullValue(),
		AllowUnknownValues: in.GetAllowUnknownValues(),
		Description:        in.GetDescription(),
		DescriptionKind:    kind6(in.GetDescriptionKind()),
	}
}

func identities6(in map[string]*tfplugin6.ResourceIdentitySchema) map[string]*schemajson.IdentitySchema {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*schemajson.IdentitySchema, len(in))
	for name, s := range in {
		is := &schemajson.IdentitySchema{Version: s.GetVersion()}
		if attrs := s.GetIdentityAttributes(); len(attrs) > 0 {
			is.Attributes = make(map[string]*schemajson.IdentityAttribute, len(attrs))
			for _, a := range attrs {
				is.Attributes[a.GetName()] = &schemajson.IdentityAttribute{
					Type:              json.RawMessage(a.GetType()),
					RequiredForImport: a.GetRequiredForImport(),
					OptionalForImport: a.GetOptionalForImport(),
					Description:       a.GetDescription(),
				}
			}
		}
		out[name] = is
	}
	return out
}

func capabilities6(in *tfplugin6.ServerCapabilities) *schemajson.ServerCapabilities {
	if in == nil {
		return nil
	}
	return &schemajson.ServerCapabilities{
		PlanDestroy:               in.GetPlanDestroy(),
		GetProviderSchemaOptional: in.GetGetProviderSchemaOptional(),
		MoveResourceState:         in.GetMoveResourceState(),
	}
}

func diagnostics6(in []*tfplugin6.Diagnostic) []schemajson.Diagnostic {
	out := make([]schemajson.Diagnostic, 0, len(in))
	for _, d := range in {
		out = append(out, schemajson.Diagnostic{
			Severity: strings.ToLower(d.GetSeverity().String()),
			Summary:  d.GetSummary(),
			Detail:   d.GetDetail(),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func kind6(k tfplugin6.StringKind) string {
	return strings.ToLower(k.String())
}
