package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/platform"
	"github.com/Resinat/Resin/internal/topology"
)

func TestHTTPProxy_E2EStatusCodeExclusionBeforeViewRebuild(t *testing.T) {
	for _, proxyKind := range []string{"forward", "reverse"} {
		for _, action := range []struct {
			name string
			code int
			rule platform.StatusCodeRule
		}{
			{
				name: "cooldown",
				code: http.StatusTooManyRequests,
				rule: platform.StatusCodeRule{
					CodeFrom: http.StatusTooManyRequests,
					CodeTo:   http.StatusTooManyRequests,
					Action:   platform.StatusCodeActionCooldown,
					Cooldown: time.Hour,
				},
			},
			{
				name: "blacklist",
				code: http.StatusForbidden,
				rule: platform.StatusCodeRule{
					CodeFrom: http.StatusForbidden,
					CodeTo:   http.StatusForbidden,
					Action:   platform.StatusCodeActionBlacklist,
				},
			},
		} {
			for _, identity := range []struct {
				name  string
				value string
			}{
				{name: "random", value: "plat"},
				{name: "sticky", value: "plat.acct"},
			} {
				t.Run(proxyKind+"/"+action.name+"/"+identity.name, func(t *testing.T) {
					env := newProxyE2EEnv(t)
					plat, ok := env.pool.GetPlatform("plat-id")
					if !ok {
						t.Fatal("platform missing from pool")
					}
					plat.StatusCodeRules = []platform.StatusCodeRule{action.rule}
					plat.ReverseProxyEmptyAccountBehavior = string(platform.ReverseProxyEmptyAccountBehaviorRandom)

					// 不启动后台 worker，确保第二次请求仍面对包含被排除节点的旧视图。
					guard := topology.NewPlatformGuard(env.pool)
					env.pool.SetPlatformGuard(guard)

					var upstreamCalls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						upstreamCalls.Add(1)
						w.WriteHeader(action.code)
						_, _ = io.WriteString(w, "status-code-trigger")
					}))
					defer upstream.Close()

					transports := NewOutboundTransportPool(OutboundTransportConfig{})
					defer transports.CloseAll()
					var handler http.Handler
					if proxyKind == "forward" {
						handler = NewForwardProxy(ForwardProxyConfig{
							ProxyToken:    "tok",
							Router:        env.router,
							Pool:          env.pool,
							Health:        env.pool,
							TransportPool: transports,
						})
					} else {
						handler = NewReverseProxy(ReverseProxyConfig{
							ProxyToken:     "tok",
							Router:         env.router,
							Pool:           env.pool,
							PlatformLookup: env.pool,
							Health:         env.pool,
							TransportPool:  transports,
						})
					}
					proxyServer := httptest.NewServer(handler)
					defer proxyServer.Close()

					clientTransport := &http.Transport{}
					defer clientTransport.CloseIdleConnections()
					requestURL := proxyServer.URL + "/tok/" + identity.value + "/http/" + strings.TrimPrefix(upstream.URL, "http://") + "/trigger"
					if proxyKind == "forward" {
						proxyURL, err := url.Parse(proxyServer.URL)
						if err != nil {
							t.Fatalf("parse proxy URL: %v", err)
						}
						clientTransport.Proxy = http.ProxyURL(proxyURL)
						requestURL = upstream.URL + "/trigger"
					}
					client := &http.Client{Transport: clientTransport, Timeout: 5 * time.Second}
					request := func() *http.Response {
						t.Helper()
						req, err := http.NewRequest(http.MethodGet, requestURL, nil)
						if err != nil {
							t.Fatalf("create request: %v", err)
						}
						if proxyKind == "forward" {
							req.Header.Set("Proxy-Authorization", basicAuth(identity.value, "tok"))
						}
						resp, err := client.Do(req)
						if err != nil {
							t.Fatalf("proxy request: %v", err)
						}
						if _, err := io.Copy(io.Discard, resp.Body); err != nil {
							_ = resp.Body.Close()
							t.Fatalf("read proxy response: %v", err)
						}
						_ = resp.Body.Close()
						return resp
					}

					first := request()
					if first.StatusCode != action.code {
						t.Fatalf("first response = %d (%s), want upstream %d", first.StatusCode, first.Header.Get("X-Resin-Error"), action.code)
					}
					exclusions := env.pool.ListPlatformExclusions(plat.ID)
					if len(exclusions) != 1 {
						t.Errorf("exclusions immediately after response = %+v, want one", exclusions)
					} else {
						entry := exclusions[0]
						if entry.EgressIP.String() != "203.0.113.10" || entry.Action != action.rule.Action || entry.StatusCode != action.code {
							t.Errorf("unexpected exclusion: %+v", entry)
						}
						if entry.ExpiresAt.IsZero() != (action.rule.Action == platform.StatusCodeActionBlacklist) {
							t.Errorf("unexpected exclusion expiry for %s: %v", action.rule.Action, entry.ExpiresAt)
						}
					}
					if plat.View().Size() != 1 {
						t.Fatal("expected stale routable view to retain its node before background rebuild")
					}

					second := request()
					if second.StatusCode != http.StatusServiceUnavailable || second.Header.Get("X-Resin-Error") != "NO_AVAILABLE_NODES" {
						t.Errorf("second response = %d (%s), want 503 NO_AVAILABLE_NODES", second.StatusCode, second.Header.Get("X-Resin-Error"))
					}
					if got := upstreamCalls.Load(); got != 1 {
						t.Errorf("upstream requests = %d, want 1: excluded egress must not receive the next request", got)
					}
				})
			}
		}
	}
}
