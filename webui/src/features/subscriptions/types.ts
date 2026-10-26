export type Subscription = {
  id: string;
  name: string;
  source_type: "remote" | "local" | "warp";
  managed?: boolean;
  max_nodes?: number;
  url: string;
  content: string;
  update_interval: string;
  node_count: number;
  healthy_node_count: number;
  ephemeral: boolean;
  incremental_alive_nodes: boolean;
  ephemeral_node_evict_delay: string;
  enabled: boolean;
  created_at: string;
  last_checked?: string;
  last_updated?: string;
  last_error?: string;
  maintenance_status?: "starting" | "maintaining" | "stalled" | "disabled";
  last_maintenance?: string;
};

export type PageResponse<T> = {
  items: T[];
  total: number;
  limit: number;
  offset: number;
};

export type SubscriptionCreateInput = {
  name: string;
  source_type?: "remote" | "local";
  url?: string;
  content?: string;
  update_interval?: string;
  enabled?: boolean;
  ephemeral?: boolean;
  incremental_alive_nodes?: boolean;
  ephemeral_node_evict_delay?: string;
};

export type SubscriptionUpdateInput = {
  max_nodes?: number;
  name?: string;
  url?: string;
  content?: string;
  update_interval?: string;
  enabled?: boolean;
  ephemeral?: boolean;
  incremental_alive_nodes?: boolean;
  ephemeral_node_evict_delay?: string;
};
