// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { StagedKind, StagedObject } from "@/features/service-editor/staged";
import { StagedObjectDialog } from "@/features/service-editor/staged-object-dialog";
import "@/i18n";

// The staged object dialog (plan 06-26, D-06; UI-SPEC "Service editor" Staged dialogs): a secret,
// config or network that is made when the service is applied, validated against the objects that
// exist and the ones already staged.

let onAdd: ReturnType<typeof vi.fn<(object: StagedObject) => void>>;
let onOpenChange: ReturnType<typeof vi.fn<(open: boolean) => void>>;

beforeEach(() => {
  onAdd = vi.fn<(object: StagedObject) => void>();
  onOpenChange = vi.fn<(open: boolean) => void>();
});

afterEach(() => cleanup());

function renderDialog(kind: StagedKind, taken: string[] = []) {
  return render(
    <StagedObjectDialog kind={kind} open onOpenChange={onOpenChange} taken={taken} onAdd={onAdd} />,
  );
}

const name = () => screen.getByLabelText("Name");
const type = (el: HTMLElement, value: string) => fireEvent.change(el, { target: { value } });

describe("StagedObjectDialog: a secret", () => {
  it("says what it does and that Docker never shows the value again", () => {
    renderDialog("secret");
    expect(screen.getByRole("dialog", { name: "Create a secret" })).toBeTruthy();
    expect(
      screen.getByText(
        "It's created when you apply the service. Docker never shows the value again.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Add secret" })).toBeTruthy();
  });

  it("needs a valid name and a value, and says which is missing", () => {
    renderDialog("secret");
    fireEvent.click(screen.getByRole("button", { name: "Add secret" }));
    expect(screen.getByText("Enter a name.")).toBeTruthy();
    expect(screen.getByText("Enter a value.")).toBeTruthy();
    expect(onAdd).not.toHaveBeenCalled();
    type(name(), "-bad");
    fireEvent.click(screen.getByRole("button", { name: "Add secret" }));
    expect(screen.getByText("Use letters, numbers, dot, dash or underscore.")).toBeTruthy();
  });

  it("refuses a name that exists or is staged already", () => {
    renderDialog("secret", ["db_pw"]);
    type(name(), "db_pw");
    type(screen.getByLabelText("Value"), "x");
    fireEvent.click(screen.getByRole("button", { name: "Add secret" }));
    expect(screen.getByText("A secret named db_pw already exists.")).toBeTruthy();
    expect(onAdd).not.toHaveBeenCalled();
  });

  it("hands over the staged secret with its value and closes", () => {
    renderDialog("secret");
    type(name(), " fresh ");
    type(screen.getByLabelText("Value"), "hunter2");
    fireEvent.click(screen.getByRole("button", { name: "Add secret" }));
    expect(onAdd).toHaveBeenCalledTimes(1);
    expect(onAdd.mock.calls[0]?.[0]).toMatchObject({
      kind: "secret",
      name: "fresh",
      value: { source: "type", text: "hunter2" },
    });
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("keeps the value in a masked field that no browser fills in", () => {
    renderDialog("secret");
    const value = screen.getByLabelText("Value");
    expect(value.getAttribute("type")).toBe("password");
    expect(value.getAttribute("autocomplete")).toBe("new-password");
  });

  it("forgets what was typed when it is closed and opened again", () => {
    const { rerender } = renderDialog("secret");
    type(name(), "fresh");
    type(screen.getByLabelText("Value"), "hunter2");
    rerender(
      <StagedObjectDialog
        kind="secret"
        open={false}
        onOpenChange={onOpenChange}
        taken={[]}
        onAdd={onAdd}
      />,
    );
    rerender(
      <StagedObjectDialog
        kind="secret"
        open
        onOpenChange={onOpenChange}
        taken={[]}
        onAdd={onAdd}
      />,
    );
    expect((name() as HTMLInputElement).value).toBe("");
    expect((screen.getByLabelText("Value") as HTMLInputElement).value).toBe("");
  });

  it("closes on Cancel without handing anything over", () => {
    renderDialog("secret");
    type(name(), "fresh");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onAdd).not.toHaveBeenCalled();
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });
});

describe("StagedObjectDialog: a config", () => {
  it("has content and an optional templating, and hands over the choice", () => {
    renderDialog("config");
    expect(screen.getByRole("dialog", { name: "Create a config" })).toBeTruthy();
    expect(screen.getByText("It's created when you apply the service.")).toBeTruthy();
    type(name(), "app.conf");
    type(screen.getByLabelText("Content"), "level=debug");
    expect(screen.getByLabelText("Templating (optional)")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Add config" }));
    expect(onAdd.mock.calls[0]?.[0]).toMatchObject({
      kind: "config",
      name: "app.conf",
      templating: "none",
      value: { text: "level=debug" },
    });
  });

  it("needs content", () => {
    renderDialog("config");
    type(name(), "app.conf");
    fireEvent.click(screen.getByRole("button", { name: "Add config" }));
    expect(screen.getByText("Enter a value.")).toBeTruthy();
    expect(onAdd).not.toHaveBeenCalled();
  });
});

describe("StagedObjectDialog: a network", () => {
  it("is made as an attachable overlay, with Driver read-only and an optional Subnet under Advanced", async () => {
    renderDialog("network");
    expect(screen.getByRole("dialog", { name: "Create a network" })).toBeTruthy();
    expect(
      screen.getByText(
        "It's created when you apply the service, as an attachable overlay network.",
      ),
    ).toBeTruthy();
    expect(screen.queryByLabelText("Subnet (optional)")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
    const dialog = screen.getByRole("dialog");
    const driver = await within(dialog).findByLabelText("Driver");
    expect((driver as HTMLInputElement).value).toBe("overlay");
    expect((driver as HTMLInputElement).readOnly).toBe(true);
    type(within(dialog).getByLabelText("Subnet (optional)"), "10.9.0.0/24");
    type(name(), "front");
    fireEvent.click(screen.getByRole("button", { name: "Add network" }));
    expect(onAdd.mock.calls[0]?.[0]).toMatchObject({
      kind: "network",
      name: "front",
      subnet: "10.9.0.0/24",
    });
  });

  it("needs only a name, and says so when the subnet is not a subnet", async () => {
    renderDialog("network");
    fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
    type(await screen.findByLabelText("Subnet (optional)"), "10.9.0.1");
    type(name(), "front");
    fireEvent.click(screen.getByRole("button", { name: "Add network" }));
    expect(screen.getByText("The subnet must be in CIDR form, like 10.20.0.0/24.")).toBeTruthy();
    expect(onAdd).not.toHaveBeenCalled();
    type(screen.getByLabelText("Subnet (optional)"), "");
    fireEvent.click(screen.getByRole("button", { name: "Add network" }));
    await waitFor(() => expect(onAdd).toHaveBeenCalledTimes(1));
    expect(onAdd.mock.calls[0]?.[0]).toMatchObject({ kind: "network", name: "front" });
  });
});
