// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, type Mock, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { components } from "@/api/schema";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { SecretList } from "@/features/secrets/secret-list";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The secrets list (plan 06-23, RES-08, UI-SPEC "Secrets and configs"): Name, Used by, Owner,
// Created and Updated, the search, the empty states and Delete…, which needs the typed name and is
// off while any service spec mentions the secret.

type SecretItem = components["schemas"]["SecretItem"];
type SecretDetail = components["schemas"]["SecretDetailBody"];

const ENV = "e1";

interface Sent {
  method: string;
  path: string;
  search: string;
}

let sent: Sent[];
let items: SecretItem[];
let details: Record<string, SecretDetail>;
let onSearch: Mock<(q: string | undefined) => void>;
let onCreating: Mock<(open: boolean) => void>;
let answerList: () => Response | Promise<Response>;
let answerDelete: (req: Sent) => Response;
let client: QueryClient;
let me: ReturnType<typeof makeMe>;

const problem = (status: number, type: string) =>
  Response.json({ type, title: "x", status, detail: "d" }, { status });

const secret = (name: string, extra: Partial<SecretItem> = {}): SecretItem => ({
  id: `sid-${name}`,
  name,
  labels: {},
  owner: "",
  ownerKind: "",
  coamingStackId: "",
  createdAt: "2026-10-01T10:00:00Z",
  updatedAt: "2026-10-02T10:00:00Z",
  usedBy: 0,
  ...extra,
});

const detailOf = (s: SecretItem, services: string[]): SecretDetail => ({
  ...s,
  usedByRows: services.map((service) => ({
    service,
    serviceId: `svc-${service}`,
    stack: "",
    file: "/run/secrets/x",
    uid: "0",
    gid: "0",
    mode: "0444",
  })),
});

beforeEach(() => {
  sent = [];
  onSearch = vi.fn<(q: string | undefined) => void>();
  onCreating = vi.fn<(open: boolean) => void>();
  me = makeMe({ envs: { [ENV]: "admin" } });
  items = [secret("api-key"), secret("db-password")];
  details = {};
  answerList = () =>
    Response.json({ items, total: items.length, unfilteredTotal: items.length, capped: false });
  answerDelete = () => new Response(null, { status: 204 });
  client = new QueryClient({ defaultOptions: { queries: { retry: false, retryDelay: 1 } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const entry: Sent = { method: req.method, path: url.pathname, search: url.search };
      sent.push(entry);
      if (entry.path === "/api/me") return Response.json(me);
      if (entry.path === `/api/env/${ENV}/secrets` && req.method === "GET") return answerList();
      if (entry.path.startsWith(`/api/env/${ENV}/secrets/`) && req.method === "GET") {
        const id = entry.path.split("/").at(-1) ?? "";
        const found = details[id];
        return found ? Response.json(found) : problem(404, "secret_not_found");
      }
      if (entry.path.startsWith(`/api/env/${ENV}/secrets/`) && req.method === "DELETE") {
        const res = answerDelete(entry);
        if (res.ok) items = items.filter((s) => `/api/env/${ENV}/secrets/${s.id}` !== entry.path);
        return res;
      }
      if (entry.path === `/api/env/${ENV}/secrets` && req.method === "POST") {
        return Response.json({ id: "sid-new", name: "new-secret" }, { status: 201 });
      }
      if (entry.path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Swarm", envType: "swarm" }] });
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

const deletes = () => sent.filter((s) => s.method === "DELETE");

// The route owns whether the sheet is open (new=1 in the address); this holds it as the route
// would.
function Host({ q, startOpen }: { q?: string; startOpen: boolean }) {
  const [creating, setCreating] = useState(startOpen);
  return (
    <SecretList
      envId={ENV}
      q={q}
      onSearchChange={onSearch}
      creating={creating}
      onCreatingChange={(open) => {
        onCreating(open);
        setCreating(open);
      }}
    />
  );
}

function mount(props: { q?: string; creating?: boolean } = {}) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <Host q={props.q} startOpen={props.creating ?? false} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const deleteButton = (name: string) =>
  screen.findByRole("button", { name: `Delete secret ${name}` });

describe("SecretList columns", () => {
  it("shows the title, the count, and a row per secret in the order the server sent them", async () => {
    mount();
    expect(await screen.findByRole("heading", { level: 1, name: "Secrets" })).toBeTruthy();
    await screen.findByText("api-key");
    expect(screen.getByText("2 secrets")).toBeTruthy();
    const names = screen
      .getAllByRole("row")
      .slice(1)
      .map((r) => r.querySelector("td")?.textContent);
    expect(names).toEqual(["api-key", "db-password"]);
  });

  it("links the name and an Open item to the secret's page", async () => {
    mount();
    const name = await screen.findByRole("link", { name: "api-key" });
    expect(name.getAttribute("href")).toBe(`/env/${ENV}/secrets/sid-api-key`);
    const open = screen.getByRole("link", { name: "Open secret api-key" });
    expect(open.getAttribute("href")).toBe(`/env/${ENV}/secrets/sid-api-key`);
  });

  it("says Not in use, Used by n services and Used by 1 service, and Stack {name} or a dash for the owner", async () => {
    items = [
      secret("lonely"),
      secret("shared", { usedBy: 3, owner: "shop", ownerKind: "stack" }),
      secret("single", { usedBy: 1 }),
    ];
    mount();
    const lonely = (await screen.findByText("lonely")).closest("tr") as HTMLElement;
    expect(within(lonely).getByText("Not in use")).toBeTruthy();
    expect(within(lonely).getAllByText("—").length).toBeGreaterThanOrEqual(1);
    const shared = screen.getByText("shared").closest("tr") as HTMLElement;
    expect(within(shared).getByRole("button", { name: "Used by 3 services" })).toBeTruthy();
    expect(within(shared).getByText("Stack shop")).toBeTruthy();
    const single = screen.getByText("single").closest("tr") as HTMLElement;
    expect(within(single).getByRole("button", { name: "Used by 1 service" })).toBeTruthy();
  });

  it("links the owner's name to the Coaming stack, and leaves a foreign stack as text", async () => {
    items = [
      secret("own", { owner: "shop", ownerKind: "stack", coamingStackId: "stk-shop" }),
      secret("old", { owner: "legacy", ownerKind: "stack" }),
    ];
    mount();
    const own = (await screen.findByText("own")).closest("tr") as HTMLElement;
    const link = within(own).getByRole("link", { name: "shop" });
    expect(link.getAttribute("href")).toBe(`/env/${ENV}/stacks/stk-shop`);
    expect(own.textContent).toContain("Stack shop");
    const old = screen.getByText("old").closest("tr") as HTMLElement;
    expect(within(old).getByText("Stack legacy")).toBeTruthy();
    expect(within(old).queryByRole("link", { name: "legacy" })).toBeNull();
  });

  it("opens the services that use a secret from Used by, each a link to the service", async () => {
    items = [secret("shared", { usedBy: 2 })];
    details["sid-shared"] = detailOf(items[0] as SecretItem, ["web", "worker"]);
    mount();
    fireEvent.click(await screen.findByRole("button", { name: "Used by 2 services" }));
    const web = await screen.findByRole("link", { name: "web" });
    expect(web.getAttribute("href")).toBe(`/env/${ENV}/services/svc-web`);
    expect(screen.getByRole("link", { name: "worker" })).toBeTruthy();
  });
});

describe("SecretList states", () => {
  it("shows eight skeleton rows while the list loads", async () => {
    answerList = () => new Promise<Response>(() => {});
    mount();
    expect(await screen.findByRole("searchbox", { name: "Search secrets" })).toBeTruthy();
    // The header row and eight placeholders.
    expect(screen.getAllByRole("row")).toHaveLength(9);
    expect(screen.getByRole("table").getAttribute("aria-busy")).toBe("true");
  });

  it("says Couldn't load secrets with Retry loading secrets, which reads the list again", async () => {
    answerList = () => problem(503, "no_manager_connected");
    mount();
    await screen.findByText("Couldn't load secrets.");
    const before = sent.filter((s) => s.path.endsWith("/secrets")).length;
    fireEvent.click(screen.getByRole("button", { name: "Retry loading secrets" }));
    await waitFor(() =>
      expect(sent.filter((s) => s.path.endsWith("/secrets")).length).toBeGreaterThan(before),
    );
  });

  it("shows the standalone-host page for a host that is not a swarm", async () => {
    answerList = () => problem(409, "not_a_swarm");
    mount();
    await screen.findByText("This isn't a swarm");
  });

  it("says No secrets, with what a secret is, for someone who may create one, and 'Nothing to show' for a Viewer", async () => {
    items = [];
    mount();
    await screen.findByText("No secrets");
    cleanup();
    me = makeMe({ envs: { [ENV]: "viewer" } });
    mount();
    await screen.findByText("No secrets");
    await waitFor(() =>
      expect(document.body.textContent).toContain("Nothing to show on Swarm right now."),
    );
  });

  it("says nothing matches the search with Clear search", async () => {
    items = [];
    mount({ q: "zzz" });
    await screen.findByText("No secrets match “zzz”.");
    fireEvent.click(screen.getAllByRole("button", { name: "Clear search" }).at(-1) as HTMLElement);
    expect(onSearch).toHaveBeenCalledWith(undefined);
  });

  it("shows 'n of total secrets' while a search narrows the list and the cap sentence when capped", async () => {
    answerList = () => Response.json({ items, total: 2, unfilteredTotal: 9, capped: true });
    mount({ q: "a" });
    await screen.findByText("api-key");
    expect(screen.getByText("2 of 9 secrets")).toBeTruthy();
    expect(
      screen.getByText("Showing the first 500 of 2. Narrow the search to see the rest."),
    ).toBeTruthy();
  });
});

describe("SecretList search", () => {
  it("has the labelled box, sends the search to the server and tells the owner after the pause", async () => {
    mount({ q: "db" });
    const box = await screen.findByLabelText("Search secrets");
    expect(box.getAttribute("placeholder")).toBe("Search by name or label");
    expect((box as HTMLInputElement).value).toBe("db");
    await waitFor(() =>
      expect(sent.some((s) => s.method === "GET" && s.search === "?q=db")).toBe(true),
    );
    fireEvent.change(box, { target: { value: "api" } });
    expect(onSearch).not.toHaveBeenCalled();
    await waitFor(() => expect(onSearch).toHaveBeenCalledWith("api"));
  });
});

describe("SecretList delete", () => {
  async function typeName(dialog: HTMLElement, name: string) {
    fireEvent.change(within(dialog).getByLabelText(`Type ${name} to confirm`), {
      target: { value: name },
    });
  }

  it("asks 'Delete {name}?', needs the typed name, sends one DELETE with it and toasts", async () => {
    mount();
    fireEvent.click(await deleteButton("api-key"));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Delete api-key?")).toBeTruthy();
    expect(
      within(dialog).getByText("This deletes the secret and its value. Nothing can read it back."),
    ).toBeTruthy();
    const confirm = within(dialog).getByRole("button", { name: "Delete secret" });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(confirm);
    expect(deletes()).toHaveLength(0);
    await typeName(dialog, "api-key");
    await waitFor(() => expect(confirm.getAttribute("aria-disabled")).not.toBe("true"));
    fireEvent.click(confirm);
    await waitFor(() => expect(deletes()).toHaveLength(1));
    expect(deletes()[0]).toMatchObject({
      path: `/api/env/${ENV}/secrets/sid-api-key`,
      search: "?confirm=api-key",
    });
    await waitFor(() => expect(document.body.textContent).toContain("Deleted secret api-key."));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    await waitFor(() => expect(screen.queryByText("api-key")).toBeNull());
  });

  it("cancels without sending anything", async () => {
    mount();
    fireEvent.click(await deleteButton("api-key"));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(deletes()).toHaveLength(0);
  });

  it("disables Delete on a secret in use with its reason, and a click opens nothing", async () => {
    items = [secret("api-key", { usedBy: 2 }), secret("one", { usedBy: 1 })];
    mount();
    const button = await deleteButton("api-key");
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    expect(document.body.textContent).toContain(
      "In use by 2 services. Open it to see what uses it.",
    );
    fireEvent.click(button);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    const one = await deleteButton("one");
    await waitFor(() => expect(one.getAttribute("aria-disabled")).toBe("true"));
    expect(document.body.textContent).toContain(
      "In use by 1 service. Open it to see what uses it.",
    );
    expect(deletes()).toHaveLength(0);
  });

  it("shows an Operator and a Viewer Delete disabled with Needs Admin", async () => {
    for (const role of ["operator", "viewer"] as const) {
      cleanup();
      me = makeMe({ envs: { [ENV]: role } });
      mount();
      const button = await deleteButton("api-key");
      await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
      expect(document.body.textContent).toContain("Needs Admin");
      fireEvent.click(button);
      expect(screen.queryByRole("alertdialog")).toBeNull();
    }
    expect(deletes()).toHaveLength(0);
  });

  it("keeps the dialog open with the way forward when the server says it is still in use", async () => {
    answerDelete = () => problem(409, "object_in_use");
    mount();
    fireEvent.click(await deleteButton("api-key"));
    const dialog = await screen.findByRole("alertdialog");
    await typeName(dialog, "api-key");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete secret" }));
    await within(dialog).findByText(
      "api-key is still in use. Remove it from the services that use it, then try again.",
    );
    expect(screen.getByRole("alertdialog")).toBeTruthy();
  });

  it("says a secret that is already gone is gone, closes the dialog and refreshes the list", async () => {
    answerDelete = () => problem(404, "secret_not_found");
    mount();
    fireEvent.click(await deleteButton("api-key"));
    const dialog = await screen.findByRole("alertdialog");
    await typeName(dialog, "api-key");
    const before = sent.filter((s) => s.method === "GET" && s.path.endsWith("/secrets")).length;
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete secret" }));
    await waitFor(() => expect(document.body.textContent).toContain("api-key is already gone."));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    await waitFor(() =>
      expect(
        sent.filter((s) => s.method === "GET" && s.path.endsWith("/secrets")).length,
      ).toBeGreaterThan(before),
    );
  });

  it("shows a refusal of the permission in the dialog", async () => {
    answerDelete = () => problem(403, "forbidden");
    mount();
    fireEvent.click(await deleteButton("api-key"));
    const dialog = await screen.findByRole("alertdialog");
    await typeName(dialog, "api-key");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete secret" }));
    await within(dialog).findByText(
      "You no longer have permission to do that. Your access may have changed.",
    );
  });
});

describe("SecretList create", () => {
  it("has a Create secret button that asks the route to open the sheet", async () => {
    mount();
    await screen.findByText("api-key");
    fireEvent.click(screen.getByRole("button", { name: "Create secret" }));
    expect(onCreating).toHaveBeenCalledWith(true);
    expect(await screen.findByRole("dialog")).toBeTruthy();
    expect(screen.getByText("Create a secret")).toBeTruthy();
  });

  it("opens the sheet when the address says new=1", async () => {
    mount({ creating: true });
    expect(await screen.findByText("Create a secret")).toBeTruthy();
  });

  it("tells the route when the sheet closes", async () => {
    mount({ creating: true });
    await screen.findByText("Create a secret");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(onCreating).toHaveBeenCalledWith(false));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("shows an Operator and a Viewer Create secret disabled with Needs Admin, and never the sheet", async () => {
    for (const role of ["operator", "viewer"] as const) {
      cleanup();
      me = makeMe({ envs: { [ENV]: role } });
      mount({ creating: true });
      const button = await screen.findByRole("button", { name: "Create secret" });
      await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
      expect(document.body.textContent).toContain("Needs Admin");
      fireEvent.click(button);
      expect(screen.queryByRole("dialog")).toBeNull();
    }
  });

  it("in an empty list says what a secret is and offers Create secret to someone who may create one", async () => {
    items = [];
    mount();
    await screen.findByText("No secrets");
    expect(
      screen.getByText(
        "Secrets hold passwords, keys and certificates that services read at run time. Docker never shows a secret's value again after you create it.",
      ),
    ).toBeTruthy();
    // The header button and the empty state's.
    expect(screen.getAllByRole("button", { name: "Create secret" })).toHaveLength(2);
  });

  it("adds the new secret to the list after it is created", async () => {
    mount({ creating: true });
    await screen.findByText("Create a secret");
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "new-secret" } });
    fireEvent.change(screen.getByLabelText("Value"), { target: { value: "v" } });
    const before = sent.filter((s) => s.method === "GET" && s.path.endsWith("/secrets")).length;
    fireEvent.click(
      within(screen.getByRole("dialog")).getByRole("button", { name: "Create secret" }),
    );
    await waitFor(() => expect(sent.some((s) => s.method === "POST")).toBe(true));
    await waitFor(() =>
      expect(
        sent.filter((s) => s.method === "GET" && s.path.endsWith("/secrets")).length,
      ).toBeGreaterThan(before),
    );
  });
});

describe("SecretList New version…", () => {
  it("has a New version… item on every row that opens the two-step rotate dialog", async () => {
    mount();
    const button = await screen.findByRole("button", { name: "New version of api-key" });
    expect(button.textContent).toBe("New version…");
    fireEvent.click(button);
    expect(await screen.findByRole("heading", { name: "New version of api-key" })).toBeTruthy();
    expect(screen.getByText("Step 1 of 2")).toBeTruthy();
  });

  it("shows an Operator and a Viewer New version… disabled with Needs Admin, and never the dialog", async () => {
    for (const role of ["operator", "viewer"] as const) {
      cleanup();
      me = makeMe({ envs: { [ENV]: role } });
      mount();
      const button = await screen.findByRole("button", { name: "New version of api-key" });
      await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
      expect(document.body.textContent).toContain("Needs Admin");
      fireEvent.click(button);
      expect(screen.queryByRole("dialog")).toBeNull();
    }
  });
});
