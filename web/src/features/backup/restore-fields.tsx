// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { useT } from "@/i18n/use-t";

interface RestoreFieldsProps {
  // Prefix for the element IDs, so the two places that use this stay distinct.
  idPrefix: string;
  fileKey: number;
  onFile: (file: File | null) => void;
  passphrase: string;
  onPassphrase: (value: string) => void;
  disabled: boolean;
}

// RestoreFields is the backup file and its passphrase, the form of both restore
// paths. Choosing a file uploads nothing: that waits for "Check backup".
export function RestoreFields({
  idPrefix,
  fileKey,
  onFile,
  passphrase,
  onPassphrase,
  disabled,
}: RestoreFieldsProps) {
  const t = useT();
  return (
    <FieldGroup className="gap-4">
      <Field>
        <FieldLabel htmlFor={`${idPrefix}-file`}>{t("restore.file.label")}</FieldLabel>
        <Input
          key={fileKey}
          id={`${idPrefix}-file`}
          type="file"
          accept=".bayplan"
          disabled={disabled}
          aria-describedby={`${idPrefix}-file-helper`}
          className="h-11 md:h-8"
          onChange={(e) => onFile(e.target.files?.[0] ?? null)}
        />
        <FieldDescription id={`${idPrefix}-file-helper`}>
          {t("restore.file.helper")}
        </FieldDescription>
      </Field>
      <Field>
        <FieldLabel htmlFor={`${idPrefix}-passphrase`}>{t("restore.passphrase.label")}</FieldLabel>
        <Input
          id={`${idPrefix}-passphrase`}
          type="password"
          autoComplete="off"
          disabled={disabled}
          value={passphrase}
          className="h-11 md:h-8"
          onChange={(e) => onPassphrase(e.target.value)}
        />
      </Field>
    </FieldGroup>
  );
}
