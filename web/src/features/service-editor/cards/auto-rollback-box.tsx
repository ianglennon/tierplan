// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Checkbox } from "@/components/ui/checkbox";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import type { AutoRollbackState } from "@/features/service-editor/update-policy";
import { useT } from "@/i18n/use-t";

interface AutoRollbackBoxProps {
  id: string;
  state: AutoRollbackState;
  onChange: (ticked: boolean) => void;
  // Which helper goes under the box: the card's or the Review step's.
  helper: "helper" | "reviewHelper";
  disabled?: boolean;
}

// AutoRollbackBox is "Roll back automatically if an update fails" (D-12 amended), the one field
// shown on the Update policy card and again on Review. A service that continues on failure shows
// it unticked and says so, because ticking it changes what the service does.
export function AutoRollbackBox({ id, state, onChange, helper, disabled }: AutoRollbackBoxProps) {
  const t = useT();
  return (
    <Field orientation="horizontal" className="items-start">
      <Checkbox
        id={id}
        checked={state.checked}
        disabled={disabled}
        onCheckedChange={(v) => onChange(v === true)}
        aria-describedby={state.continues ? `${id}-helper ${id}-continues` : `${id}-helper`}
        className="mt-0.5"
      />
      <div className="flex flex-col gap-0.5">
        <FieldLabel htmlFor={id} className="font-normal">
          {t("serviceForm.update.autoRollback.label")}
        </FieldLabel>
        <FieldDescription id={`${id}-helper`}>
          {t(`serviceForm.update.autoRollback.${helper}`)}
        </FieldDescription>
        {state.continues ? (
          <FieldDescription id={`${id}-continues`}>
            {t("serviceForm.update.autoRollback.continues")}
          </FieldDescription>
        ) : null}
      </div>
    </Field>
  );
}
