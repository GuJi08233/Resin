import type { UseFormReturn } from "react-hook-form";
import { Button } from "../../components/ui/Button";
import { useI18n } from "../../i18n";
import { getAllContinents, type ContinentCode } from "../nodes/regions";
import type { PlatformFormValues } from "./formModel";

type ContinentFilterEditorProps = {
  form: UseFormReturn<PlatformFormValues>;
  idPrefix: string;
};

type ContinentState = "unset" | "include" | "exclude";

function getContinentState(filters: string[], code: ContinentCode): ContinentState {
  if (filters.includes(`!${code}`)) {
    return "exclude";
  }
  if (filters.includes(code)) {
    return "include";
  }
  return "unset";
}

function cycleContinentFilter(filters: string[], code: ContinentCode): string[] {
  const current = getContinentState(filters, code);
  const cleaned = filters.filter((item) => item !== code && item !== `!${code}`);
  if (current === "unset") {
    return [...cleaned, code];
  }
  if (current === "include") {
    return [...cleaned, `!${code}`];
  }
  return cleaned;
}

// ContinentFilterEditor edits the platform-scoped macro-region filter.
// Clicking a continent cycles: unset -> include -> exclude -> unset.
export function ContinentFilterEditor({ form, idPrefix }: ContinentFilterEditorProps) {
  const { t } = useI18n();
  const currentFilters = form.watch("continent_filters") ?? [];
  const continents = getAllContinents();

  const toggleContinent = (code: ContinentCode) => {
    const next = cycleContinentFilter(form.getValues("continent_filters") ?? [], code);
    form.setValue("continent_filters", next, { shouldDirty: true, shouldValidate: true });
  };

  const clearAll = () => {
    form.setValue("continent_filters", [], { shouldDirty: true, shouldValidate: true });
  };

  return (
    <div className="field-group field-span-2 continent-filters">
      <div className="continent-filter-header">
        <div className="platform-drawer-section-head">
          <h4>{t("大区过滤（可选）")}</h4>
          <p>{t("按洲际大区筛选节点出口。点击大区切换状态：未选 → 包含 → 排除；可与地区过滤组合使用（例如大区选欧洲、地区填 !ru）。")}</p>
        </div>
        {currentFilters.length > 0 ? (
          <Button type="button" variant="ghost" size="sm" onClick={clearAll}>
            {t("清空")}
          </Button>
        ) : null}
      </div>

      <div className="continent-filter-pills" role="group" aria-label={t("大区过滤（可选）")}>
        {continents.map((item) => {
          const state = getContinentState(currentFilters, item.code);
          const prefix = state === "include" ? "✓ " : state === "exclude" ? "✕ " : "";
          const codeHint = state === "exclude" ? `!${item.code.toUpperCase()}` : item.code.toUpperCase();
          return (
            <button
              key={item.code}
              id={`${idPrefix}-continent-${item.code}`}
              type="button"
              className={`continent-pill continent-pill-${state}`}
              aria-pressed={state !== "unset"}
              onClick={() => toggleContinent(item.code)}
            >
              <span>{prefix}{item.label}</span>
              <small>{codeHint}</small>
            </button>
          );
        })}
      </div>
    </div>
  );
}
