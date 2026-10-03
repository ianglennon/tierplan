// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { afterEach, describe, expect, it } from "vitest";

import { apiUrl, basePath, resetBasePathForTests, stripBase, withBase } from "@/lib/base-path";

// The server writes this tag into the index.html it serves (plan 02-03).
function setBaseMeta(content: string | null): void {
  for (const el of document.head.querySelectorAll('meta[name="bayplan-base"]')) el.remove();
  resetBasePathForTests();
  if (content === null) return;
  const meta = document.createElement("meta");
  meta.name = "bayplan-base";
  meta.content = content;
  document.head.append(meta);
}

afterEach(() => {
  setBaseMeta(null);
});

describe("basePath", () => {
  it("is the meta content when the server announces a base", () => {
    setBaseMeta("/bayplan");
    expect(basePath()).toBe("/bayplan");
  });

  it("is empty without the meta tag (Vite dev server, tests)", () => {
    setBaseMeta(null);
    expect(basePath()).toBe("");
  });

  it("is empty for an empty meta content", () => {
    setBaseMeta("");
    expect(basePath()).toBe("");
  });

  it("strips one trailing slash", () => {
    setBaseMeta("/bayplan/");
    expect(basePath()).toBe("/bayplan");
  });

  it("treats a lone slash as no base", () => {
    setBaseMeta("/");
    expect(basePath()).toBe("");
  });

  it("is read once and then remembered", () => {
    setBaseMeta("/bayplan");
    expect(basePath()).toBe("/bayplan");
    document.head.querySelector('meta[name="bayplan-base"]')?.setAttribute("content", "/other");
    expect(basePath()).toBe("/bayplan");
  });
});

describe("withBase", () => {
  it("prefixes the base", () => {
    setBaseMeta("/bayplan");
    expect(withBase("/env/a")).toBe("/bayplan/env/a");
    expect(withBase("/")).toBe("/bayplan/");
  });

  it("leaves the path alone under no base", () => {
    setBaseMeta(null);
    expect(withBase("/env/a")).toBe("/env/a");
  });
});

describe("stripBase", () => {
  it("removes the base from a path under it", () => {
    setBaseMeta("/bayplan");
    expect(stripBase("/bayplan/env/a")).toBe("/env/a");
  });

  it("maps the base itself to the root", () => {
    setBaseMeta("/bayplan");
    expect(stripBase("/bayplan")).toBe("/");
    expect(stripBase("/bayplan/")).toBe("/");
  });

  it("leaves a path that only shares the prefix unchanged", () => {
    setBaseMeta("/bayplan");
    expect(stripBase("/bayplanx")).toBe("/bayplanx");
    expect(stripBase("/bayplanx/env")).toBe("/bayplanx/env");
  });

  it("leaves a path outside the base unchanged", () => {
    setBaseMeta("/bayplan");
    expect(stripBase("/env/a")).toBe("/env/a");
  });

  it("is the identity under no base", () => {
    setBaseMeta(null);
    expect(stripBase("/env/a")).toBe("/env/a");
  });
});

describe("apiUrl", () => {
  it("prefixes API and stream URLs with the base", () => {
    setBaseMeta("/bayplan");
    expect(apiUrl("/api/env/a/events/stream")).toBe("/bayplan/api/env/a/events/stream");
    expect(apiUrl("/api/audit/export?format=csv")).toBe("/bayplan/api/audit/export?format=csv");
  });

  it("is the identity under no base", () => {
    setBaseMeta(null);
    expect(apiUrl("/api/env/a/events/stream")).toBe("/api/env/a/events/stream");
  });
});
