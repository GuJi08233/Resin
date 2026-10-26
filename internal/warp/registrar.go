package warp

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

const apiBase = "https://api.cloudflareclient.com/v0i1909051800"

const maxEndpointCandidates = 64

var fallbackEndpoints = []netip.AddrPort{
	netip.MustParseAddrPort("162.159.192.1:2408"),
	netip.MustParseAddrPort("[2606:4700:d0::a29f:c001]:2408"),
	netip.MustParseAddrPort("162.159.193.1:2408"),
}

type registrationEndpoint struct {
	V4    string `json:"v4"`
	V6    string `json:"v6"`
	Host  string `json:"host"`
	Ports []int  `json:"ports"`
}

type registration struct {
	id, privateKey, peerKey string
	addresses               []netip.Prefix
	endpoints               []netip.AddrPort
	reserved                []byte
}

// Registrar shares a registration rate limit across all parent nodes.
type registrar struct {
	base          string
	gap           time.Duration
	mu            sync.Mutex
	next          time.Time
	cooldownUntil time.Time
	rateFailures  int
	changed       chan struct{}
}

func (r *registrar) wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.mu.Lock()
		now := time.Now()
		if limited := r.currentRateLimitLocked(now); limited != nil {
			r.mu.Unlock()
			return limited // preserve the attempt budget for actual tunnel work
		}
		delay := r.next.Sub(now)
		if delay <= 0 {
			r.next = now.Add(r.gap)
			r.mu.Unlock()
			return nil
		}
		if r.changed == nil {
			r.changed = make(chan struct{})
		}
		changed := r.changed
		r.mu.Unlock()
		// Recheck the shared deadline after every wakeup. Reserving future
		// slots in advance lets stale workers overwrite a later 429 pause.
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (r *registrar) register(ctx context.Context, parent adapter.Outbound) (registration, error) {
	if err := r.wait(ctx); err != nil {
		return registration{}, err
	}
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return registration{}, err
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return parent.DialContext(ctx, "tcp", M.ParseSocksaddr(address))
		},
		TLSHandshakeTimeout: 10 * time.Second,
		DisableKeepAlives:   true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	payload := map[string]string{
		"key":        base64.StdEncoding.EncodeToString(key.PublicKey().Bytes()),
		"install_id": "", "fcm_token": "", "tos": "2021-09-14T17:00:00.000+00:00",
		"model": "PC", "serial_number": "", "locale": "en_US",
	}
	var created device
	if err := r.request(ctx, client, http.MethodPost, "/reg", "", payload, &created); err != nil {
		return registration{}, fmt.Errorf("register WARP device: %w", err)
	}
	if created.ID == "" || created.Token == "" {
		return registration{}, errors.New("WARP registration omitted device credentials")
	}
	path := "/reg/" + created.ID
	var enabled device
	if err := r.request(ctx, client, http.MethodPatch, path, created.Token, map[string]bool{"warp_enabled": true}, &enabled); err != nil {
		return registration{}, fmt.Errorf("enable WARP: %w", err)
	}
	var current device
	if err := r.request(ctx, client, http.MethodGet, path, created.Token, nil, &current); err != nil {
		return registration{}, fmt.Errorf("read WARP configuration: %w", err)
	}
	if !current.Enabled {
		return registration{}, errors.New("WARP device is not enabled")
	}
	reg := registration{id: created.ID, privateKey: base64.StdEncoding.EncodeToString(key.Bytes())}
	for _, raw := range []string{current.Config.Interface.Addresses.V4, current.Config.Interface.Addresses.V6} {
		if raw == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(raw); err == nil {
			reg.addresses = append(reg.addresses, prefix)
			continue
		}
		if addr, err := netip.ParseAddr(raw); err == nil {
			addr = addr.Unmap()
			reg.addresses = append(reg.addresses, netip.PrefixFrom(addr, addr.BitLen()))
		}
	}
	if len(current.Config.Peers) == 0 || len(reg.addresses) == 0 {
		return registration{}, errors.New("WARP configuration omitted peer or interface addresses")
	}
	peer := current.Config.Peers[0]
	reg.peerKey = peer.PublicKey
	if decoded, err := base64.StdEncoding.DecodeString(current.Config.ClientID); err == nil && len(decoded) == 3 {
		reg.reserved = decoded
	}
	reg.endpoints = endpointCandidates(ctx, client, peer.Endpoint)
	if key, err := base64.StdEncoding.DecodeString(reg.peerKey); err != nil || len(key) != 32 {
		return registration{}, errors.New("WARP configuration contains an invalid peer key")
	}
	r.mu.Lock()
	if !r.cooldownUntil.After(time.Now()) {
		r.rateFailures = 0
	}
	r.mu.Unlock()
	return reg, nil
}

// endpointCandidates preserves the API's numeric endpoints before alternate
// ports. A host-only response is resolved over the same parent-backed HTTP
// client as registration, never through the host's default DNS resolver.
func endpointCandidates(ctx context.Context, client *http.Client, endpoint registrationEndpoint) []netip.AddrPort {
	bases := make([]netip.AddrPort, 0, 4)
	for _, raw := range []string{endpoint.V4, endpoint.V6, endpoint.Host} {
		if parsed, err := parseEndpoint(raw); err == nil {
			bases = append(bases, netip.AddrPortFrom(parsed.Addr().Unmap(), parsed.Port()))
		}
	}
	if len(bases) == 0 {
		if host, port, ok := endpointHostname(endpoint.Host); ok {
			for _, address := range resolveEndpointHost(ctx, client, host) {
				bases = append(bases, netip.AddrPortFrom(address, port))
			}
		}
	}
	result := make([]netip.AddrPort, 0, min(maxEndpointCandidates, len(bases)*(len(endpoint.Ports)+2)+len(fallbackEndpoints)))
	seen := make(map[netip.AddrPort]bool)
	appendCandidate := func(candidate netip.AddrPort, limit int) {
		if !candidate.IsValid() || candidate.Port() == 0 || seen[candidate] || len(result) >= limit {
			return
		}
		seen[candidate] = true
		result = append(result, candidate)
	}
	// Leave room for the known fallback addresses even in a large API response.
	apiLimit := maxEndpointCandidates - len(fallbackEndpoints)
	for _, base := range bases {
		appendCandidate(base, apiLimit)
	}
	ports := append([]int{2408}, endpoint.Ports...)
	for _, port := range ports {
		if port < 1 || port > 65535 {
			continue
		}
		for _, base := range bases {
			appendCandidate(netip.AddrPortFrom(base.Addr(), uint16(port)), apiLimit)
		}
		if len(result) >= apiLimit {
			break
		}
	}
	for _, fallback := range fallbackEndpoints {
		appendCandidate(fallback, maxEndpointCandidates)
	}
	return result
}

func endpointHostname(raw string) (string, uint16, bool) {
	host := strings.TrimSpace(raw)
	port := uint16(2408)
	if parsedHost, parsedPort, err := net.SplitHostPort(host); err == nil {
		number, err := strconv.Atoi(parsedPort)
		if err != nil || number < 1 || number > 65535 {
			return "", 0, false
		}
		host, port = parsedHost, uint16(number)
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if len(host) == 0 || len(host) > 253 {
		return "", 0, false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", 0, false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return "", 0, false
			}
		}
	}
	return host, port, true
}

func resolveEndpointHost(ctx context.Context, client *http.Client, host string) []netip.Addr {
	if client == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var addresses []netip.Addr
	for _, family := range []struct {
		name string
		kind int
	}{{"A", 1}, {"AAAA", 28}} {
		if ctx.Err() != nil {
			break
		}
		target := "https://1.1.1.1/dns-query?" + url.Values{"name": {host}, "type": {family.name}}.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Accept", "application/dns-json")
		response, err := client.Do(req)
		if err != nil {
			continue
		}
		var reply struct {
			Status int `json:"Status"`
			Answer []struct {
				Type int    `json:"type"`
				Data string `json:"data"`
			} `json:"Answer"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&reply)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || decodeErr != nil || reply.Status != 0 {
			continue
		}
		for _, answer := range reply.Answer {
			address, err := netip.ParseAddr(strings.TrimSpace(answer.Data))
			if err != nil || answer.Type != family.kind || (family.kind == 1) != address.Unmap().Is4() {
				continue
			}
			addresses = append(addresses, address.Unmap())
			if len(addresses) >= maxEndpointCandidates-len(fallbackEndpoints) {
				return addresses
			}
		}
	}
	return addresses
}

func parseEndpoint(value string) (netip.AddrPort, error) {
	value = strings.TrimSpace(value)
	if endpoint, err := netip.ParseAddrPort(value); err == nil && endpoint.Port() != 0 {
		return endpoint, nil
	}
	if addr, err := netip.ParseAddr(value); err == nil {
		return netip.AddrPortFrom(addr, 2408), nil
	}
	return netip.AddrPort{}, fmt.Errorf("non-numeric WARP endpoint %q", value)
}

func (r *registrar) request(ctx context.Context, client *http.Client, method, path, token string, payload any, target *device) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	limited := r.currentRateLimitLocked(time.Now())
	r.mu.Unlock()
	if limited != nil {
		return limited
	}
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(r.base, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "okhttp/3.12.1")
	req.Header.Set("CF-Client-Version", "a-6.10-2158")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return r.rateLimited(resp.Header.Get("Retry-After"))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// API error bodies can contain device credentials. Keep them out of logs.
		return fmt.Errorf("Cloudflare API HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("Cloudflare API response exceeds limit")
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("decode Cloudflare API: %w", err)
	}
	if len(envelope.Result) > 0 && string(envelope.Result) != "null" {
		data = envelope.Result
	}
	return json.Unmarshal(data, target)
}

// Only the fields required to establish a tunnel are decoded.
type device struct {
	ID      string `json:"id"`
	Token   string `json:"token"`
	Enabled bool   `json:"warp_enabled"`
	Config  struct {
		ClientID  string `json:"client_id"`
		Interface struct {
			Addresses struct {
				V4 string `json:"v4"`
				V6 string `json:"v6"`
			} `json:"addresses"`
		} `json:"interface"`
		Peers []struct {
			PublicKey string               `json:"public_key"`
			Endpoint  registrationEndpoint `json:"endpoint"`
		} `json:"peers"`
	} `json:"config"`
}
