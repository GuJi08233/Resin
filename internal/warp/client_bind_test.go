//go:build with_gvisor

package warp

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/wireguard-go/conn"
)

func TestClientBindCloseRejectsLateParentDial(t *testing.T) {
	packet := &blockingParentPacket{closed: make(chan struct{})}
	defer packet.Close()
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	parent := &transportParent{packet: func(context.Context, M.Socksaddr) (net.PacketConn, error) {
		close(started)
		<-release
		return packet, nil
	}}
	bind := newOwnedClientBind(context.Background(), parentPacketDialer{parent: parent}, netip.MustParseAddrPort("162.159.192.1:2408"), [3]byte{})
	receivers, _, err := bind.Open(0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := receivers[0]([][]byte{make([]byte, 2048)}, make([]int, 1), make([]conn.Endpoint, 1))
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("bind did not start dialing its parent")
	}
	closed := make(chan error, 1)
	go func() { closed <- bind.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("closing the bind waited for the parent's unfinished dial")
	}
	// Release the deliberately non-cancellable dial after Close has returned.
	// Its result must be closed, rather than resurrecting the old binding.
	release <- struct{}{}
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("closed receive returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("late parent dial kept the closed receive alive")
	}
	select {
	case <-packet.closed:
	default:
		t.Fatal("late parent packet connection was not released")
	}
}

func TestClientBindMultiplePeersPreservesReservedBytes(t *testing.T) {
	outer, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Close()
	peer, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	_ = peer.SetDeadline(time.Now().Add(time.Second))
	parent := &transportParent{packet: func(context.Context, M.Socksaddr) (net.PacketConn, error) { return outer, nil }}
	bind := newOwnedClientBind(context.Background(), parentPacketDialer{parent: parent}, netip.AddrPort{}, [3]byte{9, 9, 9})
	defer bind.Close()
	receivers, _, err := bind.Open(0)
	if err != nil {
		t.Fatal(err)
	}
	address := peer.LocalAddr().(*net.UDPAddr).AddrPort()
	bind.SetReservedForEndpoint(address, [3]byte{1, 2, 3})
	endpoint, err := bind.ParseEndpoint(address.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := bind.Send([][]byte{{4, 0, 0, 0, 42}}, endpoint); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 2048)
	n, _, err := peer.ReadFrom(buffer)
	if err != nil || string(buffer[:n]) != string([]byte{4, 1, 2, 3, 42}) {
		t.Fatalf("parent packet lost its peer's reserved bytes: %v %v", buffer[:n], err)
	}
	if _, err := peer.WriteTo([]byte{4, 1, 2, 3, 42}, outer.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	sizes, endpoints := make([]int, 1), make([]conn.Endpoint, 1)
	count, err := receivers[0]([][]byte{buffer}, sizes, endpoints)
	if err != nil || count != 1 || sizes[0] != 5 || string(buffer[:5]) != string([]byte{4, 0, 0, 0, 42}) || endpoints[0].DstToString() != address.String() {
		t.Fatalf("received peer packet did not restore WireGuard framing: count=%d size=%d data=%v endpoint=%v err=%v", count, sizes[0], buffer[:5], endpoints[0], err)
	}
}
