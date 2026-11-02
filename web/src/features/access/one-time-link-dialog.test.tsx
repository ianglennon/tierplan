// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { OneTimeLinkDialog } from "@/features/access/one-time-link-dialog";
import "@/i18n";

const toastError = vi.fn();
vi.mock("@/lib/toast", () => ({
  toastError: (message: string) => toastError(message),
  toastSuccess: vi.fn(),
}));

const TOKEN = "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-AbCde";
const HOUR = 3_600_000;

function link(url = `/invite#token=${TOKEN}`, hours = 72) {
  return { url, expiresAt: new Date(Date.now() + hours * HOUR + 60_000).toISOString() };
}

function stubClipboard(writeText: ((text: string) => Promise<void>) | undefined) {
  Object.defineProperty(navigator, "clipboard", {
    value: writeText ? { writeText } : undefined,
    configurable: true,
  });
}

// jsdom has no execCommand. The stand-in dispatches the "copy" event a browser
// would, and records what a listener put on the clipboard.
function stubExecCommand(works: boolean) {
  const copied: string[] = [];
  Object.defineProperty(document, "execCommand", {
    configurable: true,
    value: () => {
      if (!works) return false;
      const event = new Event("copy", { bubbles: true, cancelable: true });
      Object.defineProperty(event, "clipboardData", {
        value: { setData: (_type: string, data: string) => copied.push(data) },
      });
      document.dispatchEvent(event);
      return true;
    },
  });
  return copied;
}

beforeEach(() => {
  toastError.mockClear();
});

afterEach(() => {
  cleanup();
  stubClipboard(undefined);
  // biome-ignore lint/suspicious/noExplicitAny: removing the test stand-in
  delete (document as any).execCommand;
});

function renderDialog(over: Partial<Parameters<typeof OneTimeLinkDialog>[0]> = {}) {
  const onClose = vi.fn();
  render(
    <OneTimeLinkDialog title="invite" username="alice" link={link()} onClose={onClose} {...over} />,
  );
  return onClose;
}

describe("OneTimeLinkDialog", () => {
  it("titles the dialog by what the link is for", () => {
    renderDialog({ title: "invite" });
    expect(screen.getByRole("dialog", { name: "Invite link for alice" })).toBeTruthy();
    cleanup();
    renderDialog({ title: "ready" });
    expect(screen.getByRole("dialog", { name: "alice is ready" })).toBeTruthy();
    cleanup();
    renderDialog({ title: "reset" });
    expect(screen.getByRole("dialog", { name: "Password reset link for alice" })).toBeTruthy();
  });

  it("says when the link expires, with the hours in a plural-aware line", () => {
    renderDialog({ link: link(undefined, 72) });
    expect(screen.getByText(/Works once\. Expires .+ \(72 hours from now\)\.$/)).toBeTruthy();
    cleanup();
    renderDialog({ link: link(undefined, 1) });
    expect(screen.getByText(/\(1 hour from now\)\.$/)).toBeTruthy();
  });

  it("shows the notice that the link cannot be seen again", () => {
    renderDialog();
    expect(
      screen.getByText(
        "Send this link to alice yourself. Anyone who has it can set the password, so share it privately. You can't see it again after you close this dialog. If you lose it, generate a new one.",
      ),
    ).toBeTruthy();
  });

  it("masks the link by default, reveals it on Show link and hides it again", () => {
    renderDialog();
    const dialog = screen.getByRole("dialog");
    expect(dialog.textContent).not.toContain(TOKEN);
    expect(dialog.textContent).toContain("/invite#");

    const show = screen.getByRole("button", { name: "Show link" });
    expect(show.getAttribute("aria-pressed")).toBe("false");
    fireEvent.click(show);
    expect(dialog.textContent).toContain(TOKEN);
    const hide = screen.getByRole("button", { name: "Hide link" });
    expect(hide.getAttribute("aria-pressed")).toBe("true");
    fireEvent.click(hide);
    expect(dialog.textContent).not.toContain(TOKEN);
  });

  it("keeps the link on one scrolling line with Copy always beside it", () => {
    renderDialog();
    const block = document.querySelector("pre") as HTMLElement;
    expect(block.className).toContain("overflow-x-auto");
    expect(block.className).toContain("font-mono");
    expect(block.className).not.toContain("whitespace-pre-wrap");
    expect(block.contains(screen.getByRole("button", { name: "Copy link" }))).toBe(false);
  });

  it("turns an address relative to TierPlan into a full one and leaves a public URL alone", async () => {
    const writeText = vi.fn(async () => {});
    stubClipboard(writeText);
    renderDialog({ link: link(`/tierplan/invite#token=${TOKEN}`) });
    fireEvent.click(screen.getByRole("button", { name: "Copy link" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    expect(writeText).toHaveBeenLastCalledWith(
      `${window.location.origin}/tierplan/invite#token=${TOKEN}`,
    );
    cleanup();

    renderDialog({ link: link(`https://tierplan.example/invite#token=${TOKEN}`) });
    fireEvent.click(screen.getByRole("button", { name: "Copy link" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(2));
    expect(writeText).toHaveBeenLastCalledWith(`https://tierplan.example/invite#token=${TOKEN}`);
  });

  it("copies the real link by the fallback chain when there is no clipboard API", async () => {
    stubClipboard(undefined);
    const copied = stubExecCommand(true);
    renderDialog();

    fireEvent.click(screen.getByRole("button", { name: "Copy link" }));

    await screen.findByRole("button", { name: "Copied link" });
    expect(copied).toEqual([`${window.location.origin}/invite#token=${TOKEN}`]);
    expect(toastError).not.toHaveBeenCalled();
  });

  it("falls back to the manual-copy toast when nothing can copy", async () => {
    stubClipboard(undefined);
    stubExecCommand(false);
    renderDialog();

    fireEvent.click(screen.getByRole("button", { name: "Copy link" }));

    await waitFor(() =>
      expect(toastError).toHaveBeenCalledWith("Copy failed. Select the text and copy it manually."),
    );
    // The link is revealed and selected so it can be copied by hand.
    expect(screen.getByRole("dialog").textContent).toContain(TOKEN);
  });

  it("calls onClose from Close and renders nothing without a link", () => {
    const onClose = renderDialog();
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Close" }));
    expect(onClose).toHaveBeenCalledTimes(1);
    cleanup();

    renderDialog({ link: null });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.body.textContent).not.toContain(TOKEN);
  });
});
