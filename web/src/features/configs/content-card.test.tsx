// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { EditorView } from "@codemirror/view";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, type Mock, vi } from "vitest";

import { ConfigContentCard } from "@/features/configs/content-card";
import "@/i18n";

// The Content card of a config (plan 06-25, RES-08, UI-SPEC "Secrets and configs"): the content in
// the read-only viewer, coloured by the config's name, with Wrap, Copy and Download; a config that
// is not text offers Download only.

const toBase64 = (text: string) => btoa(text);

let writeText: Mock<(text: string) => Promise<void>>;
let createObjectURL: Mock<(blob: Blob) => string>;
let revokeObjectURL: Mock<(url: string) => void>;
let clicked: { download: string; href: string }[];

beforeEach(() => {
  writeText = vi.fn<(text: string) => Promise<void>>(async () => {});
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
  createObjectURL = vi.fn<(blob: Blob) => string>(() => "blob:config");
  revokeObjectURL = vi.fn<(url: string) => void>();
  Object.defineProperty(URL, "createObjectURL", { value: createObjectURL, configurable: true });
  Object.defineProperty(URL, "revokeObjectURL", { value: revokeObjectURL, configurable: true });
  clicked = [];
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (
    this: HTMLAnchorElement,
  ) {
    clicked.push({ download: this.download, href: this.href });
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  Reflect.deleteProperty(navigator, "clipboard");
});

const YAML_TEXT = "name: api\nreplicas: 2\n";

// The blob the nth download was made from.
function downloaded(n = 0): Blob {
  const blob = createObjectURL.mock.calls[n]?.[0];
  if (!blob) throw new Error("nothing was downloaded");
  return blob;
}

function viewOf(): EditorView {
  const dom = document.querySelector<HTMLElement>(".cm-editor");
  const view = dom ? EditorView.findFromDOM(dom) : null;
  if (!view) throw new Error("no editor on the page");
  return view;
}

function mount(name = "app.yaml", text: string | null = YAML_TEXT, data = toBase64(YAML_TEXT)) {
  render(<ConfigContentCard name={name} text={text} data={data} />);
}

describe("ConfigContentCard viewer", () => {
  it("shows the content in a read-only textbox named 'Content of {name}, read only'", async () => {
    mount();
    expect(screen.getByRole("heading", { name: "Content" })).toBeTruthy();
    const box = await screen.findByRole("textbox", { name: "Content of app.yaml, read only" });
    expect(box.getAttribute("aria-readonly")).toBe("true");
    expect(viewOf().state.doc.toString()).toBe(YAML_TEXT);
    expect(viewOf().state.readOnly).toBe(true);
    expect(document.querySelector(".cm-lineNumbers")).not.toBeNull();
  });

  it.each([
    ["app.yaml", "yaml"],
    ["stack.yml", "yaml"],
    ["settings.json", "json"],
    ["nginx.conf", "text"],
    ["v2_app", "text"],
  ])("colours %s as %s", async (name, language) => {
    mount(name);
    await screen.findByRole("textbox", { name: `Content of ${name}, read only` });
    expect(screen.getByTestId("spec-viewer").getAttribute("data-language")).toBe(language);
  });

  it("has a Wrap toggle that is on", async () => {
    mount();
    await screen.findByRole("textbox");
    expect(screen.getByRole("button", { name: "Wrap" }).getAttribute("aria-pressed")).toBe("true");
  });

  it("is at least 480px tall", async () => {
    mount();
    await screen.findByRole("textbox");
    // The editor's own style rule carries the height the viewer was asked for.
    const editor = document.querySelector<HTMLElement>(".cm-editor");
    expect(editor && getComputedStyle(editor).minHeight).toBe("480px");
  });

  it("shows text as text, never as markup", async () => {
    const hostile = '<img src=x onerror="alert(1)">\n<script>1</script>\n';
    mount("evil.html", hostile, toBase64(hostile));
    await screen.findByRole("textbox");
    expect(viewOf().state.doc.toString()).toBe(hostile);
    expect(document.querySelector("img")).toBeNull();
    expect(document.querySelector("script")).toBeNull();
  });
});

describe("ConfigContentCard Copy and Download", () => {
  it("copies the content", async () => {
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Copy" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(YAML_TEXT));
  });

  it("downloads the bytes as a file named after the config, from browser memory", async () => {
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Download" }));
    expect(createObjectURL).toHaveBeenCalledTimes(1);
    const blob = downloaded();
    expect(blob.type).toBe("application/octet-stream");
    expect(blob.size).toBe(YAML_TEXT.length);
    expect(clicked).toEqual([{ download: "app.yaml", href: "blob:config" }]);
    expect(revokeObjectURL).toHaveBeenCalledWith("blob:config");
  });

  it("makes a file name that cannot make a path", async () => {
    mount("../etc/passwd");
    fireEvent.click(await screen.findByRole("button", { name: "Download" }));
    expect(clicked[0]?.download).toBe("_etc_passwd");
  });
});

describe("ConfigContentCard of content that is not text", () => {
  // 0xff 0xfe 0x00 is not UTF-8.
  const binary = btoa(String.fromCharCode(0xff, 0xfe, 0x00));

  it("says it isn't text and offers Download only", async () => {
    mount("blob.bin", null, binary);
    expect(
      await screen.findByText(
        "This config isn't text, so it can't be shown here. Download it to look at it.",
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.queryByRole("button", { name: "Copy" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Wrap" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Download" }));
    const blob = downloaded();
    expect(blob.size).toBe(3);
    expect(clicked[0]?.download).toBe("blob.bin");
  });
});

describe("ConfigContentCard of a large config", () => {
  it("opens 1000 KB by drawing only the lines in view, and Copy and Download still take all of it", async () => {
    const line = "key_000000: some value here\n";
    const lines = Math.floor((1023 * 1024) / line.length);
    const big = line.repeat(lines);
    mount("big.yaml", big, toBase64(big));
    await screen.findByRole("textbox");
    expect(viewOf().state.doc.lines).toBeGreaterThan(30000);
    expect(document.querySelectorAll(".cm-line").length).toBeLessThan(2000);
    fireEvent.click(screen.getByRole("button", { name: "Copy" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    expect(writeText.mock.calls[0]?.[0]?.length).toBe(big.length);
    fireEvent.click(screen.getByRole("button", { name: "Download" }));
    expect(downloaded().size).toBe(big.length);
  });
});
