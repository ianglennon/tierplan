// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { Terminal, type TerminalApi } from "@/features/terminal/terminal";
import { DEFAULT_FONT_SIZE, type FontSize } from "@/features/terminal/terminal-toolbar";
import "@/i18n";

// The terminal's behaviour (plan 04-24, D-33). xterm needs a real layout and a GPU, so
// it is a stand-in that records what the component asks of it; the component's own logic
// is what is under test: the renderer fallback, the chords, the search bar, the refit.

const x = vi.hoisted(() => {
  type KeyHandler = (ev: KeyboardEvent) => boolean;
  const state = {
    terms: [] as FakeTerm[],
    webglFails: false,
    webgls: [] as FakeWebgl[],
    searches: [] as FakeSearch[],
    fitSize: { cols: 80, rows: 24 },
    fits: 0,
    selection: "",
  };
  class FakeTerm {
    options: Record<string, unknown>;
    cols = 0;
    rows = 0;
    textarea = document.createElement("textarea");
    keyHandler: KeyHandler | null = null;
    written: Uint8Array[] = [];
    focused = 0;
    disposed = false;
    addons: unknown[] = [];
    dataCb: ((s: string) => void) | null = null;
    binaryCb: ((s: string) => void) | null = null;
    constructor(options: Record<string, unknown>) {
      this.options = { ...options };
      state.terms.push(this);
    }
    loadAddon(a: { activate?: (t: FakeTerm) => void }) {
      this.addons.push(a);
      a.activate?.(this);
    }
    open() {}
    focus() {
      this.focused += 1;
    }
    write(b: Uint8Array) {
      this.written.push(b);
    }
    getSelection() {
      return state.selection;
    }
    attachCustomKeyEventHandler(h: KeyHandler) {
      this.keyHandler = h;
    }
    onData(cb: (s: string) => void) {
      this.dataCb = cb;
      return { dispose() {} };
    }
    onBinary(cb: (s: string) => void) {
      this.binaryCb = cb;
      return { dispose() {} };
    }
    dispose() {
      this.disposed = true;
    }
  }
  class FakeWebgl {
    lost: (() => void) | null = null;
    disposed = false;
    constructor() {
      if (state.webglFails) throw new Error("WebGL is not available");
      state.webgls.push(this);
    }
    onContextLoss(cb: () => void) {
      this.lost = cb;
      return { dispose() {} };
    }
    dispose() {
      this.disposed = true;
    }
  }
  class FakeSearch {
    calls: string[] = [];
    result = true;
    cleared = 0;
    constructor() {
      state.searches.push(this);
    }
    findNext(term: string) {
      this.calls.push(`next:${term}`);
      return this.result;
    }
    findPrevious(term: string) {
      this.calls.push(`prev:${term}`);
      return this.result;
    }
    clearDecorations() {
      this.cleared += 1;
    }
  }
  class FakeFit {
    term: FakeTerm | null = null;
    activate(t: FakeTerm) {
      this.term = t;
    }
    fit() {
      state.fits += 1;
      if (this.term) {
        this.term.cols = state.fitSize.cols;
        this.term.rows = state.fitSize.rows;
      }
    }
  }
  return { state, FakeTerm, FakeWebgl, FakeSearch, FakeFit };
});

vi.mock("@xterm/xterm", () => ({ Terminal: x.FakeTerm }));
vi.mock("@xterm/xterm/css/xterm.css", () => ({}));
vi.mock("@xterm/addon-fit", () => ({ FitAddon: x.FakeFit }));
vi.mock("@xterm/addon-webgl", () => ({ WebglAddon: x.FakeWebgl }));
vi.mock("@xterm/addon-search", () => ({ SearchAddon: x.FakeSearch }));

const clip = vi.hoisted(() => ({
  copyText: vi.fn(async (_text: string, _host?: Element | null) => true),
}));
vi.mock("@/components/copyable-code", () => clip);
const toast = vi.hoisted(() => ({ toastError: vi.fn() }));
vi.mock("@/lib/toast", () => toast);

const props = {
  name: "web",
  readOnly: false,
  fontSize: DEFAULT_FONT_SIZE as FontSize,
  screenReader: false,
  onInput: vi.fn(),
  onResize: vi.fn(),
  onLeave: vi.fn(),
  register: vi.fn(),
};

beforeEach(() => {
  x.state.terms = [];
  x.state.webgls = [];
  x.state.searches = [];
  x.state.webglFails = false;
  x.state.fitSize = { cols: 80, rows: 24 };
  x.state.fits = 0;
  x.state.selection = "";
  clip.copyText.mockClear();
  clip.copyText.mockResolvedValue(true);
  toast.toastError.mockClear();
  for (const f of [props.onInput, props.onResize, props.onLeave, props.register]) f.mockClear();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
  document.documentElement.classList.remove("dark");
  document.documentElement.removeAttribute("style");
});

async function mount(over: Partial<typeof props> = {}) {
  const view = render(<Terminal {...props} {...over} />);
  await waitFor(() => expect(x.state.terms).toHaveLength(1));
  const term = x.state.terms[0] as InstanceType<typeof x.FakeTerm>;
  return {
    view,
    term,
    rerender: (next: Partial<typeof props>) => view.rerender(<Terminal {...props} {...next} />),
  };
}

// key sends a keydown through the handler xterm was given, as xterm would, and says
// whether xterm may go on with it and whether the page's default was prevented.
function key(
  term: InstanceType<typeof x.FakeTerm>,
  init: KeyboardEventInit & { key: string },
): { passed: boolean; prevented: boolean } {
  const ev = new KeyboardEvent("keydown", { bubbles: true, cancelable: true, ...init });
  const passed = term.keyHandler?.(ev) ?? true;
  return { passed, prevented: ev.defaultPrevented };
}

describe("the terminal's setup (D-33)", () => {
  it("keeps 5000 lines, draws at 14, bolds at 600, blinks the cursor and starts without screen reader support", async () => {
    const { term } = await mount();
    expect(term.options).toMatchObject({
      scrollback: 5000,
      fontSize: 14,
      fontWeightBold: 600,
      cursorBlink: true,
      screenReaderMode: false,
    });
    expect(String(term.options.fontFamily)).toContain("Geist Mono Variable");
    expect((term.options.theme as { background: string }).background).toBe("#ffffff");
    // Fit, search and WebGL are loaded; nothing registers a bell handler, so it is silent.
    expect(term.addons).toHaveLength(3);
  });

  it("turns the cursor blink off under reduced motion", async () => {
    vi.spyOn(window, "matchMedia").mockImplementation(
      (query: string) =>
        ({
          matches: query.includes("prefers-reduced-motion"),
          media: query,
          addEventListener: () => {},
          removeEventListener: () => {},
        }) as unknown as MediaQueryList,
    );
    const { term } = await mount();
    expect(term.options.cursorBlink).toBe(false);
  });

  it("hands the panel an API that writes, focuses and opens the search", async () => {
    const { term } = await mount();
    const api = props.register.mock.calls[0]?.[0] as TerminalApi;
    api.write(new Uint8Array([104, 105]));
    expect(term.written).toHaveLength(1);
    expect(term.focused).toBeGreaterThan(0);
    act(() => api.openSearch());
    expect(await screen.findByRole("textbox", { name: "Search the terminal" })).toBeTruthy();
  });

  it("sends typed text and binary reports as bytes", async () => {
    const { term } = await mount();
    term.dataCb?.("ls\r");
    term.binaryCb?.("\x1b\xff");
    expect(Array.from(props.onInput.mock.calls[0]?.[0] as Uint8Array)).toEqual([108, 115, 13]);
    expect(Array.from(props.onInput.mock.calls[1]?.[0] as Uint8Array)).toEqual([27, 255]);
  });

  it("stops taking input once the session has ended", async () => {
    const { term, rerender } = await mount();
    expect(term.options.disableStdin).toBe(false);
    rerender({ readOnly: true });
    expect(term.options.disableStdin).toBe(true);
  });
});

describe("the renderer (D-33)", () => {
  it("uses WebGL when it starts", async () => {
    await mount();
    expect(x.state.webgls).toHaveLength(1);
    expect((x.state.webgls[0] as InstanceType<typeof x.FakeWebgl>).disposed).toBe(false);
  });

  it("goes on with xterm's DOM renderer, saying nothing, when WebGL cannot start", async () => {
    x.state.webglFails = true;
    const { term } = await mount();
    expect(x.state.webgls).toHaveLength(0);
    expect(term.addons).toHaveLength(2);
    expect(toast.toastError).not.toHaveBeenCalled();
    expect(screen.queryByRole("alert")).toBeNull();
    // It still works.
    const api = props.register.mock.calls[0]?.[0] as TerminalApi;
    api.write(new Uint8Array([1]));
    expect(term.written).toHaveLength(1);
    expect(term.disposed).toBe(false);
  });

  it("disposes WebGL when it loses its context, and the terminal goes on", async () => {
    const { term } = await mount();
    const gl = x.state.webgls[0] as InstanceType<typeof x.FakeWebgl>;
    gl.lost?.();
    expect(gl.disposed).toBe(true);
    expect(term.disposed).toBe(false);
    expect(toast.toastError).not.toHaveBeenCalled();
    const api = props.register.mock.calls[0]?.[0] as TerminalApi;
    api.write(new Uint8Array([2]));
    expect(term.written).toHaveLength(1);
  });
});

describe("the theme (D-33)", () => {
  it("follows the app's theme live, without a new terminal", async () => {
    const { term } = await mount();
    const light = term.options.theme as { background: string; foreground: string };
    act(() => document.documentElement.classList.add("dark"));
    await waitFor(() =>
      expect((term.options.theme as { background: string }).background).not.toBe(light.background),
    );
    expect((term.options.theme as { foreground: string }).foreground).not.toBe(light.foreground);
    act(() => document.documentElement.classList.remove("dark"));
    await waitFor(() =>
      expect((term.options.theme as { background: string }).background).toBe(light.background),
    );
    expect(x.state.terms).toHaveLength(1);
  });

  it("paints the box behind the terminal in the theme's background", async () => {
    await mount();
    const box = screen.getByLabelText("Terminal in web");
    expect(box.getAttribute("style")).toContain("background-color");
  });
});

describe("font size and screen reader support (D-33, UI-SPEC accessibility)", () => {
  it("refits and sends the new size once when the font size changes", async () => {
    const { term, rerender } = await mount();
    expect(props.onResize).toHaveBeenCalledTimes(1);
    expect(props.onResize).toHaveBeenLastCalledWith(80, 24);
    const fits = x.state.fits;
    x.state.fitSize = { cols: 96, rows: 28 };
    rerender({ fontSize: 12 });
    expect(term.options.fontSize).toBe(12);
    expect(x.state.fits).toBe(fits + 1);
    expect(props.onResize).toHaveBeenCalledTimes(2);
    expect(props.onResize).toHaveBeenLastCalledWith(96, 28);
  });

  it("turns screen reader mode on and off", async () => {
    const { term, rerender } = await mount();
    rerender({ screenReader: true });
    expect(term.options.screenReaderMode).toBe(true);
    rerender({ screenReader: false });
    expect(term.options.screenReaderMode).toBe(false);
  });
});

describe("copy, paste and the escape chord (D-33)", () => {
  it("Ctrl+Shift+C copies the selection through the fallback chain and keeps it from the shell", async () => {
    const { term } = await mount();
    x.state.selection = "echo hello";
    const r = key(term, { key: "C", ctrlKey: true, shiftKey: true });
    expect(r).toEqual({ passed: false, prevented: true });
    await waitFor(() => expect(clip.copyText).toHaveBeenCalledTimes(1));
    expect(clip.copyText.mock.calls[0]?.[0]).toBe("echo hello");
    expect(toast.toastError).not.toHaveBeenCalled();
  });

  it("Cmd+Shift+C copies as well", async () => {
    const { term } = await mount();
    x.state.selection = "x";
    key(term, { key: "C", metaKey: true, shiftKey: true });
    await waitFor(() => expect(clip.copyText).toHaveBeenCalledTimes(1));
  });

  it("says 'Copy failed. Select the text and copy it manually.' when nothing could be copied", async () => {
    clip.copyText.mockResolvedValue(false);
    const { term } = await mount();
    x.state.selection = "secret";
    key(term, { key: "C", ctrlKey: true, shiftKey: true });
    await waitFor(() =>
      expect(toast.toastError).toHaveBeenCalledWith(
        "Copy failed. Select the text and copy it manually.",
      ),
    );
  });

  it("copies nothing when nothing is selected, and never copies on select", async () => {
    const { term } = await mount();
    key(term, { key: "C", ctrlKey: true, shiftKey: true });
    // A selection made with the mouse is not copied by itself.
    x.state.selection = "dragged over";
    await act(async () => {});
    expect(clip.copyText).not.toHaveBeenCalled();
  });

  it("Ctrl+Shift+V is let through for the browser's own paste event", async () => {
    const { term } = await mount();
    const r = key(term, { key: "V", ctrlKey: true, shiftKey: true });
    expect(r).toEqual({ passed: false, prevented: false });
    expect(props.onInput).not.toHaveBeenCalled();
  });

  it("Ctrl+Shift+Home hands focus to the toolbar and keeps the key from the shell", async () => {
    const { term } = await mount();
    const r = key(term, { key: "Home", ctrlKey: true, shiftKey: true });
    expect(r).toEqual({ passed: false, prevented: true });
    expect(props.onLeave).toHaveBeenCalledTimes(1);
  });

  it("gives every other key to the shell, Tab, Esc, Ctrl+C and a plain Home included", async () => {
    const { term } = await mount();
    for (const init of [
      { key: "Tab" },
      { key: "Escape" },
      { key: "c", ctrlKey: true },
      { key: "v", ctrlKey: true },
      { key: "f", ctrlKey: true },
      { key: "Home" },
      { key: "Home", ctrlKey: true },
      { key: "C", shiftKey: true },
      { key: "C", ctrlKey: true, shiftKey: true, altKey: true },
      { key: "a" },
    ]) {
      expect(key(term, init), JSON.stringify(init)).toEqual({ passed: true, prevented: false });
    }
    expect(props.onLeave).not.toHaveBeenCalled();
    expect(clip.copyText).not.toHaveBeenCalled();
  });

  it("looks only at key presses, not at releases", async () => {
    const { term } = await mount();
    const ev = new KeyboardEvent("keyup", { key: "C", ctrlKey: true, shiftKey: true });
    expect(term.keyHandler?.(ev)).toBe(true);
  });
});

describe("search (D-33)", () => {
  async function openSearch() {
    const m = await mount();
    key(m.term, { key: "F", ctrlKey: true, shiftKey: true });
    const input = await screen.findByRole("textbox", { name: "Search the terminal" });
    return { ...m, input, finder: x.state.searches[0] as InstanceType<typeof x.FakeSearch> };
  }

  it("Ctrl+Shift+F opens the search bar and puts the cursor in it", async () => {
    const { input, term } = await openSearch();
    await waitFor(() => expect(document.activeElement).toBe(input));
    expect(term.keyHandler).toBeTruthy();
  });

  it("finds as the person types, Enter goes to the next match and Shift+Enter to the previous", async () => {
    const { input, finder } = await openSearch();
    fireEvent.change(input, { target: { value: "error" } });
    expect(finder.calls).toEqual(["next:error"]);
    fireEvent.keyDown(input, { key: "Enter" });
    fireEvent.keyDown(input, { key: "Enter", shiftKey: true });
    expect(finder.calls).toEqual(["next:error", "next:error", "prev:error"]);
  });

  it("has Previous and Next buttons", async () => {
    const { input, finder } = await openSearch();
    fireEvent.change(input, { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: "Next match" }));
    fireEvent.click(screen.getByRole("button", { name: "Previous match" }));
    expect(finder.calls.slice(1)).toEqual(["next:x", "prev:x"]);
  });

  it("says so when nothing matches", async () => {
    const { input, finder } = await openSearch();
    finder.result = false;
    fireEvent.change(input, { target: { value: "zzz" } });
    expect(await screen.findByText("No matches")).toBeTruthy();
    expect(input.getAttribute("aria-invalid")).toBe("true");
  });

  it("Esc closes the bar, clears the highlights and returns focus to the terminal", async () => {
    const { input, term, finder } = await openSearch();
    fireEvent.change(input, { target: { value: "a" } });
    const focused = term.focused;
    fireEvent.keyDown(input, { key: "Escape" });
    expect(screen.queryByRole("textbox", { name: "Search the terminal" })).toBeNull();
    expect(finder.cleared).toBeGreaterThan(0);
    expect(term.focused).toBe(focused + 1);
  });

  it("clearing the box clears the highlights", async () => {
    const { input, finder } = await openSearch();
    fireEvent.change(input, { target: { value: "a" } });
    const cleared = finder.cleared;
    fireEvent.change(input, { target: { value: "" } });
    expect(finder.cleared).toBe(cleared + 1);
  });
});

describe("resizing (D-33)", () => {
  it("fits and sends the size once, 100 ms after the window stops changing", async () => {
    const { term } = await mount();
    expect(term).toBeTruthy();
    vi.useFakeTimers();
    props.onResize.mockClear();
    const fits = x.state.fits;
    x.state.fitSize = { cols: 120, rows: 40 };
    for (let i = 0; i < 5; i++) {
      window.dispatchEvent(new Event("resize"));
      vi.advanceTimersByTime(40);
    }
    expect(props.onResize).not.toHaveBeenCalled();
    expect(x.state.fits).toBe(fits);
    vi.advanceTimersByTime(100);
    expect(x.state.fits).toBe(fits + 1);
    expect(props.onResize).toHaveBeenCalledTimes(1);
    expect(props.onResize).toHaveBeenCalledWith(120, 40);
  });

  it("sends nothing when the box changed but the grid did not", async () => {
    await mount();
    vi.useFakeTimers();
    props.onResize.mockClear();
    window.dispatchEvent(new Event("resize"));
    vi.advanceTimersByTime(150);
    expect(props.onResize).not.toHaveBeenCalled();
  });
});

describe("what the terminal keeps (T-04-93)", () => {
  it("writes nothing it shows or is typed into the page's storage", async () => {
    const set = vi.spyOn(Storage.prototype, "setItem");
    const { term } = await mount();
    const api = props.register.mock.calls[0]?.[0] as TerminalApi;
    api.write(new TextEncoder().encode("password: hunter2"));
    term.dataCb?.("hunter2\r");
    x.state.selection = "hunter2";
    key(term, { key: "C", ctrlKey: true, shiftKey: true });
    expect(set).not.toHaveBeenCalled();
    expect(Object.keys(localStorage)).toEqual([]);
    expect(Object.keys(sessionStorage)).toEqual([]);
  });
});
