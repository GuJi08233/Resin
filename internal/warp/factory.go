package warp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service/pause"
)

type Family string

const (
	IPv4 Family = "ipv4"
	IPv6 Family = "ipv6"
)

type Factory struct{ registrar *registrar }

func NewFactory() *Factory {
	return &Factory{registrar: &registrar{base: apiBase, gap: 12 * time.Second}}
}

// Create registers a new device and returns only a verified WARP exit. Each
// exit owns its tunnel, but never owns or closes its parent proxy.
func (f *Factory) Create(ctx context.Context, parent adapter.Outbound, family Family) (*Exit, error) {
	if parent == nil {
		return nil, errors.New("WARP requires a parent outbound")
	}
	if family != IPv4 && family != IPv6 {
		return nil, fmt.Errorf("unsupported WARP family %q", family)
	}
	if !Supported() {
		return nil, errors.New("WARP requires a build with with_gvisor")
	}
	udp := false
	for _, network := range parent.Network() {
		if network == "udp" {
			udp = true
		}
	}
	if !udp {
		return nil, errors.New("WARP parent does not support UDP")
	}
	reg, err := f.registrar.register(ctx, parent)
	if err != nil {
		return nil, err
	}
	var addresses []netip.Prefix
	for _, address := range reg.addresses {
		if (family == IPv4) == address.Addr().Is4() {
			addresses = append(addresses, address)
		}
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("WARP registration has no %s interface address", family)
	}
	var lastErr error
	for _, endpoint := range orderedEndpoints(family, reg.endpoints) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		exit, err := newTunnel(parent, family, reg, addresses, endpoint)
		if err != nil {
			lastErr = err
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		err = exit.verify(probeCtx)
		cancel()
		if err == nil {
			return exit, nil
		}
		exit.Close()
		lastErr = err
	}
	return nil, fmt.Errorf("verify WARP %s through parent: %w", family, lastErr)
}

func orderedEndpoints(family Family, endpoints []netip.AddrPort) []netip.AddrPort {
	if family != IPv6 {
		return endpoints
	}
	ordered := make([]netip.AddrPort, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint.Addr().Unmap().Is6() {
			ordered = append(ordered, endpoint)
		}
	}
	for _, endpoint := range endpoints {
		if !endpoint.Addr().Unmap().Is6() {
			ordered = append(ordered, endpoint)
		}
	}
	return ordered
}

// Exit implements a Resin outbound with one fixed inner IP family. Domain
// lookups use Cloudflare DNS through the tunnel and never the host resolver.
type Exit struct {
	outbound.Adapter
	EgressIP  netip.Addr
	Latency   time.Duration
	family    Family
	endpoint  tunnelEndpoint
	resolver  *net.Resolver
	cancel    context.CancelFunc
	closeOnce sync.Once
	closeErr  error
}

var _ adapter.Outbound = (*Exit)(nil)

type tunnelEndpoint interface {
	N.Dialer
	Close() error
}

func newTunnel(parent adapter.Outbound, family Family, reg registration, addresses []netip.Prefix, endpoint netip.AddrPort) (*Exit, error) {
	lifetime, cancel := context.WithCancel(context.Background())
	lifetime = pause.WithDefaultManager(lifetime)
	inner, err := createEndpoint(lifetime, parent, reg, addresses, endpoint)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create WARP tunnel: %w", err)
	}
	e := &Exit{Adapter: outbound.NewAdapter("warp", "warp-"+reg.id+"-"+string(family), []string{"tcp", "udp"}, nil), family: family, endpoint: inner, cancel: cancel}
	e.resolver = &net.Resolver{PreferGo: true, StrictErrors: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		dns := M.ParseSocksaddr("1.1.1.1:53")
		if family == IPv6 {
			dns = M.ParseSocksaddr("[2606:4700:4700::1111]:53")
		}
		if strings.HasPrefix(network, "tcp") {
			network = "tcp"
		} else {
			network = "udp"
		}
		return inner.DialContext(ctx, network, dns)
	}}
	return e, nil
}

// parentPacketDialer deliberately converts UDP dialing into the parent's
// ListenPacket API. This prevents sing-box from selecting a host UDP socket.
type parentPacketDialer struct{ parent adapter.Outbound }

func (d parentPacketDialer) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if network != "udp" {
		return nil, fmt.Errorf("WARP outer transport only supports UDP, got %q", network)
	}
	packet, err := d.parent.ListenPacket(ctx, destination)
	if err != nil {
		return nil, err
	}
	return bufio.NewBindPacketConn(packet, destination.UDPAddr()), nil
}
func (d parentPacketDialer) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return d.parent.ListenPacket(ctx, destination)
}

func (e *Exit) resolve(ctx context.Context, destination M.Socksaddr) ([]M.Socksaddr, error) {
	if destination.Addr.IsValid() {
		addr := destination.Addr.Unmap()
		if (e.family == IPv4) != addr.Is4() {
			return nil, fmt.Errorf("WARP %s exit cannot reach %s", e.family, addr)
		}
		destination.Addr = addr
		return []M.Socksaddr{destination}, nil
	}
	if destination.Fqdn == "" {
		return nil, errors.New("WARP destination is missing an address")
	}
	network := "ip4"
	if e.family == IPv6 {
		network = "ip6"
	}
	addresses, err := e.resolver.LookupNetIP(ctx, network, destination.Fqdn)
	if err != nil {
		return nil, err
	}
	result := make([]M.Socksaddr, 0, len(addresses))
	for _, addr := range addresses {
		result = append(result, M.Socksaddr{Addr: addr.Unmap(), Port: destination.Port})
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no %s address for %s", e.family, destination.Fqdn)
	}
	return result, nil
}

func (e *Exit) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if (strings.HasSuffix(network, "4") && e.family != IPv4) || (strings.HasSuffix(network, "6") && e.family != IPv6) {
		return nil, fmt.Errorf("WARP %s exit cannot dial %q", e.family, network)
	}
	if strings.HasPrefix(network, "tcp") {
		network = "tcp"
	} else if strings.HasPrefix(network, "udp") {
		network = "udp"
	} else {
		return nil, fmt.Errorf("unsupported WARP network %q", network)
	}
	addresses, err := e.resolve(ctx, destination)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, address := range addresses {
		conn, err := e.endpoint.DialContext(ctx, network, address)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (e *Exit) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	addresses, err := e.resolve(ctx, destination)
	if err != nil {
		return nil, err
	}
	return e.endpoint.ListenPacket(ctx, addresses[0])
}

func (e *Exit) Close() error {
	e.closeOnce.Do(func() { e.cancel(); e.closeErr = e.endpoint.Close() })
	return e.closeErr
}

func (e *Exit) verify(ctx context.Context) error {
	target := "https://1.1.1.1/cdn-cgi/trace"
	if e.family == IPv6 {
		target = "https://[2606:4700:4700::1111]/cdn-cgi/trace"
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: 5 * time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return e.DialContext(ctx, network, M.ParseSocksaddr(address))
		}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return e.verifyWithClient(ctx, client, target)
}

func (e *Exit) verifyWithClient(ctx context.Context, client *http.Client, target string) error {
	return e.verifyWithClock(ctx, client, target, time.Now)
}

func (e *Exit) verifyWithClock(ctx context.Context, client *http.Client, target string, now func() time.Time) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	var traceMu sync.Mutex
	var handshakeStart time.Time
	var handshakeLatency time.Duration
	var handshakeMeasured bool
	trace := &httptrace.ClientTrace{
		TLSHandshakeStart: func() {
			traceMu.Lock()
			handshakeStart = now()
			traceMu.Unlock()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			traceMu.Lock()
			if err == nil && !handshakeStart.IsZero() {
				handshakeLatency = now().Sub(handshakeStart)
				handshakeMeasured = true
			}
			traceMu.Unlock()
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	start := now()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("WARP trace HTTP %d", resp.StatusCode)
	}
	address, err := parseTrace(resp.Body, e.family)
	if err != nil {
		return err
	}
	e.EgressIP = address
	traceMu.Lock()
	e.Latency = handshakeLatency
	measured := handshakeMeasured
	traceMu.Unlock()
	if !measured {
		e.Latency = now().Sub(start)
	}
	// A successful probe can finish within one clock tick. Preserve its valid
	// measurement while keeping it distinct from an unknown (zero) latency.
	if e.Latency <= 0 {
		e.Latency = time.Nanosecond
	}
	return nil
}
