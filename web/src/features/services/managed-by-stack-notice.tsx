// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Info } from "lucide-react";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { useT } from "@/i18n/use-t";

interface ManagedByStackNoticeProps {
  // The stack the service belongs to. It is text from Docker, drawn as text.
  stack: string;
}

// ManagedByStackNotice says a service belongs to a stack, so a change made here would be
// overwritten by the next deploy (D-05, UI-SPEC default 10). It is information, not a warning:
// scaling, forcing an update and rolling back stay possible. Phase 7 adds the link that opens
// the stack.
export function ManagedByStackNotice({ stack }: ManagedByStackNoticeProps) {
  const t = useT();
  return (
    <Alert>
      <Info aria-hidden="true" />
      <AlertTitle>{t("servicedetail.stackOwned.title", { stack })}</AlertTitle>
      <AlertDescription>{t("servicedetail.stackOwned.body")}</AlertDescription>
    </Alert>
  );
}
