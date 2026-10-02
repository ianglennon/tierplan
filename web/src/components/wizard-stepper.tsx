// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useTranslation } from "react-i18next";

import { cn } from "@/lib/utils";

interface WizardStepperProps {
  current: number;
  total?: number;
}

// WizardStepper shows "Step n of 4" and one segment per step. Current and
// completed segments use the accent ink, the rest the muted one. The text is
// the label and the progressbar's aria-label and value text carry the same
// words, so the position never depends on colour alone.
export function WizardStepper({ current, total = 4 }: WizardStepperProps) {
  const { t } = useTranslation("setup");
  const label = t("setup.stepper.label", { current, total });
  return (
    <div
      role="progressbar"
      aria-label={label}
      aria-valuemin={1}
      aria-valuemax={total}
      aria-valuenow={current}
      aria-valuetext={label}
      className="flex flex-col gap-2"
    >
      <span className="text-xs font-medium text-muted-foreground" aria-hidden="true">
        {label}
      </span>
      <div className="flex gap-1.5" aria-hidden="true">
        {Array.from({ length: total }, (_, i) => (
          <span
            // Steps are positional and never reorder.
            // biome-ignore lint/suspicious/noArrayIndexKey: fixed-length list
            key={i}
            data-state={i < current ? "done" : "todo"}
            className={cn("h-1 flex-1 rounded-full", i < current ? "bg-primary" : "bg-muted")}
          />
        ))}
      </div>
    </div>
  );
}
