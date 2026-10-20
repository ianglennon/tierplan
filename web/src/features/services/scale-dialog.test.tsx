// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { components } from "@/api/schema";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import type { BulkReport } from "@/features/containers/bulk-result-dialog";
import { ScaleDialog, type ScaledService } from "@/features/services/scale-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";

type ServiceItem = components["schemas"]["ServiceItem"];

const web: ServiceItem = {
  id: "id-web",
  name: "web",
  mode: "replicated",
  running: 2,
  desired: 3,
  updateState: "",
  image: "acme/web:1",
  stack: "",
  bayplanStackId: "",
  agent: false,
  updatedAt: "2026-10-01T10:00:00Z",
  ports: [],
  updateMessage: "",
  hasPreviousSpec: false,
  createdAt: "2026-09-01T10:00:00Z",
};

let posted: unknown[];
let answer: () => Response;

beforeEach(() => {
  posted = [];
  answer = () =>
    Response.json({
      results: [
        {
          serviceId: "id-web",
          name: "web",
          result: "done",
          previous: 3,
          replicas: 5,
          changed: true,
        },
      ],
    });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "operator" } }));
      if (path === `/api/env/${ENV}/services/scale`) {
        posted.push(await req.clone().json());
        return answer();
      }
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Swarm one", envType: "swarm" }] });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  // A toast one test raised (with its Details action) must not still be on screen in the next
  // test's Toaster, where a click on "Details" could reach the earlier test's callback.
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
});

function mount(target: ServiceItem = web, onOpenChange = vi.fn()) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <ScaleDialog envId={ENV} targets={[target]} open onOpenChange={onOpenChange} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
  return { onOpenChange };
}

const field = () => screen.getByRole("textbox", { name: "Replicas" });
const confirm = () => screen.getByRole("button", { name: "Scale service" });

describe("ScaleDialog", () => {
  it("is titled Scale {name}, says what it runs now, and offers Cancel and Scale service", () => {
    mount();
    const dialog = screen.getByRole("dialog", { name: "Scale web" });
    expect(within(dialog).getByText("Set how many tasks should run. It runs 3 now.")).toBeTruthy();
    expect((field() as HTMLInputElement).value).toBe("3");
    expect(within(dialog).getByRole("button", { name: "Cancel" })).toBeTruthy();
    expect(confirm()).toBeTruthy();
  });

  it("accepts a whole number from 0 to 1000 and nothing else", async () => {
    mount();
    for (const bad of ["1001", "2.5", "-1", "1e2", "abc", ""]) {
      fireEvent.change(field(), { target: { value: bad } });
      fireEvent.click(confirm());
      expect(await screen.findByText("Enter a whole number from 0 to 1000.")).toBeTruthy();
    }
    expect(posted).toEqual([]);
    fireEvent.change(field(), { target: { value: "1000" } });
    fireEvent.click(confirm());
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({ targets: [{ serviceId: "id-web", replicas: 1000 }] });
  });

  it("keeps a failure inside the dialog in words, not a code", async () => {
    answer = () =>
      Response.json({
        results: [
          { serviceId: "id-web", name: "web", result: "failed", code: "no_quorum", changed: false },
        ],
      });
    const { onOpenChange } = mount();
    fireEvent.change(field(), { target: { value: "5" } });
    fireEvent.click(confirm());
    const alert = await screen.findByText(/The swarm has lost its quorum/);
    expect(alert.textContent).not.toContain("no_quorum");
    expect(screen.getByRole("dialog", { name: "Scale web" })).toBeTruthy();
    expect(onOpenChange).not.toHaveBeenCalled();
  });

  it("says which setting lets Bayplan change a service with host access, and never shows the code", async () => {
    answer = () =>
      Response.json({
        results: [
          {
            serviceId: "id-web",
            name: "web",
            result: "failed",
            code: "agent_host_policy",
            detail: "ignored: the dialog words this itself",
            changed: false,
          },
        ],
      });
    const { onOpenChange } = mount();
    fireEvent.change(field(), { target: { value: "5" } });
    fireEvent.click(confirm());
    const alert = await screen.findByText(
      "This swarm's manager agents don't let Bayplan change services with host access. Set BAYPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true on them to allow it.",
    );
    expect(alert.textContent).not.toContain("agent_host_policy");
    expect(screen.getByRole("dialog", { name: "Scale web" })).toBeTruthy();
    expect(onOpenChange).not.toHaveBeenCalled();
  });

  it("warns that scaling to 0 stops every task, and still lets the scale through", async () => {
    mount();
    expect(screen.queryByText(/Scaling to 0 stops every task/)).toBeNull();
    fireEvent.change(field(), { target: { value: "0" } });
    expect(
      await screen.findByText(
        "Scaling to 0 stops every task. The service stays, and you can scale it up again.",
      ),
    ).toBeTruthy();
    fireEvent.click(confirm());
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({ targets: [{ serviceId: "id-web", replicas: 0 }] });
  });

  it("drops the zero warning again when the count is not 0", async () => {
    mount();
    fireEvent.change(field(), { target: { value: "0" } });
    await screen.findByText(/Scaling to 0 stops every task/);
    fireEvent.change(field(), { target: { value: "2" } });
    await waitFor(() => expect(screen.queryByText(/Scaling to 0 stops every task/)).toBeNull());
  });

  it("warns that the next stack deploy sets the replica count again for a stack service", async () => {
    mount({ ...web, stack: "shop" });
    expect(
      screen.getByText(
        "web belongs to stack shop. The next stack deploy sets its replica count again.",
      ),
    ).toBeTruthy();
    fireEvent.change(field(), { target: { value: "5" } });
    fireEvent.click(confirm());
    await waitFor(() => expect(posted).toHaveLength(1));
  });

  it("shows no stack warning for a service that belongs to no stack", () => {
    mount();
    expect(screen.queryByText(/belongs to stack/)).toBeNull();
  });

  it("links the stack's name in the warning when Bayplan manages the stack, the text unchanged", () => {
    mount({ ...web, stack: "shop", bayplanStackId: "stk-shop" });
    const link = screen.getByRole("link", { name: "shop" });
    expect(link.getAttribute("href")).toBe("/env/e1/stacks/stk-shop");
    expect(link.closest('[role="status"]')?.textContent).toBe(
      "web belongs to stack shop. The next stack deploy sets its replica count again.",
    );
  });

  it("keeps the stack's name as text in the warning when Bayplan does not manage the stack", () => {
    mount({ ...web, stack: "shop" });
    expect(screen.queryByRole("link", { name: "shop" })).toBeNull();
  });
});

const api: ServiceItem = {
  ...web,
  id: "id-api",
  name: "api",
  desired: 2,
  running: 2,
  stack: "shop",
};
const worker: ServiceItem = {
  ...web,
  id: "id-worker",
  name: "worker",
  desired: 1,
  running: 1,
  stack: "shop",
};
const agent: ServiceItem = {
  ...web,
  id: "id-agent",
  name: "bayplan-agent",
  mode: "global",
  desired: 3,
  running: 3,
};
const backup: ServiceItem = {
  ...web,
  id: "id-backup",
  name: "backup",
  mode: "replicated-job",
  desired: 0,
  running: 0,
};

const doneResult = (s: ServiceItem, replicas: number) => ({
  serviceId: s.id,
  name: s.name,
  result: "done",
  previous: s.desired,
  replicas,
  changed: replicas !== s.desired,
});

function mountBulk(targets: ServiceItem[]) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const onOpenChange = vi.fn();
  const onScaled = vi.fn<(done: ScaledService[]) => void>();
  const onDetails = vi.fn<(report: BulkReport) => void>();
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <ScaleDialog
          envId={ENV}
          targets={targets}
          open
          onOpenChange={onOpenChange}
          onScaled={onScaled}
          onDetails={onDetails}
        />
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
  return { onOpenChange, onScaled, onDetails };
}

const newCount = (name: string) =>
  screen.getByRole("textbox", { name: `Replicas for ${name}` }) as HTMLInputElement;

describe("ScaleDialog: several services", () => {
  it("is titled Scale {n} services and lists every one with Service, Now, New and Note", () => {
    mountBulk([web, api, agent, backup]);
    const dialog = screen.getByRole("dialog", { name: "Scale 4 services" });
    expect(within(dialog).getByText("Set how many tasks each service should run.")).toBeTruthy();
    expect(
      within(dialog)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Service", "Now", "New", "Note"]);
    const rows = within(dialog).getAllByRole("row").slice(1);
    expect(rows.map((r) => r.querySelectorAll("td")[0]?.textContent)).toEqual([
      "web",
      "api",
      "bayplan-agent",
      "backup",
    ]);
    expect(newCount("web").value).toBe("3");
    expect(newCount("api").value).toBe("2");
    expect(within(dialog).getByText("Part of stack shop")).toBeTruthy();
  });

  it("scrolls the list inside 240px", () => {
    mountBulk([web, api]);
    const list = document.querySelector('[data-slot="target-list"]');
    expect(list?.className).toContain("max-h-60");
    expect(list?.className).toContain("overflow-y-auto");
  });

  it("mutes a global service and a job as Skipped with their note, gives them no input and leaves them out of the button label", () => {
    mountBulk([web, api, agent, backup]);
    const dialog = screen.getByRole("dialog", { name: "Scale 4 services" });
    const rowOfName = (name: string) =>
      within(dialog)
        .getAllByRole("row")
        .find((r) => r.querySelectorAll("td")[0]?.textContent === name) as HTMLElement;
    for (const [name, note] of [
      ["bayplan-agent", "Global services can't be scaled."],
      ["backup", "Jobs can't be scaled here."],
    ] as const) {
      const row = rowOfName(name);
      expect(row.hasAttribute("data-skipped")).toBe(true);
      expect(within(row).getByText(note)).toBeTruthy();
      expect(within(row).getByText("Skipped")).toBeTruthy();
      expect(within(row).queryByRole("textbox")).toBeNull();
    }
    expect(within(dialog).getByRole("button", { name: "Scale 2 services" })).toBeTruthy();
    expect(within(dialog).queryByRole("button", { name: "Scale 4 services" })).toBeNull();
  });

  it("fills every eligible input from Set all to, and leaves a skipped row alone", () => {
    mountBulk([web, api, agent]);
    fireEvent.change(screen.getByRole("textbox", { name: "Set all to" }), {
      target: { value: "4" },
    });
    expect(newCount("web").value).toBe("4");
    expect(newCount("api").value).toBe("4");
    fireEvent.change(newCount("api"), { target: { value: "6" } });
    expect(newCount("web").value).toBe("4");
    expect(newCount("api").value).toBe("6");
  });

  it("posts one request with every eligible target and a bulk ID, then toasts and closes", async () => {
    answer = () =>
      Response.json({ results: [doneResult(web, 5), doneResult(api, 2)], bulkId: "ignored" });
    const { onOpenChange, onScaled } = mountBulk([web, api, agent, backup]);
    fireEvent.change(newCount("web"), { target: { value: "5" } });
    fireEvent.click(screen.getByRole("button", { name: "Scale 2 services" }));
    await waitFor(() => expect(posted).toHaveLength(1));
    const body = posted[0] as { bulkId?: string; targets: unknown[] };
    expect(body.targets).toEqual([
      { serviceId: "id-web", replicas: 5 },
      { serviceId: "id-api", replicas: 2 },
    ]);
    expect(body.bulkId).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
    expect(await screen.findByText(/Scaled 2 services\./)).toBeTruthy();
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
    // The list waits only on the service whose count changed.
    expect(onScaled).toHaveBeenCalledWith([
      { id: "id-web", replicas: 5, changed: true },
      { id: "id-api", replicas: 2, changed: false },
    ]);
  });

  it("sends a fresh bulk ID for each scale", async () => {
    answer = () => Response.json({ results: [doneResult(web, 3), doneResult(api, 2)] });
    mountBulk([web, api]);
    fireEvent.click(screen.getByRole("button", { name: "Scale 2 services" }));
    await waitFor(() => expect(posted).toHaveLength(1));
    cleanup();
    mountBulk([web, api]);
    fireEvent.click(screen.getByRole("button", { name: "Scale 2 services" }));
    await waitFor(() => expect(posted).toHaveLength(2));
    const ids = posted.map((p) => (p as { bulkId: string }).bulkId);
    expect(new Set(ids).size).toBe(2);
  });

  it("sums a partial failure up as 'Scaled 1, 1 failed' with Details that carry the Service, Result and Reason rows", async () => {
    answer = () =>
      Response.json({
        results: [
          doneResult(web, 5),
          {
            serviceId: "id-api",
            name: "api",
            result: "failed",
            code: "service_changed",
            replicas: 2,
            changed: false,
          },
        ],
      });
    const { onDetails } = mountBulk([web, api, agent]);
    fireEvent.click(screen.getByRole("button", { name: "Scale 2 services" }));
    expect(await screen.findByText(/Scaled 1, 1 failed/)).toBeTruthy();
    expect(screen.getByText(/1 skipped/)).toBeTruthy();
    fireEvent.click(await screen.findByRole("button", { name: "Details" }));
    expect(onDetails).toHaveBeenCalledTimes(1);
    const report = onDetails.mock.calls[0]?.[0] as BulkReport;
    expect(report.title).toBe("Scale results");
    expect(report.rows.map((r) => [r.name, r.result])).toEqual([
      ["web", "done"],
      ["api", "failed"],
      ["bayplan-agent", "skipped"],
    ]);
    expect(report.rows[1]?.reason).toContain("changed by someone else");
    expect(report.rows[1]?.reason).not.toContain("service_changed");
    expect(report.rows[2]?.reason).toBe("Global services can't be scaled.");
    expect(report.bulkId).toBeTruthy();
  });

  it("closes once the bulk is answered, even when every service failed, and sums it up in one toast", async () => {
    answer = () =>
      Response.json({
        results: [
          { serviceId: "id-web", name: "web", result: "failed", code: "no_quorum", changed: false },
          { serviceId: "id-api", name: "api", result: "failed", code: "no_quorum", changed: false },
        ],
      });
    const { onOpenChange } = mountBulk([web, api]);
    fireEvent.click(screen.getByRole("button", { name: "Scale 2 services" }));
    expect(await screen.findByText(/Scaled 0, 2 failed/)).toBeTruthy();
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });

  it("sends nothing while a row holds anything but a whole number from 0 to 1000", async () => {
    mountBulk([web, api]);
    fireEvent.change(newCount("api"), { target: { value: "1001" } });
    fireEvent.click(screen.getByRole("button", { name: "Scale 2 services" }));
    expect(await screen.findByText("Enter a whole number from 0 to 1000.")).toBeTruthy();
    expect(newCount("api").getAttribute("aria-invalid")).toBe("true");
    expect(newCount("web").getAttribute("aria-invalid")).not.toBe("true");
    expect(posted).toEqual([]);
  });

  it("warns once if any row is set to 0, and still lets the scale through", async () => {
    answer = () => Response.json({ results: [doneResult(web, 0), doneResult(api, 2)] });
    mountBulk([web, api]);
    expect(screen.queryByText(/Scaling to 0 stops every task/)).toBeNull();
    fireEvent.change(newCount("web"), { target: { value: "0" } });
    expect(await screen.findAllByText(/Scaling to 0 stops every task/)).toHaveLength(1);
    fireEvent.click(screen.getByRole("button", { name: "Scale 2 services" }));
    await waitFor(() => expect(posted).toHaveLength(1));
  });

  it("warns plural-aware about the stack services among the ones it will scale", () => {
    mountBulk([web, api, worker, agent]);
    expect(
      screen.getByText(
        "2 of these belong to stacks. The next stack deploy sets their replica counts again.",
      ),
    ).toBeTruthy();
    cleanup();
    mountBulk([web, api]);
    expect(
      screen.getByText(
        "1 of these belongs to a stack. The next stack deploy sets its replica count again.",
      ),
    ).toBeTruthy();
    cleanup();
    mountBulk([web, backup]);
    expect(screen.queryByText(/of these belong/)).toBeNull();
  });

  it("keeps a long name whole in a tooltip and cut in the list", () => {
    const long = `${"a-very-long-service-name-".repeat(6)}end`;
    mountBulk([{ ...web, name: long }, api]);
    const cell = screen.getAllByText(long)[0] as HTMLElement;
    expect(cell.className).toContain("truncate");
  });
});
