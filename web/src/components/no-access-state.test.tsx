// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { NoAccessState } from "@/components/no-access-state";
import "@/i18n";

afterEach(cleanup);

describe("NoAccessState", () => {
  it("says there is no access and who to ask", () => {
    render(<NoAccessState administrators={["admin", "ops-lead"]} />);
    expect(
      screen.getByRole("heading", { name: "You don't have access to any environments yet" }),
    ).toBeTruthy();
    expect(screen.getByText("Ask an administrator to give you access.")).toBeTruthy();
    expect(screen.getByText("Administrators: admin, ops-lead")).toBeTruthy();
  });

  it("lists the first five administrators and counts the rest", () => {
    const names = ["a1", "a2", "a3", "a4", "a5", "a6", "a7"];
    render(<NoAccessState administrators={names} />);
    expect(screen.getByText("Administrators: a1, a2, a3, a4, a5 and 2 more")).toBeTruthy();
    expect(screen.queryByText(/a6/)).toBeNull();
  });

  it("counts a single remaining administrator", () => {
    render(<NoAccessState administrators={["a1", "a2", "a3", "a4", "a5", "a6"]} />);
    expect(screen.getByText("Administrators: a1, a2, a3, a4, a5 and 1 more")).toBeTruthy();
  });

  it("omits the line when the server names none", () => {
    render(<NoAccessState administrators={[]} />);
    expect(screen.queryByText(/Administrators/)).toBeNull();
    render(<NoAccessState administrators={null} />);
    expect(screen.queryByText(/Administrators/)).toBeNull();
  });

  it("renders usernames as plain text, never as markup", () => {
    render(<NoAccessState administrators={["<b>x</b>"]} />);
    expect(screen.getByText("Administrators: <b>x</b>")).toBeTruthy();
    expect(document.querySelector("b")).toBeNull();
  });
});
