// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useId } from "react";

import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { AutoRollbackBox } from "@/features/service-editor/cards/auto-rollback-box";
import {
  type CardProps,
  FormCard,
  TOUCH_HEIGHT,
  useReport,
} from "@/features/service-editor/cards/form-card";
import { SettingField, useSetting } from "@/features/service-editor/cards/text-fields";
import { getPath, removePath, setPath } from "@/features/service-editor/spec-patch";
import {
  autoRollbackState,
  seedUpdateConfig,
  setAutoRollback,
} from "@/features/service-editor/update-policy";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

const PARALLELISM = "UpdateConfig.Parallelism";
const DELAY = "UpdateConfig.Delay";
const ORDER = "UpdateConfig.Order";
const MONITOR = "UpdateConfig.Monitor";
const RATIO = "UpdateConfig.MaxFailureRatio";

const ORDERS = [
  { value: "stop-first", label: "serviceForm.update.stopFirst" },
  { value: "start-first", label: "serviceForm.update.startFirst" },
] as const;

// UpdateCard: how a rolling update goes (RES-03): tasks at a time, the delay between batches,
// stop or start first, how long each updated task is watched, and the share of failures that
// fails the update, with "Roll back automatically if an update fails" (D-12 amended) as the
// service's own failure action. A service with no UpdateConfig updates one task at a time, so the
// first write into an UpdateConfig fills in the CLI defaults, and nothing about the rollout
// changes except what the person set (RESEARCH Pitfall 5). The box reads from the spec, so the
// Review step's copy of it and this one are the same field.
export function UpdateCard({ spec, change, report }: CardProps) {
  const t = useT();
  const ids = useId();
  const write = (path: string, value: number | undefined) =>
    change((s) =>
      value === undefined ? removePath(s, path) : setPath(seedUpdateConfig(s), path, value),
    );
  const parallelism = useSetting(spec, PARALLELISM, "whole", write);
  const delay = useSetting(spec, DELAY, "duration", write);
  const monitor = useSetting(spec, MONITOR, "duration", write);
  const ratio = useSetting(spec, RATIO, "ratio", write);
  useReport("update", report, parallelism.bad || delay.bad || monitor.bad || ratio.bad);

  const found = getPath(spec, ORDER);
  const order = ORDERS.some((o) => o.value === found) ? (found as string) : "stop-first";

  return (
    <FormCard title={t("serviceForm.update.title")}>
      <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
        <SettingField
          id={`${ids}-parallelism`}
          label={t("serviceForm.update.parallelism")}
          placeholder="1"
          helper={t("serviceForm.update.parallelismHelper")}
          setting={parallelism}
        />
        <SettingField
          id={`${ids}-delay`}
          label={t("serviceForm.update.delay")}
          placeholder={t("serviceForm.update.delayPlaceholder")}
          setting={delay}
        />
        <Field>
          <FieldLabel htmlFor={`${ids}-order`}>{t("serviceForm.update.order")}</FieldLabel>
          <Select
            value={order}
            onValueChange={(v) => change((s) => setPath(seedUpdateConfig(s), ORDER, v))}
          >
            <SelectTrigger
              id={`${ids}-order`}
              aria-label={t("serviceForm.update.order")}
              className={cn("w-full", TOUCH_HEIGHT)}
            >
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {ORDERS.map((o) => (
                <SelectItem key={o.value} value={o.value}>
                  {t(o.label)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <FieldDescription>{t("serviceForm.update.orderHelper")}</FieldDescription>
        </Field>
        <SettingField
          id={`${ids}-monitor`}
          label={t("serviceForm.update.monitor")}
          placeholder={t("serviceForm.update.monitorPlaceholder")}
          helper={t("serviceForm.update.monitorHelper")}
          setting={monitor}
        />
        <SettingField
          id={`${ids}-ratio`}
          label={t("serviceForm.update.maxFailureRatio")}
          helper={t("serviceForm.update.maxFailureRatioHelper")}
          setting={ratio}
        />
      </div>
      <AutoRollbackBox
        id={`${ids}-rollback`}
        state={autoRollbackState(spec)}
        helper="helper"
        onChange={(ticked) => change((s) => setAutoRollback(s, ticked))}
      />
    </FormCard>
  );
}
