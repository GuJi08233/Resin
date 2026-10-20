package routing_test

import (
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/platform"
	"github.com/Resinat/Resin/internal/routing"
	"github.com/Resinat/Resin/internal/topology"
)

func TestStatusCodeExclusionsApplyBeforeViewRebuild(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   int
		action platform.StatusCodeAction
	}{
		{name: "blacklist", code: 403, action: platform.StatusCodeActionBlacklist},
		{name: "cooldown", code: 429, action: platform.StatusCodeActionCooldown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, subMgr := setupPool(t)
			plat, _ := pool.GetPlatform(platID)
			rule := platform.StatusCodeRule{CodeFrom: tc.code, CodeTo: tc.code, Action: tc.action}
			if tc.action == platform.StatusCodeActionCooldown {
				rule.Cooldown = time.Hour
			}
			plat.StatusCodeRules = []platform.StatusCodeRule{rule}
			other := platform.NewPlatform("other-plat", "OtherPlat", nil, nil)
			pool.RegisterPlatform(other)

			guard := topology.NewPlatformGuard(pool)
			pool.SetPlatformGuard(guard)
			// Leave the worker stopped so routing must reject live exclusions
			// while every blocked node remains in the platform view.
			t.Cleanup(guard.Stop)

			blockedIP := netip.MustParseAddr("203.0.113.10")
			original := makeRoutableNode(t, pool, subMgr, `{"guard":"original"}`, blockedIP.String(), "cloudflare.com", time.Millisecond)
			router := makeRouter(pool, nil)
			for _, name := range []string{platName, other.Name} {
				res, err := router.RouteRequest(name, "existing-account", "example.com")
				if err != nil || res.NodeHash != original {
					t.Fatalf("create original lease on %s: result=%+v, err=%v", name, res, err)
				}
			}

			// Most candidates share the excluded IP. Picking the only eligible
			// node must succeed even when random samples hit blocked candidates.
			for i := 0; i < 15; i++ {
				makeRoutableNode(t, pool, subMgr, fmt.Sprintf(`{"guard":"shared-%d"}`, i), blockedIP.String(), "cloudflare.com", time.Millisecond)
			}
			allowedIP := netip.MustParseAddr("203.0.113.20")
			allowed := makeRoutableNode(t, pool, subMgr, `{"guard":"allowed"}`, allowedIP.String(), "cloudflare.com", time.Millisecond)
			pool.RecordUpstreamStatus(platID, original, blockedIP, tc.code)
			if plat.View().Size() != 17 || !plat.View().Contains(original) {
				t.Fatal("the test requires a stale view that still contains all blocked candidates")
			}

			for i := 0; i < 32; i++ {
				res, err := router.RouteRequest(platName, "", "example.com")
				if err != nil || res.NodeHash != allowed || res.EgressIP != allowedIP {
					t.Fatalf("random request %d must find the remaining IP: result=%+v, err=%v", i, res, err)
				}
			}
			for _, account := range []string{"existing-account", "new-account"} {
				res, err := router.RouteRequest(platName, account, "example.com")
				if err != nil || res.NodeHash != allowed || res.EgressIP != allowedIP || !res.LeaseCreated {
					t.Fatalf("sticky account %s must create a lease on the remaining IP: result=%+v, err=%v", account, res, err)
				}
			}
			res, err := router.RouteRequest(other.Name, "existing-account", "example.com")
			if err != nil || res.NodeHash != original || res.EgressIP != blockedIP || res.LeaseCreated {
				t.Fatalf("another platform must keep its original lease: result=%+v, err=%v", res, err)
			}

			pool.RecordUpstreamStatus(platID, allowed, allowedIP, tc.code)
			for _, account := range []string{"", "existing-account", "another-account"} {
				if _, err := router.RouteRequest(platName, account, "example.com"); !errors.Is(err, routing.ErrNoAvailableNodes) {
					t.Fatalf("account %q with every IP excluded: got %v, want ErrNoAvailableNodes", account, err)
				}
			}
			if _, err := router.RouteRequest(other.Name, "", "example.com"); err != nil {
				t.Fatalf("another platform must remain routable when all guarded IPs are excluded: %v", err)
			}
		})
	}
}
