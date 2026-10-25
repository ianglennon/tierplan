// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { AdoptPage } from "@/features/stack-adopt/adopt-page";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The check on the Adopt page (plan 08-14, STK-13, D-16, D-20): what the first deploy of the adopted stack would change,
// with Bayplan's ownership labels counted apart, the way each piece is treated, and the settings that need an Admin.
// The check changes nothing and never blocks Adopt.

vi.mock("@/lib/toast", () => ({ toastSuccess: vi.fn(), toastError: vi.fn(), toastInfo: vi.fn() }));

const ENV = "e1";
const PATH = `/api/env/${ENV}/unmanaged-stacks/legacy`;

interface Sent {
  method: string;
  path: string;
  body?: unknown;
}

let sent: Sent[];
let target: "swarm" | "standalone";
// What the adopt check answers, and whether it waits for release() first.
let checkAnswer: { status: number; body: unknown };
let checkHeld: Promise<void> | null;
let adoptAnswer: { status: number; body: unknown };
let client: QueryClient;
const onAdopted = vi.fn();

const spec = (replicas: number, env: string[] = []) => ({
  Name: "legacy_web",
  Mode: { Replicated: { Replicas: replicas } },
  TaskTemplate: { ContainerSpec: { Image: "nginx:1.27", Env: env } },
});

// gap is a service whose first deploy changes its replicas (and an environment value, hidden for a Viewer).
const gap = (name: string, over: Record<string, unknown> = {}) => ({
  name: `legacy_${name}`,
  short: name,
  change: "changed",
  current: spec(2, ["DB_PASSWORD=<hidden>"]),
  next: spec(3, ["DB_PASSWORD=<hidden>"]),
  hiddenChanges: [],
  settings: 1,
  ...over,
});

const checkBody = (over: Record<string, unknown> = {}) => ({
  fingerprint: "fp-1",
  clean: true,
  gaps: [],
  gapCount: 0,
  ownershipOnly: [],
  objects: [],
  counts: { networks: 0, volumes: 0, secrets: 0, configs: 0 },
  policy: { kinds: [], entries: [] },
  portainer: false,
  orphan: false,
  notes: [],
  ...over,
});

const withGaps = (names: string[], over: Record<string, unknown> = {}) =>
  checkBody({ clean: false, gaps: names.map((n) => gap(n)), gapCount: names.length, ...over });

beforeEach(() => {
  sent = [];
  target = "swarm";
  checkAnswer = { status: 200, body: checkBody() };
  checkHeld = null;
  adoptAnswer = { status: 201, body: { id: "s9", name: "legacy", envId: ENV } };
  onAdopted.mockReset();
  client = new QueryClient({ defaultOptions: { queries: { retry: false, retryDelay: 0 } } });
  setCsrfToken("csrf");
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
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: target }] });
      }
      if (path === PATH) {
        return Response.json({
          name: "legacy",
          target,
          state: "running",
          servicesRunning: 2,
          servicesTotal: 2,
          services: [],
          foundBy: { label: "com.docker.stack.namespace", value: "legacy" },
          orphan: false,
          portainer: false,
          owner: "",
          adoptable: true,
          reason: "",
          fingerprint: "fp-page",
        });
      }
      if (path === `${PATH}/adopt-check` && req.method === "POST") {
        if (checkHeld) await checkHeld;
        return Response.json(checkAnswer.body, { status: checkAnswer.status });
      }
      if (path === `${PATH}/adopt` && req.method === "POST") {
        return Response.json(adoptAnswer.body, { status: adoptAnswer.status });
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

function mount() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <AdoptPage envId={ENV} name="legacy" onAdopted={onAdopted} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const checkRequests = () =>
  sent.filter((s) => s.method === "POST" && s.path === `${PATH}/adopt-check`);
const adoptRequests = () => sent.filter((s) => s.method === "POST" && s.path === `${PATH}/adopt`);

// theCard is the check card, found by its title.
async function theCard() {
  const title = await screen.findByRole("heading", { name: "What happens on the first deploy" });
  return title.closest("section") as HTMLElement;
}

function adoptButton() {
  const footer = document.querySelector("[data-slot=editor-footer]") as HTMLElement;
  return within(footer).getByRole("button", { name: /Adopt stack|Adopting/ });
}

// settled waits until the check has answered and the card says so.
async function settled() {
  const card = await theCard();
  await waitFor(() => expect(card.getAttribute("aria-busy")).toBeNull());
  await waitFor(() => expect(within(card).queryByText(/Checking legacy/)).toBeNull());
  return card;
}

describe("the check card: loading", () => {
  it("is a skeleton with 'Checking legacy…', busy, and Adopt is off with that reason", async () => {
    let release: () => void = () => undefined;
    checkHeld = new Promise((resolve) => {
      release = resolve;
    });
    mount();

    const card = await theCard();
    expect(within(card).getByText("Checking legacy…")).toBeTruthy();
    expect(card.querySelector("[aria-busy=true]")).toBeTruthy();
    expect(within(card).getByTestId("adopt-check-loading")).toBeTruthy();
    await waitFor(() => {
      const b = adoptButton();
      expect(b.getAttribute("aria-disabled")).toBe("true");
      expect(b.getAttribute("aria-describedby")).toBeTruthy();
    });
    expect(adoptRequests()).toEqual([]);

    await act(async () => {
      release();
      await checkHeld;
    });
    await settled();
    await waitFor(() => expect(adoptButton().getAttribute("aria-disabled")).toBeNull());
  });

  it("runs the check once on entry and changes nothing", async () => {
    mount();
    await settled();

    expect(checkRequests()).toHaveLength(1);
    expect(adoptRequests()).toEqual([]);
  });
});

describe("the check card: a clean round trip", () => {
  it("says nothing will change except the ownership labels, and shows no gaps", async () => {
    mount();
    const card = await settled();

    expect(
      within(card).getByText("Nothing will change except Bayplan's ownership labels."),
    ).toBeTruthy();
    expect(within(card).queryByText(/will change on the first deploy/)).toBeNull();
    expect(within(card).queryByRole("table")).toBeNull();
    expect(within(card).queryByText(/will be recreated/)).toBeNull();
  });

  it("announces the result once, in one polite live region", async () => {
    mount();
    const card = await settled();

    const sentence = within(card).getAllByText(
      "Nothing will change except Bayplan's ownership labels.",
    );
    expect(sentence).toHaveLength(1);
    const region = sentence[0]?.closest("[aria-live]");
    expect(region?.getAttribute("aria-live")).toBe("polite");
    expect(region?.getAttribute("role")).toBe("status");
  });

  it("counts the services that only take ownership labels apart, and lists them on request", async () => {
    checkAnswer = { status: 200, body: checkBody({ ownershipOnly: ["api", "db"] }) };
    mount();
    const card = await settled();

    expect(
      within(card).getByText(
        "2 services will be recreated to take Bayplan ownership labels (brief restart).",
      ),
    ).toBeTruthy();
    expect(within(card).queryByText("db")).toBeNull();
    fireEvent.click(within(card).getByRole("button", { name: "Show services" }));
    expect(await within(card).findByText("api")).toBeTruthy();
    expect(within(card).getByText("db")).toBeTruthy();
  });

  it("counts one, and words a standalone host's by its containers", async () => {
    checkAnswer = { status: 200, body: checkBody({ ownershipOnly: ["web"] }) };
    mount();
    expect(
      within(await settled()).getByText(
        "1 service will be recreated to take Bayplan ownership labels (brief restart).",
      ),
    ).toBeTruthy();
    cleanup();
    client.clear();

    target = "standalone";
    checkAnswer = { status: 200, body: checkBody({ ownershipOnly: ["web", "db", "cache"] }) };
    mount();
    expect(
      within(await settled()).getByText(
        "3 containers will be recreated to take Bayplan ownership labels (brief restart).",
      ),
    ).toBeTruthy();
  });

  it("truncates a long service name with a tooltip's worth of text kept in the list", async () => {
    const long = `${"payments-reconciliation-worker-".repeat(4)}1`;
    checkAnswer = { status: 200, body: checkBody({ ownershipOnly: [long] }) };
    mount();
    const card = await settled();

    fireEvent.click(within(card).getByRole("button", { name: "Show services" }));
    const name = await within(card).findByText(long);
    expect(name.className).toContain("truncate");
  });
});

describe("the check card: gaps", () => {
  it("counts the differences, says you can still adopt, and leaves Adopt on", async () => {
    checkAnswer = { status: 200, body: withGaps(["web", "db"]) };
    mount();
    const card = await settled();

    expect(within(card).getByText("2 differences will change on the first deploy.")).toBeTruthy();
    expect(
      within(card).getByText("You can still adopt. Nothing changes until you deploy."),
    ).toBeTruthy();
    expect(
      within(card).queryByText("Nothing will change except Bayplan's ownership labels."),
    ).toBeNull();
    await waitFor(() => expect(adoptButton().getAttribute("aria-disabled")).toBeNull());
  });

  it("says one difference in the singular", async () => {
    checkAnswer = { status: 200, body: withGaps(["web"]) };
    mount();

    expect(
      within(await settled()).getByText("1 difference will change on the first deploy."),
    ).toBeTruthy();
  });

  it("opens the rows by default for three differing services or fewer, with the Adopt columns", async () => {
    checkAnswer = { status: 200, body: withGaps(["a", "b", "c"]) };
    mount();
    const card = await settled();

    const triggers = within(card)
      .getAllByRole("button")
      .filter((b) => b.hasAttribute("aria-expanded") && !/Show services/.test(b.textContent ?? ""));
    expect(triggers).toHaveLength(3);
    for (const t of triggers) expect(t.getAttribute("aria-expanded")).toBe("true");
    const table = within(card).getAllByRole("table")[0] as HTMLElement;
    expect(within(table).getByRole("columnheader", { name: "Setting" })).toBeTruthy();
    expect(within(table).getByRole("columnheader", { name: "Running now" })).toBeTruthy();
    expect(
      within(table).getByRole("columnheader", { name: "After the first deploy" }),
    ).toBeTruthy();
    expect(
      within(card)
        .getAllByRole("table")
        .map((t) => t.querySelector("caption")?.textContent),
    ).toContain("Differences in a");
  });

  it("closes the rows for more than three, even for forty services", async () => {
    const names = Array.from({ length: 40 }, (_, i) => `svc${i}`);
    checkAnswer = { status: 200, body: withGaps(names) };
    mount();
    const card = await settled();

    expect(within(card).getByText("40 differences will change on the first deploy.")).toBeTruthy();
    const triggers = within(card)
      .getAllByRole("button")
      .filter((b) => b.hasAttribute("aria-expanded"));
    expect(triggers).toHaveLength(40);
    for (const t of triggers) expect(t.getAttribute("aria-expanded")).toBe("false");
    expect(within(card).queryByRole("table")).toBeNull();
    // Adopt is never blocked by a long list.
    await waitFor(() => expect(adoptButton().getAttribute("aria-disabled")).toBeNull());
  });

  it("wraps long setting values instead of widening the card", async () => {
    const long = `https://registry.example.com/${"very-long-path-segment/".repeat(10)}image:1`;
    checkAnswer = {
      status: 200,
      body: checkBody({
        clean: false,
        gapCount: 1,
        gaps: [
          gap("web", {
            current: { ...spec(2), Labels: { note: "short" } },
            next: { ...spec(2), Labels: { note: long } },
          }),
        ],
      }),
    };
    mount();
    const card = await settled();

    const cell = await within(card).findByText(long);
    expect(cell.className).toContain("break-all");
  });

  it("shows <hidden> where the server withheld a value, and never anything else", async () => {
    checkAnswer = {
      status: 200,
      body: checkBody({
        clean: false,
        gapCount: 1,
        gaps: [
          gap("web", {
            current: spec(2, ["API_KEY=<hidden>"]),
            next: spec(2, ["API_KEY=<hidden>"]),
            hiddenChanges: ["TaskTemplate.ContainerSpec.Env[API_KEY]"],
            settings: 1,
          }),
        ],
      }),
    };
    mount();
    const card = await settled();

    expect(await within(card).findByText("TaskTemplate.ContainerSpec.Env[API_KEY]")).toBeTruthy();
    expect(card.textContent).not.toContain("s3cret");
  });
});

describe("the check card: when the check fails", () => {
  it("says it could not check, offers Retry check, and turns Adopt off", async () => {
    checkAnswer = { status: 500, body: { title: "x", status: 500 } };
    mount();
    const card = await theCard();

    expect(
      await within(card).findByText("Couldn't check legacy against what's running."),
    ).toBeTruthy();
    expect(
      within(card).getByText("Check the connection and try again. Nothing was changed."),
    ).toBeTruthy();
    await waitFor(() => expect(adoptButton().getAttribute("aria-disabled")).toBe("true"));
    expect(adoptRequests()).toEqual([]);

    checkAnswer = { status: 200, body: checkBody() };
    fireEvent.click(within(card).getByRole("button", { name: "Retry check" }));

    expect(
      await within(card).findByText("Nothing will change except Bayplan's ownership labels."),
    ).toBeTruthy();
    expect(within(card).queryByText("Couldn't check legacy against what's running.")).toBeNull();
    await waitFor(() => expect(adoptButton().getAttribute("aria-disabled")).toBeNull());
  });

  it("says why a stack could not be rebuilt, in the server's words", async () => {
    checkAnswer = {
      status: 422,
      body: {
        type: "rebuild_failed",
        title: "x",
        status: 422,
        detail: "no service of legacy is running",
      },
    };
    mount();

    expect(await screen.findByText("no service of legacy is running")).toBeTruthy();
  });

  it("says when too many checks are running", async () => {
    checkAnswer = { status: 429, body: { type: "check_busy", title: "x", status: 429 } };
    mount();

    expect(
      await screen.findByText("Too many checks are running. Try again in a moment."),
    ).toBeTruthy();
  });

  it("words a stack that is already managed inline, with the way to open it", async () => {
    checkAnswer = {
      status: 409,
      body: {
        type: "stack_already_managed",
        title: "x",
        status: 409,
        errors: [{ location: "stackId", message: "s7" }],
      },
    };
    mount();

    expect(await screen.findByText("legacy is already managed by Bayplan.")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open stack" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s7",
    );
  });
});

describe("the check and Adopt", () => {
  it("sends the fingerprint of the check it ran", async () => {
    checkAnswer = { status: 200, body: withGaps(["web"], { fingerprint: "fp-check" }) };
    mount();
    await settled();
    await waitFor(() => expect(adoptButton().getAttribute("aria-disabled")).toBeNull());

    fireEvent.click(adoptButton());

    await waitFor(() => expect(onAdopted).toHaveBeenCalledWith("s9"));
    expect(adoptRequests().map((r) => r.body)).toEqual([{ fingerprint: "fp-check" }]);
  });

  it("runs the check again after adopt_stale, and Adopt then sends the new fingerprint", async () => {
    adoptAnswer = { status: 409, body: { type: "adopt_stale", title: "x", status: 409 } };
    mount();
    await settled();
    await waitFor(() => expect(adoptButton().getAttribute("aria-disabled")).toBeNull());
    fireEvent.click(adoptButton());
    expect(await screen.findByRole("button", { name: "Check again" })).toBeTruthy();

    checkAnswer = { status: 200, body: checkBody({ fingerprint: "fp-2" }) };
    adoptAnswer = { status: 201, body: { id: "s9", name: "legacy", envId: ENV } };
    fireEvent.click(screen.getByRole("button", { name: "Check again" }));
    await waitFor(() => expect(checkRequests()).toHaveLength(2));
    await waitFor(() => expect(screen.queryByText(/changed since the check/)).toBeNull());
    await waitFor(() => expect(adoptButton().getAttribute("aria-disabled")).toBeNull());
    fireEvent.click(adoptButton());

    await waitFor(() => expect(onAdopted).toHaveBeenCalledWith("s9"));
    expect(adoptRequests().map((r) => r.body)).toEqual([
      { fingerprint: "fp-1" },
      { fingerprint: "fp-2" },
    ]);
  });
});

const obj = (kind: string, name: string, treatment: string) => ({ kind, name, treatment });

describe("the ownership card: how the pieces are treated", () => {
  it("counts each kind first, then lists every piece with its treatment", async () => {
    checkAnswer = {
      status: 200,
      body: checkBody({
        objects: [
          obj("network", "legacy_default", "owned"),
          obj("network", "shared", "external"),
          obj("volume", "legacy_data", "owned"),
          obj("secret", "legacy_pw", "external"),
          obj("config", "legacy_conf", "external"),
        ],
        counts: { networks: 2, volumes: 1, secrets: 1, configs: 1 },
      }),
    };
    mount();
    await settled();

    const card = (
      await screen.findByRole("heading", { name: "How the pieces are treated" })
    ).closest("section") as HTMLElement;
    expect(within(card).getByText("2 networks, 1 volume, 1 secret, 1 config")).toBeTruthy();
    const table = within(card).getByRole("table");
    for (const h of ["Kind", "Name", "Treatment"]) {
      expect(within(table).getByRole("columnheader", { name: h })).toBeTruthy();
    }
    const row = (name: string) => within(table).getByText(name).closest("tr") as HTMLElement;
    expect(within(row("legacy_default")).getByText("Owned by the stack")).toBeTruthy();
    expect(within(row("legacy_data")).getByText("Owned by the stack")).toBeTruthy();
    expect(within(row("shared")).getByText("External. Bayplan doesn't change it.")).toBeTruthy();
    for (const name of ["legacy_pw", "legacy_conf"]) {
      expect(within(row(name)).getByText("External. Bayplan doesn't change it.")).toBeTruthy();
      expect(
        within(row(name)).getByText("Not stack-owned. Move it to a stack file to rotate it."),
      ).toBeTruthy();
    }
    expect(within(row("legacy_default")).queryByText(/Not stack-owned/)).toBeNull();
  });

  it("counts zero and one in the singular and the plural", async () => {
    checkAnswer = {
      status: 200,
      body: checkBody({
        objects: [obj("volume", "legacy_data", "owned")],
        counts: { networks: 0, volumes: 1, secrets: 0, configs: 0 },
      }),
    };
    mount();
    await settled();

    expect(await screen.findByText("0 networks, 1 volume, 0 secrets, 0 configs")).toBeTruthy();
  });

  it("wraps a long name instead of widening the card", async () => {
    const long = `legacy_${"a-very-long-volume-name-".repeat(8)}end`;
    checkAnswer = {
      status: 200,
      body: checkBody({
        objects: [obj("volume", long, "owned")],
        counts: { networks: 0, volumes: 1, secrets: 0, configs: 0 },
      }),
    };
    mount();
    await settled();

    expect((await screen.findByText(long)).className).toContain("break-all");
  });

  it("is a skeleton while the check runs", async () => {
    checkHeld = new Promise(() => undefined);
    mount();

    const card = (
      await screen.findByRole("heading", { name: "How the pieces are treated" })
    ).closest("section") as HTMLElement;
    expect(within(card).queryByRole("table")).toBeNull();
    expect(card.querySelector("[data-slot=skeleton]")).toBeTruthy();
  });
});

describe("the policy hits", () => {
  const dockerSock = "a bind mount of /var/run/docker.sock in web";
  const privileged = "privileged: true in web";

  it("flag the settings that need an Admin to deploy, and never block Adopt", async () => {
    checkAnswer = {
      status: 200,
      body: checkBody({
        policy: {
          kinds: ["bind mounts", "extra privileges"],
          entries: [{ text: dockerSock }, { text: privileged }],
        },
      }),
    };
    mount();
    await settled();

    const title = await screen.findByText("Deploying this stack needs Admin");
    const alert = title.closest("[data-slot=alert]") as HTMLElement;
    expect(alert.getAttribute("role")).toBe("status");
    expect(
      within(alert).getByText(
        `It uses settings that need Admin: ${dockerSock}, ${privileged}. Adopting is fine, but later deploys need an Admin.`,
      ),
    ).toBeTruthy();
    await waitFor(() => expect(adoptButton().getAttribute("aria-disabled")).toBeNull());
  });

  it("are absent when the stack uses no host access", async () => {
    mount();
    await settled();

    expect(screen.queryByText("Deploying this stack needs Admin")).toBeNull();
  });
});

describe("the alerts", () => {
  it("come in order: orphan note, Portainer advice, policy hits, environment values", async () => {
    checkAnswer = {
      status: 200,
      body: checkBody({
        orphan: true,
        portainer: true,
        policy: { kinds: ["bind mounts"], entries: [{ text: "privileged: true in web" }] },
      }),
    };
    mount();
    await settled();

    const titles = [
      "Created by Bayplan (another instance or a lost record)",
      "This stack looks like it was created by Portainer",
      "Deploying this stack needs Admin",
      "Environment values are copied in as they are",
    ];
    const nodes = await Promise.all(titles.map((t) => screen.findByText(t)));
    for (let i = 1; i < nodes.length; i += 1) {
      const before = nodes[i - 1] as HTMLElement;
      const after = nodes[i] as HTMLElement;
      expect(
        before.compareDocumentPosition(after) & Node.DOCUMENT_POSITION_FOLLOWING,
        `${titles[i - 1]} comes before ${titles[i]}`,
      ).toBeTruthy();
    }
    const checkTitle = screen.getByRole("heading", { name: "What happens on the first deploy" });
    const last = nodes[3] as HTMLElement;
    expect(
      last.compareDocumentPosition(checkTitle) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });
});
