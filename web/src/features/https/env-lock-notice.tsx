// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Info } from "lucide-react";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { useT } from "@/i18n/use-t";

// EnvLockNotice explains once, at the top of the page, what the lock icons below mean (SET-02,
// UI-SPEC "Env-lock notice"). The page draws it only while hasEnvLocks says a field is locked. It is
// a note, not an announcement, and carries no button: the fix is in the compose file.
export function EnvLockNotice() {
  const t = useT();
  return (
    <Alert role="note" className="text-foreground">
      <Info aria-hidden="true" />
      <AlertTitle>{t("https.lock.notice.title")}</AlertTitle>
      <AlertDescription className="text-foreground">{t("https.lock.notice.body")}</AlertDescription>
    </Alert>
  );
}
