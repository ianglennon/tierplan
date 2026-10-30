// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useEffect, useRef } from "react";
import { createPortal } from "react-dom";

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

// RestoringOverlay covers the page while Coaming restarts into the backup. It is
// a status, not an alert: nothing has gone wrong. It holds the beforeunload
// prompt for as long as it is mounted.
export function RestoringOverlay() {
  const t = useT();
  useBlockUnload(true);
  const host = useRef<HTMLDivElement>(null);
  const card = useRef<HTMLDivElement>(null);
  // The overlay only looks modal. Everything else on the page is made inert, so
  // the keyboard cannot reach the sidebar behind it, and focus moves onto the
  // status so a screen reader lands on it.
  useEffect(() => {
    const behind = Array.from(document.body.children).filter(
      (el) => el !== host.current && !el.hasAttribute("inert"),
    );
    for (const el of behind) el.setAttribute("inert", "");
    card.current?.focus();
    return () => {
      for (const el of behind) el.removeAttribute("inert");
    };
  }, []);
  return createPortal(
    <div
      ref={host}
      className="fixed inset-0 z-[60] flex items-center justify-center bg-background/80 p-4 backdrop-blur-sm"
    >
      <Card ref={card} tabIndex={-1} role="status" className="w-full max-w-sm outline-none">
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
    </div>,
    document.body,
  );
}
