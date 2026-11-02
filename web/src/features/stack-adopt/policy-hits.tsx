// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { TriangleAlert } from "lucide-react";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { useT } from "@/i18n/use-t";

interface PolicyHitsProps {
  // The host-access settings of the stack, each as the server words it: "a bind mount of /var/run/docker.sock in web".
  entries: string[];
}

// PolicyHits flags, before the stack is adopted, the settings that need an Admin to deploy (UI-SPEC "Adopt page",
// D-17). It lists every one whoever asks, an Admin too, and blocks nothing: adopting only records what already runs,
// and every later deploy is judged for the person who starts it. It is advice, so a status and not an alert.
export function PolicyHits({ entries }: PolicyHitsProps) {
  const t = useT();
  if (entries.length === 0) return null;
  return (
    <Alert variant="warning" role="status">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle>{t("stackAdoptCheck.policy.title")}</AlertTitle>
      <AlertDescription className="break-words">
        {t("stackAdoptCheck.policy.body", { list: entries.join(", ") })}
      </AlertDescription>
    </Alert>
  );
}
