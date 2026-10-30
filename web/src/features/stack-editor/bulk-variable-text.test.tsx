// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { createRef, useReducer } from "react";
import { afterEach, describe, expect, it } from "vitest";

import type { StackDraft } from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { fromBulkText, toBulkText } from "@/features/stack-editor/bulk-variable-text";
import { VariablesTable, type VariablesTableHandle } from "@/features/stack-editor/variables-table";
import {
  emptyWorkingCopy,
  toSaveBody,
  type WorkingCopy,
  type WorkingVar,
  workingCopyReducer,
} from "@/features/stack-editor/working-copy";
import "@/i18n";

// Bulk edit as text (plan 07-12, D-04): the table as NAME=value lines. A secret reads NAME=<hidden>
// and keeps its value unless the line is replaced; a line that is not NAME=value blocks the way
// back to the table.

afterEach(cleanup);

const v = (over: Partial<WorkingVar> = {}): WorkingVar => ({
  name: "A",
  value: "1",
  secret: false,
  hidden: false,
  ...over,
});

describe("toBulkText", () => {
  it("writes one NAME=value line per row, an empty value as NAME=", () => {
    expect(toBulkText([v({ name: "A", value: "1" }), v({ name: "B", value: "" })])).toBe("A=1\nB=");
  });

  it("writes a secret as NAME=<hidden> whether it is hidden or was typed, never its value", () => {
    const text = toBulkText([
      v({ name: "SAVED", value: "", secret: true, hidden: true }),
      v({ name: "TYPED", value: "hunter2hunter2", secret: true }),
    ]);
    expect(text).toBe("SAVED=<hidden>\nTYPED=<hidden>");
    expect(text).not.toContain("hunter2");
  });

  it("quotes a value that a line could not carry as written", () => {
    expect(toBulkText([v({ name: "A", value: "two\nlines" })])).toBe('A="two\\nlines"');
    expect(toBulkText([v({ name: "A", value: " padded " })])).toBe('A=" padded "');
    expect(toBulkText([v({ name: "A", value: "x # not a comment" })])).toBe(
      'A="x # not a comment"',
    );
    expect(toBulkText([v({ name: "A", value: "'single'" })])).toBe(`A="'single'"`);
    expect(toBulkText([v({ name: "A", value: 'tab\there "q" \\ ' })])).toBe(
      'A="tab\\there \\"q\\" \\\\ "',
    );
  });

  it("round-trips every plain and quoted value through fromBulkText", () => {
    const rows = [
      v({ name: "A", value: "1" }),
      v({ name: "B", value: "" }),
      v({ name: "C", value: "two\nlines" }),
      v({ name: "D", value: " lead and trail " }),
      v({ name: "E", value: 'say "hi" \\ there' }),
      v({ name: "F", value: "has # inside" }),
      v({ name: "G", value: "'single'" }),
    ];
    const got = fromBulkText(toBulkText(rows), rows);
    expect(got).toEqual({ ok: true, rows });
  });
});

describe("fromBulkText", () => {
  const rows = [
    v({ name: "KEEP", value: "", secret: true, hidden: true }),
    v({ name: "PLAIN", value: "old" }),
    v({ name: "GONE", value: "x" }),
  ];

  it("drops comments and blank lines and reads the rest", () => {
    const got = fromBulkText("# c\n\nPLAIN=new\nADDED=2\n", rows);
    expect(got).toEqual({
      ok: true,
      rows: [v({ name: "ADDED", value: "2" }), v({ name: "PLAIN", value: "new" })],
    });
  });

  it("keeps a secret whose line still says <hidden>, exactly as it was", () => {
    const got = fromBulkText("KEEP=<hidden>\nPLAIN=old", rows);
    expect(got).toEqual({
      ok: true,
      rows: [rows[0], rows[1]],
    });
  });

  it("replaces a secret's value when the line has another, and keeps its Secret flag", () => {
    const got = fromBulkText("KEEP=brand-new-secret", rows);
    expect(got).toEqual({
      ok: true,
      rows: [v({ name: "KEEP", value: "brand-new-secret", secret: true, hidden: false })],
    });
  });

  it("drops the rows whose lines were deleted", () => {
    const got = fromBulkText("PLAIN=old", rows);
    expect(got.ok && got.rows.map((r) => r.name)).toEqual(["PLAIN"]);
  });

  it("names the first line that is not NAME=value", () => {
    expect(fromBulkText("A=1\nB=2\nx y\nalso bad", rows)).toEqual({ ok: false, line: 3 });
    expect(fromBulkText("1BAD=x", rows)).toEqual({ ok: false, line: 1 });
  });
});

// A page in miniature: the working copy lives in a reducer, as in the editor.
function mountTable(
  vars: WorkingVar[],
  over: { readOnly?: boolean; loading?: boolean } = {},
): { copy: () => WorkingCopy; handle: React.RefObject<VariablesTableHandle | null> } {
  const handle = createRef<VariablesTableHandle>();
  let latest: WorkingCopy = emptyWorkingCopy;
  const draft: StackDraft = {
    isDraft: true,
    version: 1,
    savedBy: "ada",
    savedAt: "2026-10-19T10:00:00Z",
    baseSeq: 0,
    files: [{ path: "compose.yaml", content: "services: {}\n", secret: false }],
    vars: vars.map((x) => ({
      name: x.name,
      value: x.hidden ? "<hidden>" : x.value,
      secret: x.secret,
    })),
  };
  function Host() {
    const [copy, dispatch] = useReducer(
      workingCopyReducer,
      workingCopyReducer(emptyWorkingCopy, { type: "load", draft }),
    );
    latest = copy;
    return (
      <TooltipProvider>
        <VariablesTable
          ref={handle}
          vars={copy.vars}
          resolved={[]}
          failed={false}
          readOnly={over.readOnly ?? false}
          loading={over.loading}
          onAdd={(name) => dispatch({ type: "addVar", name })}
          onSet={(name, value) => dispatch({ type: "setVar", name, value })}
          onSecret={(name, secret) => dispatch({ type: "setSecret", name, secret })}
          onRemove={(name) => dispatch({ type: "removeVar", name })}
          onBulk={(rows) => dispatch({ type: "setVars", vars: rows })}
        />
      </TooltipProvider>
    );
  }
  render(<Host />);
  return { copy: () => latest, handle };
}

const toggle = () => screen.getByRole("button", { name: "Bulk edit as text" });
const box = () => screen.getByRole("textbox", { name: "Variables as text" }) as HTMLTextAreaElement;
const type = (text: string) => fireEvent.change(box(), { target: { value: text } });

describe("the Bulk edit as text toggle", () => {
  it("swaps the table for a 12-row mono textarea of NAME=value lines, with the helper sentence", () => {
    mountTable([v({ name: "A", value: "1" }), v({ name: "B", value: "two" })]);
    expect(toggle().getAttribute("aria-pressed")).toBe("false");
    expect(screen.getByRole("table")).toBeTruthy();
    fireEvent.click(toggle());
    expect(toggle().getAttribute("aria-pressed")).toBe("true");
    expect(screen.queryByRole("table")).toBeNull();
    expect(box().value).toBe("A=1\nB=two");
    expect(box().rows).toBe(12);
    expect(box().className).toContain("font-mono");
    expect(box().getAttribute("spellcheck")).toBe("false");
    expect(
      screen.getByText(
        "Names and values only. Secret values show as <hidden> and stay as they are unless you replace them. Switch back to set which rows are secret.",
      ),
    ).toBeTruthy();
  });

  it("never puts a secret's value on the page, hidden or typed", () => {
    mountTable([
      v({ name: "SAVED", value: "", secret: true, hidden: true }),
      v({ name: "TYPED", value: "hunter2hunter2", secret: true }),
    ]);
    fireEvent.click(toggle());
    expect(box().value).toBe("SAVED=<hidden>\nTYPED=<hidden>");
    expect(document.body.textContent).not.toContain("hunter2");
    expect(document.body.innerHTML).not.toContain("hunter2");
  });

  it("brings the edited lines back as rows, keeping each row's Secret flag", () => {
    const page = mountTable([
      v({ name: "TOKEN", value: "", secret: true, hidden: true }),
      v({ name: "PORT", value: "80" }),
    ]);
    fireEvent.click(toggle());
    type("PORT=8080\nTOKEN=<hidden>\nNEW=1");
    fireEvent.click(toggle());
    expect(screen.getByRole("table")).toBeTruthy();
    expect(page.copy().vars).toEqual([
      v({ name: "NEW", value: "1" }),
      v({ name: "PORT", value: "8080" }),
      v({ name: "TOKEN", value: "", secret: true, hidden: true }),
    ]);
    expect((screen.getByLabelText("Value of PORT") as HTMLInputElement).value).toBe("8080");
  });

  it("sends an untouched <hidden> as keep and a replaced one as the new value", () => {
    const page = mountTable([v({ name: "TOKEN", value: "", secret: true, hidden: true })]);
    fireEvent.click(toggle());
    fireEvent.click(toggle());
    expect(toSaveBody(page.copy(), 1).vars).toEqual([
      { name: "TOKEN", value: "<hidden>", secret: true },
    ]);

    fireEvent.click(toggle());
    type("TOKEN=replaced-secret");
    fireEvent.click(toggle());
    expect(toSaveBody(page.copy(), 1).vars).toEqual([
      { name: "TOKEN", value: "replaced-secret", secret: true },
    ]);
  });

  it("keeps the working copy in step with the text as it is typed, so Save draft takes the edit", () => {
    const page = mountTable([v({ name: "A", value: "1" })]);
    fireEvent.click(toggle());
    type("A=2\nB=3");
    expect(page.copy().vars.map((x) => [x.name, x.value])).toEqual([
      ["A", "2"],
      ["B", "3"],
    ]);
  });
});

describe("a line that is not NAME=value", () => {
  it("shows 'Line 3: write NAME=value.', keeps the textarea and moves focus to the message", () => {
    mountTable([v({ name: "A", value: "1" })]);
    fireEvent.click(toggle());
    type("A=1\nB=2\nx y");
    fireEvent.click(toggle());
    const message = screen.getByText("Line 3: write NAME=value.");
    expect(document.activeElement).toBe(message);
    expect(message.getAttribute("role")).toBe("alert");
    expect(toggle().getAttribute("aria-pressed")).toBe("true");
    expect(box().getAttribute("aria-invalid")).toBe("true");
    expect(box().getAttribute("aria-describedby")).toContain(message.id);
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("leaves the working copy at the last text that parsed, and lets the person back once it is fixed", () => {
    const page = mountTable([v({ name: "A", value: "1" })]);
    fireEvent.click(toggle());
    type("A=2\nx y");
    expect(page.copy().vars).toEqual([v({ name: "A", value: "1" })]);
    fireEvent.click(toggle());
    type("A=2\nB=3");
    expect(screen.queryByText(/write NAME=value/)).toBeNull();
    fireEvent.click(toggle());
    expect(screen.getByRole("table")).toBeTruthy();
    expect(page.copy().vars.map((x) => x.name)).toEqual(["A", "B"]);
  });

  it("is shown when the person leaves the box with a bad line in it, without taking focus", () => {
    mountTable([v({ name: "A", value: "1" })]);
    fireEvent.click(toggle());
    type("A=1\nnope");
    expect(screen.queryByText("Line 2: write NAME=value.")).toBeNull();
    fireEvent.blur(box());
    expect(screen.getByText("Line 2: write NAME=value.")).toBeTruthy();
    expect(document.activeElement).not.toBe(screen.getByText("Line 2: write NAME=value."));
  });

  it("refuses a pane change through the handle until the text parses", () => {
    const page = mountTable([v({ name: "A", value: "1" })]);
    expect(page.handle.current?.leave()).toBe(true);
    fireEvent.click(toggle());
    type("what");
    let allowed: boolean | undefined;
    act(() => {
      allowed = page.handle.current?.leave();
    });
    expect(allowed).toBe(false);
    expect(document.activeElement).toBe(screen.getByText("Line 1: write NAME=value."));
    type("A=1");
    expect(page.handle.current?.leave()).toBe(true);
  });
});

describe("the toggle in other states", () => {
  it("is disabled while the draft loads and absent for a Viewer", () => {
    mountTable([], { loading: true });
    expect((toggle() as HTMLButtonElement).disabled).toBe(true);
    cleanup();
    mountTable([v()], { readOnly: true });
    expect(screen.queryByRole("button", { name: "Bulk edit as text" })).toBeNull();
  });

  it("works on an empty table, where it is how a first variable is written", () => {
    const page = mountTable([]);
    fireEvent.click(toggle());
    expect(box().value).toBe("");
    type("FIRST=1");
    fireEvent.click(toggle());
    expect(page.copy().vars).toEqual([v({ name: "FIRST", value: "1" })]);
  });

  it("stays responsive on 300 lines", () => {
    const rows = Array.from({ length: 300 }, (_, i) =>
      v({ name: `VAR_${String(i).padStart(3, "0")}`, value: `${i}` }),
    );
    const page = mountTable(rows);
    fireEvent.click(toggle());
    expect(box().value.split("\n")).toHaveLength(300);
    type(`${box().value}\nEXTRA=1`);
    expect(page.copy().vars).toHaveLength(301);
    expect(within(document.body).queryByText(/write NAME=value/)).toBeNull();
  });
});
