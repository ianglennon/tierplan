// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useLocation, useNavigate } from "@tanstack/react-router";
import { useEffect, useState } from "react";

import { clampQuery, SEARCH_MAX, searchable, useSearch } from "@/api/search";
import {
  choiceFilter,
  EnvCommandItems,
  useEnvChoices,
  useSelectedEnvironment,
} from "@/components/env-switcher";
import { Command, CommandDialog, CommandInput, CommandList } from "@/components/ui/command";
import { useSearchPalette } from "@/features/search/palette-context";
import {
  SearchError,
  SearchHits,
  SearchStatus,
  type SearchTarget,
} from "@/features/search/search-hits";
import { searchTiming, useDebounced } from "@/features/search/use-debounced";
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

// EnvPalette is the Cmd+K (Mac) or Ctrl+K palette (D-35, D-18), from any signed-in page. It is
// mounted once in the signed-in layout, and the header's SearchTrigger opens the same one.
//
// With nothing typed, or a single character, it is the sidebar switcher in a dialog: the same
// environment list. From 2 characters it searches the fleet: the server's answer in groups, with
// "Search all results" first and selected. The shortcut calls preventDefault so the browser's own
// binding (the address bar search in several browsers) does not fire, and does nothing at all
// while a text field or code editor has focus, so it never takes a keystroke from someone
// typing. Closing returns focus to where it was.
export function EnvPalette() {
  const t = useT();
  const navigate = useNavigate();
  const { open, setOpen, returnTo } = useSearchPalette();
  const [query, setQuery] = useState("");
  const { items } = useEnvChoices();
  const selected = useSelectedEnvironment(items);
  const pathname = useLocation({ select: (l) => l.pathname });

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
  }, [returnTo, setOpen]);

  const typed = query.trim();
  const searching = searchable(typed);
  // The server is asked once the typing pauses, and only while the palette is open.
  const settled = useDebounced(typed, searchTiming.debounceMs);
  const search = useSearch(open ? settled : "", "palette");
  const waiting = searching && (settled !== typed || search.isFetching);
  const failed = searching && search.isError && !waiting;
  const results = search.data?.counts.all ?? 0;
  const body = searching ? search.data : undefined;
  // What the live region says, read after the typing has paused for a moment.
  const status = useDebounced(
    waiting ? "searching" : body && !failed ? "done" : "idle",
    searchTiming.announceMs,
  );
  const partial = (body?.notSearched ?? []).map((n) => n.envName);

  const go = (target: SearchTarget) => {
    setOpen(false);
    void navigate({ to: target.to, params: target.params, search: target.search });
  };

  return (
    <CommandDialog
      open={open}
      onOpenChange={setOpen}
      title={t("search.palette.title")}
      description={t("search.palette.description")}
      // 640px on a wide screen, the full width under 768px.
      className="max-w-full sm:max-w-full md:max-w-[640px]"
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
        // The server's answer is already the right list: cmdk must not filter it again.
        shouldFilter={!searching}
        label={t("search.palette.title")}
        loop
        // Ctrl+K is cmdk's "previous item" binding, and Ctrl+N, J and P are the
        // others. None of them is wanted next to this shortcut.
        vimBindings={false}
      >
        <CommandInput
          value={query}
          onValueChange={(v) => setQuery(clampQuery(v))}
          maxLength={SEARCH_MAX}
          placeholder={t("search.palette.placeholder")}
        />
        {searching ? (
          <CommandList className="max-h-[400px]" aria-busy={waiting || undefined}>
            <SearchHits typed={typed} answered={settled} body={body} busy={waiting} onOpen={go} />
            {failed ? <SearchError onRetry={() => void search.refetch()} /> : null}
            <SearchStatus
              state={status === "searching" ? "searching" : status === "done" ? "done" : "idle"}
              environments={items.length}
              results={results}
            />
          </CommandList>
        ) : (
          <EnvCommandItems
            query={query}
            selectedId={selected?.id}
            pathname={pathname}
            onChosen={() => setOpen(false)}
          />
        )}
        <div className="border-t px-3 py-2 text-xs text-muted-foreground">
          {searching && partial.length > 0
            ? t("search.palette.partial", { names: partial.join(", ") })
            : t("search.palette.hint")}
        </div>
      </Command>
    </CommandDialog>
  );
}
