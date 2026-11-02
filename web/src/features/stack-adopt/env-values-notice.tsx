// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Info } from "lucide-react";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { useT } from "@/i18n/use-t";

// EnvValuesNotice is the note the Adopt page always shows (UI-SPEC "Adopt page", D-15): the environment values of
// what runs are copied into the stored definition as they are, passwords included. It is static content on load, so
// it is a status and not announced as an alert.
export function EnvValuesNotice() {
  const t = useT();
  return (
    <Alert role="status">
      <Info aria-hidden="true" />
      <AlertTitle>{t("stackAdopt.envValues.title")}</AlertTitle>
      <AlertDescription>{t("stackAdopt.envValues.body")}</AlertDescription>
    </Alert>
  );
}
