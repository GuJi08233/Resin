package subscription

import "time"

const (
	WarpSubscriptionID   = "system-warp"
	WarpSubscriptionName = "WARP"
	SourceTypeWarp       = "warp"
)

// NewWarpSubscription represents the automatically maintained runtime exits.
// Its settings are persisted in RuntimeConfig, not as a fetched subscription.
func NewWarpSubscription(enabled bool) *Subscription {
	sub := NewSubscription(WarpSubscriptionID, WarpSubscriptionName, "", enabled, false)
	sub.SetSourceType(SourceTypeWarp)
	sub.CreatedAtNs = time.Now().UnixNano()
	sub.UpdatedAtNs = sub.CreatedAtNs
	return sub
}
