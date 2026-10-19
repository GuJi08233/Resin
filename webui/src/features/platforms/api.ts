import { apiRequest } from "../../lib/api-client";
import type {
  ListPlatformLeasesInput,
  PageResponse,
  Platform,
  PlatformCreateInput,
  PlatformExclusion,
  PlatformLease,
  PlatformStatusCodeRule,
  PlatformUpdateInput,
} from "./types";

const basePath = "/api/v1/platforms";

type ApiPlatform = Omit<Platform, "regex_filters" | "region_filters" | "status_code_rules"> & {
  regex_filters?: string[] | null;
  region_filters?: string[] | null;
  routable_node_count?: number | null;
  reverse_proxy_miss_action?: Platform["reverse_proxy_miss_action"] | null;
  reverse_proxy_empty_account_behavior?: Platform["reverse_proxy_empty_account_behavior"] | null;
  reverse_proxy_fixed_account_header?: string | null;
  passive_circuit_breaker_disabled?: boolean | null;
  status_code_rules?: Array<Partial<PlatformStatusCodeRule>> | null;
};

type ApiPlatformLease = Partial<PlatformLease>;
type ApiPlatformExclusion = Partial<PlatformExclusion>;

function parseMissAction(raw: ApiPlatform["reverse_proxy_miss_action"]): Platform["reverse_proxy_miss_action"] {
  if (raw === "TREAT_AS_EMPTY" || raw === "REJECT") {
    return raw;
  }
  throw new Error(`invalid reverse_proxy_miss_action: ${String(raw)}`);
}

function normalizeStatusCodeRules(raw: ApiPlatform["status_code_rules"]): PlatformStatusCodeRule[] {
  if (!Array.isArray(raw)) {
    return [];
  }
  const result: PlatformStatusCodeRule[] = [];
  for (const item of raw) {
    if (!item || typeof item !== "object") {
      continue;
    }
    const action =
      item.action === "COOLDOWN" || item.action === "BLACKLIST" ? item.action : null;
    if (!action) {
      continue;
    }
    const codeFrom = typeof item.code_from === "number" ? item.code_from : Number(item.code_from);
    const codeTo = typeof item.code_to === "number" ? item.code_to : Number(item.code_to);
    if (!Number.isInteger(codeFrom) || !Number.isInteger(codeTo)) {
      continue;
    }
    result.push({
      code_from: codeFrom,
      code_to: codeTo,
      action,
      cooldown: typeof item.cooldown === "string" && item.cooldown ? item.cooldown : undefined,
    });
  }
  return result;
}

function normalizePlatform(raw: ApiPlatform): Platform {
  return {
    ...raw,
    reverse_proxy_miss_action: parseMissAction(raw.reverse_proxy_miss_action),
    regex_filters: Array.isArray(raw.regex_filters) ? raw.regex_filters : [],
    region_filters: Array.isArray(raw.region_filters) ? raw.region_filters : [],
    routable_node_count: typeof raw.routable_node_count === "number" ? raw.routable_node_count : 0,
    status_code_rules: normalizeStatusCodeRules(raw.status_code_rules),
    reverse_proxy_empty_account_behavior:
      raw.reverse_proxy_empty_account_behavior === "RANDOM" ||
      raw.reverse_proxy_empty_account_behavior === "FIXED_HEADER" ||
      raw.reverse_proxy_empty_account_behavior === "ACCOUNT_HEADER_RULE"
        ? raw.reverse_proxy_empty_account_behavior
        : "RANDOM",
    reverse_proxy_fixed_account_header:
      typeof raw.reverse_proxy_fixed_account_header === "string" ? raw.reverse_proxy_fixed_account_header : "",
    passive_circuit_breaker_disabled:
      typeof raw.passive_circuit_breaker_disabled === "boolean" ? raw.passive_circuit_breaker_disabled : false,
  };
}

function normalizePlatformPage(raw: PageResponse<ApiPlatform>): PageResponse<Platform> {
  return {
    ...raw,
    items: raw.items.map(normalizePlatform),
  };
}

function normalizeLease(raw: ApiPlatformLease): PlatformLease {
  return {
    platform_id: typeof raw.platform_id === "string" ? raw.platform_id : "",
    account: typeof raw.account === "string" ? raw.account : "",
    node_hash: typeof raw.node_hash === "string" ? raw.node_hash : "",
    node_tag: typeof raw.node_tag === "string" ? raw.node_tag : "",
    egress_ip: typeof raw.egress_ip === "string" ? raw.egress_ip : "",
    expiry: typeof raw.expiry === "string" ? raw.expiry : "",
    last_accessed: typeof raw.last_accessed === "string" ? raw.last_accessed : "",
  };
}

function normalizeLeasePage(raw: PageResponse<ApiPlatformLease>): PageResponse<PlatformLease> {
  return {
    ...raw,
    items: raw.items.map(normalizeLease),
  };
}

export type ListPlatformsPageInput = {
  limit?: number;
  offset?: number;
  keyword?: string;
};

export async function listPlatforms(input: ListPlatformsPageInput = {}): Promise<PageResponse<Platform>> {
  const query = new URLSearchParams({
    limit: String(input.limit ?? 50),
    offset: String(input.offset ?? 0),
    sort_by: "name",
    sort_order: "asc",
  });
  const keyword = input.keyword?.trim();
  if (keyword) {
    query.set("keyword", keyword);
  }

  const data = await apiRequest<PageResponse<ApiPlatform>>(`${basePath}?${query.toString()}`);
  return normalizePlatformPage(data);
}

export async function getPlatform(id: string): Promise<Platform> {
  const data = await apiRequest<ApiPlatform>(`${basePath}/${id}`);
  return normalizePlatform(data);
}

export async function createPlatform(input: PlatformCreateInput): Promise<Platform> {
  const data = await apiRequest<ApiPlatform>(basePath, {
    method: "POST",
    body: input,
  });
  return normalizePlatform(data);
}

export async function updatePlatform(id: string, input: PlatformUpdateInput): Promise<Platform> {
  const data = await apiRequest<ApiPlatform>(`${basePath}/${id}`, {
    method: "PATCH",
    body: input,
  });
  return normalizePlatform(data);
}

export async function deletePlatform(id: string): Promise<void> {
  await apiRequest<void>(`${basePath}/${id}`, {
    method: "DELETE",
  });
}

export async function resetPlatform(id: string): Promise<Platform> {
  const data = await apiRequest<ApiPlatform>(`${basePath}/${id}/actions/reset-to-default`, {
    method: "POST",
  });
  return normalizePlatform(data);
}

export async function rebuildPlatform(id: string): Promise<void> {
  await apiRequest<{ status: "ok" }>(`${basePath}/${id}/actions/rebuild-routable-view`, {
    method: "POST",
  });
}

export async function listPlatformLeases(id: string, input: ListPlatformLeasesInput = {}): Promise<PageResponse<PlatformLease>> {
  const query = new URLSearchParams({
    limit: String(input.limit ?? 50),
    offset: String(input.offset ?? 0),
    sort_by: input.sort_by ?? "expiry",
    sort_order: input.sort_order ?? "asc",
  });

  const account = input.account?.trim();
  if (account) {
    query.set("account", account);
  }
  if (input.fuzzy !== undefined) {
    query.set("fuzzy", String(input.fuzzy));
  }

  const data = await apiRequest<PageResponse<ApiPlatformLease>>(`${basePath}/${id}/leases?${query.toString()}`);
  return normalizeLeasePage(data);
}

export async function deletePlatformLease(id: string, account: string): Promise<void> {
  await apiRequest<void>(`${basePath}/${id}/leases/${encodeURIComponent(account)}`, {
    method: "DELETE",
  });
}

export async function clearAllPlatformLeases(id: string): Promise<void> {
  await apiRequest<void>(`${basePath}/${id}/leases`, {
    method: "DELETE",
  });
}

function normalizePlatformExclusion(raw: ApiPlatformExclusion): PlatformExclusion {
  const action = raw.action === "COOLDOWN" || raw.action === "BLACKLIST" ? raw.action : "COOLDOWN";
  return {
    egress_ip: typeof raw.egress_ip === "string" ? raw.egress_ip : "",
    action,
    status_code: typeof raw.status_code === "number" ? raw.status_code : 0,
    node_hash: typeof raw.node_hash === "string" ? raw.node_hash : "",
    created_at: typeof raw.created_at === "string" ? raw.created_at : "",
    expires_at: typeof raw.expires_at === "string" ? raw.expires_at : null,
  };
}

export async function listPlatformExclusions(id: string): Promise<PlatformExclusion[]> {
  const data = await apiRequest<{ items?: ApiPlatformExclusion[] | null }>(
    `${basePath}/${id}/exclusions`,
  );
  if (!Array.isArray(data.items)) {
    return [];
  }
  return data.items.map(normalizePlatformExclusion);
}

export async function unblockPlatformEgress(id: string, egressIP: string): Promise<void> {
  await apiRequest<{ status: "ok" }>(`${basePath}/${id}/actions/unblock-egress-ip`, {
    method: "POST",
    body: { egress_ip: egressIP },
  });
}
