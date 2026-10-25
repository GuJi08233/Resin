//go:build !with_gvisor

package warp

import (
	"context"
	"errors"
	"net/netip"

	"github.com/sagernet/sing-box/adapter"
)

func Supported() bool { return false }

func createEndpoint(context.Context, adapter.Outbound, registration, []netip.Prefix, netip.AddrPort) (tunnelEndpoint, error) {
	return nil, errors.New("WARP requires a build with with_gvisor")
}
