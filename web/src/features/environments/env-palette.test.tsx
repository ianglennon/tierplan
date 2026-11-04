// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

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
import type { SearchBody, SearchResult } from "@/api/search";
import { makeMe } from "@/test-me";
import { settled } from "@/test-settle";

const ADMIN = makeMe({ instanceAdmin: true });
let account: typeof ADMIN = ADMIN;

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

// The search the palette asks for. A test sets searchReply for what the server answers and reads
// searchCalls for what was asked.
function hit(extra: Partial<SearchResult> & Pick<SearchResult, "type" | "name">): SearchResult {
  return {
    driver: "",
    envId: "a",
    envName: "alpha",
    id: `${extra.type}-${extra.name}`,
    image: "",
    managed: false,
    node: "",
    nodeId: "",
    services: 0,
    size: 0,
    stack: "",
    subnet: "",
    ...extra,
  };
}

function searchBody(
  results: SearchResult[],
  extra: Partial<Omit<SearchBody, "counts">> & { counts?: Partial<SearchBody["counts"]> } = {},
): SearchBody {
  const counts = { all: 0, container: 0, image: 0, network: 0, service: 0, stack: 0 };
  for (const r of results) counts[r.type]++;
  const { counts: over, ...rest } = extra;
  Object.assign(counts, over);
  counts.all = counts.container + counts.image + counts.network + counts.service + counts.stack;
  return {
    counts,
    notSearched: null,
    results,
    searchedAt: "2026-11-04T10:00:00Z",
    truncated: false,
    ...rest,
  };
}

const searchCalls: URL[] = [];
let searchReply: (url: URL) => Response | Promise<Response> = () => Response.json(searchBody([]));

beforeEach(() => {
  searchCalls.length = 0;
  searchReply = () => Response.json(searchBody([]));
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
      if (path === "/api/me") return Response.json(account);
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
      if (path === "/api/search") {
        const url = new URL(req.url);
        searchCalls.push(url);
        return searchReply(url);
      }
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
  account = ADMIN;
  cleanup();
  // The tests that look at focus append fields to document.body and leave one focused. cleanup()
  // only removes what Testing Library rendered, so the next test would press Ctrl+K with focus
  // still in a text field and the shortcut would be ignored: a test-order dependency.
  document.body.replaceChildren();
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

const palette = () => screen.queryByRole("dialog", { name: "Search TierPlan" });

describe("EnvPalette (D-35)", () => {
  beforeEach(() => setUA(LINUX));

  it("opens on Ctrl+K from any signed-in page and keeps the browser's own binding away", async () => {
    await renderReady("/environments");
    expect(palette()).toBeNull();
    const notPrevented = press(document.body, { ctrlKey: true });
    expect(notPrevented).toBe(false);
    expect(await screen.findByRole("dialog", { name: "Search TierPlan" })).toBeTruthy();
  });

  it("has the hidden title, the placeholder, both groups and All environments", async () => {
    await renderReady("/env/a");
    press(document.body, { ctrlKey: true });
    const dialog = await screen.findByRole("dialog", { name: "Search TierPlan" });
    const input = within(dialog).getByRole("combobox", { name: "Search TierPlan" });
    expect(input.getAttribute("placeholder")).toBe(
      "Search stacks, services, containers, images, networks…",
    );
    expect(document.activeElement).toBe(input);
    expect(within(dialog).getByRole("group", { name: "Recent" })).toBeTruthy();
    expect(within(dialog).getByRole("group", { name: "Environments" })).toBeTruthy();
    expect(within(dialog).getByRole("option", { name: "All environments" })).toBeTruthy();
    // The palette is for switching: nothing about Settings in it.
    expect(within(dialog).queryByRole("option", { name: "Manage environments" })).toBeNull();
    // The title is for screen readers only.
    expect(
      within(dialog).getByRole("heading", { name: "Search TierPlan" }).closest(".sr-only"),
    ).toBeTruthy();
  });

  it("has no Manage environments item for an instance admin, an env Admin or a Viewer: it is for switching", async () => {
    for (const who of [
      ADMIN,
      makeMe({ envs: { a: "admin" } }),
      makeMe({ envs: { a: "viewer" } }),
    ]) {
      account = who;
      await renderReady("/env/a");
      press(document.body, { ctrlKey: true });
      const dialog = await screen.findByRole("dialog", { name: "Search TierPlan" });
      expect(within(dialog).getByRole("option", { name: "All environments" })).toBeTruthy();
      expect(within(dialog).queryByRole("option", { name: "Manage environments" })).toBeNull();
      cleanup();
      queryClient.clear();
    }
  });

  it("uses the command key on a Mac and leaves Ctrl alone there", async () => {
    setUA(MAC);
    await renderReady("/env/a");
    expect(press(document.body, { ctrlKey: true })).toBe(true);
    await waitFor(() => expect(palette()).toBeNull());
    expect(press(document.body, { metaKey: true })).toBe(false);
    expect(await screen.findByRole("dialog", { name: "Search TierPlan" })).toBeTruthy();
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
    // The shortcut handler is synchronous; settled() lets anything the key press could have
    // started finish before the absence is asserted.
    await settled(queryClient);
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
    // The shortcut handler is synchronous; settled() lets anything the key press could have
    // started finish before the absence is asserted.
    await settled(queryClient);
    expect(palette()).toBeNull();
  });

  it("still works while focus is on a button", async () => {
    await renderReady("/env/a");
    const toggle = screen.getByRole("button", { name: "Toggle sidebar" });
    toggle.focus();
    expect(press(toggle, { ctrlKey: true })).toBe(false);
    expect(await screen.findByRole("dialog", { name: "Search TierPlan" })).toBeTruthy();
  });

  it("ignores other keys and other modifier combinations", async () => {
    await renderReady("/env/a");
    expect(fireEvent.keyDown(document.body, { key: "k" })).toBe(true);
    expect(fireEvent.keyDown(document.body, { key: "j", ctrlKey: true })).toBe(true);
    expect(press(document.body, { ctrlKey: true, shiftKey: true })).toBe(true);
    expect(press(document.body, { ctrlKey: true, altKey: true })).toBe(true);
    // The shortcut handler is synchronous; settled() lets anything the key press could have
    // started finish before the absence is asserted.
    await settled(queryClient);
    expect(palette()).toBeNull();
  });

  it("goes to the same section of the chosen environment and closes", async () => {
    const router = await renderReady("/env/a/containers");
    press(document.body, { ctrlKey: true });
    const dialog = await screen.findByRole("dialog", { name: "Search TierPlan" });
    const group = within(dialog).getByRole("group", { name: "Environments" });
    fireEvent.click(within(group).getByRole("option", { name: /Beta/ }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/env/b/containers"));
    await waitFor(() => expect(palette()).toBeNull());
  });

  it("filters the environment list by one character, and chooses with Enter", async () => {
    const router = await renderReady("/env/a/images");
    press(document.body, { ctrlKey: true });
    const dialog = await screen.findByRole("dialog", { name: "Search TierPlan" });
    const input = within(dialog).getByRole("combobox");
    // One character is still the environment list: it filters by name and by type. "b" is in
    // Beta's name only.
    fireEvent.change(input, { target: { value: "b" } });
    await waitFor(() => expect(within(dialog).queryByRole("option", { name: /Zulu/ })).toBeNull());
    fireEvent.keyDown(input, { key: "Enter" });
    await waitFor(() => expect(router.state.location.pathname).toBe("/env/b/images"));
    await waitFor(() => expect(palette()).toBeNull());
  });

  it("says no environment matches one character and still offers All environments", async () => {
    await renderReady("/env/a");
    press(document.body, { ctrlKey: true });
    const dialog = await screen.findByRole("dialog", { name: "Search TierPlan" });
    fireEvent.change(within(dialog).getByRole("combobox"), { target: { value: "q" } });
    expect(await within(dialog).findByText("No environments match “q”.")).toBeTruthy();
    expect(within(dialog).getByRole("option", { name: "All environments" })).toBeTruthy();
  });

  it("closes on Escape and puts focus back where it was", async () => {
    await renderReady("/env/a");
    const toggle = screen.getByRole("button", { name: "Toggle sidebar" });
    toggle.focus();
    press(toggle, { ctrlKey: true });
    const dialog = await screen.findByRole("dialog", { name: "Search TierPlan" });
    fireEvent.keyDown(within(dialog).getByRole("combobox"), { key: "Escape" });
    await waitFor(() => expect(palette()).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(toggle));
  });

  it("closes when the shortcut is pressed again", async () => {
    await renderReady("/env/a");
    press(document.body, { ctrlKey: true });
    const dialog = await screen.findByRole("dialog", { name: "Search TierPlan" });
    press(within(dialog).getByRole("combobox"), { ctrlKey: true });
    await waitFor(() => expect(palette()).toBeNull());
  });

  it("starts empty each time it opens", async () => {
    await renderReady("/env/a");
    press(document.body, { ctrlKey: true });
    let dialog = await screen.findByRole("dialog", { name: "Search TierPlan" });
    fireEvent.change(within(dialog).getByRole("combobox"), { target: { value: "bet" } });
    fireEvent.keyDown(within(dialog).getByRole("combobox"), { key: "Escape" });
    await waitFor(() => expect(palette()).toBeNull());
    press(document.body, { ctrlKey: true });
    dialog = await screen.findByRole("dialog", { name: "Search TierPlan" });
    expect((within(dialog).getByRole("combobox") as HTMLInputElement).value).toBe("");
  });

  it("is full width with 44px items under 768px", async () => {
    await renderReady("/env/a");
    press(document.body, { ctrlKey: true });
    const dialog = await screen.findByRole("dialog", { name: "Search TierPlan" });
    expect(dialog.className).toContain("max-w-full");
    expect(dialog.className).toContain("md:max-w-[640px]");
    for (const option of within(dialog).getAllByRole("option")) {
      expect(option.className).toContain("max-md:min-h-11");
    }
  });
});

// openPalette opens the palette with Ctrl+K and returns the dialog and its input.
async function openPalette(path = "/env/a") {
  const router = await renderReady(path);
  press(document.body, { ctrlKey: true });
  const dialog = await screen.findByRole("dialog", { name: "Search TierPlan" });
  return { router, dialog, input: within(dialog).getByRole("combobox") };
}

const WEB_HITS = [
  hit({
    type: "stack",
    name: "web-stack",
    id: "st1",
    managed: true,
    services: 3,
    envId: "b",
    envName: "Beta",
  }),
  hit({ type: "service", name: "web-api", id: "svc1", image: "nginx:1.27", stack: "web-stack" }),
];

describe("EnvPalette search (D-18)", () => {
  beforeEach(() => setUA(LINUX));

  it("asks once for 'web' with limit palette, puts 'Search all results' first and opens a hit in its environment", async () => {
    searchReply = () => Response.json(searchBody(WEB_HITS));
    const { router, dialog, input } = await openPalette();
    // Typing fast is one request: the first two edits are replaced before the pause ends.
    fireEvent.change(input, { target: { value: "we" } });
    fireEvent.change(input, { target: { value: "web" } });
    const stacks = await within(dialog).findByRole("group", { name: "Stacks" });
    expect(within(stacks).getByRole("option", { name: /web-stack/ })).toBeTruthy();
    expect(searchCalls).toHaveLength(1);
    expect(searchCalls[0]?.searchParams.get("q")).toBe("web");
    expect(searchCalls[0]?.searchParams.get("limit")).toBe("palette");

    const options = within(dialog).getAllByRole("option");
    expect(options[0]?.textContent).toContain("Search all results for “web”");
    expect(options[0]?.getAttribute("aria-selected")).toBe("true");
    // Only the environment list is gone: no Recent, no All environments.
    expect(within(dialog).queryByRole("group", { name: "Recent" })).toBeNull();

    // The name is mono with the match at weight 600; the second line is muted and says where.
    const services = within(dialog).getByRole("group", { name: "Services" });
    const service = within(services).getByRole("option", { name: /web-api/ });
    const mark = service.querySelector("mark");
    expect(mark?.textContent).toBe("web");
    expect(mark?.className).toContain("font-semibold");
    expect(service.textContent).toContain("nginx:1.27 in web-stack");

    // Arrow to the service (the stack is first), and Enter opens it in its environment.
    fireEvent.keyDown(input, { key: "ArrowDown" });
    fireEvent.keyDown(input, { key: "ArrowDown" });
    fireEvent.keyDown(input, { key: "Enter" });
    await waitFor(() => expect(router.state.location.pathname).toBe("/env/a/services/svc1"));
    await waitFor(() => expect(palette()).toBeNull());
  });
});
