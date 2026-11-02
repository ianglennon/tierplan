// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { ExternalLink, Search } from "lucide-react";
import { forwardRef, useCallback, useImperativeHandle, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { Kbd } from "@/components/ui/kbd";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { KeyboardPopover } from "@/features/terminal/keyboard-popover";
import { useT } from "@/i18n/use-t";

// The font sizes the toolbar offers, and the one it starts at (D-33).
export const FONT_SIZES = [12, 14, 16] as const;
export type FontSize = (typeof FONT_SIZES)[number];
export const DEFAULT_FONT_SIZE: FontSize = 14;

// The two terminal preferences kept in this browser (D-33, UI-SPEC accessibility). They
// are the only things the terminal puts in storage: never what it shows or what is typed.
export const FONT_SIZE_KEY = "tierplan-terminal-font-size";
export const SCREEN_READER_KEY = "tierplan-terminal-screen-reader";

function readStored(key: string): string | null {
  try {
    return window.localStorage.getItem(key);
  } catch {
    return null; // storage can be blocked; the default applies
  }
}

function writeStored(key: string, value: string): void {
  try {
    window.localStorage.setItem(key, value);
  } catch {
    // The choice still applies until the page is left.
  }
}

// useTerminalPrefs is the font size (12, 14 or 16, default 14) and whether screen reader
// support is on (default off), each remembered per browser.
export function useTerminalPrefs() {
  const [fontSize, setFontSizeState] = useState<FontSize>(() => {
    const n = Number(readStored(FONT_SIZE_KEY));
    return FONT_SIZES.find((s) => s === n) ?? DEFAULT_FONT_SIZE;
  });
  const [screenReader, setScreenReaderState] = useState(
    () => readStored(SCREEN_READER_KEY) === "true",
  );
  const setFontSize = useCallback((size: FontSize) => {
    setFontSizeState(size);
    writeStored(FONT_SIZE_KEY, String(size));
  }, []);
  const setScreenReader = useCallback((on: boolean) => {
    setScreenReaderState(on);
    writeStored(SCREEN_READER_KEY, String(on));
  }, []);
  return { fontSize, setFontSize, screenReader, setScreenReader };
}

// What the person can ask of the toolbar from outside it.
export interface TerminalToolbarHandle {
  // Puts focus on the first control: where Ctrl+Shift+Home takes it.
  focusFirst: () => void;
}

interface TerminalToolbarProps {
  // True while a session is open (connecting or connected). Once it has ended the
  // screen stays for copying, so the toolbar keeps its tools and drops the status and
  // End session.
  live: boolean;
  connected: boolean;
  // "{shell} as {user} in {directory}", already worded.
  summary: string;
  fontSize: FontSize;
  onFontSizeChange: (size: FontSize) => void;
  screenReader: boolean;
  onScreenReaderChange: (on: boolean) => void;
  onSearch: () => void;
  // Left out in the pop-out, which is already a window of its own.
  onOpenWindow?: () => void;
  onEnd: () => void;
}

// TerminalToolbar is the row above the terminal: the status, a summary of what runs,
// Font size, Search, Keyboard, Open in new window and End session (UI-SPEC "Shell").
// One row 48px high on a desktop that wraps on a narrow screen.
export const TerminalToolbar = forwardRef<TerminalToolbarHandle, TerminalToolbarProps>(
  function TerminalToolbar(p, ref) {
    const t = useT();
    const bar = useRef<HTMLDivElement>(null);
    useImperativeHandle(ref, () => ({
      focusFirst: () => {
        bar.current?.querySelector<HTMLElement>("button:not([disabled])")?.focus();
      },
    }));
    return (
      <div
        ref={bar}
        role="toolbar"
        aria-label={t("terminal.toolbar.label")}
        className="flex min-h-12 flex-wrap items-center gap-2 rounded-md border bg-card px-3 py-2"
      >
        {p.live ? (
          <output className="inline-flex shrink-0 items-center gap-1.5 rounded-full border px-2 py-0.5 text-xs">
            <span
              aria-hidden="true"
              className={
                p.connected
                  ? "inline-block size-2 shrink-0 rounded-full bg-status-online"
                  : "inline-block size-2 shrink-0 rounded-full bg-status-warning"
              }
            />
            <span className={p.connected ? "text-status-online-text" : "text-status-warning-text"}>
              {t(p.connected ? "terminal.status.connected" : "terminal.status.connecting")}
            </span>
          </output>
        ) : null}
        <Tooltip>
          <TooltipTrigger asChild>
            <span
              // biome-ignore lint/a11y/noNoninteractiveTabindex: the tooltip must open on keyboard focus (UI-SPEC D-66)
              tabIndex={0}
              className="min-w-0 flex-1 truncate rounded-sm font-mono text-xs text-muted-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:basis-full"
            >
              {p.summary}
            </span>
          </TooltipTrigger>
          <TooltipContent className="max-w-sm break-all">{p.summary}</TooltipContent>
        </Tooltip>
        <Select
          value={String(p.fontSize)}
          onValueChange={(v) => {
            const size = FONT_SIZES.find((s) => String(s) === v);
            if (size) p.onFontSizeChange(size);
          }}
        >
          <SelectTrigger
            size="sm"
            aria-label={t("terminal.toolbar.fontSize")}
            title={t("terminal.toolbar.fontSize")}
            className="w-20 max-md:h-11"
          >
            <SelectValue />
          </SelectTrigger>
          <SelectContent position="popper" align="end">
            {FONT_SIZES.map((s) => (
              <SelectItem key={s} value={String(s)}>
                {s}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="max-md:h-11"
          onClick={p.onSearch}
        >
          <Search aria-hidden="true" />
          {t("terminal.toolbar.search")}
          <Kbd className="max-md:hidden">Ctrl+Shift+F</Kbd>
        </Button>
        <KeyboardPopover
          screenReader={p.screenReader}
          onScreenReaderChange={p.onScreenReaderChange}
        />
        {p.onOpenWindow ? (
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            onClick={p.onOpenWindow}
          >
            <ExternalLink aria-hidden="true" />
            {t("terminal.toolbar.openWindow")}
          </Button>
        ) : null}
        {p.live ? (
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            onClick={p.onEnd}
          >
            {t("terminal.toolbar.end")}
          </Button>
        ) : null}
      </div>
    );
  },
);
