// Package plugin extracts a provider's full protocol-level schema by launching the
// provider binary the same way terraform does: a go-plugin handshake (magic cookie,
// AutoMTLS, protocol negotiation for versions 5 and 6) followed by a GetProviderSchema
// RPC and, when supported, GetResourceIdentitySchemas.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	hclog "github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"github.com/katbyte/tfschemaex/lib/schemajson"
	"github.com/katbyte/tfschemaex/lib/tfplugin5"
	"github.com/katbyte/tfschemaex/lib/tfplugin6"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Handshake mirrors terraform's provider plugin handshake. The magic cookie is not a
// secret — it is a well-known value used only to detect that the binary is being
// launched as a plugin rather than run directly by a user.
var Handshake = goplugin.HandshakeConfig{
	MagicCookieKey:   "TF_PLUGIN_MAGIC_COOKIE",
	MagicCookieValue: "d602bf8f470bc67ca7faa0386276bbdd4330efaf76d1a219cb4d6991ca9872b2",
}

// grpcPlugin implements go-plugin's GRPCPlugin interface for a single protocol version,
// dispensing the raw generated gRPC provider client.
type grpcPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	protocol int
}

func (p *grpcPlugin) GRPCServer(*goplugin.GRPCBroker, *grpc.Server) error {
	return errors.New("tfschemaex is a client only")
}

func (p *grpcPlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, conn *grpc.ClientConn) (any, error) {
	if p.protocol == 6 {
		return tfplugin6.NewProviderClient(conn), nil
	}
	return tfplugin5.NewProviderClient(conn), nil
}

// Extract launches the provider binary, negotiates the highest supported protocol
// version, and returns the full protocol-level schema dump along with the negotiated
// protocol version.
func Extract(ctx context.Context, binary string) (*schemajson.PluginDump, int, error) {
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: Handshake,
		VersionedPlugins: map[int]goplugin.PluginSet{
			5: {"provider": &grpcPlugin{protocol: 5}},
			6: {"provider": &grpcPlugin{protocol: 6}},
		},
		Cmd:              exec.CommandContext(ctx, binary),
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		AutoMTLS:         true,
		Logger: hclog.New(&hclog.LoggerOptions{
			Name:   "tfschemaex",
			Level:  hclog.Error,
			Output: hclog.DefaultOutput,
		}),
	})
	defer client.Kill()

	rpcClient, err := client.Client()
	if err != nil {
		return nil, 0, fmt.Errorf("starting provider plugin %s: %w", binary, err)
	}

	raw, err := rpcClient.Dispense("provider")
	if err != nil {
		return nil, 0, fmt.Errorf("dispensing provider plugin: %w", err)
	}

	protocol := client.NegotiatedVersion()

	var dump *schemajson.PluginDump
	switch c := raw.(type) {
	case tfplugin6.ProviderClient:
		dump, err = extractV6(ctx, c)
	case tfplugin5.ProviderClient:
		dump, err = extractV5(ctx, c)
	default:
		return nil, protocol, fmt.Errorf("unexpected provider client type %T", raw)
	}
	if err != nil {
		return nil, protocol, err
	}

	return dump, protocol, nil
}

// unimplemented reports whether an RPC error just means the provider predates that RPC.
func unimplemented(err error) bool {
	if s, ok := status.FromError(err); ok {
		return s.Code() == codes.Unimplemented || s.Code() == codes.Unavailable
	}
	return false
}
