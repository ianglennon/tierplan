// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { LocalHostPanel } from "@/features/environments/local-host-panel";
import "@/i18n";
import { settled } from "@/test-settle";

const toastError = vi.fn();
vi.mock("@/lib/toast", () => ({
  toastError: (message: string) => toastError(message),
  toastSuccess: vi.fn(),
}));

const SOCKET = "/var/run/docker.sock";

function probe(over: Record<string, unknown> = {}) {
  return {
    probedAt: "2026-10-01T10:00:00Z",
    reachable: true,
    supported: true,
    engineName: "Docker Engine",
    engineVersion: "29.8.2",
    apiVersion: "1.56",
    hostname: "lab-1",
    type: "standalone",
    ...over,
  };
}

function host(over: Record<string, unknown> = {}) {
  return { socketPath: SOCKET, detected: true, managed: false, probe: probe(), ...over };
}

interface Call {
  path: string;
  method: string;
  body?: Record<string, unknown>;
}

let calls: Call[];
let localHost: Record<string, unknown>;
let postAnswer: () => Response | Promise<Response>;

beforeEach(() => {
  calls = [];
  toastError.mockClear();
  localHost = host();
  postAnswer = () => Response.json({ id: "e1" }, { status: 201 });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const call: Call = { path, method: req.method };
      if (req.method === "POST") call.body = await req.clone().json();
      calls.push(call);
      if (path === "/api/local-host") return Response.json(localHost);
      if (path === "/api/version") {
        return Response.json({
          commit: "",
          display: "v0.1.0",
          sourceTreeUrl: "https://example.test/tree",
          sourceUrl: "https://example.test",
          version: "v0.1.0",
        });
      }
      if (path === "/api/environments" && req.method === "POST") return postAnswer();
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const posts = () => calls.filter((c) => c.method === "POST");
const gets = (path: string) => calls.filter((c) => c.path === path && c.method === "GET");

function renderPanel(props: { mode?: "wizard" | "settings"; onDone?: () => void } = {}) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const invalidate = vi.spyOn(client, "invalidateQueries");
  const onDone = props.onDone ?? vi.fn();
  render(
    <QueryClientProvider client={client}>
      <LocalHostPanel
        mode={props.mode ?? "wizard"}
        onDone={props.mode === "settings" ? undefined : onDone}
      />
    </QueryClientProvider>,
  );
  return { onDone, invalidate, client };
}

// While pending, the spinner's label and the text both name the button.
const pendingButton = () => screen.getByRole("button", { name: /Adding/ }) as HTMLButtonElement;
const button = (name: string) => screen.getByRole("button", { name }) as HTMLButtonElement;

describe("LocalHostPanel, a detected host", () => {
  it("shows the engine, the type and the root-equivalence notice, and adds nothing by itself", async () => {
    const { client } = renderPanel();

    expect(await screen.findByRole("heading", { name: "Manage this Docker host?" })).toBeTruthy();
    expect(
      screen.getByText("Coaming found Docker on lab-1 and can manage it through the socket."),
    ).toBeTruthy();
    expect(screen.getByText("Docker Engine 29.8.2")).toBeTruthy();
    expect(screen.getByText("Standalone")).toBeTruthy();
    expect(
      screen.getByText(
        "Managing a host through the Docker socket gives Coaming, and any admin who signs in here, root-equivalent control of that host.",
      ),
    ).toBeTruthy();
    expect(button("Manage this host")).toBeTruthy();
    expect(button("Skip for now")).toBeTruthy();

    // D-19: loading the panel, however long it sits there, never adds the host.
    await settled(client);
    expect(posts()).toEqual([]);
    expect(gets("/api/local-host")).toHaveLength(1);
  });

  it("labels the type of a swarm manager", async () => {
    localHost = host({ probe: probe({ type: "swarm-manager" }) });
    renderPanel();
    expect(await screen.findByText("Swarm manager")).toBeTruthy();
    expect(screen.queryByText(/This host is a swarm worker/)).toBeNull();
  });

  it("warns on a swarm worker and still offers Manage this host", async () => {
    localHost = host({ probe: probe({ type: "swarm-worker" }) });
    renderPanel();

    expect(await screen.findByText("Swarm worker")).toBeTruthy();
    expect(
      screen.getByText(
        "This host is a swarm worker. Pin Coaming to a manager node to manage the swarm. Coaming will show node-local data only.",
      ),
    ).toBeTruthy();
    expect(button("Manage this host").disabled).toBe(false);
  });

  it("adds the local host only when Manage this host is clicked, then reports done", async () => {
    const { onDone, invalidate } = renderPanel();
    await screen.findByRole("heading", { name: "Manage this Docker host?" });

    fireEvent.click(button("Manage this host"));

    await waitFor(() => expect(onDone).toHaveBeenCalledTimes(1));
    expect(posts()).toEqual([
      { path: "/api/environments", method: "POST", body: { kind: "local" } },
    ]);
    const keys = invalidate.mock.calls.map((c) => (c[0] as { queryKey: unknown }).queryKey);
    expect(keys).toContainEqual(["local-host"]);
    expect(keys).toContainEqual(["environments"]);
  });

  it("disables the buttons and shows the spinner while the request is pending", async () => {
    let release: (r: Response) => void = () => {};
    postAnswer = () => new Promise<Response>((resolve) => (release = resolve));
    const { onDone } = renderPanel();
    await screen.findByRole("heading", { name: "Manage this Docker host?" });

    fireEvent.click(button("Manage this host"));

    await waitFor(() => expect(pendingButton().disabled).toBe(true));
    expect(button("Skip for now").disabled).toBe(true);
    // A second click while pending sends nothing more.
    fireEvent.click(pendingButton());
    expect(posts()).toHaveLength(1);

    release(Response.json({ id: "e1" }, { status: 201 }));
    await waitFor(() => expect(onDone).toHaveBeenCalledTimes(1));
  });

  it("treats local_environment_exists as already done", async () => {
    postAnswer = () =>
      Response.json({ status: 409, type: "local_environment_exists" }, { status: 409 });
    const { onDone } = renderPanel();
    await screen.findByRole("heading", { name: "Manage this Docker host?" });

    fireEvent.click(button("Manage this host"));

    await waitFor(() => expect(onDone).toHaveBeenCalledTimes(1));
    expect(toastError).not.toHaveBeenCalled();
  });

  it("looks again, without finishing, when the socket has gone away", async () => {
    postAnswer = () => Response.json({ status: 409, type: "socket_not_found" }, { status: 409 });
    const { onDone } = renderPanel();
    await screen.findByRole("heading", { name: "Manage this Docker host?" });

    localHost = { socketPath: SOCKET, detected: false, managed: false };
    fireEvent.click(button("Manage this host"));

    expect(await screen.findByRole("heading", { name: "No Docker socket found" })).toBeTruthy();
    expect(onDone).not.toHaveBeenCalled();
    expect(toastError).not.toHaveBeenCalled();
  });

  it("toasts on any other failure and stays", async () => {
    postAnswer = () => Response.json({ status: 500 }, { status: 500 });
    const { onDone } = renderPanel();
    await screen.findByRole("heading", { name: "Manage this Docker host?" });

    fireEvent.click(button("Manage this host"));

    await waitFor(() =>
      expect(toastError).toHaveBeenCalledWith("Something went wrong. Try again."),
    );
    expect(onDone).not.toHaveBeenCalled();
    // The button works again.
    expect(button("Manage this host").disabled).toBe(false);
  });

  it("skips without sending anything", async () => {
    const { onDone } = renderPanel();
    await screen.findByRole("heading", { name: "Manage this Docker host?" });

    fireEvent.click(button("Skip for now"));

    expect(onDone).toHaveBeenCalledTimes(1);
    expect(posts()).toEqual([]);
  });

  it("has no skip button in Settings, where there is nothing to skip", async () => {
    renderPanel({ mode: "settings" });
    await screen.findByRole("heading", { name: "Manage this Docker host?" });
    expect(screen.queryByRole("button", { name: "Skip for now" })).toBeNull();
    expect(button("Manage this host")).toBeTruthy();
  });

  it("renders nothing once the host is managed", async () => {
    localHost = host({ managed: true });
    const { onDone, client } = renderPanel();
    await waitFor(() => expect(gets("/api/local-host")).toHaveLength(1));
    await settled(client);
    expect(screen.queryByRole("heading")).toBeNull();
    expect(screen.queryByRole("button")).toBeNull();
    expect(onDone).not.toHaveBeenCalled();
  });
});

describe("LocalHostPanel, an unsupported engine", () => {
  beforeEach(() => {
    localHost = host({
      probe: probe({
        supported: false,
        reason: "api_too_old",
        engineVersion: "22.0.0",
        apiVersion: "1.41",
      }),
    });
  });

  it("names the engine, version and API, and adds nothing by itself", async () => {
    const { client } = renderPanel();

    expect(await screen.findByText("Unsupported")).toBeTruthy();
    expect(
      screen.getByText(
        "Docker Engine 22.0.0 (API 1.41) isn't supported. Coaming needs Docker Engine 24 or later.",
      ),
    ).toBeTruthy();
    expect(
      screen.getByText("It will appear as unsupported, with no operations available."),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Manage this host" })).toBeNull();
    await settled(client);
    expect(posts()).toEqual([]);
  });

  it("adds it with allowUnsupported only on Add this host anyway", async () => {
    const { onDone } = renderPanel();
    await screen.findByText("Unsupported");

    fireEvent.click(button("Add this host anyway"));

    await waitFor(() => expect(onDone).toHaveBeenCalledTimes(1));
    expect(posts()).toEqual([
      {
        path: "/api/environments",
        method: "POST",
        body: { kind: "local", allowUnsupported: true },
      },
    ]);
  });

  it("uses the Podman copy for Podman", async () => {
    localHost = host({
      probe: probe({
        supported: false,
        reason: "podman",
        engineName: "Podman",
        engineVersion: "5.2.1",
      }),
    });
    renderPanel();

    expect(
      await screen.findByText("Podman 5.2.1 detected. Coaming manages Docker Engine only."),
    ).toBeTruthy();
    expect(screen.queryByText(/isn't supported/)).toBeNull();
  });
});

describe("LocalHostPanel, a socket that does not answer", () => {
  it("says so and offers Check again, but not Manage this host", async () => {
    localHost = host({
      probe: probe({ reachable: false, supported: false, reason: "unreachable" }),
    });
    renderPanel();

    expect(await screen.findByRole("heading", { name: "Docker isn't answering" })).toBeTruthy();
    expect(
      screen.getByText(
        /Coaming found a Docker socket at \/var\/run\/docker.sock, but Docker didn't answer/,
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Manage this host" })).toBeNull();

    localHost = host();
    fireEvent.click(button("Check again"));
    expect(await screen.findByRole("button", { name: "Manage this host" })).toBeTruthy();
    expect(posts()).toEqual([]);
  });
});

describe("LocalHostPanel, no Docker socket", () => {
  beforeEach(() => {
    localHost = { socketPath: SOCKET, detected: false, managed: false };
  });

  it("shows the install snippet, the footnote and Check again", async () => {
    const { client } = renderPanel();

    expect(await screen.findByRole("heading", { name: "No Docker socket found" })).toBeTruthy();
    expect(
      screen.getByText(
        "Coaming didn't find a Docker socket at /var/run/docker.sock. To manage this host, start Coaming with the socket mounted:",
      ),
    ).toBeTruthy();
    const code = (
      await screen.findByRole("button", { name: "Copy the docker run command" })
    ).closest("section");
    const text = code?.querySelector("code")?.textContent ?? "";
    expect(text).toContain("-p 7480:7480");
    expect(text).toContain("-v coaming-data:/data");
    expect(text).toContain("-v /var/run/docker.sock:/var/run/docker.sock");
    // The running version is v0.1.0, so the snippet names the 0.1 line.
    expect(text).toContain("ghcr.io/ianglennon/coaming:0.1");
    expect(text).not.toContain(":latest");
    expect(
      screen.getByText("You can add remote hosts and swarms after setup from Environments."),
    ).toBeTruthy();
    expect(button("Continue")).toBeTruthy();
    await settled(client);
    expect(posts()).toEqual([]);
  });

  it("leaves the after-setup footnote out of Settings and the overview", async () => {
    renderPanel({ mode: "settings" });
    expect(await screen.findByRole("heading", { name: "No Docker socket found" })).toBeTruthy();
    expect(screen.queryByText(/add remote hosts and swarms after setup/)).toBeNull();
  });

  it("looks again on Check again and shows the host once Docker is there", async () => {
    renderPanel();
    await screen.findByRole("heading", { name: "No Docker socket found" });

    localHost = host();
    fireEvent.click(button("Check again"));

    expect(await screen.findByRole("heading", { name: "Manage this Docker host?" })).toBeTruthy();
    expect(gets("/api/local-host")).toHaveLength(2);
    expect(posts()).toEqual([]);
  });

  it("continues without sending anything", async () => {
    const { onDone } = renderPanel();
    await screen.findByRole("heading", { name: "No Docker socket found" });

    fireEvent.click(button("Continue"));

    expect(onDone).toHaveBeenCalledTimes(1);
    expect(posts()).toEqual([]);
  });

  it("has no Continue button in Settings", async () => {
    renderPanel({ mode: "settings" });
    await screen.findByRole("heading", { name: "No Docker socket found" });
    expect(screen.queryByRole("button", { name: "Continue" })).toBeNull();
    expect(button("Check again")).toBeTruthy();
  });
});

describe("LocalHostPanel, a failed lookup", () => {
  it("shows an error with Try again and a way past it", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => Response.json({ status: 500 }, { status: 500 })),
    );
    const { onDone } = renderPanel();

    expect(await screen.findByText("Couldn't look for a Docker socket.")).toBeTruthy();
    expect(button("Try again")).toBeTruthy();
    fireEvent.click(button("Skip for now"));
    expect(onDone).toHaveBeenCalledTimes(1);
  });
});
