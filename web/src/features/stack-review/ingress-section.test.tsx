// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { StackReviewIngress } from "@/api/stacks";
import { IngressSection } from "@/features/stack-review/ingress-section";
import "@/i18n";

// The Ingress card of the review's Changes tab (plan 24-26, DEP-03, D-21, D-22, UI-SPEC S1).

afterEach(() => cleanup());

type Route = NonNullable<StackReviewIngress["routes"]>[number];

const route = (over: Partial<Route> = {}): Route => ({
  change: "added",
  hostname: "app.example.com",
  path: "/",
  port: 8080,
  scheme: "http",
  service: "web",
  ...over,
});

const populated = (): StackReviewIngress => ({
  routes: [
    route(),
    route({ change: "kept", hostname: "b.example.com", path: "/api", service: "api", port: 3000 }),
    route({ change: "released", hostname: "c.example.com", path: "", service: "old" }),
  ],
  certificates: [
    { hostname: "app.example.com", state: "will_issue" },
    { hostname: "b.example.com", state: "reused" },
    { hostname: "c.example.com", state: "waiting", reason: "The challenge failed." },
  ],
  network: [
    { service: "web", change: "joins", replacesTasks: true },
    { service: "old", change: "leaves", replacesTasks: false },
  ],
});

describe("IngressSection", () => {
  it("is a card titled Ingress with the counts of the routes, zero counts written too", () => {
    render(<IngressSection ingress={populated()} target="swarm" />);
    expect(screen.getByRole("heading", { level: 3, name: "Ingress" })).toBeTruthy();
    expect(screen.getByText("1 added, 1 kept, 1 released.")).toBeTruthy();
    cleanup();
    render(
      <IngressSection
        ingress={{ routes: [route({ change: "kept" })], certificates: [], network: [] }}
        target="swarm"
      />,
    );
    expect(screen.getByText("0 added, 1 kept, 0 released.")).toBeTruthy();
  });

  it("lists the routes in the server's order, the glyph hidden from the name, an empty path as /", () => {
    render(<IngressSection ingress={populated()} target="swarm" />);
    expect(screen.getByRole("heading", { level: 4, name: "Routes" })).toBeTruthy();
    const table = screen.getByRole("table", { name: "Routes" });
    for (const col of ["Change", "Hostname", "Path", "Service", "Port", "Scheme", "Note"]) {
      expect(within(table).getByRole("columnheader", { name: col }).getAttribute("scope")).toBe(
        "col",
      );
    }
    const rows = within(table).getAllByRole("row").slice(1);
    expect(rows.map((r) => within(r).getAllByRole("cell")[1]?.textContent)).toEqual([
      "app.example.com",
      "b.example.com",
      "c.example.com",
    ]);
    // The word is the accessible name; the glyph is drawn but hidden.
    const first = within(rows[0] as HTMLElement).getAllByRole("cell")[0] as HTMLElement;
    expect(first.textContent).toBe("+Added");
    expect(first.querySelector("[aria-hidden='true']")?.textContent).toBe("+");
    expect(
      (within(rows[1] as HTMLElement).getAllByRole("cell")[0] as HTMLElement).textContent,
    ).toBe("=Kept");
    expect(
      (within(rows[2] as HTMLElement).getAllByRole("cell")[0] as HTMLElement).textContent,
    ).toBe("−Released");
    // An empty path prefix reads as the whole hostname, and the port and scheme are drawn.
    const released = within(rows[2] as HTMLElement).getAllByRole("cell");
    expect(released[2]?.textContent).toBe("/");
    const kept = within(rows[1] as HTMLElement).getAllByRole("cell");
    expect(kept.map((c) => c.textContent)).toEqual([
      "=Kept",
      "b.example.com",
      "/api",
      "api",
      "3000",
      "http",
      "—",
    ]);
  });

  it("draws the server's note, and a dash where there is none", () => {
    render(
      <IngressSection
        ingress={{
          routes: [route({ note: "Not served on this host yet." }), route({ hostname: "z.test" })],
          certificates: [],
          network: [],
        }}
        target="standalone"
      />,
    );
    const rows = within(screen.getByRole("table", { name: "Routes" }))
      .getAllByRole("row")
      .slice(1);
    expect(within(rows[0] as HTMLElement).getAllByRole("cell")[6]?.textContent).toBe(
      "Not served on this host yet.",
    );
    expect(within(rows[1] as HTMLElement).getAllByRole("cell")[6]?.textContent).toBe("—");
  });

  it("words each certificate, gives the reason of a waiting one, and a dash otherwise", () => {
    render(<IngressSection ingress={populated()} target="swarm" />);
    const table = screen.getByRole("table", { name: "Certificates" });
    const rows = within(table).getAllByRole("row").slice(1);
    expect(
      rows.map((r) =>
        within(r)
          .getAllByRole("cell")
          .map((c) => c.textContent),
      ),
    ).toEqual([
      ["app.example.com", "Will be issued", "—"],
      ["b.example.com", "Already issued", "—"],
      ["c.example.com", "Waiting", "The challenge failed."],
    ]);
    expect(
      screen.getByText(
        "A certificate that isn't ready never stops a deploy. The route serves as soon as it is.",
      ),
    ).toBeTruthy();
  });

  it("leaves the certificate footnote out when every certificate is already issued", () => {
    render(
      <IngressSection
        ingress={{
          routes: [route()],
          certificates: [{ hostname: "app.example.com", state: "reused" }],
          network: [],
        }}
        target="swarm"
      />,
    );
    expect(screen.getByRole("table", { name: "Certificates" })).toBeTruthy();
    expect(screen.queryByText(/never stops a deploy/)).toBeNull();
  });

  it("lists the services that join or leave the network, the name in mono, the replaced tasks said", () => {
    render(<IngressSection ingress={populated()} target="swarm" />);
    expect(screen.getByRole("heading", { level: 4, name: "Network" })).toBeTruthy();
    const table = screen.getByRole("table", { name: "Network changes" });
    const rows = within(table).getAllByRole("row").slice(1);
    expect(
      rows.map((r) =>
        within(r)
          .getAllByRole("cell")
          .map((c) => c.textContent),
      ),
    ).toEqual([
      ["web", "Joins tierplan-ingress", "Replaces its tasks"],
      ["old", "Leaves tierplan-ingress", "No restart"],
    ]);
    const mono = within(rows[0] as HTMLElement).getByText("tierplan-ingress");
    expect(mono.className).toContain("font-mono");
    const icon = (rows[0] as HTMLElement).querySelector("svg");
    expect(icon?.getAttribute("aria-hidden")).toBe("true");
    expect(
      screen.getByText(
        "TierPlan attaches routed services to the ingress network itself. It never appears in your compose file.",
      ),
    ).toBeTruthy();
  });

  it("draws no network table for a standalone host, nor when no attachment changes", () => {
    render(<IngressSection ingress={populated()} target="standalone" />);
    expect(screen.queryByRole("table", { name: "Network changes" })).toBeNull();
    expect(screen.queryByRole("heading", { name: "Network" })).toBeNull();
    cleanup();
    render(<IngressSection ingress={{ ...populated(), network: [] }} target="swarm" />);
    expect(screen.queryByRole("table", { name: "Network changes" })).toBeNull();
  });

  it("draws nothing without a route, a certificate or an attachment", () => {
    const { container, rerender } = render(<IngressSection ingress={undefined} target="swarm" />);
    expect(container.innerHTML).toBe("");
    rerender(<IngressSection ingress={{ routes: null, certificates: null, network: null }} />);
    expect(container.innerHTML).toBe("");
    // Only an attachment, and it is a standalone host: nothing to show.
    rerender(
      <IngressSection
        ingress={{
          routes: [],
          certificates: [],
          network: [{ service: "web", change: "joins", replacesTasks: true }],
        }}
        target="standalone"
      />,
    );
    expect(container.innerHTML).toBe("");
  });

  it("draws 50 routes without a cap, hostnames as mono wrapping text", () => {
    const routes = Array.from({ length: 50 }, (_, i) =>
      route({ hostname: `host-${String(i).padStart(2, "0")}.example.com` }),
    );
    render(<IngressSection ingress={{ routes, certificates: [], network: [] }} target="swarm" />);
    const rows = within(screen.getByRole("table", { name: "Routes" })).getAllByRole("row");
    expect(rows).toHaveLength(51);
    const cell = within(rows[50] as HTMLElement).getAllByRole("cell")[1] as HTMLElement;
    expect(cell.textContent).toBe("host-49.example.com");
    expect(cell.className).toContain("font-mono");
    expect(cell.className).toContain("break-all");
    expect(screen.getByText("50 added, 0 kept, 0 released.")).toBeTruthy();
  });

  it("draws a hostname with markup in it as text", () => {
    render(
      <IngressSection
        ingress={{
          routes: [route({ hostname: "<img src=x onerror=alert(1)>.example.com" })],
          certificates: [],
          network: [],
        }}
        target="swarm"
      />,
    );
    expect(screen.getByText("<img src=x onerror=alert(1)>.example.com")).toBeTruthy();
    expect(document.querySelector("img")).toBeNull();
  });
});
