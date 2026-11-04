// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, type Mock, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { UpdateInfo } from "@/api/updates";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { UpdateMenuItems } from "@/features/updates/update-menu-items";
import "@/i18n";
import { makeMe } from "@/test-me";

// The update items of a row menu (plan 12-14, IMG-08, D-10, D-15): Skip this version with an Undo,
// Stop skipping, and who may use them.

const ENV = "e1";
const NEW = `sha256:${"b".repeat(64)}`;
const OLD = `sha256:${"a".repeat(64)}`;

const info = (extra: Partial<UpdateInfo> = {}): UpdateInfo => ({
  state: "update_available",
  reason: "",
  registry: "docker.io",
  repository: "library/nginx",
  tag: "1.27",
  runningDigest: OLD,
  remoteDigest: NEW,
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
let me: ReturnType<typeof makeMe>;
let client: QueryClient;
let answerSkip: () => Response;
let onIgnore: Mock<() => void>;

beforeEach(() => {
  sent = [];
  onIgnore = vi.fn<() => void>();
  me = makeMe({ envs: { [ENV]: "admin" } });
  answerSkip = () => Response.json({ update: info({ state: "skipped" }) });
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
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/updates/skip` && req.method === "POST") return answerSkip();
      if (path === `/api/env/${ENV}/updates/skip` && req.method === "DELETE") {
        return new Response(null, { status: 204 });
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

async function mount(update: UpdateInfo | undefined, name = "web") {
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <DropdownMenu defaultOpen>
          <DropdownMenuTrigger>menu</DropdownMenuTrigger>
          <DropdownMenuContent>
            <UpdateMenuItems
              envId={ENV}
              workload={{ kind: "service", name }}
              update={update}
              onIgnore={onIgnore}
            />
          </DropdownMenuContent>
        </DropdownMenu>
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
  // The role arrives with /api/me.
  await act(async () => {});
  await screen.findByRole("menuitem", { name: "Check now" });
}

describe("UpdateMenuItems: skip this version", () => {
  it("skips the offered version at once, with no confirm, and says which one", async () => {
    await mount(info());
    fireEvent.click(await screen.findByRole("menuitem", { name: "Skip this version" }));
    await waitFor(() =>
      expect(writes()).toEqual([
        {
          method: "POST",
          path: `/api/env/${ENV}/updates/skip`,
          body: { kind: "service", name: "web" },
        },
      ]),
    );
    expect(
      await screen.findByText(
        `Skipped sha256:${"b".repeat(12)} for web. A newer version will show again.`,
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("takes the skip back when Undo is pressed, and only then", async () => {
    await mount(info());
    fireEvent.click(await screen.findByRole("menuitem", { name: "Skip this version" }));
    const undo = await screen.findByRole("button", { name: "Undo" });
    expect(writes().map((w) => w.method)).toEqual(["POST"]);
    fireEvent.click(undo);
    await waitFor(() =>
      expect(writes()).toContainEqual({
        method: "DELETE",
        path: `/api/env/${ENV}/updates/skip`,
        body: { kind: "service", name: "web" },
      }),
    );
    expect(await screen.findByText("Updates for web will show again.")).toBeTruthy();
  });

  it("says it could not skip, and offers no retry, when the server refuses", async () => {
    answerSkip = () =>
      Response.json({ status: 409, type: "nothing_to_skip", title: "Nothing" }, { status: 409 });
    await mount(info());
    fireEvent.click(await screen.findByRole("menuitem", { name: "Skip this version" }));
    expect(await screen.findByText("Couldn't skip this version of web.")).toBeTruthy();
    expect(writes().filter((w) => w.method === "POST")).toHaveLength(1);
  });

  it("offers Stop skipping, and no Skip, for a skipped version; it sends the delete", async () => {
    await mount(info({ state: "skipped" }));
    expect(screen.queryByRole("menuitem", { name: "Skip this version" })).toBeNull();
    fireEvent.click(await screen.findByRole("menuitem", { name: "Stop skipping" }));
    await waitFor(() =>
      expect(writes()).toEqual([
        {
          method: "DELETE",
          path: `/api/env/${ENV}/updates/skip`,
          body: { kind: "service", name: "web" },
        },
      ]),
    );
    expect(await screen.findByText("Updates for web will show again.")).toBeTruthy();
  });

  it("offers only Check now when the row has no update object", async () => {
    await mount(undefined);
    expect(screen.queryByRole("menuitem", { name: "Skip this version" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "Ignore updates…" })).toBeNull();
  });

  it("offers no Skip for a state with no update on offer", async () => {
    await mount(info({ state: "up_to_date" }));
    expect(screen.queryByRole("menuitem", { name: "Skip this version" })).toBeNull();
  });
});

describe("UpdateMenuItems: who may skip", () => {
  it("makes Skip unavailable below Admin with the reason, and sends nothing", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    await mount(info());
    const item = await screen.findByRole("menuitem", { name: "Skip this version" });
    await waitFor(() => expect(item.getAttribute("aria-disabled")).toBe("true"));
    expect(item.getAttribute("aria-describedby")).toBeTruthy();
    expect(document.body.textContent).toContain("Needs Admin on Prod");
    fireEvent.click(item);
    expect(writes()).toEqual([]);
    // Check now stays available to an Operator.
    expect(
      screen.getByRole("menuitem", { name: "Check now" }).getAttribute("aria-disabled"),
    ).toBeNull();
  });
});
