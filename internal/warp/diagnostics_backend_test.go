//go:build with_gvisor

package warp

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"

	M "github.com/sagernet/sing/common/metadata"
)

func TestUserspaceEndpointReportsSafeRealHandshakeSnapshot(t *testing.T) {
	packet := &blockingParentPacket{closed: make(chan struct{})}
	defer packet.Close()
	parent := &transportParent{packet: func(context.Context, M.Socksaddr) (net.PacketConn, error) { return packet, nil }}
	reg := registration{id: "SECRET-DEVICE", privateKey: "eCtXsJZ27+4PbhDkHnB923tkUn2Gj59wZw5wFA75MnU=", peerKey: "Cr8hWlKvtDt7nrvf+f0brNQQzabAqrjfBvas9pmowjo="}
	exit, err := newTunnel(parent, IPv4, reg, []netip.Prefix{netip.MustParsePrefix("172.16.0.2/32")}, netip.MustParseAddrPort("162.159.192.1:2408"))
	if err != nil {
		t.Fatal(err)
	}
	defer exit.Close()
	message := exit.diagnosticError("tcp_connect", context.DeadlineExceeded).Error()
	if !strings.Contains(message, "wg_handshake=none handshake_unix=0") {
		t.Fatalf("real idle backend did not expose its unestablished handshake: %s", message)
	}
	for _, secret := range []string{reg.id, reg.privateKey, reg.peerKey, "private_key=", "public_key=", "preshared_key="} {
		if strings.Contains(message, secret) {
			t.Fatalf("real backend diagnostic included secret/key %q", secret)
		}
	}
}
