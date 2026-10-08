// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Eye, EyeOff } from "lucide-react";
import { useState } from "react";

import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group";

export interface PasswordFieldProps {
  id: string;
  label: string;
  helper?: string;
  error?: string;
  showLabel: string;
  hideLabel: string;
  inputProps: object;
}

// PasswordField is a labelled password input with its own show/hide toggle. The
// control gets aria-invalid and an aria-describedby naming the helper and, while
// there is one, the error (UI-SPEC "Forms"). The browser is told this is a new
// password, so it neither fills one in nor offers to save it.
export function PasswordField({
  id,
  label,
  helper,
  error,
  showLabel,
  hideLabel,
  inputProps,
}: PasswordFieldProps) {
  const [shown, setShown] = useState(false);
  const helperId = `${id}-helper`;
  const errorId = `${id}-error`;
  const describedBy = [helper ? helperId : null, error ? errorId : null].filter(Boolean).join(" ");
  return (
    <Field data-invalid={error ? true : undefined}>
      <FieldLabel htmlFor={id}>{label}</FieldLabel>
      <InputGroup className="h-11 md:h-8">
        <InputGroupInput
          id={id}
          {...inputProps}
          type={shown ? "text" : "password"}
          autoComplete="new-password"
          aria-invalid={Boolean(error)}
          aria-describedby={describedBy || undefined}
          className="h-full"
        />
        <InputGroupAddon align="inline-end">
          <InputGroupButton
            size="icon-xs"
            aria-label={shown ? hideLabel : showLabel}
            onClick={() => setShown((v) => !v)}
          >
            {shown ? <EyeOff aria-hidden="true" /> : <Eye aria-hidden="true" />}
          </InputGroupButton>
        </InputGroupAddon>
      </InputGroup>
      {helper ? <FieldDescription id={helperId}>{helper}</FieldDescription> : null}
      {error ? <FieldError id={errorId}>{error}</FieldError> : null}
    </Field>
  );
}
