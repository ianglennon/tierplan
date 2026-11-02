// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { TriangleAlert } from "lucide-react";

import { Alert, AlertDescription } from "@/components/ui/alert";
import { useT } from "@/i18n/use-t";

// IdleBanner warns that the shell closes in a minute because nothing has happened in it
// (D-29). It overlays the top of the terminal, which does not move, and is announced
// politely as a status. Input or output dismisses it; the panel owns when.
export function IdleBanner() {
  const t = useT();
  return (
    <div className="pointer-events-none absolute inset-x-2 top-2 z-10">
      <Alert variant="warning" role="status" className="pointer-events-auto shadow-md">
        <TriangleAlert aria-hidden="true" />
        <AlertDescription className="text-foreground">
          {t("terminal.idle.warning")}
        </AlertDescription>
      </Alert>
    </div>
  );
}
