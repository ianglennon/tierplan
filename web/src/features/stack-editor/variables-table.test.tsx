// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { StackVariable } from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { VariablesTable, type VariablesTableProps } from "@/features/stack-editor/variables-table";
import type { WorkingVar } from "@/features/stack-editor/working-copy";
import "@/i18n";

// The Variables pane (plan 07-10, STK-04, D-01, D-03): a row for every variable the files refer to
// and every one the person added, with the Resolved column the server computes. The table is a
// controlled component: the page owns the working copy.

afterEach(cleanup);

const resolvedVar = (over: Partial<StackVariable> = {}): StackVariable => ({
  name: "WEB_PORT",
  value: "8080",
  secret: false,
  source: "default",
  default: "8080",
  hasDefault: true,
  required: false,
  unused: false,
  places: [{ file: "compose.yaml", line: 12 }],
  ...over,
});

const workingVar = (over: Partial<WorkingVar> = {}): WorkingVar => ({
  name: "WEB_PORT",
  value: "",
  secret: false,
  hidden: false,
  ...over,
});

function mount(over: Partial<VariablesTableProps> = {}) {
  const handlers = {
    onAdd: vi.fn(),
    onSet: vi.fn(),
    onSecret: vi.fn(),
    onRemove: vi.fn(),
  };
  const props: VariablesTableProps = {
    vars: [],
    resolved: [resolvedVar()],
    failed: false,
    readOnly: false,
    ...handlers,
    ...over,
  };
  const view = render(
    <TooltipProvider>
      <VariablesTable {...props} />
    </TooltipProvider>,
  );
  return {
    ...handlers,
    rerender: (next: Partial<VariablesTableProps>) =>
      view.rerender(
        <TooltipProvider>
          <VariablesTable {...props} {...next} />
        </TooltipProvider>,
      ),
  };
}

const row = (name: string) => screen.getByTestId(`variable-row-${name}`);

describe("a variable the compose file refers to", () => {
  it("is listed with its default as the placeholder, its resolved value and where that comes from", () => {
    mount();
    const r = within(row("WEB_PORT"));
    expect(r.getByText("WEB_PORT")).toBeTruthy();
    expect(r.getByText("Used in 1 place")).toBeTruthy();
    const input = r.getByLabelText("Value of WEB_PORT") as HTMLInputElement;
    expect(input.value).toBe("");
    expect(input.placeholder).toBe("8080");
    expect(r.getByText("8080")).toBeTruthy();
    expect(r.getByText("From the compose file's default")).toBeTruthy();
  });

  it("shows the table's value and its source once the check has answered with it", () => {
    const view = mount({ vars: [workingVar({ value: "9090" })] });
    expect((screen.getByLabelText("Value of WEB_PORT") as HTMLInputElement).value).toBe("9090");
    expect(screen.getByText("8080")).toBeTruthy();
    view.rerender({
      vars: [workingVar({ value: "9090" })],
      resolved: [resolvedVar({ value: "9090", source: "table" })],
    });
    expect(within(row("WEB_PORT")).getByText("9090", { selector: "span" })).toBeTruthy();
    expect(screen.getByText("From the table")).toBeTruthy();
  });

  it("sends what is typed for it, without a name field to change", () => {
    const { onSet } = mount();
    fireEvent.change(screen.getByLabelText("Value of WEB_PORT"), { target: { value: "9090" } });
    expect(onSet).toHaveBeenCalledWith("WEB_PORT", "9090");
    expect(screen.queryByLabelText("Variable name")).toBeNull();
  });

  it("is marked Required, in text with an icon, when it has no value and no default", () => {
    mount({
      resolved: [
        resolvedVar({
          name: "DB_PASS",
          value: "",
          source: "unset",
          hasDefault: false,
          default: "",
          required: true,
        }),
      ],
    });
    const r = within(row("DB_PASS"));
    expect(r.getAllByText("Required").length).toBeGreaterThan(0);
    expect(r.getByText("Not set")).toBeTruthy();
    expect((r.getByLabelText("Value of DB_PASS") as HTMLInputElement).placeholder).toBe("Required");
  });

  it("shows an empty resolved value as (empty), muted", () => {
    mount({ resolved: [resolvedVar({ value: "", default: "" })] });
    expect(screen.getByText("(empty)")).toBeTruthy();
  });

  it("offers Clear value when the table holds a value, and removes the entry", () => {
    const { onRemove } = mount({ vars: [workingVar({ value: "9090" })] });
    fireEvent.click(screen.getByRole("button", { name: "Clear value of WEB_PORT" }));
    expect(onRemove).toHaveBeenCalledWith("WEB_PORT");
    expect(screen.queryByRole("button", { name: "Remove WEB_PORT" })).toBeNull();
  });
});

describe("a variable only the table holds", () => {
  it("carries Unused and can be removed", () => {
    const { onRemove } = mount({
      vars: [workingVar({ name: "OLD", value: "x" })],
      resolved: [
        resolvedVar(),
        resolvedVar({
          name: "OLD",
          value: "x",
          source: "table",
          unused: true,
          places: [],
          hasDefault: false,
          default: "",
        }),
      ],
    });
    const r = within(row("OLD"));
    expect(r.getByText("Unused")).toBeTruthy();
    fireEvent.click(r.getByRole("button", { name: "Remove OLD" }));
    expect(onRemove).toHaveBeenCalledWith("OLD");
  });

  it("is Unused as soon as it is added, when the files do not refer to it", () => {
    mount({ vars: [workingVar({ name: "NEW" })] });
    expect(within(row("NEW")).getByText("Unused")).toBeTruthy();
  });

  it("is not called unused before the server has answered", () => {
    mount({ vars: [workingVar({ name: "NEW" })], resolved: null });
    expect(within(row("NEW")).queryByText("Unused")).toBeNull();
  });
});

describe("adding a variable", () => {
  it("adds a valid name once, and shows the two errors under the field", () => {
    const { onAdd } = mount();
    fireEvent.click(screen.getByRole("button", { name: "Add variable" }));
    const field = screen.getByLabelText("Variable name");
    fireEvent.change(field, { target: { value: "1BAD" } });
    fireEvent.submit(field.closest("form") as HTMLFormElement);
    expect(
      screen.getByText("Use letters, numbers and underscores, and don't start with a number."),
    ).toBeTruthy();
    expect(onAdd).not.toHaveBeenCalled();

    fireEvent.change(field, { target: { value: "WEB_PORT" } });
    fireEvent.submit(field.closest("form") as HTMLFormElement);
    expect(screen.getByText("That variable is already in the table.")).toBeTruthy();
    expect(onAdd).not.toHaveBeenCalled();

    fireEvent.change(field, { target: { value: "GOOD_NAME" } });
    fireEvent.submit(field.closest("form") as HTMLFormElement);
    expect(onAdd).toHaveBeenCalledWith("GOOD_NAME");
    expect(screen.queryByLabelText("Variable name")).toBeNull();
  });
});

describe("the Secret switch", () => {
  it("is named for its variable and sends the change", () => {
    const { onSecret } = mount();
    fireEvent.click(screen.getByRole("switch", { name: "Secret: WEB_PORT" }));
    expect(onSecret).toHaveBeenCalledWith("WEB_PORT", true);
  });

  it("makes the value a password field that never fills itself in, and shows a hidden value as dots", () => {
    mount({
      vars: [workingVar({ name: "DB_PASS", secret: true, hidden: true })],
      resolved: [
        resolvedVar({
          name: "DB_PASS",
          value: "<hidden>",
          secret: true,
          source: "table",
          hasDefault: false,
          default: "",
        }),
      ],
    });
    const input = screen.getByLabelText("Value of DB_PASS") as HTMLInputElement;
    expect(input.type).toBe("password");
    expect(input.autocomplete).toBe("new-password");
    expect(input.getAttribute("spellcheck")).toBe("false");
    expect(input.placeholder).toBe("Hidden. Type to replace.");
    expect(input.value).toBe("");
    expect(within(row("DB_PASS")).getByText("••••••••")).toBeTruthy();
    expect(screen.queryByText("<hidden>")).toBeNull();
  });
});

describe("the states", () => {
  it("shows six skeleton rows while the draft loads, and no empty state", () => {
    mount({ loading: true, resolved: null });
    expect(screen.getAllByTestId("variable-skeleton-row")).toHaveLength(6);
    expect(screen.queryByText("No variables")).toBeNull();
  });

  it("shows a skeleton in the Resolved cell until the server has answered", () => {
    mount({ vars: [workingVar({ name: "A" })], resolved: null });
    expect(within(row("A")).getByTestId("resolved-skeleton")).toBeTruthy();
  });

  it("shows a dash and the retry note when the check failed before it ever answered", () => {
    mount({ vars: [workingVar({ name: "A" })], resolved: null, failed: true });
    const r = within(row("A"));
    expect(r.getByText("—")).toBeTruthy();
    expect(r.getByText("Couldn't check. It will retry.")).toBeTruthy();
    expect(r.queryByTestId("resolved-skeleton")).toBeNull();
  });

  it("says there are no variables when there are none", () => {
    mount({ resolved: [] });
    expect(screen.getByText("No variables")).toBeTruthy();
  });

  it("renders one row for one variable and a row for each of many, naming the count with a plural", () => {
    mount({
      resolved: [
        resolvedVar({
          places: [
            { file: "compose.yaml", line: 3 },
            { file: "compose.yaml", line: 9 },
          ],
        }),
      ],
    });
    expect(screen.getByText("Used in 2 places")).toBeTruthy();
    cleanup();
    const many = Array.from({ length: 300 }, (_, i) =>
      resolvedVar({ name: `VAR_${String(i).padStart(3, "0")}` }),
    );
    mount({ resolved: many });
    expect(screen.getAllByRole("row")).toHaveLength(301);
  });

  it("truncates a long name behind a tooltip and keeps the whole name reachable", () => {
    const long = "A_VERY_LONG_VARIABLE_NAME_THAT_KEEPS_GOING";
    mount({ resolved: [resolvedVar({ name: long })] });
    expect(within(row(long)).getByText(long).className).toContain("truncate");
  });

  it("names the table's columns for a screen reader and shows the caption", () => {
    mount();
    for (const name of ["Name", "Value", "Secret", "Resolved"]) {
      expect(screen.getByRole("columnheader", { name }).getAttribute("scope")).toBe("col");
    }
    expect(screen.getByText(/Values come from this table/)).toBeTruthy();
  });
});

describe("a viewer", () => {
  it("can read the table but not change it", () => {
    mount({ readOnly: true, vars: [workingVar({ value: "9090" })] });
    expect((screen.getByLabelText("Value of WEB_PORT") as HTMLInputElement).disabled).toBe(true);
    expect(screen.queryByRole("button", { name: "Add variable" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Clear value of WEB_PORT" })).toBeNull();
  });
});
