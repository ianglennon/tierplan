// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  ImportEnvDialog,
  type ImportEnvDialogProps,
} from "@/features/stack-editor/import-env-dialog";
import "@/i18n";

// Import .env (plan 07-12, D-04): the text is parsed in the browser, the preview says what the
// import would do, and nothing leaves the component until Import variables.

afterEach(cleanup);

function mount(over: Partial<ImportEnvDialogProps> = {}) {
  const onImport = vi.fn<ImportEnvDialogProps["onImport"]>();
  const onOpenChange = vi.fn();
  function Host() {
    const [open, setOpen] = useState(true);
    return (
      <>
        <button type="button" onClick={() => setOpen(true)}>
          reopen
        </button>
        <ImportEnvDialog
          open={open}
          existing={[]}
          onImport={onImport}
          {...over}
          onOpenChange={(next) => {
            onOpenChange(next);
            setOpen(next);
          }}
        />
      </>
    );
  }
  render(<Host />);
  return { onImport, onOpenChange };
}

const paste = (text: string) =>
  fireEvent.change(screen.getByLabelText("Paste .env text"), { target: { value: text } });
const submit = () => screen.getByRole("button", { name: "Import variables" });

describe("pasting a .env", () => {
  it("previews what it found and what it ignored, then imports the variables", () => {
    const { onImport, onOpenChange } = mount();
    paste("A=1\nB=\n# c\nbad line");
    expect(screen.getByText("2 new, 0 replaced, 2 lines ignored.")).toBeTruthy();
    expect(
      screen.getByText(
        "1 variable has no value. It stays empty unless the compose file has a default.",
      ),
    ).toBeTruthy();

    fireEvent.click(submit());
    expect(onImport).toHaveBeenCalledWith(
      [
        { name: "A", value: "1" },
        { name: "B", value: "" },
      ],
      "merge",
      false,
    );
    return waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });

  it("counts a name the table has as replaced, and says line in the singular", () => {
    mount({ existing: ["A"] });
    paste("A=1\nB=2\n# c");
    expect(screen.getByText("1 new, 1 replaced, 1 line ignored.")).toBeTruthy();
    expect(screen.queryByText(/have no value|has no value/)).toBeNull();
  });

  it("sends the choices: replace the whole table, and mark every value as secret", () => {
    const { onImport } = mount();
    paste("TOKEN=abcd1234");
    fireEvent.click(screen.getByRole("radio", { name: "Replace the whole table" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "Mark every imported value as secret" }));
    fireEvent.click(submit());
    expect(onImport).toHaveBeenCalledWith([{ name: "TOKEN", value: "abcd1234" }], "replace", true);
  });

  it("starts as merge, with the secret box unticked", () => {
    mount();
    expect(
      screen
        .getByRole("radio", { name: "Merge into the table (values that clash are replaced)" })
        .getAttribute("aria-checked"),
    ).toBe("true");
    expect(
      screen
        .getByRole("checkbox", { name: "Mark every imported value as secret" })
        .getAttribute("aria-checked"),
    ).toBe("false");
  });
});

describe("what keeps Import variables disabled", () => {
  it("is disabled with 'Paste or upload a .env file.' while the text is empty", () => {
    mount();
    expect((submit() as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText("Paste or upload a .env file.")).toBeTruthy();
    expect(submit().getAttribute("aria-describedby")).toBeTruthy();
  });

  it("is disabled with 'Nothing to import.' and says why when no line is a variable", () => {
    mount();
    paste("just some words\n# a comment");
    expect((submit() as HTMLButtonElement).disabled).toBe(true);
    expect(
      screen.getByText("No variables found in that text. Each line should look like NAME=value."),
    ).toBeTruthy();
    expect(screen.getByText("Nothing to import.")).toBeTruthy();
  });

  it("is enabled, with no reason, once one variable parses", () => {
    mount();
    paste("A=1");
    expect((submit() as HTMLButtonElement).disabled).toBe(false);
    expect(screen.queryByText("Nothing to import.")).toBeNull();
    expect(screen.queryByText("Paste or upload a .env file.")).toBeNull();
  });
});

describe("the ignored lines", () => {
  it("are listed with their numbers under Show ignored lines, long text wrapping with break-all", () => {
    mount();
    const long = `bad ${"x".repeat(300)}`;
    paste(`A=1\n# note\n${long}\n\n`);
    expect(screen.queryByRole("list", { name: "Ignored lines" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Show ignored lines" }));
    const list = screen.getByRole("list", { name: "Ignored lines" });
    const items = within(list).getAllByRole("listitem");
    expect(items).toHaveLength(3);
    expect(items[0]?.textContent).toContain("Line 2");
    expect(items[0]?.textContent).toContain("# note");
    expect(items[1]?.textContent).toContain("Line 3");
    expect(items[1]?.textContent).toContain("not NAME=value");
    expect(within(items[1] as HTMLElement).getByText(long).className).toContain("break-all");
    expect(items[2]?.textContent).toContain("Line 4");
    expect(items[2]?.textContent).toContain("blank");
    fireEvent.click(screen.getByRole("button", { name: "Hide ignored lines" }));
    expect(screen.queryByRole("list", { name: "Ignored lines" })).toBeNull();
  });
});

describe("Choose file", () => {
  const file = (name: string, content: string) => new File([content], name);
  const chooser = () => screen.getByTestId("import-env-file") as HTMLInputElement;

  it("reads the file into the box, in memory, and previews it", async () => {
    const fetchSpy = vi.fn();
    vi.stubGlobal("fetch", fetchSpy);
    try {
      mount();
      fireEvent.change(chooser(), { target: { files: [file(".env", "A=1\nB=2\n")] } });
      await waitFor(() =>
        expect((screen.getByLabelText("Paste .env text") as HTMLTextAreaElement).value).toBe(
          "A=1\nB=2\n",
        ),
      );
      expect(screen.getByText("2 new, 0 replaced, 0 lines ignored.")).toBeTruthy();
      expect(fetchSpy).not.toHaveBeenCalled();
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("refuses a file over 1 MB without reading it", async () => {
    mount();
    fireEvent.change(chooser(), { target: { files: [file("big.env", "x".repeat(1_048_577))] } });
    expect((await screen.findByRole("alert")).textContent).toBe("Files can be at most 1 MB.");
    expect((screen.getByLabelText("Paste .env text") as HTMLTextAreaElement).value).toBe("");
  });
});

describe("loading and letting go", () => {
  it("shows Importing… and holds the dialog open while the import settles", async () => {
    let finish: () => void = () => {};
    const { onImport, onOpenChange } = mount({
      onImport: () =>
        new Promise<void>((resolve) => {
          finish = resolve;
        }),
    });
    void onImport;
    paste("A=1");
    fireEvent.click(submit());
    const busy = await screen.findByRole("button", { name: "Importing…" });
    expect((busy as HTMLButtonElement).disabled).toBe(true);
    expect(onOpenChange).not.toHaveBeenCalled();
    await act(async () => finish());
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });

  it("lets go of the text when the dialog is closed", async () => {
    mount();
    paste("SECRET=hunter2hunter2");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByLabelText("Paste .env text")).toBeNull());
    fireEvent.click(screen.getByRole("button", { name: "reopen" }));
    expect(((await screen.findByLabelText("Paste .env text")) as HTMLTextAreaElement).value).toBe(
      "",
    );
  });

  it("stays responsive on a 1,000-line paste", () => {
    mount();
    const lines = Array.from({ length: 1000 }, (_, i) =>
      i % 4 === 0 ? `# note ${i}` : `VAR_${i}=value${i}`,
    );
    paste(lines.join("\n"));
    expect(screen.getByText("750 new, 0 replaced, 250 lines ignored.")).toBeTruthy();
  });
});
