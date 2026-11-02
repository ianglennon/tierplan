// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { ReactNode } from "react";
import { useId } from "react";

import { Label } from "@/components/ui/label";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";

// Which definition the adoption starts from (D-12): the one TierPlan writes from what runs, or the person's own file.
export type AdoptSourceKind = "rebuild" | "paste";

interface AdoptSourceProps {
  source: AdoptSourceKind;
  onSourceChange: (source: AdoptSourceKind) => void;
  // Why the original-file choice is off, when it is: a pasted file is compared with the values that run, which takes
  // secrets.reveal. Absent when the choice is on.
  pasteOff?: string;
  // What goes under the choice: the paste field and its variables, when the person has the original file.
  children?: ReactNode;
}

// AdoptSource is the "Start from" group of the Adopt page (UI-SPEC "Adopt page", D-12, D-20): "Rebuild from the running
// services" (the default, always available) and "I have the original compose file". Arrow keys move between the two.
export function AdoptSource({ source, onSourceChange, pasteOff, children }: AdoptSourceProps) {
  const t = useT();
  const ids = useId();
  return (
    <SettingsCard title={t("stackAdoptPaste.source.title")}>
      <RadioGroup
        value={source}
        onValueChange={(next) => onSourceChange(next === "paste" ? "paste" : "rebuild")}
        aria-label={t("stackAdoptPaste.source.title")}
      >
        {(["rebuild", "paste"] as const).map((value) => {
          const off = value === "paste" && pasteOff !== undefined;
          return (
            <div key={value} className="flex items-start gap-2 max-md:min-h-11">
              <RadioGroupItem
                value={value}
                id={`${ids}-${value}`}
                aria-describedby={`${ids}-${value}-help${off ? ` ${ids}-${value}-off` : ""}`}
                disabled={off}
                className="mt-0.5"
              />
              <div className="flex min-w-0 flex-col gap-0.5">
                <Label htmlFor={`${ids}-${value}`}>
                  {t(`stackAdoptPaste.source.${value}.label`)}
                </Label>
                <p id={`${ids}-${value}-help`} className="text-sm text-muted-foreground">
                  {t(`stackAdoptPaste.source.${value}.help`)}
                </p>
                {off ? (
                  <p id={`${ids}-${value}-off`} className="text-sm text-muted-foreground">
                    {pasteOff}
                  </p>
                ) : null}
              </div>
            </div>
          );
        })}
      </RadioGroup>
      {children}
    </SettingsCard>
  );
}
