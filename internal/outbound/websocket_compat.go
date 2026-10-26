package outbound

import (
	"context"
	"errors"
	"net"

	"github.com/sagernet/sing-box/adapter"
	sbOutbound "github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/common/tls"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

const websocketCompatTag = "resin-websocket-transport"

// Keep the upstream protocol clients, multiplexing and packet encodings. Only
// their WebSocket transport is routed through our cancellation-safe connection.
// sing-box 1.12's lazy early-data connection does not wake Read on Close before
// the first Write, which otherwise deadlocks WireGuard's receive-loop shutdown.
func (b *SingboxBuilder) buildWebsocketCompat(cfg option.Outbound, logger log.ContextLogger) (adapter.Outbound, bool, error) {
	var transport *option.V2RayTransportOptions
	var server option.ServerOptions
	var tlsOptions *option.OutboundTLSOptions
	var dialOptions option.DialerOptions
	var shared bool
	switch opts := cfg.Options.(type) {
	case *option.VMessOutboundOptions:
		transport, server, tlsOptions, dialOptions = opts.Transport, opts.ServerOptions, opts.TLS, opts.DialerOptions
		shared = opts.Multiplex != nil && opts.Multiplex.Enabled
	case *option.VLESSOutboundOptions:
		transport, server, tlsOptions, dialOptions = opts.Transport, opts.ServerOptions, opts.TLS, opts.DialerOptions
		shared = opts.Multiplex != nil && opts.Multiplex.Enabled
	case *option.TrojanOutboundOptions:
		transport, server, tlsOptions, dialOptions = opts.Transport, opts.ServerOptions, opts.TLS, opts.DialerOptions
		shared = opts.Multiplex != nil && opts.Multiplex.Enabled
	default:
		return nil, false, nil
	}
	if transport == nil || transport.Type != C.V2RayTransportTypeWebsocket || transport.WebsocketOptions.MaxEarlyData == 0 {
		return nil, false, nil
	}
	base, err := dialer.New(b.ctx, dialOptions, server.ServerIsDomain())
	if err != nil {
		return nil, true, err
	}
	var tlsConfig tls.Config
	if tlsOptions != nil {
		tlsConfig, err = tls.NewClient(b.ctx, server.Server, *tlsOptions)
		if err != nil {
			return nil, true, err
		}
	}
	if tlsConfig != nil && len(tlsConfig.NextProtos()) == 0 {
		tlsConfig.SetNextProtos([]string{"http/1.1"})
	}
	ws := &websocketTransportOutbound{
		Adapter: sbOutbound.NewAdapter("websocket", websocketCompatTag, []string{N.NetworkTCP}, nil),
		dialer:  base, server: server.Build(), options: transport.WebsocketOptions, tlsConfig: tlsConfig,
		shared: shared,
	}
	manager := &websocketOutboundManager{OutboundManager: service.FromContext[adapter.OutboundManager](b.ctx), transport: ws}
	// ContextWith mutates its registry; use an overlay to isolate parallel node
	// builds from one another and preserve the original DNS and detour services.
	ctx := service.ContextWithRegistry(b.ctx, websocketServiceRegistry{Registry: service.RegistryFromContext(b.ctx), manager: manager})
	privateDialer := option.DialerOptions{Detour: websocketCompatTag}
	var opts any
	switch original := cfg.Options.(type) {
	case *option.VMessOutboundOptions:
		copy := *original
		copy.Transport, copy.TLS, copy.DialerOptions = nil, nil, privateDialer
		if tlsConfig != nil && (copy.Security == "" || copy.Security == "auto") {
			copy.Security = "zero" // preserve upstream's automatic TLS selection
		}
		opts = &copy
	case *option.VLESSOutboundOptions:
		copy := *original
		copy.Transport, copy.TLS, copy.DialerOptions = nil, nil, privateDialer
		opts = &copy
	case *option.TrojanOutboundOptions:
		copy := *original
		copy.Transport, copy.TLS, copy.DialerOptions = nil, nil, privateDialer
		opts = &copy
	}
	ob, err := b.registry.CreateOutbound(ctx, nil, logger, cfg.Tag, cfg.Type, opts)
	if err != nil {
		return nil, true, err
	}
	var dependencies []string
	if dialOptions.Detour != "" {
		dependencies = []string{dialOptions.Detour}
	}
	return &websocketCompatOutbound{Outbound: ob, dependencies: dependencies}, true, nil
}

type websocketCompatOutbound struct {
	adapter.Outbound
	dependencies []string
}

func (o *websocketCompatOutbound) Dependencies() []string { return o.dependencies }
func (o *websocketCompatOutbound) Start(stage adapter.StartStage) error {
	return adapter.LegacyStart(o.Outbound, stage)
}
func (o *websocketCompatOutbound) InterfaceUpdated() {
	if listener, ok := o.Outbound.(interface{ InterfaceUpdated() }); ok {
		listener.InterfaceUpdated()
	}
}
func (o *websocketCompatOutbound) Close() error {
	closeOutbound(o.Outbound)
	return nil
}

type websocketServiceRegistry struct {
	service.Registry
	manager adapter.OutboundManager
}

func (r websocketServiceRegistry) Get(kind any) any {
	if _, ok := kind.(*adapter.OutboundManager); ok {
		return r.manager
	}
	return r.Registry.Get(kind)
}

type websocketOutboundManager struct {
	adapter.OutboundManager
	transport adapter.Outbound
}

func (m *websocketOutboundManager) Outbound(tag string) (adapter.Outbound, bool) {
	if tag == websocketCompatTag {
		return m.transport, true
	}
	return m.OutboundManager.Outbound(tag)
}

type websocketTransportOutbound struct {
	sbOutbound.Adapter
	dialer    N.Dialer
	server    M.Socksaddr
	options   option.V2RayWebsocketOptions
	tlsConfig tls.Config
	shared    bool
}

func (o *websocketTransportOutbound) DialContext(ctx context.Context, network string, _ M.Socksaddr) (net.Conn, error) {
	if N.NetworkName(network) != N.NetworkTCP {
		return nil, N.ErrUnknownNetwork
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if o.shared {
		// sing-mux cancels the session's dialing context before the lazy first
		// write. The pooled connection is owned by the mux session instead.
		ctx = context.WithoutCancel(ctx)
	}
	return newEarlyWebsocketConn(ctx, o.dialer, o.server, o.options, o.tlsConfig), nil
}

func (*websocketTransportOutbound) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return nil, errors.New("WebSocket transport requires TCP")
}
