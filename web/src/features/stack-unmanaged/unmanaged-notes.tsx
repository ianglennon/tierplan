// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Info, TriangleAlert } from "lucide-react";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { useT } from "@/i18n/use-t";

// UnmanagedNote is the note every unmanaged stack's page opens with (UI-SPEC "Unmanaged stack page"): TierPlan
// does not manage the stack, what the page is for and what adopting does. It is static content on load, so it is
// a status and not announced as an alert.
export function UnmanagedNote() {
  const t = useT();
  return (
    <Alert role="status">
      <Info aria-hidden="true" />
      <AlertTitle>{t("stackUnmanagedPage.note.title")}</AlertTitle>
      <AlertDescription>{t("stackUnmanagedPage.note.body")}</AlertDescription>
    </Alert>
  );
}

// OrphanIdNote is shown only when an object of the stack carries a TierPlan ID this instance has no record of
// (D-11). The ID itself is never sent to the browser, so the note cannot show it.
export function OrphanIdNote() {
  const t = useT();
  return (
    <Alert role="status">
      <Info aria-hidden="true" />
      <AlertTitle>{t("stackUnmanagedPage.orphan.title")}</AlertTitle>
      <AlertDescription>{t("stackUnmanagedPage.orphan.body")}</AlertDescription>
    </Alert>
  );
}

// PortainerAdvice is shown only when the stack carries a Portainer marker (D-19). It is advice and not an
// alert: nothing is wrong on the page, so it is a status, never announced as an error.
export function PortainerAdvice() {
  const t = useT();
  return (
    <Alert variant="warning" role="status">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle>{t("stackUnmanagedPage.portainer.title")}</AlertTitle>
      <AlertDescription>{t("stackUnmanagedPage.portainer.body")}</AlertDescription>
    </Alert>
  );
}

// NotAdoptableNote says why a stack cannot be adopted when the server says it cannot: TierPlan's own stack, its
// agent's, or a name the stack-name rule of the environment's target refuses (D-49, D-53). The reason is the
// server's own sentence, worded from the same rule text as the Create stack refusal, and is shown as given.
export function NotAdoptableNote({ reason }: { reason: string }) {
  const t = useT();
  return (
    <Alert role="status">
      <Info aria-hidden="true" />
      <AlertTitle>{t("stackUnmanaged.notAdoptable.title")}</AlertTitle>
      <AlertDescription>{reason}</AlertDescription>
    </Alert>
  );
}
