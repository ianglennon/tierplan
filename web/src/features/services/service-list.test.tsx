// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { components } from "@/api/schema";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ServiceList } from "@/features/services/service-list";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

type ServiceItem = components["schemas"]["ServiceItem"];

const svc = (extra: Partial<ServiceItem> = {}): ServiceItem => ({
  id: "id-web",
  name: "web",
  mode: "replicated",
  running: 2,
  desired: 2,
  updateState: "",
  image: `registry.example.com/acme/web:1.4@sha256:${"a".repeat(64)}`,
  stack: "",
  agent: false,
  updatedAt: "2026-10-01T10:00:00Z",
  ...extra,
});

interface Sent {
  method: string;
  path: string;
  body?: unknown;
}

let sent: Sent[];
let items: ServiceItem[];
let noManager: boolean;
let me: ReturnType<typeof makeMe>;
let answerScale: () => Response | Promise<Response>;
let client: QueryClient;

const scaleResult = (extra: Record<string, unknown> = {}) =>
  Response.json({
    results: [
      {
        serviceId: "id-web",
        name: "web",
        result: "done",
        previous: 2,
        replicas: 5,
        changed: true,
        ...extra,
      },
    ],
  });

beforeEach(() => {
  sent = [];
  noManager = false;
  items = [
    svc(),
    svc({
      id: "id-agent",
      name: "bayplan-agent",
      mode: "global",
      running: 3,
      desired: 3,
      image: "bayplan/agent:1",
    }),
    svc({
      id: "id-backup",
      name: "backup",
      mode: "replicated-job",
      running: 0,
      desired: 0,
      image: "acme/backup:2",
    }),
  ];
  me = makeMe({ envs: { [ENV]: "operator" } });
  answerScale = () => scaleResult();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const body =
        req.method === "POST"
          ? await req
              .clone()
              .json()
              .catch(() => undefined)
          : undefined;
      sent.push({ method: req.method, path, body });
      if (path === "/api/me") return Response.json(me);
      if (path === `/api/env/${ENV}/services` && req.method === "GET") {
        return Response.json({
          items,
          total: items.length,
          truncated: false,
          noManager,
          fetchedAt: "2026-10-14T09:00:00Z",
        });
      }
      if (path === `/api/env/${ENV}/services/scale` && req.method === "POST") return answerScale();
      if (path === "/api/environments") {
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

const posts = () => sent.filter((s) => s.method === "POST" && s.path.endsWith("/services/scale"));

function mount() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <ServiceList envId={ENV} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

// The menu needs the signed-in user, so it is opened once /api/me has answered.
async function openMenu(name: string) {
  await screen.findByText(name);
  await waitFor(() => expect(sent.some((s) => s.path === "/api/me")).toBe(true));
  await act(async () => {});
  const trigger = screen.getByRole("button", { name: `Actions for ${name}` });
  await act(async () => {
    fireEvent.keyDown(trigger, { key: "Enter" });
  });
}

async function openScale(name = "web") {
  mount();
  await openMenu(name);
  fireEvent.click(await screen.findByRole("menuitem", { name: "Scale…" }));
  return screen.findByRole("dialog", { name: `Scale ${name}` });
}

describe("ServiceList", () => {
  it("shows each service with its mode, replicas, image and last update", async () => {
    mount();
    expect(await screen.findByText("web")).toBeTruthy();
    for (const header of ["Name", "Mode", "Replicas", "Image", "Updated"]) {
      expect(screen.getByRole("columnheader", { name: header })).toBeTruthy();
    }
    const web = screen.getByRole("row", { name: /^web/ });
    expect(within(web).getByText("Replicated")).toBeTruthy();
    expect(within(web).getByText("2/2")).toBeTruthy();
    // The tag is shown, the digest only in the tooltip.
    expect(web.textContent).toContain("registry.example.com/acme/web:1.4");
    expect(web.textContent).not.toContain("sha256");
    const agent = screen.getByRole("row", { name: /^bayplan-agent/ });
    expect(within(agent).getByText("Global")).toBeTruthy();
    expect(within(agent).getByText("3/3")).toBeTruthy();
    const job = screen.getByRole("row", { name: /^backup/ });
    expect(within(job).getByText("Replicated job")).toBeTruthy();
    expect(within(job).getByText("—", { selector: "span" })).toBeTruthy();
    expect(screen.getByText("3 services")).toBeTruthy();
  });

  it("says no manager is connected above the rows last read", async () => {
    noManager = true;
    mount();
    expect(await screen.findByText("No manager is connected")).toBeTruthy();
    expect(screen.getByText("web")).toBeTruthy();
  });

  it("hides Scale… for a global service and a job", async () => {
    mount();
    await openMenu("bayplan-agent");
    expect(await screen.findByRole("menuitem", { name: "Open" })).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Scale…" })).toBeNull();
    cleanup();
    mount();
    await openMenu("backup");
    expect(await screen.findByRole("menuitem", { name: "Open" })).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Scale…" })).toBeNull();
  });

  it("shows Scale… disabled with Needs Operator to a viewer, and opens nothing", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    mount();
    await openMenu("web");
    const scale = await screen.findByRole("menuitem", { name: /^Scale…/ });
    expect(scale.getAttribute("aria-disabled")).toBe("true");
    expect(screen.getAllByText("Needs Operator").length).toBeGreaterThan(0);
    fireEvent.click(scale);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(posts()).toEqual([]);
  });
});

describe("ScaleDialog", () => {
  it("opens from the row menu with the title, the current count and the Replicas field", async () => {
    const dialog = await openScale();
    expect(within(dialog).getByText("Set how many tasks should run. It runs 2 now.")).toBeTruthy();
    expect(within(dialog).getByRole("textbox", { name: "Replicas" })).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeTruthy();
    expect(posts()).toEqual([]);
  });

  it("posts one target, toasts, closes, and the row reads Scaling… until the list shows the new count", async () => {
    const dialog = await openScale();
    const field = within(dialog).getByRole("textbox", { name: "Replicas" });
    fireEvent.change(field, { target: { value: "5" } });
    // The list answers with the old count until it is read again.
    fireEvent.click(within(dialog).getByRole("button", { name: "Scale service" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({ targets: [{ serviceId: "id-web", replicas: 5 }] });
    expect(await screen.findByText("Scaled web to 5.")).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    const row = screen.getByRole("row", { name: /^web/ });
    expect(within(row).getByText("Scaling…")).toBeTruthy();
    // The list is read again after the write and now shows the new count.
    items = [svc({ desired: 5, running: 5 }), ...items.slice(1)];
    await act(async () => {
      await client.invalidateQueries({ queryKey: ["env", ENV, "services"] });
    });
    await waitFor(() =>
      expect(within(screen.getByRole("row", { name: /^web/ })).getByText("5/5")).toBeTruthy(),
    );
    expect(within(screen.getByRole("row", { name: /^web/ })).queryByText("Scaling…")).toBeNull();
  });

  it("sends nothing for anything but a whole number from 0 to 1000", async () => {
    const dialog = await openScale();
    const field = within(dialog).getByRole("textbox", { name: "Replicas" });
    for (const bad of ["1001", "2.5", "-1", "abc", ""]) {
      fireEvent.change(field, { target: { value: bad } });
      fireEvent.click(within(dialog).getByRole("button", { name: "Scale service" }));
      expect(await within(dialog).findByText("Enter a whole number from 0 to 1000.")).toBeTruthy();
    }
    expect(posts()).toEqual([]);
    fireEvent.change(field, { target: { value: "0" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Scale service" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]?.body).toEqual({ targets: [{ serviceId: "id-web", replicas: 0 }] });
  });

  it("keeps a service_changed refusal inside the dialog as an alert, in words and not as a code", async () => {
    answerScale = () => scaleResult({ result: "failed", code: "service_changed", changed: false });
    const dialog = await openScale();
    fireEvent.change(within(dialog).getByRole("textbox", { name: "Replicas" }), {
      target: { value: "5" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Scale service" }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toContain("This service was changed by someone else");
    expect(alert.textContent).not.toContain("service_changed");
    expect(screen.queryByText("Scaled web to 5.")).toBeNull();
    // The dialog is still open and the row, hidden from assistive technology behind it, is
    // not waiting on anything.
    expect(screen.getByRole("dialog", { name: "Scale web" })).toBeTruthy();
    const row = screen.getByRole("row", { name: /^web/, hidden: true });
    expect(within(row).queryByText("Scaling…")).toBeNull();
  });

  it("sends the request once however often Scale service is pressed", async () => {
    let release: (r: Response) => void = () => {};
    answerScale = () => new Promise<Response>((resolve) => (release = resolve));
    const dialog = await openScale();
    fireEvent.change(within(dialog).getByRole("textbox", { name: "Replicas" }), {
      target: { value: "4" },
    });
    const confirm = within(dialog).getByRole("button", { name: "Scale service" });
    fireEvent.click(confirm);
    fireEvent.click(confirm);
    await waitFor(() => expect(posts()).toHaveLength(1));
    release(scaleResult({ replicas: 4 }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(posts()).toHaveLength(1);
  });
});
