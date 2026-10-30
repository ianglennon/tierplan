// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { VolumeDetail } from "@/api/volumes";
import { TooltipProvider } from "@/components/ui/tooltip";
import { VolumeOverview } from "@/features/volumes/overview";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The Overview tab of a volume (plan 04-18, RES-07, D-47, UI-SPEC Defaults 11).

const ENV = "e1";

function detail(extra: Partial<VolumeDetail> = {}): VolumeDetail {
  return {
    name: "appdata",
    driver: "local",
    mountpoint: "/var/lib/docker/volumes/appdata/_data",
    scope: "local",
    node: "node-1",
    nodeId: "n1",
    created: "2026-10-01T10:00:00Z",
    anonymous: false,
    options: [],
    labels: {},
    usedBy: [],
    usedByContainers: 0,
    usedByServices: 0,
    notReporting: [],
    ...extra,
  };
}

let answer: () => Response;
let sizesAnswer: () => Response;
let urls: string[];
let client: QueryClient;

beforeEach(() => {
  urls = [];
  answer = () => Response.json(detail());
  sizesAnswer = () =>
    Response.json({
      nodes: [
        {
          nodeId: "n1",
          node: "node-1",
          ok: true,
          sizes: [{ name: "appdata", size: 1288490189, refCount: 1 }],
        },
      ],
    });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      urls.push(`${url.pathname}${url.search}`);
      if (url.pathname === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
      if (url.pathname === `/api/env/${ENV}/volumes/appdata`) return answer();
      if (url.pathname === `/api/env/${ENV}/volumes/sizes`) return sizesAnswer();
      if (url.pathname === "/api/environments") return Response.json({ items: [] });
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

function mount() {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <VolumeOverview envId={ENV} volumeName="appdata" node="n1" />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const card = (title: string) => screen.findByRole("region", { name: title });

// A table's rows as text, header first.
function rows(root: HTMLElement): string[][] {
  return within(root)
    .getAllByRole("row")
    .map((r) => {
      const cells = within(r).queryAllByRole("cell");
      const shown = cells.length > 0 ? cells : within(r).queryAllByRole("columnheader");
      return shown.map((c) => c.textContent ?? "");
    });
}

describe("VolumeOverview details", () => {
  it("reads the volume for the node in the address and lists driver, mount point and scope", async () => {
    mount();
    const details = await card("Details");
    const byName = new Map(rows(details).map(([k, v]) => [k, v]));
    expect(byName.get("Name")).toBe("appdata");
    expect(byName.get("Driver")).toBe("local");
    expect(byName.get("Scope")).toBe("local");
    expect(byName.get("Mount point")).toContain("/var/lib/docker/volumes/appdata/_data");
    // A standalone host has one node, so the node is not a row.
    expect(byName.has("Node")).toBe(false);
    expect(urls).toContain(`/api/env/${ENV}/volumes/appdata?nodeId=n1`);
  });

  it("shows a dash for the size with Calculate size, and the size in binary units once it is worked out", async () => {
    mount();
    const details = await card("Details");
    fireEvent.click(within(details).getByRole("button", { name: "Calculate size" }));
    expect(await within(details).findByText("1.2 GiB")).toBeTruthy();
    expect(within(details).queryByRole("button", { name: "Calculate size" })).toBeNull();
    // Only this volume's node was asked.
    expect(urls).toContain(`/api/env/${ENV}/volumes/sizes?nodeId=n1`);
  });

  it("keeps the dash and says which node could not calculate when it failed", async () => {
    sizesAnswer = () =>
      Response.json({
        nodes: [{ nodeId: "n1", node: "node-1", ok: false, error: "slow", sizes: [] }],
      });
    mount();
    const details = await card("Details");
    fireEvent.click(within(details).getByRole("button", { name: "Calculate size" }));
    expect(
      await within(details).findByRole("button", { name: "— Couldn't calculate on node-1." }),
    ).toBeTruthy();
    expect(within(details).getByRole("button", { name: "Calculate size" })).toBeTruthy();
  });
});

describe("VolumeOverview used by", () => {
  it("says Nothing uses this volume. when nothing does", async () => {
    mount();
    const used = await card("Used by");
    expect(within(used).getByText("Nothing uses this volume.")).toBeTruthy();
    expect(within(used).queryByRole("table")).toBeNull();
  });

  it("lists each container and service with where and how it mounts the volume", async () => {
    answer = () =>
      Response.json(
        detail({
          usedBy: [
            {
              kind: "container",
              id: "c-old",
              name: "old-db",
              node: "node-1",
              nodeId: "n1",
              mountedAt: "/backup",
              readOnly: true,
              state: "exited",
            },
            {
              kind: "container",
              id: "c-web",
              name: "web",
              node: "node-1",
              nodeId: "n1",
              mountedAt: "/var/lib/db",
              readOnly: false,
              state: "running",
            },
            {
              kind: "service",
              id: "s1",
              name: "idle",
              node: "",
              nodeId: "",
              mountedAt: "/srv/data",
              readOnly: true,
              state: "stopped",
            },
          ],
          usedByContainers: 2,
          usedByServices: 1,
        }),
      );
    mount();
    const used = await card("Used by");
    expect(rows(used)).toEqual([
      ["Name", "Type", "Node", "Mounted at", "Mode", "State"],
      ["old-db", "Container", "node-1", "/backup", "Read-only", "exited"],
      ["web", "Container", "node-1", "/var/lib/db", "Read-write", "running"],
      ["idle", "Service", "—", "/srv/data", "Read-only", "stopped"],
    ]);
    // A container opens its page on its own node; a service has no page yet.
    expect(
      within(used).getByRole("link", { name: "Open container web" }).getAttribute("href"),
    ).toBe("/env/e1/containers/c-web?node=n1");
    expect(within(used).queryByRole("link", { name: /idle/ })).toBeNull();
  });
});

describe("VolumeOverview options and labels", () => {
  it("shows an option's name and no value to a caller who may not reveal, with why", async () => {
    answer = () =>
      Response.json(
        detail({
          options: [
            { name: "device", masked: true },
            { name: "o", masked: true },
          ],
        }),
      );
    mount();
    const options = await card("Options");
    expect(within(options).getByText("device")).toBeTruthy();
    expect(within(options).getByText("o")).toBeTruthy();
    expect(within(options).getByText(/Option values are hidden/)).toBeTruthy();
    expect(within(options).queryByRole("button", { name: /Show value/ })).toBeNull();
    expect(document.body.textContent).not.toContain("password");
  });

  it("keeps a revealed value hidden until the person shows it", async () => {
    answer = () =>
      Response.json(
        detail({ options: [{ name: "o", value: "username=bob,password=hunter2", masked: false }] }),
      );
    mount();
    const options = await card("Options");
    expect(within(options).queryByText("username=bob,password=hunter2")).toBeNull();
    fireEvent.click(within(options).getByRole("button", { name: /Show value/ }));
    expect(within(options).getByText("username=bob,password=hunter2")).toBeTruthy();
  });

  it("says No options. and No labels. when there are none, and lists labels by name", async () => {
    mount();
    expect(within(await card("Options")).getByText("No options.")).toBeTruthy();
    expect(within(await card("Labels")).getByText("No labels.")).toBeTruthy();
    cleanup();
    client.clear();
    answer = () => Response.json(detail({ labels: { tier: "db", app: "shop" } }));
    mount();
    const labels = await screen.findByRole("region", { name: "Labels" });
    await within(labels).findByText("app");
    expect(rows(labels)).toEqual([
      ["app", "shop"],
      ["tier", "db"],
    ]);
  });

  it("draws labels and names as text, never as markup", async () => {
    answer = () =>
      Response.json(detail({ labels: { "<img src=x onerror=alert(1)>": "<b>bold</b>" } }));
    mount();
    const labels = await card("Labels");
    await within(labels).findByText("<b>bold</b>");
    expect(labels.querySelector("img")).toBeNull();
    expect(labels.querySelector("b")).toBeNull();
  });
});
