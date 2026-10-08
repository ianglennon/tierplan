// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { matchesName, TypeToConfirm } from "@/components/type-to-confirm";
import "@/i18n";

afterEach(cleanup);

describe("matchesName", () => {
  it("is exact, case-sensitive and ignores spaces at the ends", () => {
    expect(matchesName("alice", "alice")).toBe(true);
    expect(matchesName("alice ", "alice")).toBe(true);
    expect(matchesName("  alice", "alice")).toBe(true);
    expect(matchesName("Alice", "alice")).toBe(false);
    expect(matchesName("alic", "alice")).toBe(false);
    expect(matchesName("alice b", "alice")).toBe(false);
    expect(matchesName("", "alice")).toBe(false);
  });
});

function Harness({ name, onConfirm }: { name: string; onConfirm: () => void }) {
  const [value, setValue] = useState("");
  const ok = matchesName(value, name);
  return (
    <div>
      <TypeToConfirm name={name} value={value} onChange={setValue} onConfirm={onConfirm} />
      <button type="button" aria-disabled={!ok} onClick={() => ok && onConfirm()}>
        Delete user
      </button>
    </div>
  );
}

describe("TypeToConfirm", () => {
  it("is labelled 'Type {name} to confirm' and described by 'Type {name} to enable this button'", () => {
    render(<Harness name="alice" onConfirm={vi.fn()} />);
    const input = screen.getByLabelText("Type alice to confirm");
    const hint = document.getElementById(input.getAttribute("aria-describedby") ?? "");
    expect(hint?.textContent).toBe("Type alice to enable this button");
  });

  it("turns autocomplete and spellcheck off", () => {
    render(<Harness name="alice" onConfirm={vi.fn()} />);
    const input = screen.getByLabelText("Type alice to confirm");
    expect(input.getAttribute("autocomplete")).toBe("off");
    expect(input.getAttribute("spellcheck")).toBe("false");
  });

  it("matches a pasted name with a trailing space", () => {
    render(<Harness name="alice" onConfirm={vi.fn()} />);
    const button = screen.getByRole("button", { name: "Delete user" });
    expect(button.getAttribute("aria-disabled")).toBe("true");
    fireEvent.change(screen.getByLabelText("Type alice to confirm"), {
      target: { value: "alice " },
    });
    expect(button.getAttribute("aria-disabled")).toBe("false");
  });

  it("stays unmatched for a different case or a part of the name", () => {
    render(<Harness name="alice" onConfirm={vi.fn()} />);
    const input = screen.getByLabelText("Type alice to confirm");
    const button = screen.getByRole("button", { name: "Delete user" });
    fireEvent.change(input, { target: { value: "Alice" } });
    expect(button.getAttribute("aria-disabled")).toBe("true");
    fireEvent.change(input, { target: { value: "alic" } });
    expect(button.getAttribute("aria-disabled")).toBe("true");
  });

  it("ignores Enter until the value matches, then confirms", () => {
    const onConfirm = vi.fn();
    render(<Harness name="alice" onConfirm={onConfirm} />);
    const input = screen.getByLabelText("Type alice to confirm");
    fireEvent.change(input, { target: { value: "ali" } });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onConfirm).not.toHaveBeenCalled();
    fireEvent.change(input, { target: { value: "alice" } });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });
});
