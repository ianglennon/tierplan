// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { MatchText } from "@/features/search/match-text";

afterEach(cleanup);

function parts(text: string, q: string) {
  const { container } = render(<MatchText text={text} q={q} />);
  return {
    container,
    marks: Array.from(container.querySelectorAll("mark")).map((m) => m.textContent),
    text: container.textContent,
  };
}

describe("MatchText", () => {
  it("wraps the first case-insensitive match in a mark and keeps the rest as text", () => {
    const { container, marks } = parts("My-Web-App", "web");
    expect(marks).toEqual(["Web"]);
    expect(container.textContent).toBe("My-Web-App");
    const nodes = Array.from(container.childNodes).map((n) => n.textContent);
    expect(nodes).toEqual(["My-", "Web", "-App"]);
    expect(container.querySelector("mark")?.className).toContain("font-semibold");
  });

  it("is the text alone when nothing matches or nothing was asked", () => {
    expect(parts("alpha", "web").marks).toEqual([]);
    expect(parts("alpha", "   ").marks).toEqual([]);
    expect(parts("alpha", "web").container.querySelector("mark")).toBeNull();
  });

  it("matches the characters typed and not a pattern", () => {
    expect(parts("axb a.b", "a.b").marks).toEqual(["a.b"]);
    expect(parts("f(x)", "(").marks).toEqual(["("]);
    expect(parts("a+b", "a+").marks).toEqual(["a+"]);
  });

  it("keeps markup in the text literal", () => {
    const { container, marks } = parts("<b>x</b>", "x");
    expect(container.querySelector("b")).toBeNull();
    expect(container.textContent).toBe("<b>x</b>");
    expect(marks).toEqual(["x"]);
    const whole = parts("<b>x</b>", "<b>");
    expect(whole.marks).toEqual(["<b>"]);
    expect(whole.container.querySelector("b")).toBeNull();
  });

  it("ignores surrounding spaces in the query", () => {
    expect(parts("web-app", "  web ").marks).toEqual(["web"]);
  });
});
