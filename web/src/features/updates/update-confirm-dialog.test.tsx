// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useRouterState } from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ComponentProps } from "react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, type Mock, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { FleetSelected } from "@/api/fleet";
import type { UpdatePreview, UpdatePreviewItem } from "@/api/updates";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import {
  type FleetUnavailable,
  FleetUpdateConfirmDialog,
  UpdateConfirmDialog,
} from "@/features/updates/update-confirm-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The compact update confirm (plan 12-17, IMG-06, D-05): the rows, the pre-flight warnings, the
// commit that re-checks, a digest that moved, a lock taken between the confirm and the click.

const ENV = "e1";
const A = `sha256:${"a".repeat(64)}`;
const B = `sha256:${"b".repeat(64)}`;
const C = `sha256:${"c".repeat(64)}`;

const item = (extra: Partial<UpdatePreviewItem> = {}): UpdatePreviewItem => ({
  kind: "service",
  name: "web",
  image: "nginx:1.27",
  stack: "",
  fromDigest: A,
  toDigest: B,
  platforms: [],
  changedPlatforms: [],
  ...extra,
});

const preview = (extra: Partial<UpdatePreview> = {}): UpdatePreview => ({
  items: [item()],
  warnings: [
    { code: "downtime", workload: "web" },
    { code: "no_healthcheck", workload: "web" },
  ],
  leftOut: [],
  offered: true,
  refusal: "",
  ...extra,
});

interface Sent {
  method: string;
  path: string;
  body?: unknown;
}

let sent: Sent[];
// The query string of each preview read, for the tests that check which kind was asked for.
let previewQueries: string[];
let client: QueryClient;
let answerPreview: () => Response | Promise<Response>;
let answerRun: () => Response | Promise<Response>;
let onClose: Mock<() => void>;

beforeEach(() => {
  sent = [];
  previewQueries = [];
  onClose = vi.fn<() => void>();
  answerPreview = () => Response.json(preview());
  answerRun = () => Response.json({ jobId: "j1" });
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
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "operator" } }));
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/updates/preview`) {
        previewQueries.push(new URL(req.url).search);
        return answerPreview();
      }
      if (path === `/api/env/${ENV}/updates/run` && req.method === "POST") return answerRun();
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  setCsrfToken(null);
  client.clear();
});

function Where() {
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  return <output data-testid="where">{pathname}</output>;
}

function mount(props: Partial<ComponentProps<typeof UpdateConfirmDialog>> = {}) {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <UpdateConfirmDialog
            envId={ENV}
            envName="Prod"
            workload={{ kind: "service", name: "web" }}
            onClose={onClose}
            {...props}
          />
          <Toaster />
          <Where />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const runs = () => sent.filter((s) => s.method === "POST" && s.path.endsWith("/updates/run"));
// commit is the accent button once the preview has been read (the button is there while it loads, but
// does nothing yet).
async function commit() {
  await waitFor(() => expect(document.querySelector("[data-slot=update-row]")).toBeTruthy());
  return screen.findByRole("button", { name: "Update web" });
}

describe("UpdateConfirmDialog: what it shows", () => {
  it("shows the title, the row, the warnings and the note, and sends nothing but the preview", async () => {
    mount();
    expect(await screen.findByRole("dialog")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Update web" })).toBeTruthy();
    expect(
      screen.getByText(
        "TierPlan changes only the image of this service and keeps its other settings.",
      ),
    ).toBeTruthy();
    expect(await screen.findByText("nginx:1.27")).toBeTruthy();
    expect(screen.getByRole("button", { name: /sha256:a{12}/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /sha256:b{12}/ })).toBeTruthy();
    expect(screen.getByText("Before you update")).toBeTruthy();
    expect(
      screen.getByText(
        "web runs 1 replica and Docker stops the old task before it starts the new one, so it will be down for a short time.",
      ),
    ).toBeTruthy();
    expect(screen.getByText(/web's image has no health check/)).toBeTruthy();
    expect(
      screen.getByText(
        "If the update fails, TierPlan puts the previous version back on its own and records both in the audit log.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeTruthy();
    expect((await commit()).getAttribute("aria-disabled")).toBeNull();
    expect(runs()).toEqual([]);
  });

  it("renders the title at once and skeleton rows while the preview loads", async () => {
    answerPreview = () => new Promise<Response>(() => {});
    mount();
    expect(await screen.findByRole("heading", { name: "Update web" })).toBeTruthy();
    expect(screen.getByTestId("update-skeleton")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Update web" }).getAttribute("aria-disabled")).toBe(
      "true",
    );
  });

  it("folds warnings past the third into a disclosure", async () => {
    answerPreview = () =>
      Response.json(
        preview({
          warnings: [
            { code: "downtime", workload: "web" },
            { code: "no_healthcheck", workload: "web" },
            { code: "host_ports", workload: "web" },
            { code: "volumes", workload: "web" },
            { code: "previous_failure", workload: "web" },
          ],
        }),
      );
    mount();
    const more = await screen.findByRole("button", { name: "Show 2 more warnings" });
    expect(screen.queryByText(/mounts a volume/)).toBeNull();
    fireEvent.click(more);
    expect(await screen.findByText(/web mounts a volume/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Show fewer warnings" })).toBeTruthy();
  });

  it("shows platform chips only for a multi-platform image or a partial change", async () => {
    answerPreview = () =>
      Response.json(
        preview({
          items: [
            item({ platforms: ["linux/amd64", "linux/arm64"], changedPlatforms: ["linux/arm64"] }),
          ],
        }),
      );
    mount();
    await commit();
    expect(screen.getByText("linux/arm64")).toBeTruthy();
    expect(screen.queryByText("linux/amd64")).toBeNull();
  });

  it("shows no platform chips for a single-platform image", async () => {
    answerPreview = () => Response.json(preview({ items: [item({ platforms: ["linux/amd64"] })] }));
    mount();
    await commit();
    expect(screen.queryByText("linux/amd64")).toBeNull();
  });

  it("wraps a 200-character repository path with break-all, so it cannot widen the dialog", async () => {
    const image = `registry.example.com/${"team/".repeat(40)}app:1.0`;
    answerPreview = () => Response.json(preview({ items: [item({ image })] }));
    mount();
    await commit();
    const text = screen.getByText(image);
    expect(text.className).toContain("break-all");
    expect(text.className).toContain("font-mono");
    expect(screen.getByRole("dialog").className).toContain("sm:max-w-xl");
  });

  it("explains an update that is not offered and leaves the commit unavailable", async () => {
    answerPreview = () =>
      Response.json(
        preview({ items: [], warnings: [], offered: false, refusal: "nothing_to_update" }),
      );
    mount();
    expect(await screen.findByText("web already runs the newest version.")).toBeTruthy();
    const button = screen.getByRole("button", { name: "Update web" });
    expect(button.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(button);
    expect(runs()).toEqual([]);
  });

  it("says who holds the environment and leaves the commit unavailable", async () => {
    answerPreview = () =>
      Response.json(
        preview({
          lock: { kind: "deploy", by: "ada", stack: "shop", started: new Date().toISOString() },
        }),
      );
    mount();
    expect(
      await screen.findByText(/Another stack operation is running on Prod: shop/),
    ).toBeTruthy();
    const button = await commit();
    expect(button.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(button);
    expect(runs()).toEqual([]);
  });

  it("offers a retry when the preview cannot be read", async () => {
    answerPreview = () => Response.json({ status: 500, title: "x" }, { status: 500 });
    mount();
    expect(await screen.findByText("Couldn't load what this update changes.")).toBeTruthy();
    answerPreview = () => Response.json(preview());
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(screen.queryByText("Couldn't load what this update changes.")).toBeNull(),
    );
  });
});

describe("UpdateConfirmDialog: the commit", () => {
  it("sends the digest the confirm showed, closes and opens the console", async () => {
    mount();
    fireEvent.click(await commit());
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
    expect(runs()).toEqual([
      {
        method: "POST",
        path: `/api/env/${ENV}/updates/run`,
        body: { kind: "service", name: "web", expect: { web: B } },
      },
    ]);
    await waitFor(() =>
      expect(screen.getByTestId("where").textContent).toBe(`/env/${ENV}/update-jobs/j1`),
    );
  });

  it("reads Checking… with aria-busy while the server re-checks, and sends one request", async () => {
    let release: (r: Response) => void = () => {};
    answerRun = () => new Promise<Response>((resolve) => (release = resolve));
    mount();
    const button = await commit();
    fireEvent.click(button);
    fireEvent.click(button);
    const busy = await screen.findByRole("button", { name: "Checking…" });
    expect(busy.getAttribute("aria-busy")).toBe("true");
    expect(runs()).toHaveLength(1);
    await act(async () => release(Response.json({ jobId: "j1" })));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });

  it("shows A newer version appeared when the digest moved, refreshes the rows and asks again", async () => {
    answerRun = () =>
      Response.json(
        {
          type: "update_moved",
          status: 409,
          title: "Conflict",
          errors: [
            {
              location: "items",
              message: "web",
              value: { service: "web", fromDigest: A, toDigest: C },
            },
          ],
        },
        { status: 409 },
      );
    mount();
    fireEvent.click(await commit());
    // The preview now reports the newer digest.
    answerPreview = () => Response.json(preview({ items: [item({ toDigest: C })] }));
    const alert = await screen.findByText("A newer version appeared");
    expect(alert.closest("[role=alert]")).toBeTruthy();
    expect(screen.getByText(/web changed again while you were looking/)).toBeTruthy();
    await waitFor(() => expect(document.activeElement).toBe(alert));
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.getByTestId("where").textContent).toBe("/");
    // The rows now show the newer digest.
    expect(await screen.findByRole("button", { name: /sha256:c{12}/ })).toBeTruthy();
    // The commit is back to its idle label and, pressed again, sends the newer digest.
    answerRun = () => Response.json({ jobId: "j2" });
    fireEvent.click(await commit());
    await waitFor(() => expect(runs()).toHaveLength(2));
    expect(runs()[1]?.body).toEqual({ kind: "service", name: "web", expect: { web: C } });
    await waitFor(() =>
      expect(screen.getByTestId("where").textContent).toBe(`/env/${ENV}/update-jobs/j2`),
    );
  });

  // The 409 a press gets when another operation holds the environment.
  const lockedAnswer = () =>
    Response.json(
      {
        type: "operation_locked",
        status: 409,
        title: "Conflict",
        errors: [
          { location: "stack", message: "shop" },
          { location: "job", message: "d1" },
          { location: "started", message: new Date().toISOString() },
          { location: "stackId", message: "s1" },
        ],
      },
      { status: 409 },
    );
  const held = () =>
    preview({
      lock: { kind: "deploy", by: "ana", stack: "shop", started: new Date().toISOString() },
    });

  it("shows the lock and the toast when it was taken between the confirm and the click", async () => {
    answerRun = () => {
      // The read that follows the refusal still finds the lock held.
      answerPreview = () => Response.json(held());
      return lockedAnswer();
    };
    mount();
    fireEvent.click(await commit());
    expect(
      await screen.findByText(/Another stack operation is running on Prod: shop/),
    ).toBeTruthy();
    expect(
      await screen.findByText("A deploy is running in Prod. Try again when it finishes."),
    ).toBeTruthy();
    expect(onClose).not.toHaveBeenCalled();
    expect((await commit()).getAttribute("aria-disabled")).toBe("true");
  });

  it("lets the person press again once a fresh read shows the lock is gone", async () => {
    // The refusal came while the other operation ran. The read that follows is held until the test
    // says the operation has finished, so the refusal is on screen first.
    let finished: (r: Response) => void = () => {};
    const read = new Promise<Response>((resolve) => (finished = resolve));
    answerRun = () => {
      answerPreview = () => read;
      return lockedAnswer();
    };
    mount();
    fireEvent.click(await commit());
    expect(
      await screen.findByText(/Another stack operation is running on Prod: shop/),
    ).toBeTruthy();

    finished(Response.json(preview()));
    await waitFor(() =>
      expect(screen.queryByText(/Another stack operation is running on Prod/)).toBeNull(),
    );
    answerRun = () => Response.json({ jobId: "j2" });
    fireEvent.click(await commit());
    await waitFor(() => expect(runs()).toHaveLength(2));
    await waitFor(() =>
      expect(screen.getByTestId("where").textContent).toBe(`/env/${ENV}/update-jobs/j2`),
    );
  });

  it("says nothing was changed when the server cannot be reached, and does not retry by itself", async () => {
    answerRun = () => Response.json({ status: 500, title: "x" }, { status: 500 });
    mount();
    fireEvent.click(await commit());
    expect(
      await screen.findByText("Nothing was changed. Check the connection and try again."),
    ).toBeTruthy();
    expect(runs()).toHaveLength(1);
  });

  it("closes on Cancel without sending anything", async () => {
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(runs()).toEqual([]);
  });
});

describe("UpdateConfirmDialog: the stack scope", () => {
  const stackItems = (n: number) =>
    Array.from({ length: n }, (_, i) =>
      item({ name: `shop_s${i}`, image: `acme/s${i}:1`, stack: "shop" }),
    );
  const stackPreview = (n: number, extra: Partial<UpdatePreview> = {}) =>
    preview({ items: stackItems(n), warnings: [], ...extra });
  const stackProps = { workload: { kind: "stack" as const, name: "shop" } };
  const stackCommit = (name: string) => screen.findByRole("button", { name });
  const rowsShown = (n: number) =>
    waitFor(() => expect(document.querySelectorAll("[data-slot=update-row]").length).toBe(n));

  it("asks the preview for kind stack and sends kind stack", async () => {
    answerPreview = () => Response.json(stackPreview(2));
    answerRun = () => Response.json({ jobId: "j2", stackId: "s9" });
    mount(stackProps);
    await rowsShown(2);
    expect(sent.find((s) => s.path.endsWith("/updates/preview"))).toBeTruthy();
    fireEvent.click(await stackCommit("Update 2 services"));
    await waitFor(() => expect(runs()).toHaveLength(1));
    expect(runs()[0]?.body).toEqual({
      kind: "stack",
      name: "shop",
      expect: { shop_s0: B, shop_s1: B },
    });
    await waitFor(() =>
      expect(screen.getByTestId("where").textContent).toBe("/env/e1/stacks/s9/deploys/j2"),
    );
  });

  it("reads Update 1 service for one and Update 12 services for twelve", async () => {
    answerPreview = () => Response.json(stackPreview(1));
    mount(stackProps);
    await rowsShown(1);
    expect(await stackCommit("Update 1 service")).toBeTruthy();
    cleanup();
    answerPreview = () => Response.json(stackPreview(12));
    mount(stackProps);
    await rowsShown(12);
    expect(await stackCommit("Update 12 services")).toBeTruthy();
  });

  it("names how many are left out, in the singular and the plural", async () => {
    answerPreview = () =>
      Response.json(stackPreview(1, { leftOut: [{ name: "shop_db", state: "skipped" }] }));
    mount(stackProps);
    expect(await screen.findByText("1 is skipped or ignored and won't change.")).toBeTruthy();
    cleanup();
    answerPreview = () =>
      Response.json(
        stackPreview(1, {
          leftOut: [
            { name: "shop_db", state: "skipped" },
            { name: "shop_cache", state: "ignored" },
            { name: "shop_q", state: "ignored" },
          ],
        }),
      );
    mount(stackProps);
    expect(await screen.findByText("3 are skipped or ignored and won't change.")).toBeTruthy();
  });

  it("offers Review everything first only when the host gives it, and it calls it", async () => {
    answerPreview = () => Response.json(stackPreview(1));
    const onReview = vi.fn<() => void>();
    mount({ ...stackProps, onReview });
    fireEvent.click(await screen.findByRole("button", { name: "Review everything first" }));
    expect(onReview).toHaveBeenCalledTimes(1);
    expect(runs()).toEqual([]);
    cleanup();
    mount(stackProps);
    await rowsShown(1);
    expect(screen.queryByRole("button", { name: "Review everything first" })).toBeNull();
    cleanup();
    mount({ onReview });
    await rowsShown(1);
    expect(screen.queryByRole("button", { name: "Review everything first" })).toBeNull();
  });

  it("sends a service of a managed stack to the stack's console and says it is a revision of the stack", async () => {
    answerRun = () => Response.json({ jobId: "j5", stackId: "s1" });
    mount({ managedStack: "shop" });
    expect(
      screen.getByText(
        "TierPlan pulls the new image and updates the service in place. This is saved as a new revision of shop, so you can roll back from History.",
      ),
    ).toBeTruthy();
    fireEvent.click(await commit());
    await waitFor(() =>
      expect(screen.getByTestId("where").textContent).toBe("/env/e1/stacks/s1/deploys/j5"),
    );
  });

  it("answers a stack that must be adopted first with the adopt link, and no commit", async () => {
    answerPreview = () =>
      Response.json(stackPreview(0, { items: [], offered: false, refusal: "adopt_first" }));
    mount(stackProps);
    const link = await screen.findByRole("link", { name: "Adopt stack…" });
    expect(link.getAttribute("href")).toBe("/env/e1/unmanaged-stacks/shop/adopt");
    expect(
      screen.getByText(
        "TierPlan doesn't manage this stack yet. Adopt it first so an update has a revision to roll back to.",
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Update/ })).toBeNull();
  });

  it("shows the adopt link too when the press is refused 409 adopt_first", async () => {
    answerPreview = () => Response.json(stackPreview(1));
    answerRun = () =>
      Response.json(
        { type: "adopt_first", title: "Conflict", status: 409, detail: "adopt it" },
        { status: 409 },
      );
    mount(stackProps);
    await rowsShown(1);
    fireEvent.click(await stackCommit("Update 1 service"));
    expect(await screen.findByRole("link", { name: "Adopt stack…" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Update 1/ })).toBeNull();
  });

  it("says Nothing to update and drops the commit when the press is refused 409 nothing_to_update", async () => {
    answerPreview = () => Response.json(stackPreview(1));
    answerRun = () =>
      Response.json(
        { type: "nothing_to_update", title: "Conflict", status: 409, detail: "none" },
        { status: 409 },
      );
    mount(stackProps);
    await rowsShown(1);
    fireEvent.click(await stackCommit("Update 1 service"));
    expect(await screen.findByText("Nothing to update")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Update 1/ })).toBeNull();
    expect(runs()).toHaveLength(1);
  });

  it("draws a service name with markup as text", async () => {
    answerPreview = () =>
      Response.json(stackPreview(1, { items: [item({ name: "<img src=x onerror=alert(1)>" })] }));
    mount(stackProps);
    expect(await screen.findByText("<img src=x onerror=alert(1)>")).toBeTruthy();
    expect(document.querySelector("img")).toBeNull();
  });
});

describe("UpdateConfirmDialog: the container scope", () => {
  const containerProps = { workload: { kind: "container" as const, name: "cache" } };
  const containerItem = item({ kind: "container", name: "cache", image: "redis:7" });
  const containerPreview = (extra: Partial<UpdatePreview> = {}) =>
    preview({
      items: [containerItem],
      // The server lists the downtime beside the others; the confirm puts it first.
      warnings: [
        { code: "no_healthcheck", workload: "cache" },
        { code: "container_downtime", workload: "cache" },
      ],
      ...extra,
    });
  const rowShown = () =>
    waitFor(() => expect(document.querySelector("[data-slot=update-row]")).toBeTruthy());

  it("describes the recreate, warns of the downtime first and commits as Update {name}", async () => {
    answerPreview = () => Response.json(containerPreview());
    mount(containerProps);
    expect(await screen.findByRole("heading", { name: "Update cache" })).toBeTruthy();
    expect(
      screen.getByText(
        "TierPlan recreates cache with the same settings and the new image. It keeps the old container until the new one is running, then removes it.",
      ),
    ).toBeTruthy();
    await rowShown();
    const warnings = document.querySelectorAll("section li");
    expect(warnings[0]?.textContent).toBe(
      "A container can't be replaced without stopping it, so cache will be down for a short time.",
    );
    expect(warnings[1]?.textContent).toMatch(/cache's image has no health check/);
    expect(await screen.findByRole("button", { name: "Update cache" })).toBeTruthy();
    expect(previewQueries[0]).toContain("kind=container");
    expect(previewQueries[0]).toContain("name=cache");
  });

  it("posts kind container with the digest shown, and opens the update console", async () => {
    answerPreview = () => Response.json(containerPreview());
    mount(containerProps);
    await rowShown();
    fireEvent.click(await screen.findByRole("button", { name: "Update cache" }));
    await waitFor(() => expect(runs()).toHaveLength(1));
    expect(runs()[0]?.body).toEqual({ kind: "container", name: "cache", expect: { cache: B } });
    await waitFor(() =>
      expect(screen.getByTestId("where").textContent).toBe(`/env/${ENV}/update-jobs/j1`),
    );
  });

  it("lands on the stack's deploy console for a container of a stack TierPlan manages", async () => {
    answerPreview = () => Response.json(containerPreview({ warnings: [] }));
    answerRun = () => Response.json({ jobId: "j7", stackId: "s3" });
    mount({ ...containerProps, managedStack: "shop" });
    await rowShown();
    expect(
      screen.getByText(
        /This is saved as a new revision of shop, so you can roll back from History/,
      ),
    ).toBeTruthy();
    fireEvent.click(await screen.findByRole("button", { name: "Update cache" }));
    await waitFor(() =>
      expect(screen.getByTestId("where").textContent).toBe("/env/e1/stacks/s3/deploys/j7"),
    );
  });

  it("says a task container is updated through its service when the server refuses", async () => {
    answerPreview = () =>
      Response.json(containerPreview({ items: [], offered: false, refusal: "task_container" }));
    mount(containerProps);
    expect(
      await screen.findByText(
        "cache is a task of a service. Update the service instead; its tasks are replaced for you.",
      ),
    ).toBeTruthy();
  });
});

// Update anyway (plan 12-25, D-14): a version whose update failed before is tried again on purpose.
describe("UpdateConfirmDialog: Update anyway", () => {
  const failedBefore = (reason: string, extra: Record<string, unknown> = {}) => ({
    code: "previous_failure" as const,
    workload: "web",
    reason,
    at: new Date(Date.now() - 2 * 3_600_000).toISOString(),
    ...extra,
  });
  const anywayPreview = (reason: string, others: UpdatePreview["warnings"] = []) =>
    preview({ warnings: [...(others ?? []), failedBefore(reason)] });
  const anywayButton = async () => {
    await waitFor(() => expect(document.querySelector("[data-slot=update-row]")).toBeTruthy());
    return screen.findByRole("button", { name: "Update web anyway" });
  };

  it("leads with why this version failed before and when, and names the button for what it does", async () => {
    answerPreview = () => Response.json(anywayPreview("timeout"));
    mount();
    expect(await screen.findByRole("heading", { name: "Update web anyway" })).toBeTruthy();
    expect(
      await screen.findByText(
        "This version failed before: web didn't finish updating in time. It was rolled back 2 h ago.",
      ),
    ).toBeTruthy();
    expect(await anywayButton()).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Update web" })).toBeNull();
  });

  it("shows the earlier failure open, ahead of every other warning, even when the rest fold", async () => {
    answerPreview = () =>
      Response.json(
        anywayPreview("didnt_start", [
          { code: "downtime", workload: "web" },
          { code: "no_healthcheck", workload: "web" },
          { code: "host_ports", workload: "web" },
          { code: "volumes", workload: "web" },
        ]),
      );
    mount();
    const first = await screen.findByText(/This version failed before: web didn't start/);
    const lines = Array.from(
      document.querySelectorAll("section[aria-label='Before you update'] ul > li"),
    ).map((li) => li.textContent ?? "");
    expect(lines[0]).toBe(first.textContent);
    expect(await screen.findByRole("button", { name: "Show 2 more warnings" })).toBeTruthy();
  });

  it("posts force true from this form, and only from this form", async () => {
    answerPreview = () => Response.json(anywayPreview("unhealthy"));
    mount();
    fireEvent.click(await anywayButton());
    await waitFor(() => expect(runs()).toHaveLength(1));
    expect(runs()[0]?.body).toEqual({
      kind: "service",
      name: "web",
      expect: { web: B },
      force: true,
    });
    await waitFor(() =>
      expect(screen.getByTestId("where").textContent).toBe(`/env/${ENV}/update-jobs/j1`),
    );
  });

  it("sends no force at all for an ordinary update", async () => {
    mount();
    fireEvent.click(await commit());
    await waitFor(() => expect(runs()).toHaveLength(1));
    expect(runs()[0]?.body).not.toHaveProperty("force");
  });

  it("works for a version whose image could not be pulled, and says nothing was changed, never that it was rolled back", async () => {
    answerPreview = () => Response.json(anywayPreview("pull_failed"));
    mount();
    const text = await screen.findByText(
      "This version failed before: TierPlan couldn't pull the image. Nothing was changed. That was 2 h ago.",
    );
    expect(text.textContent).not.toMatch(/rolled back/i);
    expect(
      document.querySelector("section[aria-label='Before you update']")?.textContent,
    ).not.toMatch(/rolled back/i);
    fireEvent.click(await anywayButton());
    await waitFor(() => expect(runs()).toHaveLength(1));
    expect(runs()[0]?.body).toMatchObject({ name: "web", force: true });
  });

  it("turns an ordinary Update the server refuses as failed before into the Update anyway form, with the warning", async () => {
    answerRun = () =>
      Response.json(
        {
          type: "previously_failed",
          status: 409,
          title: "Conflict",
          errors: [{ location: "reason", message: "timeout" }],
        },
        { status: 409 },
      );
    // The preview reports the earlier failure only once the server has refused the press.
    answerPreview = () => Response.json(runs().length > 0 ? anywayPreview("timeout") : preview());
    mount();
    fireEvent.click(await commit());
    await waitFor(() => expect(runs()).toHaveLength(1));
    expect(runs()[0]?.body).not.toHaveProperty("force");
    expect(await screen.findByRole("heading", { name: "Update web anyway" })).toBeTruthy();
    expect(
      await screen.findByText(/This version failed before: web didn't finish updating/),
    ).toBeTruthy();
    // Nothing else is said about it: no error alert, and the dialog stays open.
    expect(screen.queryByText("Couldn't start the update.")).toBeNull();
    expect(onClose).not.toHaveBeenCalled();
    answerRun = () => Response.json({ jobId: "j2" });
    fireEvent.click(await anywayButton());
    await waitFor(() => expect(runs()).toHaveLength(2));
    expect(runs()[1]?.body).toMatchObject({ force: true });
  });

  it("never offers it for a stack, which does not retry a failed version", async () => {
    answerPreview = () =>
      Response.json(
        preview({
          items: [item({ name: "web" }), item({ name: "db" })],
          warnings: [failedBefore("timeout")],
        }),
      );
    mount({ workload: { kind: "stack", name: "shop" } });
    expect(await screen.findByRole("heading", { name: "Update stack shop" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: /anyway/ })).toBeNull();
    const button = await screen.findByRole("button", { name: "Update 2 services" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBeNull());
    fireEvent.click(button);
    await waitFor(() => expect(runs()).toHaveLength(1));
    expect(runs()[0]?.body).not.toHaveProperty("force");
  });

  it("draws a name with markup in the warning as text", async () => {
    answerPreview = () =>
      Response.json(
        preview({ warnings: [failedBefore("didnt_start", { workload: "<b>web</b>" })] }),
      );
    mount();
    expect(
      await screen.findByText(/This version failed before: <b>web<\/b> didn't start/),
    ).toBeTruthy();
    expect(document.querySelector("section b")).toBeNull();
  });
});

// The confirm of "Update {n} selected" (plan 12-31, D-11): the ticked workloads by environment.
describe("FleetUpdateConfirmDialog", () => {
  const selected = (envId: string, envName: string, name: string, extra = {}): FleetSelected => ({
    envId,
    envName,
    type: "swarm",
    item: {
      kind: "service",
      id: `${envId}-${name}`,
      name,
      stack: "",
      tierplanStackId: "",
      image: "nginx:1.27",
      fromDigest: A,
      toDigest: B,
      changedPlatforms: [],
      selectable: true,
      update: {} as FleetSelected["item"]["update"],
      ...extra,
    },
  });
  let started: Mock;

  function mountFleet(
    items: FleetSelected[],
    unavailable: Map<string, FleetUnavailable> = new Map(),
  ) {
    started = vi.fn();
    return render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <FleetUpdateConfirmDialog
              items={items}
              unavailable={unavailable}
              onClose={onClose}
              onStarted={started}
            />
            <Toaster />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
  }

  it("reads Update 1 item for one workload, and groups it under its environment", async () => {
    mountFleet([selected("e1", "prod", "web")]);
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Update 1 item", { selector: "h2" })).toBeTruthy();
    expect(within(dialog).getByRole("region", { name: "prod" })).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Update 1 item" })).toBeTruthy();
  });

  it("draws a name as text, never as markup", async () => {
    mountFleet([selected("e1", "<b>prod</b>", "<img src=x onerror=alert(1)>")]);
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("<img src=x onerror=alert(1)>")).toBeTruthy();
    expect(within(dialog).getByText("<b>prod</b>")).toBeTruthy();
    expect(dialog.querySelector("img, b")).toBeNull();
  });

  it("leaves out an environment the caller may not update in, and says so", async () => {
    mountFleet(
      [selected("e1", "prod", "web"), selected("e2", "edge", "cache")],
      new Map([["e2", { kind: "no_permission" }]]),
    );
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Update 1 item", { selector: "h2" })).toBeTruthy();
    expect(within(dialog).getByText("Skipped: you can't update anything in edge.")).toBeTruthy();
    expect(within(dialog).queryByText("cache")).toBeNull();
  });

  it("shows the lock Alert for an environment whose lock is held, and leaves it out", async () => {
    mountFleet(
      [selected("e1", "prod", "web"), selected("e2", "edge", "cache")],
      new Map([
        [
          "e2",
          {
            kind: "locked",
            lock: { kind: "unknown", started: "2026-11-04T10:00:00Z", jobId: "j" },
          },
        ],
      ]),
    );
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Update 1 item", { selector: "h2" })).toBeTruthy();
    expect(within(dialog).getByText(/Another operation is running on edge/)).toBeTruthy();
    expect(dialog.querySelector("[data-slot=operation-lock]")).toBeTruthy();
  });

  it("starts nothing when every environment is left out", async () => {
    mountFleet([selected("e1", "prod", "web")], new Map([["e1", { kind: "offline" }]]));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Skipped: prod isn't reporting.")).toBeTruthy();
    const commit = within(dialog).getByRole("button", { name: "Update 0 items" });
    expect(commit.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(commit);
    expect(runs()).toHaveLength(0);
    expect(started).not.toHaveBeenCalled();
  });

  it("folds the rows of a long group and shows them all on request", async () => {
    mountFleet(Array.from({ length: 8 }, (_, i) => selected("e1", "prod", `svc${i}`)));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getAllByRole("listitem")).toHaveLength(5);
    fireEvent.click(within(dialog).getByRole("button", { name: "Show all 8" }));
    expect(within(dialog).getAllByRole("listitem")).toHaveLength(8);
    fireEvent.click(within(dialog).getByRole("button", { name: "Show fewer" }));
    expect(within(dialog).getAllByRole("listitem")).toHaveLength(5);
  });

  it("closes on Cancel without asking the server", async () => {
    mountFleet([selected("e1", "prod", "web")]);
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(onClose).toHaveBeenCalled();
    expect(runs()).toHaveLength(0);
  });
});
