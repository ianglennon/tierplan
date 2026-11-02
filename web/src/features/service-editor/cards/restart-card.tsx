// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useId } from "react";

import { Field, FieldLabel } from "@/components/ui/field";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  type CardProps,
  FormCard,
  TOUCH_HEIGHT,
  useReport,
} from "@/features/service-editor/cards/form-card";
import { SettingField, useSetting } from "@/features/service-editor/cards/text-fields";
import { getPath } from "@/features/service-editor/spec-patch";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

const CONDITION = "TaskTemplate.RestartPolicy.Condition";
const DELAY = "TaskTemplate.RestartPolicy.Delay";
const MAX_ATTEMPTS = "TaskTemplate.RestartPolicy.MaxAttempts";
const WINDOW = "TaskTemplate.RestartPolicy.Window";

const CONDITIONS = [
  { value: "any", label: "serviceForm.restart.any" },
  { value: "on-failure", label: "serviceForm.restart.onFailure" },
  { value: "none", label: "serviceForm.restart.none" },
] as const;

// RestartCard: when a task is restarted (Any exit, Failure only, Never), how long to wait, how
// many times to try and how long a task must run before it counts as started. A service with no
// restart policy restarts on any exit, which is what the card shows for it and writes nothing
// for. Each field writes only its own path; an empty field takes its setting out.
export function RestartCard({ spec, set, unset, report }: CardProps) {
  const t = useT();
  const ids = useId();
  const write = (path: string, value: number | undefined) =>
    value === undefined ? unset(path) : set(path, value);
  const delay = useSetting(spec, DELAY, "duration", write);
  const attempts = useSetting(spec, MAX_ATTEMPTS, "whole", write);
  const window = useSetting(spec, WINDOW, "duration", write);
  useReport("restart", report, delay.bad || attempts.bad || window.bad);

  const found = getPath(spec, CONDITION);
  const condition = CONDITIONS.some((c) => c.value === found) ? (found as string) : "any";

  return (
    <FormCard title={t("serviceForm.restart.title")}>
      <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
        <Field>
          <FieldLabel htmlFor={`${ids}-condition`}>{t("serviceForm.restart.condition")}</FieldLabel>
          <Select value={condition} onValueChange={(v) => set(CONDITION, v)}>
            <SelectTrigger
              id={`${ids}-condition`}
              aria-label={t("serviceForm.restart.condition")}
              className={cn("w-full", TOUCH_HEIGHT)}
            >
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {CONDITIONS.map((c) => (
                <SelectItem key={c.value} value={c.value}>
                  {t(c.label)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>
        <SettingField
          id={`${ids}-delay`}
          label={t("serviceForm.restart.delay")}
          placeholder={t("serviceForm.restart.delayPlaceholder")}
          setting={delay}
        />
        <SettingField
          id={`${ids}-attempts`}
          label={t("serviceForm.restart.maxAttempts")}
          helper={t("serviceForm.restart.maxAttemptsHelper")}
          setting={attempts}
        />
        <SettingField
          id={`${ids}-window`}
          label={t("serviceForm.restart.window")}
          placeholder={t("serviceForm.restart.windowPlaceholder")}
          helper={t("serviceForm.restart.windowHelper")}
          setting={window}
        />
      </div>
    </FormCard>
  );
}
