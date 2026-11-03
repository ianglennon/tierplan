// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { CrashEvidenceSheet } from "@/features/janitor/crash-evidence-sheet";
import "@/i18n";
import { evidence, runItem } from "@/test-janitor";

const ENV = "e1";

let client: QueryClient;
let answer: () => Response | Promise<Response>;
let asked: string[];
const writes: string[] = [];

const problem = (type: string, status: number) =>
  new Response(JSON.stringify({ type, title: "Error", status }), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });

beforeEach(() => {
  setCsrfToken(null);
  asked = [];
  writes.length = 0;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  answer = () => Response.json(evidence());
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname.endsWith("/evidence")) {
        asked.push(url.pathname);
        return answer();
      }
      return Response.json({}, { status: 404 });
    }),
  );
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: {
      writeText: vi.fn(async (text: string) => {
        writes.push(text);
      }),
    },
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(over: { onClose?: () => void; name?: string } = {}) {
  const onClose = over.onClose ?? vi.fn();
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <CrashEvidenceSheet
          envId={ENV}
          runId="r1"
          item={runItem({ seq: 3, name: over.name ?? "web.1.abc", hasEvidence: true })}
          onClose={onClose}
        />
      </TooltipProvider>
    </QueryClientProvider>,
  );
  return onClose;
}

const dialog = () => screen.findByRole("dialog", { name: /Crash evidence for/ });

describe("the crash evidence sheet", () => {
  it("is a labelled dialog that names the container and asks for the evidence of that item", async () => {
    mount();
    const box = await dialog();
    expect(within(box).getByText("Crash evidence for web.1.abc")).toBeTruthy();
    expect(
      within(box).getByText(
        "Saved just before TierPlan removed this container. It is kept with the run and removed with it.",
      ),
    ).toBeTruthy();
    await screen.findByText("Exit code");
    expect(asked).toEqual(["/api/env/e1/janitor/runs/r1/items/3/evidence"]);
    // Focus is inside the sheet.
    expect(box.contains(document.activeElement)).toBe(true);
  });

  it("lists the facts as a description list, with out of memory as Yes or No", async () => {
    mount();
    await screen.findByText("Exit code");
    const facts = document.querySelector("dl") as HTMLElement;
    const read = (label: string) =>
      within(facts).getByText(label, { selector: "dt" }).nextElementSibling?.textContent;
    expect(read("Exit code")).toBe("137");
    expect(read("Out of memory")).toBe("Yes");
    expect(read("Error")).toBe("—");
    expect(read("Image")).toBe("registry.example/web:1.4");
    expect(read("Node")).toBe("node-1");
    expect(read("Started")).toContain("2026");
    expect(read("Finished")).toContain("2026");
    cleanup();

    answer = () =>
      Response.json(
        evidence({ oomKilled: false, error: "task: non-zero exit (1)", startedAt: null }),
      );
    mount();
    await screen.findByText("task: non-zero exit (1)");
    const again = document.querySelector("dl") as HTMLElement;
    expect(
      within(again).getByText("Out of memory", { selector: "dt" }).nextElementSibling?.textContent,
    ).toBe("No");
    expect(
      within(again).getByText("Started", { selector: "dt" }).nextElementSibling?.textContent,
    ).toBe("—");
  });

  it("gives the image digest to copy", async () => {
    mount();
    const digest = await screen.findByRole("region", { name: "the image digest" });
    expect(digest.textContent).toContain("registry.example/web@sha256:abc123");
    expect(within(digest).getByRole("button", { name: "Copy the image digest" })).toBeTruthy();
  });

  it("shows no digest where the node had none", async () => {
    answer = () => Response.json(evidence({ imageDigest: "" }));
    mount();
    await screen.findByText("Exit code");
    expect(screen.queryByRole("region", { name: "the image digest" })).toBeNull();
  });
});

describe("the log tail", () => {
  it("is a focusable region of text named for the container, with a caption that does not over-promise", async () => {
    mount();
    const pre = await screen.findByRole("region", { name: "Last 200 log lines of web.1.abc" });
    expect(pre.tagName).toBe("PRE");
    expect(pre.getAttribute("tabindex")).toBe("0");
    expect(pre.textContent).toBe(
      "2026-11-03T10:04:58Z starting\n2026-11-03T10:04:59Z out of memory",
    );
    expect(screen.getByText("Last 200 log lines", { selector: "h3, h2, h4" })).toBeTruthy();
    expect(
      screen.getByText("Values that look like secrets were masked when this was saved."),
    ).toBeTruthy();
    expect(screen.queryByText(/Secrets and tokens were masked/)).toBeNull();
  });

  it("draws escape codes and markup as the text they are", async () => {
    const hostile = "\u001b[31m<img src=x onerror=alert(1)> <script>alert(2)</script>\u001b[0m";
    answer = () => Response.json(evidence({ logLines: [hostile, "<b>second</b>"] }));
    mount();
    const pre = await screen.findByRole("region", { name: "Last 200 log lines of web.1.abc" });
    expect(pre.textContent).toBe(`${hostile}\n<b>second</b>`);
    expect(pre.querySelector("*")).toBeNull();
    expect(document.querySelector("img, script, b")).toBeNull();
  });

  it("scrolls a very long line inside its own box and never wraps or widens the sheet", async () => {
    answer = () => Response.json(evidence({ logLines: ["x".repeat(10_000)] }));
    mount();
    const pre = await screen.findByRole("region", { name: "Last 200 log lines of web.1.abc" });
    expect(pre.className).toContain("max-h-96");
    expect(pre.className).toContain("overflow-auto");
    expect(pre.className).toContain("whitespace-pre");
    expect(pre.className).not.toContain("whitespace-pre-wrap");
    expect(pre.className).toContain("min-w-0");
    expect(pre.className).toContain("max-w-full");
  });

  it("copies the lines joined by newlines and says Copied", async () => {
    mount();
    await screen.findByRole("region", { name: "Last 200 log lines of web.1.abc" });
    fireEvent.click(screen.getByRole("button", { name: "Copy log" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Copied" })).toBeTruthy());
    expect(writes).toEqual(["2026-11-03T10:04:58Z starting\n2026-11-03T10:04:59Z out of memory"]);
    expect(screen.getByText("Copied", { selector: "[role=status]" })).toBeTruthy();
  });

  it.each([
    ["empty", "This container wrote no logs."],
    ["unavailable", "This container's log could not be read back, so none was saved."],
    [
      "cap_reached",
      "This run had already saved the most evidence it can, so no log was kept for this container.",
    ],
  ] as const)("says %s in words and offers nothing to copy", async (logState, words) => {
    answer = () => Response.json(evidence({ logState, logLines: null }));
    mount();
    expect(await screen.findByText(words)).toBeTruthy();
    expect(screen.queryByRole("region", { name: /Last 200 log lines of/ })).toBeNull();
    expect(screen.queryByRole("button", { name: "Copy log" })).toBeNull();
  });
});

describe("evidence that is not there", () => {
  it("says none was saved for a 404, and does not ask again", async () => {
    answer = () => problem("janitor_evidence_not_found", 404);
    mount();
    expect(await screen.findByText("No crash evidence was saved for this container.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Retry loading evidence" })).toBeNull();
    expect(asked).toHaveLength(1);
  });

  it("says it could not load it for any other failure, and offers to try again", async () => {
    let fail = true;
    answer = () => (fail ? problem("internal", 500) : Response.json(evidence()));
    mount();
    expect(await screen.findByText("Couldn't load the crash evidence.")).toBeTruthy();
    fail = false;
    fireEvent.click(screen.getByRole("button", { name: "Retry loading evidence" }));
    expect(await screen.findByText("Exit code")).toBeTruthy();
  });
});

describe("closing", () => {
  it("reports a close from the button and from Escape", async () => {
    const onClose = mount();
    const box = await dialog();
    fireEvent.click(within(box).getByRole("button", { name: "Close" }));
    expect(onClose).toHaveBeenCalledTimes(1);
    cleanup();
    const again = mount();
    await dialog();
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    expect(again).toHaveBeenCalledTimes(1);
  });
});
