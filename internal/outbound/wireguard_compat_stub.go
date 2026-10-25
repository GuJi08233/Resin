//go:build !with_gvisor

package outbound

import (
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
)

func (*SingboxBuilder) buildWireGuardCompat(option.Outbound, log.ContextLogger) (adapter.Outbound, bool, error) {
	return nil, false, nil
}
