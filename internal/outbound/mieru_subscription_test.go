package outbound

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/Resinat/Resin/internal/subscription"
)

func TestMieruSubscriptionBuildsEightIndependentOutbounds(t *testing.T) {
	var source strings.Builder
	for index := 1; index <= 8; index++ {
		fmt.Fprintf(&source, "- { name: test-%d, type: mieru, server: 192.0.2.%d, port: 2012, transport: TCP, handshake-mode: HANDSHAKE_NO_WAIT, username: example-user, password: example-password, multiplexing: MULTIPLEXING_HIGH }\n", index, index)
	}
	nodes, err := subscription.ParseGeneralSubscription([]byte(source.String()))
	if err != nil || len(nodes) != 8 {
		t.Fatalf("Mieru subscription parse: count=%d err=%v", len(nodes), err)
	}
	builder := newTestSingboxBuilder(t)
	defer builder.Close()
	for _, parsed := range nodes {
		outbound, err := builder.Build(parsed.RawOptions)
		if err != nil {
			t.Fatalf("build parsed Mieru node %s: %v", parsed.Tag, err)
		}
		if outbound.Type() != "mieru" || outbound.Tag() != parsed.Tag ||
			!slices.Contains(outbound.Network(), "tcp") || !slices.Contains(outbound.Network(), "udp") {
			t.Errorf("Mieru identity or TCP/UDP capability was lost: type=%s tag=%s network=%v", outbound.Type(), outbound.Tag(), outbound.Network())
		}
		if err := outbound.(io.Closer).Close(); err != nil {
			t.Errorf("close Mieru node: %v", err)
		}
	}
}
