// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Eye, EyeOff } from "lucide-react";
import { useState } from "react";

import { buttonVariants } from "@/components/ui/button";
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group";
import { TooltipProvider } from "@/components/ui/tooltip";
import { Truncated } from "@/features/access/truncated";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

interface RestoreFieldsProps {
  // Prefix for the element IDs, so the two places that use this stay distinct.
  idPrefix: string;
  file: File | null;
  // Changing the key makes the browser drop the chosen file from the input.
  fileKey: number;
  onFile: (file: File | null) => void;
  passphrase: string;
  onPassphrase: (value: string) => void;
  disabled: boolean;
}

// RestoreFields is the backup file and its passphrase, the form of both restore
// paths. Choosing a file uploads nothing: that waits for "Check backup". The
// native input is hidden and its label is the button, so the picker opens from
// the keyboard and the file name can be shown in mono, truncated, with the whole
// name in a tooltip. The passphrase is never offered to a password manager.
export function RestoreFields({
  idPrefix,
  file,
  fileKey,
  onFile,
  passphrase,
  onPassphrase,
  disabled,
}: RestoreFieldsProps) {
  const t = useT();
  const [shown, setShown] = useState(false);
  const fileId = `${idPrefix}-file`;
  const passId = `${idPrefix}-passphrase`;
  return (
    <FieldGroup className="gap-4">
      <Field>
        <FieldLabel htmlFor={fileId}>{t("restore.file.label")}</FieldLabel>
        <div className="flex min-w-0 items-center gap-2">
          <input
            key={fileKey}
            id={fileId}
            type="file"
            accept=".coaming"
            disabled={disabled}
            aria-describedby={`${fileId}-helper`}
            className="peer sr-only"
            onChange={(e) => onFile(e.target.files?.[0] ?? null)}
          />
          <label
            htmlFor={fileId}
            className={cn(
              buttonVariants({ variant: "outline" }),
              "h-11 shrink-0 cursor-pointer md:h-8",
              "peer-focus-visible:border-ring peer-focus-visible:ring-3 peer-focus-visible:ring-ring/50",
              "peer-disabled:cursor-not-allowed peer-disabled:opacity-50",
            )}
          >
            {t("restore.file.choose")}
          </label>
          <TooltipProvider>
            {file ? (
              <Truncated text={file.name} className="min-w-0 max-w-full flex-1 font-mono text-sm" />
            ) : (
              <span className="text-sm text-muted-foreground">{t("restore.file.none")}</span>
            )}
          </TooltipProvider>
        </div>
        <FieldDescription id={`${fileId}-helper`}>{t("restore.file.helper")}</FieldDescription>
      </Field>
      <Field>
        <FieldLabel htmlFor={passId}>{t("restore.passphrase.label")}</FieldLabel>
        <InputGroup className="h-11 md:h-8">
          <InputGroupInput
            id={passId}
            type={shown ? "text" : "password"}
            autoComplete="off"
            disabled={disabled}
            value={passphrase}
            className="h-full"
            onChange={(e) => onPassphrase(e.target.value)}
          />
          <InputGroupAddon align="inline-end">
            <InputGroupButton
              size="icon-xs"
              aria-label={t(shown ? "restore.passphrase.hide" : "restore.passphrase.show")}
              disabled={disabled}
              onClick={() => setShown((v) => !v)}
            >
              {shown ? <EyeOff aria-hidden="true" /> : <Eye aria-hidden="true" />}
            </InputGroupButton>
          </InputGroupAddon>
        </InputGroup>
      </Field>
    </FieldGroup>
  );
}
