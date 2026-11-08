// Package model defines domain structs shared across the persistence layer.
package model

import "encoding/json"

// Platform represents a routing platform.
type Platform struct {
	ID                               string `json:"id"`
	Name                             string `json:"name"`
	StickyTTLNs                      int64  `json:"sticky_ttl_ns"`
	RegexFilters                     []string
	RegionFilters                    []string
	IPInfoFilters                    PlatformIPInfoFilter
	ReverseProxyMissAction           string                   `json:"reverse_proxy_miss_action"`
	ReverseProxyEmptyAccountBehavior string                   `json:"reverse_proxy_empty_account_behavior"`
	ReverseProxyFixedAccountHeader   string                   `json:"reverse_proxy_fixed_account_header"`
	AllocationPolicy                 string                   `json:"allocation_policy"`
	PassiveCircuitBreakerDisabled    bool                     `json:"passive_circuit_breaker_disabled"`
	StatusCodeRules                  []PlatformStatusCodeRule `json:"status_code_rules"`
	UpdatedAtNs                      int64                    `json:"updated_at_ns"`
}

// PlatformIPInfoFilter constrains platform routability by the node's IPPure
// egress IP profile. Nil score bounds and empty type/native values mean
// "no constraint". ASNs hold canonical entries without the "AS" prefix; a
// leading "!" marks an exclusion.
type PlatformIPInfoFilter struct {
	MinFraudScore *int     `json:"min_fraud_score,omitempty"`
	MaxFraudScore *int     `json:"max_fraud_score,omitempty"`
	IPType        string   `json:"ip_type,omitempty"` // any | residential | hosting
	Native        string   `json:"native,omitempty"`  // any | native | broadcast
	ASNs          []string `json:"asns,omitempty"`
}

// PlatformStatusCodeRule maps an inclusive HTTP status code range to a
// platform-scoped egress IP action.
type PlatformStatusCodeRule struct {
	CodeFrom   int    `json:"code_from"`
	CodeTo     int    `json:"code_to"`
	Action     string `json:"action"`
	CooldownNs int64  `json:"cooldown_ns,omitempty"`
}

// PlatformEgressExclusion is a persisted platform-scoped cooldown or
// blacklist keyed by egress IP. ExpiresAtNs is 0 for permanent blacklists.
type PlatformEgressExclusion struct {
	PlatformID  string `json:"platform_id"`
	EgressIP    string `json:"egress_ip"`
	Action      string `json:"action"`
	StatusCode  int    `json:"status_code"`
	NodeHash    string `json:"node_hash"`
	CreatedAtNs int64  `json:"created_at_ns"`
	ExpiresAtNs int64  `json:"expires_at_ns"`
}

// Subscription represents a node subscription source.
type Subscription struct {
	ID                        string `json:"id"`
	Name                      string `json:"name"`
	SourceType                string `json:"source_type"`
	URL                       string `json:"url"`
	Content                   string `json:"content"`
	UpdateIntervalNs          int64  `json:"update_interval_ns"`
	Enabled                   bool   `json:"enabled"`
	Ephemeral                 bool   `json:"ephemeral"`
	IncrementalAliveNodes     bool   `json:"incremental_alive_nodes"`
	EphemeralNodeEvictDelayNs int64  `json:"ephemeral_node_evict_delay_ns"`
	CreatedAtNs               int64  `json:"created_at_ns"`
	UpdatedAtNs               int64  `json:"updated_at_ns"`
}

// Endpoint represents a persisted custom inbound listener. The environment-
// defined default endpoint is synthesized at runtime and is never stored here.
type Endpoint struct {
	ID                   string `json:"id"`
	Port                 int    `json:"port"`
	Enabled              bool   `json:"enabled"`
	AllowManagement      bool   `json:"allow_management"`
	AllowProxy           bool   `json:"allow_proxy"`
	RequireProxyAuthInfo bool   `json:"require_proxy_auth_info"`
	AllowHTTPForward     bool   `json:"allow_http_forward"`
	AllowHTTPReverse     bool   `json:"allow_http_reverse"`
	AllowSOCKS5          bool   `json:"allow_socks5"`
	CreatedAtNs          int64  `json:"created_at_ns"`
	UpdatedAtNs          int64  `json:"updated_at_ns"`
}

// AccountHeaderRule defines header extraction rules for reverse proxy account matching.
type AccountHeaderRule struct {
	URLPrefix   string `json:"url_prefix"`
	Headers     []string
	UpdatedAtNs int64 `json:"updated_at_ns"`
}

// NodeStatic holds the immutable portion of a node's data.
type NodeStatic struct {
	Hash        string          `json:"hash"`
	RawOptions  json.RawMessage `json:"raw_options_json"`
	CreatedAtNs int64           `json:"created_at_ns"`
}

// NodeDynamic holds the mutable runtime state of a node.
type NodeDynamic struct {
	Hash                               string `json:"hash"`
	FailureCount                       int    `json:"failure_count"`
	CircuitOpenSince                   int64  `json:"circuit_open_since"`
	EgressIP                           string `json:"egress_ip"`
	EgressRegion                       string `json:"egress_region"`
	EgressUpdatedAtNs                  int64  `json:"egress_updated_at_ns"`
	LastLatencyProbeAttemptNs          int64  `json:"last_latency_probe_attempt_ns"`
	LastAuthorityLatencyProbeAttemptNs int64  `json:"last_authority_latency_probe_attempt_ns"`
	LastEgressUpdateAttemptNs          int64  `json:"last_egress_update_attempt_ns"`
	// IPInfoJSON is the last successful IPPure reputation sample, JSON-encoded.
	// Empty when the node has no stored profile.
	IPInfoJSON               string `json:"ipinfo_json"`
	LastIPInfoProbeAttemptNs int64  `json:"last_ipinfo_probe_attempt_ns"`
}

// NodeLatency holds per-domain latency statistics for a node.
type NodeLatency struct {
	NodeHash      string `json:"node_hash"`
	Domain        string `json:"domain"`
	EwmaNs        int64  `json:"ewma_ns"`
	LastUpdatedNs int64  `json:"last_updated_ns"`
}

// NodeLatencyKey is the composite primary key for node_latency.
type NodeLatencyKey struct {
	NodeHash string
	Domain   string
}

// Lease represents a sticky routing lease.
type Lease struct {
	PlatformID     string `json:"platform_id"`
	Account        string `json:"account"`
	NodeHash       string `json:"node_hash"`
	EgressIP       string `json:"egress_ip"`
	CreatedAtNs    int64  `json:"created_at_ns"`
	ExpiryNs       int64  `json:"expiry_ns"`
	LastAccessedNs int64  `json:"last_accessed_ns"`
}

// LeaseKey is the composite primary key for leases.
type LeaseKey struct {
	PlatformID string
	Account    string
}

// SubscriptionNode links a subscription to a node with tags.
type SubscriptionNode struct {
	SubscriptionID string `json:"subscription_id"`
	NodeHash       string `json:"node_hash"`
	Tags           []string
	Evicted        bool `json:"evicted"`
}

// SubscriptionNodeKey is the composite primary key for subscription_nodes.
type SubscriptionNodeKey struct {
	SubscriptionID string
	NodeHash       string
}
