// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { Toaster } from "@/components/ui/sonner";
import { resetBasePathForTests } from "@/lib/base-path";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { janitorDefaults } from "@/test-janitor";
import { makeMe } from "@/test-me";
import { updateCheckDefaults } from "@/test-update-checks";

// Settings > General and About (plan 26, D-03, D-04, D-60, D-61, PLAT-05).

// announceBase does what the server does when it renders index.html (plan
// 02-03): it puts the base path in a meta tag the SPA reads once.
function announceBase(base: string | null): void {
  for (const el of document.head.querySelectorAll('meta[name="tierplan-base"]')) el.remove();
  resetBasePathForTests();
  if (base === null) return;
  const meta = document.createElement("meta");
  meta.name = "tierplan-base";
  meta.content = base;
  document.head.append(meta);
}

// TierPlan serves HTTPS only (D-07), so these tests run on an https page: the saved public URL has
// to pass the form's own https rule, and the "address in use" logic compares against this origin.
// jsdom's own page is http://localhost:3000.
const ORIGIN = "https://tierplan.example.com";
const SHA = "0123456789abcdef0123456789abcdef01234567";

const ADMIN = makeMe({
  instanceAdmin: true,
  permissions: {
    global: ["settings.read", "settings.write"],
    environments: { "*": ["environments.configure"] },
  },
});
const READER = { ...ADMIN, permissions: { global: ["settings.read"], environments: {} } };
const VIEWER = { ...ADMIN, permissions: { global: [], environments: {} } };

interface Call {
  path: string;
  method: string;
  csrf: string | null;
  body?: Record<string, unknown>;
}

const NOTICES = [
  { name: "chi", version: "v5.3.2", licence: "MIT" },
  {
    name: "shadcn/ui components (vendored in web/src/components/ui)",
    version: "vendored",
    licence: "MIT",
  },
];

let me: typeof ADMIN;
let calls: Call[];
interface SettingsState {
  instanceName: string;
  publicUrl: string;
  publicUrlLocked: boolean;
  agentUrl: string;
  agentUrlLocked: boolean;
  agentEndpoint: string;
  agentEndpointRefused: boolean;
  enrolledAgents: number;
  offlineAgents: number;
}

// settingsState is what GET /api/settings answers, with the agent fields of plan
// 02-26 at "no agent URL and no agents enrolled" unless a test sets them.
function settingsState(over: Partial<SettingsState> = {}): SettingsState {
  return {
    instanceName: "Lab",
    publicUrl: ORIGIN,
    publicUrlLocked: false,
    agentUrl: "",
    agentUrlLocked: false,
    agentEndpoint: "",
    agentEndpointRefused: false,
    enrolledAgents: 0,
    offlineAgents: 0,
    ...over,
  };
}

let settings: SettingsState;
let patchAnswer: (body: Record<string, unknown>) => Response | Promise<Response>;
let noticesAnswer: () => Response;

function applyPatch(body: Record<string, unknown>) {
  if (typeof body.instanceName === "string") settings.instanceName = body.instanceName;
  if (typeof body.publicUrl === "string") settings.publicUrl = body.publicUrl;
  if (typeof body.agentUrl === "string") settings.agentUrl = body.agentUrl;
  return Response.json(settings);
}

beforeEach(() => {
  vi.stubGlobal("location", { ...window.location, protocol: "https:", origin: ORIGIN });
  calls = [];
  me = ADMIN;
  settings = settingsState();
  patchAnswer = applyPatch;
  noticesAnswer = () => Response.json(NOTICES);
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request | string) => {
      // The notices file is fetched by path, everything else through the typed client.
      const url = typeof req === "string" ? new URL(req, ORIGIN) : new URL(req.url);
      const method = typeof req === "string" ? "GET" : req.method;
      const call: Call = {
        path: url.pathname,
        method,
        csrf: typeof req === "string" ? null : req.headers.get("X-CSRF-Token"),
      };
      if (typeof req !== "string" && method === "PATCH") call.body = await req.clone().json();
      calls.push(call);
      const path = url.pathname;
      if (path === "/api/instance") {
        return Response.json({
          instanceName: settings.instanceName,
          needsSetup: false,
          rememberDays: 30,
        });
      }
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/environments") return Response.json({ items: [] });
      if (path === "/api/local-host") return Response.json({}, { status: 404 });
      if (path === "/api/settings") {
        return method === "PATCH" ? patchAnswer(call.body ?? {}) : Response.json(settings);
      }
      // The Container shells card under the form (plan 04-23) reads its own limits.
      if (path === "/api/settings/shells")
        return Response.json({ idleMinutes: 30, maxPerUser: 10 });
      // So does the Stack history card (plan 08-06).
      if (path === "/api/settings/stack-history")
        return Response.json({ kept: 20, min: 5, max: 200, pruned: 0 });
      // And the Janitor defaults card (plan 11-24). Left unanswered, its own load-failure alert can
      // render before the form's and be the alert a test finds.
      if (path === "/api/settings/janitor") return Response.json(janitorDefaults());
      // The same for the update check defaults card (plan 12-12).
      if (path === "/api/settings/update-checks") return Response.json(updateCheckDefaults());
      if (path === "/api/version") {
        return Response.json({
          commit: SHA,
          display: "v0.1.0 (0123456)",
          sourceTreeUrl: `https://example.test/tree/${SHA}`,
          sourceUrl: "https://example.test",
          version: "v0.1.0",
        });
      }
      if (path === "/third-party-notices.json" || path === "/tierplan/third-party-notices.json") {
        return noticesAnswer();
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  setCsrfToken(null);
  document.title = "";
});

function renderAt(path: string) {
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
      <Toaster />
    </QueryClientProvider>,
  );
  return router;
}

const patches = () => calls.filter((c) => c.method === "PATCH" && c.path === "/api/settings");
const main = () => within(document.getElementById("main") as HTMLElement);
const nameField = () => screen.getByLabelText("Instance name") as HTMLInputElement;
const urlField = () => screen.getByLabelText("Public URL") as HTMLInputElement;
const agentField = () => screen.getByLabelText("Agent URL") as HTMLInputElement;
// The instance form's own button: the Container shells and Stack history cards below it have one too.
const instanceForm = () => nameField().closest("form") as HTMLFormElement;
const saveButton = () =>
  within(instanceForm()).getByRole("button", { name: "Save settings" }) as HTMLButtonElement;

async function openGeneral() {
  renderAt("/settings/general");
  await screen.findByLabelText("Instance name");
  await waitFor(() => expect(nameField().value).toBe("Lab"));
}

describe("Settings tabs", () => {
  it("shows General, Environments and About in that order, with General selected", async () => {
    renderAt("/settings/general");
    await screen.findByRole("tab", { name: "About" });
    const tabs = screen.getAllByRole("tab").map((t) => t.textContent);
    expect(tabs).toEqual(["General", "Environments", "About"]);
    expect(screen.getByRole("tab", { name: "General" }).getAttribute("aria-selected")).toBe("true");
    expect(screen.getByRole("tab", { name: "About" }).getAttribute("href")).toBe("/settings/about");
  });

  it("hides the tabs a user cannot use", async () => {
    me = READER;
    renderAt("/settings/general");
    await screen.findByRole("tab", { name: "About" });
    expect(screen.getAllByRole("tab").map((t) => t.textContent)).toEqual(["General", "About"]);
    cleanup();
    queryClient.clear();

    me = VIEWER;
    calls = [];
    renderAt("/settings/general");
    expect(await screen.findByText("You don't have permission to view this page")).toBeTruthy();
    expect(screen.queryAllByRole("tab")).toHaveLength(0);
    expect(calls.some((c) => c.path === "/api/settings")).toBe(false);
  });
});

describe("General tab", () => {
  it("shows both fields with their helpers and the tab title, and waits for a change to save", async () => {
    await openGeneral();
    expect(urlField().value).toBe(ORIGIN);
    expect(
      screen.getByText("Shown in the header, the browser tab and the sign-in page."),
    ).toBeTruthy();
    expect(nameField().maxLength).toBe(64);
    expect(saveButton().disabled).toBe(true);
    await waitFor(() => expect(document.title).toBe("General · Lab"));

    fireEvent.change(nameField(), { target: { value: "Home lab" } });
    await waitFor(() => expect(saveButton().disabled).toBe(false));
    fireEvent.change(nameField(), { target: { value: "  Lab  " } });
    await waitFor(() => expect(saveButton().disabled).toBe(true));
  });

  it("sends only the changed field with the CSRF token, toasts, refreshes the instance and disables the button again", async () => {
    await openGeneral();
    fireEvent.change(nameField(), { target: { value: "  Home lab  " } });
    fireEvent.click(saveButton());

    expect((await screen.findAllByText("Settings saved.")).length).toBeGreaterThan(0);
    expect(patches()).toHaveLength(1);
    expect(patches()[0]?.body).toEqual({ instanceName: "Home lab" });
    expect(patches()[0]?.csrf).toBe("csrf-1");
    await waitFor(() => expect(document.title).toBe("General · Home lab"));
    await waitFor(() => expect(saveButton().disabled).toBe(true));
    expect(nameField().value).toBe("Home lab");
  });

  it("asks before moving to a different origin and sends nothing on Cancel", async () => {
    await openGeneral();
    fireEvent.change(urlField(), { target: { value: "https://tierplan.example" } });
    fireEvent.click(saveButton());

    const dialog = await screen.findByRole("alertdialog", { name: "Change the public URL?" });
    expect(within(dialog).getByText(new RegExp(`You're using TierPlan at ${ORIGIN}`))).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(patches()).toHaveLength(0);
    expect(urlField().value).toBe("https://tierplan.example");
  });

  it("names the origin plus the base path as the address in use under a base", async () => {
    announceBase("/tierplan");
    try {
      await openGeneral();
      fireEvent.change(urlField(), { target: { value: "https://tierplan.example" } });
      fireEvent.click(saveButton());
      const dialog = await screen.findByRole("alertdialog", { name: "Change the public URL?" });
      expect(
        within(dialog).getByText(`You're using TierPlan at ${ORIGIN}/tierplan.`, { exact: false }),
      ).toBeTruthy();
    } finally {
      announceBase(null);
    }
  });

  it("changes the public URL after the warning is confirmed", async () => {
    await openGeneral();
    fireEvent.change(urlField(), { target: { value: "https://tierplan.example" } });
    fireEvent.click(saveButton());
    const dialog = await screen.findByRole("alertdialog", { name: "Change the public URL?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Change public URL" }));

    expect((await screen.findAllByText("Settings saved.")).length).toBeGreaterThan(0);
    expect(patches()).toHaveLength(1);
    expect(patches()[0]?.body).toEqual({ publicUrl: "https://tierplan.example" });
  });

  it("does not ask when the new address is the one in use", async () => {
    settings.publicUrl = "https://old.example";
    await openGeneral();
    fireEvent.change(urlField(), { target: { value: `${ORIGIN}/tierplan` } });
    fireEvent.click(saveButton());
    expect((await screen.findAllByText("Settings saved.")).length).toBeGreaterThan(0);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(patches()[0]?.body).toEqual({ publicUrl: `${ORIGIN}/tierplan` });
  });

  it("does not ask when only the name changes, even if the saved URL is not this origin", async () => {
    settings.publicUrl = "https://elsewhere.example";
    await openGeneral();
    fireEvent.change(nameField(), { target: { value: "Renamed" } });
    fireEvent.click(saveButton());
    expect((await screen.findAllByText("Settings saved.")).length).toBeGreaterThan(0);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(patches()[0]?.body).toEqual({ instanceName: "Renamed" });
  });

  it("shows the URL read-only with a lock and the environment variable's name, and never sends it", async () => {
    settings = settingsState({ publicUrl: "https://fixed.example", publicUrlLocked: true });
    await openGeneral();
    expect(urlField().readOnly).toBe(true);
    expect(urlField().value).toBe("https://fixed.example");
    expect(screen.getByRole("img", { name: "Locked" })).toBeTruthy();
    expect(screen.getByText("Set by the TIERPLAN_PUBLIC_URL environment variable.")).toBeTruthy();

    fireEvent.change(nameField(), { target: { value: "Renamed" } });
    fireEvent.click(saveButton());
    expect((await screen.findAllByText("Settings saved.")).length).toBeGreaterThan(0);
    expect(patches()[0]?.body).toEqual({ instanceName: "Renamed" });
  });

  it("shows inline field errors for an empty name or a relative URL and sends nothing", async () => {
    await openGeneral();
    fireEvent.change(nameField(), { target: { value: "   " } });
    fireEvent.change(urlField(), { target: { value: "/tierplan" } });
    fireEvent.click(saveButton());
    expect(await screen.findByText("Enter a name.")).toBeTruthy();
    expect(
      screen.getByText("Enter a full https address such as https://tierplan.example.com."),
    ).toBeTruthy();
    expect(nameField().getAttribute("aria-invalid")).toBe("true");
    expect(patches()).toHaveLength(0);
  });

  it("maps the server's 422 field codes to inline errors", async () => {
    patchAnswer = () =>
      Response.json(
        {
          type: "validation_failed",
          status: 422,
          errors: [{ location: "body.instanceName", message: "instance_name_too_long" }],
        },
        { status: 422, headers: { "Content-Type": "application/problem+json" } },
      );
    await openGeneral();
    fireEvent.change(nameField(), { target: { value: "Other" } });
    fireEvent.click(saveButton());
    expect(await screen.findByText("Use 64 characters or fewer.")).toBeTruthy();
    expect(nameField().getAttribute("aria-invalid")).toBe("true");
  });

  it("shows a role=alert form error when the server fails", async () => {
    patchAnswer = () => Response.json({ status: 500 }, { status: 500 });
    await openGeneral();
    fireEvent.change(nameField(), { target: { value: "Other" } });
    fireEvent.click(saveButton());
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("Something went wrong. Try again.");
  });

  it("shows the lock as a field error when the server says the URL is now fixed", async () => {
    patchAnswer = () =>
      Response.json(
        { type: "public_url_locked", status: 409 },
        { status: 409, headers: { "Content-Type": "application/problem+json" } },
      );
    await openGeneral();
    fireEvent.change(urlField(), { target: { value: ORIGIN } });
    fireEvent.change(nameField(), { target: { value: "x" } });
    fireEvent.change(urlField(), { target: { value: `${ORIGIN}/x` } });
    fireEvent.click(saveButton());
    expect(
      await screen.findByText("Set by the TIERPLAN_PUBLIC_URL environment variable."),
    ).toBeTruthy();
  });

  it("shows the spinner and pending label, disables the fields and sends a double submit once", async () => {
    let release: (r: Response) => void = () => undefined;
    patchAnswer = () => new Promise<Response>((resolve) => (release = resolve));
    await openGeneral();
    fireEvent.change(nameField(), { target: { value: "Other" } });
    fireEvent.click(saveButton());

    const pending = await screen.findByRole("button", { name: "Saving…" });
    expect((pending as HTMLButtonElement).disabled).toBe(true);
    expect((nameField().closest("fieldset") as HTMLFieldSetElement).disabled).toBe(true);
    fireEvent.click(pending);
    fireEvent.submit(nameField().closest("form") as HTMLFormElement);
    await act(async () => {
      release(applyPatch({ instanceName: "Other" }));
    });
    expect((await screen.findAllByText("Settings saved.")).length).toBeGreaterThan(0);
    expect(patches()).toHaveLength(1);
  });

  it("is read-only without settings.write: no save button and nothing to submit", async () => {
    me = READER;
    await openGeneral();
    expect(nameField().readOnly).toBe(true);
    expect(urlField().readOnly).toBe(true);
    expect(within(instanceForm()).queryByRole("button", { name: "Save settings" })).toBeNull();
    fireEvent.submit(nameField().closest("form") as HTMLFormElement);
    expect(patches()).toHaveLength(0);
  });

  it("offers a retry when the settings cannot be loaded", async () => {
    const real = globalThis.fetch;
    let fail = true;
    vi.stubGlobal("fetch", (req: Request) =>
      fail && new URL(req.url).pathname === "/api/settings"
        ? Promise.resolve(Response.json({}, { status: 500 }))
        : real(req),
    );
    renderAt("/settings/general");
    // The query client retries once after a second.
    expect(await screen.findByText("Couldn't load the settings.")).toBeTruthy();
    fail = false;
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(nameField().value).toBe("Lab"));
  });
});

// The agent URL and the agents a URL change concerns (plan 02-26, D-18, D-19,
// UI-SPEC "Settings > General additions").
describe("General tab: agent URL", () => {
  const CHANGE_AGENT = "Change the agent URL?";
  const TOLD =
    "Connected agents are sent the new address now and switch to it on their next connection. From now on TierPlan refuses agents that dial a different host or port: there's no fallback period.";

  it("shows the Agent URL field, mono, with its helper and placeholder, empty when none is set", async () => {
    await openGeneral();
    expect(agentField().value).toBe("");
    expect(agentField().placeholder).toBe("https://tierplan.internal:7480");
    expect(agentField().className).toContain("font-mono");
    expect(
      screen.getByText(
        "Leave empty to use the public URL. Set it if your hosts reach TierPlan at a different address, such as an internal hostname. It must start with https://.",
      ),
    ).toBeTruthy();
    expect(agentField().readOnly).toBe(false);
    expect(agentField().getAttribute("aria-describedby")).toContain("settings-agent-url-helper");
  });

  it("shows the updated public URL helper that mentions a path", async () => {
    await openGeneral();
    expect(
      screen.getByText(
        "The address people use to reach TierPlan. It must start with https://. If TierPlan runs under a path, include it, for example https://example.com/tierplan. It controls cookies and request origin checks.",
      ),
    ).toBeTruthy();
  });

  it("keeps a long agent URL in the field to scroll sideways instead of wrapping it", async () => {
    const long = `https://${"a".repeat(80)}.internal.example.com:7480/${"path/".repeat(20)}`;
    settings = settingsState({ agentUrl: long });
    await openGeneral();
    expect(agentField().value).toBe(long);
    expect(agentField().tagName).toBe("INPUT");
    expect(agentField().className).not.toContain("truncate");
  });

  it("shows the URL read-only with a lock and the environment variable's name when it is locked, and never sends it", async () => {
    settings = settingsState({ agentUrl: "https://agent.internal:7480", agentUrlLocked: true });
    await openGeneral();
    expect(agentField().readOnly).toBe(true);
    expect(agentField().value).toBe("https://agent.internal:7480");
    expect(
      within(agentField().closest('[data-slot="input-group"]') as HTMLElement).getByRole("img", {
        name: "Locked",
      }),
    ).toBeTruthy();
    expect(screen.getByText("Set by the TIERPLAN_AGENT_URL environment variable.")).toBeTruthy();

    fireEvent.change(nameField(), { target: { value: "Renamed" } });
    fireEvent.click(saveButton());
    expect((await screen.findAllByText("Settings saved.")).length).toBeGreaterThan(0);
    expect(patches()[0]?.body).toEqual({ instanceName: "Renamed" });
  });

  it("shows the full-address error for an invalid agent URL and sends nothing", async () => {
    await openGeneral();
    fireEvent.change(agentField(), { target: { value: "tierplan.internal" } });
    fireEvent.click(saveButton());
    expect(
      await screen.findByText("Enter a full https address such as https://tierplan.example.com."),
    ).toBeTruthy();
    expect(agentField().getAttribute("aria-invalid")).toBe("true");
    expect(patches()).toHaveLength(0);
  });

  it("saves a blank agent URL as an empty string, which clears it", async () => {
    settings = settingsState({ agentUrl: "https://old.internal:7480" });
    await openGeneral();
    fireEvent.change(agentField(), { target: { value: "   " } });
    await waitFor(() => expect(saveButton().disabled).toBe(false));
    fireEvent.click(saveButton());
    expect((await screen.findAllByText("Settings saved.")).length).toBeGreaterThan(0);
    expect(patches()[0]?.body).toEqual({ agentUrl: "" });
    await waitFor(() => expect(agentField().value).toBe(""));
  });

  it("sends only the agent URL, trimmed, and asks nothing when no agent is enrolled", async () => {
    await openGeneral();
    fireEvent.change(agentField(), { target: { value: "  https://tierplan.lan:7480  " } });
    fireEvent.click(saveButton());
    expect((await screen.findAllByText("Settings saved.")).length).toBeGreaterThan(0);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(patches()[0]?.body).toEqual({ agentUrl: "https://tierplan.lan:7480" });
  });

  it("asks before changing the agent URL when agents are enrolled, says they are told and warns about the offline one", async () => {
    settings = settingsState({ enrolledAgents: 3, offlineAgents: 1 });
    await openGeneral();
    fireEvent.change(agentField(), { target: { value: "https://tierplan.lan:7480" } });
    fireEvent.click(saveButton());

    const dialog = await screen.findByRole("alertdialog", { name: CHANGE_AGENT });
    expect(within(dialog).getByText(TOLD)).toBeTruthy();
    expect(
      within(dialog).getByText(
        "1 agent is offline. It can't learn the new address, and TierPlan will refuse it until you reinstall it with the new address or change the URL back.",
      ),
    ).toBeTruthy();
    // The sign-out warning is about the address a browser uses, not the agents'.
    expect(within(dialog).queryByText(/You're using TierPlan at/)).toBeNull();
    expect(patches()).toHaveLength(0);

    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(patches()).toHaveLength(0);
    expect(agentField().value).toBe("https://tierplan.lan:7480");

    fireEvent.click(saveButton());
    const again = await screen.findByRole("alertdialog", { name: CHANGE_AGENT });
    fireEvent.click(within(again).getByRole("button", { name: "Change agent URL" }));
    expect((await screen.findAllByText("Settings saved.")).length).toBeGreaterThan(0);
    expect(patches()).toHaveLength(1);
    expect(patches()[0]?.body).toEqual({ agentUrl: "https://tierplan.lan:7480" });
  });

  it("counts the offline agents in the plural and shows no warning when none is offline", async () => {
    settings = settingsState({ enrolledAgents: 5, offlineAgents: 2 });
    await openGeneral();
    fireEvent.change(agentField(), { target: { value: "https://tierplan.lan:7480" } });
    fireEvent.click(saveButton());
    const dialog = await screen.findByRole("alertdialog", { name: CHANGE_AGENT });
    expect(
      within(dialog).getByText(
        "2 agents are offline. They can't learn the new address, and TierPlan will refuse them until you reinstall them with the new address or change the URL back.",
      ),
    ).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    cleanup();
    queryClient.clear();

    settings = settingsState({ enrolledAgents: 5, offlineAgents: 0 });
    await openGeneral();
    fireEvent.change(agentField(), { target: { value: "https://tierplan.lan:7480" } });
    fireEvent.click(saveButton());
    const calm = await screen.findByRole("alertdialog", { name: CHANGE_AGENT });
    expect(within(calm).getByText(TOLD)).toBeTruthy();
    expect(within(calm).queryByText(/can't learn the new address/)).toBeNull();
  });

  it("asks before changing the public URL when agents are enrolled, even to the address in use, with the same paragraphs", async () => {
    settings = settingsState({
      publicUrl: "https://old.example",
      enrolledAgents: 2,
      offlineAgents: 1,
    });
    await openGeneral();
    fireEvent.change(urlField(), { target: { value: `${ORIGIN}/tierplan` } });
    fireEvent.click(saveButton());

    const dialog = await screen.findByRole("alertdialog", { name: "Change the public URL?" });
    expect(within(dialog).getByText(new RegExp(`You're using TierPlan at ${ORIGIN}`))).toBeTruthy();
    expect(within(dialog).getByText(TOLD)).toBeTruthy();
    expect(
      within(dialog).getByText(
        "1 agent is offline. It can't learn the new address, and TierPlan will refuse it until you reinstall it with the new address or change the URL back.",
      ),
    ).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Change public URL" }));
    expect((await screen.findAllByText("Settings saved.")).length).toBeGreaterThan(0);
    expect(patches()[0]?.body).toEqual({ publicUrl: `${ORIGIN}/tierplan` });
  });

  it("uses the public URL title when both URLs change", async () => {
    settings = settingsState({ publicUrl: "https://old.example", enrolledAgents: 1 });
    await openGeneral();
    fireEvent.change(urlField(), { target: { value: "https://new.example" } });
    fireEvent.change(agentField(), { target: { value: "https://tierplan.lan:7480" } });
    fireEvent.click(saveButton());
    const dialog = await screen.findByRole("alertdialog", { name: "Change the public URL?" });
    expect(within(dialog).getByText(TOLD)).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Change public URL" }));
    expect((await screen.findAllByText("Settings saved.")).length).toBeGreaterThan(0);
    expect(patches()[0]?.body).toEqual({
      publicUrl: "https://new.example",
      agentUrl: "https://tierplan.lan:7480",
    });
  });

  it("does not claim agents are told when the public URL changes under an agent URL: they keep dialing the agent URL", async () => {
    settings = settingsState({
      publicUrl: "https://old.example",
      agentUrl: "https://tierplan.lan:7480",
      enrolledAgents: 2,
      offlineAgents: 1,
    });
    await openGeneral();
    // To the address in use: nothing to ask, nothing agents need to hear.
    fireEvent.change(urlField(), { target: { value: `${ORIGIN}/tierplan` } });
    fireEvent.click(saveButton());
    expect((await screen.findAllByText("Settings saved.")).length).toBeGreaterThan(0);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(patches()[0]?.body).toEqual({ publicUrl: `${ORIGIN}/tierplan` });
  });

  it("keeps the browser warning but not the agent paragraphs for a public URL change under an agent URL to another origin", async () => {
    settings = settingsState({
      publicUrl: ORIGIN,
      agentUrl: "https://tierplan.lan:7480",
      enrolledAgents: 2,
      offlineAgents: 1,
    });
    await openGeneral();
    fireEvent.change(urlField(), { target: { value: "https://elsewhere.example" } });
    fireEvent.click(saveButton());
    const dialog = await screen.findByRole("alertdialog", { name: "Change the public URL?" });
    expect(within(dialog).getByText(new RegExp(`You're using TierPlan at ${ORIGIN}`))).toBeTruthy();
    expect(within(dialog).queryByText(TOLD)).toBeNull();
    expect(within(dialog).queryByText(/can't learn the new address/)).toBeNull();
  });

  it("clearing the agent URL while agents are enrolled asks and says the public URL is where they go", async () => {
    settings = settingsState({ agentUrl: "https://old.internal:7480", enrolledAgents: 2 });
    await openGeneral();
    fireEvent.change(agentField(), { target: { value: "" } });
    fireEvent.click(saveButton());
    const dialog = await screen.findByRole("alertdialog", { name: CHANGE_AGENT });
    expect(within(dialog).getByText(TOLD)).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Change agent URL" }));
    expect((await screen.findAllByText("Settings saved.")).length).toBeGreaterThan(0);
    expect(patches()[0]?.body).toEqual({ agentUrl: "" });
  });

  it("warns under the Agent URL field when agents cannot use the address, naming it and the switch", async () => {
    settings = settingsState({
      agentEndpoint: "ws://10.0.0.5:7480/agent/connect",
      agentEndpointRefused: true,
    });
    await openGeneral();
    const note = await screen.findByText(/ws:\/\/10\.0\.0\.5:7480\/agent\/connect/);
    expect(note.textContent).toContain("TIERPLAN_AGENT_ALLOW_INSECURE");
    expect(note.closest('[role="alert"]')).not.toBeNull();
  });

  it("shows no refused-address warning when agents can use the address", async () => {
    settings = settingsState({ agentEndpoint: "wss://tierplan.test/agent/connect" });
    await openGeneral();
    expect(screen.queryByText(/TIERPLAN_AGENT_ALLOW_INSECURE/)).toBeNull();
    expect(screen.queryByText(/Agents can't connect to this TierPlan/)).toBeNull();
  });

  it("refuses an http public URL with the https-only sentence and sends nothing", async () => {
    await openGeneral();
    fireEvent.change(urlField(), { target: { value: "http://tierplan.example.com" } });
    fireEvent.click(saveButton());
    const err = await screen.findByText(
      "TierPlan serves HTTPS only. Use an address that starts with https://, such as https://tierplan.example.com.",
    );
    expect(urlField().getAttribute("aria-invalid")).toBe("true");
    expect(urlField().getAttribute("aria-describedby")).toContain(err.id);
    expect(err.closest('[data-slot="field-error"]')).toBeTruthy();
    expect(patches()).toHaveLength(0);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("refuses an http agent URL with the agent https-only sentence and sends nothing", async () => {
    await openGeneral();
    fireEvent.change(agentField(), { target: { value: "http://tierplan.internal:7480" } });
    fireEvent.click(saveButton());
    const err = await screen.findByText(
      "Agents connect over HTTPS only. Use an address that starts with https://, such as https://tierplan.internal:7480.",
    );
    expect(agentField().getAttribute("aria-invalid")).toBe("true");
    expect(agentField().getAttribute("aria-describedby")).toContain(err.id);
    expect(patches()).toHaveLength(0);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("requires a public URL but accepts an empty agent URL", async () => {
    settings = settingsState({ agentUrl: "https://old.internal:7480" });
    await openGeneral();
    fireEvent.change(urlField(), { target: { value: "" } });
    fireEvent.click(saveButton());
    expect(
      await screen.findByText("Enter a full https address such as https://tierplan.example.com."),
    ).toBeTruthy();
    expect(patches()).toHaveLength(0);

    fireEvent.change(urlField(), { target: { value: ORIGIN } });
    fireEvent.change(agentField(), { target: { value: "" } });
    fireEvent.click(saveButton());
    expect((await screen.findAllByText("Settings saved.")).length).toBeGreaterThan(0);
    expect(patches()[0]?.body).toEqual({ agentUrl: "" });
  });

  it("validates and sends only the agent URL when the public URL is locked by the environment", async () => {
    settings = settingsState({ publicUrl: "https://fixed.example", publicUrlLocked: true });
    await openGeneral();
    expect(urlField().readOnly).toBe(true);

    fireEvent.change(agentField(), { target: { value: "http://tierplan.internal:7480" } });
    fireEvent.click(saveButton());
    expect(await screen.findByText(/Agents connect over HTTPS only/)).toBeTruthy();
    expect(patches()).toHaveLength(0);

    fireEvent.change(agentField(), { target: { value: "https://tierplan.internal:7480" } });
    fireEvent.click(saveButton());
    expect((await screen.findAllByText("Settings saved.")).length).toBeGreaterThan(0);
    expect(patches()[0]?.body).toEqual({ agentUrl: "https://tierplan.internal:7480" });
    // The locked field keeps its lock and its helper.
    expect(screen.getByText("Set by the TIERPLAN_PUBLIC_URL environment variable.")).toBeTruthy();
  });

  it("maps the server's invalid_agent_url to the same inline error", async () => {
    patchAnswer = () =>
      Response.json(
        {
          type: "validation_failed",
          status: 422,
          errors: [{ location: "body.agentUrl", message: "invalid_agent_url" }],
        },
        { status: 422, headers: { "Content-Type": "application/problem+json" } },
      );
    await openGeneral();
    fireEvent.change(agentField(), { target: { value: "https://tierplan.lan:7480" } });
    fireEvent.click(saveButton());
    expect(
      await screen.findByText("Enter a full https address such as https://tierplan.example.com."),
    ).toBeTruthy();
    expect(agentField().getAttribute("aria-invalid")).toBe("true");
  });

  it("maps the server's insecure_agent_url to its own inline error", async () => {
    patchAnswer = () =>
      Response.json(
        {
          type: "validation_failed",
          status: 422,
          errors: [{ location: "body.agentUrl", message: "insecure_agent_url" }],
        },
        { status: 422, headers: { "Content-Type": "application/problem+json" } },
      );
    await openGeneral();
    // Plan 15 removes this branch with the API field; the client now refuses an http value itself,
    // so the stubbed server code is reached with an https one.
    fireEvent.change(agentField(), { target: { value: "https://10.0.0.5:7480" } });
    fireEvent.click(saveButton());
    expect(
      await screen.findByText(/Agents refuse a plain http address for another host/),
    ).toBeTruthy();
    expect(agentField().getAttribute("aria-invalid")).toBe("true");
    expect(
      screen.queryByText("Enter a full https address such as https://tierplan.example.com."),
    ).toBeNull();
  });

  it("shows the lock as a field error when the server says the agent URL is now fixed", async () => {
    patchAnswer = () =>
      Response.json(
        { type: "agent_url_locked", status: 409 },
        { status: 409, headers: { "Content-Type": "application/problem+json" } },
      );
    await openGeneral();
    fireEvent.change(agentField(), { target: { value: "https://tierplan.lan:7480" } });
    fireEvent.click(saveButton());
    expect(
      await screen.findByText("Set by the TIERPLAN_AGENT_URL environment variable."),
    ).toBeTruthy();
  });

  it("is read-only without settings.write: the agent URL too", async () => {
    me = READER;
    await openGeneral();
    expect(agentField().readOnly).toBe(true);
    expect(within(instanceForm()).queryByRole("button", { name: "Save settings" })).toBeNull();
  });
});

describe("About tab", () => {
  it("shows the version, the commit linked to its source tree, the licence and the source link", async () => {
    renderAt("/settings/about");
    expect(await screen.findByText("v0.1.0 (0123456)")).toBeTruthy();
    expect(screen.getByRole("tab", { name: "About" }).getAttribute("aria-selected")).toBe("true");
    await waitFor(() => expect(document.title).toBe("About · Lab"));

    const commit = main().getByRole("link", { name: /Commit 0123456789abcdef/ });
    expect(commit.textContent).toBe(SHA);
    expect(commit.getAttribute("href")).toBe(`https://example.test/tree/${SHA}`);
    expect(commit.getAttribute("target")).toBe("_blank");
    expect(commit.getAttribute("rel")).toBe("noopener noreferrer");

    expect(main().getByText("Version", { selector: "dt" })).toBeTruthy();
    expect(main().getByText("Licence", { selector: "dt" })).toBeTruthy();
    expect(main().getByText("AGPL-3.0-or-later")).toBeTruthy();
    const source = main().getByRole("link", { name: "Source code (opens in a new tab)" });
    expect(source.getAttribute("href")).toBe("https://example.test");
    expect(source.getAttribute("rel")).toBe("noopener noreferrer");
  });

  it("lists every third-party notice with package, version and licence in a scrollable region", async () => {
    renderAt("/settings/about");
    const region = await screen.findByRole("region", { name: "Third-party notices list" });
    expect(screen.getByRole("heading", { name: "Third-party notices" })).toBeTruthy();
    const rows = within(region).getAllByRole("row");
    expect(rows).toHaveLength(NOTICES.length + 1);
    const chi = within(region).getByText("chi").closest("tr") as HTMLElement;
    expect(within(chi).getByText("v5.3.2")).toBeTruthy();
    expect(within(chi).getByText("MIT")).toBeTruthy();
    expect(within(region).getByText(/shadcn\/ui components/)).toBeTruthy();
    expect(calls.some((c) => c.path === "/third-party-notices.json")).toBe(true);
  });

  it("reads the notices file under the base path the server announced (plan 02-32)", async () => {
    announceBase("/tierplan");
    try {
      // Behind a proxy the file is at /tierplan/third-party-notices.json; a request
      // to the root would reach whatever else the host serves there.
      renderAt("/settings/about");
      await screen.findByRole("region", { name: "Third-party notices list" });
      expect(calls.some((c) => c.path === "/tierplan/third-party-notices.json")).toBe(true);
      expect(calls.some((c) => c.path === "/third-party-notices.json")).toBe(false);
    } finally {
      announceBase(null);
    }
  });

  it("says so, and offers a retry, when the notices file is missing or is not a list", async () => {
    noticesAnswer = () => new Response("<!doctype html><html></html>", { status: 200 });
    renderAt("/settings/about");
    expect(await screen.findByText("Couldn't load the third-party notices.")).toBeTruthy();
    noticesAnswer = () => Response.json(NOTICES);
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("region", { name: "Third-party notices list" })).toBeTruthy();
  });

  it("shows no permission without settings.read", async () => {
    me = VIEWER;
    renderAt("/settings/about");
    expect(await screen.findByText("You don't have permission to view this page")).toBeTruthy();
    expect(calls.some((c) => c.path === "/third-party-notices.json")).toBe(false);
  });
});
