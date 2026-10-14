// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it } from "vitest";

import {
  entriesOf,
  KeyValueEditor,
  type KeyValueEditorLabels,
  type KeyValueRow,
  newRow,
} from "@/components/key-value-editor";

afterEach(cleanup);

const labels: KeyValueEditorLabels = {
  add: "Add label",
  key: "Label name",
  value: "Label value",
  remove: (key) => (key ? `Remove label ${key}` : "Remove label"),
};

let latest: KeyValueRow[] = [];

function Host({ initial = [] as KeyValueRow[] }) {
  const [rows, setRows] = useState<KeyValueRow[]>(initial);
  latest = rows;
  return <KeyValueEditor value={rows} onChange={setRows} labels={labels} />;
}

describe("KeyValueEditor", () => {
  it("starts with no rows and one button that adds an empty row", () => {
    render(<Host />);
    expect(screen.queryAllByLabelText("Label name")).toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Add label" }));
    expect(screen.getAllByLabelText("Label name")).toHaveLength(1);
    expect(screen.getAllByLabelText("Label value")).toHaveLength(1);
  });

  it("keeps what is typed in the row it was typed in and gives every row its own remove button", () => {
    render(<Host initial={[newRow("team", "a"), newRow("tier", "web")]} />);
    const keys = screen.getAllByLabelText("Label name") as HTMLInputElement[];
    expect(keys.map((k) => k.value)).toEqual(["team", "tier"]);
    fireEvent.change(keys[1] as HTMLInputElement, { target: { value: "stage" } });
    expect(entriesOf(latest)).toEqual({ team: "a", stage: "web" });
    expect(screen.getByRole("button", { name: "Remove label team" })).toBeTruthy();
  });

  it("removes only the row whose button was pressed", () => {
    render(<Host initial={[newRow("team", "a"), newRow("tier", "web")]} />);
    fireEvent.click(screen.getByRole("button", { name: "Remove label team" }));
    expect(entriesOf(latest)).toEqual({ tier: "web" });
    expect(screen.getAllByLabelText("Label name")).toHaveLength(1);
  });

  it("shows markup as text", () => {
    render(<Host initial={[newRow("<b>x</b>", "<img src=x>")]} />);
    expect((screen.getByLabelText("Label name") as HTMLInputElement).value).toBe("<b>x</b>");
    expect(document.querySelector("img")).toBeNull();
  });
});

describe("KeyValueEditor row errors", () => {
  it("shows the message of a row that has one under it, marks its name invalid and describes it", () => {
    const bad = newRow("com.docker.x", "1");
    const errors = new Map([[bad.id, "Labels starting with com.docker. are reserved."]]);
    render(
      <KeyValueEditor
        value={[newRow("a", "1"), bad]}
        onChange={() => {}}
        labels={labels}
        errors={errors}
      />,
    );
    const names = screen.getAllByLabelText("Label name");
    expect(names[0]?.getAttribute("aria-invalid")).toBeNull();
    expect(names[1]?.getAttribute("aria-invalid")).toBe("true");
    const message = screen.getByText("Labels starting with com.docker. are reserved.");
    expect(names[1]?.getAttribute("aria-describedby")).toBe(message.id);
    expect(message.id).not.toBe("");
  });
});

describe("entriesOf", () => {
  it("leaves out a row with no name, trims names, and keeps the last value of a repeated name", () => {
    expect(
      entriesOf([newRow("", "orphan"), newRow(" a ", "1"), newRow("a", "2"), newRow("b", "")]),
    ).toEqual({ a: "2", b: "" });
  });
});
