//go:build with_gvisor

package warp

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
)

func TestCreateProbesParentUDPBeforeRegistration(t *testing.T) {
	// A pending registration gap would block any attempt that reaches it.
	factory := &Factory{registrar: &registrar{next: time.Now().Add(time.Hour)},
		probeUDP: func(context.Context, adapter.Outbound) error {
			return &ParentUDPError{Rejected: true, Err: errors.New("socks5: request rejected, code=7")}
		}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := factory.Create(ctx, probeParent(nil), IPv4)
	if parentErr, ok := AsParentUDPError(err); !ok || !parentErr.Rejected || ctx.Err() != nil {
		t.Fatalf("UDP refusal did not fail before registration: %v", err)
	}
}

func TestCreateReusesUnusedRegistrationAfterTunnelFailure(t *testing.T) {
	progress := 0
	ctx, cancel := context.WithTimeout(WithProgress(context.Background(), func() { progress++ }), 2*time.Second)
	defer cancel()
	reg := registration{id: "spare", addresses: []netip.Prefix{netip.MustParsePrefix("172.16.0.2/32")},
		endpoints: []netip.AddrPort{netip.MustParseAddrPort("162.159.192.1:2408")}, expires: time.Now().Add(time.Minute)}
	var used []string
	factory := &Factory{registrar: &registrar{next: time.Now().Add(time.Hour)}, spares: []registration{reg},
		probeUDP: func(context.Context, adapter.Outbound) error { return nil },
		openTunnel: func(tunnelCtx context.Context, _ adapter.Outbound, _ Family, reg registration, _ []netip.Prefix, _ netip.AddrPort) (*Exit, error) {
			if deadline, ok := tunnelCtx.Deadline(); !ok || time.Until(deadline) > tunnelBudget {
				t.Error("tunnel phase lost its own budget")
			}
			used = append(used, reg.id)
			return nil, errors.New("mock tunnel failure")
		}}
	for attempt := 0; attempt < registrationMaxTries; attempt++ {
		if _, err := factory.Create(ctx, probeParent(nil), IPv4); err == nil || ctx.Err() != nil {
			t.Fatalf("attempt %d waited for a new registration instead of reusing the spare: %v", attempt+1, err)
		}
	}
	if len(used) != registrationMaxTries || used[0] != "spare" || used[2] != "spare" {
		t.Fatalf("spare registration was not reused across attempts: %v", used)
	}
	if len(factory.spares) != 0 {
		t.Fatal("a registration exceeded its reuse limit")
	}
	if progress < registrationMaxTries*2 {
		t.Fatalf("attempt phases did not report progress: %d", progress)
	}
}
