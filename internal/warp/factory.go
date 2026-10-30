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
	"slices"
	"strconv"
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

type Factory struct {
	registrar *registrar
	// Internal seams keep endpoint failure/cancellation tests independent of
	// device registration and public network access.
	openTunnel func(context.Context, adapter.Outbound, Family, registration, []netip.Prefix, netip.AddrPort) (*Exit, error)
	verifyExit func(context.Context, *Exit) error
	probeUDP   func(context.Context, adapter.Outbound) error

	spareMu sync.Mutex
	spares  []registration
}

const (
	// The tunnel phase starts after registration, so queueing behind the
	// shared registration gap cannot consume the endpoint attempt budget.
	tunnelBudget         = 45 * time.Second
	endpointProbeTimeout = 8 * time.Second
	// A failed attempt leaves an unused device. Hand it to the next attempt
	// instead of registering again, within bounded count, age, and reuse.
	spareLimit           = 16
	spareLifetime        = 30 * time.Minute
	registrationMaxTries = 3
)

func NewFactory() *Factory {
	return &Factory{registrar: &registrar{base: apiBase, gap: 12 * time.Second}}
}

func (f *Factory) takeSpare(now time.Time) (registration, bool) {
	f.spareMu.Lock()
	defer f.spareMu.Unlock()
	for len(f.spares) > 0 {
		last := len(f.spares) - 1
		spare := f.spares[last]
		f.spares = f.spares[:last]
		if now.Before(spare.expires) {
			return spare, true
		}
	}
	return registration{}, false
}

func (f *Factory) keepSpare(reg registration) {
	if reg.tries >= registrationMaxTries {
		return
	}
	f.spareMu.Lock()
	defer f.spareMu.Unlock()
	if len(f.spares) >= spareLimit {
		f.spares = slices.Delete(f.spares, 0, 1)
	}
	f.spares = append(f.spares, reg)
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
		return nil, &ParentUDPError{Rejected: true, Err: errors.New("WARP parent does not support UDP")}
	}
	probe := f.probeUDP
	if probe == nil {
		probe = probeParentUDP
	}
	if err := probe(ctx, parent); err != nil {
		return nil, fmt.Errorf("probe WARP parent UDP: %w", err)
	}
	reportProgress(ctx)
	reg, reused := f.takeSpare(time.Now())
	if !reused {
		var err error
		if reg, err = f.registrar.register(ctx, parent); err != nil {
			return nil, err
		}
		reg.expires = time.Now().Add(spareLifetime)
		reportProgress(ctx)
	}
	reg.tries++
	tunnelCtx, cancel := context.WithTimeout(ctx, tunnelBudget)
	exit, err := f.createRegistered(tunnelCtx, parent, family, reg)
	cancel()
	if err != nil && ctx.Err() == nil {
		f.keepSpare(reg)
	}
	return exit, err
}

func (f *Factory) createRegistered(ctx context.Context, parent adapter.Outbound, family Family, reg registration) (*Exit, error) {
	var addresses []netip.Prefix
	for _, address := range reg.addresses {
		if (family == IPv4) == address.Addr().Is4() {
			addresses = append(addresses, address)
		}
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("WARP registration has no %s interface address", family)
	}
	open := f.openTunnel
	if open == nil {
		open = newTunnel
	}
	verify := f.verifyExit
	if verify == nil {
		verify = func(ctx context.Context, exit *Exit) error { return exit.verify(ctx) }
	}
	var firstErr, lastErr error
	attempts := 0
	recordFailure := func(endpoint netip.AddrPort, phase string, err error) {
		if phase == "verify" {
			// verify already reports the precise TCP/TLS/HTTP phase.
			lastErr = fmt.Errorf("endpoint=%s: %w", endpoint, err)
		} else {
			lastErr = fmt.Errorf("endpoint=%s phase=%s: %w", endpoint, phase, err)
		}
		if firstErr == nil {
			firstErr = lastErr
		}
	}
	for _, endpoint := range orderedEndpoints(family, reg.endpoints) {
		if err := ctx.Err(); err != nil {
			if lastErr == nil {
				return nil, fmt.Errorf("verify WARP %s through parent attempts=%d: %w", family, attempts, err)
			}
			break
		}
		attempts++
		exit, err := open(ctx, parent, family, reg, addresses, endpoint)
		if err != nil {
			recordFailure(endpoint, "tunnel_create", err)
			reportProgress(ctx)
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, endpointProbeTimeout)
		if failed := exit.outerFailed(); failed != nil {
			// Stop waiting for a TCP timeout once the parent refuses UDP.
			go func() {
				select {
				case <-failed:
					cancel()
				case <-probeCtx.Done():
				}
			}()
		}
		err = verify(probeCtx, exit)
		cancel()
		if err == nil && ctx.Err() == nil {
			return exit, nil
		}
		if err == nil {
			err = ctx.Err()
		}
		recordFailure(endpoint, "verify", err)
		exit.Close()
		reportProgress(ctx)
		if exit.outerFailure() {
			// Every endpoint uses the same parent UDP association, so trying
			// the remaining endpoints would only repeat the same refusal.
			return nil, fmt.Errorf("verify WARP %s through parent attempts=%d: %w", family, attempts, &ParentUDPError{Err: lastErr})
		}
	}
	if lastErr == nil {
		lastErr = errors.New("registration contains no usable WireGuard endpoints")
	}
	if attempts > 1 && firstErr != nil {
		lastErr = fmt.Errorf("first=(%v) last=(%w)", firstErr, lastErr)
	}
	if err := ctx.Err(); err != nil {
		lastErr = errors.Join(lastErr, err)
	}
	return nil, fmt.Errorf("verify WARP %s through parent attempts=%d: %w", family, attempts, lastErr)
}

func orderedEndpoints(family Family, endpoints []netip.AddrPort) []netip.AddrPort {
	if family != IPv6 {
		return endpoints
	}
	var v4, v6 []netip.AddrPort
	for _, endpoint := range endpoints {
		if endpoint.Addr().Unmap().Is6() {
			v6 = append(v6, endpoint)
		} else {
			v4 = append(v4, endpoint)
		}
	}
	// An IPv6 inner exit can use an IPv4 outer proxy path. Interleave the
	// endpoint families so a larger advertised port list cannot consume the
	// whole attempt budget before the IPv4 fallback is tried even once.
	ordered := make([]netip.AddrPort, 0, len(endpoints))
	for index := 0; index < max(len(v4), len(v6)); index++ {
		if index < len(v6) {
			ordered = append(ordered, v6[index])
		}
		if index < len(v4) {
			ordered = append(ordered, v4[index])
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
	outer     *outerDiagnostics
}

var _ adapter.Outbound = (*Exit)(nil)

type tunnelEndpoint interface {
	N.Dialer
	Close() error
}

func newTunnel(ctx context.Context, parent adapter.Outbound, family Family, reg registration, addresses []netip.Prefix, endpoint netip.AddrPort) (*Exit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(context.Background())
	lifetime = pause.WithDefaultManager(lifetime)
	// Setup belongs to the caller's attempt. A published tunnel has its own
	// lifetime, since the manager cancels the attempt after successful creation.
	stopSetupCancellation := context.AfterFunc(ctx, cancel)
	outer := newOuterDiagnostics()
	inner, err := createEndpoint(lifetime, &diagnosticParentOutbound{Outbound: parent, state: outer}, reg, addresses, endpoint)
	setupDetached := stopSetupCancellation()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create WARP tunnel: %w", err)
	}
	if !setupDetached || ctx.Err() != nil {
		cancel()
		inner.Close()
		return nil, fmt.Errorf("create WARP tunnel: %w", ctx.Err())
	}
	e := &Exit{Adapter: outbound.NewAdapter("warp", "warp-"+reg.id+"-"+string(family), []string{"tcp", "udp"}, nil), family: family, endpoint: inner, cancel: cancel, outer: outer}
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
	phase := "tcp_connect"
	failure := func(err error) error {
		traceMu.Lock()
		failedPhase := phase
		traceMu.Unlock()
		return e.diagnosticError(failedPhase, err)
	}
	trace := &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) {
			traceMu.Lock()
			phase = "http_request"
			traceMu.Unlock()
		},
		TLSHandshakeStart: func() {
			traceMu.Lock()
			phase = "tls_handshake"
			handshakeStart = now()
			traceMu.Unlock()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			traceMu.Lock()
			if err == nil && !handshakeStart.IsZero() {
				handshakeLatency = now().Sub(handshakeStart)
				handshakeMeasured = true
				phase = "http_request"
			}
			traceMu.Unlock()
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				traceMu.Lock()
				phase = "http_headers"
				traceMu.Unlock()
			}
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	start := now()
	resp, err := client.Do(req)
	if err != nil {
		return failure(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return e.diagnosticError("http_headers", fmt.Errorf("WARP trace HTTP %d", resp.StatusCode))
	}
	address, err := parseTrace(resp.Body, e.family)
	if err != nil {
		return e.diagnosticError("http_body", err)
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

// tunnelDiagnostics contains only explicitly allowed numeric IPC fields. Never
// attach the raw IpcGet output or any key material to an error or a log.
type tunnelDiagnostics struct {
	available     bool
	handshakeUnix int64
	rxBytes       uint64
	txBytes       uint64
}

func parseTunnelDiagnostics(raw string) tunnelDiagnostics {
	result := tunnelDiagnostics{available: true}
	for _, line := range strings.Split(raw, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "last_handshake_time_sec":
			seconds, err := strconv.ParseInt(value, 10, 64)
			if err == nil && seconds > result.handshakeUnix {
				result.handshakeUnix = seconds
			}
		case "rx_bytes", "tx_bytes":
			count, err := strconv.ParseUint(value, 10, 64)
			if err == nil {
				if key == "rx_bytes" {
					result.rxBytes += count
				} else {
					result.txBytes += count
				}
			}
		}
	}
	return result
}

func (e *Exit) diagnosticError(phase string, err error) error {
	var state tunnelDiagnostics
	if endpoint, ok := e.endpoint.(interface{ diagnostics() tunnelDiagnostics }); ok {
		state = endpoint.diagnostics()
	}
	status := "unavailable"
	if state.available {
		status = "none"
		if state.handshakeUnix > 0 {
			status = "complete"
		}
	}
	outer := ""
	if e.outer != nil {
		e.outer.mu.Lock()
		outer = fmt.Sprintf(" outer_tx_packets=%d outer_rx_packets=%d", e.outer.txPackets, e.outer.rxPackets)
		if e.outer.lastError != nil {
			outer += fmt.Sprintf(" outer_last_op=%s outer_last_error=%q", e.outer.lastOp, e.outer.lastError.Error())
		}
		e.outer.mu.Unlock()
	}
	return fmt.Errorf("phase=%s wg_handshake=%s handshake_unix=%d rx_bytes=%d tx_bytes=%d%s: %w",
		phase, status, state.handshakeUnix, state.rxBytes, state.txBytes, outer, err)
}

// outerListenFailureLimit tolerates one transient parent hiccup; the bind
// retries every second, so the limit is reached within about one second.
const outerListenFailureLimit = 2

type outerDiagnostics struct {
	mu                   sync.Mutex
	txPackets, rxPackets uint64
	lastOp               string
	lastError            error
	listenFailures       int
	failed               chan struct{}
}

func newOuterDiagnostics() *outerDiagnostics {
	return &outerDiagnostics{failed: make(chan struct{})}
}

func (d *outerDiagnostics) record(operation string, transferred bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if transferred {
		if operation == "read" {
			d.rxPackets++
		} else if operation == "write" {
			d.txPackets++
		}
	}
	if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
		d.lastOp, d.lastError = operation, err
		if operation == "listen_packet" && d.rxPackets == 0 && d.failed != nil {
			d.listenFailures++
			if d.listenFailures == outerListenFailureLimit {
				close(d.failed)
			}
		}
	}
}

// outerFailed is closed when the parent repeatedly refused the outer UDP
// association before any WireGuard packet arrived.
func (e *Exit) outerFailed() <-chan struct{} {
	if e.outer == nil {
		return nil
	}
	return e.outer.failed
}

func (e *Exit) outerFailure() bool {
	failed := e.outerFailed()
	if failed == nil {
		return false
	}
	select {
	case <-failed:
		return true
	default:
		return false
	}
}

type diagnosticParentOutbound struct {
	adapter.Outbound
	state *outerDiagnostics
}

func (p *diagnosticParentOutbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	packet, err := p.Outbound.ListenPacket(ctx, destination)
	p.state.record("listen_packet", false, err)
	if err != nil {
		return nil, err
	}
	return &diagnosticPacketConn{PacketConn: packet, state: p.state}, nil
}

type diagnosticPacketConn struct {
	net.PacketConn
	state *outerDiagnostics
}

func (p *diagnosticPacketConn) ReadFrom(buffer []byte) (int, net.Addr, error) {
	n, address, err := p.PacketConn.ReadFrom(buffer)
	p.state.record("read", n > 0, err)
	return n, address, err
}

func (p *diagnosticPacketConn) WriteTo(buffer []byte, address net.Addr) (int, error) {
	n, err := p.PacketConn.WriteTo(buffer, address)
	p.state.record("write", n > 0, err)
	return n, err
}
