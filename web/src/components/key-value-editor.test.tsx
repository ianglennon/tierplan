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

// A row whose stored value the server withheld (D-09): the editor is never given it.
const maskedLabels: KeyValueEditorLabels = {
  ...labels,
  maskedPlaceholder: "Hidden. Type to replace.",
  reset: (key) => `Reset ${key}`,
};

function MaskedHost({ onReset }: { onReset?: (row: KeyValueRow) => void }) {
  const [rows, setRows] = useState<KeyValueRow[]>([
    { ...newRow("TOKEN", ""), masked: true },
    newRow("MODE", "prod"),
  ]);
  latest = rows;
  return <KeyValueEditor value={rows} onChange={setRows} labels={maskedLabels} onReset={onReset} />;
}

const tokenValue = () => screen.getAllByLabelText("Label value")[0] as HTMLInputElement;

describe("KeyValueEditor masked rows", () => {
  it("shows the placeholder with no value in the page, and a name that can't be changed", () => {
    render(<MaskedHost />);
    expect(tokenValue().value).toBe("");
    expect(tokenValue().placeholder).toBe("Hidden. Type to replace.");
    expect((screen.getAllByLabelText("Label name")[0] as HTMLInputElement).readOnly).toBe(true);
    expect((screen.getAllByLabelText("Label name")[1] as HTMLInputElement).readOnly).toBe(false);
    expect(screen.queryByRole("button", { name: "Reset TOKEN" })).toBeNull();
    expect(document.body.innerHTML).not.toContain("&lt;hidden&gt;");
  });

  it("gives an ordinary row no placeholder", () => {
    render(<MaskedHost />);
    expect((screen.getAllByLabelText("Label value")[1] as HTMLInputElement).placeholder).toBe("");
  });

  it("shows Reset once a value is typed over it, and Reset puts the hidden row back", () => {
    const reset: KeyValueRow[] = [];
    render(<MaskedHost onReset={(row) => reset.push(row)} />);
    fireEvent.change(tokenValue(), { target: { value: "fresh" } });
    expect(tokenValue().value).toBe("fresh");
    expect(latest[0]?.masked).toBe(true);
    expect(latest[0]?.value).toBe("fresh");
    fireEvent.click(screen.getByRole("button", { name: "Reset TOKEN" }));
    expect(tokenValue().value).toBe("");
    expect(tokenValue().placeholder).toBe("Hidden. Type to replace.");
    expect(latest[0]?.value).toBe("");
    expect(screen.queryByRole("button", { name: "Reset TOKEN" })).toBeNull();
    expect(reset.map((r) => r.key)).toEqual(["TOKEN"]);
  });

  it("removes a masked row like any other", () => {
    render(<MaskedHost />);
    fireEvent.click(screen.getByRole("button", { name: "Remove label TOKEN" }));
    expect(latest.map((r) => r.key)).toEqual(["MODE"]);
  });
});

describe("entriesOf", () => {
  it("leaves out a row with no name, trims names, and keeps the last value of a repeated name", () => {
    expect(
      entriesOf([newRow("", "orphan"), newRow(" a ", "1"), newRow("a", "2"), newRow("b", "")]),
    ).toEqual({ a: "2", b: "" });
  });
});
