// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { GitStackVariable } from "@/api/git";
import { TooltipProvider } from "@/components/ui/tooltip";
import {
  RequiredVariablesLine,
  type TableVar,
  VariablesPreview,
} from "@/features/git-source/variables-preview";
import "@/i18n";

// The Variables card of the From git page (plan 09-16, GIT-08, D-07): every ${VAR} the compose files
// use, the value typed for it, whether it is secret, and the value a deploy would use with where it
// comes from. The server resolves; the page only draws.

afterEach(cleanup);

const varOf = (name: string, over: Partial<GitStackVariable> = {}): GitStackVariable => ({
  name,
  value: "",
  default: "",
  hasDefault: false,
  places: [{ file: "compose.yaml", line: 3 }],
  required: false,
  secret: false,
  secretLike: false,
  source: "unset",
  unused: false,
  ...over,
});

// Harness holds the table the way the page does, and tells the test what it holds.
function Harness({
  vars,
  initial = [],
  enabled = true,
  checking = false,
  seen = () => {},
}: {
  vars: readonly GitStackVariable[] | null;
  initial?: TableVar[];
  enabled?: boolean;
  checking?: boolean;
  seen?: (table: TableVar[]) => void;
}) {
  const [value, setValue] = useState<TableVar[]>(initial);
  return (
    <VariablesPreview
      vars={vars}
      value={value}
      enabled={enabled}
      checking={checking}
      onChange={(next) => {
        seen(next);
        setValue(next);
      }}
    />
  );
}

function mount(props: Parameters<typeof Harness>[0]) {
  return render(
    <TooltipProvider>
      <Harness {...props} />
    </TooltipProvider>,
  );
}

const row = (name: string) => screen.getByTestId(`git-variable-row-${name}`);

describe("VariablesPreview", () => {
  it("lists every detected variable with Name, Value, Secret and Resolved", () => {
    mount({ vars: [varOf("DB_HOST"), varOf("WEB_PORT")] });
    for (const name of ["Name", "Value", "Secret", "Resolved"]) {
      expect(screen.getByRole("columnheader", { name })).toBeTruthy();
    }
    expect(within(row("DB_HOST")).getByText("DB_HOST")).toBeTruthy();
    expect(within(row("WEB_PORT")).getByText("WEB_PORT")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Variables" })).toBeTruthy();
    expect(
      screen.getByText(
        "Values come from this table first, then from the repository's .env, then from the compose file's defaults.",
      ),
    ).toBeTruthy();
  });

  it("says in each Resolved cell where the value comes from", () => {
    mount({
      vars: [
        varOf("A_TABLE", { source: "table", value: "typed" }),
        varOf("B_REPO", { source: "repo-env", value: "from-env" }),
        varOf("C_DEFAULT", { source: "default", value: "dflt", hasDefault: true, default: "dflt" }),
        varOf("D_UNSET", { source: "unset", required: true }),
      ],
    });
    expect(within(row("A_TABLE")).getByText("From the table")).toBeTruthy();
    expect(within(row("A_TABLE")).getByText("typed")).toBeTruthy();
    expect(within(row("B_REPO")).getByText("From the repo's .env")).toBeTruthy();
    expect(within(row("B_REPO")).getByText("from-env")).toBeTruthy();
    expect(within(row("C_DEFAULT")).getByText("From the compose file's default")).toBeTruthy();
    expect(within(row("C_DEFAULT")).getByText("dflt")).toBeTruthy();
    expect(within(row("D_UNSET")).getByText("Not set")).toBeTruthy();
    expect(within(row("D_UNSET")).getByText("Required")).toBeTruthy();
  });

  it("puts the .env value, else the default, else Required in the Value placeholder", () => {
    mount({
      vars: [
        varOf("B_REPO", { source: "repo-env", value: "from-env", hasDefault: true, default: "d" }),
        varOf("C_DEFAULT", { source: "default", value: "dflt", hasDefault: true, default: "dflt" }),
        varOf("D_UNSET", { source: "unset", required: true }),
      ],
    });
    const placeholder = (name: string) =>
      (screen.getByLabelText(`Value of ${name}`) as HTMLInputElement).placeholder;
    expect(placeholder("B_REPO")).toBe("from-env");
    expect(placeholder("C_DEFAULT")).toBe("dflt");
    expect(placeholder("D_UNSET")).toBe("Required");
  });

  it("reports a typed value and a Secret switch to the page, and clears a value", () => {
    const seen = vi.fn();
    mount({ vars: [varOf("DB_HOST")], seen });
    fireEvent.change(screen.getByLabelText("Value of DB_HOST"), { target: { value: "db1" } });
    expect(seen).toHaveBeenLastCalledWith([{ name: "DB_HOST", value: "db1", secret: false }]);
    fireEvent.click(screen.getByRole("switch", { name: "Secret: DB_HOST" }));
    expect(seen).toHaveBeenLastCalledWith([{ name: "DB_HOST", value: "db1", secret: true }]);
    expect((screen.getByLabelText("Value of DB_HOST") as HTMLInputElement).type).toBe("password");
    fireEvent.click(screen.getByRole("button", { name: "Clear value of DB_HOST" }));
    expect(seen).toHaveBeenLastCalledWith([]);
    expect((screen.getByLabelText("Value of DB_HOST") as HTMLInputElement).value).toBe("");
  });

  it("shows a secret value as dots in Resolved and never gives the typed text back", () => {
    mount({
      vars: [varOf("API_URL", { source: "table", secret: true, value: "<hidden>" })],
      initial: [{ name: "API_URL", value: "hunter2", secret: true }],
    });
    expect(row("API_URL").textContent).not.toContain("<hidden>");
    expect(within(row("API_URL")).getByText("••••••••")).toBeTruthy();
    expect(document.body.textContent).not.toContain("hunter2");
  });

  it("hides a secret-like value committed in the .env behind dots, with the reason, and warns", async () => {
    mount({
      vars: [varOf("DB_PASSWORD", { source: "repo-env", secretLike: true, value: "<hidden>" })],
    });
    const dots = within(row("DB_PASSWORD")).getByText("••••");
    expect(row("DB_PASSWORD").textContent).not.toContain("<hidden>");
    fireEvent.focus(dots);
    expect(
      (
        await screen.findAllByText(
          "Committed in the repository. Hidden here because the name looks like a secret.",
        )
      ).length,
    ).toBeGreaterThan(0);
    expect(within(row("DB_PASSWORD")).getByText("From the repo's .env")).toBeTruthy();
    expect(
      screen.getByText(
        "DB_PASSWORD looks like a secret and is committed in the repository's .env. Set a secret variable here to override it.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Set secret value" })).toBeTruthy();
    // G-09-3: the shadcn TableCell base is whitespace-nowrap; the warning cell must override it so
    // the sentence wraps at 375px instead of spilling across the shrink-0 button.
    const cell = screen.getByTestId("git-variable-warning-DB_PASSWORD").querySelector("td");
    expect(cell?.className).toContain("whitespace-normal");
    expect(cell?.className).not.toContain("whitespace-nowrap");
  });

  it("turns the row into a secret row and focuses its input on Set secret value", () => {
    const seen = vi.fn();
    mount({
      vars: [varOf("DB_PASSWORD", { source: "repo-env", secretLike: true, value: "<hidden>" })],
      seen,
    });
    fireEvent.click(screen.getByRole("button", { name: "Set secret value" }));
    expect(seen).toHaveBeenLastCalledWith([{ name: "DB_PASSWORD", value: "", secret: true }]);
    const input = screen.getByLabelText("Value of DB_PASSWORD") as HTMLInputElement;
    expect(input.type).toBe("password");
    expect(document.activeElement).toBe(input);
    expect(
      (
        screen.getByRole("switch", { name: "Secret: DB_PASSWORD" }) as HTMLButtonElement
      ).getAttribute("aria-checked"),
    ).toBe("true");
  });

  it("drops the warning once a value is typed over the committed one", () => {
    mount({
      vars: [varOf("DB_PASSWORD", { source: "repo-env", secretLike: true, value: "<hidden>" })],
    });
    fireEvent.change(screen.getByLabelText("Value of DB_PASSWORD"), {
      target: { value: "s3cret" },
    });
    expect(screen.queryByRole("button", { name: "Set secret value" })).toBeNull();
  });

  it("marks a variable of the table that no file uses as unused and lets it go", () => {
    const seen = vi.fn();
    mount({
      vars: [varOf("OLD", { source: "table", value: "x", unused: true, places: [] })],
      initial: [{ name: "OLD", value: "x", secret: false }],
      seen,
    });
    expect(within(row("OLD")).getByText("Unused")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Remove OLD" }));
    expect(seen).toHaveBeenLastCalledWith([]);
  });

  it("drops the variables of a removed compose file after the next check", () => {
    const { rerender } = mount({ vars: [varOf("FROM_A"), varOf("FROM_B")] });
    expect(screen.getAllByRole("row")).toHaveLength(3);
    rerender(
      <TooltipProvider>
        <Harness vars={[varOf("FROM_A")]} />
      </TooltipProvider>,
    );
    expect(screen.queryByTestId("git-variable-row-FROM_B")).toBeNull();
    expect(screen.getByTestId("git-variable-row-FROM_A")).toBeTruthy();
  });

  it("says there are no variables when the compose files use none", () => {
    mount({ vars: [] });
    expect(screen.getByText("No variables")).toBeTruthy();
    expect(screen.getByText(/The compose file doesn't use any \$\{VARIABLES\}\./)).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("waits for a compose file, and for the first check", () => {
    const { unmount } = mount({ vars: null, enabled: false });
    expect(screen.getByText("Choose a compose file first.")).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
    unmount();
    mount({ vars: null, checking: true });
    expect(screen.getByText("Checking the files…")).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("scrolls 100 or more variables inside the card at the editor's row height", () => {
    const many = Array.from({ length: 120 }, (_, i) =>
      varOf(`VAR_${String(i).padStart(3, "0")}`, { source: "default", value: "v" }),
    );
    mount({ vars: many });
    expect(screen.getAllByRole("row")).toHaveLength(121);
    const scroller = screen.getByTestId("git-variables-scroll");
    expect(scroller.className).toContain("overflow-y-auto");
    expect(scroller.className).toMatch(/max-h-/);
    expect(row("VAR_000").className).toContain("h-12");
  });
});

describe("RequiredVariablesLine", () => {
  it("counts the required variables that have no value", () => {
    render(
      <RequiredVariablesLine
        vars={[
          varOf("A", { required: true }),
          varOf("B", { required: true }),
          varOf("C", { source: "table", value: "x" }),
        ]}
      />,
    );
    expect(
      screen.getByText(
        "2 required variables have no value. Review won't deploy until you set them.",
      ),
    ).toBeTruthy();
  });

  it("says it in the singular for one", () => {
    render(<RequiredVariablesLine vars={[varOf("A", { required: true })]} />);
    expect(
      screen.getByText("1 required variable has no value. Review won't deploy until you set it."),
    ).toBeTruthy();
  });

  it("is absent when every variable has a value, and before the first check", () => {
    const { container, rerender } = render(
      <RequiredVariablesLine vars={[varOf("A", { source: "default", value: "x" })]} />,
    );
    expect(container.textContent).toBe("");
    rerender(<RequiredVariablesLine vars={null} />);
    expect(container.textContent).toBe("");
  });

  it("does not count a variable no file uses", () => {
    const { container } = render(
      <RequiredVariablesLine vars={[varOf("A", { required: true, unused: true })]} />,
    );
    expect(container.textContent).toBe("");
  });
});
