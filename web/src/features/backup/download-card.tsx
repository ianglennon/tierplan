// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useForm } from "react-hook-form";

import { exportBackup, saveBlob } from "@/api/backup";
import { Button } from "@/components/ui/button";
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";

interface Values {
  passphrase: string;
  confirm: string;
}

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// DownloadCard asks for a passphrase and hands the encrypted backup the server
// makes to the browser (D-28, D-32).
export function DownloadCard() {
  const t = useT();
  const form = useForm<Values>({ defaultValues: { passphrase: "", confirm: "" } });

  async function submit(values: Values) {
    const file = await exportBackup(values.passphrase);
    saveBlob(file.blob, file.name);
    form.reset();
  }

  return (
    <SettingsCard title={t("backup.download.title")}>
      <form noValidate onSubmit={form.handleSubmit(submit)} className="flex flex-col gap-4">
        <FieldGroup className="gap-4">
          <Field>
            <FieldLabel htmlFor="backup-passphrase">{t("backup.download.passphrase")}</FieldLabel>
            <Input
              id="backup-passphrase"
              type="password"
              autoComplete="new-password"
              className={TOUCH_HEIGHT}
              {...form.register("passphrase")}
            />
          </Field>
          <Field>
            <FieldLabel htmlFor="backup-confirm">{t("backup.download.confirm")}</FieldLabel>
            <Input
              id="backup-confirm"
              type="password"
              autoComplete="new-password"
              className={TOUCH_HEIGHT}
              {...form.register("confirm")}
            />
          </Field>
        </FieldGroup>
        <div>
          <Button type="submit" className={TOUCH_HEIGHT}>
            {t("backup.download.submit")}
          </Button>
        </div>
      </form>
    </SettingsCard>
  );
}
