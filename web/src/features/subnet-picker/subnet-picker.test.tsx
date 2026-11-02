// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { SubnetCheck, SubnetCheckRequest } from "@/api/address-space";
import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import {
  type BlockingState,
  SubnetPicker,
  type SubnetPickerProps,
} from "@/features/subnet-picker/subnet-picker";
import { CHECK_DEBOUNCE_MS, checkTiming } from "@/features/subnet-picker/use-subnet-check";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";
import { waitUnderFakeClock } from "@/test-settle";

const ENV = "e1";

let client: QueryClient;
let me: ReturnType<typeof makeMe>;
// What the stubbed server answers to a subnet check, and the checks it was asked.
let answer: (body: SubnetCheckRequest) => Response | Promise<Response>;
let asked: SubnetCheckRequest[];

const clean = (extra: Partial<SubnetCheck> = {}): Response =>
  Response.json({
    findings: [],
    routeChecks: [],
    ipv6NotChecked: false,
    tooManyTasks: false,
    ...extra,
  });

beforeEach(() => {
  asked = [];
  checkTiming.debounceMs = 0;
  me = makeMe({ envs: { [ENV]: "admin" } });
  answer = () => clean();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Homelab", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/address-space/subnet-check`) {
        const body = (await req.json()) as SubnetCheckRequest;
        asked.push(body);
        return answer(body);
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  vi.useRealTimers();
  checkTiming.debounceMs = CHECK_DEBOUNCE_MS;
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

interface HarnessProps extends Partial<SubnetPickerProps> {
  initial?: string;
  onBlocking?: (state: BlockingState) => void;
  // Told each time the picker changes the tick.
  onTick?: (checked: boolean) => void;
}

// The picker as the create dialog holds it: the subnet and the tick are the parent's.
function Harness({ initial = "", onBlocking, onTick, ...props }: HarnessProps) {
  const [value, setValue] = useState(initial);
  const [createAnyway, setCreateAnyway] = useState(false);
  return (
    <SubnetPicker
      envId={ENV}
      envType="swarm"
      value={value}
      onChange={setValue}
      canCreateAnyway
      createAnyway={createAnyway}
      onCreateAnywayChange={(checked) => {
        setCreateAnyway(checked);
        onTick?.(checked);
      }}
      onBlockingChange={onBlocking}
      {...props}
    />
  );
}

function mount(props: HarnessProps = {}) {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <Harness {...props} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const subnetInput = () => screen.getByLabelText("Subnet") as HTMLInputElement;
const tasksInput = () => screen.getByLabelText("Expected tasks") as HTMLInputElement;
const typeSubnet = (value: string) => fireEvent.change(subnetInput(), { target: { value } });
const typeTasks = (value: string) => fireEvent.change(tasksInput(), { target: { value } });
const suggestButton = () =>
  screen.getByRole("button", { name: "Suggest a free subnet of the chosen size" });

describe("SubnetPicker", () => {
  it("suggests a subnet, fills the field, moves focus there and says what it did", async () => {
    answer = (body) =>
      body.suggest
        ? clean({ suggestion: { subnet: "10.0.2.64/26", pool: "10.0.0.0/8" } })
        : clean();
    mount();
    expect(screen.getByText("Subnet", { selector: "legend" })).toBeTruthy();
    fireEvent.click(suggestButton());
    await waitFor(() => expect(subnetInput().value).toBe("10.0.2.64/26"));
    expect(asked[0]).toEqual({ driver: undefined, mask: 24, suggest: true });
    expect(document.activeElement).toBe(subnetInput());
    expect(await screen.findByText("Lowest free /26 block in 10.0.0.0/8.")).toBeTruthy();
    expect(screen.getByText("Suggested 10.0.2.64/26")).toBeTruthy();
    // The block is checked at once, as the field now holds it.
    await waitFor(() => expect(asked.some((b) => b.subnet === "10.0.2.64/26")).toBe(true));
    expect(await screen.findByText("No overlaps found.")).toBeTruthy();
    // The helper is for the suggestion only: typing takes it away.
    typeSubnet("10.0.2.64/2");
    expect(screen.queryByText(/Lowest free/)).toBeNull();
  });

  it("sends the chosen size and the tasks with Suggest, and the node of a local network", async () => {
    answer = (body) =>
      body.suggest
        ? clean({ suggestion: { subnet: "172.20.0.0/27", pool: "172.17.0.0/12" } })
        : clean();
    mount({ driver: "bridge", nodeId: "node-2-id" });
    typeTasks("20");
    await waitFor(() => expect(asked.some((b) => b.tasks === 20)).toBe(true));
    fireEvent.click(suggestButton());
    await waitFor(() => expect(asked.some((b) => b.suggest)).toBe(true));
    expect(asked.find((b) => b.suggest)).toEqual({
      driver: "bridge",
      nodeId: "node-2-id",
      tasks: 20,
      mask: 24,
      suggest: true,
    });
  });

  it("says no free block is left, with a way to the address space", async () => {
    answer = (body) =>
      body.suggest ? clean({ noFreeBlock: { mask: 24, pool: "10.20.0.0/16" } }) : clean();
    mount();
    fireEvent.click(suggestButton());
    expect(
      await screen.findByText(
        "No free /24 block is left in 10.20.0.0/16. Try a smaller size, free up space first, or plan a bigger pool.",
      ),
    ).toBeTruthy();
    const link = screen.getByRole("link", { name: "Open address space" });
    expect(link.getAttribute("href")).toBe("/env/e1/address-space");
    expect(screen.getByRole("link", { name: "Open pool guidance" }).getAttribute("href")).toBe(
      "/env/e1/address-space/guide",
    );
    expect(subnetInput().value).toBe("");
  });

  it("says Suggest failed and leaves the field alone", async () => {
    answer = () => Response.json({ title: "x", status: 500 }, { status: 500 });
    mount({ initial: "" });
    fireEvent.click(suggestButton());
    expect(
      await screen.findByText("Couldn't suggest a subnet. Type one, or try again."),
    ).toBeTruthy();
    expect(subnetInput().value).toBe("");
  });

  it("asks for a node before it suggests for a local network on a swarm", async () => {
    mount({ nodeMissing: true });
    const button = suggestButton();
    expect(button.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(button);
    expect(asked).toEqual([]);
  });

  it("recommends a size from tasks", async () => {
    answer = (body) =>
      body.tasks === 20
        ? clean({ needed: 52, recommendedMask: 26, recommendedUsable: 61 })
        : clean();
    mount();
    typeTasks("20");
    expect(await screen.findByText("Recommended: /26 (61 addresses) for 20 tasks.")).toBeTruthy();
    expect(screen.getByRole("combobox", { name: "Size" }).textContent).toContain(
      "/26 · 61 addresses",
    );
  });

  it("says when the tasks need more than a /24 holds", async () => {
    answer = () => clean({ tooManyTasks: true, needed: 900 });
    mount();
    typeTasks("400");
    expect(
      await screen.findByText(
        "400 tasks need more than a /24 holds. Split the work across more than one network.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("combobox", { name: "Size" }).textContent).toContain(
      "/24 · 253 addresses",
    );
  });

  it("refuses tasks that are not a whole number from 1 to 10000, and sends none", async () => {
    mount();
    typeTasks("0");
    expect(await screen.findByText("Enter a whole number from 1 to 10000.")).toBeTruthy();
    typeTasks("10001");
    expect(screen.getByText("Enter a whole number from 1 to 10000.")).toBeTruthy();
    expect(asked).toEqual([]);
  });

  it("follows the recommendation until the size is picked", async () => {
    answer = (body) => {
      if (body.tasks === 20) return clean({ recommendedMask: 26, recommendedUsable: 61 });
      if (body.tasks === 30) return clean({ recommendedMask: 25, recommendedUsable: 125 });
      return clean();
    };
    mount({ defaultMask: 24 });
    const size = () => screen.getByRole("combobox", { name: "Size" }).textContent;
    expect(size()).toContain("/24 · 253 addresses");
    typeTasks("20");
    await waitFor(() => expect(size()).toContain("/26 · 61 addresses"));

    fireEvent.keyDown(screen.getByRole("combobox", { name: "Size" }), { key: "Enter" });
    fireEvent.click(await screen.findByRole("option", { name: "/27 · 29 addresses" }));
    expect(size()).toContain("/27 · 29 addresses");

    typeTasks("30");
    expect(await screen.findByText("Recommended: /25 (125 addresses) for 30 tasks.")).toBeTruthy();
    expect(size()).toContain("/27 · 29 addresses");
  });

  it("starts at the pool's mask, and at /24 when the pool's mask is not one of the sizes", async () => {
    const { unmount } = mount({ defaultMask: 26 });
    expect(screen.getByRole("combobox", { name: "Size" }).textContent).toContain(
      "/26 · 61 addresses",
    );
    unmount();
    mount({ defaultMask: 20 });
    expect(screen.getByRole("combobox", { name: "Size" }).textContent).toContain(
      "/24 · 253 addresses",
    );
  });

  it("states the formula in 'How TierPlan works this out'", () => {
    mount();
    fireEvent.click(screen.getByRole("button", { name: "How TierPlan works this out" }));
    expect(
      screen.getByText(
        /Addresses needed = \(2 × tasks \+ services \+ nodes\) × 1\.25, rounded up, plus 3 reserved\./,
      ),
    ).toBeTruthy();
  });

  it("shows instant host-bit feedback before the server answers", async () => {
    mount();
    typeSubnet("10.0.5.7/24");
    expect(
      screen.getByText("10.0.5.7/24 has host bits set. Did you mean 10.0.5.0/24?"),
    ).toBeTruthy();
    expect(asked).toEqual([]);
    typeSubnet("10.0.5.0/30");
    expect(
      screen.getByText("A subnet smaller than /29 can't hold a gateway and any containers."),
    ).toBeTruthy();
    // The server is still asked, and has the last word.
    await waitFor(() => expect(asked.length).toBeGreaterThan(0));
  });

  it("shows 'Checking the subnet…' in place of the findings and never stops typing", async () => {
    let release: (r: Response) => void = () => {};
    answer = () => new Promise<Response>((resolve) => (release = resolve));
    mount();
    typeSubnet("10.0.5.0/26");
    const checking = await screen.findByText("Checking the subnet…");
    expect(checking.closest("[role=status]")).toBeTruthy();
    expect(subnetInput().disabled).toBe(false);
    typeSubnet("10.0.5.0/27");
    expect(subnetInput().value).toBe("10.0.5.0/27");
    await waitFor(() => expect(asked.length).toBeGreaterThan(0));
    release(clean());
    await waitFor(() => expect(screen.queryByText("Checking the subnet…")).toBeNull());
  });

  it("says no overlaps were found for a clean answer", async () => {
    mount();
    typeSubnet("10.0.5.0/26");
    const line = await screen.findByText("No overlaps found.");
    expect(line.closest("[role=status]")).toBeTruthy();
  });

  it("renders findings in a polite live region and blocks only on an overlap in the environment", async () => {
    const onBlocking = vi.fn();
    answer = () =>
      clean({
        findings: [
          {
            kind: "same_env",
            severity: "blocking",
            network: "shop_default",
            subnet: "10.0.5.0/24",
            node: "node-1",
          },
          {
            kind: "host_route",
            severity: "warning",
            node: "node-1",
            route: "10.0.5.0/24",
            iface: "eth0",
          },
        ],
      });
    mount({ onBlocking });
    typeSubnet("10.0.5.0/26");
    expect(
      await screen.findByText(
        /Overlaps shop_default \(10\.0\.5\.0\/24\) on node-1\. Docker can't tell the two networks apart\./,
      ),
    ).toBeTruthy();
    const list = screen.getAllByRole("listitem")[0] as HTMLElement;
    expect(list.closest("[aria-live=polite]")).toBeTruthy();
    expect(subnetInput().getAttribute("aria-invalid")).toBe("true");
    expect(onBlocking).toHaveBeenLastCalledWith("overridable");
    expect(screen.queryByText("No overlaps found.")).toBeNull();
  });

  it("does not set aria-invalid for a warning", async () => {
    answer = () =>
      clean({
        findings: [
          {
            kind: "host_route",
            severity: "warning",
            node: "node-1",
            route: "192.168.4.0/24",
            iface: "eth0",
          },
        ],
      });
    const onBlocking = vi.fn();
    mount({ onBlocking });
    typeSubnet("192.168.4.0/24");
    await screen.findByText(/Overlaps a route on node-1: 192\.168\.4\.0\/24 on eth0/);
    expect(subnetInput().getAttribute("aria-invalid")).toBe("false");
    expect(onBlocking).toHaveBeenLastCalledWith("none");
    expect(screen.queryByRole("checkbox")).toBeNull();
  });

  it("offers Create it anyway under a blocking finding, and announces the tick", async () => {
    answer = () =>
      clean({
        findings: [{ kind: "same_env", severity: "blocking", network: "a", subnet: "10.0.5.0/24" }],
      });
    mount();
    typeSubnet("10.0.5.0/26");
    const box = await screen.findByRole("checkbox", { name: "Create it anyway" });
    expect(box.getAttribute("aria-checked")).toBe("false");
    expect(
      screen.getByText(
        "Some setups, such as macvlan, share a range on purpose. TierPlan records that you chose this.",
      ),
    ).toBeTruthy();
    // It is described by the finding.
    const described = (box.getAttribute("aria-describedby") ?? "").split(" ");
    expect(document.getElementById(described[0] ?? "")?.textContent).toContain("Overlaps a");
    fireEvent.click(box);
    expect(box.getAttribute("aria-checked")).toBe("true");
    expect(await screen.findByText("Overlap accepted")).toBeTruthy();
  });

  it("offers no Create it anyway for an overlap between swarm networks, and says why", async () => {
    const onBlocking = vi.fn();
    answer = () =>
      clean({
        findings: [
          {
            kind: "same_env",
            severity: "blocking",
            network: "ovl-a",
            subnet: "10.0.1.0/24",
            noOverride: true,
          },
        ],
      });
    mount({ onBlocking });
    typeSubnet("10.0.1.0/25");
    const note = await screen.findByText(
      "A swarm can't give two of its networks overlapping subnets, so this one can't be created anyway. Choose another subnet.",
    );
    expect(screen.queryByRole("checkbox")).toBeNull();
    expect(onBlocking).toHaveBeenLastCalledWith("final");
    expect(subnetInput().getAttribute("aria-invalid")).toBe("true");
    expect((subnetInput().getAttribute("aria-describedby") ?? "").split(" ")).toContain(note.id);
  });

  it("clears a tick when the overlap can no longer be overridden", async () => {
    const onTick = vi.fn();
    answer = (body) =>
      clean({
        findings: [
          {
            kind: "same_env",
            severity: "blocking",
            network: "a",
            subnet: "10.0.5.0/24",
            ...(body.subnet === "10.0.1.0/25" ? { noOverride: true } : {}),
          },
        ],
      });
    mount({ onTick });
    typeSubnet("10.0.5.0/26");
    fireEvent.click(await screen.findByRole("checkbox", { name: "Create it anyway" }));
    expect(onTick).toHaveBeenLastCalledWith(true);
    typeSubnet("10.0.1.0/25");
    await screen.findByText(/can't be created anyway/);
    expect(onTick).toHaveBeenLastCalledWith(false);
    expect(screen.queryByRole("checkbox")).toBeNull();
  });

  it("opens the compact variant by itself for an overlap that can't be overridden", async () => {
    answer = () =>
      clean({
        findings: [
          {
            kind: "same_env",
            severity: "blocking",
            network: "ovl-a",
            subnet: "10.0.1.0/24",
            noOverride: true,
          },
        ],
      });
    mount({ variant: "compact" });
    const trigger = screen.getByRole("button", { name: /Subnet\s*Automatic/ });
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    typeSubnet("10.0.1.0/25");
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /Subnet/ }).getAttribute("aria-expanded")).toBe(
        "true",
      ),
    );
    expect(await screen.findByText(/can't be created anyway/)).toBeTruthy();
  });

  it("leaves Create it anyway aria-disabled below Env Admin, with the reason", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    answer = () =>
      clean({
        findings: [{ kind: "same_env", severity: "blocking", network: "a", subnet: "10.0.5.0/24" }],
      });
    mount({ canCreateAnyway: false });
    typeSubnet("10.0.5.0/26");
    const box = await screen.findByRole("checkbox", { name: "Create it anyway" });
    expect(box.getAttribute("aria-disabled")).toBe("true");
    await waitFor(() => expect(document.body.textContent).toContain("Needs Admin on Homelab"));
    fireEvent.click(box);
    expect(box.getAttribute("aria-checked")).toBe("false");
  });

  it("says why host routes were not checked, from the answer", async () => {
    answer = () =>
      clean({
        routeChecks: [
          { node: "node-1", local: false, state: "old_agent" },
          { node: "node-2", local: false, state: "offline" },
        ],
      });
    mount();
    typeSubnet("10.0.5.0/26");
    expect(
      await screen.findByText(
        /Route check skipped on node-1: update the agent to check host routes\./,
      ),
    ).toBeTruthy();
    expect(
      screen.getByText(/Route check skipped on node-2: the agent isn't connected\./),
    ).toBeTruthy();
    expect(screen.getByText("No overlaps found.")).toBeTruthy();
  });

  it("says it did not check an IPv6 subnet", async () => {
    answer = () => clean({ ipv6NotChecked: true });
    mount();
    typeSubnet("fd00:1::/64");
    expect(await screen.findByText("IPv6 subnets aren't checked for overlaps.")).toBeTruthy();
    expect(screen.queryByText("No overlaps found.")).toBeNull();
  });

  it("shows the check error without blocking, and tells the parent nothing blocks", async () => {
    answer = () => Response.json({ title: "x", status: 500 }, { status: 500 });
    const onBlocking = vi.fn();
    mount({ onBlocking });
    typeSubnet("10.0.5.0/26");
    const line = await screen.findByText(
      "Couldn't check this subnet. You can still create the network.",
    );
    expect(line.closest("[role=status]")).toBeTruthy();
    expect(onBlocking).toHaveBeenLastCalledWith("none");
    expect(subnetInput().disabled).toBe(false);
  });

  it("shows the form's own refusal under the field and marks the input invalid", () => {
    mount({ error: "Enter a subnet like 10.20.0.0/24." });
    expect(screen.getByText("Enter a subnet like 10.20.0.0/24.")).toBeTruthy();
    expect(subnetInput().getAttribute("aria-invalid")).toBe("true");
  });

  it("renders the compact variant closed, with Automatic and then the chosen subnet", async () => {
    mount({ variant: "compact" });
    const trigger = screen.getByRole("button", { name: /Subnet\s*Automatic/ });
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(subnetInput().closest("[hidden]")).not.toBeNull();
    fireEvent.click(trigger);
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    expect(subnetInput().closest("[hidden]")).toBeNull();
    typeSubnet("10.0.5.0/26");
    const summary = screen.getByText("10.0.5.0/26", { selector: "span" });
    expect(summary.className).toContain("font-mono");
  });

  it("starts the tasks from a replica count and says so", () => {
    mount({ replicas: 6 });
    expect(tasksInput().value).toBe("6");
    expect(screen.getByText("Based on 6 replicas.")).toBeTruthy();
  });
});

describe("SubnetPicker (the wait)", () => {
  it("asks once, 400 ms after the last change, and not before", async () => {
    vi.useFakeTimers();
    checkTiming.debounceMs = CHECK_DEBOUNCE_MS;
    mount();
    typeSubnet("10.0.5.0/2");
    await act(() => vi.advanceTimersByTimeAsync(300));
    typeSubnet("10.0.5.0/26");
    await act(() => vi.advanceTimersByTimeAsync(CHECK_DEBOUNCE_MS - 1));
    expect(asked).toEqual([]);
    await act(() => vi.advanceTimersByTimeAsync(1));
    await waitUnderFakeClock(() => expect(asked).toHaveLength(1));
    expect(asked[0]).toEqual({ subnet: "10.0.5.0/26" });
  });
});
