// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useId } from "react";

import { Label } from "@/components/ui/label";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { useT } from "@/i18n/use-t";

// The roles in the order the picker lists them, highest first.
export const ROLES = ["admin", "operator", "viewer"] as const;
export type RoleId = (typeof ROLES)[number];

interface RolePickerProps {
  value: RoleId;
  onChange: (role: RoleId) => void;
  // The highest role the picker offers. A delegating env Admin can never pick
  // above their own level, which is always Admin in 0.1, so all three show.
  max?: RoleId;
  disabled?: boolean;
}

// RolePicker is a radio group of Admin, Operator and Viewer, each with a one-line
// description (UI-SPEC). Arrow keys move and select; the role name is the
// radio's label and the description its aria-describedby. The selected option
// has a muted background and the filled dot, not colour alone. Every option is at
// least 56px high and the description wraps inside it rather than truncating.
export function RolePicker({ value, onChange, max = "admin", disabled }: RolePickerProps) {
  const t = useT();
  const id = useId();
  const offered = ROLES.slice(ROLES.indexOf(max));
  return (
    <RadioGroup
      value={value}
      onValueChange={(next) => onChange(next as RoleId)}
      aria-label={t("grants.role.pickerLabel")}
      disabled={disabled}
      className="gap-2"
    >
      {offered.map((role) => {
        const itemId = `${id}-${role}`;
        return (
          <Label
            key={role}
            htmlFor={itemId}
            className="flex min-h-14 cursor-pointer items-start gap-3 rounded-lg border p-3 font-normal has-data-[state=checked]:bg-muted has-disabled:cursor-not-allowed has-disabled:opacity-60"
          >
            <RadioGroupItem
              id={itemId}
              value={role}
              aria-labelledby={`${itemId}-label`}
              aria-describedby={`${itemId}-desc`}
              className="mt-0.5"
            />
            <span className="flex min-w-0 flex-col gap-0.5">
              <span id={`${itemId}-label`} className="text-sm font-medium">
                {t(`grants.role.${role}.label`)}
              </span>
              <span id={`${itemId}-desc`} className="text-xs leading-snug text-muted-foreground">
                {t(`grants.role.${role}.description`)}
              </span>
            </span>
          </Label>
        );
      })}
    </RadioGroup>
  );
}
