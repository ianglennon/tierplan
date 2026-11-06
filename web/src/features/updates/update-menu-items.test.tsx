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
import { TestRouter } from "@/test-router";

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
  extra: {
    kind?: "service" | "container";
    withUpdate?: boolean;
    project?: string;
    service?: string;
  } = {},
) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
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
                project={extra.project}
                service={extra.service}
              />
            </DropdownMenuContent>
          </DropdownMenu>
          <Toaster />
        </TooltipProvider>
      </TestRouter>
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

  it("is not offered where the row cannot be updated here", async () => {
    await mount(info(), "web", { withUpdate: false });
    expect(screen.queryByRole("menuitem", { name: "Update…" })).toBeNull();
  });
});

describe("UpdateMenuItems: Update anyway… (D-14)", () => {
  const failure = (extra: Partial<NonNullable<UpdateInfo["failure"]>> = {}) => ({
    digest: NEW,
    reason: "timeout",
    at: new Date().toISOString(),
    rollbackFailed: false,
    jobId: "j1",
    ...extra,
  });
  const anyway = () => screen.findByRole("menuitem", { name: "Update anyway…" });

  it("replaces Update… after a rollback, and opens the confirm without sending anything", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    await mount(info({ state: "rolled_back", failure: failure() }));
    const item = await anyway();
    expect(screen.queryByRole("menuitem", { name: "Update…" })).toBeNull();
    await waitFor(() => expect(item.getAttribute("aria-disabled")).toBeNull());
    fireEvent.click(item);
    expect(onUpdate).toHaveBeenCalledTimes(1);
    expect(writes()).toEqual([]);
  });

  it("is offered for a version whose image could not be pulled, since a registry glitch is recoverable", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    await mount(info({ state: "rolled_back", failure: failure({ reason: "pull_failed" }) }));
    const item = await anyway();
    await waitFor(() => expect(item.getAttribute("aria-disabled")).toBeNull());
    fireEvent.click(item);
    expect(onUpdate).toHaveBeenCalledTimes(1);
  });

  it("is offered after a rollback that failed too", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    await mount(info({ state: "rollback_failed", failure: failure({ rollbackFailed: true }) }));
    expect(await anyway()).toBeTruthy();
  });

  it("is offered for a container that was rolled back", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    await mount(info({ state: "rolled_back", failure: failure() }), "cache", {
      kind: "container",
      project: "shop",
      service: "",
    });
    fireEvent.click(await anyway());
    expect(onUpdate).toHaveBeenCalledTimes(1);
  });

  it("is unavailable below Operator, with the reason", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    await mount(info({ state: "rolled_back", failure: failure() }));
    const item = await anyway();
    await waitFor(() => expect(item.getAttribute("aria-disabled")).toBe("true"));
    expect(document.body.textContent).toContain("Needs Operator on Prod");
    fireEvent.click(item);
    expect(onUpdate).not.toHaveBeenCalled();
  });

  it("is not offered when the version is up to date, and plain Update… stays for an update on offer", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    await mount(info({ state: "up_to_date" }));
    expect(screen.queryByRole("menuitem", { name: "Update anyway…" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "Update…" })).toBeNull();
    cleanup();
    await mount(info());
    expect(await screen.findByRole("menuitem", { name: "Update…" })).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Update anyway…" })).toBeNull();
  });
});

describe("UpdateMenuItems: a container", () => {
  const container = { kind: "container" as const, project: "shop", service: "" };

  it("offers Update… to an Operator and opens the confirm, sending nothing itself", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    await mount(info(), "cache", container);
    const item = await screen.findByRole("menuitem", { name: "Update…" });
    await waitFor(() => expect(item.getAttribute("aria-disabled")).toBeNull());
    fireEvent.click(item);
    expect(onUpdate).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("menuitem", { name: "Adopt to update…" })).toBeNull();
    expect(writes()).toEqual([]);
  });

  it("is unavailable below Operator, with the reason", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    await mount(info(), "cache", container);
    await disabledItem("Needs Operator on Prod");
  });

  it("reads Adopt to update… for a container of a Compose project TierPlan does not manage, and goes to its adopt flow", async () => {
    await mount(info({ refusal: "compose_unmanaged" }), "shop-web-1", container);
    const link = await screen.findByRole("menuitem", { name: "Adopt to update…" });
    await waitFor(() =>
      expect(link.getAttribute("href")).toBe(`/env/${ENV}/unmanaged-stacks/shop/adopt`),
    );
    expect(screen.queryByRole("menuitem", { name: "Update…" })).toBeNull();
    fireEvent.click(link);
    expect(onUpdate).not.toHaveBeenCalled();
    expect(writes()).toEqual([]);
  });

  it("encodes a project name with a slash in the adopt address", async () => {
    await mount(info({ refusal: "compose_unmanaged" }), "x-1", { ...container, project: "a/b" });
    const link = await screen.findByRole("menuitem", { name: "Adopt to update…" });
    await waitFor(() => expect(link.getAttribute("href")).toContain("a%2Fb"));
  });

  it("makes Adopt to update… unavailable with the role it takes below Env Admin", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    await mount(info({ refusal: "compose_unmanaged" }), "shop-web-1", container);
    const item = await screen.findByRole("menuitem", { name: "Adopt to update…" });
    await waitFor(() => expect(item.getAttribute("aria-disabled")).toBe("true"));
    expect(item.getAttribute("href")).toBeNull();
    expect(document.body.textContent).toContain("Needs Admin on Prod");
  });

  it("shows no Adopt to update… when the container is up to date", async () => {
    await mount(
      info({ state: "up_to_date", refusal: "compose_unmanaged" }),
      "shop-web-1",
      container,
    );
    expect(screen.queryByRole("menuitem", { name: "Adopt to update…" })).toBeNull();
  });

  it("makes Update… of a swarm task container unavailable and names the service to update instead", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    await mount(info({ refusal: "task_container" }), "web.1.abc", { ...container, service: "web" });
    await disabledItem("Update the service web instead. Its tasks are replaced for you.");
  });
});

describe("UpdateMenuItems: the fleet is read again", () => {
  // The Updates tab and the Overview tiles sit on the same page as the row menu. Nothing observes
  // the fleet key here, so a read that is marked invalid is a read the page would make again.
  const fleetKey = ["fleet", "updates", "updates", []];

  it.each([
    ["Skip this version", () => info()],
    ["Stop skipping", () => info({ state: "skipped" })],
    ["Stop ignoring updates", () => info({ state: "ignored", ignore: held() })],
    ["Check now", () => info()],
  ])("marks the fleet's reads stale after %s", async (item, update) => {
    client.setQueryData(fleetKey, { groups: [] });
    await mount(update());
    expect(client.getQueryState(fleetKey)?.isInvalidated).toBe(false);
    fireEvent.click(await screen.findByRole("menuitem", { name: item }));
    await waitFor(() => expect(client.getQueryState(fleetKey)?.isInvalidated).toBe(true));
  });
});
