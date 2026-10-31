// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
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

beforeEach(() => {
  setCsrfToken(null);
  guideTiming.debounceMs = 0;
  requests = [];
  envType = "swarm";
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
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <PoolGuidePage envId={ENV} part={part} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
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
