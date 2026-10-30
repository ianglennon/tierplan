// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { Toaster } from "@/components/ui/sonner";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { makeMe } from "@/test-me";

const SOCKET = "/var/run/docker.sock";

const ADMIN = makeMe({ instanceAdmin: true });
const VIEWER = makeMe({ permissions: { global: [], environments: {} } });

// A user who may read environments but nothing in Settings.
// It holds a grant on every environment: a user with none sees the no-access
// state instead (D-08).
const READER = {
  ...makeMe({ allEnvironments: "viewer" }),
  permissions: { global: [], environments: { "*": ["environments.read"] } },
};

function environment(over: Record<string, unknown> = {}) {
  return {
    id: "e1",
    name: "lab-1",
    kind: "local",
    enabled: true,
    isSelfHost: true,
    createdAt: "2026-10-01T10:00:00Z",
    type: "standalone",
    engine: { name: "Docker Engine", version: "29.8.2", apiVersion: "1.56" },
    status: { state: "online" },
    ...over,
  };
}

function localHost(over: Record<string, unknown> = {}) {
  return {
    socketPath: SOCKET,
    detected: true,
    managed: false,
    probe: {
      probedAt: "2026-10-01T10:00:00Z",
      reachable: true,
      supported: true,
      engineName: "Docker Engine",
      engineVersion: "29.8.2",
      apiVersion: "1.56",
      hostname: "lab-1",
      type: "standalone",
    },
    ...over,
  };
}

interface Call {
  path: string;
  method: string;
  csrf: string | null;
  body?: Record<string, unknown>;
}

let me: typeof ADMIN;
let calls: Call[];
let environments: ReturnType<typeof environment>[];
let host: Record<string, unknown>;
let patchAnswer: (body: Record<string, unknown>) => Response | Promise<Response>;
let deleteAnswer: () => Response | Promise<Response>;
let users: Record<string, unknown>[];

function userRow(over: Record<string, unknown> = {}) {
  return {
    id: "u1",
    username: "alice",
    displayName: "",
    email: "",
    instanceAdmin: false,
    status: "active",
    teams: [],
    teamCount: 0,
    ...over,
  };
}

beforeEach(() => {
  calls = [];
  me = ADMIN;
  environments = [environment()];
  host = localHost({ managed: true });
  users = [userRow({ id: "u0", username: "admin", instanceAdmin: true })];
  setCsrfToken(null);
  patchAnswer = (body) => {
    const env = environments[0];
    if (env) env.name = String(body.name);
    return Response.json(env);
  };
  deleteAnswer = () => {
    environments = [];
    host = localHost();
    return new Response(null, { status: 204 });
  };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const call: Call = { path, method: req.method, csrf: req.headers.get("X-CSRF-Token") };
      if (req.method === "PATCH") call.body = await req.clone().json();
      calls.push(call);
      if (path === "/api/instance") {
        return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
      }
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/environments") return Response.json({ items: environments });
      if (path === "/api/local-host") return Response.json(host);
      if (path === "/api/users") return Response.json(users);
      if (path === "/api/env/e1" && req.method === "PATCH") return patchAnswer(call.body ?? {});
      if (path === "/api/environments/e1" && req.method === "DELETE") return deleteAnswer();
      if (path === "/api/version") {
        return Response.json({
          commit: "",
          display: "v0.0.0",
          sourceTreeUrl: "https://example.test/tree",
          sourceUrl: "https://example.test",
          version: "v0.0.0",
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

const writes = (method: string) => calls.filter((c) => c.method === method);

// The page's own content: the environment switcher in the sidebar also names
// each environment, so queries for a row's text look inside <main>.
const main = () => within(document.getElementById("main") as HTMLElement);

describe("Settings layout", () => {
  it("shows the title and the Environments tab as the selected, URL-routed tab", async () => {
    const router = renderAt("/settings/environments");
    expect(await screen.findByRole("heading", { level: 1, name: "Settings" })).toBeTruthy();
    const tab = screen.getByRole("tab", { name: "Environments" });
    expect(tab.getAttribute("aria-selected")).toBe("true");
    expect(tab.getAttribute("href")).toBe("/settings/environments");
    expect(router.state.location.pathname).toBe("/settings/environments");
    await waitFor(() => expect(document.title).toBe("Environments · Lab"));
  });

  it("opens the General tab from /settings", async () => {
    const router = renderAt("/settings");
    await screen.findByRole("tab", { name: "Environments" });
    await waitFor(() => expect(router.state.location.pathname).toBe("/settings/general"));
  });

  it("opens the Environments tab from /settings for an env Admin who holds no installation-wide permission (WR-19, D-07)", async () => {
    me = makeMe({ envs: { e1: "admin" } });
    const router = renderAt("/settings");
    await waitFor(() => expect(router.state.location.pathname).toBe("/settings/environments"));
    expect(await main().findByText("lab-1")).toBeTruthy();
    expect(screen.queryByText("You don't have permission to view this page")).toBeNull();
    // The one tab is Environments: no General, no About.
    expect(screen.getAllByRole("tab").map((tab) => tab.textContent)).toEqual(["Environments"]);
  });

  it("gives an env Admin only the environments they administer, with Rename but no Remove host", async () => {
    environments = [
      environment(),
      environment({ id: "e2", name: "edge", kind: "agent", isSelfHost: false }),
      environment({ id: "e3", name: "build", kind: "agent", isSelfHost: false }),
    ];
    me = makeMe({ envs: { e1: "admin", e2: "viewer", e3: "operator" } });
    renderAt("/settings/environments");
    await screen.findByRole("tab", { name: "Environments" });
    await main().findByText("lab-1");
    expect(main().queryByText("edge")).toBeNull();
    expect(main().queryByText("build")).toBeNull();
    expect(main().getByRole("button", { name: "Rename lab-1" })).toBeTruthy();
    expect(main().queryByRole("button", { name: /^Remove host/ })).toBeNull();
  });

  it("leaves opting the host in to instance admins: an env Admin is not offered Manage this host", async () => {
    environments = [environment({ id: "e2", name: "edge", kind: "agent", isSelfHost: false })];
    host = localHost({ managed: false });
    me = makeMe({ envs: { e2: "admin" } });
    renderAt("/settings/environments");
    await screen.findByRole("tab", { name: "Environments" });
    await main().findByText("edge");
    expect(main().queryByRole("button", { name: "Manage this host" })).toBeNull();
    cleanup();
    queryClient.clear();

    me = ADMIN;
    renderAt("/settings/environments");
    await screen.findByRole("tab", { name: "Environments" });
    expect(await main().findByRole("button", { name: "Manage this host" })).toBeTruthy();
  });

  it("shows an env Admin of two of three environments those two", async () => {
    environments = [
      environment({ id: "e1", name: "alpha", kind: "agent", isSelfHost: false }),
      environment({ id: "e2", name: "beta", kind: "agent", isSelfHost: false }),
      environment({ id: "e3", name: "gamma", kind: "agent", isSelfHost: false }),
    ];
    me = makeMe({ envs: { e1: "admin", e2: "admin", e3: "viewer" } });
    renderAt("/settings/environments");
    await screen.findByRole("tab", { name: "Environments" });
    await main().findByText("alpha");
    expect(main().getByText("beta")).toBeTruthy();
    expect(main().queryByText("gamma")).toBeNull();
  });

  it("sends a user with neither permission from /settings to the home page (WR-19)", async () => {
    me = READER;
    environments = [];
    const router = renderAt("/settings");
    await screen.findByText("No environments yet");
    expect(router.state.location.pathname).toBe("/environments");
  });

  it("hides the Environments tab, and sends /settings elsewhere, from a Viewer or Operator of every environment", async () => {
    for (const role of ["viewer", "operator"] as const) {
      me = makeMe({ allEnvironments: role });
      renderAt("/settings/environments");
      expect(await screen.findByText("You don't have permission to view this page")).toBeTruthy();
      expect(screen.queryByRole("tab", { name: "Environments" })).toBeNull();
      cleanup();
      queryClient.clear();
    }
  });

  it("hides the Environments tab from a user who may add environments but not manage them", async () => {
    me = {
      ...ADMIN,
      permissions: { global: ["environments.create", "settings.read"], environments: {} },
    };
    renderAt("/settings/general");
    await screen.findByRole("tab", { name: "General" });
    expect(screen.queryByRole("tab", { name: "Environments" })).toBeNull();
  });

  it("renders no permission without environments.configure on any environment, and asks the server for nothing", async () => {
    me = VIEWER;
    renderAt("/settings/environments");
    expect(await screen.findByText("You don't have permission to view this page")).toBeTruthy();
    expect(calls.some((c) => c.path === "/api/local-host")).toBe(false);
  });
});

describe("Backup tab", () => {
  it("shows the Backup tab to an instance admin and opens the page", async () => {
    const router = renderAt("/settings/backup");
    const tab = await screen.findByRole("tab", { name: "Backup" });
    expect(tab.getAttribute("aria-selected")).toBe("true");
    expect(tab.getAttribute("href")).toBe("/settings/backup");
    expect(router.state.location.pathname).toBe("/settings/backup");
    expect(await screen.findByRole("heading", { name: "Download a backup" })).toBeTruthy();
    await waitFor(() => expect(document.title).toBe("Backup · Lab"));
  });

  it("keeps the Backup tab from anyone without backup.manage and says so on a direct visit", async () => {
    me = {
      ...ADMIN,
      permissions: { global: ["settings.read"], environments: {} },
    };
    renderAt("/settings/general");
    await screen.findByRole("tab", { name: "General" });
    expect(screen.queryByRole("tab", { name: "Backup" })).toBeNull();
    cleanup();
    queryClient.clear();

    renderAt("/settings/backup");
    expect(await screen.findByText("You don't have permission to view this page")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Download a backup" })).toBeNull();
  });
});

describe("Access tab", () => {
  it("shows the Access tab to an instance admin between Environments and Backup", async () => {
    renderAt("/settings/general");
    await screen.findByRole("tab", { name: "Access" });
    expect(screen.getAllByRole("tab").map((tab) => tab.textContent)).toEqual([
      "General",
      "Environments",
      "Access",
      "Backup",
      "About",
    ]);
  });

  it("opens the Users sub-tab from /settings/access and lists the users from the API", async () => {
    users = [
      userRow({ id: "u0", username: "admin", instanceAdmin: true }),
      userRow({ id: "u1", username: "carol", status: "invite_pending" }),
    ];
    const router = renderAt("/settings/access");
    await waitFor(() => expect(router.state.location.pathname).toBe("/settings/access/users"));
    const tab = await screen.findByRole("tab", { name: "Access" });
    expect(tab.getAttribute("aria-selected")).toBe("true");
    expect(await main().findByText("carol")).toBeTruthy();
    expect(main().getByText("admin")).toBeTruthy();
    expect(main().getByText("Invite pending")).toBeTruthy();
    expect(main().getByText("Instance admin")).toBeTruthy();
    expect(main().getByRole("tab", { name: "Users" }).getAttribute("aria-selected")).toBe("true");
    await waitFor(() => expect(document.title).toBe("Users · Lab"));
  });

  it("keeps the Access tab from anyone without users.manage and asks the server for no users", async () => {
    me = {
      ...ADMIN,
      permissions: { global: ["settings.read"], environments: {} },
    };
    renderAt("/settings/general");
    await screen.findByRole("tab", { name: "General" });
    expect(screen.queryByRole("tab", { name: "Access" })).toBeNull();
    cleanup();
    queryClient.clear();

    renderAt("/settings/access/users");
    expect(await screen.findByText("You don't have permission to view this page")).toBeTruthy();
    expect(calls.some((c) => c.path === "/api/users")).toBe(false);
  });
});

describe("navigation", () => {
  it("adds Settings to the sidebar for an admin and leaves it out for a viewer", async () => {
    renderAt("/settings/environments");
    await screen.findByRole("tab", { name: "Environments" });
    const sidebar = document.querySelector("[data-slot=sidebar]") as HTMLElement;
    const link = await within(sidebar).findByRole("link", { name: "Settings" });
    expect(link.getAttribute("href")).toBe("/settings");
    cleanup();
    queryClient.clear();

    me = READER;
    environments = [];
    renderAt("/");
    await screen.findByText("No environments yet");
    const rail = document.querySelector("[data-slot=sidebar]") as HTMLElement;
    expect(within(rail).queryByRole("link", { name: "Settings" })).toBeNull();
  });

  it("points the switcher's Manage environments item at /settings/environments", async () => {
    renderAt("/settings/environments");
    const trigger = await screen.findByRole("button", { name: "Switch environment" });
    fireEvent.click(trigger);
    const item = await screen.findByRole("option", { name: "Manage environments" });
    expect(item.getAttribute("href")).toBe("/settings/environments");
  });
});

describe("Environments tab", () => {
  it("lists each environment with its type, health, engine version and own-instance badge", async () => {
    renderAt("/settings/environments");
    await screen.findByRole("tab", { name: "Environments" });
    const list = await main().findByRole("list");
    const row = within(list).getByRole("listitem");
    expect(within(row).getByText("lab-1")).toBeTruthy();
    expect(within(row).getByText("Standalone")).toBeTruthy();
    expect(within(row).getByText("Online")).toBeTruthy();
    expect(within(row).getByText("29.8.2")).toBeTruthy();
    expect(within(row).getByText("Coaming (this instance)")).toBeTruthy();
    expect(within(row).getByRole("button", { name: "Rename lab-1" })).toBeTruthy();
    expect(within(row).getByRole("button", { name: "Remove host lab-1" })).toBeTruthy();
    // A managed host has nothing to add.
    expect(screen.queryByRole("button", { name: "Manage this host" })).toBeNull();
  });

  it("leaves the own-instance badge off a host that is not Coaming's own", async () => {
    environments = [environment({ isSelfHost: false })];
    renderAt("/settings/environments");
    await screen.findByRole("tab", { name: "Environments" });
    await main().findByText("lab-1");
    expect(screen.queryByText("Coaming (this instance)")).toBeNull();
  });
});

describe("rename", () => {
  async function openRename() {
    renderAt("/settings/environments");
    fireEvent.click(await screen.findByRole("button", { name: "Rename lab-1" }));
    return await screen.findByRole("dialog", { name: "Rename environment" });
  }

  it("PATCHes the trimmed name with the CSRF header, refreshes the list and toasts", async () => {
    const dialog = await openRename();
    const field = within(dialog).getByLabelText("Name") as HTMLInputElement;
    expect(field.value).toBe("lab-1");
    fireEvent.change(field, { target: { value: "  production  " } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Rename environment" }));

    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    const patch = writes("PATCH");
    expect(patch).toHaveLength(1);
    expect(patch[0]?.path).toBe("/api/env/e1");
    expect(patch[0]?.body).toEqual({ name: "production" });
    expect(patch[0]?.csrf).toBe("csrf-1");
    expect(await main().findByText("production")).toBeTruthy();
    expect((await screen.findAllByText("Settings saved.")).length).toBeGreaterThan(0);
  });

  it("sends nothing when the name did not change", async () => {
    const dialog = await openRename();
    fireEvent.click(within(dialog).getByRole("button", { name: "Rename environment" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(writes("PATCH")).toHaveLength(0);
  });

  it("shows an inline field error for an empty or too long name and sends nothing", async () => {
    const dialog = await openRename();
    const field = within(dialog).getByLabelText("Name") as HTMLInputElement;

    fireEvent.change(field, { target: { value: "   " } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Rename environment" }));
    expect(await within(dialog).findByText("Enter a name.")).toBeTruthy();
    expect(field.getAttribute("aria-invalid")).toBe("true");
    expect(field.getAttribute("aria-describedby")).toContain("rename-environment-name-error");

    fireEvent.change(field, { target: { value: "x".repeat(65) } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Rename environment" }));
    expect(await within(dialog).findByText("Use 64 characters or fewer.")).toBeTruthy();
    expect(writes("PATCH")).toHaveLength(0);
  });

  it("shows the server's 422 invalid_name on the field", async () => {
    patchAnswer = () => Response.json({ status: 422, type: "invalid_name" }, { status: 422 });
    const dialog = await openRename();
    fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "bad" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Rename environment" }));
    expect(await within(dialog).findByText("Enter a name of 1 to 64 characters.")).toBeTruthy();
    expect(screen.getByRole("dialog", { name: "Rename environment" })).toBeTruthy();
  });

  it("shows other failures as a form-level alert and keeps the dialog open", async () => {
    patchAnswer = () => Response.json({ status: 500 }, { status: 500 });
    const dialog = await openRename();
    fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "prod" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Rename environment" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toBe("Something went wrong. Try again.");
  });

  it("shows the spinner, disables the field and both buttons, and ignores a second submit", async () => {
    let release: (r: Response) => void = () => {};
    patchAnswer = () => new Promise<Response>((resolve) => (release = resolve));
    const dialog = await openRename();
    const field = within(dialog).getByLabelText("Name") as HTMLInputElement;
    fireEvent.change(field, { target: { value: "prod" } });
    const submit = within(dialog).getByRole("button", { name: "Rename environment" });
    fireEvent.click(submit);

    const pending = await within(dialog).findByRole("button", { name: /Renaming/ });
    // The fieldset disables its controls, which :disabled sees and .disabled does not.
    expect(pending.matches(":disabled")).toBe(true);
    expect(field.matches(":disabled")).toBe(true);
    expect(within(dialog).getByRole("button", { name: "Cancel" }).matches(":disabled")).toBe(true);
    expect(pending.querySelector("[role=status], svg")).toBeTruthy();
    fireEvent.submit(dialog.querySelector("form") as HTMLFormElement);
    // Escape does not dismiss it either.
    fireEvent.keyDown(dialog, { key: "Escape" });
    expect(screen.getByRole("dialog", { name: "Rename environment" })).toBeTruthy();
    expect(writes("PATCH")).toHaveLength(1);

    await act(async () => release(Response.json(environment({ name: "prod" }))));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(writes("PATCH")).toHaveLength(1);
  });

  it("returns focus to the Rename button when it closes", async () => {
    const dialog = await openRename();
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() =>
      expect(document.activeElement).toBe(screen.getByRole("button", { name: "Rename lab-1" })),
    );
  });
});

describe("remove", () => {
  async function openRemove() {
    renderAt("/settings/environments");
    fireEvent.click(await screen.findByRole("button", { name: "Remove host lab-1" }));
    return await screen.findByRole("alertdialog");
  }

  it("asks 'Remove {name}?' with the UI-SPEC body and starts focus on Cancel", async () => {
    const dialog = await openRemove();
    expect(within(dialog).getByText("Remove lab-1?")).toBeTruthy();
    expect(
      within(dialog).getByText(
        "Coaming will stop managing this host. Containers, images, volumes and networks on it are not changed. You can add it again in Settings.",
      ),
    ).toBeTruthy();
    await waitFor(() =>
      expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" })),
    );
    const confirm = within(dialog).getByRole("button", { name: "Remove host" });
    expect(confirm.className).toContain("bg-destructive-fill");
  });

  it("does nothing on Cancel", async () => {
    const dialog = await openRemove();
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(writes("DELETE")).toHaveLength(0);
    expect(main().getByText("lab-1")).toBeTruthy();
  });

  it("DELETEs with the CSRF header and offers 'Manage this host' while the socket is there", async () => {
    const dialog = await openRemove();
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove host" }));

    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    const del = writes("DELETE");
    expect(del).toHaveLength(1);
    expect(del[0]?.path).toBe("/api/environments/e1");
    expect(del[0]?.csrf).toBe("csrf-1");

    expect(await screen.findByText("No environments yet")).toBeTruthy();
    expect(await screen.findByRole("button", { name: "Manage this host" })).toBeTruthy();
    // The same root-equivalence notice as the wizard.
    expect(screen.getByText(/root-equivalent control of that host/)).toBeTruthy();
    // Settings has nothing to skip.
    expect(screen.queryByRole("button", { name: "Skip for now" })).toBeNull();
  });

  it("shows the no-environments empty state with the docker run snippet when no socket is left", async () => {
    deleteAnswer = () => {
      environments = [];
      host = { socketPath: SOCKET, detected: false, managed: false };
      return new Response(null, { status: 204 });
    };
    const dialog = await openRemove();
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove host" }));
    expect(await screen.findByText("No environments yet")).toBeTruthy();
    expect(await screen.findByText("No Docker socket found")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Copy the docker run command" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Manage this host" })).toBeNull();
  });

  it("re-adds the host with 'Manage this host'", async () => {
    const dialog = await openRemove();
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove host" }));
    const add = await screen.findByRole("button", { name: "Manage this host" });
    environments = [environment()];
    host = localHost({ managed: true });
    fireEvent.click(add);
    await waitFor(() => expect(calls.some((c) => c.method === "POST")).toBe(true));
    expect(await screen.findByRole("button", { name: "Rename lab-1" })).toBeTruthy();
  });

  it("keeps the dialog open with the spinner while the request runs, then reports a failure", async () => {
    let release: (r: Response) => void = () => {};
    deleteAnswer = () => new Promise<Response>((resolve) => (release = resolve));
    const dialog = await openRemove();
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove host" }));

    const pending = await within(dialog).findByRole("button", { name: /Removing/ });
    expect((pending as HTMLButtonElement).disabled).toBe(true);
    expect(
      (within(dialog).getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled,
    ).toBe(true);
    fireEvent.keyDown(dialog, { key: "Escape" });
    expect(screen.getByRole("alertdialog")).toBeTruthy();
    expect(writes("DELETE")).toHaveLength(1);

    await act(async () => release(Response.json({ status: 500 }, { status: 500 })));
    expect(await screen.findByText("Something went wrong. Try again.")).toBeTruthy();
    // The environment is still there and the dialog can be used again.
    expect(screen.getByRole("alertdialog")).toBeTruthy();
    expect(
      (
        within(screen.getByRole("alertdialog")).getByRole("button", {
          name: "Remove host",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(false);
  });

  it("treats an environment that is already gone as removed", async () => {
    deleteAnswer = () => {
      environments = [];
      return Response.json({ status: 404 }, { status: 404 });
    };
    const dialog = await openRemove();
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove host" }));
    expect(await screen.findByText("No environments yet")).toBeTruthy();
  });
});
