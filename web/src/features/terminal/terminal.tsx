// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { FitAddon } from "@xterm/addon-fit";
import { SearchAddon } from "@xterm/addon-search";
import { WebglAddon } from "@xterm/addon-webgl";
import { Terminal as XTerm } from "@xterm/xterm";
import { ChevronDown, ChevronUp, X } from "lucide-react";
import { type KeyboardEvent, useEffect, useMemo, useRef, useState } from "react";
import "@xterm/xterm/css/xterm.css";

import { copyText } from "@/components/copyable-code";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { useResolvedTheme, xtermTheme } from "@/features/terminal/terminal-theme";
import type { FontSize } from "@/features/terminal/terminal-toolbar";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";

// What the panel can ask of the terminal once it is up.
export interface TerminalApi {
  // Shows the process's output. Bytes, so a character split across two frames is put
  // together by the terminal and not cut by the page.
  write: (bytes: Uint8Array) => void;
  focus: () => void;
  // Opens the search bar over the terminal's top right and puts the cursor in it.
  openSearch: () => void;
}

export interface TerminalProps {
  // The container's name, for the terminal's accessible name.
  name: string;
  // True once the session has ended: the screen stays for copying, and takes no input.
  readOnly: boolean;
  // The size of the text; the terminal refits when it changes.
  fontSize: FontSize;
  // Turns on xterm's screen reader support, which is slower to draw and so off by default.
  screenReader: boolean;
  // What the person typed (or pasted), as the bytes to send to the process.
  onInput: (bytes: Uint8Array) => void;
  // The size in characters after the terminal fitted its box, once at the start and
  // again, debounced, whenever the box changes.
  onResize: (cols: number, rows: number) => void;
  // Ctrl+Shift+Home: the person asks to leave the terminal, which otherwise keeps every
  // key, so focus goes to the toolbar.
  onLeave: () => void;
  // Hands the panel the terminal's API when it is up, and null when it goes.
  register: (api: TerminalApi | null) => void;
}

// The font the app's mono text uses (index.css). It is loaded before the terminal
// measures a cell, or the grid is measured in the fallback font and drawn in this one.
const FONT_FAMILY = '"Geist Mono Variable", ui-monospace, monospace';
const SCROLLBACK = 5000;
const RESIZE_DEBOUNCE_MS = 100;

// The highlight of search matches; xterm wants #RRGGBB here, and these read on both themes.
const MATCH = {
  matchBackground: "#facc15",
  matchBorder: "#a16207",
  matchOverviewRuler: "#facc15",
  activeMatchBackground: "#fb923c",
  activeMatchBorder: "#c2410c",
  activeMatchColorOverviewRuler: "#fb923c",
} as const;

// xterm hands typed text as a string, and mouse and other binary reports as a string
// of bytes (one byte to a character). Both become the bytes the process receives.
const encoder = new TextEncoder();
function binaryBytes(s: string): Uint8Array {
  const out = new Uint8Array(s.length);
  for (let i = 0; i < s.length; i++) out[i] = s.charCodeAt(i) & 0xff;
  return out;
}

// The chords the terminal keeps for itself (UI-SPEC "Terminal behaviour"): everything
// else, Tab and Esc included, goes to the shell. Copy, paste and search use Ctrl or Cmd
// with Shift, which no shell program binds; the way out is Ctrl+Shift+Home.
type Chord = "copy" | "paste" | "search" | "leave";
export function chordOf(ev: KeyboardEvent | globalThis.KeyboardEvent): Chord | null {
  if (!ev.shiftKey || ev.altKey) return null;
  const key = ev.key.toLowerCase();
  if (ev.ctrlKey && !ev.metaKey && key === "home") return "leave";
  if (!ev.ctrlKey && !ev.metaKey) return null;
  if (key === "c") return "copy";
  if (key === "v") return "paste";
  if (key === "f") return "search";
  return null;
}

// Terminal hosts xterm.js in its box: it fits the box, keeps the size current and
// reports typed bytes. It draws with WebGL and falls back to xterm's DOM renderer when
// WebGL cannot start or its context is lost, saying nothing about it. It never logs,
// stores or sends what it shows anywhere else.
export function Terminal({
  name,
  readOnly,
  fontSize,
  screenReader,
  onInput,
  onResize,
  onLeave,
  register,
}: TerminalProps) {
  const t = useT();
  const host = useRef<HTMLDivElement>(null);
  const xterm = useRef<XTerm | null>(null);
  const search = useRef<SearchAddon | null>(null);
  const refitNow = useRef<() => void>(() => {});
  const searchInput = useRef<HTMLInputElement>(null);
  const [searching, setSearching] = useState(false);
  const [query, setQuery] = useState("");
  const [noMatch, setNoMatch] = useState(false);
  const appTheme = useResolvedTheme();
  const theme = useMemo(() => xtermTheme(appTheme), [appTheme]);
  // The latest values, so the terminal made once keeps using the current ones.
  const handlers = useRef({ onInput, onResize, onLeave, register, t });
  handlers.current = { onInput, onResize, onLeave, register, t };
  const now = useRef({ readOnly, fontSize, screenReader, theme });
  now.current = { readOnly, fontSize, screenReader, theme };
  const label = t("terminal.name", { name });

  // The terminal is made once for the life of this component; the panel keys it by
  // session, so a new session is a new terminal. The label is read once, at the start.
  // biome-ignore lint/correctness/useExhaustiveDependencies: made once, see above
  useEffect(() => {
    let disposed = false;
    let cleanup = () => {};

    const start = async () => {
      try {
        await document.fonts?.load(`${now.current.fontSize}px "Geist Mono Variable"`);
      } catch {
        // Unmeasured is better than no terminal.
      }
      const el = host.current;
      if (disposed || !el) return;

      const motion = window.matchMedia?.("(prefers-reduced-motion: reduce)");
      const term = new XTerm({
        fontFamily: FONT_FAMILY,
        fontSize: now.current.fontSize,
        fontWeightBold: 600,
        scrollback: SCROLLBACK,
        cursorBlink: !(motion?.matches ?? false),
        screenReaderMode: now.current.screenReader,
        theme: now.current.theme,
      });
      const fit = new FitAddon();
      const finder = new SearchAddon();
      term.loadAddon(fit);
      term.loadAddon(finder);
      term.open(el);
      // WebGL draws much faster; where it cannot start, or later loses its context, the
      // addon is disposed and xterm goes on with its DOM renderer. Nothing is shown.
      try {
        const gl = new WebglAddon();
        gl.onContextLoss(() => gl.dispose());
        term.loadAddon(gl);
      } catch {
        // The DOM renderer is already in use.
      }
      term.textarea?.setAttribute("aria-label", label);
      term.options.disableStdin = now.current.readOnly;
      xterm.current = term;
      search.current = finder;

      let last = "";
      const refit = () => {
        try {
          fit.fit();
        } catch {
          return; // the box has no size yet
        }
        const size = `${term.cols}x${term.rows}`;
        if (size === last) return;
        last = size;
        handlers.current.onResize(term.cols, term.rows);
      };
      refitNow.current = refit;
      refit();

      // The cursor stops blinking when the person asks for less motion, also later.
      const onMotion = () => {
        term.options.cursorBlink = !(motion?.matches ?? false);
      };
      motion?.addEventListener?.("change", onMotion);

      const data = term.onData((s) => handlers.current.onInput(encoder.encode(s)));
      const binary = term.onBinary((s) => handlers.current.onInput(binaryBytes(s)));

      // Copy, paste, search and the way out; every other key is the shell's. Copy-on-select
      // is off: nothing is copied until the person asks. Paste is not done here: the chord
      // is let through so the browser's own paste event reaches xterm, which works on a
      // plain-HTTP address where the Clipboard API does not.
      term.attachCustomKeyEventHandler((ev) => {
        if (ev.type !== "keydown") return true;
        switch (chordOf(ev)) {
          case "copy": {
            ev.preventDefault();
            const selected = term.getSelection();
            if (selected) {
              void copyText(selected, el).then((ok) => {
                if (!ok) toastError(handlers.current.t("env.copy.failed"));
              });
            }
            return false;
          }
          case "paste":
            return false;
          case "search":
            ev.preventDefault();
            setSearching(true);
            return false;
          case "leave":
            ev.preventDefault();
            handlers.current.onLeave();
            return false;
          default:
            return true;
        }
      });

      // One fit and one resize frame, 100 ms after the box stops changing.
      let timer: ReturnType<typeof setTimeout> | undefined;
      const later = () => {
        clearTimeout(timer);
        timer = setTimeout(refit, RESIZE_DEBOUNCE_MS);
      };
      const observer =
        typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(later);
      observer?.observe(el);
      window.addEventListener("resize", later);

      handlers.current.register({
        write: (bytes) => term.write(bytes),
        focus: () => term.focus(),
        openSearch: () => setSearching(true),
      });
      term.focus();

      cleanup = () => {
        clearTimeout(timer);
        observer?.disconnect();
        window.removeEventListener("resize", later);
        motion?.removeEventListener?.("change", onMotion);
        data.dispose();
        binary.dispose();
        handlers.current.register(null);
        term.dispose();
        xterm.current = null;
        search.current = null;
        refitNow.current = () => {};
      };
    };
    void start();

    return () => {
      disposed = true;
      cleanup();
    };
  }, []);

  useEffect(() => {
    if (xterm.current) xterm.current.options.disableStdin = readOnly;
  }, [readOnly]);

  // A new size of text changes the grid, so the terminal fits again and the process is
  // told the new size once.
  useEffect(() => {
    const term = xterm.current;
    if (!term) return;
    term.options.fontSize = fontSize;
    refitNow.current();
  }, [fontSize]);

  useEffect(() => {
    if (xterm.current) xterm.current.options.screenReaderMode = screenReader;
  }, [screenReader]);

  // The colours follow the app's theme, live.
  useEffect(() => {
    if (xterm.current) xterm.current.options.theme = theme;
  }, [theme]);

  useEffect(() => {
    if (!searching) return;
    searchInput.current?.focus();
    searchInput.current?.select();
  }, [searching]);

  function find(forward: boolean) {
    if (query === "") {
      search.current?.clearDecorations();
      setNoMatch(false);
      return;
    }
    const options = { decorations: MATCH };
    const found = forward
      ? search.current?.findNext(query, options)
      : search.current?.findPrevious(query, options);
    setNoMatch(found === false);
  }

  function closeSearch() {
    search.current?.clearDecorations();
    setSearching(false);
    setNoMatch(false);
    xterm.current?.focus();
  }

  function onSearchKey(event: KeyboardEvent<HTMLInputElement>) {
    if (event.key === "Enter") {
      event.preventDefault();
      find(!event.shiftKey);
    } else if (event.key === "Escape") {
      event.preventDefault();
      closeSearch();
    }
  }

  return (
    <div className="relative h-full min-h-64 w-full">
      {/* The border and padding sit on this box, not on the element xterm is opened
          in: the fit addon measures that element's parent and subtracts only xterm's
          own padding, so padding here would leave the last row half cut off. */}
      <section
        aria-label={label}
        style={{ backgroundColor: theme.background }}
        className="h-full min-h-64 w-full overflow-hidden rounded-md border p-2"
      >
        <div ref={host} className="h-full w-full" />
      </section>
      {searching ? (
        <search
          aria-label={t("terminal.search.label")}
          className="absolute top-2 right-4 z-10 flex max-w-[calc(100%-2rem)] items-center gap-1 rounded-lg border bg-popover p-1 shadow-md"
        >
          <Input
            ref={searchInput}
            value={query}
            onChange={(e) => {
              // Matches are found as the person types.
              const value = e.target.value;
              setQuery(value);
              if (value === "") {
                search.current?.clearDecorations();
                setNoMatch(false);
              } else {
                const found = search.current?.findNext(value, {
                  decorations: MATCH,
                  incremental: true,
                });
                setNoMatch(found === false);
              }
            }}
            onKeyDown={onSearchKey}
            aria-label={t("terminal.search.label")}
            aria-invalid={noMatch}
            placeholder={t("terminal.search.placeholder")}
            className="h-11 w-44 min-w-0 md:h-8"
            autoComplete="off"
            spellCheck={false}
          />
          <Button
            type="button"
            variant="ghost"
            size="icon-sm"
            className="max-md:size-11"
            aria-label={t("terminal.search.previous")}
            onClick={() => find(false)}
          >
            <ChevronUp aria-hidden="true" />
          </Button>
          <Button
            type="button"
            variant="ghost"
            size="icon-sm"
            className="max-md:size-11"
            aria-label={t("terminal.search.next")}
            onClick={() => find(true)}
          >
            <ChevronDown aria-hidden="true" />
          </Button>
          <Button
            type="button"
            variant="ghost"
            size="icon-sm"
            className="max-md:size-11"
            aria-label={t("terminal.search.close")}
            onClick={closeSearch}
          >
            <X aria-hidden="true" />
          </Button>
          <span role="status" className="sr-only">
            {noMatch ? t("terminal.search.noMatch") : ""}
          </span>
        </search>
      ) : null}
    </div>
  );
}
