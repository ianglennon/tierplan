// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { FitAddon } from "@xterm/addon-fit";
import { Terminal as XTerm } from "@xterm/xterm";
import { useEffect, useRef } from "react";
import "@xterm/xterm/css/xterm.css";

import { useT } from "@/i18n/use-t";

// What the panel can ask of the terminal once it is up.
export interface TerminalApi {
  // Shows the process's output. Bytes, so a character split across two frames is put
  // together by the terminal and not cut by the page.
  write: (bytes: Uint8Array) => void;
  focus: () => void;
}

export interface TerminalProps {
  // The container's name, for the terminal's accessible name.
  name: string;
  // True once the session has ended: the screen stays for copying, and takes no input.
  readOnly: boolean;
  // What the person typed (or pasted), as the bytes to send to the process.
  onInput: (bytes: Uint8Array) => void;
  // The size in characters after the terminal fitted its box, once at the start and
  // again, debounced, whenever the box changes.
  onResize: (cols: number, rows: number) => void;
  // Hands the panel the terminal's API when it is up, and null when it goes.
  register: (api: TerminalApi | null) => void;
}

// The font the app's mono text uses (index.css). It is loaded before the terminal
// measures a cell, or the grid is measured in the fallback font and drawn in this one.
const FONT_FAMILY = '"Geist Mono Variable", ui-monospace, monospace';
const FONT_SIZE = 14;
const SCROLLBACK = 5000;
const RESIZE_DEBOUNCE_MS = 100;

// A fixed dark theme for now; the terminal follows the app's theme with the toolbar.
const THEME = { background: "#0f1115", foreground: "#e6e8ee", cursor: "#e6e8ee" } as const;

// xterm hands typed text as a string, and mouse and other binary reports as a string
// of bytes (one byte to a character). Both become the bytes the process receives.
const encoder = new TextEncoder();
function binaryBytes(s: string): Uint8Array {
  const out = new Uint8Array(s.length);
  for (let i = 0; i < s.length; i++) out[i] = s.charCodeAt(i) & 0xff;
  return out;
}

// Terminal hosts xterm.js in its box: it fits the box, keeps the size current and
// reports typed bytes. It never logs, stores or sends what it shows anywhere else.
export function Terminal({ name, readOnly, onInput, onResize, register }: TerminalProps) {
  const t = useT();
  const host = useRef<HTMLElement>(null);
  const xterm = useRef<XTerm | null>(null);
  // The latest callbacks, so the terminal made once keeps calling the current ones.
  const handlers = useRef({ onInput, onResize, register });
  handlers.current = { onInput, onResize, register };
  const readOnlyNow = useRef(readOnly);
  readOnlyNow.current = readOnly;
  const label = t("terminal.name", { name });

  // The terminal is made once for the life of this component; the panel keys it by
  // session, so a new session is a new terminal. The label is read once, at the start.
  // biome-ignore lint/correctness/useExhaustiveDependencies: made once, see above
  useEffect(() => {
    let disposed = false;
    let cleanup = () => {};

    const start = async () => {
      try {
        await document.fonts?.load(`${FONT_SIZE}px "Geist Mono Variable"`);
      } catch {
        // Unmeasured is better than no terminal.
      }
      const el = host.current;
      if (disposed || !el) return;

      const reduced = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ?? false;
      const term = new XTerm({
        fontFamily: FONT_FAMILY,
        fontSize: FONT_SIZE,
        scrollback: SCROLLBACK,
        cursorBlink: !reduced,
        theme: THEME,
      });
      const fit = new FitAddon();
      term.loadAddon(fit);
      term.open(el);
      term.textarea?.setAttribute("aria-label", label);
      term.options.disableStdin = readOnlyNow.current;
      xterm.current = term;

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
      refit();

      const data = term.onData((s) => handlers.current.onInput(encoder.encode(s)));
      const binary = term.onBinary((s) => handlers.current.onInput(binaryBytes(s)));
      let timer: ReturnType<typeof setTimeout> | undefined;
      const observer =
        typeof ResizeObserver === "undefined"
          ? undefined
          : new ResizeObserver(() => {
              clearTimeout(timer);
              timer = setTimeout(refit, RESIZE_DEBOUNCE_MS);
            });
      observer?.observe(el);

      handlers.current.register({
        write: (bytes) => term.write(bytes),
        focus: () => term.focus(),
      });
      term.focus();

      cleanup = () => {
        clearTimeout(timer);
        observer?.disconnect();
        data.dispose();
        binary.dispose();
        handlers.current.register(null);
        term.dispose();
        xterm.current = null;
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

  return (
    <section
      ref={host}
      aria-label={label}
      style={{ backgroundColor: THEME.background }}
      className="h-full min-h-64 w-full overflow-hidden rounded-md border p-2"
    />
  );
}
