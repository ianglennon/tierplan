// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { downloadUrl } from "@/features/logs/download-popover";
import { stackSource, unmanagedStackSource } from "@/features/logs/log-source";
import { UnmanagedLogs } from "@/features/stack-unmanaged/unmanaged-logs";
import { UnmanagedOverview } from "@/features/stack-unmanaged/unmanaged-overview";
import { UnmanagedResources } from "@/features/stack-unmanaged/unmanaged-resources";
import { StackDetailLayout } from "@/features/stacks/stack-detail-layout";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The read-only page of a stack Coaming did not make (plan 08-09, STK-12, D-09, D-10, D-11, D-19).

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

const ENV = "e1";

type Service = {
  id: string;
  name: string;
  short: string;
  mode: string;
  state: string;
  updateState: string;
  running: number;
  desired: number;
  image: string;
  pinned: string;
  pinState: string;
  ports: string[];
  containers: { id: string; name: string; state: string; health: string }[];
};

const service = (short: string, over: Partial<Service> = {}): Service => ({
  id: `svc-${short}`,
  name: `legacy_${short}`,
  short,
  mode: "replicated",
  state: "running",
  updateState: "",
  running: 1,
  desired: 1,
  image: "nginx:1.27",
  pinned: "nginx:1.27@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  pinState: "pinned",
  ports: [],
  containers: [],
  ...over,
});

let role: "viewer" | "operator" | "admin";
let body: Record<string, unknown>;
let resources: Record<string, unknown>;
let notFound: { managedStackId?: string } | null;
// What the stack's own read does: answer, fail with a 500, or wait for release() before it answers.
let failing: boolean;
let held: Promise<void> | null;
let client: QueryClient;
// The name of the stack the fake engine holds, as the address carries it.
let served = "legacy";

beforeEach(() => {
  role = "viewer";
  toasts.info.mockReset();
  body = {
    name: "legacy",
    target: "swarm",
    state: "degraded",
    servicesRunning: 1,
    servicesTotal: 2,
    services: [
      service("app", { ports: ["8080->80/tcp"] }),
      service("db", { running: 0, state: "not_running" }),
    ],
    foundBy: { label: "com.docker.stack.namespace", value: "legacy" },
    orphan: false,
    portainer: false,
    owner: "",
    adoptable: true,
    reason: "",
  };
  resources = {
    tasks: { items: [], error: "" },
    networks: {
      items: [
        { id: "n1", name: "legacy_default", driver: "overlay", scope: "swarm", ownership: "stack" },
        { id: "n2", name: "shared", driver: "overlay", scope: "swarm", ownership: "external" },
      ],
      error: "",
    },
    volumes: {
      items: [
        {
          name: "legacy_data",
          driver: "local",
          node: "mgr-1",
          nodeId: "node-1",
          ownership: "stack",
        },
        {
          name: "shared-vol",
          driver: "local",
          node: "mgr-1",
          nodeId: "node-1",
          ownership: "external",
        },
      ],
      error: "",
      notReporting: [],
    },
    secrets: {
      items: [
        {
          id: "sec1",
          name: "legacy_pw",
          fullName: "legacy_pw",
          hash: "",
          usedBy: [],
          ownership: "external",
        },
      ],
      error: "",
    },
    configs: { items: [], error: "" },
  };
  notFound = null;
  served = "legacy";
  failing = false;
  held = null;
  client = new QueryClient({ defaultOptions: { queries: { retry: false, retryDelay: 0 } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: role } }));
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/unmanaged-stacks/${encodeURIComponent(served)}`) {
        if (held) await held;
        if (failing) return Response.json({ title: "x", status: 500 }, { status: 500 });
        if (notFound) {
          return Response.json(
            {
              type: "unmanaged_stack_not_found",
              title: "Not Found",
              status: 404,
              detail: "gone",
              errors: notFound.managedStackId
                ? [{ location: "managedStackId", message: notFound.managedStackId }]
                : undefined,
            },
            { status: 404 },
          );
        }
        return Response.json(body);
      }
      if (path === `/api/env/${ENV}/unmanaged-stacks/${encodeURIComponent(served)}/resources`) {
        return Response.json(resources);
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(name = "legacy") {
  served = name;
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <StackDetailLayout variant="unmanaged" envId={ENV} name={name}>
            <UnmanagedOverview envId={ENV} name={name} />
          </StackDetailLayout>
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("the Unmanaged stack page: header and notes", () => {
  it("shows the name, its state, Swarm and Unmanaged, and the line saying Coaming does not manage it", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: "legacy", level: 2 })).toBeTruthy();
    expect(await screen.findByText("Degraded")).toBeTruthy();
    // The badge in the header and the Target row of the Details.
    expect(await screen.findAllByText("Swarm")).toHaveLength(2);
    // The badge in the header and the Source row of the Details.
    expect(await screen.findAllByText("Unmanaged")).toHaveLength(2);
    expect(await screen.findByText("Prod · Not managed by Coaming")).toBeTruthy();
    const crumb = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(within(crumb).getByRole("link", { name: "Stacks" }).getAttribute("href")).toBe(
      "/env/e1/stacks",
    );
  });

  it("always shows the note that Coaming doesn't manage the stack, as a status", async () => {
    mount();
    const note = (await screen.findByText("Coaming doesn't manage this stack")).closest(
      "[data-slot=alert]",
    );
    expect(note?.getAttribute("role")).toBe("status");
    expect(
      screen.getByText(
        "You can look at it here. Adopt it to deploy, update and roll it back with Coaming. Adopting changes nothing until you deploy.",
      ),
    ).toBeTruthy();
  });

  it("offers no Editor or History tab and no Deploy, Stop, Start or Remove action", async () => {
    mount();
    await screen.findByRole("heading", { name: "legacy", level: 2 });
    expect(screen.getAllByRole("tab").map((tab) => tab.textContent)).toEqual([
      "Overview",
      "Generated compose",
      "Resources",
      "Logs",
    ]);
    for (const name of [/deploy/i, /stop/i, /start/i, /remove/i, /edit/i]) {
      expect(screen.queryByRole("button", { name })).toBeNull();
      expect(screen.queryByRole("link", { name })).toBeNull();
    }
  });

  it("offers an Env Admin Adopt stack as a link to the Adopt page, with the line on removing", async () => {
    role = "admin";
    mount();

    const adopt = await screen.findByRole("link", { name: "Adopt stack" });
    expect(adopt.getAttribute("href")).toBe("/env/e1/unmanaged-stacks/legacy/adopt");
    expect(screen.getByText("To remove it, adopt it first.")).toBeTruthy();
  });

  it("shows an Operator Adopt stack off with Needs Admin, and opens nothing", async () => {
    role = "operator";
    mount();

    const adopt = await screen.findByRole("button", { name: "Adopt stack" });
    await waitFor(() => expect(adopt.getAttribute("aria-disabled")).toBe("true"));
    fireEvent.click(adopt);

    expect(toasts.info).toHaveBeenCalledWith("Needs Admin");
    expect(screen.queryByRole("link", { name: "Adopt stack" })).toBeNull();
  });

  it("shows Adopt stack off with the reason for Coaming's own stack, even to an Admin", async () => {
    role = "admin";
    body = { ...body, owner: "coaming", adoptable: false, reason: "This is Coaming's own stack." };
    mount();

    const adopt = await screen.findByRole("button", { name: "Adopt stack" });
    expect(adopt.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(adopt);

    expect(toasts.info).toHaveBeenCalledWith("This is Coaming's own stack.");
    expect(screen.queryByRole("link", { name: "Adopt stack" })).toBeNull();
  });

  it("adds the orphan note and the Portainer advice only when they apply", async () => {
    mount();
    await screen.findByText("Coaming doesn't manage this stack");
    expect(screen.queryByText("Created by Coaming (another instance or a lost record)")).toBeNull();
    expect(screen.queryByText("This stack looks like it was created by Portainer")).toBeNull();
    cleanup();
    body = { ...body, orphan: true, portainer: true };
    mount();
    expect(
      await screen.findByText("Created by Coaming (another instance or a lost record)"),
    ).toBeTruthy();
    const advice = screen
      .getByText("This stack looks like it was created by Portainer")
      .closest("[data-slot=alert]");
    // Advice is never an alert: nothing is wrong on the page.
    expect(advice?.getAttribute("role")).toBe("status");
    expect(
      within(advice as HTMLElement).getByText(
        "Remove it from Portainer without deleting it, or the two tools will fight over it. Coaming doesn't contact Portainer.",
      ),
    ).toBeTruthy();
  });
});

describe("the Unmanaged stack page: unusual names (D-48, D-53)", () => {
  it("opens a mixed-case name and says nothing about adopting when it can be adopted", async () => {
    body = { ...body, name: "MyApp", adoptable: true, reason: "" };
    mount("MyApp");
    expect(await screen.findByRole("heading", { name: "MyApp", level: 2 })).toBeTruthy();
    expect(screen.queryByText("Can't be adopted")).toBeNull();
  });

  it("opens a name with a space and a slash through an encoded address, and says why it can't be adopted", async () => {
    const reason =
      "Coaming can't adopt My App/x: a stack name on a swarm takes only letters, numbers, dash and underscore, starting with a letter or number, up to 61 characters.";
    body = { ...body, name: "My App/x", adoptable: false, reason };
    mount("My App/x");
    expect(await screen.findByRole("heading", { name: "My App/x", level: 2 })).toBeTruthy();
    const note = (await screen.findByText("Can't be adopted")).closest("[data-slot=alert]");
    expect(note?.getAttribute("role")).toBe("status");
    expect(within(note as HTMLElement).getByText(reason)).toBeTruthy();
    // The note is the reason of the server, as given; a Coaming-owned stack's reason reads the same way. Adopt stack
    // is there but off, and never a link to the Adopt page.
    expect(screen.queryByRole("link", { name: /adopt/i })).toBeNull();
    expect(screen.getByRole("button", { name: "Adopt stack" }).getAttribute("aria-disabled")).toBe(
      "true",
    );
  });
});

describe("the Unmanaged stack page: Overview", () => {
  it("lists the services found by label with their state, replicas, image and ports, each swarm service linked to its page", async () => {
    mount();
    const link = await screen.findByRole("link", { name: "app" });
    expect(link.getAttribute("href")).toBe("/env/e1/services/svc-app");
    const row = link.closest("tr") as HTMLElement;
    expect(within(row).getByText("1/1")).toBeTruthy();
    expect(within(row).getByText("nginx:1.27")).toBeTruthy();
    expect(within(row).getByText("8080->80/tcp")).toBeTruthy();
    expect(
      within(screen.getByRole("link", { name: "db" }).closest("tr") as HTMLElement).getByText(
        "0/1",
      ),
    ).toBeTruthy();
  });

  it("shows the Details: environment, target, source Unmanaged, the label it was found by and the service count", async () => {
    mount();
    const details = (await screen.findByRole("heading", { name: "Details" })).closest(
      "[data-slot=card]",
    ) as HTMLElement;
    expect(within(details).getByRole("link", { name: "Prod" }).getAttribute("href")).toBe(
      "/env/e1/stacks",
    );
    expect(within(details).getByText("Swarm")).toBeTruthy();
    expect(within(details).getByText("Unmanaged")).toBeTruthy();
    expect(within(details).getByText("Label com.docker.stack.namespace = legacy")).toBeTruthy();
    expect(within(details).getByText("2 services")).toBeTruthy();
  });
});

describe("the Unmanaged stack page: Resources", () => {
  function mountResources() {
    render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <UnmanagedResources envId={ENV} name="legacy" />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
  }

  const cardOf = async (title: string) =>
    (await screen.findByRole("heading", { name: title })).closest(
      "[data-slot=card]",
    ) as HTMLElement;

  it("previews what adopting would own: labelled networks and volumes read Stack, the others External", async () => {
    mountResources();
    const networks = await cardOf("Networks");
    await within(networks).findByText("legacy_default");
    const net = (name: string) => within(networks).getByText(name).closest("tr") as HTMLElement;
    expect(within(net("legacy_default")).getByText("Stack")).toBeTruthy();
    expect(within(net("shared")).getByText("External")).toBeTruthy();
    const volumes = await cardOf("Volumes");
    await within(volumes).findByText("legacy_data");
    const vol = (name: string) => within(volumes).getByText(name).closest("tr") as HTMLElement;
    expect(within(vol("legacy_data")).getByText("Stack")).toBeTruthy();
    expect(within(vol("shared-vol")).getByText("External")).toBeTruthy();
  });

  it("reads a secret as External and says on the Networks and Volumes cards what the badges mean", async () => {
    mountResources();
    const secrets = await cardOf("Secrets");
    const pw = await within(secrets).findByText("legacy_pw");
    expect(within(pw.closest("tr") as HTMLElement).getByText("External")).toBeTruthy();
    const caption =
      "Labelled objects would be owned by the stack after adopting. The others stay external.";
    expect(within(await cardOf("Networks")).getByText(new RegExp(caption))).toBeTruthy();
    expect(within(await cardOf("Volumes")).getByText(new RegExp(caption))).toBeTruthy();
    // Not on the others, where it would mean nothing.
    expect(within(secrets).queryByText(new RegExp(caption))).toBeNull();
  });

  it("leaves out a card with no objects", async () => {
    mountResources();
    await within(await cardOf("Networks")).findByText("legacy_default");
    expect(screen.queryByRole("heading", { name: "Configs" })).toBeNull();
    expect(screen.queryByRole("heading", { name: "Tasks" })).toBeNull();
    expect(screen.queryByRole("heading", { name: "Containers" })).toBeNull();
  });

  it("keeps a card whose read failed, with its own Retry, and leaves the others alone", async () => {
    resources = { ...resources, secrets: { items: [], error: "read_failed" } };
    mountResources();
    const secrets = await cardOf("Secrets");
    expect(await within(secrets).findByText("Couldn't load secrets.")).toBeTruthy();
    expect(within(secrets).getByRole("button", { name: "Retry loading secrets" })).toBeTruthy();
    expect(await within(await cardOf("Networks")).findByText("legacy_default")).toBeTruthy();
  });
});

describe("the Unmanaged stack page: Logs", () => {
  const proto = HTMLElement.prototype as unknown as Record<string, unknown>;
  const saved: Record<string, PropertyDescriptor | undefined> = {};
  const lay = (prop: string, value: number) => {
    saved[prop] = Object.getOwnPropertyDescriptor(HTMLElement.prototype, prop);
    Object.defineProperty(HTMLElement.prototype, prop, { configurable: true, value });
  };

  beforeEach(() => {
    FakeEventSource.reset();
    vi.stubGlobal("EventSource", FakeEventSource);
    lay("offsetHeight", 400);
    lay("offsetWidth", 800);
  });

  afterEach(() => {
    localStorage.clear();
    for (const [prop, desc] of Object.entries(saved)) {
      if (desc) Object.defineProperty(HTMLElement.prototype, prop, desc);
      else delete proto[prop];
    }
  });

  function mountLogs() {
    render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <UnmanagedLogs envId={ENV} name="legacy" params={{}} onParamsChange={() => {}} />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
  }

  it("opens the merged log at the stack's name, not at an ID", async () => {
    mountLogs();
    const stream = await waitFor(() => {
      const found = FakeEventSource.instances[0];
      if (!found) throw new Error("no stream was opened");
      return found;
    });
    const url = new URL(stream.url, "http://x");
    expect(url.pathname).toBe("/api/env/e1/unmanaged-stacks/legacy/logs/stream");
    expect(url.searchParams.get("sid")).toBeTruthy();
    await act(async () => {
      stream.open();
      stream.sendFrame("ready", { tty: false, target: "swarm", tasks: [] });
    });
  });

  it("addresses the download of an unmanaged stack by its name, and a managed stack's by its ID as before", () => {
    expect(
      downloadUrl({ envId: ENV, source: unmanagedStackSource("my stack", "swarm") }, {}, false),
    ).toBe("/api/env/e1/unmanaged-stacks/my%20stack/logs/download");
    expect(downloadUrl({ envId: ENV, source: stackSource("s1", "swarm") }, {}, false)).toBe(
      "/api/env/e1/stacks/s1/logs/download",
    );
  });

  it("offers a download from the unmanaged route and no pop-out window", async () => {
    mountLogs();
    await waitFor(() => expect(FakeEventSource.instances.length).toBeGreaterThan(0));
    expect(screen.queryByRole("button", { name: "Open in new window" })).toBeNull();
    expect(await screen.findByRole("button", { name: "Download…" })).toBeTruthy();
  });
});

describe("the Unmanaged stack page: states", () => {
  it("shows the header, the alert and the cards as skeletons in their final places while it loads", async () => {
    let release = () => {};
    held = new Promise<void>((resolve) => {
      release = resolve;
    });
    mount();
    // The breadcrumb names the stack at once; the header, the alert and the two cards are skeletons.
    expect(screen.getByRole("navigation", { name: "Breadcrumb" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "legacy", level: 2 })).toBeNull();
    expect(screen.queryByText("Coaming doesn't manage this stack")).toBeNull();
    expect(document.querySelectorAll("[data-slot=skeleton]").length).toBeGreaterThanOrEqual(4);
    // The tabs are there already, so the page does not jump when the stack arrives.
    expect(screen.getByRole("tab", { name: "Overview" })).toBeTruthy();
    release();
    expect(await screen.findByRole("heading", { name: "legacy", level: 2 })).toBeTruthy();
    await waitFor(() => expect(document.querySelectorAll("[data-slot=skeleton]").length).toBe(0));
  });

  it("says it could not load the stack, with a Retry that reads it again", async () => {
    failing = true;
    mount();
    expect(await screen.findByText("Couldn't load legacy.")).toBeTruthy();
    expect(screen.getByText("Check the connection and try again.")).toBeTruthy();
    failing = false;
    fireEvent.click(screen.getByRole("button", { name: "Retry loading legacy" }));
    expect(await screen.findByRole("heading", { name: "legacy", level: 2 })).toBeTruthy();
    expect(screen.queryByText("Couldn't load legacy.")).toBeNull();
  });

  it("turns into This stack is gone when no labelled service or container has the name", async () => {
    notFound = {};
    mount();
    expect(
      await screen.findByRole("heading", { name: "This stack is gone", level: 2 }),
    ).toBeTruthy();
    expect(
      screen.getByText(
        "It was removed outside Coaming, or it is now managed. Go back to the list to find it.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "Back to stacks" }).getAttribute("href")).toBe(
      "/env/e1/stacks",
    );
    // Nothing of the page is left, and no way to a managed stack is offered when there is none.
    expect(screen.queryByRole("tab")).toBeNull();
    expect(screen.queryByRole("link", { name: "Open stack" })).toBeNull();
  });

  it("offers Open stack when a stack of Coaming's holds the name now", async () => {
    notFound = { managedStackId: "stk-9" };
    mount();
    const open = await screen.findByRole("link", { name: "Open stack" });
    expect(open.getAttribute("href")).toBe("/env/e1/stacks/stk-9");
    expect(screen.getByRole("link", { name: "Back to stacks" })).toBeTruthy();
  });

  it("turns into the gone state without a reload when the stack is removed while the page is open", async () => {
    mount();
    expect(await screen.findByRole("heading", { name: "legacy", level: 2 })).toBeTruthy();
    notFound = {};
    await act(async () => {
      await client.invalidateQueries();
    });
    expect(
      await screen.findByRole("heading", { name: "This stack is gone", level: 2 }),
    ).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "legacy", level: 2 })).toBeNull();
  });

  it("shows the real states of a degraded stack and no advice that does not apply", async () => {
    mount();
    expect(await screen.findByText("Degraded")).toBeTruthy();
    const db = (await screen.findByRole("link", { name: "db" })).closest("tr") as HTMLElement;
    expect(within(db).getByText("0/1")).toBeTruthy();
    expect(screen.queryByText("This stack looks like it was created by Portainer")).toBeNull();
    expect(screen.queryByText("Created by Coaming (another instance or a lost record)")).toBeNull();
  });

  it("counts one service in the singular and many in the plural", async () => {
    body = { ...body, servicesTotal: 1, services: [service("app")] };
    mount();
    expect(await screen.findByText("1 service")).toBeTruthy();
    cleanup();
    body = { ...body, servicesTotal: 2, services: [service("app"), service("db")] };
    mount();
    expect(await screen.findByText("2 services")).toBeTruthy();
  });

  it("says nothing runs for a stack with no service left and shows no service rows", async () => {
    body = { ...body, state: "not_running", servicesRunning: 0, servicesTotal: 0, services: [] };
    mount();
    expect(await screen.findByText("No service of this stack is running.")).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("wraps the long name and label in mono in the Details and clamps the display name to two lines", async () => {
    const long = `legacy-${"x".repeat(240)}`;
    body = {
      ...body,
      name: long,
      foundBy: { label: "com.docker.stack.namespace", value: long },
    };
    mount();
    const heading = await screen.findByRole("heading", { level: 2 });
    expect(heading.className).toContain("line-clamp-2");
    expect(heading.className).toContain("break-all");
    const found = await screen.findByText(`Label com.docker.stack.namespace = ${long}`);
    expect(found.className).toContain("break-all");
    expect(found.className).toContain("font-mono");
  });

  it("keeps a table of sixty services inside a card that scrolls sideways", async () => {
    const many = Array.from({ length: 60 }, (_, i) => service(`svc-${String(i).padStart(2, "0")}`));
    body = { ...body, servicesTotal: 60, servicesRunning: 60, state: "running", services: many };
    mount();
    const table = await screen.findByRole("table");
    expect(within(table).getAllByRole("row")).toHaveLength(61);
    expect(table.parentElement?.className).toContain("overflow-x-auto");
  });
});
