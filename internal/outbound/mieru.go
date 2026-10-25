package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"

	mierucommon "github.com/enfein/mieru/v3/apis/common"
	mierumodel "github.com/enfein/mieru/v3/apis/model"
	"github.com/enfein/mieru/v3/pkg/appctl/appctlcommon"
	pb "github.com/enfein/mieru/v3/pkg/appctl/appctlpb"
	mierulog "github.com/enfein/mieru/v3/pkg/log"
	"github.com/enfein/mieru/v3/pkg/protocol"
	"github.com/sagernet/sing-box/adapter"
	sbOutbound "github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
	"google.golang.org/protobuf/proto"
)

// Mieru is registered locally because the bundled sing-box version has no
// Mieru outbound. Protocol framing and cryptography use the official client.
type mieruOutboundOptions struct {
	option.DialerOptions
	option.ServerOptions
	Username      string             `json:"username"`
	Password      string             `json:"password"`
	Transport     string             `json:"transport,omitempty"`
	Multiplexing  string             `json:"multiplexing,omitempty"`
	HandshakeMode string             `json:"handshake_mode,omitempty"`
	Network       option.NetworkList `json:"network,omitempty"`
	MTU           int32              `json:"mtu,omitempty"`
}

func registerMieruOutbound(registry *sbOutbound.Registry) {
	sbOutbound.Register[mieruOutboundOptions](registry, "mieru", newMieruOutbound)
}

type mieruOutbound struct {
	sbOutbound.Adapter
	mux       *protocol.Mux
	handshake pb.HandshakeMode
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	closed    bool
	conns     map[*mieruTrackedConn]struct{}
	once      sync.Once
	err       error
}

var _ adapter.Outbound = (*mieruOutbound)(nil)

var mieruLoggingOnce sync.Once

func newMieruOutbound(ctx context.Context, _ adapter.Router, _ log.ContextLogger, tag string, options mieruOutboundOptions) (adapter.Outbound, error) {
	// Match the official embedding API: don't emit process-global protocol logs.
	mieruLoggingOnce.Do(func() { mierulog.SetFormatter(&mierulog.NilFormatter{}) })
	profile, err := mieruProfile(options)
	if err != nil {
		return nil, err
	}
	outer, err := dialer.NewWithOptions(dialer.Options{
		Context: ctx, Options: options.DialerOptions,
		RemoteIsDomain: options.ServerIsDomain(), ResolverOnDetour: true,
	})
	if err != nil {
		return nil, fmt.Errorf("Mieru outer dialer: %w", err)
	}
	resolver := mieruResolver{router: service.FromContext[adapter.DNSRouter](ctx)}
	if resolving, ok := outer.(dialer.ResolveDialer); ok {
		resolver.query = resolving.QueryOptions()
	}
	transport := mieruOuterDialer{dialer: outer}
	if err := appctlcommon.ValidateClientConfigSingleProfile(profile); err != nil {
		// The profile holds credentials; errors must never include its JSON.
		return nil, fmt.Errorf("Mieru configuration: %w", err)
	}
	mux, err := appctlcommon.NewClientMuxFromProfile(profile, transport, transport, resolver,
		&mierucommon.ClientDNSConfig{BypassDialerDNS: true})
	if err != nil {
		return nil, fmt.Errorf("start Mieru client: %w", err)
	}
	lifetime, cancel := context.WithCancel(ctx)
	return &mieruOutbound{
		Adapter: sbOutbound.NewAdapterWithDialerOptions("mieru", tag, options.Network.Build(), options.DialerOptions),
		mux:     mux, handshake: profile.GetHandshakeMode(), ctx: lifetime, cancel: cancel, conns: make(map[*mieruTrackedConn]struct{}),
	}, nil
}

func mieruProfile(options mieruOutboundOptions) (*pb.ClientProfile, error) {
	if strings.TrimSpace(options.Server) == "" || options.ServerPort == 0 {
		return nil, errors.New("Mieru server and server_port are required")
	}
	if options.Username == "" || options.Password == "" {
		return nil, errors.New("Mieru username and password are required")
	}
	transport := strings.ToUpper(strings.TrimSpace(options.Transport))
	if transport == "" {
		transport = "TCP"
	}
	var protocol pb.TransportProtocol
	switch transport {
	case "TCP":
		protocol = pb.TransportProtocol_TCP
	case "UDP":
		protocol = pb.TransportProtocol_UDP
	default:
		return nil, errors.New("Mieru transport must be TCP or UDP")
	}
	multiplexing := strings.ToUpper(strings.TrimSpace(options.Multiplexing))
	if multiplexing == "" {
		multiplexing = "MULTIPLEXING_LOW"
	}
	muxValue, valid := pb.MultiplexingLevel_value[multiplexing]
	if !valid {
		return nil, errors.New("invalid Mieru multiplexing level")
	}
	handshake := strings.ToUpper(strings.TrimSpace(options.HandshakeMode))
	if handshake == "" {
		handshake = "HANDSHAKE_STANDARD"
	}
	handshakeValue, valid := pb.HandshakeMode_value[handshake]
	if !valid {
		return nil, errors.New("invalid Mieru handshake_mode")
	}
	server := &pb.ServerEndpoint{
		PortBindings: []*pb.PortBinding{{Port: proto.Int32(int32(options.ServerPort)), Protocol: protocol.Enum()}},
	}
	if ip := net.ParseIP(options.Server); ip != nil {
		server.IpAddress = proto.String(ip.String())
	} else {
		server.DomainName = proto.String(options.Server)
	}
	return &pb.ClientProfile{
		ProfileName:   proto.String("resin"),
		User:          &pb.User{Name: proto.String(options.Username), Password: proto.String(options.Password)},
		Servers:       []*pb.ServerEndpoint{server},
		Multiplexing:  &pb.MultiplexingConfig{Level: pb.MultiplexingLevel(muxValue).Enum()},
		HandshakeMode: pb.HandshakeMode(handshakeValue).Enum(), Mtu: proto.Int32(options.MTU),
	}, nil
}

// The Mieru library receives explicit dialers for both underlays. Neither path
// falls back to its default net.Dialer, preserving Resin DNS and dialer options.
type mieruOuterDialer struct{ dialer N.Dialer }

func (d mieruOuterDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.dialer.DialContext(ctx, network, M.ParseSocksaddr(address))
}

func (d mieruOuterDialer) ListenPacket(ctx context.Context, _ string, local, remote string) (net.PacketConn, error) {
	if local != "" {
		return nil, errors.New("Mieru underlay requested an unsupported local packet binding")
	}
	return d.dialer.ListenPacket(ctx, M.ParseSocksaddr(remote))
}

type mieruResolver struct {
	router adapter.DNSRouter
	query  adapter.DNSQueryOptions
}

func (r mieruResolver) LookupIP(ctx context.Context, network, host string) ([]net.IP, error) {
	if r.router == nil {
		return nil, errors.New("Mieru node DNS resolver is not available")
	}
	if network != "" && network != "ip" && network != "ip4" && network != "ip6" {
		return nil, net.UnknownNetworkError(network)
	}
	addresses, err := r.router.Lookup(ctx, host, r.query)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		address = address.Unmap()
		if (network == "ip4" && !address.Is4()) || (network == "ip6" && !address.Is6()) {
			continue
		}
		ips = append(ips, net.IP(address.AsSlice()))
	}
	return ips, nil
}

func mieruDestination(network string, destination M.Socksaddr) (*mierumodel.NetAddrSpec, error) {
	if !destination.Addr.IsValid() && destination.Fqdn == "" {
		return nil, errors.New("Mieru destination has no address")
	}
	address := &mierumodel.NetAddrSpec{Net: network, AddrSpec: mierumodel.AddrSpec{
		FQDN: destination.Fqdn, Port: int(destination.Port),
	}}
	if destination.Addr.IsValid() {
		address.IP = net.IP(destination.Addr.AsSlice())
	}
	return address, nil
}

func (o *mieruOutbound) dialStream(ctx context.Context, destination net.Addr) (net.Conn, error) {
	requestCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(o.ctx, cancel)
	defer func() { stop(); cancel() }()
	if err := o.ctx.Err(); err != nil {
		return nil, net.ErrClosed
	}
	conn, err := o.mux.DialContext(requestCtx)
	if err != nil {
		return nil, err
	}
	tracked := &mieruTrackedConn{Conn: conn, owner: o}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	o.conns[tracked] = struct{}{}
	o.mu.Unlock()
	address, ok := destination.(*mierumodel.NetAddrSpec)
	if !ok {
		_ = tracked.Close()
		return nil, errors.New("invalid Mieru destination type")
	}
	if o.handshake == pb.HandshakeMode_HANDSHAKE_NO_WAIT {
		return newMieruEarlyConn(tracked, o.ctx, address), nil
	}
	if err := mieruHandshake(requestCtx, tracked, address, nil); err != nil {
		_ = tracked.Close()
		return nil, err
	}
	return tracked, nil
}

func (o *mieruOutbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
		network = N.NetworkTCP
	case "udp", "udp4", "udp6":
		network = N.NetworkUDP
	default:
		return nil, net.UnknownNetworkError(network)
	}
	if !slices.Contains(o.Network(), network) {
		return nil, fmt.Errorf("Mieru outbound does not support %s", network)
	}
	address, err := mieruDestination(network, destination)
	if err != nil {
		return nil, err
	}
	if network == N.NetworkTCP {
		return o.dialStream(ctx, address)
	}
	packet, err := o.ListenPacket(ctx, destination)
	if err != nil {
		return nil, err
	}
	return bufio.NewBindPacketConn(packet, address), nil
}

func (o *mieruOutbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	if !slices.Contains(o.Network(), N.NetworkUDP) {
		return nil, errors.New("Mieru outbound does not support UDP")
	}
	address, err := mieruDestination(N.NetworkUDP, destination)
	if err != nil {
		return nil, err
	}
	stream, err := o.dialStream(ctx, address)
	if err != nil {
		return nil, err
	}
	return &mieruPacketConn{PacketConn: mierucommon.NewUDPAssociateWrapper(mierucommon.NewPacketOverStreamTunnel(stream))}, nil
}

// Serialize framed packet I/O so concurrent WriteTo calls cannot interleave
// SOCKS5 destination headers or packet-over-stream frame boundaries.
type mieruPacketConn struct {
	net.PacketConn
	readMu, writeMu sync.Mutex
}

func (c *mieruPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	return c.PacketConn.ReadFrom(p)
}

func (c *mieruPacketConn) WriteTo(p []byte, address net.Addr) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.PacketConn.WriteTo(p, address)
}

type mieruTrackedConn struct {
	net.Conn
	owner     *mieruOutbound
	once      sync.Once
	err       error
	deadlines mieruDeadlines
}

func (c *mieruTrackedConn) Close() error {
	c.once.Do(func() {
		c.stopDeadlines()
		c.err = c.Conn.Close()
		c.owner.mu.Lock()
		delete(c.owner.conns, c)
		c.owner.mu.Unlock()
	})
	return c.err
}

func (o *mieruOutbound) Close() error {
	o.once.Do(func() {
		o.mu.Lock()
		o.closed = true
		connections := make([]*mieruTrackedConn, 0, len(o.conns))
		for conn := range o.conns {
			connections = append(connections, conn)
		}
		o.mu.Unlock()
		o.cancel()
		// Closing underlays first interrupts blocked physical writes before a
		// session's graceful-close path tries to flush its final control frame.
		o.err = o.mux.Close()
		for _, conn := range connections {
			_ = conn.Close()
		}
	})
	return o.err
}
