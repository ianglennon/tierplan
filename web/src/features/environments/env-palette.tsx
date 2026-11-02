// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useLocation } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";

import {
  choiceFilter,
  EnvCommandItems,
  useEnvChoices,
  useSelectedEnvironment,
} from "@/components/env-switcher";
import { Command, CommandDialog, CommandInput } from "@/components/ui/command";
import { useT } from "@/i18n/use-t";
import { isMacPlatform } from "@/lib/env-nav";

// The palette's own root, so its own filter is never taken for "another text
// field" when the shortcut is pressed inside it.
const PALETTE_ATTR = "data-env-palette";

// isOtherTextTarget is true when the shortcut belongs to what has focus: a text
// field or a code editor that is not the palette. A button, a link or the page
// itself does not count, so the shortcut works from anywhere else.
//
// A code editor must set `data-code-editor` on its root. CodeMirror's editable
// is a contenteditable, which is caught anyway, but the attribute is the stated
// rule (UI-SPEC: Phase 7's editor keeps it) and also covers an editor that is
// not editable at that moment.
function isOtherTextTarget(el: Element | null): boolean {
  if (!(el instanceof HTMLElement)) return false;
  if (el.closest(`[${PALETTE_ATTR}]`)) return false;
  if (el.closest("[data-code-editor]")) return true;
  if (el.isContentEditable || el.closest('[contenteditable]:not([contenteditable="false"])')) {
    return true;
  }
  if (el instanceof HTMLTextAreaElement || el instanceof HTMLSelectElement) return true;
  if (el instanceof HTMLInputElement) {
    return ![
      "button",
      "checkbox",
      "radio",
      "submit",
      "reset",
      "range",
      "color",
      "file",
      "image",
    ].includes(el.type);
  }
  return false;
}

// EnvPalette is the Cmd+K (Mac) or Ctrl+K palette (D-35): the same list as the
// sidebar switcher in a dialog, from any signed-in page. It is mounted once in
// the signed-in layout. The shortcut calls preventDefault so the browser's own
// binding (the address bar search in several browsers) does not fire, and does
// nothing at all while a text field or code editor has focus, so it never takes
// a keystroke from someone typing. Closing returns focus to where it was.
export function EnvPalette() {
  const t = useT();
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const { items } = useEnvChoices();
  const selected = useSelectedEnvironment(items);
  const pathname = useLocation({ select: (l) => l.pathname });
  // What had focus when the palette opened. A dialog with no trigger button
  // cannot return focus on its own.
  const returnTo = useRef<Element | null>(null);

  // Whatever closes it, it opens empty next time.
  useEffect(() => {
    if (!open) setQuery("");
  }, [open]);

  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key.toLowerCase() !== "k" || e.shiftKey || e.altKey || e.defaultPrevented) return;
      // The platform's key and not the other: Ctrl+K is not the shortcut on a
      // Mac, and Meta+K is not one elsewhere.
      const mac = isMacPlatform();
      if (mac ? !e.metaKey || e.ctrlKey : !e.ctrlKey || e.metaKey) return;
      if (isOtherTextTarget(document.activeElement)) return;
      e.preventDefault();
      returnTo.current = document.activeElement;
      setOpen((was) => !was);
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, []);

  return (
    <CommandDialog
      open={open}
      onOpenChange={setOpen}
      title={t("switcher.palette.title")}
      description={t("switcher.palette.description")}
      // 512px on a wide screen, the full width under 768px.
      className="max-w-full sm:max-w-full md:max-w-lg"
      onCloseAutoFocus={(event) => {
        event.preventDefault();
        const back = returnTo.current;
        if (back instanceof HTMLElement && back.isConnected) back.focus();
        else document.getElementById("main")?.focus();
      }}
    >
      <Command
        {...{ [PALETTE_ATTR]: "" }}
        filter={choiceFilter}
        label={t("switcher.palette.placeholder")}
        loop
        // Ctrl+K is cmdk's "previous item" binding, and Ctrl+N, J and P are the
        // others. None of them is wanted next to this shortcut.
        vimBindings={false}
      >
        <CommandInput
          value={query}
          onValueChange={setQuery}
          placeholder={t("switcher.palette.placeholder")}
        />
        <EnvCommandItems
          query={query}
          selectedId={selected?.id}
          pathname={pathname}
          onChosen={() => setOpen(false)}
        />
      </Command>
    </CommandDialog>
  );
}
