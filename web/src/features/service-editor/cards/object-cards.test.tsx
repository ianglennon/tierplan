// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ServiceForm } from "@/features/service-editor/service-form";
import { getPath } from "@/features/service-editor/spec-patch";
import { createStagedStore, type StagedStore } from "@/features/service-editor/staged";
import type { SpecValue } from "@/features/spec-editor/spec-format";
import "@/i18n";
import { makeMe } from "@/test-me";

// The Form tab's Secrets, Configs and Networks cards (plan 06-26, D-01, D-06; UI-SPEC "Service
// editor"): each row picks an object and writes its reference into the spec, and nothing else moves.

const ENV = "e1";
const SECRETS = "TaskTemplate.ContainerSpec.Secrets";
const CONFIGS = "TaskTemplate.ContainerSpec.Configs";
const NETWORKS = "TaskTemplate.Networks";

const stamp = {
  createdAt: "2026-10-01T10:00:00Z",
  updatedAt: "2026-10-01T10:00:00Z",
  labels: {},
  owner: "",
  ownerKind: "",
};
const secretItem = (id: string, name: string) => ({ id, name, usedBy: 0, ...stamp });
const configItem = (id: string, name: string) => ({
  id,
  name,
  usedBy: 0,
  templating: "none",
  ...stamp,
});
const netItem = (id: string, name: string) => ({
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
});

const base = (): SpecValue => ({
  Name: "api",
  TaskTemplate: { ContainerSpec: { Image: "nginx:1.27" } },
  Mode: { Replicated: { Replicas: 2 } },
});

function withContainer(
  extra: Record<string, unknown>,
  task: Record<string, unknown> = {},
): SpecValue {
  return {
    Name: "api",
    TaskTemplate: { ContainerSpec: { Image: "nginx:1.27", ...extra }, ...task },
    Mode: { Replicated: { Replicas: 2 } },
  };
}

const secretRef = (id: string, name: string, file: string) => ({
  SecretID: id,
  SecretName: name,
  File: { Name: file, UID: "0", GID: "0", Mode: 292 },
});

let latest: SpecValue;
let validity: boolean[];
let store: StagedStore;

function Host({ initial = base() }: { initial?: SpecValue }) {
  const [spec, setSpec] = useState(initial);
  latest = spec;
  return (
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <ServiceForm
          spec={spec}
          mode="edit"
          envId={ENV}
          staged={store}
          onSpecChange={(next) => {
            latest = next;
            setSpec(next);
          }}
          onValidityChange={(v) => validity.push(v)}
        />
      </TooltipProvider>
    </QueryClientProvider>
  );
}

beforeEach(() => {
  latest = {};
  validity = [];
  store = createStagedStore();
  setCsrfToken(null);
  queryClient.setDefaultOptions({ queries: { retry: false } });
  const me = makeMe({ envs: { [ENV]: "admin" } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(
        typeof input === "string" ? input : input instanceof URL ? input.href : input.url,
        "http://localhost",
      );
      const list = (items: unknown[]) => ({
        items,
        total: items.length,
        unfilteredTotal: items.length,
        capped: false,
      });
      switch (url.pathname) {
        case "/api/me":
          return Response.json(me);
        case `/api/env/${ENV}/secrets`:
          return Response.json(list([secretItem("sec1", "db_pw"), secretItem("sec2", "api_key")]));
        case `/api/env/${ENV}/configs`:
          return Response.json(
            list([configItem("cfg1", "app_conf"), configItem("cfg2", "nginx.conf")]),
          );
        case `/api/env/${ENV}/networks`:
          return Response.json({
            items: [netItem("net1", "shop_front"), netItem("net2", "shop_back")],
            total: 2,
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

const type = (el: HTMLElement, value: string) => fireEvent.change(el, { target: { value } });
const card = (name: string) => within(screen.getByRole("region", { name }));
const lastValid = () => validity[validity.length - 1];

async function pick(scope: ReturnType<typeof within>, option: string | RegExp) {
  fireEvent.click(scope.getByRole("combobox"));
  fireEvent.click(await screen.findByRole("option", { name: option }));
}

describe("the Secrets, Configs and Networks cards", () => {
  it("sit between Ports and Mounts, in that order", () => {
    render(<Host />);
    const titles = screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent);
    expect(titles.slice(2, 7)).toEqual(["Ports", "Secrets", "Configs", "Networks", "Mounts"]);
  });
});

describe("SecretsCard", () => {
  const withSecrets = () => withContainer({ Secrets: [secretRef("sec1", "db_pw", "db_pw.txt")] });
  const row = (n: number) => within(screen.getByRole("group", { name: `Secret ${n}` }));

  it("shows a row for each secret with its file name, UID, GID and mode, and where it is mounted", () => {
    render(<Host initial={withSecrets()} />);
    expect(row(1).getByRole("combobox").textContent).toContain("db_pw");
    expect((row(1).getByLabelText("File name") as HTMLInputElement).value).toBe("db_pw.txt");
    expect((row(1).getByLabelText("UID") as HTMLInputElement).value).toBe("0");
    expect((row(1).getByLabelText("GID") as HTMLInputElement).value).toBe("0");
    expect((row(1).getByLabelText("Mode") as HTMLInputElement).value).toBe("0444");
    expect(row(1).getByText("Mounted at /run/secrets/db_pw.txt")).toBeTruthy();
  });

  it("adds a secret from the picker with the file name set to its name and the default mode", async () => {
    render(<Host />);
    fireEvent.click(card("Secrets").getByRole("button", { name: "Add secret" }));
    await pick(row(1), "api_key");
    await waitFor(() =>
      expect(getPath(latest, SECRETS)).toEqual([secretRef("sec2", "api_key", "api_key")]),
    );
    expect((row(1).getByLabelText("File name") as HTMLInputElement).value).toBe("api_key");
    expect(lastValid()).toBe(true);
  });

  it("changes only the field that was edited and keeps the rest of the reference", () => {
    render(<Host initial={withSecrets()} />);
    type(row(1).getByLabelText("File name"), "pw");
    expect(getPath(latest, SECRETS)).toEqual([
      {
        SecretID: "sec1",
        SecretName: "db_pw",
        File: { Name: "pw", UID: "0", GID: "0", Mode: 292 },
      },
    ]);
    type(row(1).getByLabelText("Mode"), "0400");
    expect((getPath(latest, SECRETS) as { File: { Mode: number } }[])[0]?.File.Mode).toBe(0o400);
  });

  it("tells off a mode that is not octal and leaves the spec as it was", () => {
    render(<Host initial={withSecrets()} />);
    type(row(1).getByLabelText("Mode"), "99");
    expect(screen.getByText("Enter a mode like 0444.")).toBeTruthy();
    expect((getPath(latest, SECRETS) as { File: { Mode: number } }[])[0]?.File.Mode).toBe(292);
    expect(lastValid()).toBe(false);
  });

  it("removes a row, and the whole list when the last one goes", () => {
    render(<Host initial={withSecrets()} />);
    fireEvent.click(row(1).getByRole("button", { name: "Remove secret 1" }));
    expect(getPath(latest, SECRETS)).toBeUndefined();
  });

  it("says a secret can only be attached once", async () => {
    render(<Host initial={withSecrets()} />);
    fireEvent.click(card("Secrets").getByRole("button", { name: "Add secret" }));
    await pick(row(2), "db_pw");
    expect(await screen.findByText("That secret is already attached.")).toBeTruthy();
    expect(lastValid()).toBe(false);
  });

  it("writes a staged secret by name alone, because it has no ID until it is created", async () => {
    store.put({ kind: "secret", name: "fresh", value: { source: "type", text: "x", file: null } });
    render(<Host />);
    fireEvent.click(card("Secrets").getByRole("button", { name: "Add secret" }));
    await pick(row(1), /^fresh/);
    await waitFor(() =>
      expect(getPath(latest, SECRETS)).toEqual([
        { SecretName: "fresh", File: { Name: "fresh", UID: "0", GID: "0", Mode: 292 } },
      ]),
    );
  });

  it("leaves a row nobody has chosen a secret for out of the spec, and says so once touched", async () => {
    render(<Host />);
    fireEvent.click(card("Secrets").getByRole("button", { name: "Add secret" }));
    expect(getPath(latest, SECRETS)).toBeUndefined();
    fireEvent.blur(row(1).getByLabelText("File name"));
    expect(await screen.findByText("Choose a secret.")).toBeTruthy();
    expect(lastValid()).toBe(false);
  });
});

describe("ConfigsCard", () => {
  const cfgRef = (id: string, name: string, path: string) => ({
    ConfigID: id,
    ConfigName: name,
    File: { Name: path, UID: "0", GID: "0", Mode: 292 },
  });
  const row = (n: number) => within(screen.getByRole("group", { name: `Config ${n}` }));

  it("defaults the path to /{name} for a config picked from the list", async () => {
    render(<Host />);
    fireEvent.click(card("Configs").getByRole("button", { name: "Add config" }));
    await pick(row(1), "nginx.conf");
    await waitFor(() =>
      expect(getPath(latest, CONFIGS)).toEqual([cfgRef("cfg2", "nginx.conf", "/nginx.conf")]),
    );
    expect((row(1).getByLabelText("Path") as HTMLInputElement).value).toBe("/nginx.conf");
  });

  it("needs an absolute path once it is touched", () => {
    render(
      <Host initial={withContainer({ Configs: [cfgRef("cfg1", "app_conf", "/etc/app.conf")] })} />,
    );
    type(row(1).getByLabelText("Path"), "etc/app.conf");
    expect(screen.getByText("Enter an absolute path that starts with /.")).toBeTruthy();
    expect(lastValid()).toBe(false);
    type(row(1).getByLabelText("Path"), "/etc/app2.conf");
    expect(lastValid()).toBe(true);
  });

  it("keeps a runtime config, which is not mounted anywhere, as it is", () => {
    const runtime = { ConfigID: "cfg9", ConfigName: "rt", Runtime: {} };
    render(
      <Host
        initial={withContainer({ Configs: [runtime, cfgRef("cfg1", "app_conf", "/etc/app.conf")] })}
      />,
    );
    fireEvent.click(row(1).getByRole("button", { name: "Remove config 1" }));
    expect(getPath(latest, CONFIGS)).toEqual([runtime]);
  });
});

describe("NetworksCard", () => {
  const row = (n: number) => within(screen.getByRole("group", { name: `Network ${n}` }));

  it("shows the network by name when the spec holds its ID, with its aliases", async () => {
    render(
      <Host
        initial={withContainer({}, { Networks: [{ Target: "net1", Aliases: ["web", "api"] }] })}
      />,
    );
    await waitFor(() => expect(row(1).getByRole("combobox").textContent).toContain("shop_front"));
    expect((row(1).getByLabelText("Aliases") as HTMLInputElement).value).toBe("web, api");
  });

  it("attaches a network from the picker by its ID", async () => {
    render(<Host />);
    fireEvent.click(card("Networks").getByRole("button", { name: "Add network" }));
    await pick(row(1), "shop_back");
    await waitFor(() => expect(getPath(latest, NETWORKS)).toEqual([{ Target: "net2" }]));
  });

  it("writes the aliases as a list and keeps what else the attachment holds", () => {
    render(
      <Host
        initial={withContainer({}, { Networks: [{ Target: "net1", DriverOpts: { a: "b" } }] })}
      />,
    );
    type(row(1).getByLabelText("Aliases"), "web, api ,");
    expect(getPath(latest, NETWORKS)).toEqual([
      { Target: "net1", Aliases: ["web", "api"], DriverOpts: { a: "b" } },
    ]);
    type(row(1).getByLabelText("Aliases"), "");
    expect(getPath(latest, NETWORKS)).toEqual([{ Target: "net1", DriverOpts: { a: "b" } }]);
  });

  it("writes a staged network by name, for the server to swap for its ID", async () => {
    store.put({ kind: "network", name: "front" });
    render(<Host />);
    fireEvent.click(card("Networks").getByRole("button", { name: "Add network" }));
    await pick(row(1), /^front/);
    await waitFor(() => expect(getPath(latest, NETWORKS)).toEqual([{ Target: "front" }]));
  });

  it("says a network can only be attached once", async () => {
    render(<Host initial={withContainer({}, { Networks: [{ Target: "net1" }] })} />);
    fireEvent.click(card("Networks").getByRole("button", { name: "Add network" }));
    await pick(row(2), "shop_front");
    expect(await screen.findByText("That network is already attached.")).toBeTruthy();
  });
});
