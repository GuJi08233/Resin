import { z } from "zod";
import { allocationPolicies, emptyAccountBehaviors, missActions, statusCodeActions } from "./constants";
import { parseHeaderLines, parseLinesToList } from "./formParsers";
import type {
  Platform,
  PlatformCreateInput,
  PlatformIPInfoFilters,
  PlatformStatusCodeAction,
  PlatformUpdateInput,
} from "./types";

const platformNameForbiddenChars = ".:|/\\@?#%~";
const platformNameForbiddenSpacing = " \t\r\n";
const platformNameReserved = "api";

function containsAny(source: string, chars: string): boolean {
  for (const ch of chars) {
    if (source.includes(ch)) {
      return true;
    }
  }
  return false;
}

export const platformNameRuleHint = "平台名不能包含 .:|/\\@?#%~、空格、Tab、换行、回车，也不能为保留字。";

export type PlatformStatusCodeRuleForm = {
  code_from: string;
  code_to: string;
  action: PlatformStatusCodeAction;
  cooldown: string;
};

type ParsedScore = { value: number | null; invalid: boolean };

// parseScoreField parses an optional 0-100 fraud score. An empty string means
// "no bound"; anything else must be an in-range integer.
export function parseScoreField(input: string | undefined): ParsedScore {
  const trimmed = (input ?? "").trim();
  if (!trimmed) {
    return { value: null, invalid: false };
  }
  const value = Number(trimmed);
  if (!Number.isInteger(value) || value < 0 || value > 100) {
    return { value: null, invalid: true };
  }
  return { value, invalid: false };
}

export type ASNFilterEntry = { raw: string; value: string; valid: boolean };

// parseASNFilterLines parses one ASN per line. Entries accept an optional
// "AS" prefix and an optional leading "!" negation, and are normalized to
// the canonical form the API stores ("64500" / "!64500").
export function parseASNFilterLines(input: string | undefined): ASNFilterEntry[] {
  return parseLinesToList(input).map((raw) => {
    let value = raw;
    let negative = false;
    if (value.startsWith("!")) {
      negative = true;
      value = value.slice(1).trim();
    }
    if (/^as/i.test(value)) {
      value = value.slice(2).trim();
    }
    const valid = /^\d+$/.test(value) && Number(value) >= 1 && Number(value) <= 4294967295;
    return {
      raw,
      value: valid ? `${negative ? "!" : ""}${Number(value)}` : raw,
      valid,
    };
  });
}

const statusCodeRuleFormSchema = z.object({
  code_from: z.string(),
  code_to: z.string(),
  action: z.enum(statusCodeActions),
  cooldown: z.string().optional(),
});

export const platformFormSchema = z.object({
  name: z.string().trim()
    .min(1, "平台名称不能为空")
    .refine((value) => !containsAny(value, platformNameForbiddenChars), {
      message: "平台名称不能包含字符 .:|/\\@?#%~",
    })
    .refine((value) => !containsAny(value, platformNameForbiddenSpacing), {
      message: "平台名称不能包含空格、Tab、换行、回车",
    })
    .refine((value) => value.toLowerCase() !== platformNameReserved, {
      message: "平台名称不能为保留字",
    }),
  sticky_ttl: z.string().optional(),
  regex_filters_text: z.string().optional(),
  region_filters_text: z.string().optional(),
  ipinfo_min_fraud_score: z.string().optional(),
  ipinfo_max_fraud_score: z.string().optional(),
  ipinfo_ip_type: z.enum(["any", "residential", "hosting"]),
  ipinfo_native: z.enum(["any", "native", "broadcast"]),
  ipinfo_asns_text: z.string().optional(),
  reverse_proxy_miss_action: z.enum(missActions),
  reverse_proxy_empty_account_behavior: z.enum(emptyAccountBehaviors),
  reverse_proxy_fixed_account_header: z.string().optional(),
  allocation_policy: z.enum(allocationPolicies),
  passive_circuit_breaker_disabled: z.boolean(),
  status_code_rules: z.array(statusCodeRuleFormSchema),
}).superRefine((value, ctx) => {
  if (
    value.reverse_proxy_empty_account_behavior === "FIXED_HEADER" &&
    parseHeaderLines(value.reverse_proxy_fixed_account_header).length === 0
  ) {
    ctx.addIssue({
      code: "custom",
      path: ["reverse_proxy_fixed_account_header"],
      message: "用于提取 Account 的 Headers 不能为空",
    });
  }

  const ranges: Array<{ from: number; to: number; index: number }> = [];
  (value.status_code_rules ?? []).forEach((rule, index) => {
    const fromText = rule.code_from.trim();
    const toText = rule.code_to.trim();
    const from = Number(fromText);
    // An empty end code means "match this single status code".
    const to = toText === "" ? from : Number(toText);
    const fromValid = fromText !== "" && Number.isInteger(from) && from >= 100 && from <= 599;
    const toValid = toText === "" || (Number.isInteger(to) && to >= 100 && to <= 599);
    if (!fromValid) {
      ctx.addIssue({
        code: "custom",
        path: ["status_code_rules", index, "code_from"],
        message: "状态码必须是 100 到 599 之间的整数",
      });
    }
    if (!toValid) {
      ctx.addIssue({
        code: "custom",
        path: ["status_code_rules", index, "code_to"],
        message: "状态码必须是 100 到 599 之间的整数",
      });
    }
    if (!fromValid || !toValid) {
      return;
    }
    if (from > to) {
      ctx.addIssue({
        code: "custom",
        path: ["status_code_rules", index, "code_to"],
        message: "起始状态码不能大于结束状态码",
      });
      return;
    }
    if (rule.action === "COOLDOWN" && !(rule.cooldown ?? "").trim()) {
      ctx.addIssue({
        code: "custom",
        path: ["status_code_rules", index, "cooldown"],
        message: "冷却规则必须填写冷却时长",
      });
    }
    ranges.push({ from, to, index });
  });

  ranges.sort((a, b) => a.from - b.from || a.to - b.to);
  for (let i = 1; i < ranges.length; i += 1) {
    if (ranges[i].from <= ranges[i - 1].to) {
      ctx.addIssue({
        code: "custom",
        path: ["status_code_rules", ranges[i].index, "code_from"],
        message: "状态码区间不能重叠",
      });
    }
  }

  const minScore = parseScoreField(value.ipinfo_min_fraud_score);
  const maxScore = parseScoreField(value.ipinfo_max_fraud_score);
  if (minScore.invalid) {
    ctx.addIssue({
      code: "custom",
      path: ["ipinfo_min_fraud_score"],
      message: "风险分必须是 0 到 100 之间的整数",
    });
  }
  if (maxScore.invalid) {
    ctx.addIssue({
      code: "custom",
      path: ["ipinfo_max_fraud_score"],
      message: "风险分必须是 0 到 100 之间的整数",
    });
  }
  if (!minScore.invalid && !maxScore.invalid && minScore.value !== null && maxScore.value !== null) {
    if (minScore.value > maxScore.value) {
      ctx.addIssue({
        code: "custom",
        path: ["ipinfo_max_fraud_score"],
        message: "最小风险分不能大于最大风险分",
      });
    }
  }

  parseASNFilterLines(value.ipinfo_asns_text).forEach((entry) => {
    if (!entry.valid) {
      ctx.addIssue({
        code: "custom",
        path: ["ipinfo_asns_text"],
        message: "ASN 格式无效（支持 64500、AS64500、!64500）",
      });
    }
  });
});

export type PlatformFormValues = z.infer<typeof platformFormSchema>;

export const defaultPlatformFormValues: PlatformFormValues = {
  name: "",
  sticky_ttl: "",
  regex_filters_text: "",
  region_filters_text: "",
  ipinfo_min_fraud_score: "",
  ipinfo_max_fraud_score: "",
  ipinfo_ip_type: "any",
  ipinfo_native: "any",
  ipinfo_asns_text: "",
  reverse_proxy_miss_action: "TREAT_AS_EMPTY",
  reverse_proxy_empty_account_behavior: "RANDOM",
  reverse_proxy_fixed_account_header: "Authorization",
  allocation_policy: "BALANCED",
  passive_circuit_breaker_disabled: false,
  status_code_rules: [],
};

// countIPInfoFilterConditions counts the active constraints in an IP profile
// filter, used for the platform list card badge.
export function countIPInfoFilterConditions(filters: PlatformIPInfoFilters | undefined): number {
  if (!filters) {
    return 0;
  }
  let count = 0;
  if (filters.min_fraud_score !== null && filters.min_fraud_score !== undefined) {
    count += 1;
  }
  if (filters.max_fraud_score !== null && filters.max_fraud_score !== undefined) {
    count += 1;
  }
  if (filters.ip_type !== "any") {
    count += 1;
  }
  if (filters.native !== "any") {
    count += 1;
  }
  count += filters.asns.length;
  return count;
}

export function platformToFormValues(platform: Platform): PlatformFormValues {  const regexFilters = Array.isArray(platform.regex_filters) ? platform.regex_filters : [];
  const regionFilters = Array.isArray(platform.region_filters) ? platform.region_filters : [];
  const ipInfoFilters = platform.ipinfo_filters;

  return {
    name: platform.name,
    sticky_ttl: platform.sticky_ttl,
    regex_filters_text: regexFilters.join("\n"),
    region_filters_text: regionFilters.join("\n"),
    ipinfo_min_fraud_score:
      ipInfoFilters?.min_fraud_score === null || ipInfoFilters?.min_fraud_score === undefined
        ? ""
        : String(ipInfoFilters.min_fraud_score),
    ipinfo_max_fraud_score:
      ipInfoFilters?.max_fraud_score === null || ipInfoFilters?.max_fraud_score === undefined
        ? ""
        : String(ipInfoFilters.max_fraud_score),
    ipinfo_ip_type: ipInfoFilters?.ip_type ?? "any",
    ipinfo_native: ipInfoFilters?.native ?? "any",
    ipinfo_asns_text: (ipInfoFilters?.asns ?? []).join("\n"),
    reverse_proxy_miss_action: platform.reverse_proxy_miss_action,
    reverse_proxy_empty_account_behavior: platform.reverse_proxy_empty_account_behavior,
    reverse_proxy_fixed_account_header: platform.reverse_proxy_fixed_account_header,
    allocation_policy: platform.allocation_policy,
    passive_circuit_breaker_disabled: platform.passive_circuit_breaker_disabled,
    status_code_rules: (platform.status_code_rules ?? []).map((rule) => ({
      code_from: String(rule.code_from),
      // Render single-code rules as "429" rather than "429 ~ 429".
      code_to: rule.code_to === rule.code_from ? "" : String(rule.code_to),
      action: rule.action,
      cooldown: rule.cooldown ?? "",
    })),
  };
}

function toIPInfoFiltersPayload(values: PlatformFormValues): PlatformIPInfoFilters {
  return {
    min_fraud_score: parseScoreField(values.ipinfo_min_fraud_score).value,
    max_fraud_score: parseScoreField(values.ipinfo_max_fraud_score).value,
    ip_type: values.ipinfo_ip_type,
    native: values.ipinfo_native,
    asns: parseASNFilterLines(values.ipinfo_asns_text)
      .filter((entry) => entry.valid)
      .map((entry) => entry.value),
  };
}

function toPlatformPayloadBase(values: PlatformFormValues) {
  return {
    name: values.name.trim(),
    regex_filters: parseLinesToList(values.regex_filters_text),
    region_filters: parseLinesToList(values.region_filters_text, (value) => value.toLowerCase()),
    ipinfo_filters: toIPInfoFiltersPayload(values),
    reverse_proxy_miss_action: values.reverse_proxy_miss_action,
    reverse_proxy_empty_account_behavior: values.reverse_proxy_empty_account_behavior,
    reverse_proxy_fixed_account_header: parseHeaderLines(values.reverse_proxy_fixed_account_header).join("\n"),
    allocation_policy: values.allocation_policy,
    passive_circuit_breaker_disabled: values.passive_circuit_breaker_disabled,
    status_code_rules: (values.status_code_rules ?? []).map((rule) => {
      const codeFrom = Number(rule.code_from);
      const codeToText = (rule.code_to ?? "").trim();
      return {
        code_from: codeFrom,
        code_to: codeToText === "" ? codeFrom : Number(codeToText),
        action: rule.action,
        cooldown: rule.action === "COOLDOWN" ? (rule.cooldown ?? "").trim() : "",
      };
    }),
  };
}

export function toPlatformCreateInput(values: PlatformFormValues): PlatformCreateInput {
  return {
    ...toPlatformPayloadBase(values),
    sticky_ttl: values.sticky_ttl?.trim() || undefined,
  };
}

export function toPlatformUpdateInput(values: PlatformFormValues): PlatformUpdateInput {
  return {
    ...toPlatformPayloadBase(values),
    sticky_ttl: values.sticky_ttl?.trim() || "",
  };
}
