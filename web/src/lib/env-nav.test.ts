// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { afterEach, describe, expect, it } from "vitest";

import { resetBasePathForTests } from "@/lib/base-path";
import { ENV_SECTIONS, isMacPlatform, shortcutLabel, switchTarget } from "@/lib/env-nav";

describe("switchTarget (D-35)", () => {
  it("keeps the section when the target has it", () => {
    expect(switchTarget("/env/a/containers", "b")).toBe("/env/b/containers");
    expect(switchTarget("/env/a/images", "b")).toBe("/env/b/images");
    expect(switchTarget("/env/a/volumes", "b")).toBe("/env/b/volumes");
    expect(switchTarget("/env/a/networks", "b")).toBe("/env/b/networks");
  });

  it("goes to the dashboard from the dashboard", () => {
    expect(switchTarget("/env/a", "b")).toBe("/env/b");
    expect(switchTarget("/env/a/", "b")).toBe("/env/b");
  });

  it("goes to the dashboard from outside an environment", () => {
    expect(switchTarget("/settings/general", "b")).toBe("/env/b");
    expect(switchTarget("/environments", "b")).toBe("/env/b");
    expect(switchTarget("/audit", "b")).toBe("/env/b");
    expect(switchTarget("/", "b")).toBe("/env/b");
  });

  it("drops a section the target does not have", () => {
    expect(switchTarget("/env/a/unknown-section", "b")).toBe("/env/b");
  });

  it("keeps the section but not what is below it", () => {
    expect(switchTarget("/env/a/containers/abc123", "b")).toBe("/env/b/containers");
  });

  it("encodes an ID that is not path-safe", () => {
    expect(switchTarget("/env/a/containers", "b/c d")).toBe("/env/b%2Fc%20d/containers");
  });

  it("lists the dashboard and the four resource sections", () => {
    expect([...ENV_SECTIONS]).toEqual(["", "containers", "images", "volumes", "networks"]);
  });
});

describe("shortcut label", () => {
  const originalUA = navigator.userAgent;
  function setUA(ua: string) {
    Object.defineProperty(navigator, "userAgent", { value: ua, configurable: true });
  }
  afterEach(() => {
    setUA(originalUA);
    resetBasePathForTests();
  });

  it("is the command key on macOS and Ctrl K elsewhere", () => {
    setUA("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15");
    expect(isMacPlatform()).toBe(true);
    expect(shortcutLabel()).toBe("⌘K");
    setUA("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/150.0");
    expect(isMacPlatform()).toBe(false);
    expect(shortcutLabel()).toBe("Ctrl K");
  });
});
