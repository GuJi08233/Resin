package warp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

type endpointRoundTripper func(*http.Request) (*http.Response, error)

func (f endpointRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestEndpointCandidatesKeepOriginalPortsAndAddAPIAlternates(t *testing.T) {
	client := &http.Client{Transport: endpointRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("numeric API endpoints must not trigger DNS")
		return nil, nil
	})}
	got := endpointCandidates(context.Background(), client, registrationEndpoint{
		V4: "198.51.100.1:987", V6: "[2001:db8::1]:988", Host: "unused.invalid:989",
		Ports: []int{0, -1, 65536, 2408, 4500, 500, 4500},
	})
	wantPrefix := []netip.AddrPort{
		netip.MustParseAddrPort("198.51.100.1:987"), netip.MustParseAddrPort("[2001:db8::1]:988"),
		netip.MustParseAddrPort("198.51.100.1:2408"), netip.MustParseAddrPort("[2001:db8::1]:2408"),
		netip.MustParseAddrPort("198.51.100.1:4500"), netip.MustParseAddrPort("[2001:db8::1]:4500"),
		netip.MustParseAddrPort("198.51.100.1:500"), netip.MustParseAddrPort("[2001:db8::1]:500"),
	}
	if len(got) != len(wantPrefix)+len(fallbackEndpoints) || !slices.Equal(got[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("API endpoint preference/ports changed: %v", got)
	}
	if !slices.Equal(got[len(wantPrefix):], fallbackEndpoints) {
		t.Fatalf("fallbacks are not last: %v", got)
	}
}

func TestEndpointCandidatesResolveHostOnlyViaProvidedHTTPClient(t *testing.T) {
	var queries []string
	client := &http.Client{Transport: endpointRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme != "https" || request.URL.Host != "1.1.1.1" || request.URL.Path != "/dns-query" ||
			request.URL.Query().Get("name") != "warp-endpoint.invalid" || request.Header.Get("Accept") != "application/dns-json" {
			t.Fatalf("DNS did not use the explicit parent-backed DoH path: %s", request.URL)
		}
		if request.Header.Get("Authorization") != "" {
			t.Fatal("registration credentials leaked into DNS request")
		}
		family := request.URL.Query().Get("type")
		queries = append(queries, family)
		answer := `{"Status":0,"Answer":[{"type":1,"data":"198.51.100.2"},{"type":5,"data":"ignored.invalid"}]}`
		if family == "AAAA" {
			answer = `{"Status":0,"Answer":[{"type":28,"data":"2001:db8::2"}]}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(answer)), Header: make(http.Header)}, nil
	})}
	got := endpointCandidates(context.Background(), client, registrationEndpoint{Host: "WARP-ENDPOINT.invalid.:4500", Ports: []int{500}})
	if !slices.Equal(queries, []string{"A", "AAAA"}) {
		t.Fatalf("unexpected DNS requests %v", queries)
	}
	wantPrefix := []netip.AddrPort{
		netip.MustParseAddrPort("198.51.100.2:4500"), netip.MustParseAddrPort("[2001:db8::2]:4500"),
		netip.MustParseAddrPort("198.51.100.2:2408"), netip.MustParseAddrPort("[2001:db8::2]:2408"),
		netip.MustParseAddrPort("198.51.100.2:500"), netip.MustParseAddrPort("[2001:db8::2]:500"),
	}
	if len(got) < len(wantPrefix) || !slices.Equal(got[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("host-only candidates lost addresses/ports: %v", got)
	}
}

func TestEndpointCandidatesBoundUntrustedResponseAndDeduplicate(t *testing.T) {
	ports := make([]int, 1000)
	for index := range ports {
		ports[index] = index + 1
	}
	got := endpointCandidates(context.Background(), nil, registrationEndpoint{
		V4: "198.51.100.1:2408", V6: "[2001:db8::1]:2408", Host: "198.51.100.1:2408", Ports: ports,
	})
	if len(got) != maxEndpointCandidates || !slices.Equal(got[len(got)-len(fallbackEndpoints):], fallbackEndpoints) {
		t.Fatalf("candidate limit or fallbacks violated: count=%d endpoints=%v", len(got), got)
	}
	seen := make(map[netip.AddrPort]bool)
	for _, candidate := range got {
		if seen[candidate] || candidate.Port() == 0 {
			t.Fatalf("duplicate or invalid candidate %s", candidate)
		}
		seen[candidate] = true
	}
}

func TestEndpointCandidatesKeepFallbacksWhenDNSFailsOrIsCanceled(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if canceled {
				cancel()
			}
			calls := 0
			client := &http.Client{Transport: endpointRoundTripper(func(request *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(strings.NewReader(`{"Status":2}`)), Header: make(http.Header)}, nil
			})}
			got := endpointCandidates(ctx, client, registrationEndpoint{Host: "warp-endpoint.invalid"})
			if !slices.Equal(got, fallbackEndpoints) || (canceled && calls != 0) {
				t.Fatalf("DNS failure/cancellation lost safe fallbacks: calls=%d endpoints=%v", calls, got)
			}
		})
	}
}

func TestEndpointHostnameRejectsMalformedOrOutOfRangeInput(t *testing.T) {
	for _, invalid := range []string{"", "name.invalid:0", "name.invalid:65536", "name.invalid:-1", "https://name.invalid", "a..invalid", "-name.invalid", "name.invalid/path", "name.invalid?secret=x"} {
		if _, _, ok := endpointHostname(invalid); ok {
			t.Errorf("accepted invalid endpoint hostname %q", invalid)
		}
	}
	for _, value := range []string{"example.invalid", "example.invalid:65535"} {
		if _, _, ok := endpointHostname(value); !ok {
			t.Errorf("rejected valid endpoint hostname %q", value)
		}
	}
}
