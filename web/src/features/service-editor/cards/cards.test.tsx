// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { placementRetry } from "@/api/placement";
import { queryClient } from "@/api/query-client";
import { node } from "@/features/placement/fixtures";
import { ServiceForm } from "@/features/service-editor/service-form";
import { getPath } from "@/features/service-editor/spec-patch";
import everyField from "@/features/service-editor/testdata/every-field-spec.json";
import type { SpecValue } from "@/features/spec-editor/spec-format";
import "@/i18n";

// The Form tab's Mounts, Placement, Restart policy and Update policy cards (plan 06-21, D-01,
// D-12 amended, D-38, RES-03; UI-SPEC "Service editor"): each control reads and writes its own
// paths of the spec that was loaded, and nothing else moves.

const MOUNTS = "TaskTemplate.ContainerSpec.Mounts";
const NODES = [
  node({ id: "n1", hostname: "mgr-1", role: "manager", labels: { disk: "ssd" } }),
  node({ id: "n2", hostname: "wrk-1", role: "worker", labels: { disk: "ssd" } }),
  node({ id: "n3", hostname: "wrk-2", role: "worker", labels: {} }),
  node({ id: "n4", hostname: "wrk-3", role: "worker", labels: { disk: "hdd" } }),
];

const bare = (): SpecValue => ({
  Name: "api",
  TaskTemplate: { ContainerSpec: { Image: "nginx:1.27" } },
  Mode: { Replicated: { Replicas: 2 } },
});

const full = () => structuredClone(everyField) as SpecValue;

let latest: SpecValue;
let validity: boolean[];
let placementAnswer: () => Response;

function Host({ initial = bare() }: { initial?: SpecValue }) {
  const [spec, setSpec] = useState(initial);
  latest = spec;
  return (
    <QueryClientProvider client={queryClient}>
      <ServiceForm
        spec={spec}
        mode="edit"
        envId="e1"
        onSpecChange={(next) => {
          latest = next;
          setSpec(next);
        }}
        onValidityChange={(v) => validity.push(v)}
      />
    </QueryClientProvider>
  );
}

beforeEach(() => {
  latest = {};
  validity = [];
  setCsrfToken(null);
  placementRetry.attempts = 0;
  placementAnswer = () => Response.json({ nodes: NODES, tasks: [], services: [] });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.endsWith("/api/env/e1/placement")) return placementAnswer();
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  placementRetry.attempts = 3;
});

const type = (el: HTMLElement, value: string) => fireEvent.change(el, { target: { value } });
const card = (name: string) => screen.getByRole("region", { name });
const lastValid = () => validity[validity.length - 1];

async function choose(combobox: HTMLElement, option: string) {
  fireEvent.keyDown(combobox, { key: "Enter" });
  fireEvent.click(await screen.findByRole("option", { name: option }));
}

describe("the cards", () => {
  it("renders Secrets, Configs, Networks, Mounts, Placement, Restart policy and Update policy between the older cards, in order", () => {
    render(<Host />);
    const titles = screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent);
    expect(titles).toEqual([
      "Basics",
      "Environment variables",
      "Ports",
      "Secrets",
      "Configs",
      "Networks",
      "Mounts",
      "Resources",
      "Placement",
      "Restart policy",
      "Update policy",
    ]);
  });
});

describe("MountsCard", () => {
  const mount = (n: number) => within(screen.getByRole("group", { name: `Mount ${n}` }));
  const withMounts = (mounts: unknown[]): SpecValue => {
    const s = full();
    (s.TaskTemplate as { ContainerSpec: Record<string, unknown> }).ContainerSpec.Mounts = mounts;
    return s;
  };

  it("shows a row for each mount with its type, source, destination and read-only flag", () => {
    render(<Host initial={full()} />);
    const one = mount(1);
    expect((one.getByLabelText("Source") as HTMLInputElement).value).toBe("web-data");
    expect((one.getByLabelText("Destination") as HTMLInputElement).value).toBe("/data");
    expect(one.getByRole("combobox", { name: "Type" }).textContent).toBe("Volume");
    expect(one.getByRole("switch", { name: "Read-only" }).getAttribute("aria-checked")).toBe(
      "false",
    );
    const two = mount(2);
    expect((two.getByLabelText("Source") as HTMLInputElement).value).toBe("/srv/conf");
    expect(two.getByRole("combobox", { name: "Type" }).textContent).toBe("Bind");
    expect(two.getByRole("switch", { name: "Read-only" }).getAttribute("aria-checked")).toBe(
      "true",
    );
    const three = mount(3);
    expect(three.getByRole("combobox", { name: "Type" }).textContent).toBe("Tmpfs");
    expect(three.queryByLabelText("Source")).toBeNull();
  });

  it("shows a mount of a type the form does not offer as it is, and carries it through", () => {
    render(<Host initial={full()} />);
    const four = mount(4);
    expect(four.getByRole("combobox", { name: "Type" }).textContent).toBe("image");
    expect((four.getByLabelText("Source") as HTMLInputElement).value).toBe(
      "docker.io/library/alpine:3",
    );
    fireEvent.click(mount(2).getByRole("switch", { name: "Read-only" }));
    const mounts = getPath(latest, MOUNTS) as Record<string, unknown>[];
    const original = getPath(full(), MOUNTS) as Record<string, unknown>[];
    expect(mounts[0]).toEqual(original[0]);
    expect(mounts[2]).toEqual(original[2]);
    expect(mounts[3]).toEqual(original[3]);
    expect(mounts[1]).toEqual({ ...original[1], ReadOnly: false });
  });

  it("changes only the destination of a mount and keeps the options the form does not show", () => {
    render(<Host initial={full()} />);
    type(mount(1).getByLabelText("Destination"), "/var/data");
    const mounts = getPath(latest, MOUNTS) as Record<string, unknown>[];
    const original = getPath(full(), MOUNTS) as Record<string, unknown>[];
    expect(mounts[0]).toEqual({ ...original[0], Target: "/var/data" });
    expect(mounts[0]?.VolumeOptions).toEqual(original[0]?.VolumeOptions);
    expect(mounts.slice(1)).toEqual(original.slice(1));
  });

  it("starts with no rows and one Add mount button, and writes nothing until a row is complete", () => {
    render(<Host />);
    expect(screen.queryByRole("group", { name: /^Mount / })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Add mount" }));
    expect(mount(1).getByRole("combobox", { name: "Type" }).textContent).toBe("Volume");
    expect(getPath(latest, MOUNTS)).toBeUndefined();
    type(mount(1).getByLabelText("Destination"), "/data");
    expect(getPath(latest, MOUNTS)).toEqual([{ Type: "volume", Target: "/data" }]);
    type(mount(1).getByLabelText("Source"), "web-data");
    fireEvent.click(mount(1).getByRole("switch", { name: "Read-only" }));
    expect(getPath(latest, MOUNTS)).toEqual([
      { Type: "volume", Source: "web-data", Target: "/data", ReadOnly: true },
    ]);
  });

  it("works at many rows: add as many as wanted, remove any, and the last one removed takes the list out", () => {
    render(<Host />);
    for (let i = 1; i <= 3; i++) {
      fireEvent.click(screen.getByRole("button", { name: "Add mount" }));
      type(mount(i).getByLabelText("Destination"), `/d${i}`);
    }
    expect((getPath(latest, MOUNTS) as unknown[]).length).toBe(3);
    fireEvent.click(screen.getByRole("button", { name: "Remove mount 2" }));
    expect((getPath(latest, MOUNTS) as { Target: string }[]).map((m) => m.Target)).toEqual([
      "/d1",
      "/d3",
    ]);
    fireEvent.click(screen.getByRole("button", { name: "Remove mount 1" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove mount 1" }));
    expect(getPath(latest, MOUNTS)).toBeUndefined();
    expect(screen.queryByRole("group", { name: /^Mount / })).toBeNull();
    expect(lastValid()).toBe(true);
  });

  it("shows 50 mounts", () => {
    const many = Array.from({ length: 50 }, (_, i) => ({
      Type: "volume",
      Source: `v${i}`,
      Target: `/v${i}`,
    }));
    render(<Host initial={withMounts(many)} />);
    expect(screen.getAllByRole("group", { name: /^Mount \d+$/ })).toHaveLength(50);
  });

  it("shows the bind notice with the D-38 words while a bind row exists, and not otherwise", () => {
    const notice =
      "A bind mount gives tasks access to a folder on whichever node runs them, and the folder must exist on every node that can run the service. Folders under /srv, /mnt and /media are open to Operators; any other host path or the Docker socket needs Admin.";
    render(<Host initial={withMounts([{ Type: "volume", Source: "v", Target: "/v" }])} />);
    expect(screen.queryByText(notice)).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Add mount" }));
    void choose(mount(2).getByRole("combobox", { name: "Type" }), "Bind");
    return waitFor(() => {
      expect(screen.getByText(notice)).toBeTruthy();
    }).then(() => {
      fireEvent.click(screen.getByRole("button", { name: "Remove mount 2" }));
      expect(screen.queryByText(notice)).toBeNull();
    });
  });

  it("does not disable Bind for anyone: the server decides who may use it", async () => {
    render(<Host initial={withMounts([{ Type: "volume", Source: "v", Target: "/v" }])} />);
    fireEvent.keyDown(mount(1).getByRole("combobox", { name: "Type" }), { key: "Enter" });
    const bind = await screen.findByRole("option", { name: "Bind" });
    expect(bind.getAttribute("aria-disabled")).not.toBe("true");
    expect(bind.hasAttribute("data-disabled")).toBe(false);
  });

  it("asks for an absolute destination, and writes nothing while it is not one", () => {
    render(<Host initial={full()} />);
    type(mount(1).getByLabelText("Destination"), "data");
    expect(screen.getByText("Enter an absolute path that starts with /.")).toBeTruthy();
    expect((getPath(latest, MOUNTS) as { Target: string }[])[0]?.Target).toBe("/data");
    expect(lastValid()).toBe(false);
    type(mount(1).getByLabelText("Destination"), "/data2");
    expect(screen.queryByText("Enter an absolute path that starts with /.")).toBeNull();
    expect((getPath(latest, MOUNTS) as { Target: string }[])[0]?.Target).toBe("/data2");
    expect(lastValid()).toBe(true);
  });

  it("asks for an absolute source on a bind", async () => {
    render(<Host />);
    fireEvent.click(screen.getByRole("button", { name: "Add mount" }));
    await choose(mount(1).getByRole("combobox", { name: "Type" }), "Bind");
    type(mount(1).getByLabelText("Destination"), "/app");
    type(mount(1).getByLabelText("Source"), "app");
    expect(screen.getByText("Enter an absolute path that starts with /.")).toBeTruthy();
    expect(getPath(latest, MOUNTS)).toBeUndefined();
    type(mount(1).getByLabelText("Source"), "/srv/app");
    expect(getPath(latest, MOUNTS)).toEqual([{ Type: "bind", Source: "/srv/app", Target: "/app" }]);
  });

  it("drops the options of the old type when the type changes", async () => {
    render(<Host initial={full()} />);
    await choose(mount(1).getByRole("combobox", { name: "Type" }), "Bind");
    type(mount(1).getByLabelText("Source"), "/srv/web");
    const first = (getPath(latest, MOUNTS) as Record<string, unknown>[])[0];
    expect(first).toEqual({
      Type: "bind",
      Source: "/srv/web",
      Target: "/data",
      ReadOnly: false,
      Consistency: "default",
    });
  });

  it("takes the source away when the type becomes tmpfs", async () => {
    render(<Host initial={full()} />);
    await choose(mount(1).getByRole("combobox", { name: "Type" }), "Tmpfs");
    expect(mount(1).queryByLabelText("Source")).toBeNull();
    const first = (getPath(latest, MOUNTS) as Record<string, unknown>[])[0];
    expect(first).toEqual({
      Type: "tmpfs",
      Target: "/data",
      ReadOnly: false,
      Consistency: "default",
    });
  });
});

describe("PlacementCard", () => {
  const rule = (n: number) => screen.getByLabelText(`Constraint ${n}`) as HTMLInputElement;
  const constraints = () => getPath(latest, "TaskTemplate.Placement.Constraints");
  const withConstraints = (list: string[]): SpecValue => ({
    ...bare(),
    TaskTemplate: { ContainerSpec: { Image: "nginx" }, Placement: { Constraints: list } },
  });

  it("shows each constraint in mono with how many nodes it matches", async () => {
    render(<Host initial={withConstraints(["node.role==worker", "node.labels.disk==ssd"])} />);
    expect(rule(1).value).toBe("node.role==worker");
    expect(rule(1).className).toContain("font-mono");
    expect(await screen.findByText("Matches 3 of 4 nodes")).toBeTruthy();
    expect(screen.getByText("Matches 2 of 4 nodes")).toBeTruthy();
  });

  it("warns, with an icon and words, of a constraint no node matches", async () => {
    render(<Host initial={withConstraints(["node.labels.gpu==true"])} />);
    const warning = await screen.findByText("Matches no nodes");
    expect(warning.parentElement?.querySelector("svg.lucide-triangle-alert")).toBeTruthy();
  });

  it("counts a node as in the singular", async () => {
    render(<Host initial={withConstraints(["node.role==manager"])} />);
    expect(await screen.findByText("Matches 1 of 4 nodes")).toBeTruthy();
  });

  it("shows no match count while the nodes are unknown", async () => {
    placementAnswer = () => Response.json({ title: "no" }, { status: 500 });
    render(<Host initial={withConstraints(["node.role==worker"])} />);
    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalled());
    expect(screen.queryByText(/Matches/)).toBeNull();
  });

  it("starts with no rows, adds one, and writes the constraint once it reads", async () => {
    render(<Host />);
    expect(screen.queryByLabelText("Constraint 1")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Add constraint" }));
    expect(constraints()).toBeUndefined();
    type(rule(1), "node.role==worker");
    expect(constraints()).toEqual(["node.role==worker"]);
    expect(await screen.findByText("Matches 3 of 4 nodes")).toBeTruthy();
    expect(
      screen.getByText("For example node.role==worker or node.labels.disk==ssd."),
    ).toBeTruthy();
  });

  it("refuses a constraint the shared parser rejects and keeps the one before it", () => {
    render(<Host initial={withConstraints(["node.role==worker"])} />);
    type(rule(1), "node.role=worker");
    expect(screen.getByText("Enter a constraint like node.role==worker.")).toBeTruthy();
    expect(constraints()).toEqual(["node.role==worker"]);
    expect(lastValid()).toBe(false);
    type(rule(1), "node.role==manager");
    expect(screen.queryByText("Enter a constraint like node.role==worker.")).toBeNull();
    expect(constraints()).toEqual(["node.role==manager"]);
    expect(lastValid()).toBe(true);
  });

  it("works at many rows, and removing the last takes the list out and keeps the platforms", () => {
    render(<Host initial={full()} />);
    expect(rule(2).value).toBe("node.labels.disk==ssd");
    fireEvent.click(screen.getByRole("button", { name: "Add constraint" }));
    type(rule(3), "node.hostname!=wrk-9");
    expect(constraints()).toEqual([
      "node.role==worker",
      "node.labels.disk==ssd",
      "node.hostname!=wrk-9",
    ]);
    fireEvent.click(screen.getByRole("button", { name: "Remove constraint 2" }));
    expect(constraints()).toEqual(["node.role==worker", "node.hostname!=wrk-9"]);
    fireEvent.click(screen.getByRole("button", { name: "Remove constraint 1" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove constraint 1" }));
    expect(constraints()).toBeUndefined();
    expect(getPath(latest, "TaskTemplate.Placement.Platforms")).toEqual(
      getPath(full(), "TaskTemplate.Placement.Platforms"),
    );
  });

  it("sets and clears the label to spread over", () => {
    render(<Host />);
    const spread = screen.getByLabelText("Spread over (label)") as HTMLInputElement;
    expect(spread.placeholder).toBe("node.labels.zone");
    type(spread, "node.labels.zone");
    expect(getPath(latest, "TaskTemplate.Placement.Preferences")).toEqual([
      { Spread: { SpreadDescriptor: "node.labels.zone" } },
    ]);
    type(spread, "");
    expect(getPath(latest, "TaskTemplate.Placement.Preferences")).toBeUndefined();
  });

  it("changes the label of an existing spread and nothing around it", () => {
    render(<Host initial={full()} />);
    const spread = screen.getByLabelText("Spread over (label)") as HTMLInputElement;
    expect(spread.value).toBe("node.labels.zone");
    type(spread, "node.labels.rack");
    expect(getPath(latest, "TaskTemplate.Placement.Preferences")).toEqual([
      { Spread: { SpreadDescriptor: "node.labels.rack" } },
    ]);
    expect(getPath(latest, "TaskTemplate.Placement.MaxReplicas")).toBe(2);
  });

  it("keeps further spread preferences it does not show, and says so", () => {
    const spec = full();
    (spec.TaskTemplate as { Placement: Record<string, unknown> }).Placement.Preferences = [
      { Spread: { SpreadDescriptor: "node.labels.zone" } },
      { Spread: { SpreadDescriptor: "node.labels.rack" } },
    ];
    render(<Host initial={spec} />);
    expect(
      screen.getByText("1 more preference is kept as it is. Edit it in the Spec tab."),
    ).toBeTruthy();
    type(screen.getByLabelText("Spread over (label)"), "node.labels.dc");
    expect(getPath(latest, "TaskTemplate.Placement.Preferences")).toEqual([
      { Spread: { SpreadDescriptor: "node.labels.dc" } },
      { Spread: { SpreadDescriptor: "node.labels.rack" } },
    ]);
  });

  it("sets, clears and checks the most replicas per node", () => {
    render(<Host />);
    const max = screen.getByLabelText("Max replicas per node") as HTMLInputElement;
    type(max, "3");
    expect(getPath(latest, "TaskTemplate.Placement.MaxReplicas")).toBe(3);
    type(max, "many");
    expect(screen.getByText("Enter a whole number of 0 or more.")).toBeTruthy();
    expect(getPath(latest, "TaskTemplate.Placement.MaxReplicas")).toBe(3);
    expect(lastValid()).toBe(false);
    type(max, "");
    expect(getPath(latest, "TaskTemplate.Placement.MaxReplicas")).toBeUndefined();
    expect(lastValid()).toBe(true);
  });
});

describe("RestartCard", () => {
  const policy = () => getPath(latest, "TaskTemplate.RestartPolicy");
  const region = () => within(card("Restart policy"));

  it("reads Any exit for a service with no restart policy, and writes nothing", () => {
    render(<Host />);
    expect(region().getByRole("combobox", { name: "Restart when" }).textContent).toBe("Any exit");
    expect(policy()).toBeUndefined();
  });

  it("maps Any exit, Failure only and Never to any, on-failure and none", async () => {
    render(<Host />);
    await choose(region().getByRole("combobox", { name: "Restart when" }), "Failure only");
    expect(getPath(latest, "TaskTemplate.RestartPolicy.Condition")).toBe("on-failure");
    await choose(region().getByRole("combobox", { name: "Restart when" }), "Never");
    expect(getPath(latest, "TaskTemplate.RestartPolicy.Condition")).toBe("none");
    await choose(region().getByRole("combobox", { name: "Restart when" }), "Any exit");
    expect(getPath(latest, "TaskTemplate.RestartPolicy.Condition")).toBe("any");
  });

  it("fills the fields from the spec", () => {
    render(<Host initial={full()} />);
    expect(region().getByRole("combobox", { name: "Restart when" }).textContent).toBe(
      "Failure only",
    );
    expect((region().getByLabelText("Delay") as HTMLInputElement).value).toBe("5s");
    expect((region().getByLabelText("Max attempts") as HTMLInputElement).value).toBe("4");
    expect((region().getByLabelText("Window") as HTMLInputElement).value).toBe("2m");
  });

  it("writes a delay as nanoseconds, refuses text that is not a duration, and clears it", () => {
    render(<Host initial={full()} />);
    const delay = region().getByLabelText("Delay") as HTMLInputElement;
    expect(delay.placeholder).toBe("5s");
    type(delay, "30s");
    expect(getPath(latest, "TaskTemplate.RestartPolicy.Delay")).toBe(30_000_000_000);
    type(delay, "5");
    expect(region().getByText("Enter a duration like 5s, 30s or 2m.")).toBeTruthy();
    expect(getPath(latest, "TaskTemplate.RestartPolicy.Delay")).toBe(30_000_000_000);
    expect(lastValid()).toBe(false);
    type(delay, "");
    expect(getPath(latest, "TaskTemplate.RestartPolicy.Delay")).toBeUndefined();
    expect(getPath(latest, "TaskTemplate.RestartPolicy.Condition")).toBe("on-failure");
    expect(lastValid()).toBe(true);
  });

  it("writes the window and says what it is for", () => {
    render(<Host />);
    const window = region().getByLabelText("Window") as HTMLInputElement;
    expect(window.placeholder).toBe("120s");
    expect(
      region().getByText("How long a task must run before it counts as started."),
    ).toBeTruthy();
    type(window, "1m30s");
    expect(getPath(latest, "TaskTemplate.RestartPolicy.Window")).toBe(90_000_000_000);
  });

  it("leaves max attempts empty to retry forever, and writes a number otherwise", () => {
    render(<Host initial={full()} />);
    const attempts = region().getByLabelText("Max attempts") as HTMLInputElement;
    expect(region().getByText("Leave empty to retry forever.")).toBeTruthy();
    type(attempts, "");
    expect(getPath(latest, "TaskTemplate.RestartPolicy.MaxAttempts")).toBeUndefined();
    type(attempts, "7");
    expect(getPath(latest, "TaskTemplate.RestartPolicy.MaxAttempts")).toBe(7);
    type(attempts, "x");
    expect(region().getByText("Enter a whole number of 0 or more.")).toBeTruthy();
    expect(getPath(latest, "TaskTemplate.RestartPolicy.MaxAttempts")).toBe(7);
  });
});

describe("UpdateCard", () => {
  const region = () => within(card("Update policy"));
  const config = () => getPath(latest, "UpdateConfig");
  const box = () =>
    region().getByRole("checkbox", { name: "Roll back automatically if an update fails" });
  const SEEDED = {
    Parallelism: 1,
    Delay: 0,
    FailureAction: "pause",
    Monitor: 5_000_000_000,
    MaxFailureRatio: 0,
    Order: "stop-first",
  };

  it("fills the five fields and the box from the spec", () => {
    render(<Host initial={full()} />);
    expect((region().getByLabelText("Tasks at a time") as HTMLInputElement).value).toBe("2");
    expect((region().getByLabelText("Delay between batches") as HTMLInputElement).value).toBe(
      "10s",
    );
    expect((region().getByLabelText("Monitor") as HTMLInputElement).value).toBe("5s");
    expect((region().getByLabelText("Max failure ratio") as HTMLInputElement).value).toBe("0.25");
    expect(region().getByRole("combobox", { name: "Order" }).textContent).toBe("Start first");
    expect(box().getAttribute("aria-checked")).toBe("true");
  });

  it("explains the fields", () => {
    render(<Host />);
    expect(region().getByText("0 updates all at once.")).toBeTruthy();
    expect(
      region().getByText("Start first brings the new task up before the old one stops."),
    ).toBeTruthy();
    expect(
      region().getByText(
        "Saved on the service, so every later update uses it, including one-click image updates.",
      ),
    ).toBeTruthy();
    expect((region().getByLabelText("Delay between batches") as HTMLInputElement).placeholder).toBe(
      "10s",
    );
  });

  it("seeds the CLI defaults when the box is ticked on a service with no UpdateConfig", () => {
    render(<Host />);
    expect(box().getAttribute("aria-checked")).toBe("false");
    fireEvent.click(box());
    expect(config()).toEqual({ ...SEEDED, FailureAction: "rollback" });
    expect(box().getAttribute("aria-checked")).toBe("true");
  });

  it("leaves the UpdateConfig out again when the box is ticked and unticked on such a service", () => {
    render(<Host />);
    fireEvent.click(box());
    fireEvent.click(box());
    expect(config()).toBeUndefined();
    expect(latest).toEqual(bare());
  });

  it("sets pause when a service that rolled back is unticked, and changes nothing else", () => {
    render(<Host initial={full()} />);
    fireEvent.click(box());
    expect(config()).toEqual({
      ...(getPath(full(), "UpdateConfig") as object),
      FailureAction: "pause",
    });
    expect(getPath(latest, "RollbackConfig")).toEqual(getPath(full(), "RollbackConfig"));
  });

  it("shows a service that continues on failure unticked, says so, and ticking changes it to roll back", () => {
    const spec = { ...bare(), UpdateConfig: { Parallelism: 3, FailureAction: "continue" } };
    render(<Host initial={spec} />);
    expect(box().getAttribute("aria-checked")).toBe("false");
    expect(
      region().getByText(
        "Currently set to continue on failure. Ticking this changes it to roll back.",
      ),
    ).toBeTruthy();
    fireEvent.click(box());
    expect(config()).toEqual({ Parallelism: 3, FailureAction: "rollback" });
    expect(region().queryByText(/Currently set to continue/)).toBeNull();
  });

  it("seeds the defaults under a field typed on a service with no UpdateConfig, so the speed does not change", () => {
    render(<Host />);
    type(region().getByLabelText("Delay between batches"), "10s");
    expect(config()).toEqual({ ...SEEDED, Delay: 10_000_000_000 });
  });

  it("writes the tasks at a time, the monitor, the ratio and the order", async () => {
    render(<Host initial={full()} />);
    type(region().getByLabelText("Tasks at a time"), "4");
    type(region().getByLabelText("Monitor"), "30s");
    type(region().getByLabelText("Max failure ratio"), "0.5");
    await choose(region().getByRole("combobox", { name: "Order" }), "Stop first");
    expect(config()).toEqual({
      ...(getPath(full(), "UpdateConfig") as object),
      Parallelism: 4,
      Monitor: 30_000_000_000,
      MaxFailureRatio: 0.5,
      Order: "stop-first",
    });
  });

  it("writes a ratio of 0, which the spec then states", () => {
    render(<Host initial={full()} />);
    type(region().getByLabelText("Max failure ratio"), "0");
    expect(getPath(latest, "UpdateConfig.MaxFailureRatio")).toBe(0);
  });

  it("tells off a duration, a count and a ratio that are not usable, and writes none of them", () => {
    render(<Host initial={full()} />);
    type(region().getByLabelText("Delay between batches"), "soon");
    type(region().getByLabelText("Tasks at a time"), "-1");
    type(region().getByLabelText("Max failure ratio"), "2");
    expect(region().getByText("Enter a duration like 5s, 30s or 2m.")).toBeTruthy();
    expect(region().getByText("Enter a whole number of 0 or more.")).toBeTruthy();
    expect(region().getByText("Enter a number from 0 to 1.")).toBeTruthy();
    expect(config()).toEqual(getPath(full(), "UpdateConfig"));
    expect(lastValid()).toBe(false);
  });

  it("takes a cleared field out of the UpdateConfig", () => {
    render(<Host initial={full()} />);
    type(region().getByLabelText("Delay between batches"), "");
    expect(getPath(latest, "UpdateConfig.Delay")).toBeUndefined();
    expect(getPath(latest, "UpdateConfig.Parallelism")).toBe(2);
  });
});
