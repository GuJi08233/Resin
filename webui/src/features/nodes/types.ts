export type NodeTag = {
  subscription_id: string;
  subscription_name: string;
  tag: string;
};

export type NodeIPInfo = {
  ip: string;
  asn: number;
  as_organization: string;
  country: string;
  country_code: string;
  region: string;
  region_code: string;
  city: string;
  timezone: string;
  latitude: string;
  longitude: string;
  postal_code: string;
  fraud_score: number;
  risk_level:
    | "very_clean"
    | "clean"
    | "neutral"
    | "light_risk"
    | "medium_risk"
    | "high_risk";
  is_residential: boolean;
  is_broadcast: boolean;
  updated_at: string;
};

export type NodeSummary = {
  node_hash: string;
  parent_hash?: string;
  warp_family?: "ipv4" | "ipv6";
  created_at: string;
  enabled: boolean;
  display_tag?: string;
  has_outbound: boolean;
  last_error?: string;
  circuit_open_since?: string;
  failure_count: number;
  egress_ip?: string;
  ipinfo?: NodeIPInfo;
  last_ipinfo_probe_attempt?: string;
  reference_latency_ms?: number;
  region?: string;
  last_egress_update?: string;
  last_latency_probe_attempt?: string;
  last_authority_latency_probe_attempt?: string;
  last_egress_update_attempt?: string;
  tags: NodeTag[];
};

export type PageResponse<T> = {
  items: T[];
  total: number;
  limit: number;
  offset: number;
  unique_egress_ips: number;
  unique_healthy_egress_ips: number;
};

export type NodeSortBy = "tag" | "created_at" | "failure_count" | "region";
export type SortOrder = "asc" | "desc";

export type NodeListFilters = {
  platform_id?: string;
  subscription_id?: string;
  tag_keyword?: string;
  region?: string;
  egress_ip?: string;
  probed_since?: string;
  enabled?: boolean;
  circuit_open?: boolean;
  has_outbound?: boolean;
};

export type NodeListQuery = NodeListFilters & {
  sort_by?: NodeSortBy;
  sort_order?: SortOrder;
  limit?: number;
  offset?: number;
};

export type EgressProbeResult = {
  egress_ip: string;
  region?: string;
  latency_ewma_ms: number;
};

export type LatencyProbeResult = {
  latency_ewma_ms: number;
};
