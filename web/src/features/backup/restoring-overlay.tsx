// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useEffect } from "react";

import { Card, CardContent, CardHeader } from "@/components/ui/card";
import { Spinner } from "@/components/ui/spinner";
import { useT } from "@/i18n/use-t";

// useBlockUnload asks the browser to confirm before the page is closed or
// reloaded while active. A restore is the one request in the app that blocks
// navigation (UI-SPEC "Backup page").
export function useBlockUnload(active: boolean): void {
  useEffect(() => {
    if (!active) return;
    const hold = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      // Older browsers need a value set for the prompt to show.
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", hold);
    return () => window.removeEventListener("beforeunload", hold);
  }, [active]);
}

// RestoringOverlay covers the page while Bayplan restarts into the backup. It is
// a status, not an alert: nothing has gone wrong. It holds the beforeunload
// prompt for as long as it is mounted.
export function RestoringOverlay() {
  const t = useT();
  useBlockUnload(true);
  return (
    <div className="fixed inset-0 z-[60] flex items-center justify-center bg-background/80 p-4 backdrop-blur-sm">
      <Card role="status" className="w-full max-w-sm">
        <CardHeader>
          <h3 className="flex items-center gap-2 text-base font-semibold">
            <Spinner aria-hidden="true" />
            {t("restore.overlay.title")}
          </h3>
        </CardHeader>
        <CardContent>
          <p className="text-sm text-muted-foreground">{t("restore.overlay.body")}</p>
        </CardContent>
      </Card>
    </div>
  );
}
