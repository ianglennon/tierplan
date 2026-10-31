// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { SubnetCheck, SubnetCheckRequest } from "@/api/address-space";
import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import type { StagedKind, StagedObject } from "@/features/service-editor/staged";
import { StagedObjectDialog } from "@/features/service-editor/staged-object-dialog";
import { CHECK_DEBOUNCE_MS, checkTiming } from "@/features/subnet-picker/use-subnet-check";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The staged object dialog (plan 06-26, D-06; UI-SPEC "Service editor" Staged dialogs): a secret,
// config or network that is made when the service is applied, validated against the objects that
// exist and the ones already staged. A network's subnet is the compact Subnet picker (plan 10-19).

const ENV = "e1";

let onAdd: ReturnType<typeof vi.fn<(object: StagedObject) => void>>;
let onOpenChange: ReturnType<typeof vi.fn<(open: boolean) => void>>;
let client: QueryClient;
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
  onAdd = vi.fn<(object: StagedObject) => void>();
  onOpenChange = vi.fn<(open: boolean) => void>();
  asked = [];
  answer = () => clean();
  checkTiming.debounceMs = 0;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Homelab", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/address-space`) {
        return Response.json({
          pools: [{ cidr: "10.0.0.0/8", mask: 24, source: "swarm", state: "ok", percent: 3 }],
        });
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
  checkTiming.debounceMs = CHECK_DEBOUNCE_MS;
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

function dialog(kind: StagedKind, taken: string[], open: boolean, replicas?: number) {
  return (
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <StagedObjectDialog
            kind={kind}
            envId={ENV}
            replicas={replicas}
            open={open}
            onOpenChange={onOpenChange}
            taken={taken}
            onAdd={onAdd}
          />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>
  );
}

function renderDialog(kind: StagedKind, taken: string[] = [], replicas?: number) {
  const view = render(dialog(kind, taken, true, replicas));
  return {
    ...view,
    // The dialog closed and then opened again, as the picker does between two uses.
    reopen: () => {
      view.rerender(dialog(kind, taken, false, replicas));
      view.rerender(dialog(kind, taken, true, replicas));
    },
  };
}

const name = () => screen.getByLabelText("Name");
const type = (el: HTMLElement, value: string) => fireEvent.change(el, { target: { value } });

describe("StagedObjectDialog: a secret", () => {
  it("says what it does and that Docker never shows the value again", () => {
    renderDialog("secret");
    expect(screen.getByRole("dialog", { name: "Create a secret" })).toBeTruthy();
    expect(
      screen.getByText(
        "It's created when you apply the service. Docker never shows the value again.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Add secret" })).toBeTruthy();
  });

  it("needs a valid name and a value, and says which is missing", () => {
    renderDialog("secret");
    fireEvent.click(screen.getByRole("button", { name: "Add secret" }));
    expect(screen.getByText("Enter a name.")).toBeTruthy();
    expect(screen.getByText("Enter a value.")).toBeTruthy();
    expect(onAdd).not.toHaveBeenCalled();
    type(name(), "-bad");
    fireEvent.click(screen.getByRole("button", { name: "Add secret" }));
    expect(screen.getByText("Use letters, numbers, dot, dash or underscore.")).toBeTruthy();
  });

  it("refuses a name that exists or is staged already", () => {
    renderDialog("secret", ["db_pw"]);
    type(name(), "db_pw");
    type(screen.getByLabelText("Value"), "x");
    fireEvent.click(screen.getByRole("button", { name: "Add secret" }));
    expect(screen.getByText("A secret named db_pw already exists.")).toBeTruthy();
    expect(onAdd).not.toHaveBeenCalled();
  });

  it("hands over the staged secret with its value and closes", () => {
    renderDialog("secret");
    type(name(), " fresh ");
    type(screen.getByLabelText("Value"), "hunter2");
    fireEvent.click(screen.getByRole("button", { name: "Add secret" }));
    expect(onAdd).toHaveBeenCalledTimes(1);
    expect(onAdd.mock.calls[0]?.[0]).toMatchObject({
      kind: "secret",
      name: "fresh",
      value: { source: "type", text: "hunter2" },
    });
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("keeps the value in a masked field that no browser fills in", () => {
    renderDialog("secret");
    const value = screen.getByLabelText("Value");
    expect(value.getAttribute("type")).toBe("password");
    expect(value.getAttribute("autocomplete")).toBe("new-password");
  });

  it("forgets what was typed when it is closed and opened again", () => {
    const view = renderDialog("secret");
    type(name(), "fresh");
    type(screen.getByLabelText("Value"), "hunter2");
    view.reopen();
    expect((name() as HTMLInputElement).value).toBe("");
    expect((screen.getByLabelText("Value") as HTMLInputElement).value).toBe("");
  });

  it("closes on Cancel without handing anything over", () => {
    renderDialog("secret");
    type(name(), "fresh");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onAdd).not.toHaveBeenCalled();
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });
});

describe("StagedObjectDialog: a config", () => {
  it("has content and an optional templating, and hands over the choice", () => {
    renderDialog("config");
    expect(screen.getByRole("dialog", { name: "Create a config" })).toBeTruthy();
    expect(screen.getByText("It's created when you apply the service.")).toBeTruthy();
    type(name(), "app.conf");
    type(screen.getByLabelText("Content"), "level=debug");
    expect(screen.getByLabelText("Templating (optional)")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Add config" }));
    expect(onAdd.mock.calls[0]?.[0]).toMatchObject({
      kind: "config",
      name: "app.conf",
      templating: "none",
      value: { text: "level=debug" },
    });
  });

  it("needs content", () => {
    renderDialog("config");
    type(name(), "app.conf");
    fireEvent.click(screen.getByRole("button", { name: "Add config" }));
    expect(screen.getByText("Enter a value.")).toBeTruthy();
    expect(onAdd).not.toHaveBeenCalled();
  });
});

describe("StagedObjectDialog: a network", () => {
  const subnetInput = () => screen.getByLabelText("Subnet") as HTMLInputElement;
  const openSubnet = () =>
    fireEvent.click(screen.getByRole("button", { name: /Subnet\s*Automatic/ }));

  it("is made as an attachable overlay, with Driver read-only under Advanced", async () => {
    renderDialog("network");
    expect(screen.getByRole("dialog", { name: "Create a network" })).toBeTruthy();
    expect(
      screen.getByText(
        "It's created when you apply the service, as an attachable overlay network.",
      ),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
    const driver = await within(screen.getByRole("dialog")).findByLabelText("Driver");
    expect((driver as HTMLInputElement).value).toBe("overlay");
    expect((driver as HTMLInputElement).readOnly).toBe(true);
  });

  it("needs only a name: the Subnet section reads Automatic and stages no subnet", async () => {
    renderDialog("network");
    const trigger = screen.getByRole("button", { name: /Subnet\s*Automatic/ });
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    type(name(), "front");
    fireEvent.click(screen.getByRole("button", { name: "Add network" }));
    await waitFor(() => expect(onAdd).toHaveBeenCalledTimes(1));
    expect(onAdd.mock.calls[0]?.[0]).toEqual({ kind: "network", name: "front" });
  });

  it("opens the compact picker and stages the suggestion", async () => {
    answer = (body) =>
      body.suggest ? clean({ suggestion: { subnet: "10.0.2.0/26", pool: "10.0.0.0/8" } }) : clean();
    renderDialog("network");
    openSubnet();
    fireEvent.click(
      screen.getByRole("button", { name: "Suggest a free subnet of the chosen size" }),
    );
    await waitFor(() => expect(subnetInput().value).toBe("10.0.2.0/26"));
    // Closed or open, the section now reads the chosen subnet in mono.
    expect(screen.getByText("10.0.2.0/26", { selector: "button span" }).className).toContain(
      "font-mono",
    );
    expect(asked[0]).toMatchObject({ driver: "overlay", suggest: true });
    type(name(), "front");
    fireEvent.click(screen.getByRole("button", { name: "Add network" }));
    await waitFor(() => expect(onAdd).toHaveBeenCalledTimes(1));
    expect(onAdd.mock.calls[0]?.[0]).toEqual({
      kind: "network",
      name: "front",
      subnet: "10.0.2.0/26",
    });
  });

  it("prefills tasks from replicas", async () => {
    answer = () => clean({ needed: 19, recommendedMask: 27, recommendedUsable: 29 });
    renderDialog("network", [], 6);
    openSubnet();
    expect((screen.getByLabelText("Expected tasks") as HTMLInputElement).value).toBe("6");
    expect(screen.getByText("Based on 6 replicas.")).toBeTruthy();
    await waitFor(() => expect(asked.some((b) => b.tasks === 6)).toBe(true));
  });

  it("says so when the subnet is not a subnet, and opens the section to show it", async () => {
    renderDialog("network");
    openSubnet();
    type(subnetInput(), "10.9.0.1");
    type(name(), "front");
    fireEvent.click(screen.getByRole("button", { name: "Add network" }));
    expect(screen.getByText("The subnet must be in CIDR form, like 10.20.0.0/24.")).toBeTruthy();
    expect(onAdd).not.toHaveBeenCalled();
    type(subnetInput(), "");
    fireEvent.click(screen.getByRole("button", { name: "Add network" }));
    await waitFor(() => expect(onAdd).toHaveBeenCalledTimes(1));
    expect(onAdd.mock.calls[0]?.[0]).toMatchObject({ kind: "network", name: "front" });
  });

  it("stages createAnyway after ticking, and not before", async () => {
    answer = (body) =>
      body.subnet
        ? clean({
            findings: [
              {
                kind: "same_env",
                severity: "blocking",
                network: "existing",
                subnet: "10.77.0.0/24",
              },
            ],
          })
        : clean();
    renderDialog("network");
    openSubnet();
    type(subnetInput(), "10.77.0.0/25");
    type(name(), "front");
    const box = await screen.findByRole("checkbox", { name: "Create it anyway" });
    // The editor's Add waits for the tick.
    const add = screen.getByRole("button", { name: "Add network" });
    expect(add.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(add);
    expect(onAdd).not.toHaveBeenCalled();
    fireEvent.click(box);
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Add network" }).getAttribute("aria-disabled"),
      ).toBe(null),
    );
    fireEvent.click(screen.getByRole("button", { name: "Add network" }));
    expect(onAdd.mock.calls[0]?.[0]).toEqual({
      kind: "network",
      name: "front",
      subnet: "10.77.0.0/25",
      createAnyway: true,
    });
  });
});
