// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useId } from "react";

import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useT } from "@/i18n/use-t";

// matchesName says whether what was typed confirms the name: equal after
// trimming the ends and case-sensitive, so a pasted name with a trailing space
// still matches (D-18).
export function matchesName(typed: string, name: string): boolean {
  return typed.trim() === name;
}

interface TypeToConfirmProps {
  // The name to type, such as a username or the instance name.
  name: string;
  value: string;
  onChange: (value: string) => void;
  // Called when Enter is pressed with a matching value, so the dialog can
  // confirm from the keyboard. Enter does nothing until the value matches.
  onConfirm?: () => void;
  disabled?: boolean;
}

// TypeToConfirm is the labelled input "Type {name} to confirm" under the body of
// a destructive dialog. The caller keeps its confirm button aria-disabled until
// matchesName is true. Paste is allowed; the input offers no autocomplete and no
// spellcheck. The name is shown as text and wraps, so a long one stays readable.
export function TypeToConfirm({ name, value, onChange, onConfirm, disabled }: TypeToConfirmProps) {
  const t = useT();
  const id = useId();
  const hintId = `${id}-hint`;
  return (
    <div className="flex flex-col gap-2">
      <Label htmlFor={id} className="break-words">
        {t("needs.typeToConfirm.label", { name })}
      </Label>
      <Input
        id={id}
        value={value}
        disabled={disabled}
        autoComplete="off"
        spellCheck={false}
        autoCapitalize="off"
        aria-describedby={hintId}
        className="h-11 md:h-8"
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={(e) => {
          if (e.key !== "Enter") return;
          // Never submit a surrounding form by accident.
          e.preventDefault();
          if (matchesName(value, name)) onConfirm?.();
        }}
      />
      <p id={hintId} className="sr-only">
        {t("needs.typeToConfirm.hint", { name })}
      </p>
    </div>
  );
}
