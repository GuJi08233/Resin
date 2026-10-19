import { Plus, Trash2 } from "lucide-react";
import type { UseFormReturn } from "react-hook-form";
import { useFieldArray } from "react-hook-form";
import { Button } from "../../components/ui/Button";
import { Input } from "../../components/ui/Input";
import { Select } from "../../components/ui/Select";
import { useI18n } from "../../i18n";
import { statusCodeActionLabel, statusCodeActions } from "./constants";
import type { PlatformFormValues } from "./formModel";
import type { PlatformStatusCodeAction } from "./types";

type StatusCodeRuleEditorProps = {
  form: UseFormReturn<PlatformFormValues>;
  idPrefix: string;
};

function firstRuleErrorMessage(rowError: unknown): string | undefined {
  if (!rowError || typeof rowError !== "object") {
    return undefined;
  }
  const fields = ["code_from", "code_to", "action", "cooldown"];
  for (const field of fields) {
    const item = (rowError as Record<string, { message?: string }>)[field];
    if (item?.message) {
      return item.message;
    }
  }
  return undefined;
}

export function StatusCodeRuleEditor({ form, idPrefix }: StatusCodeRuleEditorProps) {
  const { t } = useI18n();
  const { fields, append, remove } = useFieldArray({
    control: form.control,
    name: "status_code_rules",
  });
  const rules = form.watch("status_code_rules") ?? [];
  const errors = form.formState.errors.status_code_rules;

  const rowErrorAt = (index: number) => (Array.isArray(errors) ? errors[index] : undefined);

  return (
    <div className="field-group field-span-2 status-code-rules">
      <div className="platform-drawer-section-head">
        <h4>{t("状态码冷却 / 拉黑")}</h4>
        <p>{t("当上游返回指定状态码时，仅对当前平台排除该出口 IP。冷却到期自动恢复，拉黑需手动解除。")}</p>
        <p className="muted" style={{ marginTop: 4, fontSize: 12 }}>
          {t("支持单个状态码或区间：结束码留空表示只匹配起始码，填写后按闭区间匹配。")}
        </p>
        <p className="muted" style={{ marginTop: 4, fontSize: 12 }}>
          {t("仅 HTTP 正向代理和反向代理生效；CONNECT/SOCKS5 无法读取状态码。")}
        </p>
      </div>

      <div className="status-code-rule-list">
        {fields.map((field, index) => {
          const action: PlatformStatusCodeAction = rules[index]?.action ?? "COOLDOWN";
          const rowError = firstRuleErrorMessage(rowErrorAt(index));
          return (
            <div className="status-code-rule-row" key={field.id}>
              <Input
                id={`${idPrefix}-status-code-from-${index}`}
                aria-label={t("状态码")}
                placeholder={t("例如 429")}
                invalid={Boolean(rowErrorAt(index))}
                {...form.register(`status_code_rules.${index}.code_from` as const)}
              />
              <span className="status-code-rule-sep" aria-hidden="true">
                ~
              </span>
              <Input
                id={`${idPrefix}-status-code-to-${index}`}
                aria-label={t("结束状态码（可选）")}
                placeholder={t("留空=单码")}
                title={t("留空表示只匹配起始状态码；填写后按闭区间匹配。")}
                invalid={Boolean(rowErrorAt(index))}
                {...form.register(`status_code_rules.${index}.code_to` as const)}
              />
              <Select
                id={`${idPrefix}-status-code-action-${index}`}
                aria-label={t("处理动作")}
                {...form.register(`status_code_rules.${index}.action` as const)}
              >
                {statusCodeActions.map((item) => (
                  <option key={item} value={item}>
                    {t(statusCodeActionLabel[item])}
                  </option>
                ))}
              </Select>
              <Input
                id={`${idPrefix}-status-code-cooldown-${index}`}
                aria-label={t("冷却时长")}
                placeholder={t("例如 5m")}
                disabled={action !== "COOLDOWN"}
                {...form.register(`status_code_rules.${index}.cooldown` as const)}
              />
              <Button
                type="button"
                variant="ghost"
                size="sm"
                onClick={() => remove(index)}
                aria-label={t("删除规则")}
                title={t("删除规则")}
                style={{ color: "var(--delete-btn-color, #c27070)" }}
              >
                <Trash2 size={14} />
              </Button>
              {rowError ? <p className="field-error status-code-rule-error">{t(rowError)}</p> : null}
            </div>
          );
        })}
        {!fields.length ? <p className="muted">{t("未配置状态码规则")}</p> : null}
      </div>

      <div>
        <Button
          type="button"
          variant="secondary"
          size="sm"
          onClick={() => append({ code_from: "", code_to: "", action: "COOLDOWN", cooldown: "" })}
        >
          <Plus size={14} />
          {t("添加规则")}
        </Button>
      </div>
    </div>
  );
}
