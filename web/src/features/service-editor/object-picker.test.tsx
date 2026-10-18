// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ObjectPicker, type PickedObject } from "@/features/service-editor/object-picker";
import { ServiceForm } from "@/features/service-editor/service-form";
import {
  createStagedStore,
  StagedContext,
  type StagedKind,
  type StagedStore,
} from "@/features/service-editor/staged";
import type { SpecValue } from "@/features/spec-editor/spec-format";
import "@/i18n";
import { makeMe } from "@/test-me";

// The object picker (plan 06-26, D-06, D-13; UI-SPEC "Service editor" Picker, Interaction "Object
// pickers"): existing secrets, configs and networks in a combobox, and Create new… last, which
// stages an object in the editor without leaving it.

const ENV = "e1";

const secret = (id: string, name: string) => ({
  id,
  name,
  labels: {},
  createdAt: "2026-10-01T10:00:00Z",
  updatedAt: "2026-10-01T10:00:00Z",
  usedBy: 0,
  owner: "",
  ownerKind: "",
});

const config = (id: string, name: string) => ({ ...secret(id, name), templating: "none" });

const net = (id: string, name: string, extra: Record<string, unknown> = {}) => ({
  id,
  name,
  driver: "overlay",
  scope: "swarm",
  builtIn: false,
  attachable: true,
  internal: false,
  ipv6: false,
  node: "Cluster",
  nodeId: "n1",
  owner: "",
  ownerKind: "",
  subnets: [],
  attachedContainers: 0,
  attachedServices: 0,
  ...extra,
});

let secrets: unknown[];
let configs: unknown[];
let networks: unknown[];
let me: ReturnType<typeof makeMe>;
let store: StagedStore;
let onSelect: ReturnType<typeof vi.fn<(picked: PickedObject) => void>>;
let writes: string[];

beforeEach(() => {
  secrets = [secret("sec1", "db_pw"), secret("sec2", "api_key")];
  configs = [config("cfg1", "app_conf"), config("cfg2", "nginx.conf")];
  networks = [
    net("net1", "shop_front"),
    net("net-bridge", "bridge", { scope: "local", driver: "bridge", builtIn: true }),
    net("net-ingress", "ingress", { builtIn: true }),
    net("net-host", "host", { scope: "local", driver: "host", builtIn: true }),
  ];
  me = makeMe({ envs: { [ENV]: "admin" } });
  store = createStagedStore();
  onSelect = vi.fn<(picked: PickedObject) => void>();
  writes = [];
  setCsrfToken(null);
  queryClient.setDefaultOptions({ queries: { retry: false } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(
        typeof input === "string" ? input : input instanceof URL ? input.href : input.url,
        "http://localhost",
      );
      const method = input instanceof Request ? input.method : "GET";
      if (method !== "GET") writes.push(`${method} ${url.pathname}`);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === `/api/env/${ENV}/secrets`) {
        return Response.json({
          items: secrets,
          total: secrets.length,
          unfilteredTotal: secrets.length,
          capped: false,
        });
      }
      if (url.pathname === `/api/env/${ENV}/configs`) {
        return Response.json({
          items: configs,
          total: configs.length,
          unfilteredTotal: configs.length,
          capped: false,
        });
      }
      if (url.pathname === `/api/env/${ENV}/networks`) {
        return Response.json({
          items: networks,
          total: networks.length,
          truncated: false,
          nodes: [],
          notReporting: [],
        });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
});

const NEEDS_REVEAL = "Needs the reveal secrets permission to add a secret";

function renderPicker(
  kind: StagedKind = "secret",
  value: PickedObject | null = null,
  restrict?: { keep: readonly string[]; reason: string },
) {
  return render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <StagedContext.Provider value={store}>
          <ObjectPicker
            kind={kind}
            envId={ENV}
            label={kind === "secret" ? "Secret" : "Network"}
            value={value}
            onSelect={onSelect}
            restrict={restrict}
          />
        </StagedContext.Provider>
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

const trigger = () => screen.getByRole("combobox", { name: /Secret|Network/ });

async function open() {
  fireEvent.click(trigger());
  return screen.findByRole("listbox");
}

describe("ObjectPicker", () => {
  it("reads Choose a secret until one is chosen, then the name of the one that is", () => {
    const { unmount } = renderPicker("secret");
    expect(trigger().textContent).toContain("Choose a secret");
    unmount();
    renderPicker("secret", { name: "db_pw", id: "sec1", staged: false });
    expect(trigger().textContent).toContain("db_pw");
  });

  it("lists the existing objects, with a search and Create new secret… last", async () => {
    renderPicker();
    const list = await open();
    expect(screen.getByPlaceholderText("Find a secret…")).toBeTruthy();
    await within(list).findByRole("option", { name: "db_pw" });
    const items = within(list).getAllByRole("option");
    expect(items.map((i) => i.textContent)).toEqual(["api_key", "db_pw", "Create new secret…"]);
  });

  it("filters the list as the person types", async () => {
    renderPicker();
    await open();
    await screen.findByRole("option", { name: "db_pw" });
    fireEvent.change(screen.getByPlaceholderText("Find a secret…"), { target: { value: "api" } });
    await waitFor(() => expect(screen.queryByRole("option", { name: "db_pw" })).toBeNull());
    expect(screen.getByRole("option", { name: "api_key" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "Create new secret…" })).toBeTruthy();
  });

  it("says No secrets yet. when there are none, and still offers Create new", async () => {
    secrets = [];
    renderPicker();
    await open();
    expect(await screen.findByText("No secrets yet.")).toBeTruthy();
    expect(screen.getByRole("option", { name: "Create new secret…" })).toBeTruthy();
  });

  it("hands over the object that was chosen and returns focus to the trigger", async () => {
    renderPicker();
    await open();
    fireEvent.click(await screen.findByRole("option", { name: "api_key" }));
    expect(onSelect).toHaveBeenCalledWith({ name: "api_key", id: "sec2", staged: false });
    await waitFor(() => expect(screen.queryByRole("listbox")).toBeNull());
    expect(document.activeElement).toBe(trigger());
  });

  it("closes on Escape and returns focus to the trigger", async () => {
    renderPicker();
    await open();
    fireEvent.keyDown(screen.getByPlaceholderText("Find a secret…"), { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("listbox")).toBeNull());
    expect(document.activeElement).toBe(trigger());
    expect(onSelect).not.toHaveBeenCalled();
  });

  it("lists a staged object with the New badge", async () => {
    store.put({ kind: "secret", name: "fresh", value: { source: "type", text: "x", file: null } });
    renderPicker();
    await open();
    const option = await screen.findByRole("option", { name: /fresh/ });
    expect(within(option).getByText("New")).toBeTruthy();
  });

  it("leaves out the networks a service can't attach to: the built-in ones and those that are local to a node", async () => {
    renderPicker("network");
    await open();
    await screen.findByRole("option", { name: "shop_front" });
    const names = screen.getAllByRole("option").map((o) => o.textContent);
    expect(names).toEqual(["shop_front", "Create new network…"]);
  });

  it("stages a new object without leaving the editor, selects it and returns focus to the trigger", async () => {
    renderPicker();
    await open();
    fireEvent.click(await screen.findByRole("option", { name: "Create new secret…" }));
    const dialog = await screen.findByRole("dialog", { name: "Create a secret" });
    fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "fresh" } });
    fireEvent.change(within(dialog).getByLabelText("Value"), { target: { value: "hunter2" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Add secret" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(onSelect).toHaveBeenCalledWith({ name: "fresh", staged: true });
    expect(store.get("secret", "fresh")?.value?.text).toBe("hunter2");
    expect(document.activeElement).toBe(trigger());
    // Nothing was created on the server: that happens when the service is applied.
    expect(writes).toEqual([]);
  });

  it("offers Create new only to someone who may create the kind, and says what it takes", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    renderPicker();
    await open();
    const create = await screen.findByRole("option", { name: /Create new secret…/ });
    await waitFor(() => expect(create.getAttribute("aria-disabled")).toBe("true"));
    expect(create.textContent).toContain("Needs Admin");
    fireEvent.click(create);
    expect(screen.queryByRole("dialog", { name: "Create a secret" })).toBeNull();
  });

  it("offers a caller without the reveal secrets permission only the secrets the service uses, and says why", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    renderPicker("secret", null, { keep: ["sec1", "db_pw"], reason: NEEDS_REVEAL });
    await open();
    const kept = await screen.findByRole("option", { name: "db_pw" });
    const refused = screen.getByRole("option", { name: /api_key/ });
    expect(kept.getAttribute("aria-disabled")).not.toBe("true");
    expect(refused.getAttribute("aria-disabled")).toBe("true");
    expect(refused.textContent).toContain(NEEDS_REVEAL);
    fireEvent.click(refused);
    expect(onSelect).not.toHaveBeenCalled();
    fireEvent.click(kept);
    expect(onSelect).toHaveBeenCalledWith({ name: "db_pw", id: "sec1", staged: false });
  });

  it("skips a disabled secret with the arrow keys and Enter does not choose it", async () => {
    renderPicker("secret", null, { keep: ["sec1", "db_pw"], reason: NEEDS_REVEAL });
    await open();
    const search = screen.getByPlaceholderText("Find a secret…");
    const refused = await screen.findByRole("option", { name: /api_key/ });
    const kept = screen.getByRole("option", { name: "db_pw" });
    await waitFor(() => expect(kept.getAttribute("aria-selected")).toBe("true"));
    for (const key of ["ArrowDown", "ArrowDown", "ArrowDown", "ArrowUp", "ArrowUp", "ArrowUp"]) {
      fireEvent.keyDown(search, { key });
      expect(refused.getAttribute("aria-selected")).not.toBe("true");
    }
    fireEvent.keyDown(search, { key: "Enter" });
    expect(onSelect).toHaveBeenCalledTimes(1);
    expect(onSelect).toHaveBeenCalledWith({ name: "db_pw", id: "sec1", staged: false });
  });

  it("returns focus to the trigger on Escape from a restricted picker", async () => {
    renderPicker("secret", null, { keep: ["sec1", "db_pw"], reason: NEEDS_REVEAL });
    await open();
    await screen.findByRole("option", { name: /api_key/ });
    fireEvent.keyDown(screen.getByPlaceholderText("Find a secret…"), { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("listbox")).toBeNull());
    expect(document.activeElement).toBe(trigger());
    expect(onSelect).not.toHaveBeenCalled();
  });
});

// The secrets card hands the picker the restriction: it knows the secrets the service uses as the
// server holds them, and whether the caller may reveal secrets (D-13 as amended by plan 06-47).
describe("the secrets card of the Form", () => {
  const mounted = (): SpecValue => ({
    Name: "api",
    TaskTemplate: {
      ContainerSpec: {
        Image: "nginx:1.27",
        Secrets: [
          { SecretID: "sec1", SecretName: "db_pw", File: { Name: "db_pw", UID: "0", GID: "0" } },
        ],
      },
    },
    Mode: { Replicated: { Replicas: 2 } },
  });

  function renderForm(attached: readonly string[], mode: "edit" | "create" = "edit") {
    return render(
      <QueryClientProvider client={queryClient}>
        <TooltipProvider>
          <ServiceForm
            spec={mode === "edit" ? mounted() : { Name: "api" }}
            mode={mode}
            envId={ENV}
            staged={store}
            attachedSecrets={attached}
            onSpecChange={() => {}}
          />
        </TooltipProvider>
      </QueryClientProvider>,
    );
  }

  async function openSecretRow() {
    fireEvent.click(screen.getByRole("combobox", { name: "Secret" }));
    await screen.findByRole("listbox");
  }

  it("restricts its pickers for an Operator without the reveal permission", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    renderForm(["sec1", "db_pw"]);
    await openSecretRow();
    const refused = await screen.findByRole("option", { name: /api_key/ });
    expect(refused.getAttribute("aria-disabled")).toBe("true");
    expect(refused.textContent).toContain(NEEDS_REVEAL);
    expect(screen.getByRole("option", { name: "db_pw" }).getAttribute("aria-disabled")).not.toBe(
      "true",
    );
  });

  it("offers every secret to an environment Admin", async () => {
    me = makeMe({ envs: { [ENV]: "admin" } });
    renderForm(["sec1", "db_pw"]);
    await openSecretRow();
    const other = await screen.findByRole("option", { name: "api_key" });
    expect(other.getAttribute("aria-disabled")).not.toBe("true");
    expect(screen.queryByText(NEEDS_REVEAL)).toBeNull();
  });

  it("offers every secret to an Operator who was granted the reveal permission", async () => {
    me = makeMe({ envs: { [ENV]: "operator" }, extras: { [ENV]: ["secrets.reveal"] } });
    renderForm(["sec1", "db_pw"]);
    await openSecretRow();
    const other = await screen.findByRole("option", { name: "api_key" });
    expect(other.getAttribute("aria-disabled")).not.toBe("true");
    expect(screen.queryByText(NEEDS_REVEAL)).toBeNull();
  });

  it("restricts nothing for configs and networks", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    render(
      <QueryClientProvider client={queryClient}>
        <TooltipProvider>
          <ServiceForm
            spec={{
              Name: "api",
              TaskTemplate: {
                ContainerSpec: {
                  Image: "nginx:1.27",
                  Configs: [
                    {
                      ConfigID: "cfg1",
                      ConfigName: "app_conf",
                      File: { Name: "/etc/app.conf", UID: "0", GID: "0", Mode: 292 },
                    },
                  ],
                },
                Networks: [{ Target: "net1" }],
              },
              Mode: { Replicated: { Replicas: 2 } },
            }}
            mode="edit"
            envId={ENV}
            staged={store}
            attachedSecrets={[]}
            onSpecChange={() => {}}
          />
        </TooltipProvider>
      </QueryClientProvider>,
    );
    for (const [label, other] of [
      ["Config", "nginx.conf"],
      ["Network", "shop_front"],
    ] as const) {
      fireEvent.click(screen.getByRole("combobox", { name: label }));
      const option = await screen.findByRole("option", { name: other });
      expect(option.getAttribute("aria-disabled")).not.toBe("true");
      for (const o of screen.getAllByRole("option")) {
        if (o.textContent?.startsWith("Create new")) continue;
        expect(o.getAttribute("aria-disabled")).not.toBe("true");
      }
      expect(screen.queryByText(NEEDS_REVEAL)).toBeNull();
      fireEvent.keyDown(screen.getByRole("listbox"), { key: "Escape" });
      await waitFor(() => expect(screen.queryByRole("listbox")).toBeNull());
    }
  });

  it("a new service's secret picker is not restricted for an env Admin", async () => {
    me = makeMe({ envs: { [ENV]: "admin" } });
    renderForm([], "create");
    fireEvent.click(screen.getByRole("button", { name: "Add secret" }));
    await openSecretRow();
    for (const name of ["db_pw", "api_key"]) {
      const option = await screen.findByRole("option", { name });
      expect(option.getAttribute("aria-disabled")).not.toBe("true");
    }
    expect(screen.queryByText(NEEDS_REVEAL)).toBeNull();
  });
});
