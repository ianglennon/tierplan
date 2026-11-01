// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { AddressSpace } from "@/api/address-space";
import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { CommandBlock } from "@/features/pool-guide/command-block";
import { daemonJsonSnippet, swarmInitCommand } from "@/features/pool-guide/generate";
import { type GuidePart, PoolGuidePage } from "@/features/pool-guide/pool-guide-page";
import { guideTiming } from "@/features/pool-guide/use-debounced";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The Pool guidance page (D-23): text to copy, and nothing that runs.

const ENV = "e1";

let client: QueryClient;
let envType: "swarm" | "standalone";
let requests: string[];
let space: Partial<AddressSpace>;
let smallSubnets: { enabled: boolean; mask: number };
let stackNames: string[];

beforeEach(() => {
  setCsrfToken(null);
  guideTiming.debounceMs = 0;
  requests = [];
  envType = "swarm";
  space = {};
  smallSubnets = { enabled: false, mask: 26 };
  stackNames = [];
  const me = makeMe({ envs: { [ENV]: "admin" } });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      requests.push(`${req.method} ${url.pathname}`);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Lab", envType }] });
      }
      if (url.pathname === `/api/env/${ENV}/address-space`) {
        return Response.json({
          envType,
          measured: true,
          networks: [],
          networksTotal: 0,
          networksTruncated: false,
          notReporting: [],
          orphans: [],
          kept: [],
          overlays: [],
          pools: [],
          ...space,
        });
      }
      if (url.pathname === `/api/env/${ENV}/address-space/settings`) {
        return Response.json({ smallSubnets, pools: [], version: 1, changedBy: "" });
      }
      if (url.pathname === `/api/env/${ENV}/stacks`) {
        return Response.json({
          items: stackNames.map((name) => ({ name, managed: true })),
          total: stackNames.length,
          unfilteredTotal: stackNames.length,
        });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
  guideTiming.debounceMs = 150;
});

function mount(part?: GuidePart) {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <PoolGuidePage envId={ENV} part={part} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

function view(part?: GuidePart) {
  return (
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <PoolGuidePage envId={ENV} part={part} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>
  );
}

const INIT_DEFAULT =
  "docker swarm init --default-addr-pool 10.0.0.0/19 --default-addr-pool-mask-length 26";

describe("the Pool guidance page", () => {
  it("shows the breadcrumb, the title, the caption and the note that nothing is run", async () => {
    mount("new-swarm");
    expect(await screen.findByRole("heading", { level: 1, name: "Pool guidance" })).toBeTruthy();
    expect(screen.getByRole("navigation", { name: "Breadcrumb" }).textContent).toContain(
      "Address space",
    );
    expect(screen.getByRole("link", { name: "Address space" }).getAttribute("href")).toBe(
      "/env/e1/address-space",
    );
    expect(
      screen.getByText(
        "Plan address pools without touching your hosts. Coaming explains the steps and gives you commands to copy. It never runs them.",
      ),
    ).toBeTruthy();
    expect(
      screen.getByText(
        "Coaming never runs swarm init or swarm leave, and never edits daemon.json. Run the commands yourself on the right host.",
      ),
    ).toBeTruthy();
    for (const name of ["Free up space", "New swarm", "Rebuild a swarm", "Standalone host"]) {
      expect(screen.getByRole("tab", { name })).toBeTruthy();
    }
  });

  it("links each tab to its part in the address", async () => {
    mount("new-swarm");
    await screen.findByRole("heading", { level: 1, name: "Pool guidance" });
    expect(screen.getByRole("tab", { name: "Rebuild a swarm" }).getAttribute("href")).toBe(
      "/env/e1/address-space/guide?part=rebuild",
    );
    expect(screen.getByRole("tab", { name: "New swarm" }).getAttribute("aria-current")).toBe(
      "page",
    );
  });

  it("scrolls the tab bar sideways inside a padded wrapper, not on the tab list", async () => {
    mount("new-swarm");
    const list = await screen.findByRole("tablist", { name: "Parts of the pool guidance" });
    // The tab list has a fixed height and its 2px underline hangs below it: an overflow class on
    // the list itself would show a 1px vertical scroll bar (G-10-9b).
    expect(list.className).not.toMatch(/overflow/);
    const wrapper = list.parentElement;
    expect(wrapper?.className).toContain("overflow-x-auto");
    expect(wrapper?.className).toContain("pb-2");
  });

  it("generates the swarm init command", async () => {
    mount("new-swarm");
    expect(await screen.findByText(INIT_DEFAULT)).toBeTruthy();
    expect(
      screen.getByText("This pool holds 128 networks of /26, so 50 networks leave 78 spare."),
    ).toBeTruthy();
    expect(
      screen.getByText("The pool is fixed at init. Changing it later means rebuilding the swarm."),
    ).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Copy command: Run this on the first manager" }),
    ).toBeTruthy();
  });

  it("follows the fields and avoids a range", async () => {
    mount("new-swarm");
    await screen.findByText(INIT_DEFAULT);
    fireEvent.change(screen.getByLabelText("Networks you expect"), { target: { value: "500" } });
    expect(
      await screen.findByText(
        "docker swarm init --default-addr-pool 10.0.0.0/16 --default-addr-pool-mask-length 26",
      ),
    ).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Ranges to avoid"), { target: { value: "10.0.0.0/8" } });
    expect(
      await screen.findByText(
        "docker swarm init --default-addr-pool 172.16.0.0/16 --default-addr-pool-mask-length 26",
      ),
    ).toBeTruthy();
  });

  it("gives the error line and no command for a range line that is not a CIDR", async () => {
    mount("new-swarm");
    await screen.findByText(INIT_DEFAULT);
    fireEvent.change(screen.getByLabelText("Ranges to avoid"), {
      target: { value: "10.0.0.0/8; rm -rf /" },
    });
    expect(
      await screen.findByText(/Enter a CIDR like 192\.168\.0\.0\/16 on each line\./),
    ).toBeTruthy();
    expect(screen.queryByText(/docker swarm init/)).toBeNull();
    expect(screen.queryByRole("button", { name: /Copy command/ })).toBeNull();
  });

  it("says when no range is big enough", async () => {
    mount("new-swarm");
    await screen.findByText(INIT_DEFAULT);
    fireEvent.change(screen.getByLabelText("Ranges to avoid"), {
      target: { value: "10.0.0.0/8\n172.16.0.0/12" },
    });
    expect(
      await screen.findByText(
        "No range outside the ones you listed is big enough. Remove a range to avoid, or use a smaller subnet size.",
      ),
    ).toBeTruthy();
    expect(screen.queryByText(/docker swarm init/)).toBeNull();
  });

  it("notes a subnet size too small for the nodes", async () => {
    mount("new-swarm");
    await screen.findByText(INIT_DEFAULT);
    fireEvent.change(screen.getByLabelText("Nodes you expect"), { target: { value: "70" } });
    expect(
      await screen.findByText(
        "A /26 holds 61 addresses, fewer than the 70 nodes each overlay gives an address to. Choose a bigger subnet size.",
      ),
    ).toBeTruthy();
  });

  it("never runs, sends or stores anything", async () => {
    mount("new-swarm");
    await screen.findByText(INIT_DEFAULT);
    fireEvent.change(screen.getByLabelText("Networks you expect"), { target: { value: "70" } });
    await screen.findByText(/docker swarm init/);
    expect(requests.filter((r) => !r.startsWith("GET "))).toEqual([]);
    expect(requests.filter((r) => r.includes("address-space"))).toEqual([]);
    expect(localStorage.length).toBe(0);
    expect(screen.queryByRole("button", { name: /^(Run|Execute|Apply)/i })).toBeNull();
  });
});

const orphan = (name: string, cls: "orphaned" | "unused" | "cant_tell", subnets: string[]) => ({
  class: cls,
  formerOwner: "",
  formerOwnerKind: "" as const,
  idleSince: "2026-10-01T00:00:00Z",
  idleSinceSource: "created" as const,
  name,
  networkId: `id-${name}`,
  node: "Cluster",
  nodeId: "n1",
  scope: "swarm" as const,
  subnets,
});

const overlay = (
  name: string,
  percent: number,
  state: "normal" | "getting_full" | "almost_full",
) => ({
  id: `id-${name}`,
  name,
  ingress: false,
  measured: true,
  outsidePool: false,
  subnets: ["10.0.1.0/24"],
  capacity: 256,
  used: Math.round((256 * percent) / 100),
  percent,
  state,
});

describe("the default part", () => {
  it("opens on Free up space for a swarm", async () => {
    mount();
    expect(await screen.findByRole("heading", { level: 2, name: "Free up space" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Free up space" }).getAttribute("aria-current")).toBe(
      "page",
    );
  });

  it("opens on Standalone host for a standalone environment", async () => {
    envType = "standalone";
    mount();
    expect(await screen.findByRole("heading", { level: 2, name: "Standalone host" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Standalone host" }).getAttribute("aria-current")).toBe(
      "page",
    );
  });

  it("opens the part the address names, whatever the type", async () => {
    envType = "standalone";
    mount("new-swarm");
    expect(await screen.findByRole("heading", { level: 2, name: "New swarm" })).toBeTruthy();
  });
});

describe("Rebuild a swarm", () => {
  it("states the downtime from load and lists seven ordered steps", async () => {
    mount("rebuild");
    const downtime = await screen.findByText(
      "This means downtime. Every service stops while you rebuild the swarm.",
    );
    expect(downtime.closest("[role=status]")).not.toBeNull();
    expect(
      screen.getByText(
        "Docker can't change a swarm's pool in place. The only way is to rebuild it.",
      ),
    ).toBeTruthy();
    const steps = within(screen.getByRole("list", { name: "Steps to rebuild the swarm" }));
    const items = steps.getAllByRole("listitem");
    expect(items.map((li) => within(li).getByRole("heading", { level: 3 }).textContent)).toEqual([
      "Back up",
      "Remove the stacks",
      "Leave the swarm",
      "Create the swarm with the new pool",
      "Rejoin the nodes",
      "Adopt the new swarm in Coaming",
      "Redeploy",
    ]);
    expect(screen.getByText("docker swarm leave --force")).toBeTruthy();
    expect(screen.getByText("docker swarm join-token worker")).toBeTruthy();
    expect(
      screen.getByText(
        "Coaming sees a new cluster. Open this environment's settings and choose Adopt new cluster.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open settings" }).getAttribute("href")).toBe(
      "/settings/environments/e1",
    );
  });

  it("shows in step 4 the command New swarm shows, and shares the inputs between the tabs", async () => {
    const view_ = mount("rebuild");
    expect(await screen.findByText(INIT_DEFAULT)).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Copy command: Run on the new first manager" }),
    ).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Networks you expect"), { target: { value: "500" } });
    const bigger =
      "docker swarm init --default-addr-pool 10.0.0.0/16 --default-addr-pool-mask-length 26";
    expect(await screen.findByText(bigger)).toBeTruthy();
    view_.rerender(view("new-swarm"));
    expect(await screen.findByText(bigger)).toBeTruthy();
    expect((screen.getByLabelText("Networks you expect") as HTMLInputElement).value).toBe("500");
  });

  it("writes a docker stack rm line for each plain stack name and leaves the rest to the Stacks page", async () => {
    stackNames = ["web", "shop_v2", "my stack", "x;rm -rf /"];
    mount("rebuild");
    const block = await screen.findByText(/docker stack rm web/);
    expect(block.textContent).toBe("docker stack rm web\ndocker stack rm shop_v2");
    expect(
      screen.getByText(
        "Remove each stack from Coaming's Stacks page, or run the command below on a manager.",
      ),
    ).toBeTruthy();
    expect(
      screen.getByText(
        "2 stacks have names that can't go in a command. Remove them from Coaming's Stacks page.",
      ),
    ).toBeTruthy();
    expect(screen.queryByText(/my stack/)).toBeNull();
    expect(screen.queryByText(/rm -rf/)).toBeNull();
  });

  it("gives only the sentence when no stack name is safe", async () => {
    stackNames = ["a b"];
    mount("rebuild");
    expect(await screen.findByText("Remove each stack from Coaming's Stacks page.")).toBeTruthy();
    expect(screen.queryByText(/docker stack rm/)).toBeNull();
  });

  it("asks for the fields to be fixed instead of showing a command", async () => {
    mount("rebuild");
    await screen.findByText(INIT_DEFAULT);
    fireEvent.change(screen.getByLabelText("Ranges to avoid"), { target: { value: "nope" } });
    expect(await screen.findByText("Fix the fields above to see the command.")).toBeTruthy();
    expect(screen.queryByText(/docker swarm init/)).toBeNull();
  });
});

describe("Standalone host", () => {
  beforeEach(() => {
    envType = "standalone";
  });

  it("generates the daemon.json snippet from the prefilled row", async () => {
    mount("standalone");
    const code = await screen.findByText(/"default-address-pools"/);
    expect(JSON.parse(code.textContent ?? "")).toEqual({
      "default-address-pools": [{ base: "10.20.0.0/16", size: 24 }],
    });
    expect(code.textContent).toBe(
      '{\n  "default-address-pools": [\n    {\n      "base": "10.20.0.0/16",\n      "size": 24\n    }\n  ]\n}',
    );
    expect(
      screen.getByRole("button", { name: "Copy command: Merge this into /etc/docker/daemon.json" }),
    ).toBeTruthy();
    expect(screen.getByText(/sudo systemctl restart docker/)).toBeTruthy();
    expect(
      (await screen.findByRole("link", { name: "Open address pool settings" })).getAttribute(
        "href",
      ),
    ).toBe("/settings/environments/e1#address-pools");
  });

  it("makes one object for each of five rows", async () => {
    mount("standalone");
    await screen.findByText(/"default-address-pools"/);
    for (let n = 2; n <= 5; n++) {
      fireEvent.click(screen.getByRole("button", { name: "Add pool" }));
      fireEvent.change(screen.getByLabelText(`Base for pool ${n}`), {
        target: { value: `10.${20 + n}.0.0/16` },
      });
      fireEvent.change(screen.getByLabelText(`Size for pool ${n}`), { target: { value: "24" } });
    }
    const code = await screen.findByText(/10\.25\.0\.0\/16/);
    expect(JSON.parse(code.textContent ?? "")["default-address-pools"]).toHaveLength(5);
  });

  it("shows the row error and no snippet for a base that is not a pool", async () => {
    mount("standalone");
    await screen.findByText(/"default-address-pools"/);
    fireEvent.change(screen.getByLabelText("Base for pool 1"), {
      target: { value: '10.0.0.0/8"}] ; rm -rf /' },
    });
    expect(await screen.findByText("Enter a base like 10.20.0.0/16.")).toBeTruthy();
    expect(screen.queryByText(/default-address-pools/)).toBeNull();
    expect(
      screen.getByText("Fill in the base and size of every pool to see the snippet."),
    ).toBeTruthy();
  });

  it("removes a row and keeps focus on Add pool", async () => {
    mount("standalone");
    await screen.findByText(/"default-address-pools"/);
    fireEvent.click(screen.getByRole("button", { name: "Remove pool 1" }));
    expect(screen.queryByLabelText("Base for pool 1")).toBeNull();
    expect(
      await screen.findByText("Fill in the base and size of every pool to see the snippet."),
    ).toBeTruthy();
  });

  it("does not offer the settings link on a swarm", async () => {
    envType = "swarm";
    mount("standalone");
    await screen.findByText(/"default-address-pools"/);
    expect(screen.queryByRole("link", { name: "Open address pool settings" })).toBeNull();
  });
});

describe("Free up space", () => {
  it("counts the orphaned and unused networks and the addresses they hold", async () => {
    space = {
      orphans: [
        orphan("a", "orphaned", ["10.0.1.0/24"]),
        orphan("b", "orphaned", ["10.0.2.0/24"]),
        orphan("c", "orphaned", ["10.0.3.0/24", "fd00::/64"]),
        orphan("d", "unused", ["10.0.4.0/25"]),
        orphan("e", "unused", ["10.0.5.0/25"]),
        orphan("f", "cant_tell", ["10.0.6.0/24"]),
      ],
    };
    mount("free-space");
    expect(
      await screen.findByText("3 orphaned and 2 unused networks hold 1,024 addresses."),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open orphaned networks" }).getAttribute("href")).toBe(
      "/env/e1/address-space#orphans",
    );
    expect(screen.getByText("No network is filling up.")).toBeTruthy();
    expect(
      screen.getByText(
        "New networks get a /26 instead of a /24 and the pool lasts about 4 times longer.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open settings" }).getAttribute("href")).toBe(
      "/settings/environments/e1#default-subnets",
    );
    expect(screen.queryByText("On, /26")).toBeNull();
  });

  it("shows the done state when small stack subnets are on", async () => {
    smallSubnets = { enabled: true, mask: 27 };
    space = { orphans: [orphan("a", "orphaned", ["10.0.1.0/24"])] };
    mount("free-space");
    expect(await screen.findByText("On, /27")).toBeTruthy();
    expect(
      screen.getByText(
        "New networks get a /27 instead of a /24 and the pool lasts about 8 times longer.",
      ),
    ).toBeTruthy();
  });

  it("links the networks at 70 percent or more with their percent", async () => {
    space = {
      overlays: [
        overlay("shop_back", 92, "almost_full"),
        overlay("web_net", 71, "getting_full"),
        overlay("calm", 10, "normal"),
      ],
    };
    mount("free-space");
    const link = await screen.findByRole("link", { name: "shop_back, 92% used" });
    expect(link.getAttribute("href")).toBe("/env/e1/networks/id-shop_back");
    expect(screen.getByRole("link", { name: "web_net, 71% used" })).toBeTruthy();
    expect(screen.queryByRole("link", { name: /calm/ })).toBeNull();
    expect(screen.queryByText("No network is filling up.")).toBeNull();
  });

  it("says there is nothing to free up", async () => {
    mount("free-space");
    expect(await screen.findByText("Nothing to free up")).toBeTruthy();
    expect(
      screen.getByText(
        "No orphaned networks, and no network is filling up. Small stack subnets are the next thing to turn on.",
      ),
    ).toBeTruthy();
    expect(screen.queryByText("Remove orphaned networks")).toBeNull();
    expect(screen.getByText("Use smaller subnets for new stack networks")).toBeTruthy();
  });
});

describe("long commands", () => {
  it("scrolls long commands instead of wrapping", () => {
    const five = daemonJsonSnippet(
      [16, 17, 18, 19, 20].map((n) => ({ base: `10.${n}.0.0/16`, size: "24" })),
    );
    const longInit = swarmInitCommand("172.16.0.0/12", 28);
    render(
      <>
        <CommandBlock caption="Five pools" command={five} />
        <CommandBlock caption="Long init" command={longInit} />
      </>,
    );
    for (const text of [five, longInit]) {
      const code = screen.getByText((_, el) => el?.tagName === "CODE" && el.textContent === text);
      const pre = code.closest("pre");
      expect(pre).not.toBeNull();
      expect(pre?.className).toContain("overflow-x-auto");
      expect(pre?.className).toContain("whitespace-pre");
      expect(pre?.className).not.toContain("whitespace-normal");
      expect(pre?.className).not.toContain("break-");
    }
  });
});
