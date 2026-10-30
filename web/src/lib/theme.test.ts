// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { afterEach, describe, expect, it, vi } from "vitest";

import { getTheme, setTheme, watchSystemTheme } from "@/lib/theme";

// stubSystem fakes the operating system's colour scheme. `flip` changes it and
// tells every listener, as a real change would.
function stubSystem(initialDark: boolean) {
  let dark = initialDark;
  const listeners = new Set<() => void>();
  const stub = (query: string) => ({
    get matches() {
      return dark && query.includes("dark");
    },
    media: query,
    addEventListener: (_: string, fn: () => void) => listeners.add(fn),
    removeEventListener: (_: string, fn: () => void) => listeners.delete(fn),
  });
  vi.stubGlobal("matchMedia", stub);
  window.matchMedia = stub as unknown as typeof window.matchMedia;
  return {
    flip(next: boolean) {
      dark = next;
      for (const fn of listeners) fn();
    },
  };
}

const isDark = () => document.documentElement.classList.contains("dark");

afterEach(() => {
  vi.unstubAllGlobals();
  window.localStorage.clear();
  document.documentElement.classList.remove("dark");
});

describe("theme", () => {
  it("defaults to system", () => {
    expect(getTheme()).toBe("system");
  });

  it("remembers the choice in localStorage under coaming-theme", () => {
    stubSystem(false);
    setTheme("dark");
    expect(window.localStorage.getItem("coaming-theme")).toBe("dark");
    expect(getTheme()).toBe("dark");
  });

  it("ignores a stored value that is not a mode", () => {
    window.localStorage.setItem("coaming-theme", "purple");
    expect(getTheme()).toBe("system");
  });

  it("toggles the dark class for light and dark whatever the system says", () => {
    stubSystem(true);
    setTheme("dark");
    expect(isDark()).toBe(true);
    setTheme("light");
    expect(isDark()).toBe(false);
  });

  it("follows the system in system mode", () => {
    stubSystem(true);
    setTheme("system");
    expect(isDark()).toBe(true);
  });

  it("re-applies when the system changes in system mode, but not in an explicit mode", () => {
    const sys = stubSystem(false);
    setTheme("system");
    const stop = watchSystemTheme();
    sys.flip(true);
    expect(isDark()).toBe(true);
    setTheme("light");
    sys.flip(false);
    sys.flip(true);
    expect(isDark()).toBe(false);
    stop();
  });

  it("stops watching when asked", () => {
    const sys = stubSystem(false);
    setTheme("system");
    const stop = watchSystemTheme();
    stop();
    sys.flip(true);
    expect(isDark()).toBe(false);
  });
});
