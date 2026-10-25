// Package warp creates Cloudflare WARP exits over existing proxy outbounds.
// Registration and WireGuard packets both use the supplied parent; this package
// never opens a direct socket or changes the host's interfaces or routing table.
// The WARP protocol flow follows the local opencode2api reference. Tunnel
// implementation is provided by the project's existing sing-box dependency.
package warp
