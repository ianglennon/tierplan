// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { type ComponentProps, useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { FileRail } from "@/features/stack-editor/file-rail";
import type { WorkingFile } from "@/features/stack-editor/working-copy";
import "@/i18n";

// The file rail (plan 07-03, D-05; UI-SPEC "Editor" file rail and "Add file popover"): the files
// of the working copy, compose.yaml pinned first, and the ways to add, rename and delete the rest.

const file = (path: string, over: Partial<WorkingFile> = {}): WorkingFile => ({
  path,
  content: "",
  secret: false,
  hidden: false,
  ...over,
});

type Props = ComponentProps<typeof FileRail>;

function setup(over: Partial<Props> = {}) {
  const handlers = {
    onSelect: vi.fn(),
    onAdd: vi.fn(),
    onRename: vi.fn(),
    onDelete: vi.fn(),
  };
  const props: Props = {
    files: [file("compose.yaml"), file(".env"), file("config/nginx.conf")],
    selected: "compose.yaml",
    readOnly: false,
    ...handlers,
    ...over,
  };
  render(
    <TooltipProvider>
      <FileRail {...props} />
    </TooltipProvider>,
  );
  return handlers;
}

afterEach(cleanup);

const nav = () => screen.getByRole("navigation", { name: "Stack files" });
const rowButtons = () =>
  within(nav()).getAllByRole("button", { name: /^(?!Actions for|Add file)/ });

describe("the list", () => {
  it("is a labelled navigation with compose.yaml first and the others in the order given", () => {
    setup();
    expect(within(nav()).getByRole("heading", { name: "Files" })).toBeTruthy();
    const names = within(nav())
      .getAllByRole("listitem")
      .map((li) => li.querySelector("[data-file]")?.getAttribute("data-file"));
    expect(names).toEqual(["compose.yaml", ".env", "config/nginx.conf"]);
  });

  it("marks the open file with aria-current", () => {
    setup({ selected: ".env" });
    const open = within(nav()).getByRole("button", { name: ".env" });
    expect(open.getAttribute("aria-current")).toBe("true");
    expect(
      within(nav()).getByRole("button", { name: "compose.yaml" }).getAttribute("aria-current"),
    ).toBeNull();
  });

  it("gives compose.yaml no menu and every other file one", () => {
    setup();
    expect(within(nav()).queryByRole("button", { name: "Actions for compose.yaml" })).toBeNull();
    expect(within(nav()).getByRole("button", { name: "Actions for .env" })).toBeTruthy();
    expect(
      within(nav()).getByRole("button", { name: "Actions for config/nginx.conf" }),
    ).toBeTruthy();
  });

  it("opens a file when it is clicked", () => {
    const { onSelect } = setup();
    fireEvent.click(within(nav()).getByRole("button", { name: "config/nginx.conf" }));
    expect(onSelect).toHaveBeenCalledWith("config/nginx.conf");
  });

  it("says what a stack with only compose.yaml can add", () => {
    setup({ files: [file("compose.yaml")] });
    expect(
      screen.getByText(
        "No other files. Add one for an env_file, a config or a secret the compose file refers to.",
      ),
    ).toBeTruthy();
  });

  it("does not say it once there is another file", () => {
    setup();
    expect(screen.queryByText(/^No other files\./)).toBeNull();
  });

  it("badges a secret file and labels one the compose file does not use", () => {
    setup({
      files: [file("compose.yaml"), file("key.pem", { secret: true, hidden: true }), file(".env")],
      unused: new Set([".env"]),
    });
    const row = (path: string) => {
      const el = nav().querySelector<HTMLElement>(`[data-file="${path}"]`);
      if (!el) throw new Error(`no row for ${path}`);
      return el;
    };
    const secret = row("key.pem");
    expect(within(secret).getByText("Secret")).toBeTruthy();
    const env = row(".env");
    expect(within(env).getByText("Not used")).toBeTruthy();
    expect(within(secret).queryByText("Not used")).toBeNull();
  });

  it("badges a file the compose file uses under secrets before the server has sealed it", () => {
    setup({
      files: [file("compose.yaml"), file("tok.txt", { content: "abc" })],
      secret: new Set(["tok.txt"]),
    });
    const row = nav().querySelector<HTMLElement>('[data-file="tok.txt"]');
    expect(within(row as HTMLElement).getByText("Secret")).toBeTruthy();
    const compose = nav().querySelector<HTMLElement>('[data-file="compose.yaml"]');
    expect(within(compose as HTMLElement).queryByText("Secret")).toBeNull();
  });

  it("does not badge a file twice when it is both flagged and used under secrets", () => {
    setup({
      files: [file("compose.yaml"), file("tok.txt", { secret: true, hidden: true })],
      secret: new Set(["tok.txt"]),
    });
    expect(within(nav()).getAllByText("Secret")).toHaveLength(1);
  });

  it("keeps the whole name on a long one, truncated by the layout", () => {
    const long = `${"deeply/".repeat(10)}nginx-with-a-very-long-name.conf`;
    setup({ files: [file("compose.yaml"), file(long)] });
    const button = within(nav()).getByRole("button", { name: long });
    expect(button.querySelector(".truncate")?.textContent).toBe(long);
  });
});

describe("the keyboard", () => {
  it("moves between files with the arrow keys, Home and End", () => {
    setup();
    const [first, second, third] = rowButtons();
    if (!first || !second || !third) throw new Error("three file buttons expected");
    first.focus();
    fireEvent.keyDown(first, { key: "ArrowDown" });
    expect(document.activeElement).toBe(second);
    fireEvent.keyDown(second, { key: "ArrowDown" });
    expect(document.activeElement).toBe(third);
    fireEvent.keyDown(third, { key: "ArrowDown" });
    expect(document.activeElement).toBe(third);
    fireEvent.keyDown(third, { key: "ArrowUp" });
    expect(document.activeElement).toBe(second);
    fireEvent.keyDown(second, { key: "Home" });
    expect(document.activeElement).toBe(first);
    fireEvent.keyDown(first, { key: "End" });
    expect(document.activeElement).toBe(third);
  });

  it("opens the row menu from the menu key and from Shift+F10", async () => {
    setup();
    const env = within(nav()).getByRole("button", { name: ".env" });
    env.focus();
    fireEvent.keyDown(env, { key: "ContextMenu" });
    expect(await screen.findByRole("menuitem", { name: "Rename…" })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: "Delete file" })).toBeTruthy();
    fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());
    fireEvent.keyDown(env, { key: "F10", shiftKey: true });
    expect(await screen.findByRole("menuitem", { name: "Delete file" })).toBeTruthy();
  });

  it("does not open a menu on compose.yaml", () => {
    setup();
    const compose = within(nav()).getByRole("button", { name: "compose.yaml" });
    fireEvent.keyDown(compose, { key: "ContextMenu" });
    expect(screen.queryByRole("menu")).toBeNull();
  });
});

describe("Add file", () => {
  async function openAdd() {
    fireEvent.click(screen.getByRole("button", { name: "Add file" }));
    return screen.findByRole("dialog", { name: "Add a file" });
  }
  const typeName = (dialog: HTMLElement, name: string) =>
    fireEvent.change(within(dialog).getByLabelText("File name"), { target: { value: name } });
  const submit = (dialog: HTMLElement) =>
    fireEvent.click(within(dialog).getByRole("button", { name: "Add file" }));

  it("opens a popover with a labelled field, its helper and the placeholder", async () => {
    setup();
    const dialog = await openAdd();
    const field = within(dialog).getByLabelText("File name") as HTMLInputElement;
    expect(field.placeholder).toBe(".env or config/nginx.conf");
    expect(within(dialog).getByText("A path inside this stack.")).toBeTruthy();
  });

  it("adds a file by its path and closes", async () => {
    const { onAdd } = setup();
    const dialog = await openAdd();
    typeName(dialog, "config/app.env");
    submit(dialog);
    expect(onAdd).toHaveBeenCalledWith("config/app.env");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it.each(["../x", "/etc/passwd", "a//b", "a\\b", "C:/x", ""])(
    "refuses %j with the path message and adds nothing",
    async (name) => {
      const { onAdd } = setup();
      const dialog = await openAdd();
      typeName(dialog, name);
      submit(dialog);
      const alert = await within(dialog).findByRole("alert");
      expect(alert.textContent).toBe("Use a path inside this stack, without “..” or a leading /.");
      expect(onAdd).not.toHaveBeenCalled();
      expect(screen.getByRole("dialog", { name: "Add a file" })).toBeTruthy();
    },
  );

  it("refuses a name the stack already has, naming it", async () => {
    const { onAdd } = setup();
    const dialog = await openAdd();
    typeName(dialog, ".env");
    submit(dialog);
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe("A file named .env already exists.");
    expect(onAdd).not.toHaveBeenCalled();
  });

  it("clears the message when the name is changed", async () => {
    setup();
    const dialog = await openAdd();
    typeName(dialog, "../x");
    submit(dialog);
    await within(dialog).findByRole("alert");
    typeName(dialog, "x");
    expect(within(dialog).queryByRole("alert")).toBeNull();
  });

  it("starts empty each time it is opened", async () => {
    setup();
    const dialog = await openAdd();
    typeName(dialog, "../x");
    submit(dialog);
    await within(dialog).findByRole("alert");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    const again = await openAdd();
    expect((within(again).getByLabelText("File name") as HTMLInputElement).value).toBe("");
    expect(within(again).queryByRole("alert")).toBeNull();
  });

  it("adds each uploaded file by its name, with its text", async () => {
    const { onAdd } = setup();
    const dialog = await openAdd();
    const input = dialog.querySelector<HTMLInputElement>("input[type='file']");
    if (!input) throw new Error("no file input");
    expect(input.multiple).toBe(true);
    fireEvent.change(input, {
      target: {
        files: [
          new File(["A=1\n"], "app.env", { type: "text/plain" }),
          new File(["events {}"], "nginx.conf", { type: "text/plain" }),
        ],
      },
    });
    await waitFor(() => expect(onAdd).toHaveBeenCalledTimes(2));
    expect(onAdd).toHaveBeenCalledWith("app.env", "A=1\n");
    expect(onAdd).toHaveBeenCalledWith("nginx.conf", "events {}");
  });

  it("skips an uploaded file whose name is taken or too big and says so", async () => {
    const { onAdd } = setup();
    const dialog = await openAdd();
    const input = dialog.querySelector<HTMLInputElement>("input[type='file']");
    if (!input) throw new Error("no file input");
    fireEvent.change(input, {
      target: {
        files: [
          new File(["x"], ".env"),
          new File(["x".repeat(1024 * 1024 + 1)], "big.bin"),
          new File(["ok"], "ok.txt"),
        ],
      },
    });
    await waitFor(() => expect(onAdd).toHaveBeenCalledWith("ok.txt", "ok"));
    expect(onAdd).toHaveBeenCalledTimes(1);
    const alerts = within(dialog)
      .getAllByRole("alert")
      .map((a) => a.textContent);
    expect(alerts).toContain("A file named .env already exists.");
    expect(alerts).toContain("Files can be at most 1 MB.");
  });
});

describe("the row menu", () => {
  async function openMenu(name: string) {
    const trigger = within(nav()).getByRole("button", { name: `Actions for ${name}` });
    fireEvent.keyDown(trigger, { key: "Enter" });
    return screen.findByRole("menu");
  }

  it("deletes a file without asking", async () => {
    const { onDelete } = setup();
    await openMenu(".env");
    fireEvent.click(screen.getByRole("menuitem", { name: "Delete file" }));
    expect(onDelete).toHaveBeenCalledWith(".env");
  });

  it("renames a file through a dialog that checks the new name", async () => {
    const { onRename } = setup();
    await openMenu(".env");
    fireEvent.click(screen.getByRole("menuitem", { name: "Rename…" }));
    const dialog = await screen.findByRole("dialog", { name: "Rename .env" });
    const field = within(dialog).getByLabelText("File name") as HTMLInputElement;
    expect(field.value).toBe(".env");

    fireEvent.change(field, { target: { value: "../env" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Rename" }));
    expect((await within(dialog).findByRole("alert")).textContent).toBe(
      "Use a path inside this stack, without “..” or a leading /.",
    );
    fireEvent.change(field, { target: { value: "compose.yaml" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Rename" }));
    expect((await within(dialog).findByRole("alert")).textContent).toBe(
      "A file named compose.yaml already exists.",
    );
    expect(onRename).not.toHaveBeenCalled();

    fireEvent.change(field, { target: { value: "prod.env" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Rename" }));
    expect(onRename).toHaveBeenCalledWith(".env", "prod.env");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("closes the rename dialog without renaming when the name is not changed", async () => {
    const { onRename } = setup();
    await openMenu(".env");
    fireEvent.click(screen.getByRole("menuitem", { name: "Rename…" }));
    const dialog = await screen.findByRole("dialog", { name: "Rename .env" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Rename" }));
    expect(onRename).not.toHaveBeenCalled();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("cannot rename a secret file whose content is hidden", async () => {
    setup({ files: [file("compose.yaml"), file("key.pem", { secret: true, hidden: true })] });
    await openMenu("key.pem");
    expect(screen.queryByRole("menuitem", { name: "Rename…" })).toBeNull();
    expect(screen.getByRole("menuitem", { name: "Delete file" })).toBeTruthy();
  });
});

describe("a Viewer", () => {
  it("sees the files and can open them, with no way to add, rename or delete", () => {
    const { onSelect } = setup({ readOnly: true });
    expect(screen.queryByRole("button", { name: "Add file" })).toBeNull();
    expect(within(nav()).queryByRole("button", { name: /^Actions for/ })).toBeNull();
    fireEvent.click(within(nav()).getByRole("button", { name: ".env" }));
    expect(onSelect).toHaveBeenCalledWith(".env");
  });

  it("is not told to add a file when only compose.yaml exists", () => {
    setup({ readOnly: true, files: [file("compose.yaml")] });
    expect(screen.queryByText(/^No other files\./)).toBeNull();
  });
});

describe("under 768px", () => {
  beforeEach(() => {
    vi.stubGlobal("innerWidth", 375);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("shows no list of buttons, only the select", () => {
    setup();
    expect(screen.getByRole("combobox", { name: "File" })).toBeTruthy();
    expect(within(nav()).queryByRole("button", { name: "config/nginx.conf" })).toBeNull();
    expect(screen.getByRole("button", { name: "Add file" })).toBeTruthy();
  });

  it("offers a labelled select of the files that opens the one chosen", () => {
    const { onSelect } = setup({ selected: ".env" });
    const select = screen.getByRole("combobox", { name: "File" }) as HTMLSelectElement;
    expect(Array.from(select.options).map((o) => o.value)).toEqual([
      "compose.yaml",
      ".env",
      "config/nginx.conf",
    ]);
    expect(select.value).toBe(".env");
    fireEvent.change(select, { target: { value: "config/nginx.conf" } });
    expect(onSelect).toHaveBeenCalledWith("config/nginx.conf");
  });

  it("says in the select which file the compose file does not use", () => {
    setup({ unused: new Set([".env"]) });
    const select = screen.getByRole("combobox", { name: "File" }) as HTMLSelectElement;
    expect(Array.from(select.options).map((o) => o.textContent)).toEqual([
      "compose.yaml",
      ".env (Not used)",
      "config/nginx.conf",
    ]);
  });

  it("keeps the actions of the open file beside the select", () => {
    setup({ selected: ".env" });
    const mobile = screen.getByTestId("file-select");
    expect(within(mobile).getByRole("button", { name: "Actions for .env" })).toBeTruthy();
  });
});

describe("as the working copy changes", () => {
  it("shows a file as soon as it is added", () => {
    function Harness() {
      const [files, setFiles] = useState([file("compose.yaml")]);
      return (
        <TooltipProvider>
          <FileRail
            files={files}
            selected="compose.yaml"
            readOnly={false}
            onSelect={() => {}}
            onAdd={(p) => setFiles((f) => [...f, file(p)])}
            onRename={() => {}}
            onDelete={() => {}}
          />
        </TooltipProvider>
      );
    }
    render(<Harness />);
    fireEvent.click(screen.getByRole("button", { name: "Add file" }));
    return screen.findByRole("dialog", { name: "Add a file" }).then((dialog) => {
      fireEvent.change(within(dialog).getByLabelText("File name"), {
        target: { value: "new.env" },
      });
      fireEvent.click(within(dialog).getByRole("button", { name: "Add file" }));
      expect(within(nav()).getByRole("button", { name: "new.env" })).toBeTruthy();
    });
  });
});

describe("a git stack", () => {
  const gitFiles = [
    file("compose.yaml", { origin: "repo" }),
    file("web.env", { origin: "repo" }),
    file("db_pw.txt", { origin: "overlay", secret: true, hidden: true }),
  ];

  it("badges each file Repo or Overlay and gives the menu to overlay files only", () => {
    setup({ git: true, files: gitFiles, onAddOverlay: vi.fn() });
    const row = (path: string) => nav().querySelector(`[data-file="${path}"]`) as HTMLElement;
    expect(within(row("compose.yaml")).getByText("Repo")).toBeTruthy();
    expect(within(row("db_pw.txt")).getByText("Overlay")).toBeTruthy();
    // An overlay is always secret, so it carries one badge and not two.
    expect(within(row("db_pw.txt")).queryByText("Secret")).toBeNull();
    expect(within(nav()).getAllByRole("button", { name: /^Actions for/ })).toHaveLength(1);
    expect(within(nav()).getByRole("button", { name: "Actions for db_pw.txt" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Add file" })).toBeNull();
    expect(screen.getByRole("button", { name: "Add overlay file" })).toBeTruthy();
  });

  it("deletes an overlay file from its menu", async () => {
    const { onDelete } = setup({ git: true, files: gitFiles, onAddOverlay: vi.fn() });
    fireEvent.keyDown(within(nav()).getByRole("button", { name: "Actions for db_pw.txt" }), {
      key: "Enter",
    });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Delete file" }));
    expect(onDelete).toHaveBeenCalledWith("db_pw.txt");
  });

  it("says there are no overlay files, under the rail, until there is one", () => {
    setup({ git: true, files: gitFiles.slice(0, 2), onAddOverlay: vi.fn() });
    expect(
      screen.getByText(
        "No overlay files. Add one for a secret or config file that isn't in the repository.",
      ),
    ).toBeTruthy();
    cleanup();
    setup({ git: true, files: gitFiles, onAddOverlay: vi.fn() });
    expect(screen.queryByText(/^No overlay files\./)).toBeNull();
  });

  it("hands over the path and the contents of a new overlay file and nothing else", async () => {
    const onAddOverlay = vi.fn();
    const { onAdd } = setup({ git: true, files: gitFiles, onAddOverlay });
    fireEvent.click(screen.getByRole("button", { name: "Add overlay file" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText("Path"), { target: { value: "secrets/x.txt" } });
    fireEvent.change(within(dialog).getByLabelText("Contents"), { target: { value: "s3cret" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Add overlay file" }));
    expect(onAddOverlay).toHaveBeenCalledWith("secrets/x.txt", "s3cret");
    expect(onAdd).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("gives a Viewer the files and the empty line, with no button and no menu", () => {
    const { onSelect } = setup({
      git: true,
      readOnly: true,
      files: gitFiles.slice(0, 2),
      onAddOverlay: vi.fn(),
    });
    expect(screen.queryByRole("button", { name: "Add overlay file" })).toBeNull();
    expect(within(nav()).queryByRole("button", { name: /^Actions for/ })).toBeNull();
    fireEvent.click(within(nav()).getByRole("button", { name: /^web\.env/ }));
    expect(onSelect).toHaveBeenCalledWith("web.env");
  });

  describe("under 768px", () => {
    beforeEach(() => {
      vi.stubGlobal("innerWidth", 375);
    });
    afterEach(() => {
      vi.unstubAllGlobals();
    });

    it("says in the select where each file comes from", () => {
      setup({ git: true, files: gitFiles, onAddOverlay: vi.fn() });
      const select = screen.getByRole("combobox", { name: "File" }) as HTMLSelectElement;
      expect(Array.from(select.options).map((o) => o.textContent)).toEqual([
        "compose.yaml (Repo)",
        "web.env (Repo)",
        "db_pw.txt (Overlay)",
      ]);
    });
  });
});
