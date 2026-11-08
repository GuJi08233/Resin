import type { UseFormReturn } from "react-hook-form";
import { Input } from "../../components/ui/Input";
import { Select } from "../../components/ui/Select";
import { Textarea } from "../../components/ui/Textarea";
import { useI18n } from "../../i18n";
import type { PlatformFormValues } from "./formModel";

type IPInfoFilterEditorProps = {
  form: UseFormReturn<PlatformFormValues>;
  idPrefix: string;
};

const ipTypeOptions = [
  { value: "any", label: "不限" },
  { value: "residential", label: "住宅 IP" },
  { value: "hosting", label: "机房 IP" },
] as const;

const nativeOptions = [
  { value: "any", label: "不限" },
  { value: "native", label: "原生 IP" },
  { value: "broadcast", label: "广播 IP" },
] as const;

// IPInfoFilterEditor edits the platform-scoped IPPure profile filter.
// Values come from the node's last successful IP profile probe.
export function IPInfoFilterEditor({ form, idPrefix }: IPInfoFilterEditorProps) {
  const { t } = useI18n();
  const minError = form.formState.errors.ipinfo_min_fraud_score?.message;
  const maxError = form.formState.errors.ipinfo_max_fraud_score?.message;
  const asnError = form.formState.errors.ipinfo_asns_text?.message;

  return (
    <div className="field-group field-span-2 ipinfo-filters">
      <div className="platform-drawer-section-head">
        <h4>{t("IP 画像过滤（可选）")}</h4>
        <p>{t("依据节点最近一次 IP 画像探测的结果筛选出口。启用任一条件后，尚未探测过画像的节点不会被此平台路由。")}</p>
      </div>

      <div className="form-grid">
        <div className="field-group">
          <label className="field-label" htmlFor={`${idPrefix}-ipinfo-min-score`}>
            {t("最小风险分")}
          </label>
          <Input
            id={`${idPrefix}-ipinfo-min-score`}
            type="number"
            min={0}
            max={100}
            step={1}
            placeholder={t("留空表示不限")}
            invalid={Boolean(minError)}
            {...form.register("ipinfo_min_fraud_score")}
          />
          {minError ? <p className="field-error">{t(minError)}</p> : null}
        </div>

        <div className="field-group">
          <label className="field-label" htmlFor={`${idPrefix}-ipinfo-max-score`}>
            {t("最大风险分")}
          </label>
          <Input
            id={`${idPrefix}-ipinfo-max-score`}
            type="number"
            min={0}
            max={100}
            step={1}
            placeholder={t("留空表示不限")}
            invalid={Boolean(maxError)}
            {...form.register("ipinfo_max_fraud_score")}
          />
          {maxError ? <p className="field-error">{t(maxError)}</p> : null}
        </div>

        <div className="field-group">
          <label className="field-label" htmlFor={`${idPrefix}-ipinfo-ip-type`}>
            {t("IP 属性")}
          </label>
          <Select id={`${idPrefix}-ipinfo-ip-type`} {...form.register("ipinfo_ip_type")}>
            {ipTypeOptions.map((item) => (
              <option key={item.value} value={item.value}>
                {t(item.label)}
              </option>
            ))}
          </Select>
        </div>

        <div className="field-group">
          <label className="field-label" htmlFor={`${idPrefix}-ipinfo-native`}>
            {t("原生属性")}
          </label>
          <Select id={`${idPrefix}-ipinfo-native`} {...form.register("ipinfo_native")}>
            {nativeOptions.map((item) => (
              <option key={item.value} value={item.value}>
                {t(item.label)}
              </option>
            ))}
          </Select>
        </div>
      </div>

      <div className="field-group">
        <label className="field-label" htmlFor={`${idPrefix}-ipinfo-asns`}>
          {t("ASN 白名单 / 黑名单（每行一个）")}
        </label>
        <Textarea
          id={`${idPrefix}-ipinfo-asns`}
          rows={4}
          placeholder={t("每行一个 ASN，例如：\n\nAS132203\n64500\n!4134\n\n不带 ! 表示仅允许这些 ASN，! 开头表示排除。")}
          invalid={Boolean(asnError)}
          {...form.register("ipinfo_asns_text")}
        />
        {asnError ? <p className="field-error">{t(asnError)}</p> : null}
        <p className="muted" style={{ marginTop: 4, fontSize: 12 }}>
          {t("可省略 AS 前缀。白名单与黑名单可混用，同一个 ASN 不能同时出现在两侧。")}
        </p>
      </div>
    </div>
  );
}
