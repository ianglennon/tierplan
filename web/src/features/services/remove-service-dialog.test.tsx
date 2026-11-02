// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { ServiceItem } from "@/api/services";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import {
  RemoveServiceDialog,
  RemoveServiceMenuItem,
} from "@/features/services/remove-service-dialog";
import "@/i18n";
import { DropdownMenu, DropdownMenuContent } from "@/components/ui/dropdown-menu";
import { type Needs, useNeeds } from "@/hooks/use-needs";
import { makeMe } from "@/test-me";
import { settled } from "@/test-settle";

const ENV = "e1";

const web: ServiceItem = {
  id: "id-web",
  name: "web",
  mode: "replicated",
  running: 2,
  desired: 3,
  updateState: "",
  image: "localhost:5000/private/web:1",
  stack: "",
  tierplanStackId: "",
  agent: false,
  updatedAt: "2026-10-01T10:00:00Z",
  ports: [],
  updateMessage: "",
  hasPreviousSpec: true,
  createdAt: "2026-09-01T10:00:00Z",
};

const agent: ServiceItem = {
  ...web,
  id: "id-agent",
  name: "tierplan-agent",
  mode: "global",
  agent: true,
};

// What the server was sent, as "METHOD path?query".
let sent: string[];
let answer: () => Response;
// A test that wants the request held sets this and opens it when it is done looking.
let gate: Promise<void> | null = null;

beforeEach(() => {
  sent = [];
  answer = () => new Response(null, { status: 204 });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") {
        return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
      }
      if (req.method === "DELETE" && url.pathname.startsWith(`/api/env/${ENV}/services/`)) {
        sent.push(`${req.method} ${url.pathname}${url.search}`);
        if (gate) await gate;
        return answer();
      }
      if (url.pathname === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Swarm one", envType: "swarm" }] });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  // sonner keeps its toasts in module state across tests and replays the active ones to the next
  // Toaster that mounts, so a toast one test raised must not reach the page of the next.
  toast.dismiss();
  cleanup();
  gate = null;
  vi.unstubAllGlobals();
});

function problem(status: number, type: string, extra: Record<string, unknown> = {}) {
  return Response.json({ status, type, title: "x", ...extra }, { status });
}

function mount(target: ServiceItem | null = web) {
  const onClose = vi.fn();
  const onRemoved = vi.fn();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <RemoveServiceDialog envId={ENV} target={target} onClose={onClose} onRemoved={onRemoved} />
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
  return { onClose, onRemoved };
}

describe("RemoveServiceDialog", () => {
  it("asks Remove {name}? and says what stays, with Cancel focused and a destructive confirm", () => {
    mount();
    const dialog = screen.getByRole("alertdialog", { name: "Remove web?" });
    expect(
      within(dialog).getByText(
        "Swarm stops its 2 tasks and deletes the service. Its images, volumes, networks, secrets and configs stay.",
      ),
    ).toBeTruthy();
    expect(document.activeElement).toBe(within(dialog).getByRole("button", { name: "Cancel" }));
    const confirm = within(dialog).getByRole("button", { name: "Remove service" });
    expect(confirm.className).toContain("bg-destructive-fill");
    expect(dialog.className).toContain("sm:max-w-120");
    expect(within(dialog).queryByRole("textbox")).toBeNull();
  });

  it("says one task in the singular", () => {
    mount({ ...web, running: 1 });
    expect(screen.getByText(/^Swarm stops its 1 task and deletes the service/)).toBeTruthy();
  });

  it("removes the service once, says so and hands the page over", async () => {
    const { onClose, onRemoved } = mount();
    fireEvent.click(screen.getByRole("button", { name: "Remove service" }));
    await waitFor(() => expect(sent).toEqual(["DELETE /api/env/e1/services/id-web"]));
    expect(await screen.findByText("Removed web.")).toBeTruthy();
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(onRemoved).toHaveBeenCalledTimes(1);
  });

  it("does not send a second request on a second click, and cannot be dismissed meanwhile", async () => {
    let open: () => void = () => {};
    gate = new Promise<void>((resolve) => {
      open = resolve;
    });
    const { onClose } = mount();
    const confirm = screen.getByRole("button", { name: "Remove service" });
    fireEvent.click(confirm);
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(await screen.findByText("Removing…")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /Removing/ }));
    fireEvent.keyDown(screen.getByRole("alertdialog"), { key: "Escape" });
    expect(sent).toHaveLength(1);
    expect(onClose).not.toHaveBeenCalled();
    open();
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(sent).toHaveLength(1);
  });

  it("for the agent service warns, asks for its name and keeps the confirm aria-disabled until it matches", async () => {
    const { onRemoved } = mount(agent);
    const dialog = screen.getByRole("alertdialog", { name: "Remove tierplan-agent?" });
    expect(
      within(dialog).getByText(
        "This is TierPlan's agent service. Removing it cuts TierPlan off from every node it runs on.",
      ),
    ).toBeTruthy();
    const field = within(dialog).getByLabelText("Type tierplan-agent to confirm");
    const confirm = within(dialog).getByRole("button", { name: "Remove agent service" });
    expect(within(dialog).queryByRole("button", { name: "Remove service" })).toBeNull();
    expect(confirm.getAttribute("aria-disabled")).toBe("true");

    fireEvent.click(confirm);
    expect(sent).toHaveLength(0);

    fireEvent.change(field, { target: { value: "tierplan-agen" } });
    expect(confirm.getAttribute("aria-disabled")).toBe("true");
    fireEvent.change(field, { target: { value: "tierplan-agent" } });
    expect(confirm.getAttribute("aria-disabled")).not.toBe("true");

    fireEvent.click(confirm);
    await waitFor(() =>
      expect(sent).toEqual(["DELETE /api/env/e1/services/id-agent?confirm=tierplan-agent"]),
    );
    await waitFor(() => expect(onRemoved).toHaveBeenCalled());
  });

  it("keeps a refusal inside the dialog as an alert, in words", async () => {
    answer = () =>
      problem(409, "stack_owned", { errors: [{ location: "stack", message: "shop" }] });
    const { onClose, onRemoved } = mount();
    fireEvent.click(screen.getByRole("button", { name: "Remove service" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Managed by stack shop. Change it through the stack.");
    expect(onClose).not.toHaveBeenCalled();
    expect(onRemoved).not.toHaveBeenCalled();
    expect(screen.getByRole("alertdialog", { name: "Remove web?" })).toBeTruthy();
  });

  it("words a lost permission and a server that could not be reached", async () => {
    answer = () => problem(403, "forbidden");
    mount();
    fireEvent.click(screen.getByRole("button", { name: "Remove service" }));
    expect((await screen.findByRole("alert")).textContent).toContain(
      "You no longer have permission",
    );
    cleanup();

    answer = () => Response.error();
    mount();
    fireEvent.click(screen.getByRole("button", { name: "Remove service" }));
    expect((await screen.findByRole("alert")).textContent).toContain(
      "The connection dropped before TierPlan answered",
    );
  });

  it("says a service that is already gone in a toast and closes", async () => {
    answer = () => problem(404, "service_not_found");
    const { onClose, onRemoved } = mount();
    fireEvent.click(screen.getByRole("button", { name: "Remove service" }));
    expect(await screen.findByText("web is already gone.")).toBeTruthy();
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(onRemoved).toHaveBeenCalledTimes(1);
  });

  it("is closed when there is no target", () => {
    mount(null);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });
});

function MenuHarness({ stack }: { stack: string }) {
  const needs: Needs = useNeeds("services.manage", ENV);
  return (
    <DropdownMenu open>
      <DropdownMenuContent>
        <RemoveServiceMenuItem stack={stack} needs={needs} onSelect={onSelect} />
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

// The reason an unavailable item gives, as a screen reader gets it.
function reasonOf(item: HTMLElement): string {
  const id = item.getAttribute("aria-describedby");
  return (id && document.getElementById(id)?.textContent) || "";
}

const onSelect = vi.fn();
let menuClient: QueryClient;

function mountMenu(stack: string, role: "operator" | "admin") {
  onSelect.mockReset();
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: role } }));
      return Response.json({}, { status: 404 });
    }),
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  menuClient = client;
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <MenuHarness stack={stack} />
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

// The item as it is once /api/me has answered: what it is before that is not what a person sees.
async function itemOnceSignedIn(): Promise<HTMLElement> {
  await waitFor(() => expect(screen.getByRole("menuitem", { name: "Remove…" })).toBeTruthy());
  // /api/me answers asynchronously; the item changes with it. Nothing is fetching once it has.
  await settled(menuClient);
  return screen.getByRole("menuitem", { name: "Remove…" });
}

describe("RemoveServiceMenuItem", () => {
  it("is Remove… in destructive text and opens the dialog for an Admin on a service of its own", async () => {
    mountMenu("", "admin");
    await waitFor(() =>
      expect(
        screen.getByRole("menuitem", { name: "Remove…" }).getAttribute("aria-disabled"),
      ).not.toBe("true"),
    );
    const item = await itemOnceSignedIn();
    expect(item.getAttribute("data-variant")).toBe("destructive");
    expect(item.getAttribute("aria-disabled")).not.toBe("true");
    fireEvent.click(item);
    expect(onSelect).toHaveBeenCalledTimes(1);
  });

  it("is aria-disabled with Needs Admin for an Operator, and opens nothing", async () => {
    mountMenu("", "operator");
    await waitFor(() =>
      expect(reasonOf(screen.getByRole("menuitem", { name: "Remove…" }))).toBe("Needs Admin"),
    );
    const item = await itemOnceSignedIn();
    expect(item.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(item);
    expect(onSelect).not.toHaveBeenCalled();
  });

  it("is aria-disabled with the stack's reason for a service a stack owns, even for an Admin", async () => {
    mountMenu("shop", "admin");
    await waitFor(() =>
      expect(reasonOf(screen.getByRole("menuitem", { name: "Remove…" }))).toBe(
        "Managed by stack shop. Change it through the stack.",
      ),
    );
    const item = await itemOnceSignedIn();
    expect(item.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(item);
    expect(onSelect).not.toHaveBeenCalled();
  });
});
