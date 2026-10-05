// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { InstallInfo } from "@/api/environments";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { EnvHealthStreamProvider } from "@/features/environments/health-stream";
import { InstallDialog } from "@/features/environments/install-dialog";
import "@/i18n";

const TOKEN = "bpjt_E1_s3cr3tvalue";
const MASK = "••••••••";

function install(extra: Partial<InstallInfo> = {}, generation = 1): InstallInfo {
  const secret = `bayplan-agent-token-g${generation}`;
  return {
    agentUrl: "wss://bayplan.example.com/agent/connect",
    generation,
    image: "ghcr.io/ianglennon/bayplan-agent:0.1.0",
    joinToken: TOKEN,
    manualCleanup: {
      standalone: " docker rm -f bayplan-agent",
      swarm: " docker service rm bayplan-agent",
    },
    secretName: secret,
    standalone: {
      command: ` docker run -d --name bayplan-agent -e BAYPLAN_AGENT_JOIN_TOKEN='${TOKEN}' ghcr.io/ianglennon/bayplan-agent:0.1.0`,
      composeFile: `services:\n  agent:\n    environment:\n      BAYPLAN_AGENT_JOIN_TOKEN: '${TOKEN}'\n`,
    },
    swarm: {
      command: ` printf %s '${TOKEN}' | docker secret create ${secret} - && docker service create --name bayplan-agent ghcr.io/ianglennon/bayplan-agent:0.1.0`,
      stackFile: `# printf %s '${TOKEN}' | docker secret create ${secret} -\nservices:\n  agent:\n    image: ghcr.io/ianglennon/bayplan-agent:0.1.0\n`,
      ...(generation > 1
        ? {
            rotateCommand: ` OLD=$(docker service inspect bayplan-agent) && printf %s '${TOKEN}' | docker secret create ${secret} -`,
          }
        : {}),
    },
    ...extra,
  };
}

interface Sent {
  method: string;
  path: string;
  body: unknown;
}

let sent: Sent[];
let createResponse: () => Response | Promise<Response>;
let rotateResponse: () => Response | Promise<Response>;
let writeText: ReturnType<typeof vi.fn<(text: string) => Promise<void>>>;
let client: QueryClient;
let onOpenChange: ReturnType<typeof vi.fn<(open: boolean) => void>>;
let router: ReturnType<typeof makeRouter>;

type Props = { mode: "add" | "rotate"; envId?: string };

function Harness(props: Props) {
  const [open, setOpen] = useState(true);
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        reopen
      </button>
      <InstallDialog
        open={open}
        mode={props.mode}
        envId={props.envId}
        onOpenChange={(next) => {
          onOpenChange(next);
          setOpen(next);
        }}
      />
    </>
  );
}

function makeRouter(props: Props) {
  const root = createRootRoute({ component: () => <Harness {...props} /> });
  const settings = createRoute({
    getParentRoute: () => root,
    path: "/settings/general",
    component: () => <p>general settings</p>,
  });
  const env = createRoute({
    getParentRoute: () => root,
    path: "/env/$envId",
    component: () => null,
  });
  return createRouter({
    routeTree: root.addChildren([settings, env]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
}

async function mount(props: Props) {
  router = makeRouter(props);
  render(
    <QueryClientProvider client={client}>
      <EnvHealthStreamProvider>
        <RouterProvider router={router} />
      </EnvHealthStreamProvider>
    </QueryClientProvider>,
  );
  await screen.findByRole("dialog");
}

function created(extra: Partial<InstallInfo> = {}) {
  return () =>
    Response.json(
      {
        id: "e9",
        name: "edge",
        kind: "agent",
        enabled: true,
        isSelfHost: false,
        isLocalSocket: false,
        createdAt: "2026-10-03T10:00:00Z",
        engine: {},
        status: { state: "pending" },
        agentVersion: null,
        agents: null,
        engineVersion: null,
        failingServices: null,
        lastSeen: null,
        nodes: null,
        updateAvailable: false,
        install: install(extra),
      },
      { status: 201 },
    );
}

beforeEach(() => {
  sent = [];
  onOpenChange = vi.fn<(open: boolean) => void>();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  createResponse = created();
  rotateResponse = () =>
    Response.json({ install: install({}, 2), envType: undefined, hasAgents: false });
  writeText = vi.fn<(text: string) => Promise<void>>(async () => {});
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      let body: unknown;
      if (req.method !== "GET")
        body = await req
          .clone()
          .json()
          .catch(() => undefined);
      sent.push({ method: req.method, path, body });
      if (req.method === "POST" && path === "/api/environments") return createResponse();
      if (req.method === "POST" && path === "/api/environments/e1/token") return rotateResponse();
      return Response.json({}, { status: 404 });
    }),
  );
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  localStorage.clear();
  sessionStorage.clear();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
  Object.defineProperty(navigator, "clipboard", { value: undefined, configurable: true });
});

const posts = () => sent.filter((s) => s.method === "POST");
const nameField = () => screen.getByLabelText("Name (optional)") as HTMLInputElement;
const submit = () =>
  fireEvent.click(screen.getByRole("button", { name: "Generate install command" }));
const dialog = () => screen.getByRole("dialog");

async function createWith(name: string) {
  fireEvent.change(nameField(), { target: { value: name } });
  submit();
  return screen.findByRole("heading", { name: "Install the agent" });
}

describe("InstallDialog add flow, step 1", () => {
  it("asks for an optional name and makes no request until it is submitted", async () => {
    await mount({ mode: "add" });
    expect(screen.getByRole("heading", { name: "Add environment" })).toBeTruthy();
    expect(
      screen.getByText("Name it now, or let the first agent name it after its host."),
    ).toBeTruthy();
    expect(
      screen.getByText(
        "Leave this empty to use the host's name when the first agent connects. You can rename it later.",
      ),
    ).toBeTruthy();
    expect(nameField().required).toBe(false);
    expect(sent).toEqual([]);
  });

  it("says Use 64 characters or fewer for a 65 character name and posts nothing", async () => {
    await mount({ mode: "add" });
    fireEvent.change(nameField(), { target: { value: "x".repeat(65) } });
    submit();
    expect(await screen.findByText("Use 64 characters or fewer.")).toBeTruthy();
    expect(nameField().getAttribute("aria-invalid")).toBe("true");
    expect(posts()).toEqual([]);
  });

  it("accepts exactly 64 characters, counting a character once however it is encoded", async () => {
    await mount({ mode: "add" });
    await createWith("\u{1F433}".repeat(64));
    expect(posts()[0]?.body).toEqual({ kind: "agent", name: "\u{1F433}".repeat(64) });
  });

  it("posts kind agent with no name when the field is blank", async () => {
    await mount({ mode: "add" });
    submit();
    await screen.findByRole("heading", { name: "Install the agent" });
    expect(posts()).toHaveLength(1);
    expect(posts()[0]?.path).toBe("/api/environments");
    expect(posts()[0]?.body).toEqual({ kind: "agent" });
  });

  it("disables the field, shows the spinner and posts once however often it is clicked", async () => {
    let release: (r: Response) => void = () => {};
    createResponse = () => new Promise<Response>((resolve) => (release = resolve));
    await mount({ mode: "add" });
    fireEvent.change(nameField(), { target: { value: "edge" } });

    const button = screen.getByRole("button", { name: "Generate install command" });
    fireEvent.click(button);
    fireEvent.click(button);
    fireEvent.submit(button.closest("form") as HTMLFormElement);

    await waitFor(() => expect(nameField().matches(":disabled")).toBe(true));
    const pending = screen.getByRole("button", { name: "Generating…" });
    expect(pending.querySelector("[data-slot=spinner], svg")).toBeTruthy();
    expect(pending.matches(":disabled")).toBe(true);
    // The dialog cannot be dismissed while the call runs.
    fireEvent.keyDown(dialog(), { key: "Escape" });
    expect(onOpenChange).not.toHaveBeenCalled();
    expect(posts()).toHaveLength(1);
    expect(posts()[0]?.body).toEqual({ kind: "agent", name: "edge" });

    await act(async () => release(created()() as Response));
    await screen.findByRole("heading", { name: "Install the agent" });
    expect(posts()).toHaveLength(1);
  });

  it("moves focus to the step 2 heading", async () => {
    await mount({ mode: "add" });
    const heading = await createWith("edge");
    await waitFor(() => expect(document.activeElement).toBe(heading));
    expect(heading.tagName).toBe("H2");
  });

  it("shows a role=alert and stays on step 1 when the server fails", async () => {
    createResponse = () => Response.json({ status: 500, title: "boom" }, { status: 500 });
    await mount({ mode: "add" });
    fireEvent.change(nameField(), { target: { value: "edge" } });
    submit();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(
      "Couldn't create the environment. Try again, and check the Bayplan logs if it keeps happening.",
    );
    expect(screen.queryByRole("heading", { name: "Install the agent" })).toBeNull();
    expect(nameField().matches(":disabled")).toBe(false);
    expect(nameField().value).toBe("edge");
    expect(screen.getByRole("button", { name: "Generate install command" })).toBeTruthy();
  });

  it("explains a missing public URL and links to the settings", async () => {
    createResponse = () =>
      Response.json(
        { status: 409, type: "public_url_required", title: "Conflict" },
        { status: 409, headers: { "Content-Type": "application/problem+json" } },
      );
    await mount({ mode: "add" });
    submit();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Set the public URL or the agent URL in Settings");
    expect(within(alert).getByRole("link", { name: "Settings" })).toBeTruthy();
  });
});

const insecureAgentUrlProblem = () =>
  Response.json(
    { status: 409, type: "insecure_agent_url", title: "Conflict" },
    { status: 409, headers: { "Content-Type": "application/problem+json" } },
  );

// G-02-R4-3: a plain ws:// address for another host is refused by Bayplan and
// by agents. The dialog says why, in its own words, and where to fix it.
describe("InstallDialog insecure agent address", () => {
  const generic = {
    create:
      "Couldn't create the environment. Try again, and check the Bayplan logs if it keeps happening.",
    rotate:
      "Couldn't generate a new token. Try again, and check the Bayplan logs if it keeps happening.",
  };

  it("explains the refusal when adding an environment, with a Settings link and no generic failure", async () => {
    createResponse = insecureAgentUrlProblem;
    await mount({ mode: "add" });
    fireEvent.change(nameField(), { target: { value: "edge" } });
    submit();
    const alert = await screen.findByRole("alert");
    expect(screen.getAllByRole("alert")).toHaveLength(1);
    expect(alert.textContent).toContain("plain ws://");
    expect(alert.textContent).toContain("BAYPLAN_AGENT_ALLOW_INSECURE");
    expect(alert.textContent).not.toContain(generic.create);
    const link = within(alert).getByRole("link", { name: "Settings" });
    expect(link.getAttribute("href")).toBe("/settings/general");
    expect(screen.queryByRole("heading", { name: "Install the agent" })).toBeNull();
    expect(nameField().matches(":disabled")).toBe(false);
    expect(nameField().value).toBe("edge");
  });

  it("explains the refusal when rotating a token, without the generic rotate failure", async () => {
    rotateResponse = insecureAgentUrlProblem;
    await mount({ mode: "rotate", envId: "e1" });
    fireEvent.click(screen.getByRole("button", { name: "Generate new token" }));
    // The confirm step has its own notice, so pick the failure by its content.
    const alert = await screen.findByText(/plain ws:\/\//, { selector: "[role=alert] *" });
    const failure = alert.closest("[role=alert]") as HTMLElement;
    expect(
      screen.getAllByRole("alert").filter((a) => a.textContent?.includes("plain ws://")),
    ).toHaveLength(1);
    expect(failure.textContent).toContain("BAYPLAN_AGENT_ALLOW_INSECURE");
    expect(
      screen
        .getAllByRole("alert")
        .map((a) => a.textContent)
        .join("|"),
    ).not.toContain(generic.rotate);
    expect(within(failure).getByRole("link", { name: "Settings" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Install the agent" })).toBeNull();
  });
});

describe("InstallDialog step 2", () => {
  it("selects Swarm first and masks the token in the command", async () => {
    await mount({ mode: "add" });
    await createWith("edge");

    const tabs = screen.getAllByRole("tab").map((t) => t.textContent);
    expect(tabs).toEqual(["Swarm", "Standalone"]);
    expect(screen.getByRole("tab", { name: "Swarm" }).getAttribute("aria-selected")).toBe("true");
    const shown = dialog().querySelector("code")?.textContent ?? "";
    expect(shown.startsWith(" printf %s '")).toBe(true);
    expect(shown).toContain(`'${MASK}'`);
    expect(dialog().textContent).not.toContain(TOKEN);
    expect(
      screen.getByText(/Run it on any manager node\. It creates a Docker secret/),
    ).toBeTruthy();
    expect(screen.getByText(/Run one of these on a swarm manager/)).toBeTruthy();
  });

  it("shows the stack file with the helper and the secret sentence on the Swarm compose view", async () => {
    await mount({ mode: "add" });
    await createWith("edge");

    fireEvent.click(screen.getByRole("radio", { name: "Compose file" }));

    expect(dialog().querySelector("code")?.textContent).toContain("# printf %s");
    expect(dialog().querySelector("code")?.textContent).toContain(`'${MASK}'`);
    expect(dialog().textContent).not.toContain(TOKEN);
    expect(dialog().textContent).toContain(
      "Save this as bayplan-agent.yml and run docker stack deploy -c bayplan-agent.yml bayplan-agent.",
    );
    expect(dialog().textContent).toContain(
      "Create the secret first with the command in the file's first line.",
    );
    expect(screen.getByRole("button", { name: "Copy compose file" })).toBeTruthy();
  });

  it("shows the standalone command and its compose file without the secret sentence", async () => {
    await mount({ mode: "add" });
    await createWith("edge");

    fireEvent.mouseDown(screen.getByRole("tab", { name: "Standalone" }), { button: 0 });
    fireEvent.click(screen.getByRole("tab", { name: "Standalone" }));
    expect(dialog().querySelector("code")?.textContent).toContain(
      "docker run -d --name bayplan-agent",
    );
    expect(dialog().textContent).toContain(
      "Run it on the host. It starts the agent container with the Docker socket mounted.",
    );

    fireEvent.click(screen.getByRole("radio", { name: "Compose file" }));
    expect(dialog().textContent).toContain(
      "Save this as bayplan-agent.yml and run docker compose -f bayplan-agent.yml up -d.",
    );
    expect(dialog().textContent).not.toContain("Create the secret first");
    expect(dialog().textContent).not.toContain(TOKEN);
  });

  it("lists the image and address, a link to change the address, and no pin unless there is one", async () => {
    await mount({ mode: "add" });
    await createWith("edge");

    expect(dialog().textContent).toContain("Agent image");
    expect(dialog().textContent).toContain("ghcr.io/ianglennon/bayplan-agent:0.1.0");
    expect(dialog().textContent).toContain("Connects to");
    expect(dialog().textContent).toContain("wss://bayplan.example.com/agent/connect");
    expect(screen.getByRole("link", { name: "Change the agent URL" }).getAttribute("href")).toBe(
      "/settings/general",
    );
    expect(screen.queryByText("Server certificate pin")).toBeNull();
  });

  it("shows the certificate pin row when the install has a pin", async () => {
    createResponse = created({ pin: "sha256/AbCdEf0123=" });
    await mount({ mode: "add" });
    await createWith("edge");
    expect(screen.getByText("Server certificate pin")).toBeTruthy();
    expect(screen.getByText("sha256/AbCdEf0123=")).toBeTruthy();
  });

  it("shows the secret notice and no button but Close, Copy and Show token", async () => {
    await mount({ mode: "add" });
    await createWith("edge");

    expect(dialog().textContent).toContain("This command contains a secret.");
    expect(dialog().textContent).toContain("rotate the token from Settings");
    const buttons = within(dialog())
      .getAllByRole("button")
      .map((b) => b.getAttribute("aria-label") ?? b.textContent);
    expect(buttons).toContain("Close");
    expect(buttons).not.toContain("Next");
    expect(buttons).not.toContain("Done");
    expect(buttons.filter((b) => b === "Close")).toHaveLength(1);
  });

  it("Show token reveals it with aria-pressed, and Copy writes the real command with its leading space", async () => {
    await mount({ mode: "add" });
    await createWith("edge");

    fireEvent.click(screen.getByRole("button", { name: "Copy command" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    const copied = String(writeText.mock.calls[0]?.[0]);
    expect(copied.startsWith(" printf %s '")).toBe(true);
    expect(copied).toContain(`'${TOKEN}'`);
    expect(dialog().textContent).not.toContain(TOKEN);

    const toggle = screen.getByRole("button", { name: "Show token" });
    fireEvent.click(toggle);
    expect(screen.getByRole("button", { name: "Hide token" }).getAttribute("aria-pressed")).toBe(
      "true",
    );
    expect(dialog().querySelector("code")?.textContent).toBe(copied);
  });

  it("closes from the Close button", async () => {
    await mount({ mode: "add" });
    await createWith("edge");
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("closes the dialog when the agent URL link is followed", async () => {
    await mount({ mode: "add" });
    await createWith("edge");
    fireEvent.click(screen.getByRole("link", { name: "Change the agent URL" }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });
});

describe("InstallDialog and the waiting panel", () => {
  it("shows the waiting panel under the notice and follows the new environment's events", async () => {
    await mount({ mode: "add" });
    await createWith("edge");
    const panel = screen.getByText("Waiting for the first agent…").closest("[role=status]");
    if (!panel) throw new Error("the waiting panel is not a status region");
    expect(panel.textContent).toContain("Waiting for the first agent…");

    const stream = FakeEventSource.instances[0];
    await act(async () => {
      stream?.open();
      stream?.sendFrame(
        "agent.refused",
        {
          envId: "e9",
          at: new Date().toISOString(),
          code: "auth_rejected",
          mode: "enrol",
          node: "n",
          ip: "203.0.113.9",
        },
        "1",
      );
    });
    expect(
      within(dialog()).getByText(/Refused: Bayplan rejected the agent's credentials\./),
    ).toBeTruthy();
    // The panel is in the dialog, not another place on the page.
    expect(dialog().contains(panel)).toBe(true);
  });

  it("leaves the environment alone when the dialog is closed with the panel waiting", async () => {
    await mount({ mode: "add" });
    await createWith("edge");
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(sent.filter((s) => s.method === "DELETE")).toEqual([]);
  });

  it("shows the panel for Finish setup on a pending environment", async () => {
    await mount({ mode: "rotate", envId: "e1" });
    fireEvent.click(screen.getByRole("button", { name: "Generate new token" }));
    await screen.findByRole("heading", { name: "Install the agent" });
    expect(dialog().textContent).toContain("Waiting for the first agent…");
  });

  it("does not wait for a first agent on an environment that already has them", async () => {
    rotateResponse = () =>
      Response.json({ install: install({}, 2), envType: "swarm", hasAgents: true });
    await mount({ mode: "rotate", envId: "e1" });
    fireEvent.click(screen.getByRole("button", { name: "Generate new token" }));
    await screen.findByRole("heading", { name: "Install the agent" });
    expect(dialog().textContent).not.toContain("Waiting for the first agent…");
  });
});

describe("InstallDialog rotate flow", () => {
  it("opens on the confirm and makes no request when it is closed", async () => {
    await mount({ mode: "rotate", envId: "e1" });
    expect(screen.getByRole("heading", { name: "Show install command" })).toBeTruthy();
    expect(dialog().textContent).toContain(
      "This replaces the previous token. Install commands you copied earlier stop working. Agents that are already enrolled aren't affected.",
    );
    // Focus starts on Cancel, as in every destructive dialog.
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Cancel" }));

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(sent).toEqual([]);
  });

  it("Escape cancels without a request", async () => {
    await mount({ mode: "rotate", envId: "e1" });
    fireEvent.keyDown(dialog(), { key: "Escape" });
    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(sent).toEqual([]);
  });

  it("posts the rotation only when Generate new token is pressed, then shows step 2 for a first install", async () => {
    await mount({ mode: "rotate", envId: "e1" });
    fireEvent.click(screen.getByRole("button", { name: "Generate new token" }));

    const heading = await screen.findByRole("heading", { name: "Install the agent" });
    expect(posts()).toHaveLength(1);
    expect(posts()[0]?.path).toBe("/api/environments/e1/token");
    await waitFor(() => expect(document.activeElement).toBe(heading));
    expect(screen.getAllByRole("tab").map((t) => t.textContent)).toEqual(["Swarm", "Standalone"]);
    // A first install gets the full command, with the next generation's secret.
    expect(dialog().querySelector("code")?.textContent).toContain("bayplan-agent-token-g2");
    expect(screen.queryByText("Update the join token on this swarm")).toBeNull();
  });

  it("does not post twice when Generate new token is pressed twice", async () => {
    let release: (r: Response) => void = () => {};
    rotateResponse = () => new Promise<Response>((resolve) => (release = resolve));
    await mount({ mode: "rotate", envId: "e1" });
    const button = screen.getByRole("button", { name: "Generate new token" });
    fireEvent.click(button);
    fireEvent.click(button);
    await waitFor(() => expect(screen.getByRole("button", { name: "Generating…" })).toBeTruthy());
    expect(posts()).toHaveLength(1);
    await act(async () => release(Response.json({ install: install({}, 2), hasAgents: false })));
    await screen.findByRole("heading", { name: "Install the agent" });
  });

  it("shows only the Swarm tab, with the update heading and the rotate command, for a swarm that has agents", async () => {
    rotateResponse = () =>
      Response.json({ install: install({}, 2), envType: "swarm", hasAgents: true });
    await mount({ mode: "rotate", envId: "e1" });
    fireEvent.click(screen.getByRole("button", { name: "Generate new token" }));
    await screen.findByRole("heading", { name: "Install the agent" });

    expect(screen.getAllByRole("tab").map((t) => t.textContent)).toEqual(["Swarm"]);
    expect(screen.getByText("Update the join token on this swarm")).toBeTruthy();
    expect(
      screen.getByText(
        "Docker secrets can't be edited, so this command removes the old secret and adds the new one.",
      ),
    ).toBeTruthy();
    expect(dialog().querySelector("code")?.textContent).toContain(" OLD=$(docker service inspect");
    // The service name differs for a stack-file install: the dialog says so.
    expect(dialog().textContent).toContain("If you installed from the stack file");
    expect(screen.queryByRole("radio", { name: "Compose file" })).toBeNull();
    expect(dialog().textContent).not.toContain(TOKEN);
  });

  it("shows only the Standalone tab for a standalone environment that has agents", async () => {
    rotateResponse = () =>
      Response.json({ install: install({}, 2), envType: "standalone", hasAgents: true });
    await mount({ mode: "rotate", envId: "e1" });
    fireEvent.click(screen.getByRole("button", { name: "Generate new token" }));
    await screen.findByRole("heading", { name: "Install the agent" });

    expect(screen.getAllByRole("tab").map((t) => t.textContent)).toEqual(["Standalone"]);
    expect(dialog().querySelector("code")?.textContent).toContain("docker run -d");
  });

  it("shows a role=alert and stays on the confirm when the rotation fails", async () => {
    rotateResponse = () => Response.json({ status: 500, title: "boom" }, { status: 500 });
    await mount({ mode: "rotate", envId: "e1" });
    fireEvent.click(screen.getByRole("button", { name: "Generate new token" }));
    await waitFor(() =>
      expect(
        screen
          .getAllByRole("alert")
          .map((a) => a.textContent)
          .join("|"),
      ).toContain(
        "Couldn't generate a new token. Try again, and check the Bayplan logs if it keeps happening.",
      ),
    );
    expect(screen.getByRole("button", { name: "Generate new token" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Install the agent" })).toBeNull();
  });
});

describe("InstallDialog and the join token (D-20, T-02-53)", () => {
  function everywhere(): string {
    return [
      JSON.stringify({ ...localStorage }),
      JSON.stringify({ ...sessionStorage }),
      window.location.href,
      JSON.stringify(router.state.location),
      JSON.stringify(history.state),
      document.cookie,
    ].join("\n");
  }

  it("keeps the token out of storage, the URL and router state while the dialog is open", async () => {
    await mount({ mode: "add" });
    await createWith("edge");
    fireEvent.click(screen.getByRole("button", { name: "Show token" }));
    fireEvent.click(screen.getByRole("button", { name: "Copy command" }));
    await waitFor(() => expect(writeText).toHaveBeenCalled());
    expect(everywhere()).not.toContain(TOKEN);
  });

  it("drops the token from the page and the query cache when the dialog closes and starts empty on reopen", async () => {
    await mount({ mode: "add" });
    await createWith("edge");
    fireEvent.click(screen.getByRole("button", { name: "Show token" }));
    expect(dialog().textContent).toContain(TOKEN);

    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(document.body.textContent).not.toContain(TOKEN);
    expect(everywhere()).not.toContain(TOKEN);
    await waitFor(() => {
      const cached = JSON.stringify([
        client
          .getQueryCache()
          .getAll()
          .map((q) => q.state.data),
        client
          .getMutationCache()
          .getAll()
          .map((m) => m.state.data),
      ]);
      expect(cached).not.toContain(TOKEN);
    });

    fireEvent.click(screen.getByRole("button", { name: "reopen" }));
    await screen.findByRole("dialog");
    expect(screen.getByRole("heading", { name: "Add environment" })).toBeTruthy();
    expect(nameField().value).toBe("");
    expect(document.body.textContent).not.toContain(TOKEN);
    expect(screen.queryByRole("button", { name: "Hide token" })).toBeNull();
  });

  it("never marks the environment list stale with the token, only refreshes it", async () => {
    client.setQueryData(["environments"], { items: [] });
    await mount({ mode: "add" });
    await createWith("edge");
    expect(
      JSON.stringify(
        client
          .getQueryCache()
          .getAll()
          .map((q) => q.state.data),
      ),
    ).not.toContain(TOKEN);
    expect(client.getQueryState(["environments"])?.isInvalidated).toBe(true);
  });
});
