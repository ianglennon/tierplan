// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { HIDDEN } from "@/features/spec-editor/diff";
import { SpecDiff } from "@/features/spec-editor/spec-diff";
import "@/i18n";

afterEach(cleanup);

function spec(container: Record<string, unknown>, rest: Record<string, unknown> = {}) {
  return { Name: "api", TaskTemplate: { ContainerSpec: container }, ...rest };
}

const table = () => screen.getByRole("table", { name: "Changes to api" });

describe("SpecDiff", () => {
  it("is a table captioned Changes to {name} with the columns Change, Setting, Current, New", () => {
    render(
      <SpecDiff name="api" current={spec({ Image: "api:1" })} next={spec({ Image: "api:2" })} />,
    );
    const heads = within(table()).getAllByRole("columnheader");
    expect(heads.map((h) => h.textContent)).toEqual(["Change", "Setting", "Current", "New"]);
  });

  it("takes the value column headings it is given", () => {
    render(
      <SpecDiff
        name="api"
        current={spec({ Image: "api:2" })}
        next={spec({ Image: "api:1" })}
        headings={{ current: "Now", next: "After rollback" }}
      />,
    );
    const heads = within(table()).getAllByRole("columnheader");
    expect(heads.map((h) => h.textContent)).toEqual(["Change", "Setting", "Now", "After rollback"]);
  });

  it("writes the change as a glyph and a word, and gives the setting its label and its path", () => {
    render(
      <SpecDiff
        name="api"
        current={spec({ Image: "api:1", Env: ["OLD=1", "KEEP=a"] })}
        next={spec({ Image: "api:2", Env: ["KEEP=b", "NEW=2"] })}
      />,
    );
    const rows = within(table()).getAllByRole("row");
    const body = rows.map((r) => r.textContent ?? "");
    const image = body.find((t) => t.includes("TaskTemplate.ContainerSpec.Image"));
    expect(image).toContain("~Changed");
    expect(image).toContain("Image");
    expect(image).toContain("api:1");
    expect(image).toContain("api:2");
    const added = body.find((t) => t.includes("Env[NEW]"));
    expect(added).toContain("+Added");
    expect(added).toContain("Environment variable NEW");
    const removed = body.find((t) => t.includes("Env[OLD]"));
    expect(removed).toContain("−Removed");
    const path = screen.getByText("TaskTemplate.ContainerSpec.Image");
    expect(path.className).toContain("font-mono");
  });

  it("tints added rows and removed rows, and leaves a changed row plain", () => {
    render(
      <SpecDiff
        name="api"
        current={spec({ Image: "api:1", Env: ["OLD=1"] })}
        next={spec({ Image: "api:2", Env: ["NEW=2"] })}
      />,
    );
    const rowOf = (text: string) =>
      within(table())
        .getAllByRole("row")
        .find((r) => r.textContent?.includes(text));
    expect(rowOf("Env[NEW]")?.className).toContain("bg-status-online/8");
    expect(rowOf("Env[OLD]")?.className).toContain("bg-status-offline/8");
    expect(rowOf("ContainerSpec.Image")?.className).not.toContain("bg-status");
  });

  it("lists the groups under heading rows in the card order", () => {
    render(
      <SpecDiff
        name="api"
        current={spec({ Image: "api:1", Env: ["A=1"] }, { UpdateConfig: { Parallelism: 1 } })}
        next={spec({ Image: "api:2", Env: ["A=2"] }, { UpdateConfig: { Parallelism: 2 } })}
      />,
    );
    const groups = within(table())
      .getAllByRole("rowheader")
      .map((h) => h.textContent);
    expect(groups).toEqual(["Basics", "Environment", "Update policy"]);
  });

  it("prints a withheld value as bullets on both sides and never lists an unchanged one", () => {
    render(
      <SpecDiff
        name="api"
        current={spec({ Env: [`TOKEN=${HIDDEN}`, "MODE=a"] })}
        next={spec({ Env: [`TOKEN=${HIDDEN}`, "MODE=b"] })}
      />,
    );
    expect(within(table()).queryByText(/TOKEN/)).toBeNull();
    expect(table().textContent).not.toContain(HIDDEN);
  });

  it("reads a replaced withheld value as Hidden value replaced and shows neither value", () => {
    render(
      <SpecDiff
        name="api"
        current={spec({ Env: [`TOKEN=${HIDDEN}`] })}
        next={spec({ Env: ["TOKEN=brand-new-secret"] })}
      />,
    );
    const row = within(table())
      .getAllByRole("row")
      .find((r) => r.textContent?.includes("Env[TOKEN]"));
    expect(row?.textContent).toContain("••••");
    expect(row?.textContent).toContain("Hidden value replaced");
    expect(document.body.textContent).not.toContain("brand-new-secret");
    expect(document.body.textContent).not.toContain(HIDDEN);
  });

  it("prints an added withheld variable as bullets in New and nothing in Current", () => {
    render(
      <SpecDiff name="api" current={spec({ Env: [] })} next={spec({ Env: [`TOKEN=${HIDDEN}`] })} />,
    );
    const row = within(table())
      .getAllByRole("row")
      .find((r) => r.textContent?.includes("Env[TOKEN]"));
    expect(row?.textContent).toContain("••••");
    expect(row?.textContent).not.toContain("Hidden value replaced");
  });

  it("says there is nothing to show when the specs are the same", () => {
    render(<SpecDiff name="api" current={spec({ Image: "a:1" })} next={spec({ Image: "a:1" })} />);
    expect(screen.getByText("No changes")).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("wraps long values in mono so nothing is cut off", () => {
    const long = `registry.example.com/${"x".repeat(200)}:1`;
    render(<SpecDiff name="api" current={spec({ Image: "a:1" })} next={spec({ Image: long })} />);
    const cell = screen.getByText(long);
    expect(cell.className).toContain("break-all");
    expect(cell.className).toContain("font-mono");
  });

  it("falls back to the last part of an unknown path for a label", () => {
    render(
      <SpecDiff
        name="api"
        current={spec({ SomeNewSetting: "a" })}
        next={spec({ SomeNewSetting: "b" })}
      />,
    );
    expect(screen.getByText("Some new setting")).toBeTruthy();
    expect(screen.getByText("Other settings")).toBeTruthy();
  });
});

describe("SpecDiff for a service that does not exist yet (06-27)", () => {
  it("has the columns Setting and Value, and lists every setting of the new spec once", () => {
    render(
      <SpecDiff
        name="api"
        create
        current={{}}
        next={spec({ Image: "api:2" }, { Mode: { Replicated: { Replicas: 2 } } })}
      />,
    );
    const heads = within(table()).getAllByRole("columnheader");
    expect(heads.map((h) => h.textContent)).toEqual(["Setting", "Value"]);
    const rows = within(table())
      .getAllByRole("row")
      .map((r) => r.textContent ?? "");
    const image = rows.find((t) => t.includes("TaskTemplate.ContainerSpec.Image"));
    expect(image).toContain("api:2");
    expect(image).not.toContain("Added");
    const replicas = rows.find((t) => t.includes("Mode.Replicated.Replicas"));
    expect(replicas).toContain("2");
  });

  it("shows a value the server withheld as bullets, never as the value", () => {
    render(
      <SpecDiff
        name="api"
        create
        current={{}}
        next={spec({ Image: "api:2", Env: [`DB=${HIDDEN}`] })}
      />,
    );
    expect(within(table()).getByText("••••")).toBeTruthy();
  });
});
