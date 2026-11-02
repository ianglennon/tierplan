// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  REVEAL_MS,
  SecretFileCard,
  type SecretFileCardProps,
} from "@/features/stack-editor/secret-file-card";
import "@/i18n";

// The card that stands in for the editor of a secret stack file (plan 07-13, STK-04, UI-SPEC
// "Editor" row "Secret file (content hidden)"): the content is never shown unless the person holds
// secrets.reveal and asks, and then only for a minute.

const SECRET = "tok-abcdef-12345";

function mount(over: Partial<SecretFileCardProps> = {}) {
  const props: SecretFileCardProps = {
    path: "tok.txt",
    canReveal: false,
    readOnly: false,
    reveal: vi.fn(async () => SECRET),
    onReplace: vi.fn(),
    onUpload: vi.fn(),
    ...over,
  };
  const view = render(<SecretFileCard {...props} />);
  return {
    ...props,
    ...view,
    update: (next: Partial<SecretFileCardProps>) =>
      view.rerender(<SecretFileCard {...props} {...next} />),
  };
}

afterEach(() => cleanup());

describe("the card", () => {
  it("says the content is hidden, never shows it, and offers Replace and Upload", () => {
    mount();
    expect(screen.getByText("The content of tok.txt is hidden.")).toBeTruthy();
    expect(
      screen.getByText(
        "Secret files are stored encrypted and never shown again. Replace the content to change it.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Replace content" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Upload a file" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Show content" })).toBeNull();
    expect(document.body.textContent).not.toContain(SECRET);
  });

  it("offers Show content only to a user with secrets.reveal", () => {
    mount({ canReveal: true });
    const show = screen.getByRole("button", { name: "Show content" });
    expect(show.getAttribute("aria-pressed")).toBe("false");
  });

  it("offers no way to change the file to a Viewer, who can still be allowed to reveal it", () => {
    mount({ readOnly: true, canReveal: true });
    expect(screen.queryByRole("button", { name: "Replace content" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Upload a file" })).toBeNull();
    expect(screen.getByRole("button", { name: "Show content" })).toBeTruthy();
  });

  it("hands over to the page when Replace content is chosen", () => {
    const { onReplace } = mount();
    fireEvent.click(screen.getByRole("button", { name: "Replace content" }));
    expect(onReplace).toHaveBeenCalledTimes(1);
  });

  it("names a long file name whole and lets it break", () => {
    const long = `${"deeply/".repeat(10)}tok.txt`;
    mount({ path: long });
    const title = screen.getByText(`The content of ${long} is hidden.`);
    expect(title.className).toContain("break-all");
  });
});

describe("Upload a file", () => {
  const chosen = (content: string | Blob, name = "token.txt") =>
    new File([content], name, { type: "text/plain" });
  const pick = (file: File) => {
    const input = document.querySelector<HTMLInputElement>('input[type="file"]');
    if (!input) throw new Error("no file input");
    fireEvent.change(input, { target: { files: [file] } });
  };

  it("reads the chosen file in the browser and hands its text over, uploading nothing", async () => {
    const fetchSpy = vi.fn();
    vi.stubGlobal("fetch", fetchSpy);
    const { onUpload } = mount();
    await act(async () => {
      pick(chosen("new-token-text"));
    });
    expect(onUpload).toHaveBeenCalledWith("new-token-text");
    expect(fetchSpy).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it("refuses a file over 500 KB and says so, keeping the text out of the page", async () => {
    const { onUpload } = mount();
    await act(async () => {
      pick(chosen("x".repeat(512_000)));
    });
    expect(onUpload).not.toHaveBeenCalled();
    expect(screen.getByText("Secret files can be at most 500 KB.")).toBeTruthy();
    expect(screen.getByRole("alert").textContent).toContain("500 KB");
  });

  it("accepts a file of exactly 500 KB less one byte, the most a secret may hold", async () => {
    const { onUpload } = mount();
    await act(async () => {
      pick(chosen("y".repeat(511_999)));
    });
    expect(onUpload).toHaveBeenCalledTimes(1);
  });
});

describe("Show content", () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  const show = () => screen.getByRole("button", { name: /^(Show|Hide) content$/ });
  const content = () =>
    screen.queryByRole("textbox", { name: "Content of tok.txt" }) as HTMLTextAreaElement | null;

  it("fetches the content on the click, shows it read-only as text and marks the button pressed", async () => {
    const { reveal } = mount({ canReveal: true });
    expect(reveal).not.toHaveBeenCalled();
    await act(async () => {
      fireEvent.click(show());
    });
    expect(reveal).toHaveBeenCalledTimes(1);
    expect(content()?.value).toBe(SECRET);
    expect(content()?.readOnly).toBe(true);
    expect(show().getAttribute("aria-pressed")).toBe("true");
    expect(show().textContent).toBe("Hide content");
  });

  it("hides the content again after 60 seconds and announces it politely", async () => {
    mount({ canReveal: true });
    await act(async () => {
      fireEvent.click(show());
    });
    await act(async () => {
      vi.advanceTimersByTime(REVEAL_MS - 1);
    });
    expect(content()?.value).toBe(SECRET);
    await act(async () => {
      vi.advanceTimersByTime(1);
    });
    expect(content()).toBeNull();
    expect(show().textContent).toBe("Show content");
    const live = screen.getByText("tok.txt hidden again");
    expect(live.closest("[aria-live]")?.getAttribute("aria-live")).toBe("polite");
    expect(document.body.textContent).not.toContain(SECRET);
  });

  it("hides the content at once when Hide content is pressed", async () => {
    mount({ canReveal: true });
    await act(async () => {
      fireEvent.click(show());
    });
    await act(async () => {
      fireEvent.click(show());
    });
    expect(content()).toBeNull();
    expect(screen.getByText("tok.txt hidden again")).toBeTruthy();
  });

  it("lets go of the content when the person chooses to replace the file", async () => {
    const { update } = mount({ canReveal: true });
    await act(async () => {
      fireEvent.click(show());
    });
    expect(content()).not.toBeNull();
    update({ path: "other.txt" });
    expect(content()).toBeNull();
  });

  it("never lets the content outlive the card", async () => {
    const { unmount } = mount({ canReveal: true });
    await act(async () => {
      fireEvent.click(show());
    });
    unmount();
    expect(document.body.textContent).not.toContain(SECRET);
    // The timer went with it: nothing fires after the card is gone.
    await act(async () => {
      vi.advanceTimersByTime(REVEAL_MS * 2);
    });
  });

  it("says what is missing when the server refuses with 403 or fails, and shows nothing", async () => {
    const reveal = vi
      .fn()
      .mockRejectedValueOnce(Object.assign(new Error("denied"), { status: 403 }))
      .mockRejectedValueOnce(Object.assign(new Error("boom"), { status: 500 }));
    mount({ canReveal: true, reveal });
    await act(async () => {
      fireEvent.click(show());
    });
    expect(
      screen.getByText("Needs the reveal secrets permission to show this content."),
    ).toBeTruthy();
    await act(async () => {
      fireEvent.click(show());
    });
    expect(screen.getByText("Couldn't load the content. Try again.")).toBeTruthy();
    expect(content()).toBeNull();
  });

  it("asks once for a click, not once per render", async () => {
    const { reveal, update } = mount({ canReveal: true });
    update({});
    update({});
    expect(reveal).not.toHaveBeenCalled();
  });
});
