// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { CopyableCode } from "@/components/copyable-code";
import { dockerRunSnippet } from "@/features/environments/docker-run-snippet";
import "@/i18n";

const toastError = vi.fn();
vi.mock("@/lib/toast", () => ({
  toastError: (message: string) => toastError(message),
  toastSuccess: vi.fn(),
}));

const CODE = "docker run -d\n  --name bayplan\n  ghcr.io/ianglennon/bayplan:edge";
const COPY_NAME = "Copy the docker run command";
const FAILED = "Copy failed. Select the text and copy it manually.";

function stubClipboard(writeText: ((text: string) => Promise<void>) | undefined) {
  Object.defineProperty(navigator, "clipboard", {
    value: writeText ? { writeText } : undefined,
    configurable: true,
  });
}

// jsdom has no execCommand. The stand-in records what the hidden textarea held
// at the moment of the call, because the textarea is gone again afterwards.
let execCalls: { command: string; text: string }[];
function stubExecCommand(result: boolean | "throw") {
  execCalls = [];
  Object.defineProperty(document, "execCommand", {
    configurable: true,
    value: (command: string) => {
      const area = document.querySelector("textarea");
      execCalls.push({ command, text: area?.value ?? "" });
      if (result === "throw") throw new Error("not allowed");
      return result;
    },
  });
}

beforeEach(() => {
  toastError.mockClear();
  execCalls = [];
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  stubClipboard(undefined);
  // biome-ignore lint/suspicious/noExplicitAny: removing the test stand-in
  delete (document as any).execCommand;
  window.getSelection()?.removeAllRanges();
});

describe("CopyableCode", () => {
  it("copies the exact text with navigator.clipboard and confirms it", async () => {
    const writeText = vi.fn(async () => {});
    stubClipboard(writeText);
    render(<CopyableCode code={CODE} label="the docker run command" />);

    fireEvent.click(screen.getByRole("button", { name: COPY_NAME }));

    await waitFor(() => expect(writeText).toHaveBeenCalledWith(CODE));
    expect(
      await screen.findByRole("button", { name: "Copied the docker run command" }),
    ).toBeTruthy();
    expect(toastError).not.toHaveBeenCalled();
  });

  it("falls back to a hidden textarea and execCommand when there is no clipboard API", async () => {
    stubClipboard(undefined);
    stubExecCommand(true);
    render(<CopyableCode code={CODE} label="the docker run command" />);

    fireEvent.click(screen.getByRole("button", { name: COPY_NAME }));

    await screen.findByRole("button", { name: "Copied the docker run command" });
    expect(execCalls).toEqual([{ command: "copy", text: CODE }]);
    // The helper textarea never stays in the page.
    expect(document.querySelector("textarea")).toBeNull();
    expect(toastError).not.toHaveBeenCalled();
  });

  it("falls back to execCommand when the clipboard API rejects", async () => {
    stubClipboard(async () => {
      throw new DOMException("denied", "NotAllowedError");
    });
    stubExecCommand(true);
    render(<CopyableCode code={CODE} label="the docker run command" />);

    fireEvent.click(screen.getByRole("button", { name: COPY_NAME }));

    await screen.findByRole("button", { name: "Copied the docker run command" });
    expect(execCalls).toHaveLength(1);
    expect(toastError).not.toHaveBeenCalled();
  });

  it("selects the text and toasts when every copy method fails", async () => {
    stubClipboard(async () => {
      throw new DOMException("denied", "NotAllowedError");
    });
    stubExecCommand("throw");
    render(<CopyableCode code={CODE} label="the docker run command" />);

    fireEvent.click(screen.getByRole("button", { name: COPY_NAME }));

    await waitFor(() => expect(toastError).toHaveBeenCalledWith(FAILED));
    expect(window.getSelection()?.toString()).toBe(CODE);
    expect(document.querySelector("textarea")).toBeNull();
    // It still says "Copy", not "Copied".
    expect(screen.getByRole("button", { name: COPY_NAME })).toBeTruthy();
  });

  it("treats execCommand returning false as a failure", async () => {
    stubClipboard(undefined);
    stubExecCommand(false);
    render(<CopyableCode code={CODE} label="the docker run command" />);

    fireEvent.click(screen.getByRole("button", { name: COPY_NAME }));

    await waitFor(() => expect(toastError).toHaveBeenCalledWith(FAILED));
  });

  it("shows the code as read-only text and goes back to Copy after a moment", async () => {
    vi.useFakeTimers();
    stubClipboard(async () => {});
    const { container } = render(<CopyableCode code={CODE} label="the docker run command" />);
    expect(container.querySelector("code")?.textContent).toBe(CODE);

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: COPY_NAME }));
    });
    expect(screen.getByRole("button", { name: "Copied the docker run command" })).toBeTruthy();

    await act(async () => {
      vi.advanceTimersByTime(2500);
    });
    expect(screen.getByRole("button", { name: COPY_NAME })).toBeTruthy();
  });
});

describe("dockerRunSnippet", () => {
  const mounts = [
    "-p 7480:7480",
    "-v bayplan-data:/data",
    "-v /var/run/docker.sock:/var/run/docker.sock",
  ];

  it("uses the edge tag for a development build", () => {
    for (const version of ["dev", "", "v0.1.0-3-gabc1234-dirty"]) {
      const snippet = dockerRunSnippet(version);
      expect(snippet).toContain("ghcr.io/ianglennon/bayplan:edge");
      for (const m of mounts) expect(snippet).toContain(m);
    }
  });

  it("uses major.minor for a release, with or without the v", () => {
    expect(dockerRunSnippet("0.1.0")).toContain("ghcr.io/ianglennon/bayplan:0.1");
    expect(dockerRunSnippet("v0.2.7")).toContain("ghcr.io/ianglennon/bayplan:0.2");
    expect(dockerRunSnippet("0.1.0")).not.toContain("0.1.0");
    for (const m of mounts) expect(dockerRunSnippet("0.1.0")).toContain(m);
  });

  it("never uses the latest tag", () => {
    for (const version of ["dev", "", "0.1.0", "v1.4.2", "latest", "garbage"]) {
      expect(dockerRunSnippet(version)).not.toContain(":latest");
    }
  });

  it("is the full documented command", () => {
    expect(dockerRunSnippet("dev")).toBe(
      [
        "docker run -d --name bayplan --restart unless-stopped \\",
        "  -p 7480:7480 \\",
        "  -v bayplan-data:/data \\",
        "  -v /var/run/docker.sock:/var/run/docker.sock \\",
        "  ghcr.io/ianglennon/bayplan:edge",
      ].join("\n"),
    );
  });
});
