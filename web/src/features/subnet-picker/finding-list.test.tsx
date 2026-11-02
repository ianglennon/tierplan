// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { RouteCheck, SubnetFinding } from "@/api/address-space";
import { FindingList, RouteCheckNotes } from "@/features/subnet-picker/finding-list";
import "@/i18n";

afterEach(cleanup);

const same = (extra: Partial<SubnetFinding> = {}): SubnetFinding => ({
  kind: "same_env",
  severity: "blocking",
  network: "shop_default",
  subnet: "10.0.2.0/24",
  ...extra,
});
const warn = (extra: Partial<SubnetFinding> & Pick<SubnetFinding, "kind">): SubnetFinding => ({
  severity: "warning",
  ...extra,
});

describe("FindingList", () => {
  it("renders each finding kind in its sentence with its hidden prefix", () => {
    render(
      <FindingList
        findings={[
          same(),
          warn({
            kind: "other_env",
            network: "lab_net",
            subnet: "10.0.2.0/25",
            environment: "Lab",
          }),
          warn({ kind: "host_route", node: "node-1", route: "10.0.0.0/24", iface: "tun0" }),
          warn({ kind: "outside_pool", pool: "10.20.0.0/16" }),
        ]}
      />,
    );
    const rows = screen.getAllByRole("listitem").map((li) => li.textContent);
    expect(rows).toEqual([
      "Blocking: Overlaps shop_default (10.0.2.0/24). Docker can't tell the two networks apart.",
      "Warning: Overlaps lab_net (10.0.2.0/25) on Lab. This only matters if the two sites are routed or joined by a VPN.",
      "Warning: Overlaps a route on node-1: 10.0.0.0/24 on tun0. Traffic to that range may go to the network instead of the container.",
      "Warning: Outside the address pool 10.20.0.0/16. Docker allows it, but it won't count toward the pool's usage.",
    ]);
    // The prefix is for assistive technology only.
    expect(document.querySelector("li span.sr-only")?.textContent).toBe("Blocking: ");
  });

  it("names the node of a local network the subnet overlaps", () => {
    render(<FindingList findings={[same({ node: "node-2", network: "br0" })]} />);
    expect(screen.getByRole("listitem").textContent).toBe(
      "Blocking: Overlaps br0 (10.0.2.0/24) on node-2. Docker can't tell the two networks apart.",
    );
  });

  it("collapses past five findings behind 'Show n more findings'", () => {
    const many = Array.from({ length: 7 }, (_, i) =>
      warn({ kind: "other_env", network: `n${i}`, subnet: "10.0.0.0/24", environment: "Lab" }),
    );
    render(<FindingList findings={many} />);
    expect(screen.getAllByRole("listitem")).toHaveLength(5);
    fireEvent.click(screen.getByRole("button", { name: "Show 2 more findings" }));
    expect(screen.getAllByRole("listitem")).toHaveLength(7);
    expect(screen.queryByRole("button", { name: /more findings/ })).toBeNull();
  });

  it("shows exactly five findings with no button", () => {
    const five = Array.from({ length: 5 }, (_, i) =>
      warn({ kind: "host_route", node: "node-1", route: `10.0.${i}.0/24`, iface: "eth0" }),
    );
    render(<FindingList findings={five} />);
    expect(screen.getAllByRole("listitem")).toHaveLength(5);
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("shows nothing for no findings", () => {
    const { container } = render(<FindingList findings={[]} />);
    expect(container.textContent).toBe("");
  });

  it("shows names as text, never as markup", () => {
    render(<FindingList findings={[same({ network: "<img src=x onerror=alert(1)>" })]} />);
    expect(document.querySelector("img")).toBeNull();
    expect(screen.getByRole("listitem").textContent).toContain("<img src=x onerror=alert(1)>");
  });
});

const check = (extra: Partial<RouteCheck> & Pick<RouteCheck, "node" | "state">): RouteCheck => ({
  local: false,
  ...extra,
});

describe("RouteCheckNotes", () => {
  it("words each state, with the fix in a command block on a swarm", () => {
    render(
      <RouteCheckNotes
        envType="swarm"
        checks={[
          check({ node: "node-1", state: "old_agent" }),
          check({
            node: "node-2",
            state: "unmounted",
            fix: "docker service update --mount-add type=bind,src=/proc/1/net/route,dst=/host/route,ro tierplan_agent",
          }),
          check({ node: "node-3", state: "offline" }),
          check({ node: "node-4", state: "failed" }),
          check({ node: "node-5", state: "ok" }),
        ]}
      />,
    );
    const text = document.body.textContent ?? "";
    expect(text).toContain(
      "Note: Route check skipped on node-1: update the agent to check host routes.",
    );
    expect(text).toContain(
      "Route check skipped on node-2: the agent can't read the host's routes. Mount the route file (see the agent install).",
    );
    expect(text).toContain("Route check skipped on node-3: the agent isn't connected.");
    expect(text).toContain("Route check skipped on node-4: the agent's answer couldn't be read.");
    expect(text).not.toContain("node-5");
    const block = screen.getByRole("region", { name: "the command that adds the route mount" });
    expect(within(block).getByText(/docker service update --mount-add/)).toBeTruthy();
    expect(screen.queryByText(/Re-run the agent install command/)).toBeNull();
  });

  it("tells a standalone host to re-run the agent install command", () => {
    render(
      <RouteCheckNotes
        envType="standalone"
        checks={[check({ node: "box", state: "unmounted" })]}
      />,
    );
    expect(document.body.textContent).toContain(
      "Route check skipped on box: the agent can't read the host's routes. Mount the route file (see the agent install).",
    );
    expect(
      screen.getByText(
        "Re-run the agent install command from this environment's settings to add the mount.",
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("region")).toBeNull();
  });

  it("groups more than three nodes in one state into one line with their number", () => {
    const nodes = ["a", "b", "c", "d"].map((node) => check({ node, state: "unmounted" }));
    render(<RouteCheckNotes envType="standalone" checks={nodes} />);
    const lines = screen.getAllByText(/Route check skipped/);
    expect(lines).toHaveLength(1);
    expect(lines[0]?.textContent).toContain("Route check skipped on 4 nodes: the agent can't read");
    expect(screen.getByText("a, b, c, d")).toBeTruthy();
  });

  it("keeps three nodes in one state as three lines", () => {
    const nodes = ["a", "b", "c"].map((node) => check({ node, state: "offline" }));
    render(<RouteCheckNotes envType="swarm" checks={nodes} />);
    expect(screen.getAllByText(/Route check skipped/)).toHaveLength(3);
  });

  it("words TierPlan's own host's missing mount with the flag to add", () => {
    render(
      <RouteCheckNotes
        envType="standalone"
        checks={[check({ node: "dev", state: "unmounted", local: true })]}
      />,
    );
    expect(document.body.textContent).toContain(
      "Route check skipped on dev: TierPlan's container can't read this host's routes. Add -v /proc/1/net/route:/host/route:ro to TierPlan's docker run.",
    );
    expect(screen.queryByText(/Re-run the agent/)).toBeNull();
  });

  it("shows nothing when every node was read", () => {
    const { container } = render(
      <RouteCheckNotes envType="swarm" checks={[check({ node: "n", state: "ok" })]} />,
    );
    expect(container.textContent).toBe("");
  });

  it("shows a node name as text, never as markup", () => {
    render(
      <RouteCheckNotes
        envType="swarm"
        checks={[check({ node: "<img src=x onerror=alert(1)>", state: "offline" })]}
      />,
    );
    expect(document.querySelector("img")).toBeNull();
  });
});
