// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useId } from "react";

import type { StackVariable } from "@/api/stacks";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { useT } from "@/i18n/use-t";

// What a person typed for a variable of the pasted file. It is the page's own state.
export type PastedValue = { value: string; secret: boolean };

interface PastedVariablesProps {
  // The variables the check reports for the file in its current state.
  variables: StackVariable[];
  // What has been typed, by name.
  values: Record<string, PastedValue>;
  onChange: (name: string, next: PastedValue) => void;
  // True once the check of the current text and values has settled: a variable it still calls required is flagged then.
  settled: boolean;
}

// PastedVariables lists the variables a pasted compose file uses, one input each (UI-SPEC "Pasted variables", D-12): the
// label "Value of {NAME}", the file's own default or "Required" as the placeholder, and a switch "Secret: {NAME}". They
// are used to check the file against what runs and are saved with the adopted stack, a secret one sealed. A secret
// input is a password field that the browser does not fill or remember, as in the stack editor. A variable no longer
// used by the file is not listed.
export function PastedVariables({ variables, values, onChange, settled }: PastedVariablesProps) {
  const t = useT();
  const ids = useId();
  const used = variables.filter((v) => !v.unused);
  if (used.length === 0) return null;
  return (
    <fieldset className="flex min-w-0 flex-col gap-3" data-slot="pasted-variables">
      <legend className="text-sm font-medium">{t("stackAdoptPaste.variables.title")}</legend>
      <p className="text-sm text-muted-foreground">{t("stackAdoptPaste.variables.help")}</p>
      <ul className="flex flex-col gap-3">
        {used.map((v, i) => {
          const entry = values[v.name] ?? { value: "", secret: false };
          const inputId = `${ids}-${i}`;
          const missing = settled && v.required && entry.value === "";
          return (
            <li key={v.name} className="flex min-w-0 flex-col gap-1.5">
              <Label htmlFor={inputId}>
                {t("stackAdoptPaste.variables.value", { name: v.name })}
              </Label>
              <div className="flex min-w-0 flex-wrap items-center gap-3">
                <Input
                  id={inputId}
                  type={entry.secret ? "password" : "text"}
                  autoComplete={entry.secret ? "new-password" : "off"}
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                  value={entry.value}
                  placeholder={v.hasDefault ? v.default : t("stackAdoptPaste.variables.required")}
                  aria-invalid={missing || undefined}
                  aria-describedby={missing ? `${inputId}-error` : undefined}
                  onChange={(e) => onChange(v.name, { ...entry, value: e.target.value })}
                  className="min-w-0 flex-1 font-mono max-md:h-11"
                />
                <div className="flex shrink-0 items-center gap-2 max-md:min-h-11">
                  <Switch
                    id={`${inputId}-secret`}
                    size="sm"
                    checked={entry.secret}
                    onCheckedChange={(secret) => onChange(v.name, { ...entry, secret })}
                    aria-label={t("stackAdoptPaste.variables.secret", { name: v.name })}
                  />
                  <Label htmlFor={`${inputId}-secret`} className="text-sm text-muted-foreground">
                    {t("stackAdoptPaste.variables.secretShort")}
                  </Label>
                </div>
              </div>
              {missing ? (
                <p id={`${inputId}-error`} className="text-sm text-status-offline-text">
                  {t("stackAdoptPaste.variables.missing", { name: v.name })}
                </p>
              ) : null}
            </li>
          );
        })}
      </ul>
    </fieldset>
  );
}
