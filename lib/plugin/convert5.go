package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/katbyte/tfschemaex/lib/schemajson"
	"github.com/katbyte/tfschemaex/lib/tfplugin5"
)

func extractV5(ctx context.Context, c tfplugin5.ProviderClient) (*schemajson.PluginDump, error) {
	resp, err := c.GetSchema(ctx, &tfplugin5.GetProviderSchema_Request{})
	if err != nil {
		return nil, fmt.Errorf("GetSchema: %w", err)
	}

	dump := &schemajson.PluginDump{
		Provider:           schema5(resp.GetProvider()),
		ProviderMeta:       schema5(resp.GetProviderMeta()),
		Resources:          schemaMap5(resp.GetResourceSchemas()),
		DataSources:        schemaMap5(resp.GetDataSourceSchemas()),
		EphemeralResources: schemaMap5(resp.GetEphemeralResourceSchemas()),
		ListResources:      schemaMap5(resp.GetListResourceSchemas()),
		Functions:          functions5(resp.GetFunctions()),
		ServerCapabilities: capabilities5(resp.GetServerCapabilities()),
		Diagnostics:        diagnostics5(resp.GetDiagnostics()),
	}

	if actions := resp.GetActionSchemas(); len(actions) > 0 {
		dump.Actions = make(map[string]*schemajson.Schema, len(actions))
		for name, a := range actions {
			dump.Actions[name] = schema5(a.GetSchema())
		}
	}

	// identity schemas arrived in protocol 5.8 — older providers return Unimplemented
	if ident, err := c.GetResourceIdentitySchemas(ctx, &tfplugin5.GetResourceIdentitySchemas_Request{}); err == nil {
		dump.ResourceIdentitySchemas = identities5(ident.GetIdentitySchemas())
	} else if !unimplemented(err) {
		return nil, fmt.Errorf("GetResourceIdentitySchemas: %w", err)
	}

	return dump, nil
}

func schemaMap5(in map[string]*tfplugin5.Schema) map[string]*schemajson.Schema {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*schemajson.Schema, len(in))
	for name, s := range in {
		out[name] = schema5(s)
	}
	return out
}

func schema5(in *tfplugin5.Schema) *schemajson.Schema {
	if in == nil {
		return nil
	}
	return &schemajson.Schema{
		Version: in.GetVersion(),
		Block:   block5(in.GetBlock()),
	}
}

func block5(in *tfplugin5.Schema_Block) *schemajson.Block {
	if in == nil {
		return nil
	}

	out := &schemajson.Block{
		Description:        in.GetDescription(),
		DescriptionKind:    kind5(in.GetDescriptionKind()),
		Deprecated:         in.GetDeprecated(),
		DeprecationMessage: in.GetDeprecationMessage(),
	}

	if attrs := in.GetAttributes(); len(attrs) > 0 {
		out.Attributes = make(map[string]*schemajson.Attribute, len(attrs))
		for _, a := range attrs {
			out.Attributes[a.GetName()] = &schemajson.Attribute{
				Type:               json.RawMessage(a.GetType()),
				Description:        a.GetDescription(),
				DescriptionKind:    kind5(a.GetDescriptionKind()),
				Required:           a.GetRequired(),
				Optional:           a.GetOptional(),
				Computed:           a.GetComputed(),
				Sensitive:          a.GetSensitive(),
				WriteOnly:          a.GetWriteOnly(),
				Deprecated:         a.GetDeprecated(),
				DeprecationMessage: a.GetDeprecationMessage(),
			}
		}
	}

	if blocks := in.GetBlockTypes(); len(blocks) > 0 {
		out.BlockTypes = make(map[string]*schemajson.NestedBlock, len(blocks))
		for _, b := range blocks {
			out.BlockTypes[b.GetTypeName()] = &schemajson.NestedBlock{
				Nesting:  strings.ToLower(b.GetNesting().String()),
				Block:    block5(b.GetBlock()),
				MinItems: b.GetMinItems(),
				MaxItems: b.GetMaxItems(),
			}
		}
	}

	return out
}

func functions5(in map[string]*tfplugin5.Function) map[string]*schemajson.Function {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*schemajson.Function, len(in))
	for name, f := range in {
		fn := &schemajson.Function{
			VariadicParameter:  functionParameter5(f.GetVariadicParameter()),
			Return:             json.RawMessage(f.GetReturn().GetType()),
			Summary:            f.GetSummary(),
			Description:        f.GetDescription(),
			DescriptionKind:    kind5(f.GetDescriptionKind()),
			DeprecationMessage: f.GetDeprecationMessage(),
		}
		for _, p := range f.GetParameters() {
			fn.Parameters = append(fn.Parameters, functionParameter5(p))
		}
		out[name] = fn
	}
	return out
}

func functionParameter5(in *tfplugin5.Function_Parameter) *schemajson.FunctionParameter {
	if in == nil {
		return nil
	}
	return &schemajson.FunctionParameter{
		Name:               in.GetName(),
		Type:               json.RawMessage(in.GetType()),
		AllowNullValue:     in.GetAllowNullValue(),
		AllowUnknownValues: in.GetAllowUnknownValues(),
		Description:        in.GetDescription(),
		DescriptionKind:    kind5(in.GetDescriptionKind()),
	}
}

func identities5(in map[string]*tfplugin5.ResourceIdentitySchema) map[string]*schemajson.IdentitySchema {
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

func capabilities5(in *tfplugin5.ServerCapabilities) *schemajson.ServerCapabilities {
	if in == nil {
		return nil
	}
	return &schemajson.ServerCapabilities{
		PlanDestroy:               in.GetPlanDestroy(),
		GetProviderSchemaOptional: in.GetGetProviderSchemaOptional(),
		MoveResourceState:         in.GetMoveResourceState(),
	}
}

func diagnostics5(in []*tfplugin5.Diagnostic) []schemajson.Diagnostic {
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

func kind5(k tfplugin5.StringKind) string {
	return strings.ToLower(k.String())
}
