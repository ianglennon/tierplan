// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { EditorView } from "@codemirror/view";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { SpecViewer } from "@/features/spec-editor/spec-viewer";
import "@/i18n";

// The read-only view of a spec or a config (plan 06-19; the config viewer of a later plan uses
// it too): a labelled textbox that cannot be edited, with a Wrap switch.

afterEach(() => cleanup());

const YAML_TEXT = "Name: api\nLabels:\n  tier: backend\n";

function viewOf(): EditorView {
  const dom = document.querySelector<HTMLElement>(".cm-editor");
  const view = dom ? EditorView.findFromDOM(dom) : null;
  if (!view) throw new Error("no editor on the page");
  return view;
}

describe("SpecViewer", () => {
  it("is a read-only multiline textbox with an accessible name", async () => {
    render(<SpecViewer text={YAML_TEXT} label="Service spec, YAML" format="yaml" />);
    const box = await screen.findByRole("textbox", { name: "Service spec, YAML" });
    expect(box.getAttribute("aria-readonly")).toBe("true");
    expect(box.getAttribute("aria-multiline")).toBe("true");
    expect(viewOf().state.doc.toString()).toBe(YAML_TEXT);
    expect(viewOf().state.readOnly).toBe(true);
  });

  it("shows line numbers", async () => {
    render(<SpecViewer text={YAML_TEXT} label="Spec" format="yaml" />);
    await screen.findByRole("textbox", { name: "Spec" });
    expect(document.querySelector(".cm-lineNumbers")).not.toBeNull();
  });

  it("takes the language from the format when it is given", async () => {
    render(<SpecViewer text="{}" label="Spec" format="json" fileName="nginx.yaml" />);
    await screen.findByRole("textbox", { name: "Spec" });
    expect(screen.getByTestId("spec-viewer").getAttribute("data-language")).toBe("json");
  });

  it.each([
    ["app.json", "json"],
    ["APP.JSON", "json"],
    ["stack.yaml", "yaml"],
    ["stack.yml", "yaml"],
    ["nginx.conf", "text"],
    ["Dockerfile", "text"],
  ])("takes the language of %s from its name: %s", async (fileName, language) => {
    render(<SpecViewer text="x" label="Config" fileName={fileName} />);
    await screen.findByRole("textbox", { name: "Config" });
    expect(screen.getByTestId("spec-viewer").getAttribute("data-language")).toBe(language);
  });

  it("is plain text with neither a format nor a file name", async () => {
    render(<SpecViewer text="x" label="Config" />);
    await screen.findByRole("textbox", { name: "Config" });
    expect(screen.getByTestId("spec-viewer").getAttribute("data-language")).toBe("text");
  });

  it("wraps lines until Wrap is turned off", async () => {
    render(<SpecViewer text={YAML_TEXT} label="Spec" format="yaml" />);
    await screen.findByRole("textbox", { name: "Spec" });
    const wrap = screen.getByRole("button", { name: "Wrap" });
    expect(wrap.getAttribute("aria-pressed")).toBe("true");
    expect(document.querySelector(".cm-content.cm-lineWrapping")).not.toBeNull();
    fireEvent.click(wrap);
    expect(wrap.getAttribute("aria-pressed")).toBe("false");
    expect(document.querySelector(".cm-content.cm-lineWrapping")).toBeNull();
    fireEvent.click(wrap);
    expect(document.querySelector(".cm-content.cm-lineWrapping")).not.toBeNull();
  });

  it("follows a new text", async () => {
    const { rerender } = render(<SpecViewer text={YAML_TEXT} label="Spec" format="yaml" />);
    await screen.findByRole("textbox", { name: "Spec" });
    rerender(<SpecViewer text={"Name: other\n"} label="Spec" format="yaml" />);
    expect(viewOf().state.doc.toString()).toBe("Name: other\n");
  });

  it("leaves Tab to the browser", async () => {
    render(<SpecViewer text={YAML_TEXT} label="Spec" format="yaml" />);
    await screen.findByRole("textbox", { name: "Spec" });
    const tab = new KeyboardEvent("keydown", { key: "Tab", bubbles: true, cancelable: true });
    document.querySelector(".cm-content")?.dispatchEvent(tab);
    expect(tab.defaultPrevented).toBe(false);
  });
});
