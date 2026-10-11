// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { InspectJson } from "@/components/inspect-json";
import "@/i18n";

const toastError = vi.fn();
vi.mock("@/lib/toast", () => ({
  toastError: (message: string) => toastError(message),
  toastSuccess: vi.fn(),
}));

// The inspect block of a container, network or volume (plan 04-11, D-04): shows the
// document as the server sent it, copies and downloads it without a request.

const DOC = { Id: "abc", Config: { Env: ["DB_PASSWORD=<hidden>"], Cmd: ["nginx", "-g", "x"] } };
const TEXT = JSON.stringify(DOC, null, 2);
const NOTICE =
  "Environment variable values are hidden. Needs the reveal secrets permission to show them.";
const FAILED = "Copy failed. Select the text and copy it manually.";

function stubClipboard(writeText: ((text: string) => Promise<void>) | undefined) {
  Object.defineProperty(navigator, "clipboard", {
    value: writeText ? { writeText } : undefined,
    configurable: true,
  });
}

beforeEach(() => {
  toastError.mockClear();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  stubClipboard(undefined);
  // biome-ignore lint/suspicious/noExplicitAny: removing a stub from jsdom
  delete (document as any).execCommand;
});

describe("InspectJson", () => {
  it("shows the document as indented text in a focusable block named for the container", () => {
    render(<InspectJson name="web" value={DOC} masked={false} />);
    const block = screen.getByLabelText("Inspect output for web");
    expect(block.getAttribute("tabindex")).toBe("0");
    expect(block.textContent).toBe(TEXT);
    expect(block.querySelector("pre")).not.toBeNull();
    // Nothing the document holds is read as markup.
    cleanup();
    render(<InspectJson name="web" value={{ a: "<img src=x onerror=alert(1)>" }} masked={false} />);
    expect(document.querySelector("img")).toBeNull();
  });

  it("says what its caller tells it when values are withheld, in place of the container's wording", () => {
    render(<InspectJson name="data" value={DOC} masked maskedNotice="Option values are hidden." />);
    expect(screen.getByText("Option values are hidden.")).toBeTruthy();
    expect(screen.queryByText(NOTICE)).toBeNull();
  });

  it("explains hidden values only when the server withheld some, and shows its placeholder as sent", () => {
    const { rerender } = render(<InspectJson name="web" value={DOC} masked={false} />);
    expect(screen.queryByText(NOTICE)).toBeNull();
    rerender(<InspectJson name="web" value={DOC} masked />);
    expect(screen.getByText(NOTICE)).toBeTruthy();
    expect(screen.getByLabelText("Inspect output for web").textContent).toContain(
      "DB_PASSWORD=<hidden>",
    );
  });

  it("copies the exact text and says Copied", async () => {
    const writeText = vi.fn(async () => {});
    stubClipboard(writeText);
    render(<InspectJson name="web" value={DOC} masked={false} />);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Copy JSON" }));
    });
    expect(writeText).toHaveBeenCalledWith(TEXT);
    await waitFor(() =>
      expect(screen.getAllByRole("status").some((s) => s.textContent === "Copied")).toBe(true),
    );
    expect(toastError).not.toHaveBeenCalled();
  });

  it("tells the person to copy by hand, and selects the text, when nothing could copy", async () => {
    stubClipboard(undefined);
    Object.defineProperty(document, "execCommand", { configurable: true, value: () => false });
    render(<InspectJson name="web" value={DOC} masked={false} />);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Copy JSON" }));
    });
    expect(toastError).toHaveBeenCalledWith(FAILED);
    expect(screen.queryByText("Copied")).toBeNull();
    expect(window.getSelection()?.toString()).toBe(TEXT);
  });

  it("saves {name}.json from a Blob and makes no request", async () => {
    const fetchSpy = vi.fn();
    vi.stubGlobal("fetch", fetchSpy);
    const created: Blob[] = [];
    const createObjectURL = vi.fn((blob: Blob) => {
      created.push(blob);
      return "blob:inspect";
    });
    const revokeObjectURL = vi.fn();
    vi.stubGlobal("URL", Object.assign(URL, { createObjectURL, revokeObjectURL }));
    const downloads: { name: string; href: string }[] = [];
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (
      this: HTMLAnchorElement,
    ) {
      downloads.push({ name: this.download, href: this.href });
    });
    render(<InspectJson name="web" value={DOC} masked={false} />);
    fireEvent.click(screen.getByRole("button", { name: "Download JSON" }));
    expect(downloads).toEqual([{ name: "web.json", href: "blob:inspect" }]);
    expect(await created[0]?.text()).toBe(TEXT);
    expect(created[0]?.type).toBe("application/json");
    expect(revokeObjectURL).toHaveBeenCalledWith("blob:inspect");
    expect(fetchSpy).not.toHaveBeenCalled();
    expect(document.querySelector("a[download]")).toBeNull();
    click.mockRestore();
  });

  it("does not let a name make a path of the saved file", () => {
    const createObjectURL = vi.fn(() => "blob:x");
    vi.stubGlobal("URL", Object.assign(URL, { createObjectURL, revokeObjectURL: vi.fn() }));
    const names: string[] = [];
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (
      this: HTMLAnchorElement,
    ) {
      names.push(this.download);
    });
    render(<InspectJson name="../etc/passwd" value={DOC} masked={false} />);
    fireEvent.click(screen.getByRole("button", { name: "Download JSON" }));
    expect(names).toEqual(["_etc_passwd.json"]);
    click.mockRestore();
  });
});
