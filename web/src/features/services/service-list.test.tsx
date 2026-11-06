// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { components } from "@/api/schema";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import type { ServiceSearch, ServiceSearchPatch } from "@/features/services/list-search";
import { ServiceList } from "@/features/services/service-list";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";
import { waitUnderFakeClock } from "@/test-settle";

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
  tierplanStackId: "",
  agent: false,
  updatedAt: "2026-10-01T10:00:00Z",
  ports: [],
  updateMessage: "",
  hasPreviousSpec: false,
  createdAt: "2026-09-01T10:00:00Z",
  ...extra,
});

interface Sent {
  method: string;
  path: string;
  // The query string of the request, with its leading "?" when it has one.
  search: string;
  body?: unknown;
}

const COUNTS = { degraded: 0, updating: 0, problem: 0, loop: 0 };

let sent: Sent[];
let items: ServiceItem[];
let noManager: boolean;
let me: ReturnType<typeof makeMe>;
let answerScale: () => Response | Promise<Response>;
let answerCheck: () => Response | Promise<Response>;
let client: QueryClient;
// What the list adds to or replaces in its answer, and a full replacement of the answer.
let listExtra: Record<string, unknown>;
let answerList: ((url: URL) => Response | Promise<Response>) | undefined;
// The changes the list asked its owner to make to the search.
let changes: ServiceSearchPatch[];

function listBody(rows: ServiceItem[], extra: Record<string, unknown> = {}) {
  return {
    items: rows,
    total: rows.length,
    unfilteredTotal: rows.length,
    capped: false,
    statusCounts: COUNTS,
    noManager,
    fetchedAt: "2026-10-14T09:00:00Z",
    ...extra,
  };
}

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
  listExtra = {};
  answerList = undefined;
  changes = [];
  items = [
    svc(),
    svc({
      id: "id-agent",
      name: "tierplan-agent",
      mode: "global",
      running: 3,
      desired: 3,
      image: "tierplan/agent:1",
      agent: true,
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
  answerCheck = () => Response.json({ checked: 3, updates: 1, failed: 0 });
  client = new QueryClient({ defaultOptions: { queries: { retry: false, retryDelay: 1 } } });
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
      const url = new URL(req.url);
      sent.push({ method: req.method, path, search: url.search, body });
      if (path === "/api/me") return Response.json(me);
      if (path === `/api/env/${ENV}/services` && req.method === "GET") {
        return answerList ? answerList(url) : Response.json(listBody(items, listExtra));
      }
      if (path === `/api/env/${ENV}/services/scale` && req.method === "POST") return answerScale();
      if (path === `/api/env/${ENV}/updates/check` && req.method === "POST") return answerCheck();
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

function mount(search: ServiceSearch = {}) {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <ServiceList
            envId={ENV}
            search={search}
            onSearchChange={(patch) => changes.push(patch)}
          />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

// The list requests the server was asked for, as path and query.
const listRequests = () =>
  sent
    .filter((s) => s.method === "GET" && s.path === `/api/env/${ENV}/services`)
    .map((s) => s.search);

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
    for (const header of ["Name", "Updates", "Mode", "Replicas", "Image", "Updated"]) {
      expect(screen.getByRole("columnheader", { name: header })).toBeTruthy();
    }
    const web = screen.getByRole("row", { name: /^Select web/ });
    expect(within(web).getByText("Replicated")).toBeTruthy();
    expect(within(web).getByText("2/2")).toBeTruthy();
    // The tag is shown, the digest only in the tooltip.
    expect(web.textContent).toContain("registry.example.com/acme/web:1.4");
    expect(web.textContent).not.toContain("sha256");
    const agent = screen.getByRole("row", { name: /^Select tierplan-agent/ });
    expect(within(agent).getByText("Global")).toBeTruthy();
    expect(within(agent).getByText("3/3")).toBeTruthy();
    const job = screen.getByRole("row", { name: /^Select backup/ });
    expect(within(job).getByText("Replicated job")).toBeTruthy();
    // The replicas and the Updates cell each show a dash: the job has no count, and no update object.
    expect(within(job).getAllByText("—", { selector: "span" })).toHaveLength(2);
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
    await openMenu("tierplan-agent");
    expect(await screen.findByRole("menuitem", { name: "Open" })).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Scale…" })).toBeNull();
    cleanup();
    mount();
    await openMenu("backup");
    expect(await screen.findByRole("menuitem", { name: "Open" })).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Scale…" })).toBeNull();
  });

  it("links the name to the service's page", async () => {
    mount();
    const link = await screen.findByRole("link", { name: "web" });
    expect(link.getAttribute("href")).toBe("/env/e1/services/id-web");
    expect(screen.getByRole("link", { name: "tierplan-agent" }).getAttribute("href")).toBe(
      "/env/e1/services/id-agent",
    );
  });

  it("opens the service's page from Open in the row menu, for every service", async () => {
    mount();
    for (const [name, id] of [
      ["web", "id-web"],
      ["tierplan-agent", "id-agent"],
      ["backup", "id-backup"],
    ] as const) {
      await openMenu(name);
      const open = await screen.findByRole("menuitem", { name: "Open" });
      expect(open.getAttribute("aria-disabled")).toBeNull();
      expect(open.getAttribute("data-disabled")).toBeNull();
      expect(open.getAttribute("href")).toBe(`/env/e1/services/${id}`);
      fireEvent.keyDown(open, { key: "Escape" });
    }
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

describe("the row menu's Force update… and Roll back…", () => {
  it("lists Force update… and Roll back… for every mode, after Scale… where there is one", async () => {
    mount();
    await openMenu("web");
    const names = (await screen.findAllByRole("menuitem")).map((m) =>
      m.textContent?.replace(/\s+/g, " ").trim(),
    );
    expect(names.slice(0, 4)).toEqual([
      "Open",
      "Scale…",
      "Force update…",
      "Roll back…There's no earlier version to roll back to.",
    ]);
    cleanup();
    mount();
    await openMenu("tierplan-agent");
    expect(await screen.findByRole("menuitem", { name: "Force update…" })).toBeTruthy();
    expect(screen.getByRole("menuitem", { name: /^Roll back…/ })).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Scale…" })).toBeNull();
  });

  it("opens the force update dialog from the row menu", async () => {
    mount();
    await openMenu("web");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Force update…" }));
    expect(await screen.findByRole("dialog", { name: "Force update web?" })).toBeTruthy();
    expect(posts()).toEqual([]);
  });

  it("opens the rollback dialog from the row menu when the service has an earlier version", async () => {
    items = [svc({ hasPreviousSpec: true }), ...items.slice(1)];
    mount();
    await openMenu("web");
    const back = await screen.findByRole("menuitem", { name: /^Roll back…/ });
    expect(back.getAttribute("aria-disabled")).toBeNull();
    fireEvent.click(back);
    expect(await screen.findByRole("dialog", { name: "Roll back web?" })).toBeTruthy();
  });

  it("shows Roll back… disabled with the reason, and opens nothing, without an earlier version", async () => {
    mount();
    await openMenu("web");
    const back = await screen.findByRole("menuitem", { name: /^Roll back…/ });
    expect(back.getAttribute("aria-disabled")).toBe("true");
    expect(within(back).getByText("There's no earlier version to roll back to.")).toBeTruthy();
    fireEvent.click(back);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("shows both disabled with Needs Operator to a viewer, and opens nothing", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    items = [svc({ hasPreviousSpec: true }), ...items.slice(1)];
    mount();
    await openMenu("web");
    for (const name of [/^Force update…/, /^Roll back…/]) {
      const item = await screen.findByRole("menuitem", { name });
      expect(item.getAttribute("aria-disabled")).toBe("true");
      fireEvent.click(item);
    }
    expect(screen.getAllByText("Needs Operator").length).toBeGreaterThan(0);
    expect(screen.queryByRole("dialog")).toBeNull();
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
    const row = screen.getByRole("row", { name: /^Select web/ });
    expect(within(row).getByText("Scaling…")).toBeTruthy();
    // The list is read again after the write and now shows the new count.
    items = [svc({ desired: 5, running: 5 }), ...items.slice(1)];
    await act(async () => {
      await client.invalidateQueries({ queryKey: ["env", ENV, "services"] });
    });
    await waitFor(() =>
      expect(
        within(screen.getByRole("row", { name: /^Select web/ })).getByText("5/5"),
      ).toBeTruthy(),
    );
    expect(
      within(screen.getByRole("row", { name: /^Select web/ })).queryByText("Scaling…"),
    ).toBeNull();
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
    const row = screen.getByRole("row", { name: /^Select web/, hidden: true });
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

describe("the restart-loop badge", () => {
  const LOOP = {
    state: "flagged" as const,
    count: 4,
    windowMinutes: 10,
    latestError: "task: non-zero exit (1)",
    since: "2026-10-14T09:00:00Z",
  };

  it("sits after the name of a flagged service and beside its state, never in place of it", async () => {
    items = [
      svc({ running: 0, desired: 2, restartLoop: LOOP }),
      svc({ id: "id-ok", name: "calm" }),
    ];
    mount();
    await screen.findByText("web");
    const flagged = within(screen.getByRole("row", { name: /^Select web/ }));
    expect(flagged.getByText("Restart loop")).toBeTruthy();
    expect(flagged.getByText("Not running")).toBeTruthy();
    expect(
      within(screen.getByRole("row", { name: /^Select calm/ })).queryByText("Restart loop"),
    ).toBeNull();
  });

  it("draws the grey Muted badge in place of Restart loop for a muted loop", async () => {
    items = [svc({ restartLoop: { ...LOOP, state: "muted" } })];
    mount();
    await screen.findByText("web");
    const row = within(screen.getByRole("row", { name: /^Select web/ }));
    expect(row.getByText("Muted")).toBeTruthy();
    expect(row.queryByText("Restart loop")).toBeNull();
  });

  it("puts Mute restart-loop alerts in the row menu of a service in a loop", async () => {
    items = [svc({ restartLoop: LOOP })];
    mount();
    await openMenu("web");
    expect(await screen.findByRole("menuitem", { name: "Mute restart-loop alerts" })).toBeTruthy();
  });

  it("puts Unmute restart-loop alerts there when the loop is muted", async () => {
    items = [svc({ restartLoop: { ...LOOP, state: "muted" } })];
    mount();
    await openMenu("web");
    expect(
      await screen.findByRole("menuitem", { name: "Unmute restart-loop alerts" }),
    ).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Mute restart-loop alerts" })).toBeNull();
  });

  it("leaves the item out for a service that is not in a loop", async () => {
    mount();
    await openMenu("web");
    await screen.findByRole("menuitem", { name: /Force update/ });
    expect(screen.queryByRole("menuitem", { name: /restart-loop alerts/ })).toBeNull();
  });

  it("is not drawn for a service whose restartLoop is null", async () => {
    mount();
    await screen.findByText("web");
    expect(
      screen.queryAllByText("Restart loop").map((e) => e.closest("[data-slot='badge']")),
    ).toEqual([]);
  });
});

describe("the State column", () => {
  it("comes after Name and shows each row's state word by the priority rule", async () => {
    items = [
      svc(),
      svc({ id: "id-deg", name: "degraded-svc", running: 1, desired: 3 }),
      svc({ id: "id-down", name: "down-svc", running: 0, desired: 2 }),
      svc({ id: "id-zero", name: "zero-svc", running: 0, desired: 0 }),
      svc({ id: "id-upd", name: "updating-svc", updateState: "updating" }),
      svc({ id: "id-paused", name: "paused-svc", updateState: "paused" }),
      svc({ id: "id-rb", name: "rolledback-svc", updateState: "rollback_completed" }),
      svc({ id: "id-job", name: "job-svc", mode: "replicated-job", running: 0, desired: 0 }),
    ];
    mount();
    await screen.findByText("web");
    const headers = screen.getAllByRole("columnheader").map((h) => h.textContent);
    // The checkbox column has no title.
    expect(headers.slice(1, 5)).toEqual(["Name", "State", "Updates", "Mode"]);
    const stateOf = (name: RegExp) => within(screen.getByRole("row", { name })).getByText;
    expect(stateOf(/^Select web/)("Running")).toBeTruthy();
    expect(stateOf(/^Select degraded-svc/)("Degraded")).toBeTruthy();
    expect(stateOf(/^Select down-svc/)("Not running")).toBeTruthy();
    expect(stateOf(/^Select zero-svc/)("Scaled to 0")).toBeTruthy();
    expect(stateOf(/^Select updating-svc/)("Updating")).toBeTruthy();
    expect(stateOf(/^Select paused-svc/)("Update paused")).toBeTruthy();
    expect(stateOf(/^Select rolledback-svc/)("Rolled back")).toBeTruthy();
    expect(stateOf(/^Select job-svc/)("Completed")).toBeTruthy();
  });

  it("explains a service's replicas in a tooltip", async () => {
    items = [svc({ running: 1, desired: 3 })];
    mount();
    // The Degraded chip has the same word until the counts arrive, so look in the row.
    await screen.findByText("web");
    const word = within(screen.getByRole("row", { name: /^Select web/ })).getByText("Degraded");
    await act(async () => {
      fireEvent.focus(word);
    });
    expect((await screen.findAllByText("1 of 3 replicas running")).length).toBeGreaterThan(0);
  });

  it("shows a pulsing Scaling… in the State cell, with the target in Replicas, until the list shows the new count", async () => {
    const dialog = await openScale();
    fireEvent.change(within(dialog).getByRole("textbox", { name: "Replicas" }), {
      target: { value: "5" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Scale service" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    const row = screen.getByRole("row", { name: /^Select web/ });
    const cells = within(row).getAllByRole("cell");
    // Select, Name, State, Updates, Mode, Replicas
    expect(within(cells[2] as HTMLElement).getByText("Scaling…")).toBeTruthy();
    expect(within(cells[2] as HTMLElement).queryByText("Running")).toBeNull();
    expect(cells[5]?.textContent).toBe("2/5");
    expect(within(cells[5] as HTMLElement).queryByText("Scaling…")).toBeNull();
  });
});

describe("a standalone environment", () => {
  it("shows This isn't a swarm with a way back to containers instead of the table", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        const path = new URL(req.url).pathname;
        if (path === "/api/me") return Response.json(me);
        if (path === `/api/env/${ENV}/services`) {
          return Response.json(
            {
              type: "not_a_swarm",
              title: "Conflict",
              status: 409,
              detail: "This environment is a standalone Docker host, not a swarm.",
            },
            { status: 409 },
          );
        }
        if (path === "/api/environments") {
          return Response.json({ items: [{ id: ENV, name: "Homelab", envType: "standalone" }] });
        }
        return Response.json({}, { status: 404 });
      }),
    );
    mount();
    expect(await screen.findByRole("heading", { name: "This isn't a swarm" })).toBeTruthy();
    expect(
      await screen.findByText(
        "Homelab is a standalone host, so it has no services, nodes, secrets or configs.",
      ),
    ).toBeTruthy();
    const back = screen.getByRole("link", { name: "Back to containers" });
    expect(back.getAttribute("href")).toBe(`/env/${ENV}/containers`);
    expect(screen.queryByRole("table")).toBeNull();
    expect(screen.queryByText("Couldn't load services.")).toBeNull();
  });
});

const names = () =>
  screen
    .getAllByRole("row")
    .map((r) => r.textContent ?? "")
    .filter((t) => t !== "");

describe("the services list: stack links", () => {
  it("links a stack TierPlan manages in its group header and a foreign stack to its read-only page", async () => {
    items = [
      svc({ id: "id-web", name: "shop_web", stack: "shop", tierplanStackId: "stk-shop" }),
      svc({ id: "id-old", name: "old_app", stack: "legacy" }),
    ];
    mount();
    await screen.findByText("shop_web");
    const headers = Array.from(
      document.querySelectorAll<HTMLElement>('[data-slot="group-header-row"]'),
    );
    const shop = headers.find((h) => h.textContent?.startsWith("shop")) as HTMLElement;
    const legacy = headers.find((h) => h.textContent?.startsWith("legacy")) as HTMLElement;
    expect(within(shop).getByRole("link", { name: "shop" }).getAttribute("href")).toBe(
      "/env/e1/stacks/stk-shop",
    );
    expect(within(legacy).getByRole("link", { name: "legacy" }).getAttribute("href")).toBe(
      "/env/e1/unmanaged-stacks/legacy",
    );
  });
});

describe("the services list: groups", () => {
  const STACKED = () => [
    svc({ id: "id-web", name: "shop_web", stack: "shop" }),
    svc({ id: "id-db", name: "shop_db", stack: "shop", running: 0, desired: 1 }),
    svc({ id: "id-blog", name: "blog_app", stack: "Blog" }),
    svc({ id: "id-loose", name: "loose" }),
  ];

  it("groups the rows under their stack, A to Z, with Ungrouped last and a healthy count in each header", async () => {
    items = STACKED();
    mount();
    await screen.findByText("shop_web");
    const headers = Array.from(document.querySelectorAll('[data-slot="group-header-row"]')).map(
      (h) => h.textContent,
    );
    expect(headers).toEqual([
      expect.stringMatching(/^Blog\s*Stack\s*1 of 1 healthy$/),
      expect.stringMatching(/^shop\s*Stack\s*1 of 2 healthy$/),
      expect.stringMatching(/^Ungrouped\s*1 of 1 healthy$/),
    ]);
    // Inside a group the rows follow the sort: name ascending.
    const rows = names();
    expect(rows.findIndex((r) => r.startsWith("shop_db"))).toBeLessThan(
      rows.findIndex((r) => r.startsWith("shop_web")),
    );
  });

  it("shows one flat list, with no header bands, when Group by is None", async () => {
    items = STACKED();
    mount({ group: "none" });
    await screen.findByText("shop_web");
    expect(document.querySelectorAll('[data-slot="group-header-row"]')).toHaveLength(0);
    expect(screen.getByRole("radio", { name: "None" }).getAttribute("aria-checked")).toBe("true");
  });

  it("asks for None through the search and keeps Stack as the default that leaves the URL", async () => {
    items = STACKED();
    mount();
    await screen.findByText("shop_web");
    expect(screen.getByRole("radio", { name: "Stack" }).getAttribute("aria-checked")).toBe("true");
    fireEvent.click(screen.getByRole("radio", { name: "None" }));
    expect(changes).toEqual([{ group: "none" }]);
    cleanup();
    changes = [];
    mount({ group: "none" });
    await screen.findByText("shop_web");
    fireEvent.click(screen.getByRole("radio", { name: "Stack" }));
    expect(changes).toEqual([{ group: undefined }]);
  });

  it("folds every group away and opens them again with the collapse-all button", async () => {
    items = STACKED();
    mount();
    await screen.findByText("shop_web");
    fireEvent.click(screen.getByRole("button", { name: "Collapse all groups" }));
    expect(screen.queryByText("shop_web")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Expand all groups" }));
    expect(screen.getByText("shop_web")).toBeTruthy();
  });
});

describe("the services list: search and status chips", () => {
  it("has a search labelled Search services, with the words of the spec", async () => {
    mount();
    await screen.findByText("web");
    const box = screen.getByRole("searchbox", { name: "Search services" });
    expect(box.getAttribute("placeholder")).toBe("Search by name, image, label or stack");
  });

  it("waits 250 ms, then puts the search in the URL and asks the server for it", async () => {
    mount();
    await screen.findByText("web");
    const box = screen.getByRole("searchbox", { name: "Search services" });
    answerList = (url) =>
      Response.json(
        listBody(
          items.filter((r) => r.name.includes(url.searchParams.get("q") ?? "")),
          { unfilteredTotal: 3 },
        ),
      );
    fireEvent.change(box, { target: { value: "web" } });
    expect(changes).toEqual([]);
    await waitFor(() => expect(changes).toEqual([{ q: "web" }]));
  });

  it("sends the search and the statuses of the URL to the server, and shows the rows it answers", async () => {
    answerList = (url) => {
      const q = url.searchParams.get("q");
      return Response.json(
        listBody(q ? items.filter((r) => r.name.includes(q)) : items, {
          unfilteredTotal: 3,
          statusCounts: { degraded: 2, updating: 1, problem: 0 },
        }),
      );
    };
    mount({ q: "web", status: "degraded,problem" });
    await screen.findByText("web");
    await waitFor(() => expect(listRequests()).toContain("?q=web&status=degraded%2Cproblem"));
    expect(screen.queryByText("backup")).toBeNull();
  });

  it("shows Degraded, Updating, Update problem and Restart loop with the counts of the whole list, in a group named Filter by status", async () => {
    listExtra = {
      statusCounts: { degraded: 3, updating: 1, problem: 0, loop: 2 },
      unfilteredTotal: 9,
    };
    mount();
    await screen.findByText("web");
    const group = screen.getByRole("group", { name: "Filter by status" });
    expect(
      within(group)
        .getAllByRole("button")
        .map((b) => b.textContent),
    ).toEqual(["Degraded 3", "Updating 1", "Update problem 0", "Restart loop 2"]);
    // The chips wrap onto further rows at every width, so none is cut off at 375px (G-06-2).
    expect(group.classList.contains("flex-wrap")).toBe(true);
    expect(group.className).not.toMatch(/overflow-x-auto|flex-nowrap/);
  });

  it("toggles the Restart loop chip through the search as status=loop", async () => {
    listExtra = { statusCounts: { degraded: 0, updating: 0, problem: 1, loop: 2 } };
    mount({ status: "problem" });
    await screen.findByText("web");
    fireEvent.click(screen.getByRole("button", { name: "Restart loop 2" }));
    expect(changes).toEqual([{ status: "problem,loop" }]);
  });

  it("sends status=loop to the server, which filters before its cap", async () => {
    mount({ status: "loop" });
    await screen.findByText("web");
    await waitFor(() => expect(listRequests()).toContain("?status=loop"));
    expect(screen.getByRole("button", { name: /^Restart loop/ }).getAttribute("aria-pressed")).toBe(
      "true",
    );
  });

  it("toggles a chip through the search, in chip order, and shows it pressed", async () => {
    listExtra = { statusCounts: { degraded: 3, updating: 1, problem: 2 } };
    mount({ status: "problem" });
    await screen.findByText("web");
    expect(
      screen.getByRole("button", { name: "Update problem 2" }).getAttribute("aria-pressed"),
    ).toBe("true");
    expect(screen.getByRole("button", { name: "Degraded 3" }).getAttribute("aria-pressed")).toBe(
      "false",
    );
    fireEvent.click(screen.getByRole("button", { name: "Degraded 3" }));
    expect(changes).toEqual([{ status: "degraded,problem" }]);
    fireEvent.click(screen.getByRole("button", { name: "Update problem 2" }));
    expect(changes.at(-1)).toEqual({ status: undefined });
  });

  it("shows Clear filters only while a search or a chip is set, and clears both", async () => {
    mount();
    await screen.findByText("web");
    expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
    cleanup();
    mount({ q: "web", status: "degraded" });
    await screen.findByText("web");
    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(changes).toEqual([{ q: undefined, status: undefined }]);
  });

  it("counts the matches against the whole list once a filter is set", async () => {
    answerList = () => Response.json(listBody(items.slice(0, 2), { unfilteredTotal: 3, total: 2 }));
    mount({ q: "a" });
    expect(await screen.findByText("2 of 3 services")).toBeTruthy();
    expect(screen.queryByText("3 services")).toBeNull();
  });
});

describe("the services list: sorting", () => {
  it("makes Name, State and Updated sortable with aria-sort, and Name ascending the default", async () => {
    mount();
    await screen.findByText("web");
    const header = (name: string) => screen.getByRole("columnheader", { name });
    expect(header("Name").getAttribute("aria-sort")).toBe("ascending");
    expect(header("State").getAttribute("aria-sort")).toBe("none");
    expect(header("Updated").getAttribute("aria-sort")).toBe("none");
    expect(header("Mode").getAttribute("aria-sort")).toBeNull();
    expect(header("Ports").getAttribute("aria-sort")).toBeNull();
  });

  it("reports a sort and flips the direction of the column that is sorted", async () => {
    mount();
    await screen.findByText("web");
    fireEvent.click(
      within(screen.getByRole("columnheader", { name: "State" })).getByRole("button"),
    );
    expect(changes).toEqual([{ sort: "state", dir: "asc" }]);
    cleanup();
    changes = [];
    mount({ sort: "updated", dir: "asc" });
    await screen.findByText("web");
    fireEvent.click(
      within(screen.getByRole("columnheader", { name: "Updated" })).getByRole("button"),
    );
    expect(changes).toEqual([{ sort: "updated", dir: "desc" }]);
  });

  it("orders the rows by the sort of the URL, descending when asked", async () => {
    mount({ group: "none", sort: "name", dir: "desc" });
    await screen.findByText("web");
    const rows = names();
    const at = (name: string) => rows.findIndex((r) => r.startsWith(name));
    expect(at("web")).toBeLessThan(at("tierplan-agent"));
    expect(at("tierplan-agent")).toBeLessThan(at("backup"));
  });
});

describe("the services list: columns", () => {
  it("shows the first two published ports as 8080→80/tcp, then +n with all of them in a tooltip", async () => {
    items = [
      svc({ ports: ["8080->80/tcp", "8443->443/tcp", "9000->9000/udp (host)"] }),
      svc({ id: "id-one", name: "one", ports: ["53->53/udp"] }),
      svc({ id: "id-none", name: "none" }),
    ];
    mount();
    await screen.findByText("web");
    expect(screen.getByRole("columnheader", { name: "Ports" })).toBeTruthy();
    const web = screen.getByRole("row", { name: /^Select web/ });
    expect(web.textContent).toContain("8080→80/tcp, 8443→443/tcp");
    expect(web.textContent).not.toContain("9000");
    const more = within(web).getByText("+1");
    await act(async () => {
      fireEvent.focus(more);
    });
    expect((await screen.findAllByText(/9000→9000\/udp \(host\)/)).length).toBeGreaterThan(0);
    const one = screen.getByRole("row", { name: /^Select one/ });
    expect(one.textContent).toContain("53→53/udp");
    expect(within(one).queryByText(/^\+\d/)).toBeNull();
    const none = screen.getByRole("row", { name: /^Select none/ });
    expect(within(none).getAllByText("—").length).toBeGreaterThan(0);
  });

  it("marks TierPlan's agent service with the TierPlan agent badge", async () => {
    mount();
    await screen.findByText("web");
    expect(
      within(screen.getByRole("row", { name: /^Select tierplan-agent/ })).getByText(
        "TierPlan agent",
      ),
    ).toBeTruthy();
    expect(
      within(screen.getByRole("row", { name: /^Select web/ })).queryByText("TierPlan agent"),
    ).toBeNull();
  });

  it("reads a global service's replicas as running/desired and shows — for an unknown mode", async () => {
    items = [
      svc({ id: "id-g", name: "gl", mode: "global", running: 2, desired: 3 }),
      svc({ id: "id-x", name: "mystery", mode: "mystery" as ServiceItem["mode"] }),
    ];
    mount();
    await screen.findByText("gl");
    expect(within(screen.getByRole("row", { name: /^Select gl/ })).getByText("2/3")).toBeTruthy();
    const cells = within(screen.getByRole("row", { name: /^Select mystery/ })).getAllByRole("cell");
    // Select, Name, State, Mode
    expect(cells[3]?.textContent).toBe("—");
  });

  it("keeps the Name column at least 224px wide and cuts a long name with the whole of it in the text", async () => {
    const long = "a-very-long-service-name-".repeat(6);
    items = [svc({ name: long })];
    mount();
    await screen.findByText(long);
    expect(screen.getByRole("columnheader", { name: "Name" }).className).toContain("min-w-[224px]");
  });
});

describe("the services list: states", () => {
  it("shows 8 skeleton rows and the toolbar at once while loading", async () => {
    answerList = () => new Promise<Response>(() => {});
    mount();
    expect(await screen.findByRole("searchbox", { name: "Search services" })).toBeTruthy();
    expect(screen.getByRole("group", { name: "Filter by status" })).toBeTruthy();
    // The header row and eight placeholders.
    expect(screen.getAllByRole("row")).toHaveLength(9);
    expect(screen.getByRole("table").getAttribute("aria-busy")).toBe("true");
  });

  it("shows the list error with Retry loading services, and loads again on press", async () => {
    answerList = () => Response.json({ title: "Bad", status: 500 }, { status: 500 });
    mount();
    expect(await screen.findByText("Couldn't load services.")).toBeTruthy();
    expect(screen.getByText("Docker may be unreachable. Try again.")).toBeTruthy();
    answerList = undefined;
    fireEvent.click(screen.getByRole("button", { name: "Retry loading services" }));
    expect(await screen.findByText("web")).toBeTruthy();
    expect(screen.queryByText("Couldn't load services.")).toBeNull();
  });

  it("says the update checks cannot be read when the filter by update is refused, not that Docker is unreachable", async () => {
    answerList = (url) =>
      url.searchParams.get("updates")
        ? Response.json(
            { type: "update_checks_unavailable", title: "Service Unavailable", status: 503 },
            { status: 503 },
          )
        : Response.json(listBody(items, listExtra));
    mount({ updates: "available" });
    expect(await screen.findByText("Couldn't load services.")).toBeTruthy();
    expect(screen.getByText(/Update checks can't be read right now/)).toBeTruthy();
    expect(screen.queryByText("Docker may be unreachable. Try again.")).toBeNull();
    // It is not an empty list: no "nothing matches" state stands beside the notice.
    expect(screen.queryByText(/No services/)).toBeNull();
    expect(screen.getByRole("button", { name: "Retry loading services" })).toBeTruthy();
  });

  it("shows the no-manager banner above the rows last read", async () => {
    noManager = true;
    mount();
    expect(await screen.findByText("No manager is connected")).toBeTruthy();
    expect(screen.getByText("web")).toBeTruthy();
    expect(screen.getByRole("searchbox", { name: "Search services" })).toBeTruthy();
  });

  it("says Showing the first 500 of n when the server capped the list", async () => {
    listExtra = { capped: true, total: 640, unfilteredTotal: 640 };
    mount();
    await screen.findByText("web");
    expect(
      screen.getByText("Showing the first 500 of 640. Narrow the search to see the rest."),
    ).toBeTruthy();
    cleanup();
    client.clear();
    listExtra = {};
    mount();
    await screen.findByText("web");
    expect(screen.queryByText(/Showing the first 500/)).toBeNull();
  });

  it("counts one service in the singular", async () => {
    items = [svc()];
    mount();
    expect(await screen.findByText("1 service")).toBeTruthy();
  });

  describe("when there is nothing to show", () => {
    beforeEach(() => {
      items = [];
    });

    it("says there are no services and what to do, to someone who can create one", async () => {
      me = makeMe({
        permissions: { global: [], environments: { "*": ["services.read", "services.manage"] } },
      });
      mount();
      expect(await screen.findByRole("heading", { name: "No services" })).toBeTruthy();
      expect(
        screen.getByText(
          "Services run your containers across the swarm. Create one, or deploy a stack that defines them.",
        ),
      ).toBeTruthy();
    });

    it("says nothing to show on the environment, to someone who cannot", async () => {
      mount();
      expect(await screen.findByRole("heading", { name: "No services" })).toBeTruthy();
      expect(await screen.findByText("Nothing to show on Swarm right now.")).toBeTruthy();
      expect(screen.queryByText(/Create one/)).toBeNull();
    });

    it("says no services match these filters, with Clear filters, when a chip is set", async () => {
      mount({ status: "degraded" });
      expect(
        await screen.findByRole("heading", { name: "No services match these filters." }),
      ).toBeTruthy();
      const empty = screen
        .getByRole("heading", { name: "No services match these filters." })
        .closest("[data-slot=empty]") as HTMLElement;
      fireEvent.click(within(empty).getByRole("button", { name: "Clear filters" }));
      expect(changes).toEqual([{ q: undefined, status: undefined }]);
    });

    it("says no services match the search, with Clear search, when only a search is set", async () => {
      mount({ q: "zzz" });
      expect(await screen.findByRole("heading", { name: "No services match “zzz”." })).toBeTruthy();
      const empty = screen
        .getByRole("heading", { name: "No services match “zzz”." })
        .closest("[data-slot=empty]") as HTMLElement;
      fireEvent.click(within(empty).getByRole("button", { name: "Clear search" }));
      expect(changes).toEqual([{ q: undefined }]);
    });

    it("writes the search as text, whatever it holds", async () => {
      mount({ q: "<img src=x onerror=alert(1)>" });
      expect(await screen.findByRole("heading", { name: /^No services match/ })).toBeTruthy();
      expect(document.querySelector("img")).toBeNull();
    });
  });
});

describe("the services list: Create service (06-27)", () => {
  const adminMe = () => makeMe({ envs: { [ENV]: "admin" } });
  const create = () => screen.getAllByRole("link", { name: "Create service" });

  it("is a link to the create page for an Env Admin", async () => {
    me = adminMe();
    mount();
    await screen.findByText("web");
    const link = await screen.findByRole("link", { name: "Create service" });
    expect(link.getAttribute("href")).toBe(`/env/${ENV}/services/new`);
  });

  it("is shown disabled with Needs Admin to an Operator, and goes nowhere", async () => {
    mount();
    await screen.findByText("web");
    const button = await screen.findByRole("button", { name: "Create service" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    expect(document.body.textContent).toContain("Needs Admin");
    expect(screen.queryByRole("link", { name: "Create service" })).toBeNull();
    fireEvent.click(button);
  });

  it("is disabled with the reason when no manager is connected", async () => {
    me = adminMe();
    noManager = true;
    mount();
    await screen.findByText("web");
    const button = await screen.findByRole("button", { name: "Create service" });
    await waitFor(() => expect(button.getAttribute("aria-disabled")).toBe("true"));
    expect(document.body.textContent).toContain(
      "No manager is connected, so swarm changes are off.",
    );
    expect(screen.queryByRole("link", { name: "Create service" })).toBeNull();
  });

  it("is offered again in the empty state, to someone who can create", async () => {
    me = adminMe();
    items = [];
    mount();
    expect(await screen.findByRole("heading", { name: "No services" })).toBeTruthy();
    await waitFor(() => expect(create()).toHaveLength(2));
    for (const link of create()) expect(link.getAttribute("href")).toBe(`/env/${ENV}/services/new`);
  });

  it("is not offered in the empty state to someone who cannot", async () => {
    items = [];
    mount();
    expect(await screen.findByRole("heading", { name: "No services" })).toBeTruthy();
    await screen.findByText("Nothing to show on Swarm right now.");
    expect(screen.queryByRole("link", { name: "Create service" })).toBeNull();
    expect(screen.getAllByRole("button", { name: "Create service" })).toHaveLength(1);
  });
});

describe("the services list: live updates", () => {
  it("updates a changed row in place and highlights it", async () => {
    mount();
    await screen.findByText("web");
    items = [svc({ running: 1, desired: 2 }), ...items.slice(1)];
    await act(async () => {
      await client.invalidateQueries({ queryKey: ["env", ENV, "services"] });
    });
    const row = await waitFor(() => {
      const r = screen.getByRole("row", { name: /^Select web/ });
      expect(within(r).getByText("1/2")).toBeTruthy();
      return r;
    });
    expect(row.getAttribute("data-changed")).toBe("true");
  });

  it("holds a new row behind the changes-waiting pill while a row menu is open, and shows it when the menu closes", async () => {
    mount();
    await openMenu("web");
    expect(await screen.findByRole("menuitem", { name: "Scale…" })).toBeTruthy();
    items = [...items, svc({ id: "id-new", name: "newcomer" })];
    await act(async () => {
      await client.invalidateQueries({ queryKey: ["env", ENV, "services"] });
    });
    expect(await screen.findByText("1 change waiting")).toBeTruthy();
    expect(screen.queryByText("newcomer")).toBeNull();
    fireEvent.keyDown(screen.getByRole("menuitem", { name: "Scale…" }), { key: "Escape" });
    expect(await screen.findByText("newcomer")).toBeTruthy();
    await waitFor(() => expect(screen.queryByText("1 change waiting")).toBeNull());
  });
});

describe("the services list: selection and bulk scale", () => {
  const STACKED = () => [
    svc({ id: "id-web", name: "shop_web", stack: "shop", desired: 2 }),
    svc({ id: "id-db", name: "shop_db", stack: "shop", desired: 1, running: 1 }),
    svc({ id: "id-blog", name: "blog_app", stack: "Blog", desired: 3, running: 3 }),
    svc({ id: "id-agent", name: "tierplan-agent", mode: "global", running: 3, desired: 3 }),
    svc({ id: "id-backup", name: "backup", mode: "replicated-job", running: 0, desired: 0 }),
  ];

  const box = (name: string) => screen.getByRole("checkbox", { name: `Select ${name}` });
  const isOn = (el: HTMLElement) =>
    el.getAttribute("aria-checked") ?? el.getAttribute("data-state");
  const bar = () => screen.getByRole("toolbar", { name: "Bulk actions" });

  async function mountReady() {
    items = STACKED();
    mount();
    await screen.findByText("shop_web");
    await waitFor(() => expect(sent.some((s) => s.path === "/api/me")).toBe(true));
    await act(async () => {});
  }

  it("gives every row a named checkbox, the header one to select all services and each group one of its own", async () => {
    await mountReady();
    for (const name of ["shop_web", "shop_db", "blog_app", "tierplan-agent", "backup"]) {
      expect(box(name)).toBeTruthy();
    }
    expect(screen.getByRole("checkbox", { name: "Select all services" })).toBeTruthy();
    expect(screen.getByRole("checkbox", { name: "Select shop" })).toBeTruthy();
    expect(screen.queryByRole("toolbar", { name: "Bulk actions" })).toBeNull();
  });

  it("shows the bulk bar with '{n} selected', Scale… and Clear selection, and clears on request", async () => {
    await mountReady();
    fireEvent.click(box("shop_web"));
    fireEvent.click(box("blog_app"));
    expect(within(bar()).getByText("2 selected")).toBeTruthy();
    expect(within(bar()).getByRole("button", { name: "Scale…" })).toBeTruthy();
    fireEvent.click(within(bar()).getByRole("button", { name: "Clear selection" }));
    expect(screen.queryByRole("toolbar", { name: "Bulk actions" })).toBeNull();
    expect(isOn(box("shop_web"))).toBe("false");
  });

  it("selects a whole group from its checkbox and every row from the header", async () => {
    await mountReady();
    const group = screen.getByRole("checkbox", { name: "Select shop" });
    fireEvent.click(group);
    expect(isOn(box("shop_web"))).toBe("true");
    expect(isOn(box("shop_db"))).toBe("true");
    expect(isOn(box("blog_app"))).toBe("false");
    fireEvent.click(screen.getByRole("checkbox", { name: "Select all services" }));
    expect(within(bar()).getByText("5 selected")).toBeTruthy();
  });

  it("offers 'Select all {n}' only when every visible row is selected and folded groups hold more", async () => {
    await mountReady();
    fireEvent.click(screen.getByRole("button", { name: "Collapse shop" }));
    expect(screen.queryByText("shop_web")).toBeNull();
    fireEvent.click(screen.getByRole("checkbox", { name: "Select all services" }));
    expect(within(bar()).getByText("3 selected")).toBeTruthy();
    fireEvent.click(within(bar()).getByRole("button", { name: "Select all 5" }));
    expect(within(bar()).getByText("5 selected")).toBeTruthy();
    expect(within(bar()).queryByRole("button", { name: /^Select all/ })).toBeNull();
  });

  it("does not offer 'Select all {n}' when nothing is folded away", async () => {
    await mountReady();
    fireEvent.click(screen.getByRole("checkbox", { name: "Select all services" }));
    expect(within(bar()).queryByRole("button", { name: /^Select all/ })).toBeNull();
  });

  it("disables Scale… with 'None of the selected services can be scaled.' when nothing selected is replicated", async () => {
    await mountReady();
    fireEvent.click(box("tierplan-agent"));
    fireEvent.click(box("backup"));
    const scale = within(bar()).getByRole("button", { name: "Scale…" });
    expect(scale.getAttribute("aria-disabled")).toBe("true");
    expect(
      screen.getAllByText("None of the selected services can be scaled.").length,
    ).toBeGreaterThan(0);
    fireEvent.click(scale);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(posts()).toEqual([]);
  });

  it("disables Scale… with Needs Operator for a viewer", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    await mountReady();
    fireEvent.click(box("shop_web"));
    expect(
      within(bar()).getByRole("button", { name: "Scale…" }).getAttribute("aria-disabled"),
    ).toBe("true");
    expect(screen.getAllByText("Needs Operator").length).toBeGreaterThan(0);
  });

  it("opens the bulk dialog with every selected row, and the mixed ones skipped with their notes", async () => {
    await mountReady();
    fireEvent.click(screen.getByRole("checkbox", { name: "Select all services" }));
    fireEvent.click(within(bar()).getByRole("button", { name: "Scale…" }));
    const dialog = await screen.findByRole("dialog", { name: "Scale 5 services" });
    expect(within(dialog).getByRole("button", { name: "Scale 3 services" })).toBeTruthy();
    expect(within(dialog).getByText("Global services can't be scaled.")).toBeTruthy();
    expect(within(dialog).getByText("Jobs can't be scaled here.")).toBeTruthy();
    expect(within(dialog).getAllByText("Part of stack shop")).toHaveLength(2);
    expect(posts()).toEqual([]);
  });

  it("scales the eligible rows in one request, marks them Scaling… with a disabled checkbox and menu, and releases them when the list shows the counts", async () => {
    answerScale = () =>
      Response.json({
        results: [
          {
            serviceId: "id-web",
            name: "shop_web",
            result: "done",
            previous: 2,
            replicas: 4,
            changed: true,
          },
          {
            serviceId: "id-db",
            name: "shop_db",
            result: "done",
            previous: 1,
            replicas: 4,
            changed: true,
          },
          {
            serviceId: "id-blog",
            name: "blog_app",
            result: "done",
            previous: 3,
            replicas: 4,
            changed: true,
          },
        ],
      });
    await mountReady();
    fireEvent.click(screen.getByRole("checkbox", { name: "Select all services" }));
    fireEvent.click(within(bar()).getByRole("button", { name: "Scale…" }));
    const dialog = await screen.findByRole("dialog", { name: "Scale 5 services" });
    fireEvent.change(within(dialog).getByRole("textbox", { name: "Set all to" }), {
      target: { value: "4" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Scale 3 services" }));
    await waitFor(() => expect(posts()).toHaveLength(1));
    const body = posts()[0]?.body as { bulkId: string; targets: unknown[] };
    expect(body.targets).toEqual([
      { serviceId: "id-blog", replicas: 4 },
      { serviceId: "id-db", replicas: 4 },
      { serviceId: "id-web", replicas: 4 },
    ]);
    expect(body.bulkId).toBeTruthy();
    expect(await screen.findByText(/Scaled 3 services\./)).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    // The selection is spent and the three rows wait for their counts.
    expect(screen.queryByRole("toolbar", { name: "Bulk actions" })).toBeNull();
    for (const name of ["shop_web", "shop_db", "blog_app"]) {
      const row = screen.getByRole("row", { name: new RegExp(`^Select ${name}`) });
      expect(within(row).getByText("Scaling…")).toBeTruthy();
      expect(row.querySelector("[data-pending]")).toBeTruthy();
      expect(
        within(row)
          .getByRole("checkbox", { name: `Select ${name}` })
          .hasAttribute("disabled"),
      ).toBe(true);
      expect(
        (within(row).getByRole("button", { name: `Actions for ${name}` }) as HTMLButtonElement)
          .disabled,
      ).toBe(true);
    }
    expect(
      within(screen.getByRole("row", { name: /^Select backup/ })).queryByText("Scaling…"),
    ).toBeNull();
    // The list shows the counts: the rows are released.
    items = items.map((i) =>
      ["id-web", "id-db", "id-blog"].includes(i.id) ? { ...i, desired: 4, running: 4 } : i,
    );
    await act(async () => {
      await client.invalidateQueries({ queryKey: ["env", ENV, "services"] });
    });
    await waitFor(() => expect(screen.queryByText("Scaling…")).toBeNull());
  });

  it("releases a Scaling… row after 30 seconds if the list never shows the count", async () => {
    // The 30 seconds are a timer of the component's own, set when the scale is answered. The clock
    // is fully fake from the click on, so only the explicit advance below can reach them; under it
    // the test waits with vi.waitFor, because Testing Library's waitFor and findBy poll on the
    // faked timers and so never poll.
    try {
      await mountReady();
      fireEvent.click(box("shop_web"));
      fireEvent.click(box("blog_app"));
      fireEvent.click(within(bar()).getByRole("button", { name: "Scale…" }));
      const dialog = await screen.findByRole("dialog", { name: "Scale 2 services" });
      answerScale = () =>
        Response.json({
          results: [
            {
              serviceId: "id-web",
              name: "shop_web",
              result: "done",
              previous: 2,
              replicas: 9,
              changed: true,
            },
            {
              serviceId: "id-blog",
              name: "blog_app",
              result: "done",
              previous: 3,
              replicas: 9,
              changed: true,
            },
          ],
        });
      fireEvent.change(within(dialog).getByRole("textbox", { name: "Set all to" }), {
        target: { value: "9" },
      });
      vi.useFakeTimers();
      fireEvent.click(within(dialog).getByRole("button", { name: "Scale 2 services" }));
      await waitUnderFakeClock(() => expect(screen.getAllByText("Scaling…").length).toBe(2));
      await act(async () => {
        vi.advanceTimersByTime(30_001);
      });
      await waitUnderFakeClock(() => expect(screen.queryByText("Scaling…")).toBeNull());
    } finally {
      vi.useRealTimers();
    }
  });

  it("opens BulkResultDialog from a partial failure's Details with Service, Result and Reason", async () => {
    answerScale = () =>
      Response.json({
        results: [
          {
            serviceId: "id-web",
            name: "shop_web",
            result: "done",
            previous: 2,
            replicas: 4,
            changed: true,
          },
          {
            serviceId: "id-blog",
            name: "blog_app",
            result: "failed",
            code: "service_changed",
            replicas: 4,
            changed: false,
          },
        ],
      });
    await mountReady();
    fireEvent.click(box("shop_web"));
    fireEvent.click(box("blog_app"));
    fireEvent.click(box("backup"));
    fireEvent.click(within(bar()).getByRole("button", { name: "Scale…" }));
    const dialog = await screen.findByRole("dialog", { name: "Scale 3 services" });
    fireEvent.change(within(dialog).getByRole("textbox", { name: "Set all to" }), {
      target: { value: "4" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Scale 2 services" }));
    expect(await screen.findByText(/Scaled 1, 1 failed/)).toBeTruthy();
    fireEvent.click(await screen.findByRole("button", { name: "Details" }));
    const results = await screen.findByRole("dialog", { name: "Scale results" });
    expect(
      within(results)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Service", "Result", "Reason"]);
    const rows = within(results).getAllByRole("row").slice(1);
    expect(rows.map((r) => Array.from(r.querySelectorAll("td")).map((c) => c.textContent))).toEqual(
      [
        ["backup", "Skipped", "Jobs can't be scaled here."],
        ["blog_app", "Failed", expect.stringContaining("changed by someone else")],
        ["shop_web", "Done", "—"],
      ],
    );
  });
});

describe("the services list: Live stats (06-33)", () => {
  beforeEach(() => {
    FakeEventSource.reset();
    vi.stubGlobal("EventSource", FakeEventSource);
  });

  const liveSource = (): FakeEventSource => {
    const s = FakeEventSource.instances[FakeEventSource.instances.length - 1];
    if (!s) throw new Error("no stats stream was opened");
    return s;
  };
  const idsOf = (s: FakeEventSource) =>
    (new URL(s.url, "http://x").searchParams.get("ids") ?? "").split(",").sort();

  const entry = (id: string, extra: Record<string, unknown> = {}) => ({
    id,
    cpu: 142.4,
    mem: 1.2 * 1024 ** 3,
    netRx: 1.2 * 1024 ** 2,
    netTx: 340 * 1024,
    counted: 2,
    silent: 0,
    limitBytes: null,
    ...extra,
  });

  it("has the switch, off by default, with no stats columns and no stream", async () => {
    mount();
    await screen.findByText("web");
    expect(screen.getByRole("switch", { name: "Live stats" }).getAttribute("aria-checked")).toBe(
      "false",
    );
    for (const name of ["CPU", "Memory", "Network"]) {
      expect(screen.queryByRole("columnheader", { name })).toBeNull();
    }
    expect(FakeEventSource.instances).toHaveLength(0);
  });

  it("asks for stats through the search when the switch is turned on", async () => {
    mount();
    await screen.findByText("web");
    fireEvent.click(screen.getByRole("switch", { name: "Live stats" }));
    expect(changes).toEqual([{ stats: 1 }]);
  });

  it("takes the switch out of the search when it is turned off", async () => {
    mount({ stats: 1 });
    await screen.findByText("web");
    fireEvent.click(screen.getByRole("switch", { name: "Live stats" }));
    expect(changes).toEqual([{ stats: undefined }]);
  });

  it("adds CPU, Memory and Network after Updated, and one stream for the rows on screen", async () => {
    mount({ stats: 1 });
    await screen.findByText("web");
    const heads = screen.getAllByRole("columnheader").map((h) => h.textContent);
    const updated = heads.indexOf("Updated");
    expect(heads.slice(updated, updated + 4)).toEqual(["Updated", "CPU", "Memory", "Network"]);
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(new URL(liveSource().url, "http://x").pathname).toBe(
      `/api/env/${ENV}/services/stats/stream`,
    );
    expect(idsOf(liveSource())).toEqual(["id-agent", "id-backup", "id-web"]);
  });

  it("shows — while waiting and the numbers once the stream has them", async () => {
    mount({ stats: 1 });
    await screen.findByText("web");
    const row = () => screen.getByRole("row", { name: /^Select web/ });
    expect(within(row()).getAllByText("—").length).toBeGreaterThanOrEqual(3);
    act(() => liveSource().sendFrame("sample", { t: 1, services: [entry("id-web")] }));
    expect(within(row()).getByText("142%")).toBeTruthy();
    expect(within(row()).getByText("1.2 GiB")).toBeTruthy();
    expect(within(row()).getByText("↓ 1.2 MiB/s ↑ 340 KiB/s")).toBeTruthy();
    // The other rows were not in the round and still wait.
    const agent = screen.getByRole("row", { name: /^Select tierplan-agent/ });
    expect(within(agent).queryByText("142%")).toBeNull();
  });

  it("marks a total that leaves out tasks on nodes that are not reporting", async () => {
    mount({ stats: 1 });
    await screen.findByText("web");
    act(() =>
      liveSource().sendFrame("sample", { t: 1, services: [entry("id-web", { silent: 1 })] }),
    );
    const row = screen.getByRole("row", { name: /^Select web/ });
    expect(
      within(row).getByRole("img", { name: "Not counting 1 task on a node that isn't reporting." }),
    ).toBeTruthy();
  });

  it("streams only the rows of groups that are open, and nothing when every group is folded", async () => {
    items = [
      svc({ id: "id-web", name: "shop_web", stack: "shop" }),
      svc({ id: "id-loose", name: "loose" }),
    ];
    mount({ stats: 1 });
    await screen.findByText("shop_web");
    expect(idsOf(liveSource())).toEqual(["id-loose", "id-web"]);
    const first = liveSource();
    act(() => liveSource().sendFrame("sample", { t: 1, services: [entry("id-web")] }));
    fireEvent.click(screen.getByRole("button", { name: "Collapse all groups" }));
    // Both groups are folded: nothing is on screen, so the stream closes at once.
    expect(first.closed).toBe(true);
  });

  it("closes the stream when the list is left", async () => {
    mount({ stats: 1 });
    await screen.findByText("web");
    const s = liveSource();
    cleanup();
    expect(s.closed).toBe(true);
  });
});

const update = (extra: Partial<components["schemas"]["UpdateInfoBody"]> = {}) => ({
  state: "not_checked" as const,
  reason: "" as const,
  registry: "registry.example.com",
  repository: "acme/web",
  tag: "1.4",
  runningDigest: `sha256:${"a".repeat(64)}`,
  remoteDigest: "",
  changedPlatforms: [],
  checkedAt: "",
  successAt: "",
  note: "" as const,
  task: false,
  ...extra,
});

describe("the services list: image updates (12-04)", () => {
  const withUpdates = () => {
    items = [
      svc({
        update: update({
          state: "update_available",
          remoteDigest: `sha256:${"b".repeat(64)}`,
          checkedAt: new Date().toISOString(),
        }),
      }),
      svc({
        id: "id-api",
        name: "api",
        update: update({
          state: "check_failed",
          reason: "auth",
          checkedAt: new Date().toISOString(),
        }),
      }),
      svc({ id: "id-db", name: "db" }),
    ];
  };

  it("says each service's update state in the Updates column, and a dash when the server sent none", async () => {
    withUpdates();
    mount();
    await screen.findByText("web");
    const web = screen.getByRole("row", { name: /^Select web/ });
    expect(within(web).getByText("Update available")).toBeTruthy();
    const api = screen.getByRole("row", { name: /^Select api/ });
    expect(within(api).getByText("Check failed")).toBeTruthy();
    // The browser never reads a missing or failed check as up to date.
    expect(screen.queryByText("Up to date")).toBeNull();
    const db = screen.getByRole("row", { name: /^Select db/ });
    expect(within(db).getAllByText("—", { selector: "span" }).length).toBeGreaterThan(0);
  });

  it("puts the failure reason behind the badge by aria-describedby, in TierPlan's words", async () => {
    withUpdates();
    mount();
    await screen.findByText("web");
    const api = screen.getByRole("row", { name: /^Select api/ });
    const badge = within(api).getByText("Check failed").closest("[aria-describedby]");
    const id = badge?.getAttribute("aria-describedby") ?? "";
    const text = document.getElementById(id)?.textContent ?? "";
    expect(text).toContain("registry.example.com refused TierPlan's credentials.");
    expect(text).toContain("It has never succeeded.");
  });

  it("asks the registries again from Check now, and says what it found", async () => {
    withUpdates();
    mount();
    await openMenu("web");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Check now" }));
    await waitFor(() =>
      expect(
        sent.some((s) => s.method === "POST" && s.path === `/api/env/${ENV}/updates/check`),
      ).toBe(true),
    );
    expect(await screen.findByText("Checked 3 images. 1 update available.")).toBeTruthy();
    // The lists are read again, since each carries the update objects.
    await waitFor(() => expect(listRequests().length).toBeGreaterThan(1));
  });

  it("shows Check now disabled with the reason to a viewer, and sends nothing", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    mount();
    await openMenu("web");
    const item = await screen.findByRole("menuitem", { name: /^Check now/ });
    expect(item.getAttribute("aria-disabled")).toBe("true");
    expect(screen.getAllByText("Needs Operator on Swarm").length).toBeGreaterThan(0);
    fireEvent.click(item);
    expect(sent.some((s) => s.path.endsWith("/updates/check"))).toBe(false);
  });

  it("toasts a plain failure when Check now fails", async () => {
    answerCheck = () => Response.json({ status: 500 }, { status: 500 });
    mount();
    await openMenu("web");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Check now" }));
    expect(await screen.findByText("Couldn't check for updates. Try again.")).toBeTruthy();
  });
});

describe("service list: the Updates available filter (12-10)", () => {
  it("sets updates=available in the search when pressed, and takes it out when pressed again", async () => {
    mount();
    await screen.findByText("web");
    const toggle = screen.getByRole("button", { name: "Updates available" });
    expect(toggle.getAttribute("aria-pressed")).toBe("false");
    fireEvent.click(toggle);
    expect(changes).toEqual([{ updates: "available" }]);
    cleanup();
    changes = [];
    mount({ updates: "available" });
    await screen.findByText("web");
    const pressed = screen.getByRole("button", { name: "Updates available" });
    expect(pressed.getAttribute("aria-pressed")).toBe("true");
    fireEvent.click(pressed);
    expect(changes).toHaveLength(1);
    expect(changes[0]).toHaveProperty("updates", undefined);
  });

  it("asks the server for updates=available, with the chips and the search, before its cap", async () => {
    mount({ updates: "available", status: "degraded" });
    await screen.findByText("web");
    await waitFor(() => expect(listRequests()).toContain("?status=degraded&updates=available"));
  });

  it("counts as a filter: Clear filters shows, and resets it", async () => {
    mount({ updates: "available" });
    await screen.findByText("web");
    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(changes).toHaveLength(1);
    expect(changes[0]).toHaveProperty("updates", undefined);
  });

  it("says no services match these filters when the toggle alone leaves nothing", async () => {
    answerList = () => Response.json(listBody([], { unfilteredTotal: 3, total: 0 }));
    mount({ updates: "available" });
    expect(
      await screen.findByRole("heading", { name: "No services match these filters." }),
    ).toBeTruthy();
  });
});
