// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { CircleCheck, Info } from "lucide-react";
import { FullPageState } from "@/components/full-page-state";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { useT } from "@/i18n/use-t";
import { withBase } from "@/lib/base-path";

// BackupRestored fills the page once TierPlan has restarted into the backup: the
// session is gone, so the only way on is to sign in with an account from the
// backup. The link is a plain page load, which also drops everything the old
// session left in memory.
export function BackupRestored() {
  const t = useT();
  return (
    <FullPageState
      icon={CircleCheck}
      heading={t("restore.done.heading")}
      body={t("restore.done.body")}
      detail={
        <Alert role="note">
          <Info aria-hidden="true" />
          <AlertDescription className="text-foreground">
            {t("restore.done.agents")}
          </AlertDescription>
        </Alert>
      }
      action={
        <Button asChild className="h-11 md:h-8">
          <a href={withBase("/login")}>{t("restore.done.action")}</a>
        </Button>
      }
    />
  );
}
