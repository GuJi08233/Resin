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
	"strings"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

const apiBase = "https://api.cloudflareclient.com/v0i1909051800"

type registration struct {
	id, privateKey, peerKey string
	addresses               []netip.Prefix
	endpoints               []netip.AddrPort
	reserved                []byte
}

// Registrar shares a registration rate limit across all parent nodes.
type registrar struct {
	base string
	gap  time.Duration
	mu   sync.Mutex
	next time.Time
}

func (r *registrar) wait(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if delay := time.Until(r.next); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.next = time.Now().Add(r.gap)
	return nil
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
	for _, raw := range []string{peer.Endpoint.V4, peer.Endpoint.V6} {
		if endpoint, err := parseEndpoint(raw); err == nil {
			reg.endpoints = append(reg.endpoints, endpoint)
		}
	}
	// Numeric endpoints avoid host DNS and work when the API returns only a hostname.
	for _, raw := range []string{"162.159.192.1:2408", "[2606:4700:d0::a29f:c001]:2408", "162.159.193.1:2408"} {
		endpoint := netip.MustParseAddrPort(raw)
		found := false
		for _, existing := range reg.endpoints {
			if existing == endpoint {
				found = true
				break
			}
		}
		if !found {
			reg.endpoints = append(reg.endpoints, endpoint)
		}
	}
	if key, err := base64.StdEncoding.DecodeString(reg.peerKey); err != nil || len(key) != 32 {
		return registration{}, errors.New("WARP configuration contains an invalid peer key")
	}
	return reg, nil
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
			PublicKey string `json:"public_key"`
			Endpoint  struct {
				V4    string `json:"v4"`
				V6    string `json:"v6"`
				Host  string `json:"host"`
				Ports []int  `json:"ports"`
			} `json:"endpoint"`
		} `json:"peers"`
	} `json:"config"`
}
