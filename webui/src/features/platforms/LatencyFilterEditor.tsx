import type { UseFormReturn } from "react-hook-form";
import { Input } from "../../components/ui/Input";
import { useI18n } from "../../i18n";
import type { PlatformFormValues } from "./formModel";

type LatencyFilterEditorProps = {
  form: UseFormReturn<PlatformFormValues>;
  idPrefix: string;
};

// LatencyFilterEditor edits the platform-scoped reference latency filter.
// Values are matched against the node's average EWMA latency across configured
// authority domains (in milliseconds).
export function LatencyFilterEditor({ form, idPrefix }: LatencyFilterEditorProps) {
  const { t } = useI18n();
  const minError = form.formState.errors.latency_min_ms?.message;
  const maxError = form.formState.errors.latency_max_ms?.message;

  return (
    <div className="field-group field-span-2 latency-filters">
      <div className="platform-drawer-section-head">
        <h4>{t("延迟过滤（可选）")}</h4>
        <p>{t("依据节点在权威延迟域名的平均 EWMA 参考延迟（ms）筛选出口。启用任一条件后，尚无参考延迟的节点不会被此平台路由。")}</p>
      </div>

      <div className="form-grid">
        <div className="field-group">
          <label className="field-label" htmlFor={`${idPrefix}-latency-min-ms`}>
            {t("最小参考延迟 (ms)")}
          </label>
          <Input
            id={`${idPrefix}-latency-min-ms`}
            type="number"
            min={0}
            step={1}
            placeholder={t("留空表示不限")}
            invalid={Boolean(minError)}
            {...form.register("latency_min_ms")}
          />
          {minError ? <p className="field-error">{t(minError)}</p> : null}
        </div>

        <div className="field-group">
          <label className="field-label" htmlFor={`${idPrefix}-latency-max-ms`}>
            {t("最大参考延迟 (ms)")}
          </label>
          <Input
            id={`${idPrefix}-latency-max-ms`}
            type="number"
            min={0}
            step={1}
            placeholder={t("留空表示不限")}
            invalid={Boolean(maxError)}
            {...form.register("latency_max_ms")}
          />
          {maxError ? <p className="field-error">{t(maxError)}</p> : null}
        </div>
      </div>
    </div>
  );
}
