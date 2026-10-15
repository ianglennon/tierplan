// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { serviceRetry } from "@/api/services";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";
import {
  bareServiceDetail,
  DIGEST,
  SERVICE_ENV,
  SERVICE_ID,
  type ServiceApiState,
  type ServiceDetailBody,
  serviceDetail,
  serviceFetch,
} from "@/test-service";

// The Overview tab of a service (plan 06-06, RES-02, D-39, UI-SPEC "Service detail" and the E2
// states).

const PAGE = `/env/${SERVICE_ENV}/services/${SERVICE_ID}`;

let state: ServiceApiState;
const defaults = queryClient.getDefaultOptions();

beforeEach(() => {
  state = {
    sent: [],
    me: makeMe({ envs: { [SERVICE_ENV]: "operator" } }),
    detail: () => Response.json(serviceDetail()),
    inspect: () => Response.json({ masked: false, document: {} }),
    scale: () => Response.json({ results: [] }),
  };
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  serviceRetry.attempts = 0;
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal("fetch", vi.fn(serviceFetch(state)));
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  queryClient.setDefaultOptions(defaults);
  serviceRetry.attempts = 1;
});

async function overview(body: ServiceDetailBody = serviceDetail()) {
  state.detail = () => Response.json(body);
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [PAGE] }),
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  await screen.findByRole("heading", { level: 2 });
}

// card finds one card by its title.
function card(title: string): HTMLElement {
  const heading = screen.getByRole("heading", { level: 3, name: title });
  const region = heading.closest("section");
  if (!region) throw new Error(`no card ${title}`);
  return region;
}

describe("the Overview cards", () => {
  it("shows the twelve cards in the two columns", async () => {
    await overview();
    for (const title of [
      "Service",
      "Image",
      "Ports",
      "Mounts",
      "Secrets and configs",
      "Networks",
      "Environment variables",
      "Resources",
      "Placement rules",
      "Restart policy",
      "Update policy",
      "Labels",
    ]) {
      expect(screen.getByRole("heading", { level: 3, name: title })).toBeTruthy();
    }
  });

  it("fills the Service card: ID, mode, replicas, times and stack", async () => {
    await overview(serviceDetail({ stack: "shop" }));
    const c = within(card("Service"));
    expect(c.getByText(SERVICE_ID)).toBeTruthy();
    expect(c.getByText("Replicated")).toBeTruthy();
    expect(c.getByText("2 of 3 tasks running")).toBeTruthy();
    expect(c.getByText("shop")).toBeTruthy();
  });

  it("uses the singular for one task", async () => {
    await overview(serviceDetail({ running: 1, desired: 1 }));
    expect(within(card("Service")).getByText("1 of 1 task running")).toBeTruthy();
  });

  it("fills the Image card: the tag, the full digest and that it is pinned", async () => {
    await overview();
    const c = within(card("Image"));
    expect(c.getByText("1.4")).toBeTruthy();
    expect(c.getByText(DIGEST)).toBeTruthy();
    expect(c.getByText("Yes, to the digest above")).toBeTruthy();
  });

  it("says the tag can move for an image with no digest", async () => {
    await overview(bareServiceDetail());
    const c = within(card("Image"));
    expect(c.getByText("No, the tag can move")).toBeTruthy();
  });

  it("lists the ports with their mode in words", async () => {
    await overview();
    const c = within(card("Ports"));
    expect(c.getByText("8080")).toBeTruthy();
    expect(c.getByText("80")).toBeTruthy();
    expect(c.getByText("tcp")).toBeTruthy();
    expect(c.getByText("Ingress")).toBeTruthy();
  });

  it("lists the mounts, linking a volume to its page and showing read-only", async () => {
    await overview();
    const c = within(card("Mounts"));
    expect(c.getByRole("link", { name: "api-data" }).getAttribute("href")).toBe(
      "/env/e1/volumes/api-data",
    );
    expect(c.getByText("/srv/conf")).toBeTruthy();
    expect(c.queryByRole("link", { name: "/srv/conf" })).toBeNull();
    expect(c.getByText("Read-write")).toBeTruthy();
    expect(c.getByText("Read-only")).toBeTruthy();
  });

  it("lists the secrets and configs by reference, with their file, owner and mode", async () => {
    await overview();
    const c = within(card("Secrets and configs"));
    expect(c.getByText("Secret")).toBeTruthy();
    expect(c.getByText("db_pw")).toBeTruthy();
    expect(c.getByText("db_pw.txt")).toBeTruthy();
    expect(c.getByText("Config")).toBeTruthy();
    expect(c.getByText("app_conf")).toBeTruthy();
    expect(c.getByText("/etc/app.conf")).toBeTruthy();
    expect(c.getByText("1000 / 1000")).toBeTruthy();
    expect(c.getByText("0440")).toBeTruthy();
  });

  it("links a secret's name to the secret's page, and leaves a config's name as text", async () => {
    await overview();
    const c = within(card("Secrets and configs"));
    expect(c.getByRole("link", { name: "db_pw" }).getAttribute("href")).toBe(
      "/env/e1/secrets/sec1",
    );
    expect(c.queryByRole("link", { name: "app_conf" })).toBeNull();
  });

  it("lists the networks by name, linking to the network, with their aliases", async () => {
    await overview();
    const c = within(card("Networks"));
    expect(c.getByRole("link", { name: "shop_front" }).getAttribute("href")).toBe(
      "/env/e1/networks/net-front",
    );
    expect(c.getByText("api.internal")).toBeTruthy();
  });

  it("words the resources, and says No limit and None reserved for what is unset", async () => {
    await overview();
    const c = within(card("Resources"));
    expect(c.getByText("0.5 CPUs")).toBeTruthy();
    expect(c.getByText("0.25 CPUs")).toBeTruthy();
    expect(c.getByText("256 MiB")).toBeTruthy();
    expect(c.getByText("None reserved")).toBeTruthy();
  });

  it("lists the placement rules", async () => {
    await overview();
    const c = within(card("Placement rules"));
    expect(c.getByText("node.role==worker")).toBeTruthy();
    expect(c.getByText("Spread over node.labels.zone")).toBeTruthy();
    expect(c.getByText("2")).toBeTruthy();
  });

  it("words the restart policy and the update policy", async () => {
    await overview();
    const r = within(card("Restart policy"));
    expect(r.getByText("Failure only")).toBeTruthy();
    expect(r.getByText("5s")).toBeTruthy();
    expect(r.getByText("4")).toBeTruthy();
    expect(r.getByText("2m")).toBeTruthy();
    const u = within(card("Update policy"));
    expect(u.getByText("2")).toBeTruthy();
    expect(u.getByText("Start first")).toBeTruthy();
    expect(u.getByText("Pause")).toBeTruthy();
    expect(u.getByText("0.25")).toBeTruthy();
  });

  it("lists the labels", async () => {
    await overview();
    const c = within(card("Labels"));
    expect(c.getByText("tier")).toBeTruthy();
    expect(c.getByText("backend")).toBeTruthy();
  });
});

describe("secret values", () => {
  it("shows a Viewer the names with dots, never a value, and says why", async () => {
    await overview();
    const c = within(card("Environment variables"));
    expect(c.getByText("DB_PASSWORD")).toBeTruthy();
    expect(c.getByText("MODE")).toBeTruthy();
    expect(c.getAllByText("••••••••")).toHaveLength(2);
    expect(c.queryByRole("button", { name: /Show value/ })).toBeNull();
    expect(
      c.getByText("Values are hidden. Needs the reveal secrets permission to show them."),
    ).toBeTruthy();
  });

  it("gives a holder of secrets.reveal the values, still hidden until asked", async () => {
    await overview(
      serviceDetail({
        env: [{ name: "DB_PASSWORD", value: "hunter2", masked: false }],
        envMasked: false,
      }),
    );
    const c = within(card("Environment variables"));
    expect(c.queryByText("hunter2")).toBeNull();
    fireEvent.click(c.getByRole("button", { name: "Show value of DB_PASSWORD" }));
    expect(c.getByText("hunter2")).toBeTruthy();
    expect(c.queryByText(/Values are hidden/)).toBeNull();
  });
});

describe("the E2 states", () => {
  it("empty: each card has its own empty line", async () => {
    await overview(bareServiceDetail());
    expect(within(card("Ports")).getByText("No published ports.")).toBeTruthy();
    expect(within(card("Mounts")).getByText("No mounts.")).toBeTruthy();
    expect(within(card("Secrets and configs")).getByText("No secrets or configs.")).toBeTruthy();
    expect(within(card("Networks")).getByText("Not attached to any network.")).toBeTruthy();
    expect(
      within(card("Environment variables")).getByText("No environment variables."),
    ).toBeTruthy();
    expect(
      within(card("Placement rules")).getByText("No placement rules. Tasks can run on any node."),
    ).toBeTruthy();
    expect(within(card("Labels")).getByText("No labels.")).toBeTruthy();
  });

  it("partial: missing sections show a dash or No limit", async () => {
    await overview(bareServiceDetail());
    const r = within(card("Resources"));
    expect(r.getAllByText("No limit")).toHaveLength(2);
    expect(r.getAllByText("None reserved")).toHaveLength(2);
    expect(within(card("Restart policy")).getAllByText("—").length).toBeGreaterThanOrEqual(1);
    expect(within(card("Update policy")).getAllByText("—").length).toBeGreaterThanOrEqual(1);
    expect(within(card("Image")).getAllByText("—").length).toBeGreaterThanOrEqual(1);
  });

  it("scaled to zero: says so with a Scale service action and no error styling", async () => {
    await overview(serviceDetail({ running: 0, desired: 0, updateStatus: "" }));
    const note = screen.getByText("Scaled to 0 replicas");
    expect(note.closest("[role=alert]")).toBeNull();
    const scale = screen.getByRole("button", { name: "Scale service" });
    fireEvent.click(scale);
    expect(await screen.findByRole("dialog", { name: "Scale api" })).toBeTruthy();
  });

  it("scaled to zero: a Viewer sees Scale service unavailable", async () => {
    state.me = makeMe({ envs: { [SERVICE_ENV]: "viewer" } });
    await overview(serviceDetail({ running: 0, desired: 0 }));
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Scale service" }).getAttribute("aria-disabled"),
      ).toBe("true"),
    );
  });

  it("scaled to zero: a global service has no Scale service action", async () => {
    await overview(serviceDetail({ mode: "global", running: 0, desired: 0 }));
    expect(screen.queryByRole("button", { name: "Scale service" })).toBeNull();
  });

  it("overflow and long text: tables scroll inside their card and long values wrap", async () => {
    const long = "x".repeat(300);
    await overview(
      serviceDetail({
        placement: { constraints: [`node.labels.${long}==1`], preferences: [], maxReplicas: 0 },
      }),
    );
    const constraint = within(card("Placement rules")).getByText(`node.labels.${long}==1`);
    expect(constraint.className).toContain("break-all");
    const table = within(card("Ports")).getByRole("table");
    expect(table.closest("[data-slot=table-container]")?.className).toContain("overflow-x-auto");
  });

  it("long text: the name clamps to two lines and the tooltip carries it whole", async () => {
    await overview(serviceDetail({ name: "n".repeat(120) }));
    const heading = screen.getByRole("heading", { level: 2 });
    expect(heading.className).toContain("line-clamp-2");
  });
});
