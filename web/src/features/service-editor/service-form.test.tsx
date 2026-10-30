// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { ServiceForm } from "@/features/service-editor/service-form";
import { getPath, removePath, setPath } from "@/features/service-editor/spec-patch";
import everyField from "@/features/service-editor/testdata/every-field-spec.json";
import type { SpecValue } from "@/features/spec-editor/spec-format";
import "@/i18n";

// The Form tab's four cards (plan 06-20, D-01, D-02, D-09, RES-03; UI-SPEC "Service editor"):
// every control reads and writes one path of the spec that was loaded, and nothing else moves.

const DIGEST = `sha256:${"0123456789abcdef".repeat(4)}`;

const base = (): SpecValue => ({
  Name: "api",
  Labels: { tier: "backend", team: "shop" },
  TaskTemplate: {
    ContainerSpec: {
      Image: `registry.example.com/shop/api:1.4@${DIGEST}`,
      Command: ["/bin/api", "--serve"],
      Env: ["MODE=prod", "DB_PASSWORD=<hidden>", "URL=https://x.test/?a=1&b=2"],
    },
    Resources: {
      Limits: { NanoCPUs: 500000000, MemoryBytes: 268435456 },
      Reservations: { NanoCPUs: 250000000, MemoryBytes: 8589934592 },
    },
  },
  Mode: { Replicated: { Replicas: 3 } },
  EndpointSpec: {
    Ports: [
      { Protocol: "tcp", TargetPort: 80, PublishedPort: 8080, PublishMode: "ingress" },
      { Name: "dns", Protocol: "udp", TargetPort: 53, PublishedPort: 5353, PublishMode: "host" },
    ],
  },
});

let latest: SpecValue;
let changes: number;
let validity: boolean[];
let opened: number;
let resolveAnswer: () => Response;

function Host({
  initial = base(),
  mode = "edit",
}: {
  initial?: SpecValue;
  mode?: "edit" | "create";
}) {
  const [spec, setSpec] = useState(initial);
  latest = spec;
  return (
    <QueryClientProvider client={queryClient}>
      <ServiceForm
        spec={spec}
        mode={mode}
        envId="e1"
        onSpecChange={(next) => {
          changes++;
          latest = next;
          setSpec(next);
        }}
        onValidityChange={(v) => validity.push(v)}
        onOpenSpec={() => opened++}
      />
    </QueryClientProvider>
  );
}

beforeEach(() => {
  latest = {};
  changes = 0;
  validity = [];
  opened = 0;
  setCsrfToken(null);
  resolveAnswer = () =>
    Response.json({
      input: "x",
      normalized: "registry.example.com/shop/api:1.5",
      matchedOn: "",
      credential: null,
      considered: [],
    });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.endsWith("/registries/resolve")) return resolveAnswer();
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
const input = (name: string) => screen.getByLabelText(name) as HTMLInputElement;

async function choose(combobox: HTMLElement, option: string) {
  fireEvent.keyDown(combobox, { key: "Enter" });
  fireEvent.click(await screen.findByRole("option", { name: option }));
}

describe("the cards", () => {
  it("fills Basics, Environment, Ports and Resources from the loaded spec", () => {
    render(<Host />);
    for (const title of ["Basics", "Environment variables", "Ports", "Resources"]) {
      expect(screen.getByRole("heading", { name: title, level: 2 })).toBeTruthy();
    }
    expect(input("Name").value).toBe("api");
    expect(input("Image").value).toBe(`registry.example.com/shop/api:1.4@${DIGEST}`);
    expect(input("Replicas").value).toBe("3");
    expect(
      (screen.getAllByLabelText("Label name") as HTMLInputElement[]).map((i) => i.value),
    ).toEqual(["tier", "team"]);
    expect(
      (screen.getAllByLabelText("Variable name") as HTMLInputElement[]).map((i) => i.value),
    ).toEqual(["MODE", "DB_PASSWORD", "URL"]);
    const ports = screen.getAllByRole("group", { name: /^Port \d+$/ });
    expect(ports).toHaveLength(2);
    expect(
      (
        within(ports[0] as HTMLElement).getByLabelText(
          "Published port (optional)",
        ) as HTMLInputElement
      ).value,
    ).toBe("8080");
    expect(
      (within(ports[0] as HTMLElement).getByLabelText("Container port") as HTMLInputElement).value,
    ).toBe("80");
    expect(
      (within(ports[1] as HTMLElement).getByLabelText("Container port") as HTMLInputElement).value,
    ).toBe("53");
    expect(
      within(ports[1] as HTMLElement).getByRole("combobox", { name: "Protocol" }).textContent,
    ).toContain("UDP");
    expect(
      within(ports[1] as HTMLElement).getByRole("combobox", { name: "Mode" }).textContent,
    ).toContain("Host");
    expect(input("CPU limit (cores)").value).toBe("0.5");
    expect(input("CPU reservation (cores)").value).toBe("0.25");
    expect(input("Memory limit").value).toBe("256");
    expect(screen.getByRole("combobox", { name: "Memory limit unit" }).textContent).toContain(
      "MiB",
    );
    expect(input("Memory reservation").value).toBe("8");
    expect(screen.getByRole("combobox", { name: "Memory reservation unit" }).textContent).toContain(
      "GiB",
    );
  });

  it("changes nothing in the spec until a field is edited", () => {
    render(<Host initial={structuredClone(everyField) as SpecValue} />);
    expect(changes).toBe(0);
    expect(latest).toEqual(everyField);
  });

  it("is one column, the cards in the order of the copy", () => {
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

describe("a field patches its own path of the loaded spec", () => {
  it("changes the image and nothing else, over a spec that has every field", () => {
    const loaded = structuredClone(everyField) as SpecValue;
    render(<Host initial={loaded} />);
    type(input("Image"), "nginx:1.28");
    const expected = setPath(loaded, "TaskTemplate.ContainerSpec.Image", "nginx:1.28");
    expect(latest).toEqual(expected);
    expect(JSON.stringify(latest)).toBe(JSON.stringify(expected));
  });

  it("changes the replicas to a number", () => {
    render(<Host />);
    type(input("Replicas"), "5");
    expect(getPath(latest, "Mode.Replicated.Replicas")).toBe(5);
    expect(getPath(latest, "TaskTemplate")).toEqual(getPath(base(), "TaskTemplate"));
  });

  it("changes the labels as a map, in the order of the rows", () => {
    render(<Host />);
    type(screen.getAllByLabelText("Label value")[0] as HTMLElement, "web");
    expect(getPath(latest, "Labels")).toEqual({ tier: "web", team: "shop" });
    fireEvent.click(screen.getByRole("button", { name: "Add label" }));
    type(screen.getAllByLabelText("Label name")[2] as HTMLElement, "env");
    type(screen.getAllByLabelText("Label value")[2] as HTMLElement, "prod");
    expect(Object.keys(getPath(latest, "Labels") as object)).toEqual(["tier", "team", "env"]);
    fireEvent.click(screen.getByRole("button", { name: "Remove label tier" }));
    expect(getPath(latest, "Labels")).toEqual({ team: "shop", env: "prod" });
  });

  it("takes Labels out of the spec when the last label is removed from a spec that had some", () => {
    render(<Host initial={{ ...base(), Labels: { only: "one" } }} />);
    fireEvent.click(screen.getByRole("button", { name: "Remove label only" }));
    expect(getPath(latest, "Labels")).toBeUndefined();
  });

  it("keeps a setting the form does not show through every edit", () => {
    render(<Host />);
    type(input("Image"), "nginx:1.28");
    type(input("Replicas"), "2");
    type(input("CPU limit (cores)"), "1");
    expect(getPath(latest, "TaskTemplate.ContainerSpec.Command")).toEqual(["/bin/api", "--serve"]);
  });
});

describe("Name and mode", () => {
  it("are read-only on edit, with the reason", () => {
    render(<Host />);
    expect(input("Name").readOnly).toBe(true);
    expect(
      screen.getByText("A service's name can't change. Create a new service instead."),
    ).toBeTruthy();
    const replicated = screen.getByRole("radio", { name: "Replicated" });
    expect(replicated.getAttribute("aria-checked")).toBe("true");
    expect((replicated as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("radio", { name: "Global" }) as HTMLButtonElement).disabled).toBe(
      true,
    );
    expect(screen.getByText("A service's mode can't change.")).toBeTruthy();
  });

  it("hide the replicas of a global service, and a job is edited in the Spec tab only", () => {
    const { unmount } = render(<Host initial={{ ...base(), Mode: { Global: {} } }} />);
    expect(screen.getByRole("radio", { name: "Global" }).getAttribute("aria-checked")).toBe("true");
    expect(screen.queryByLabelText("Replicas")).toBeNull();
    unmount();
    render(<Host initial={{ ...base(), Mode: { ReplicatedJob: { MaxConcurrent: 1 } } }} />);
    expect(screen.queryByLabelText("Replicas")).toBeNull();
    expect(screen.getByText("Jobs are created and edited in the Spec tab only.")).toBeTruthy();
  });

  it("are chosen on create: a name that is checked, and Replicated or Global", () => {
    render(<Host mode="create" initial={{ Name: "", Mode: { Replicated: { Replicas: 1 } } }} />);
    expect(input("Name").readOnly).toBe(false);
    expect(
      screen.getByText("Letters, numbers, dot, dash or underscore. Up to 63 characters."),
    ).toBeTruthy();
    type(input("Name"), "web");
    expect(getPath(latest, "Name")).toBe("web");
    type(input("Name"), "bad name");
    expect(screen.getByText("Use letters, numbers, dot, dash or underscore.")).toBeTruthy();
    expect(getPath(latest, "Name")).toBe("web");
    type(input("Name"), "");
    expect(screen.getByText("Enter a name.")).toBeTruthy();
    fireEvent.click(screen.getByRole("radio", { name: "Global" }));
    expect(getPath(latest, "Mode")).toEqual({ Global: {} });
    expect(screen.queryByLabelText("Replicas")).toBeNull();
    fireEvent.click(screen.getByRole("radio", { name: "Replicated" }));
    expect(getPath(latest, "Mode")).toEqual({ Replicated: { Replicas: 1 } });
    expect(input("Replicas").value).toBe("1");
  });
});

describe("field errors", () => {
  it("refuse a replicas that is not a whole number, write nothing and say so to the page", async () => {
    render(<Host />);
    for (const bad of ["abc", "-1", "1.5", ""]) {
      type(input("Replicas"), bad);
      expect(screen.getByText("Enter a whole number of 0 or more.")).toBeTruthy();
      expect(input("Replicas").getAttribute("aria-invalid")).toBe("true");
      expect(getPath(latest, "Mode.Replicated.Replicas")).toBe(3);
    }
    await waitFor(() => expect(validity.at(-1)).toBe(false));
    type(input("Replicas"), "0");
    expect(getPath(latest, "Mode.Replicated.Replicas")).toBe(0);
    expect(screen.queryByText("Enter a whole number of 0 or more.")).toBeNull();
    await waitFor(() => expect(validity.at(-1)).toBe(true));
  });

  it("refuse an empty image or one with a space in it", () => {
    render(<Host />);
    type(input("Image"), "");
    expect(
      screen.getByText("Enter an image like nginx:1.27 or ghcr.io/acme/app:latest."),
    ).toBeTruthy();
    type(input("Image"), "nginx 1");
    expect(
      screen.getByText("Enter an image like nginx:1.27 or ghcr.io/acme/app:latest."),
    ).toBeTruthy();
    expect(getPath(latest, "TaskTemplate.ContainerSpec.Image")).toBe(
      `registry.example.com/shop/api:1.4@${DIGEST}`,
    );
  });

  it("refuse cores that are not a number like 0.5", () => {
    render(<Host />);
    type(input("CPU limit (cores)"), "half");
    expect(screen.getByText("Enter a number of cores like 0.5.")).toBeTruthy();
    expect(getPath(latest, "TaskTemplate.Resources.Limits.NanoCPUs")).toBe(500000000);
    type(input("CPU limit (cores)"), "-1");
    expect(screen.getByText("Enter a number of cores like 0.5.")).toBeTruthy();
  });
});

describe("Ports", () => {
  const rows = () => screen.getAllByRole("group", { name: /^Port \d+$/ });
  const field = (row: number, name: string) =>
    within(rows()[row] as HTMLElement).getByLabelText(name) as HTMLInputElement;

  it("edits one port and leaves the other, and what the form does not show of it, as it was", () => {
    render(<Host />);
    type(field(0, "Published port (optional)"), "9090");
    const ports = getPath(latest, "EndpointSpec.Ports") as Record<string, unknown>[];
    expect(ports[0]).toEqual({
      Protocol: "tcp",
      TargetPort: 80,
      PublishedPort: 9090,
      PublishMode: "ingress",
    });
    expect(ports[1]).toEqual(getPath(base(), "EndpointSpec.Ports[1]"));
    expect(ports[1]?.Name).toBe("dns");
  });

  it("takes the published port off when it is emptied", () => {
    render(<Host />);
    type(field(0, "Published port (optional)"), "");
    const port = getPath(latest, "EndpointSpec.Ports[0]") as Record<string, unknown>;
    expect("PublishedPort" in port).toBe(false);
    expect(port.TargetPort).toBe(80);
  });

  it("changes the protocol and the mode", async () => {
    render(<Host />);
    await choose(
      within(rows()[0] as HTMLElement).getByRole("combobox", { name: "Protocol" }),
      "SCTP",
    );
    await choose(within(rows()[0] as HTMLElement).getByRole("combobox", { name: "Mode" }), "Host");
    const port = getPath(latest, "EndpointSpec.Ports[0]") as Record<string, unknown>;
    expect(port.Protocol).toBe("sctp");
    expect(port.PublishMode).toBe("host");
  });

  it("refuses a port outside 1 to 65535 and writes nothing for it", () => {
    render(<Host />);
    for (const bad of ["0", "65536", "abc", ""]) {
      type(field(0, "Container port"), bad);
      expect(screen.getByText("Enter a port from 1 to 65535.")).toBeTruthy();
      expect(getPath(latest, "EndpointSpec.Ports[0].TargetPort")).toBe(80);
    }
    type(field(0, "Published port (optional)"), "70000");
    expect(screen.getAllByText("Enter a port from 1 to 65535.").length).toBeGreaterThan(0);
    type(field(0, "Published port (optional)"), "8080");
    type(field(0, "Container port"), "81");
    expect(getPath(latest, "EndpointSpec.Ports[0].TargetPort")).toBe(81);
  });

  it("refuses a published port used twice for the same protocol, on both rows, but not for another", async () => {
    render(<Host />);
    type(field(1, "Published port (optional)"), "8080");
    expect(screen.queryByText("That published port is used twice.")).toBeNull();
    await choose(
      within(rows()[1] as HTMLElement).getByRole("combobox", { name: "Protocol" }),
      "TCP",
    );
    expect(screen.getAllByText("That published port is used twice.")).toHaveLength(2);
    await waitFor(() => expect(validity.at(-1)).toBe(false));
    type(field(1, "Published port (optional)"), "8081");
    expect(screen.queryByText("That published port is used twice.")).toBeNull();
    await waitFor(() => expect(validity.at(-1)).toBe(true));
  });

  it("adds a port with the usual defaults and removes one", () => {
    render(<Host />);
    fireEvent.click(screen.getByRole("button", { name: "Add port" }));
    expect(rows()).toHaveLength(3);
    type(field(2, "Container port"), "3000");
    const added = getPath(latest, "EndpointSpec.Ports[2]");
    expect(added).toEqual({ Protocol: "tcp", TargetPort: 3000, PublishMode: "ingress" });
    fireEvent.click(screen.getByRole("button", { name: "Remove port 1" }));
    const ports = getPath(latest, "EndpointSpec.Ports") as unknown[];
    expect(ports).toHaveLength(2);
    expect((ports[0] as Record<string, unknown>).Name).toBe("dns");
  });

  it("works at none: a spec with no ports, then one", () => {
    render(<Host initial={removePath(base(), "EndpointSpec")} />);
    expect(screen.queryAllByRole("group", { name: /^Port \d+$/ })).toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Add port" }));
    type(field(0, "Container port"), "80");
    expect(getPath(latest, "EndpointSpec.Ports")).toEqual([
      { Protocol: "tcp", TargetPort: 80, PublishMode: "ingress" },
    ]);
  });

  it("takes EndpointSpec.Ports out when the last port is removed", () => {
    render(<Host initial={setPath(base(), "EndpointSpec.Ports", [{ TargetPort: 80 }])} />);
    fireEvent.click(screen.getByRole("button", { name: "Remove port 1" }));
    expect(getPath(latest, "EndpointSpec.Ports")).toBeUndefined();
  });

  it("writes a port that does not name its protocol or mode as it was found until they are chosen", () => {
    render(<Host initial={setPath(base(), "EndpointSpec.Ports", [{ TargetPort: 80 }])} />);
    type(field(0, "Published port (optional)"), "8080");
    expect(getPath(latest, "EndpointSpec.Ports[0]")).toEqual({
      TargetPort: 80,
      PublishedPort: 8080,
    });
  });
});

describe("Environment", () => {
  const value = (i: number) => screen.getAllByLabelText("Variable value")[i] as HTMLInputElement;

  it("shows a hidden value as the placeholder, with the value nowhere in the page", () => {
    render(<Host />);
    expect(value(1).value).toBe("");
    expect(value(1).placeholder).toBe("Hidden. Type to replace.");
    expect(value(0).value).toBe("prod");
    expect(document.body.textContent).not.toContain("<hidden>");
    expect(screen.queryByDisplayValue("<hidden>")).toBeNull();
    expect(
      screen.getByText(
        "Values are hidden. A hidden value stays as it is unless you type a new one.",
      ),
    ).toBeTruthy();
  });

  it("does not say values are hidden when none is", () => {
    render(<Host initial={setPath(base(), "TaskTemplate.ContainerSpec.Env", ["MODE=prod"])} />);
    expect(screen.queryByText(/Values are hidden/)).toBeNull();
  });

  it("sends an untouched hidden row as NAME=<hidden> when another row is edited", () => {
    render(<Host />);
    type(value(0), "staging");
    expect(getPath(latest, "TaskTemplate.ContainerSpec.Env")).toEqual([
      "MODE=staging",
      "DB_PASSWORD=<hidden>",
      "URL=https://x.test/?a=1&b=2",
    ]);
  });

  it("sends what is typed over a hidden row, shows Reset, and Reset sends the placeholder again", () => {
    render(<Host />);
    type(value(1), "s3cret");
    expect(getPath(latest, "TaskTemplate.ContainerSpec.Env[1]")).toBe("DB_PASSWORD=s3cret");
    fireEvent.click(screen.getByRole("button", { name: "Reset DB_PASSWORD" }));
    expect(getPath(latest, "TaskTemplate.ContainerSpec.Env[1]")).toBe("DB_PASSWORD=<hidden>");
    expect(value(1).value).toBe("");
    expect(screen.queryByRole("button", { name: "Reset DB_PASSWORD" })).toBeNull();
  });

  it("does not let a hidden variable be renamed, which the server could not keep the value for", () => {
    render(<Host />);
    expect((screen.getAllByLabelText("Variable name")[1] as HTMLInputElement).readOnly).toBe(true);
  });

  it("deletes the variable when its row is removed", () => {
    render(<Host />);
    fireEvent.click(screen.getByRole("button", { name: "Remove variable DB_PASSWORD" }));
    expect(getPath(latest, "TaskTemplate.ContainerSpec.Env")).toEqual([
      "MODE=prod",
      "URL=https://x.test/?a=1&b=2",
    ]);
  });

  it("adds a variable, keeping a value that holds an equals sign", () => {
    render(<Host />);
    fireEvent.click(screen.getByRole("button", { name: "Add variable" }));
    type(screen.getAllByLabelText("Variable name")[3] as HTMLElement, "OPTS");
    type(value(3), "a=b");
    expect(getPath(latest, "TaskTemplate.ContainerSpec.Env[3]")).toBe("OPTS=a=b");
  });

  it("leaves an entry it does not change exactly as it was, even one without an equals sign", () => {
    render(
      <Host initial={setPath(base(), "TaskTemplate.ContainerSpec.Env", ["BARE", "MODE=prod"])} />,
    );
    type(value(1), "x");
    expect(getPath(latest, "TaskTemplate.ContainerSpec.Env")).toEqual(["BARE", "MODE=x"]);
  });

  it("takes Env out of the spec when the last variable is removed", () => {
    render(<Host initial={setPath(base(), "TaskTemplate.ContainerSpec.Env", ["MODE=prod"])} />);
    fireEvent.click(screen.getByRole("button", { name: "Remove variable MODE" }));
    expect(getPath(latest, "TaskTemplate.ContainerSpec.Env")).toBeUndefined();
  });
});

describe("Resources", () => {
  it("stores cores as NanoCPUs", () => {
    render(<Host />);
    type(input("CPU limit (cores)"), "1.5");
    expect(getPath(latest, "TaskTemplate.Resources.Limits.NanoCPUs")).toBe(1500000000);
    type(input("CPU reservation (cores)"), "0.1");
    expect(getPath(latest, "TaskTemplate.Resources.Reservations.NanoCPUs")).toBe(100000000);
  });

  it("stores memory as bytes in MiB or GiB, and works the bytes out again when the unit changes", async () => {
    render(<Host />);
    type(input("Memory limit"), "512");
    expect(getPath(latest, "TaskTemplate.Resources.Limits.MemoryBytes")).toBe(536870912);
    await choose(screen.getByRole("combobox", { name: "Memory limit unit" }), "GiB");
    expect(getPath(latest, "TaskTemplate.Resources.Limits.MemoryBytes")).toBe(512 * 1024 ** 3);
    type(input("Memory reservation"), "2");
    expect(getPath(latest, "TaskTemplate.Resources.Reservations.MemoryBytes")).toBe(2 * 1024 ** 3);
  });

  it("takes the limit off when the field is emptied, and leaves Pids and the rest of Resources alone", () => {
    render(<Host initial={setPath(base(), "TaskTemplate.Resources.Limits.Pids", 100)} />);
    type(input("CPU limit (cores)"), "");
    type(input("Memory limit"), "");
    expect(getPath(latest, "TaskTemplate.Resources.Limits")).toEqual({ Pids: 100 });
    expect(getPath(latest, "TaskTemplate.Resources.Reservations")).toEqual(
      getPath(base(), "TaskTemplate.Resources.Reservations"),
    );
  });

  it("writes nothing for a field that holds no limit and is left empty", () => {
    render(<Host initial={removePath(base(), "TaskTemplate.Resources")} />);
    expect(input("CPU limit (cores)").value).toBe("");
    type(input("CPU limit (cores)"), "");
    expect(getPath(latest, "TaskTemplate.Resources")).toBeUndefined();
  });

  it("refuses memory that is not a whole number", () => {
    render(<Host />);
    type(input("Memory limit"), "lots");
    expect(screen.getByText("Enter a whole number of 0 or more.")).toBeTruthy();
    expect(getPath(latest, "TaskTemplate.Resources.Limits.MemoryBytes")).toBe(268435456);
  });

  it("says what the limits and reservations mean", () => {
    render(<Host />);
    expect(screen.getAllByText("Leave empty for no limit.").length).toBeGreaterThan(0);
    expect(
      screen.getAllByText("Swarm only places a task on a node with this much free.").length,
    ).toBeGreaterThan(0);
  });
});

describe("the image's credential preview and tag browser", () => {
  it("shows what the image pulls and the credential it will use, once typing stops", async () => {
    render(<Host />);
    type(input("Image"), "ghcr.io/acme/app:1");
    expect(await screen.findByText("Pulls registry.example.com/shop/api:1.5")).toBeTruthy();
    expect(screen.getByText("Credential: anonymous. No stored credential matches.")).toBeTruthy();
  });

  it("shows a stored credential's name and scope", async () => {
    resolveAnswer = () =>
      Response.json({
        input: "x",
        normalized: "ghcr.io/acme/app:1",
        matchedOn: "host",
        credential: { id: "c1", name: "ghcr-acme", scope: { kind: "instance" } },
        considered: [],
      });
    render(<Host />);
    type(input("Image"), "ghcr.io/acme/app:1");
    expect(await screen.findByText(/Credential: “ghcr-acme”/)).toBeTruthy();
  });

  it("offers Browse tags for a repository", () => {
    render(<Host />);
    expect(screen.getByRole("button", { name: "Browse tags" })).toBeTruthy();
  });

  it("says the tag is pinned when it is applied", () => {
    render(<Host />);
    expect(
      screen.getByText("Coaming pins the tag to its current digest when you apply."),
    ).toBeTruthy();
  });
});

describe("the caution bar", () => {
  it("is not there when the form shows everything the spec holds", () => {
    render(<Host initial={removePath(base(), "TaskTemplate.ContainerSpec.Command")} />);
    expect(screen.queryByText(/This service has additional settings not shown here/)).toBeNull();
  });

  it("names the settings the form does not show, and opens the Spec tab", () => {
    render(<Host />);
    expect(
      screen.getByText(
        "This service has additional settings not shown here. Saving from this form keeps them unchanged. See the Spec tab.",
      ),
    ).toBeTruthy();
    expect(screen.getByText("Not shown: TaskTemplate.ContainerSpec.Command")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Open the Spec tab" }));
    expect(opened).toBe(1);
  });

  it("names up to three paths, and the rest in a popover", async () => {
    const many = structuredClone(everyField) as SpecValue;
    render(<Host initial={many} />);
    expect(
      screen.getByText(
        "Not shown: TaskTemplate.ContainerSpec.Labels, TaskTemplate.ContainerSpec.Command, TaskTemplate.ContainerSpec.Args",
      ),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /^and \d+ more$/ }));
    const list = await screen.findByRole("list", { name: "Settings not shown" });
    expect(within(list).getAllByRole("listitem").length).toBeGreaterThan(20);
    expect(within(list).getByText("TaskTemplate.LogDriver")).toBeTruthy();
  });
});
