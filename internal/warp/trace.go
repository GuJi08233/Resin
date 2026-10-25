package warp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
)

func parseTrace(body io.Reader, family Family) (netip.Addr, error) {
	scanner := bufio.NewScanner(io.LimitReader(body, 64<<10))
	var raw string
	enabled := false
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "ip":
			raw = strings.TrimSpace(value)
		case "warp":
			enabled = strings.TrimSpace(value) == "on" || strings.TrimSpace(value) == "plus"
		}
	}
	if err := scanner.Err(); err != nil {
		return netip.Addr{}, err
	}
	if !enabled {
		return netip.Addr{}, errors.New("Cloudflare trace does not confirm WARP")
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("invalid WARP trace IP: %w", err)
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return netip.Addr{}, errors.New("WARP trace IP is not public")
	}
	if (family == IPv4) != addr.Is4() {
		return netip.Addr{}, fmt.Errorf("WARP trace IP %s does not match %s", addr, family)
	}
	return addr, nil
}
