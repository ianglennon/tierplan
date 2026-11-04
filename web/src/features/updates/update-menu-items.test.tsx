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
let onUpdate: Mock<() => void>;
// What the environments list says of the environment's health.
let envStatus: { state: string } | undefined;

beforeEach(() => {
  sent = [];
  onIgnore = vi.fn<() => void>();
  onUpdate = vi.fn<() => void>();
  envStatus = undefined;
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
        return Response.json({
          items: [{ id: ENV, name: "Prod", envType: "swarm", status: envStatus }],
        });
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

async function mount(
  update: UpdateInfo | undefined,
  name = "web",
  extra: { kind?: "service" | "container"; withUpdate?: boolean } = {},
) {
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <DropdownMenu defaultOpen>
          <DropdownMenuTrigger>menu</DropdownMenuTrigger>
          <DropdownMenuContent>
            <UpdateMenuItems
              envId={ENV}
              workload={{ kind: extra.kind ?? "service", name }}
              update={update}
              onIgnore={onIgnore}
              onUpdate={extra.withUpdate === false ? undefined : onUpdate}
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

const held = (extra: Partial<NonNullable<UpdateInfo["ignore"]>> = {}) => ({
  mode: "ignore" as const,
  scope: "workload" as const,
  setBy: "Ada",
  setAt: new Date().toISOString(),
  digest: "",
  ...extra,
});

describe("UpdateMenuItems: ignore updates", () => {
  it("asks the row to open the ignore dialog and sends nothing yet", async () => {
    await mount(info({ state: "up_to_date" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Ignore updates…" }));
    expect(onIgnore).toHaveBeenCalledTimes(1);
    expect(writes()).toEqual([]);
  });

  it("offers Ignore updates beside Stop skipping for a skipped version", async () => {
    await mount(info({ state: "skipped" }));
    expect(screen.getByRole("menuitem", { name: "Ignore updates…" })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: "Stop skipping" })).toBeTruthy();
  });

  it("offers no ignore for an image that is pinned or from no registry", async () => {
    await mount(info({ state: "pinned_in_file" }));
    expect(screen.queryByRole("menuitem", { name: "Ignore updates…" })).toBeNull();
    cleanup();
    await mount(info({ state: "not_from_registry" }));
    expect(screen.queryByRole("menuitem", { name: "Ignore updates…" })).toBeNull();
  });

  it("stops ignoring one workload, with no confirm, and says it will show again", async () => {
    await mount(info({ state: "ignored", ignore: held() }));
    expect(screen.queryByRole("menuitem", { name: "Ignore updates…" })).toBeNull();
    fireEvent.click(await screen.findByRole("menuitem", { name: "Stop ignoring updates" }));
    await waitFor(() =>
      expect(writes()).toEqual([
        {
          method: "DELETE",
          path: `/api/env/${ENV}/updates/ignore`,
          body: { scope: "workload", kind: "service", name: "web" },
        },
      ]),
    );
    expect(await screen.findByText("Updates for web will show again.")).toBeTruthy();
  });

  it("stops ignoring an image by the reference the server matches on", async () => {
    await mount(info({ state: "ignored", ignore: held({ scope: "image" }) }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Stop ignoring updates" }));
    await waitFor(() =>
      expect(writes()).toEqual([
        {
          method: "DELETE",
          path: `/api/env/${ENV}/updates/ignore`,
          body: { scope: "image", image: "docker.io/library/nginx:1.27" },
        },
      ]),
    );
    expect(await screen.findByText("Updates for nginx:1.27 will show again.")).toBeTruthy();
  });

  it("says an ignore by the label cannot be stopped here, and sends nothing", async () => {
    await mount(info({ state: "ignored", ignore: held({ scope: "label", setBy: "" }) }));
    expect(screen.queryByRole("menuitem", { name: "Ignore updates…" })).toBeNull();
    const item = await screen.findByRole("menuitem", { name: "Stop ignoring updates" });
    expect(item.getAttribute("aria-disabled")).toBe("true");
    expect(document.body.textContent).toContain(
      "Ignored by the tierplan.updates=ignore label on the object. Remove the label from your compose file to stop ignoring.",
    );
    fireEvent.click(item);
    expect(writes()).toEqual([]);
  });

  it("makes Ignore and Stop ignoring unavailable below Admin with the reason", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    await mount(info({ state: "up_to_date" }));
    const ignore = await screen.findByRole("menuitem", { name: "Ignore updates…" });
    await waitFor(() => expect(ignore.getAttribute("aria-disabled")).toBe("true"));
    expect(document.body.textContent).toContain("Needs Admin on Prod");
    fireEvent.click(ignore);
    expect(onIgnore).not.toHaveBeenCalled();
    cleanup();
    await mount(info({ state: "ignored", ignore: held() }));
    const stop = await screen.findByRole("menuitem", { name: "Stop ignoring updates" });
    await waitFor(() => expect(stop.getAttribute("aria-disabled")).toBe("true"));
    fireEvent.click(stop);
    expect(writes()).toEqual([]);
  });

  it("says it could not stop ignoring when the server refuses", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        const path = new URL(req.url).pathname;
        if (path === "/api/me") return Response.json(me);
        if (path === "/api/environments") {
          return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
        }
        return Response.json({ status: 404, type: "ignore_not_found" }, { status: 404 });
      }),
    );
    await mount(info({ state: "ignored", ignore: held() }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Stop ignoring updates" }));
    expect(await screen.findByText("Couldn't stop ignoring updates for web.")).toBeTruthy();
  });
});

const disabledItem = async (reason: string) => {
  const item = await screen.findByRole("menuitem", { name: "Update…" });
  await waitFor(() => expect(item.getAttribute("aria-disabled")).toBe("true"));
  expect(document.body.textContent).toContain(reason);
  fireEvent.click(item);
  expect(onUpdate).not.toHaveBeenCalled();
  expect(writes()).toEqual([]);
};

describe("UpdateMenuItems: Update…", () => {
  it("opens the confirm for an Operator when an update is on offer, and sends nothing itself", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    await mount(info());
    const item = await screen.findByRole("menuitem", { name: "Update…" });
    await waitFor(() => expect(item.getAttribute("aria-disabled")).toBeNull());
    fireEvent.click(item);
    expect(onUpdate).toHaveBeenCalledTimes(1);
    expect(writes()).toEqual([]);
  });

  it("is unavailable below Operator, with the reason", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    await mount(info());
    await disabledItem("Needs Operator on Prod");
  });

  it("is unavailable while another operation holds the environment, and says which", async () => {
    await mount(
      info({
        state: "busy",
        holder: { kind: "deploy", by: "ada", stack: "shop", started: new Date().toISOString() },
      }),
    );
    await disabledItem("Waiting for a deploy to finish. shop is deploying.");
  });

  it("is unavailable while this workload is updating, and points to the console", async () => {
    await mount(info({ state: "updating", jobId: "j1" }));
    await disabledItem("An update is already running. Open the console.");
  });

  it.each([
    [
      "pinned_in_file",
      "The image reference in your compose file has a digest, so it never changes. Edit the file to move it.",
    ],
    [
      "not_from_registry",
      "This image didn't come from a registry, so there is nothing to update to.",
    ],
    ["not_checked", "TierPlan has no check result for this image yet. Choose Check now first."],
  ] as const)("is unavailable for %s, with its reason", async (state, reason) => {
    await mount(info({ state }));
    await disabledItem(reason);
  });

  it("is unavailable after a failed check, and says why it failed", async () => {
    await mount(info({ state: "check_failed", reason: "unreachable" }));
    await disabledItem(
      "The last check failed: TierPlan couldn't reach docker.io. Choose Check now to try again.",
    );
  });

  it("is unavailable in an environment that is not reporting", async () => {
    envStatus = { state: "offline" };
    await mount(info());
    await disabledItem("Prod isn't reporting, so TierPlan can't update it.");
  });

  it("is not offered for a version that is up to date, skipped or ignored", async () => {
    for (const state of ["up_to_date", "skipped", "ignored"] as const) {
      await mount(info({ state }));
      expect(screen.queryByRole("menuitem", { name: "Update…" })).toBeNull();
      cleanup();
    }
  });

  it("is not offered for a container, or where the row cannot be updated here", async () => {
    await mount(info(), "web", { kind: "container" });
    expect(screen.queryByRole("menuitem", { name: "Update…" })).toBeNull();
    cleanup();
    await mount(info(), "web", { withUpdate: false });
    expect(screen.queryByRole("menuitem", { name: "Update…" })).toBeNull();
  });
});
