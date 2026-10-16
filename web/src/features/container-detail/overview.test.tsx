// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { ContainerDetail } from "@/api/container-detail";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { Overview } from "@/features/container-detail/overview";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";
import { settled } from "@/test-settle";

// The Overview tab of a container (plan 04-10, RES-01, D-04, P3 D-15a).

const ENV = "e1";

function detail(extra: Partial<ContainerDetail> = {}): ContainerDetail {
  return {
    nodeId: "n1",
    node: "node-1",
    readOnly: false,
    id: "web-id",
    name: "web",
    image: "nginx:1.27",
    imageId: "sha256:abc",
    created: "2026-10-01T10:00:00Z",
    state: "running",
    health: "healthy",
    startedAt: "2026-10-01T10:00:05Z",
    exitCode: 0,
    oomKilled: false,
    restartCount: 2,
    command: ["nginx", "-g", "daemon off;"],
    entrypoint: [],
    commandMasked: false,
    restartPolicy: "no",
    restartMaxRetries: 0,
    ports: [],
    mounts: [],
    networks: [],
    labels: {},
    env: [],
    logConfig: { type: "json-file", options: [] },
    masked: false,
    isSelf: false,
    selfKind: "",
    project: "",
    stack: "",
    service: "",
    muted: false,
    ...extra,
  };
}

let answer: () => Response;
let disconnectAnswer: () => Response;
let me: ReturnType<typeof makeMe>;
let posts: Array<{ path: string; body: unknown }>;
let meServed: number;
let urls: string[];
let client: QueryClient;

beforeEach(() => {
  urls = [];
  posts = [];
  meServed = 0;
  me = makeMe({ envs: { [ENV]: "viewer" } });
  answer = () => Response.json(detail());
  disconnectAnswer = () => new Response(null, { status: 204 });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") {
        meServed++;
        return Response.json(me);
      }
      if (url.pathname === `/api/env/${ENV}/containers/web-id`) {
        urls.push(`${url.pathname}${url.search}`);
        return answer();
      }
      if (req.method === "POST") {
        posts.push({ path: url.pathname, body: await req.clone().json() });
        if (url.pathname.endsWith("/disconnect")) return disconnectAnswer();
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

function mount() {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <Overview envId={ENV} containerId="web-id" node="n1" />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const card = (title: string) => screen.getByRole("region", { name: title });

// The page holds its actions back until it knows who the user is: wait for that answer, and
// for the cache to have taken it in (nothing fetching), so the buttons below have their final state.
async function signedIn() {
  await waitFor(() => expect(meServed).toBeGreaterThan(0));
  await settled(client);
}

describe("Overview: the tracer cards", () => {
  it("asks for the container on its node and shows skeletons until it answers", async () => {
    mount();
    expect(screen.getByRole("status").textContent).toBe("Loading the container");
    await screen.findByRole("region", { name: "State" });
    expect(urls).toEqual(["/api/env/e1/containers/web-id?nodeId=n1"]);
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("shows the State rows", async () => {
    answer = () =>
      Response.json(
        detail({
          state: "exited",
          health: "unhealthy",
          exitCode: 137,
          oomKilled: true,
          restartCount: 4,
        }),
      );
    mount();
    const state = await screen.findByRole("region", { name: "State" });
    const rows = within(state)
      .getAllByRole("row")
      .map((r) => Array.from(r.querySelectorAll("td")).map((c) => c.textContent));
    expect(rows.map((r) => r[0])).toEqual([
      "Status",
      "Health",
      "Started",
      "Finished",
      "Exit code",
      "Restarts",
      "Out of memory killed",
    ]);
    expect(rows[0]?.[1]).toBe("Exited");
    expect(rows[1]?.[1]).toBe("Unhealthy");
    expect(rows[3]?.[1]).toBe("—"); // never finished: no time
    expect(rows[4]?.[1]).toBe("137");
    expect(rows[5]?.[1]).toBe("4");
    expect(rows[6]?.[1]).toBe("Yes");
  });

  it("names the variables, hides every value and says why when the server withheld them", async () => {
    answer = () =>
      Response.json(
        detail({
          masked: true,
          env: [
            { name: "DB_PASSWORD", masked: true },
            { name: "MODE", masked: true },
          ],
        }),
      );
    mount();
    const env = await screen.findByRole("region", { name: "Environment variables" });
    const rows = within(env).getAllByRole("row");
    expect(rows.map((r) => r.querySelectorAll("td")[0]?.textContent)).toEqual([
      "DB_PASSWORD",
      "MODE",
    ]);
    expect(within(env).getAllByText("••••••••")).toHaveLength(2);
    expect(within(env).queryByRole("button")).toBeNull();
    expect(
      within(env).getByText("Values are hidden. Needs the reveal secrets permission to show them."),
    ).toBeTruthy();
  });

  it("offers a Show value toggle for each variable the server sent a value for, and no caption", async () => {
    answer = () =>
      Response.json(detail({ env: [{ name: "DB_PASSWORD", value: "hunter2", masked: false }] }));
    mount();
    const env = await screen.findByRole("region", { name: "Environment variables" });
    expect(within(env).queryByText("hunter2")).toBeNull();
    expect(within(env).queryByText(/Values are hidden/)).toBeNull();
    fireEvent.click(within(env).getByRole("button", { name: "Show value of DB_PASSWORD" }));
    expect(within(env).getByText("hunter2")).toBeTruthy();
  });

  it("says so when there are no variables", async () => {
    mount();
    const env = await screen.findByRole("region", { name: "Environment variables" });
    expect(within(env).getByText("No environment variables.")).toBeTruthy();
    await waitFor(() => expect(card("State")).toBeTruthy());
  });
});

describe("Overview: the other cards", () => {
  const rows = (card: HTMLElement) =>
    within(card)
      .getAllByRole("row")
      .map((r) => Array.from(r.querySelectorAll("th,td")).map((c) => c.textContent));

  it("shows the image and command rows, with a dash for what is not set", async () => {
    answer = () =>
      Response.json(
        detail({
          command: ["nginx", "-g", "daemon off;"],
          entrypoint: ["/docker-entrypoint.sh"],
          workingDir: "/app",
        }),
      );
    mount();
    const card = await screen.findByRole("region", { name: "Image and command" });
    expect(rows(card)).toEqual([
      ["Image", "nginx:1.27"],
      ["Command", "nginx -g daemon off;"],
      ["Entrypoint", "/docker-entrypoint.sh"],
      ["Working directory", "/app"],
      ["User", "\u2014"],
    ]);
  });

  it("draws the command and entrypoint as masked values, with no text of them, when the server withheld them", async () => {
    answer = () =>
      Response.json(
        detail({
          command: ["<hidden>"],
          entrypoint: ["<hidden>"],
          commandMasked: true,
          masked: true,
        }),
      );
    mount();
    const card = await screen.findByRole("region", { name: "Image and command" });
    expect(rows(card)).toEqual([
      ["Image", "nginx:1.27"],
      ["Command", "••••••••"],
      ["Entrypoint", "••••••••"],
      ["Working directory", "\u2014"],
      ["User", "\u2014"],
    ]);
    expect(within(card).queryByText(/hidden>/)).toBeNull();
    expect(within(card).queryByRole("button")).toBeNull();
    expect(within(card).getByText(/The command line is hidden/)).toBeTruthy();
  });

  it("leaves an empty entrypoint as a dash even when the command was withheld", async () => {
    answer = () =>
      Response.json(
        detail({ command: ["<hidden>"], entrypoint: [], commandMasked: true, masked: true }),
      );
    mount();
    const card = await screen.findByRole("region", { name: "Image and command" });
    expect(rows(card)[2]).toEqual(["Entrypoint", "\u2014"]);
  });

  it("lists the published ports, or says there are none", async () => {
    answer = () =>
      Response.json(
        detail({
          ports: [
            { containerPort: 80, protocol: "tcp", hostIp: "0.0.0.0", hostPort: "8080" },
            { containerPort: 53, protocol: "udp", hostPort: "5353" },
          ],
        }),
      );
    mount();
    const card = await screen.findByRole("region", { name: "Ports" });
    expect(rows(card)).toEqual([
      ["Container port", "Host", "Protocol"],
      ["80", "0.0.0.0:8080", "tcp"],
      ["53", "5353", "udp"],
    ]);
    cleanup();
    client.clear();
    answer = () => Response.json(detail());
    mount();
    expect(await screen.findByText("No published ports.")).toBeTruthy();
  });

  it("lists the mounts with their mode, or says there are none", async () => {
    answer = () =>
      Response.json(
        detail({
          mounts: [
            {
              type: "volume",
              name: "data",
              source: "/var/lib/docker/volumes/data/_data",
              destination: "/data",
              rw: true,
            },
            {
              type: "bind",
              source: "/srv/site",
              destination: "/usr/share/nginx/html",
              mode: "z",
              rw: false,
            },
          ],
        }),
      );
    mount();
    const card = await screen.findByRole("region", { name: "Mounts" });
    expect(rows(card)).toEqual([
      ["Type", "Source", "Destination", "Mode"],
      ["volume", "data", "/data", "Read-write"],
      ["bind", "/srv/site", "/usr/share/nginx/html", "Read-only, z"],
    ]);
    // A volume's name opens its page on this container's node; a bind mount is only a path.
    const link = within(card).getByRole("link", { name: "data" });
    expect(link.getAttribute("href")).toBe("/env/e1/volumes/data?node=n1");
    expect(within(card).queryByRole("link", { name: "/srv/site" })).toBeNull();
    cleanup();
    client.clear();
    answer = () => Response.json(detail());
    mount();
    expect(await screen.findByText("No mounts.")).toBeTruthy();
  });

  it("lists the networks with their addresses and aliases, or says there are none", async () => {
    answer = () =>
      Response.json(
        detail({
          networks: [
            {
              networkId: "n1",
              name: "front",
              ipv4: "10.0.0.2",
              mac: "02:42:0a:00:00:02",
              aliases: ["web", "site"],
            },
            { networkId: "n2", name: "back", aliases: [] },
          ],
        }),
      );
    mount();
    const card = await screen.findByRole("region", { name: "Networks" });
    expect(rows(card)).toEqual([
      ["Network", "IPv4", "IPv6", "MAC", "Aliases", "Actions"],
      ["front", "10.0.0.2", "\u2014", "02:42:0a:00:00:02", "web, site", "Disconnect\u2026"],
      ["back", "\u2014", "\u2014", "\u2014", "\u2014", "Disconnect\u2026"],
    ]);
    cleanup();
    client.clear();
    answer = () => Response.json(detail());
    mount();
    expect(await screen.findByText("Not connected to any network.")).toBeTruthy();
  });

  it("words the restart policy, and shows the maximum only when it is set", async () => {
    mount();
    let card = await screen.findByRole("region", { name: "Restart policy" });
    expect(card.textContent).toContain("Never restart");
    expect(card.textContent).not.toContain("Maximum");
    cleanup();
    client.clear();
    answer = () => Response.json(detail({ restartPolicy: "on-failure", restartMaxRetries: 3 }));
    mount();
    await waitFor(() =>
      expect(screen.getByRole("region", { name: "Restart policy" }).textContent).toContain(
        "Maximum 3 retries",
      ),
    );
    card = screen.getByRole("region", { name: "Restart policy" });
    expect(card.textContent).toContain("Restart on failure");
    cleanup();
    client.clear();
    answer = () => Response.json(detail({ restartPolicy: "always" }));
    mount();
    await waitFor(() =>
      expect(screen.getByRole("region", { name: "Restart policy" }).textContent).toContain(
        "Always restart",
      ),
    );
    expect(screen.getByRole("region", { name: "Restart policy" }).textContent).not.toContain(
      "Maximum",
    );
  });

  it("lists the labels by key, or says there are none", async () => {
    answer = () => Response.json(detail({ labels: { "b.label": "two", "a.label": "one" } }));
    mount();
    const card = await screen.findByRole("region", { name: "Labels" });
    expect(rows(card)).toEqual([
      ["a.label", "one"],
      ["b.label", "two"],
    ]);
    cleanup();
    client.clear();
    answer = () => Response.json(detail());
    mount();
    expect(await screen.findByText("No labels.")).toBeTruthy();
  });

  it("puts State, Image and command, Restart policy, Ports and Mounts in the first column", async () => {
    mount();
    await screen.findByRole("region", { name: "State" });
    const column = (title: string) => screen.getByRole("region", { name: title }).parentElement;
    const first = column("State");
    for (const t of ["Image and command", "Restart policy", "Ports", "Mounts"]) {
      expect(column(t)).toBe(first);
    }
    const second = column("Networks");
    expect(second).not.toBe(first);
    expect(column("Labels")).toBe(second);
    expect(column("Environment variables")).toBe(second);
  });
});

describe("Overview: connecting and disconnecting from the Networks card", () => {
  const withNetworks = () =>
    detail({
      networks: [
        { networkId: "front-id", name: "front", ipv4: "10.0.0.2", aliases: ["web"] },
        { networkId: "back-id", name: "back", aliases: [] },
      ],
    });

  it("shows 'Connect to network' and 'Disconnect…' disabled with 'Needs Operator' for a Viewer", async () => {
    answer = () => Response.json(withNetworks());
    mount();
    const networks = await screen.findByRole("region", { name: "Networks" });
    await signedIn();
    const connect = await within(networks).findByRole("button", { name: "Connect to network" });
    expect(connect.getAttribute("aria-disabled")).toBe("true");
    const disconnect = within(networks).getByRole("button", { name: "Disconnect web from front" });
    expect(disconnect.getAttribute("aria-disabled")).toBe("true");
    expect(within(networks).getAllByText("Needs Operator").length).toBeGreaterThan(0);
    fireEvent.click(connect);
    fireEvent.click(disconnect);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(posts).toEqual([]);
  });

  it("opens the connect dialog for an Operator, also when the container is on no network", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    mount();
    expect(await screen.findByText("Not connected to any network.")).toBeTruthy();
    await signedIn();
    const connect = screen.getByRole("button", { name: "Connect to network" });
    expect(connect.getAttribute("aria-disabled")).toBeNull();
    fireEvent.click(connect);
    expect(await screen.findByRole("dialog", { name: "Connect web to a network" })).toBeTruthy();
  });

  it("confirms, then disconnects the container from that row's network on its node", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    answer = () => Response.json(withNetworks());
    mount();
    const row = await screen.findByRole("button", { name: "Disconnect web from front" });
    await signedIn();
    fireEvent.click(row);
    const dialog = await screen.findByRole("alertdialog", { name: "Disconnect web from front?" });
    expect(posts).toEqual([]);
    fireEvent.click(within(dialog).getByRole("button", { name: "Disconnect container" }));
    await waitFor(() => expect(posts).toHaveLength(1));
    expect(posts[0]).toEqual({
      path: `/api/env/${ENV}/networks/front-id/disconnect`,
      body: { containerId: "web-id", nodeId: "n1" },
    });
    expect(await screen.findByText("Disconnected web from front.")).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  });

  it("reads the container again after a disconnect so the card follows", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    answer = () => Response.json(withNetworks());
    mount();
    const row = await screen.findByRole("button", { name: "Disconnect web from front" });
    await signedIn();
    fireEvent.click(row);
    const dialog = await screen.findByRole("alertdialog");
    const before = urls.length;
    fireEvent.click(within(dialog).getByRole("button", { name: "Disconnect container" }));
    await waitFor(() => expect(urls.length).toBeGreaterThan(before));
  });
});
