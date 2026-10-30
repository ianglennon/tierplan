// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  EMPTY_VALUE,
  formatSize,
  maxBytes,
  type ObjectKind,
  type ObjectValue,
  ValueSourceField,
  valueBytes,
  valueProblem,
  valueSize,
} from "@/features/secrets/value-source-field";
import "@/i18n";

// The value field (plan 06-23, RES-08, D-24, D-25, D-40): type it or upload a file; the value
// lives only in React state, is never previewed for a secret, is checked against the 500 KB
// limit in the browser, and is cleared when the field goes away.

afterEach(cleanup);

const file = (name: string, bytes: Uint8Array<ArrayBuffer> | string) => new File([bytes], name);

// A harness that holds the value as a form would, and shows nothing of it itself.
function Harness({
  kind = "secret",
  initial = EMPTY_VALUE,
  problem,
  spy,
}: {
  kind?: ObjectKind;
  initial?: ObjectValue;
  problem?: "empty" | "noFile" | "tooBig";
  spy?: (v: ObjectValue) => void;
}) {
  const [value, setValue] = useState(initial);
  return (
    <ValueSourceField
      kind={kind}
      value={value}
      problem={problem}
      onChange={(v) => {
        spy?.(v);
        setValue(v);
      }}
    />
  );
}

const chooser = () => document.querySelector("input[type='file']") as HTMLInputElement;

describe("the pure helpers", () => {
  it("allows 500 KB less one byte for a secret and 1000 KB less one byte for a config", () => {
    expect(maxBytes("secret")).toBe(511_999);
    expect(maxBytes("config")).toBe(1_023_999);
  });

  it("measures typed text as UTF-8 bytes and a file by its size", () => {
    expect(valueSize({ ...EMPTY_VALUE, text: "abc" })).toBe(3);
    expect(valueSize({ ...EMPTY_VALUE, text: "é" })).toBe(2);
    expect(
      valueSize({
        source: "file",
        text: "",
        file: { name: "f", size: 40, bytes: new Uint8Array(40) },
      }),
    ).toBe(40);
    expect(valueSize(EMPTY_VALUE)).toBe(0);
  });

  it("gives the bytes to send: the UTF-8 of typed text, or the file's bytes", () => {
    expect(Array.from(valueBytes({ ...EMPTY_VALUE, text: "é" }) ?? [])).toEqual([0xc3, 0xa9]);
    const bytes = new Uint8Array([1, 2, 3]);
    expect(valueBytes({ source: "file", text: "", file: { name: "f", size: 3, bytes } })).toBe(
      bytes,
    );
    expect(valueBytes({ source: "file", text: "", file: null })).toBeNull();
  });

  it("finds the problem in a value: empty, no file, or too big", () => {
    expect(valueProblem("secret", EMPTY_VALUE)).toBe("empty");
    expect(valueProblem("secret", { ...EMPTY_VALUE, text: "x" })).toBeUndefined();
    expect(valueProblem("secret", { ...EMPTY_VALUE, text: "x".repeat(511_999) })).toBeUndefined();
    expect(valueProblem("secret", { ...EMPTY_VALUE, text: "x".repeat(512_000) })).toBe("tooBig");
    expect(valueProblem("config", { ...EMPTY_VALUE, text: "x".repeat(512_000) })).toBeUndefined();
    expect(valueProblem("secret", { source: "file", text: "", file: null })).toBe("noFile");
    expect(
      valueProblem("secret", {
        source: "file",
        text: "",
        file: { name: "f", size: 600_000, bytes: null },
      }),
    ).toBe("tooBig");
    expect(
      valueProblem("secret", {
        source: "file",
        text: "",
        file: { name: "f", size: 0, bytes: new Uint8Array(0) },
      }),
    ).toBe("empty");
  });

  it("words a size", () => {
    expect(formatSize(12)).toBe("12 B");
    expect(formatSize(2048)).toBe("2 KB");
    expect(formatSize(1536)).toBe("1.5 KB");
    expect(formatSize(400 * 1024)).toBe("400 KB");
  });
});

describe("typing a secret", () => {
  it("offers Type it and Upload a file, with Type it chosen", () => {
    render(<Harness />);
    expect(screen.getByRole("radio", { name: "Type it" }).getAttribute("aria-checked")).toBe(
      "true",
    );
    expect(screen.getByRole("radio", { name: "Upload a file" }).getAttribute("aria-checked")).toBe(
      "false",
    );
  });

  it("is a masked input that no browser fills in or checks", () => {
    render(<Harness />);
    const input = screen.getByLabelText("Value") as HTMLInputElement;
    expect(input.type).toBe("password");
    expect(input.getAttribute("autocomplete")).toBe("new-password");
    expect(input.getAttribute("spellcheck")).toBe("false");
    expect(input.getAttribute("autocapitalize")).toBe("off");
    expect(
      screen.getByText("For multi-line values such as certificates, upload a file."),
    ).toBeTruthy();
  });

  it("shows and hides the value with a button that says whether it is pressed", () => {
    render(<Harness initial={{ ...EMPTY_VALUE, text: "hunter2" }} />);
    const input = screen.getByLabelText("Value") as HTMLInputElement;
    const show = screen.getByRole("button", { name: "Show value" });
    expect(show.getAttribute("aria-pressed")).toBe("false");
    fireEvent.click(show);
    expect(input.type).toBe("text");
    const hide = screen.getByRole("button", { name: "Hide value" });
    expect(hide.getAttribute("aria-pressed")).toBe("true");
    fireEvent.click(hide);
    expect(input.type).toBe("password");
  });

  it("hands what is typed to the owner", () => {
    const spy = vi.fn();
    render(<Harness spy={spy} />);
    fireEvent.change(screen.getByLabelText("Value"), { target: { value: "s3cret" } });
    expect(spy).toHaveBeenLastCalledWith({ source: "type", text: "s3cret", file: null });
  });

  it("shows the size against the limit as the person types", () => {
    render(<Harness initial={{ ...EMPTY_VALUE, text: "x".repeat(2048) }} />);
    expect(screen.getByText("2 KB of 500 KB")).toBeTruthy();
  });

  it("accepts 511,999 bytes and refuses 512,000 with the limit in words", () => {
    const { unmount } = render(<Harness initial={{ ...EMPTY_VALUE, text: "x".repeat(511_999) }} />);
    expect(screen.queryByText("Values can be at most 500 KB.")).toBeNull();
    unmount();
    render(<Harness initial={{ ...EMPTY_VALUE, text: "x".repeat(512_000) }} />);
    expect(screen.getByText("Values can be at most 500 KB.")).toBeTruthy();
    expect(screen.getByLabelText("Value").getAttribute("aria-invalid")).toBe("true");
  });

  it("shows the problem a submit found: Enter a value.", () => {
    render(<Harness problem="empty" />);
    expect(screen.getByText("Enter a value.")).toBeTruthy();
    expect(screen.getByLabelText("Value").getAttribute("aria-invalid")).toBe("true");
  });
});

describe("uploading a file", () => {
  it("switches to a Choose file button and drops what was typed", () => {
    const spy = vi.fn();
    render(<Harness initial={{ ...EMPTY_VALUE, text: "typed" }} spy={spy} />);
    fireEvent.click(screen.getByRole("radio", { name: "Upload a file" }));
    expect(spy).toHaveBeenLastCalledWith({ source: "file", text: "", file: null });
    expect(screen.getByRole("button", { name: "Choose file" })).toBeTruthy();
    expect(screen.queryByLabelText("Value")).toBeNull();
  });

  it("says Choose a file. when a submit found none", () => {
    render(<Harness initial={{ source: "file", text: "", file: null }} problem="noFile" />);
    expect(screen.getByText("Choose a file.")).toBeTruthy();
  });

  it("reads the file with the File API and shows its name and size, never its content", async () => {
    const spy = vi.fn();
    render(<Harness initial={{ source: "file", text: "", file: null }} spy={spy} />);
    const content = "-----BEGIN PRIVATE KEY-----\nTOP-SECRET-CONTENT";
    fireEvent.change(chooser(), { target: { files: [file("tls.key", content)] } });
    await screen.findByText(`tls.key (${formatSize(content.length)})`);
    const last = spy.mock.calls.at(-1)?.[0] as ObjectValue;
    expect(last.source).toBe("file");
    expect(last.file?.name).toBe("tls.key");
    expect(new TextDecoder().decode(last.file?.bytes ?? new Uint8Array())).toBe(content);
    // Nothing of it is shown.
    expect(document.body.textContent).not.toContain("TOP-SECRET-CONTENT");
    expect(document.querySelector("textarea")).toBeNull();
    expect(screen.getByText("0 KB of 500 KB")).toBeTruthy();
  });

  it("removes the file with Remove file", async () => {
    const spy = vi.fn();
    render(<Harness initial={{ source: "file", text: "", file: null }} spy={spy} />);
    fireEvent.change(chooser(), { target: { files: [file("tls.key", "abc")] } });
    fireEvent.click(await screen.findByRole("button", { name: "Remove file" }));
    expect(spy).toHaveBeenLastCalledWith({ source: "file", text: "", file: null });
    expect(screen.getByRole("button", { name: "Choose file" })).toBeTruthy();
  });

  it("does not read a file over the limit, and says so", async () => {
    const spy = vi.fn();
    render(<Harness initial={{ source: "file", text: "", file: null }} spy={spy} />);
    fireEvent.change(chooser(), { target: { files: [file("big.bin", new Uint8Array(512_000))] } });
    await screen.findByText("Values can be at most 500 KB.");
    const last = spy.mock.calls.at(-1)?.[0] as ObjectValue;
    expect(last.file).toEqual({ name: "big.bin", size: 512_000, bytes: null });
  });

  it("wraps a long file name in mono rather than running off the sheet", async () => {
    const name = `${"a".repeat(120)}.pem`;
    render(<Harness initial={{ source: "file", text: "", file: null }} />);
    fireEvent.change(chooser(), { target: { files: [file(name, "x")] } });
    const label = await screen.findByText(`${name} (1 B)`);
    expect(label.className).toContain("break-all");
    expect(label.className).toContain("font-mono");
  });
});

describe("clearing", () => {
  it("clears the value when the field goes away", () => {
    const spy = vi.fn();
    const { unmount } = render(
      <ValueSourceField kind="secret" value={{ ...EMPTY_VALUE, text: "hunter2" }} onChange={spy} />,
    );
    spy.mockClear();
    unmount();
    expect(spy).toHaveBeenCalledWith(EMPTY_VALUE);
  });

  it("never writes the value to storage or the address", async () => {
    render(<Harness />);
    fireEvent.change(screen.getByLabelText("Value"), { target: { value: "hunter2-unique" } });
    await waitFor(() =>
      expect((screen.getByLabelText("Value") as HTMLInputElement).value).toBe("hunter2-unique"),
    );
    expect(JSON.stringify({ ...localStorage })).not.toContain("hunter2-unique");
    expect(JSON.stringify({ ...sessionStorage })).not.toContain("hunter2-unique");
    expect(window.location.href).not.toContain("hunter2-unique");
  });
});

describe("a config", () => {
  it("is a text area of 12 rows with the 1000 KB limit, not a masked input", () => {
    render(<Harness kind="config" initial={{ ...EMPTY_VALUE, text: "key=value" }} />);
    const area = screen.getByLabelText("Content") as HTMLTextAreaElement;
    expect(area.tagName).toBe("TEXTAREA");
    expect(area.rows).toBe(12);
    expect(screen.getByText("0 KB of 1000 KB")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Show value" })).toBeNull();
  });
});
