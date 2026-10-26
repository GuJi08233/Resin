//go:build with_gvisor

package warp

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/wireguard-go/conn"
)

// ownedClientBind publishes parent connections under a lock and keeps binding
// cancellation independent of dialing. Close can interrupt a slow dial without
// waiting for the dialing mutex, and a completed stale dial cannot reopen it.
type ownedClientBind struct {
	ctx         context.Context
	dialer      N.Dialer
	connectAddr netip.AddrPort
	reserved    [3]byte
	mu          sync.Mutex
	dialMu      sync.Mutex
	bindCtx     context.Context
	cancel      context.CancelFunc
	packet      *ownedParentPacket
	byEndpoint  map[netip.AddrPort][3]byte
}

var _ conn.Bind = (*ownedClientBind)(nil)

func newOwnedClientBind(ctx context.Context, dialer N.Dialer, address netip.AddrPort, reserved [3]byte) *ownedClientBind {
	return &ownedClientBind{ctx: ctx, dialer: dialer, connectAddr: address, reserved: reserved, byEndpoint: make(map[netip.AddrPort][3]byte)}
}

func (b *ownedClientBind) Open(uint16) ([]conn.ReceiveFunc, uint16, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ctx.Err(); err != nil {
		return nil, 0, err
	}
	if b.bindCtx != nil {
		return nil, 0, conn.ErrBindAlreadyOpen
	}
	ctx, cancel := context.WithCancel(b.ctx)
	b.bindCtx, b.cancel = ctx, cancel
	return []conn.ReceiveFunc{func(packets [][]byte, sizes []int, endpoints []conn.Endpoint) (int, error) {
		return b.receive(ctx, packets, sizes, endpoints)
	}}, 0, nil
}

func (b *ownedClientBind) Close() error {
	b.mu.Lock()
	if b.cancel != nil {
		b.cancel()
	}
	packet := b.packet
	b.packet, b.bindCtx, b.cancel = nil, nil, nil
	b.mu.Unlock()
	if packet != nil {
		return packet.Close()
	}
	return nil
}

func (b *ownedClientBind) connect(ctx context.Context) (*ownedParentPacket, error) {
	b.dialMu.Lock()
	defer b.dialMu.Unlock()
	b.mu.Lock()
	if ctx == nil || ctx != b.bindCtx || ctx.Err() != nil {
		b.mu.Unlock()
		return nil, net.ErrClosed
	}
	if packet := b.packet; packet != nil {
		select {
		case <-packet.done:
		default:
			b.mu.Unlock()
			return packet, nil
		}
	}
	b.mu.Unlock()
	var packet net.PacketConn
	var err error
	if b.connectAddr.IsValid() {
		var stream net.Conn
		stream, err = b.dialer.DialContext(ctx, N.NetworkUDP, M.SocksaddrFromNetIP(b.connectAddr))
		if err == nil {
			packet = bufio.NewUnbindPacketConn(stream)
		}
	} else {
		packet, err = b.dialer.ListenPacket(ctx, M.Socksaddr{Addr: netip.IPv4Unspecified()})
	}
	if err != nil {
		return nil, err
	}
	owned := &ownedParentPacket{PacketConn: packet, done: make(chan struct{})}
	b.mu.Lock()
	if ctx != b.bindCtx || ctx.Err() != nil {
		b.mu.Unlock()
		owned.Close()
		return nil, net.ErrClosed
	}
	b.packet = owned
	b.mu.Unlock()
	return owned, nil
}

func (b *ownedClientBind) receive(ctx context.Context, packets [][]byte, sizes []int, endpoints []conn.Endpoint) (int, error) {
	packet, err := b.connect(ctx)
	if err != nil {
		// Preserve retries for transient parent failures, but make the wait
		// cancellable so WireGuard can always join this receive routine.
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return 0, net.ErrClosed
		case <-timer.C:
			return 0, nil
		}
	}
	n, address, err := packet.ReadFrom(packets[0])
	if err != nil {
		packet.Close()
		if ctx.Err() != nil {
			return 0, net.ErrClosed
		}
		return 0, nil
	}
	if n > 3 {
		clear(packets[0][1:4])
	}
	sizes[0] = n
	endpoints[0] = parentEndpoint(M.SocksaddrFromNet(address).Unwrap().AddrPort())
	return 1, nil
}

func (b *ownedClientBind) Send(buffers [][]byte, endpoint conn.Endpoint) error {
	destination, ok := endpoint.(parentEndpoint)
	if !ok {
		return conn.ErrWrongEndpointType
	}
	b.mu.Lock()
	ctx := b.bindCtx
	reserved, found := b.byEndpoint[netip.AddrPort(destination)]
	if !found {
		reserved = b.reserved
	}
	b.mu.Unlock()
	packet, err := b.connect(ctx)
	if err != nil {
		return err
	}
	for _, buffer := range buffers {
		if len(buffer) > 3 {
			copy(buffer[1:4], reserved[:])
		}
		if _, err := packet.WriteTo(buffer, net.UDPAddrFromAddrPort(netip.AddrPort(destination))); err != nil {
			packet.Close()
			return err
		}
	}
	return nil
}

func (b *ownedClientBind) SetReservedForEndpoint(destination netip.AddrPort, reserved [3]byte) {
	b.mu.Lock()
	b.byEndpoint[destination] = reserved
	b.mu.Unlock()
}

func (*ownedClientBind) SetMark(uint32) error { return nil }
func (*ownedClientBind) BatchSize() int       { return 1 }
func (*ownedClientBind) ParseEndpoint(value string) (conn.Endpoint, error) {
	address, err := netip.ParseAddrPort(value)
	return parentEndpoint(address), err
}

type ownedParentPacket struct {
	net.PacketConn
	done chan struct{}
	once sync.Once
	err  error
}

func (p *ownedParentPacket) Close() error {
	p.once.Do(func() {
		close(p.done)
		p.err = p.PacketConn.Close()
	})
	return p.err
}

type parentEndpoint netip.AddrPort

func (parentEndpoint) ClearSrc()             {}
func (parentEndpoint) SrcToString() string   { return "" }
func (e parentEndpoint) DstToString() string { return netip.AddrPort(e).String() }
func (e parentEndpoint) DstToBytes() []byte {
	value, _ := netip.AddrPort(e).MarshalBinary()
	return value
}
func (e parentEndpoint) DstIP() netip.Addr { return netip.AddrPort(e).Addr() }
func (parentEndpoint) SrcIP() netip.Addr   { return netip.Addr{} }
