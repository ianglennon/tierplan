// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { EditorView } from "@codemirror/view";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, type Mock, vi } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { SpecEditor } from "@/features/spec-editor/spec-editor";
import { renderSpec, SPEC_FORMAT_KEY } from "@/features/spec-editor/spec-format";
import "@/i18n";

const copyText = vi.fn(async (_text: string, _host?: Element | null) => true);
vi.mock("@/components/copyable-code", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/components/copyable-code")>()),
  copyText: (text: string, host?: Element | null) => copyText(text, host),
}));

// The Spec tab's editor (plan 06-19, D-01, D-09, UI-SPEC "Service editor" and "Spec editor
// keyboard"): a labelled multiline textbox over the parsed spec, in YAML or JSON.

const SPEC = {
  Name: "api",
  Labels: { tier: "backend" },
  TaskTemplate: {
    ContainerSpec: { Image: "nginx:1.27", Env: ["MODE=prod", "DB_PASSWORD=<hidden>"] },
  },
  Mode: { Replicated: { Replicas: 2 } },
};

let onChange: Mock<(value: Record<string, unknown>) => void>;
let onValidityChange: Mock<(valid: boolean) => void>;

beforeEach(() => {
  localStorage.clear();
  copyText.mockClear();
  onChange = vi.fn<(value: Record<string, unknown>) => void>();
  onValidityChange = vi.fn<(valid: boolean) => void>();
});

afterEach(() => {
  cleanup();
  localStorage.clear();
});

function renderEditor(initial: Record<string, unknown> = SPEC) {
  return render(
    <TooltipProvider>
      <SpecEditor initial={initial} onChange={onChange} onValidityChange={onValidityChange} />
    </TooltipProvider>,
  );
}

function viewOf(): EditorView {
  const dom = document.querySelector<HTMLElement>(".cm-editor");
  const view = dom ? EditorView.findFromDOM(dom) : null;
  if (!view) throw new Error("no editor on the page");
  return view;
}

// A person typing: the whole text replaced, which is one change to the editor.
function typeSpec(text: string) {
  const view = viewOf();
  act(() => {
    view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: text } });
  });
}

const docText = () => viewOf().state.doc.toString();

describe("SpecEditor", () => {
  it("opens in YAML with the whole spec as text", async () => {
    renderEditor();
    const box = await screen.findByRole("textbox", { name: "Service spec, YAML" });
    expect(box.getAttribute("aria-multiline")).toBe("true");
    expect(docText()).toBe(renderSpec(SPEC, "yaml"));
    expect(screen.getByRole("radio", { name: "YAML" }).getAttribute("aria-checked")).toBe("true");
    expect(screen.getByRole("radio", { name: "JSON" }).getAttribute("aria-checked")).toBe("false");
    expect(screen.getByRole("radiogroup", { name: "Format" })).toBeTruthy();
  });

  it("shows a hidden value as the placeholder the server sent, with the helper that says so", async () => {
    renderEditor();
    await screen.findByRole("textbox", { name: "Service spec, YAML" });
    expect(docText()).toContain("DB_PASSWORD=<hidden>");
    expect(
      screen.getByText(
        "The full service spec as Docker stores it. Hidden values show as <hidden>. They stay as they are unless you replace them.",
      ),
    ).toBeTruthy();
  });

  it("wraps long lines by default and numbers them", async () => {
    renderEditor();
    await screen.findByRole("textbox", { name: "Service spec, YAML" });
    expect(document.querySelector(".cm-content.cm-lineWrapping")).not.toBeNull();
    expect(document.querySelector(".cm-lineNumbers")).not.toBeNull();
  });

  it("switches to JSON through the parsed value and remembers the choice", async () => {
    renderEditor();
    await screen.findByRole("textbox", { name: "Service spec, YAML" });
    fireEvent.click(screen.getByRole("radio", { name: "JSON" }));
    await screen.findByRole("textbox", { name: "Service spec, JSON" });
    expect(docText()).toBe(renderSpec(SPEC, "json"));
    expect(localStorage.getItem(SPEC_FORMAT_KEY)).toBe("json");
    fireEvent.click(screen.getByRole("radio", { name: "YAML" }));
    await screen.findByRole("textbox", { name: "Service spec, YAML" });
    expect(docText()).toBe(renderSpec(SPEC, "yaml"));
    expect(localStorage.getItem(SPEC_FORMAT_KEY)).toBe("yaml");
  });

  it("opens in the format a person chose before", async () => {
    localStorage.setItem(SPEC_FORMAT_KEY, "json");
    renderEditor();
    await screen.findByRole("textbox", { name: "Service spec, JSON" });
    expect(docText()).toBe(renderSpec(SPEC, "json"));
  });

  it("keeps an edit through a format switch", async () => {
    renderEditor();
    await screen.findByRole("textbox", { name: "Service spec, YAML" });
    typeSpec(renderSpec({ ...SPEC, Name: "renamed" }, "yaml"));
    fireEvent.click(screen.getByRole("radio", { name: "JSON" }));
    await waitFor(() => expect(docText()).toContain('"Name": "renamed"'));
  });

  it("calls onChange with the parsed spec when the text parses", async () => {
    renderEditor();
    await screen.findByRole("textbox", { name: "Service spec, YAML" });
    const edited = { ...SPEC, Name: "renamed" };
    typeSpec(renderSpec(edited, "yaml"));
    expect(onChange).toHaveBeenLastCalledWith(edited);
    expect(onValidityChange).not.toHaveBeenCalledWith(false);
  });

  it("does not call onChange for text that does not parse, and says so", async () => {
    renderEditor();
    await screen.findByRole("textbox", { name: "Service spec, YAML" });
    typeSpec("Name: api\nLabels: [oops\n");
    expect(onChange).not.toHaveBeenCalled();
    expect(onValidityChange).toHaveBeenLastCalledWith(false);
    // The alert waits for typing to stop (400 ms), so it is not there yet.
    expect(screen.queryByRole("alert")).toBeNull();
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText(/^Line \d+: .+/)).toBeTruthy();
    expect(within(alert).getByText("Fix the spec before you review changes.")).toBeTruthy();
  });

  it("clears the alert and calls onChange once the text parses again", async () => {
    renderEditor();
    await screen.findByRole("textbox", { name: "Service spec, YAML" });
    typeSpec("Name: [oops\n");
    await screen.findByRole("alert");
    typeSpec(renderSpec(SPEC, "yaml"));
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
    expect(onValidityChange).toHaveBeenLastCalledWith(true);
    expect(onChange).toHaveBeenLastCalledWith(SPEC);
  });

  it("announces one distinct error once: the same error typed on is the same alert", async () => {
    renderEditor();
    await screen.findByRole("textbox", { name: "Service spec, YAML" });
    typeSpec("Name: [oops\n");
    const first = await screen.findByRole("alert");
    typeSpec("Name: [oops!\n");
    await new Promise((r) => setTimeout(r, 500));
    expect(screen.getAllByRole("alert")).toHaveLength(1);
    expect(screen.getByRole("alert")).toBe(first);
  });

  it("names the line of a JSON error", async () => {
    localStorage.setItem(SPEC_FORMAT_KEY, "json");
    renderEditor();
    await screen.findByRole("textbox", { name: "Service spec, JSON" });
    typeSpec('{\n  "Name": "api",\n  "Labels": }\n');
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText(/^Line 3: /)).toBeTruthy();
  });

  it("refuses a format switch while the text is broken, and says why at once", async () => {
    renderEditor();
    await screen.findByRole("textbox", { name: "Service spec, YAML" });
    typeSpec("Name: [oops\n");
    fireEvent.click(screen.getByRole("radio", { name: "JSON" }));
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByRole("radio", { name: "YAML" }).getAttribute("aria-checked")).toBe("true");
    expect(docText()).toBe("Name: [oops\n");
    expect(localStorage.getItem(SPEC_FORMAT_KEY)).toBeNull();
  });

  it("says comments are dropped while the YAML holds some, and not otherwise", async () => {
    renderEditor();
    await screen.findByRole("textbox", { name: "Service spec, YAML" });
    const note = "Comments are dropped if you switch to JSON.";
    expect(screen.queryByText(note)).toBeNull();
    typeSpec(`# the api\n${renderSpec(SPEC, "yaml")}`);
    expect(await screen.findByText(note)).toBeTruthy();
    fireEvent.click(screen.getByRole("radio", { name: "JSON" }));
    await screen.findByRole("textbox", { name: "Service spec, JSON" });
    expect(screen.queryByText(note)).toBeNull();
    // The editor holds a change from outside back while the person is still typing.
    await waitFor(() => expect(docText()).not.toContain("the api"));
  });

  it("copies the text as it is now", async () => {
    renderEditor();
    await screen.findByRole("textbox", { name: "Service spec, YAML" });
    const edited = renderSpec({ ...SPEC, Name: "copied" }, "yaml");
    typeSpec(edited);
    fireEvent.click(screen.getByRole("button", { name: "Copy spec" }));
    await waitFor(() => expect(copyText).toHaveBeenCalledTimes(1));
    expect(copyText.mock.calls[0]?.[0]).toBe(edited);
  });

  it("discards edits: the text and the parsed spec go back to the loaded one", async () => {
    renderEditor();
    await screen.findByRole("textbox", { name: "Service spec, YAML" });
    const discard = screen.getByRole("button", { name: "Discard edits" });
    expect((discard as HTMLButtonElement).disabled).toBe(true);
    typeSpec("Name: [oops\n");
    await waitFor(() => expect((discard as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(discard);
    await waitFor(() => expect(docText()).toBe(renderSpec(SPEC, "yaml")));
    expect(onChange).toHaveBeenLastCalledWith(SPEC);
    expect(onValidityChange).toHaveBeenLastCalledWith(true);
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
    expect((discard as HTMLButtonElement).disabled).toBe(true);
  });

  it("leaves Tab to the browser, so it moves focus out of the editor", async () => {
    renderEditor();
    await screen.findByRole("textbox", { name: "Service spec, YAML" });
    const content = document.querySelector<HTMLElement>(".cm-content");
    expect(content).not.toBeNull();
    const before = docText();
    const tab = new KeyboardEvent("keydown", { key: "Tab", bubbles: true, cancelable: true });
    content?.dispatchEvent(tab);
    expect(tab.defaultPrevented).toBe(false);
    const back = new KeyboardEvent("keydown", {
      key: "Tab",
      shiftKey: true,
      bubbles: true,
      cancelable: true,
    });
    content?.dispatchEvent(back);
    expect(back.defaultPrevented).toBe(false);
    expect(docText()).toBe(before);
  });

  it("opens the search panel on Ctrl+F", async () => {
    renderEditor();
    await screen.findByRole("textbox", { name: "Service spec, YAML" });
    const content = document.querySelector<HTMLElement>(".cm-content");
    const find = new KeyboardEvent("keydown", {
      key: "f",
      ctrlKey: true,
      bubbles: true,
      cancelable: true,
    });
    act(() => {
      content?.dispatchEvent(find);
    });
    await waitFor(() => expect(document.querySelector(".cm-search")).not.toBeNull());
  });

  it("stores nothing but the format choice", async () => {
    renderEditor();
    await screen.findByRole("textbox", { name: "Service spec, YAML" });
    typeSpec(renderSpec({ ...SPEC, Name: "secret-name-xyz" }, "yaml"));
    fireEvent.click(screen.getByRole("radio", { name: "JSON" }));
    await screen.findByRole("textbox", { name: "Service spec, JSON" });
    expect(localStorage.length).toBe(1);
    expect(localStorage.getItem(SPEC_FORMAT_KEY)).toBe("json");
    expect(sessionStorage.length).toBe(0);
    expect(window.location.href).not.toContain("secret-name-xyz");
  });
});
