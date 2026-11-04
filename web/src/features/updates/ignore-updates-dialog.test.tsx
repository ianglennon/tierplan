// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, type Mock, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { UpdateInfo } from "@/api/updates";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { IgnoreUpdatesDialog } from "@/features/updates/ignore-updates-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";

// The ignore dialog (plan 12-14, IMG-08, D-15, D-16): the scope choice, the label hint, the one
// accent button, and the toasts.

const ENV = "e1";

const info = (extra: Partial<UpdateInfo> = {}): UpdateInfo => ({
  state: "up_to_date",
  reason: "",
  registry: "docker.io",
  repository: "library/nginx",
  tag: "1.27",
  runningDigest: `sha256:${"a".repeat(64)}`,
  remoteDigest: `sha256:${"a".repeat(64)}`,
  changedPlatforms: [],
  checkedAt: new Date().toISOString(),
  successAt: "",
  note: "",
  task: false,
  ...extra,
});

interface Sent {
  method: string;
  path: string;
  body?: unknown;
}

let sent: Sent[];
let client: QueryClient;
let answerIgnore: () => Response;
let onOpenChange: Mock<(open: boolean) => void>;
let onClosed: Mock<() => void>;

beforeEach(() => {
  sent = [];
  onOpenChange = vi.fn<(open: boolean) => void>();
  onClosed = vi.fn<() => void>();
  answerIgnore = () => Response.json({});
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const body = await req
        .clone()
        .json()
        .catch(() => undefined);
      sent.push({ method: req.method, path, body });
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/updates/ignore` && req.method === "POST") {
        return answerIgnore();
      }
      if (path === `/api/env/${ENV}/updates/ignore` && req.method === "DELETE") {
        return new Response(null, { status: 204 });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  setCsrfToken(null);
});

const writes = () => sent.filter((s) => s.method !== "GET");

async function mount(props: { name?: string; update?: UpdateInfo; imageCount?: number } = {}) {
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <IgnoreUpdatesDialog
          envId={ENV}
          workload={{ kind: "service", name: props.name ?? "web" }}
          update={props.update ?? info()}
          imageCount={props.imageCount}
          open
          onOpenChange={onOpenChange}
          onClosed={onClosed}
        />
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
  await act(async () => {});
}

describe("IgnoreUpdatesDialog", () => {
  it("offers the image in this environment, with its count, and the workload alone", async () => {
    await mount({ imageCount: 3 });
    const dialog = screen.getByRole("dialog");
    expect(dialog.textContent).toContain("TierPlan stops showing updates for this");
    const group = screen.getByRole("radiogroup");
    expect(group).toBeTruthy();
    expect(screen.getByText("Scope")).toBeTruthy();
    expect(await screen.findByRole("radio", { name: /This image in prod/ })).toBeTruthy();
    expect(screen.getByText("nginx:1.27, for all 3 workloads that use it here.")).toBeTruthy();
    expect(screen.getByRole("radio", { name: /Only web/ })).toBeTruthy();
    expect(
      screen.getByText("Other workloads that use the same image still show updates."),
    ).toBeTruthy();
  });

  it("says every workload when the count is not known, and a single one in the singular", async () => {
    await mount();
    expect(screen.getByText("nginx:1.27, for every workload that uses it here.")).toBeTruthy();
    cleanup();
    await mount({ imageCount: 1 });
    expect(screen.getByText("nginx:1.27, for the 1 workload that uses it here.")).toBeTruthy();
  });

  it("ignores the image in this environment when that is chosen", async () => {
    await mount({ imageCount: 3 });
    fireEvent.click(await screen.findByRole("radio", { name: /This image in prod/ }));
    fireEvent.click(screen.getByRole("button", { name: "Ignore updates" }));
    await waitFor(() =>
      expect(writes()).toEqual([
        {
          method: "POST",
          path: `/api/env/${ENV}/updates/ignore`,
          body: { scope: "image", image: "docker.io/library/nginx:1.27" },
        },
      ]),
    );
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
    expect(await screen.findByText("Ignoring updates for nginx:1.27.")).toBeTruthy();
  });

  it("ignores only the workload when that is chosen, which is where it starts", async () => {
    await mount();
    expect(screen.getByRole("radio", { name: /Only web/ }).getAttribute("aria-checked")).toBe(
      "true",
    );
    fireEvent.click(screen.getByRole("button", { name: "Ignore updates" }));
    await waitFor(() =>
      expect(writes()).toEqual([
        {
          method: "POST",
          path: `/api/env/${ENV}/updates/ignore`,
          body: { scope: "workload", kind: "service", name: "web" },
        },
      ]),
    );
    expect(await screen.findByText("Ignoring updates for web.")).toBeTruthy();
  });

  it("offers only the workload for an image that cannot be named", async () => {
    await mount({ update: info({ registry: "", repository: "", tag: "" }) });
    expect(screen.queryByRole("radio", { name: /This image in/ })).toBeNull();
    expect(screen.getByRole("radio", { name: /Only web/ })).toBeTruthy();
  });

  it("takes the choice back when Undo is pressed on the toast, and not before", async () => {
    await mount();
    fireEvent.click(screen.getByRole("button", { name: "Ignore updates" }));
    const undo = await screen.findByRole("button", { name: "Undo" });
    expect(writes().map((w) => w.method)).toEqual(["POST"]);
    fireEvent.click(undo);
    await waitFor(() =>
      expect(writes()).toContainEqual({
        method: "DELETE",
        path: `/api/env/${ENV}/updates/ignore`,
        body: { scope: "workload", kind: "service", name: "web" },
      }),
    );
    expect(await screen.findByText("Updates for web will show again.")).toBeTruthy();
  });

  it("stays open and says it could not, when the server refuses", async () => {
    answerIgnore = () =>
      Response.json({ status: 500, type: "internal", title: "Error" }, { status: 500 });
    await mount();
    fireEvent.click(screen.getByRole("button", { name: "Ignore updates" }));
    expect(await screen.findByText("Couldn't ignore updates for web.")).toBeTruthy();
    expect(onOpenChange).not.toHaveBeenCalledWith(false);
    expect(screen.getByRole("dialog")).toBeTruthy();
  });

  it("shows the label in mono and copies it", async () => {
    const writeText = vi.fn(async () => {});
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    await mount();
    expect(
      screen.getByText(/To keep this in your compose file instead, add the label/).textContent,
    ).toContain("tierplan.updates=ignore to the service.");
    const code = screen.getAllByText("tierplan.updates=ignore");
    expect(code.length).toBeGreaterThan(0);
    for (const el of code)
      expect(el.className + el.parentElement?.className).toContain("font-mono");
    fireEvent.click(screen.getByRole("button", { name: "Copy label" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith("tierplan.updates=ignore"));
  });

  it("puts focus on the title, and Escape cancels and hands focus back", async () => {
    await mount();
    await waitFor(() => expect(document.activeElement?.textContent).toBe("Ignore updates"));
    expect(document.activeElement?.getAttribute("data-slot")).toBe("dialog-title");
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
    expect(writes()).toEqual([]);
  });

  it("cancels with the Cancel button and sends nothing", async () => {
    await mount();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(writes()).toEqual([]);
  });

  it("draws markup in a name and in an image as literal text", async () => {
    await mount({
      name: "<b>web</b>",
      update: info({ repository: "acme/<i>x</i>" }),
      imageCount: 2,
    });
    expect(document.body.querySelector("b")).toBeNull();
    expect(document.body.querySelector("i")).toBeNull();
    expect(screen.getByText("Only <b>web</b>")).toBeTruthy();
    expect(
      screen.getByText("acme/<i>x</i>:1.27, for all 2 workloads that use it here."),
    ).toBeTruthy();
  });
});
