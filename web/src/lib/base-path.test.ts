// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { afterEach, describe, expect, it } from "vitest";

import { apiUrl, basePath, resetBasePathForTests, stripBase, withBase } from "@/lib/base-path";

// The server writes this tag into the index.html it serves (plan 02-03).
function setBaseMeta(content: string | null): void {
  for (const el of document.head.querySelectorAll('meta[name="coaming-base"]')) el.remove();
  resetBasePathForTests();
  if (content === null) return;
  const meta = document.createElement("meta");
  meta.name = "coaming-base";
  meta.content = content;
  document.head.append(meta);
}

afterEach(() => {
  setBaseMeta(null);
});

describe("basePath", () => {
  it("is the meta content when the server announces a base", () => {
    setBaseMeta("/coaming");
    expect(basePath()).toBe("/coaming");
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
    setBaseMeta("/coaming/");
    expect(basePath()).toBe("/coaming");
  });

  it("treats a lone slash as no base", () => {
    setBaseMeta("/");
    expect(basePath()).toBe("");
  });

  it("is read once and then remembered", () => {
    setBaseMeta("/coaming");
    expect(basePath()).toBe("/coaming");
    document.head.querySelector('meta[name="coaming-base"]')?.setAttribute("content", "/other");
    expect(basePath()).toBe("/coaming");
  });
});

describe("withBase", () => {
  it("prefixes the base", () => {
    setBaseMeta("/coaming");
    expect(withBase("/env/a")).toBe("/coaming/env/a");
    expect(withBase("/")).toBe("/coaming/");
  });

  it("leaves the path alone under no base", () => {
    setBaseMeta(null);
    expect(withBase("/env/a")).toBe("/env/a");
  });
});

describe("stripBase", () => {
  it("removes the base from a path under it", () => {
    setBaseMeta("/coaming");
    expect(stripBase("/coaming/env/a")).toBe("/env/a");
  });

  it("maps the base itself to the root", () => {
    setBaseMeta("/coaming");
    expect(stripBase("/coaming")).toBe("/");
    expect(stripBase("/coaming/")).toBe("/");
  });

  it("leaves a path that only shares the prefix unchanged", () => {
    setBaseMeta("/coaming");
    expect(stripBase("/coamingx")).toBe("/coamingx");
    expect(stripBase("/coamingx/env")).toBe("/coamingx/env");
  });

  it("leaves a path outside the base unchanged", () => {
    setBaseMeta("/coaming");
    expect(stripBase("/env/a")).toBe("/env/a");
  });

  it("is the identity under no base", () => {
    setBaseMeta(null);
    expect(stripBase("/env/a")).toBe("/env/a");
  });
});

describe("apiUrl", () => {
  it("prefixes API and stream URLs with the base", () => {
    setBaseMeta("/coaming");
    expect(apiUrl("/api/env/a/events/stream")).toBe("/coaming/api/env/a/events/stream");
    expect(apiUrl("/api/audit/export?format=csv")).toBe("/coaming/api/audit/export?format=csv");
  });

  it("is the identity under no base", () => {
    setBaseMeta(null);
    expect(apiUrl("/api/env/a/events/stream")).toBe("/api/env/a/events/stream");
  });
});
