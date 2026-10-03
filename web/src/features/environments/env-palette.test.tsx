// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { EnvironmentItem } from "@/api/environments";
import { queryClient } from "@/api/query-client";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";

const ME = {
  user: { id: "u1", username: "admin", role: "admin" },
  csrfToken: "csrf-1",
  permissions: {
    global: ["settings.read"],
    environments: { "*": ["environments.read", "environments.manage"] },
  },
};

function env(id: string, name: string, state: EnvironmentItem["status"]["state"], extra = {}) {
  return {
    id,
    name,
    kind: "agent",
    enabled: true,
    isSelfHost: false,
    isLocalSocket: false,
    createdAt: "2026-10-01T10:00:00Z",
    engine: {},
    status: { state },
    envType: "swarm",
    agentVersion: null,
    agents: null,
    engineVersion: null,
    failingServices: null,
    lastSeen: null,
    nodes: null,
    updateAvailable: false,
    ...extra,
  };
}

const ALL = [
  env("z", "Zulu", "online"),
  env("a", "alpha", "online"),
  env("b", "Beta", "offline", { envType: "standalone", status: { state: "offline" } }),
];

beforeEach(() => {
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/instance") {
        return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
      }
      if (path === "/api/me") return Response.json(ME);
      if (path === "/api/version") {
        return Response.json({
          commit: "",
          display: "v0.1.0",
          sourceTreeUrl: "https://example.test/tree",
          sourceUrl: "https://example.test",
          version: "v0.1.0",
        });
      }
      if (path === "/api/environments") return Response.json({ items: ALL });
      if (path === "/api/me/recent-environments") return Response.json({ environmentIds: ["z"] });
      return Response.json({}, { status: 404 });
    }),
  );
});

const originalUA = navigator.userAgent;
function setUA(ua: string) {
  Object.defineProperty(navigator, "userAgent", { value: ua, configurable: true });
}
const LINUX = "Mozilla/5.0 (X11; Linux x86_64) Chrome/150.0";
const MAC = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  setUA(originalUA);
  document.title = "";
});

async function renderReady(path: string) {
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  const trigger = await screen.findByRole("button", { name: "Switch environment" });
  await waitFor(() => expect(within(trigger).getByText(/alpha|Zulu|Beta/)).toBeTruthy());
  return router;
}

// fireEvent answers false when a handler called preventDefault.
const press = (target: Element, init: KeyboardEventInit) =>
  fireEvent.keyDown(target, { key: "k", ...init });

const palette = () => screen.queryByRole("dialog", { name: "Switch environment" });

describe("EnvPalette (D-35)", () => {
  beforeEach(() => setUA(LINUX));

  it("opens on Ctrl+K from any signed-in page and keeps the browser's own binding away", async () => {
    await renderReady("/environments");
    expect(palette()).toBeNull();
    const notPrevented = press(document.body, { ctrlKey: true });
    expect(notPrevented).toBe(false);
    expect(await screen.findByRole("dialog", { name: "Switch environment" })).toBeTruthy();
  });

  it("has the hidden title, the placeholder, both groups and All environments", async () => {
    await renderReady("/env/a");
    press(document.body, { ctrlKey: true });
    const dialog = await screen.findByRole("dialog", { name: "Switch environment" });
    const input = within(dialog).getByRole("combobox", { name: "Jump to an environment…" });
    expect(input.getAttribute("placeholder")).toBe("Jump to an environment…");
    expect(document.activeElement).toBe(input);
    expect(within(dialog).getByRole("group", { name: "Recent" })).toBeTruthy();
    expect(within(dialog).getByRole("group", { name: "Environments" })).toBeTruthy();
    expect(within(dialog).getByRole("option", { name: "All environments" })).toBeTruthy();
    // The palette is for switching: nothing about Settings in it.
    expect(within(dialog).queryByRole("option", { name: "Manage environments" })).toBeNull();
    // The title is for screen readers only.
    expect(within(dialog).getByText("Switch environment").closest(".sr-only")).toBeTruthy();
  });

  it("uses the command key on a Mac and leaves Ctrl alone there", async () => {
    setUA(MAC);
    await renderReady("/env/a");
    expect(press(document.body, { ctrlKey: true })).toBe(true);
    await waitFor(() => expect(palette()).toBeNull());
    expect(press(document.body, { metaKey: true })).toBe(false);
    expect(await screen.findByRole("dialog", { name: "Switch environment" })).toBeTruthy();
  });

  it("ignores the shortcut while focus is in another text field", async () => {
    await renderReady("/env/a");
    const field = document.createElement("input");
    field.type = "text";
    document.body.append(field);
    field.focus();
    expect(press(field, { ctrlKey: true })).toBe(true);
    const area = document.createElement("textarea");
    document.body.append(area);
    area.focus();
    expect(press(area, { ctrlKey: true })).toBe(true);
    const editable = document.createElement("div");
    editable.setAttribute("contenteditable", "true");
    editable.tabIndex = 0;
    document.body.append(editable);
    editable.focus();
    expect(press(editable, { ctrlKey: true })).toBe(true);
    await new Promise((r) => setTimeout(r, 30));
    expect(palette()).toBeNull();
  });

  it("ignores the shortcut inside a code editor", async () => {
    await renderReady("/env/a");
    const editor = document.createElement("div");
    editor.setAttribute("data-code-editor", "");
    const inner = document.createElement("div");
    inner.tabIndex = 0;
    editor.append(inner);
    document.body.append(editor);
    inner.focus();
    expect(press(inner, { ctrlKey: true })).toBe(true);
    await new Promise((r) => setTimeout(r, 30));
    expect(palette()).toBeNull();
  });

  it("still works while focus is on a button", async () => {
    await renderReady("/env/a");
    const toggle = screen.getByRole("button", { name: "Toggle sidebar" });
    toggle.focus();
    expect(press(toggle, { ctrlKey: true })).toBe(false);
    expect(await screen.findByRole("dialog", { name: "Switch environment" })).toBeTruthy();
  });

  it("ignores other keys and other modifier combinations", async () => {
    await renderReady("/env/a");
    expect(fireEvent.keyDown(document.body, { key: "k" })).toBe(true);
    expect(fireEvent.keyDown(document.body, { key: "j", ctrlKey: true })).toBe(true);
    expect(press(document.body, { ctrlKey: true, shiftKey: true })).toBe(true);
    expect(press(document.body, { ctrlKey: true, altKey: true })).toBe(true);
    await new Promise((r) => setTimeout(r, 30));
    expect(palette()).toBeNull();
  });

  it("goes to the same section of the chosen environment and closes", async () => {
    const router = await renderReady("/env/a/containers");
    press(document.body, { ctrlKey: true });
    const dialog = await screen.findByRole("dialog", { name: "Switch environment" });
    const group = within(dialog).getByRole("group", { name: "Environments" });
    fireEvent.click(within(group).getByRole("option", { name: /Beta/ }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/env/b/containers"));
    await waitFor(() => expect(palette()).toBeNull());
  });

  it("filters by name and type, and chooses with Enter", async () => {
    const router = await renderReady("/env/a/images");
    press(document.body, { ctrlKey: true });
    const dialog = await screen.findByRole("dialog", { name: "Switch environment" });
    const input = within(dialog).getByRole("combobox");
    fireEvent.change(input, { target: { value: "standalone" } });
    await waitFor(() => expect(within(dialog).queryByRole("option", { name: /Zulu/ })).toBeNull());
    fireEvent.keyDown(input, { key: "Enter" });
    await waitFor(() => expect(router.state.location.pathname).toBe("/env/b/images"));
    await waitFor(() => expect(palette()).toBeNull());
  });

  it("says nothing matches and still offers All environments", async () => {
    await renderReady("/env/a");
    press(document.body, { ctrlKey: true });
    const dialog = await screen.findByRole("dialog", { name: "Switch environment" });
    fireEvent.change(within(dialog).getByRole("combobox"), { target: { value: "zzz" } });
    expect(await within(dialog).findByText("No environments match “zzz”.")).toBeTruthy();
    expect(within(dialog).getByRole("option", { name: "All environments" })).toBeTruthy();
  });

  it("closes on Escape and puts focus back where it was", async () => {
    await renderReady("/env/a");
    const toggle = screen.getByRole("button", { name: "Toggle sidebar" });
    toggle.focus();
    press(toggle, { ctrlKey: true });
    const dialog = await screen.findByRole("dialog", { name: "Switch environment" });
    fireEvent.keyDown(within(dialog).getByRole("combobox"), { key: "Escape" });
    await waitFor(() => expect(palette()).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(toggle));
  });

  it("closes when the shortcut is pressed again", async () => {
    await renderReady("/env/a");
    press(document.body, { ctrlKey: true });
    const dialog = await screen.findByRole("dialog", { name: "Switch environment" });
    press(within(dialog).getByRole("combobox"), { ctrlKey: true });
    await waitFor(() => expect(palette()).toBeNull());
  });

  it("starts empty each time it opens", async () => {
    await renderReady("/env/a");
    press(document.body, { ctrlKey: true });
    let dialog = await screen.findByRole("dialog", { name: "Switch environment" });
    fireEvent.change(within(dialog).getByRole("combobox"), { target: { value: "bet" } });
    fireEvent.keyDown(within(dialog).getByRole("combobox"), { key: "Escape" });
    await waitFor(() => expect(palette()).toBeNull());
    press(document.body, { ctrlKey: true });
    dialog = await screen.findByRole("dialog", { name: "Switch environment" });
    expect((within(dialog).getByRole("combobox") as HTMLInputElement).value).toBe("");
  });

  it("is full width with 44px items under 768px", async () => {
    await renderReady("/env/a");
    press(document.body, { ctrlKey: true });
    const dialog = await screen.findByRole("dialog", { name: "Switch environment" });
    expect(dialog.className).toContain("max-w-full");
    expect(dialog.className).toContain("md:max-w-lg");
    for (const option of within(dialog).getAllByRole("option")) {
      expect(option.className).toContain("max-md:min-h-11");
    }
  });
});
