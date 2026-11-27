// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider, useMutation } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { IngressStarted, ReviewItem, ReviewPreview } from "@/api/ingress-switches";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ReviewDialog, type ReviewKind } from "@/features/ingress/review-dialog";
import { SettingsTab } from "@/features/ingress/settings-tab";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";
import { waitUnderFakeClock } from "@/test-settle";

// The review dialog every ingress switch goes through (D-07, UI-SPEC S6), driven from the Settings tab's Ingress
// card: the server's preview first, the confirm, then the job read every 2 seconds. The clock is fake, so a read
// that is two seconds away is one advance of it and no test waits.

const ENV = "e1";
const BASE = `/api/env/${ENV}/ingress`;

type Json = Record<string, unknown>;
type Write = { method: string; path: string };

let status: Json;
let writes: Write[];
let jobReads: number;
let requests: string[];
let previewAnswer: () => Promise<Response> | Response;
let startAnswer: () => Response;
let job: Json;
// jobAnswer, when set, answers a read of the job instead of `job`; n counts the reads so far.
let jobAnswer: ((n: number) => Response) | undefined;
let me: ReturnType<typeof makeMe>;
let client: QueryClient;

function problem(status: number, type: string, detail: string): Response {
  return Response.json(
    { title: "Problem", status, type, detail },
    { status, headers: { "Content-Type": "application/problem+json" } },
  );
}

function statusBody(over: Json = {}): Json {
  return {
    enabled: false,
    enableFailed: false,
    disablingIncomplete: false,
    paused: false,
    pendingWipes: 0,
    redeploy: [],
    nodes: { expected: 3, enrolled: 3, ready: 2 },
    ...over,
  };
}

function enablePreview(over: Json = {}): Json {
  return { findings: [], redeploy: [], creates: {}, ...over };
}

function step(id: string, label: string, state: string, over: Json = {}): Json {
  return { id, label, state, kind: "step", parent: "", code: "", message: "", detail: "", ...over };
}

function jobBody(over: Json = {}): Json {
  return {
    id: "j1",
    kind: "ingress_enable",
    state: "running",
    items: [],
    output: [],
    createdAt: "2026-11-26T10:00:00Z",
    finishedAt: "",
    startedBy: "u1",
    ...over,
  };
}

beforeEach(() => {
  vi.useFakeTimers({
    toFake: ["Date", "setTimeout", "clearTimeout", "setInterval", "clearInterval"],
  });
  setCsrfToken(null);
  status = statusBody();
  writes = [];
  jobReads = 0;
  requests = [];
  me = makeMe({ envs: { [ENV]: "admin" } });
  previewAnswer = () => Response.json(enablePreview());
  startAnswer = () => Response.json({ jobId: "j1" }, { status: 202 });
  job = jobBody();
  jobAnswer = undefined;
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false } },
  });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      requests.push(`${req.method} ${path}`);
      if (path === "/api/me") return Response.json(me);
      if (path === `${BASE}/status`) return Response.json(status);
      if (path === `${BASE}/enable/preview`) return previewAnswer();
      if (path === `${BASE}/disable/preview`) return previewAnswer();
      if (path === `${BASE}/enable` || path === `${BASE}/disable`) {
        writes.push({ method: req.method, path });
        return startAnswer();
      }
      if (path === `${BASE}/jobs/j1`) {
        jobReads += 1;
        return jobAnswer ? jobAnswer(jobReads) : Response.json(job);
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  client.clear();
});

function mount() {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <SettingsTab envId={ENV} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

// open presses the card's button and waits for the dialog.
async function open(button: string, title: string): Promise<HTMLElement> {
  mount();
  const trigger = await waitUnderFakeClock(() => screen.getByRole("button", { name: button }));
  fireEvent.click(trigger);
  return waitUnderFakeClock(() => screen.getByRole("dialog", { name: title }));
}

async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

function confirmOf(dialog: HTMLElement, name: string): HTMLButtonElement {
  return within(dialog).getByRole("button", { name }) as HTMLButtonElement;
}

describe("turning ingress on", () => {
  it("shows Off, checks first, posts nothing before the confirm, follows the job and ends with the card On", async () => {
    let release: (answer: Response) => void = () => {};
    previewAnswer = () =>
      new Promise<Response>((resolve) => {
        release = resolve;
      });
    const dialog = await open("Turn on ingress", "Turn on ingress?");
    // The Settings tab has a status line on every card, so the Ingress card's own is read in its own section (the
    // open dialog hides the page from the accessibility tree, so it is found by its heading).
    const card = screen.getByText("Ingress", { selector: "h3" }).closest("section") as HTMLElement;
    expect(within(card).getByText("Off")).toBeTruthy();
    // The preview has not answered: the dialog says so and the confirm waits.
    expect(within(dialog).getByText("Checking…")).toBeTruthy();
    expect(confirmOf(dialog, "Turn on ingress").disabled).toBe(true);

    await act(async () => {
      release(
        Response.json(
          enablePreview({
            findings: [
              {
                code: "ingress_port_in_use",
                severity: "warning",
                message: "Port 8443 is in use on node-2.",
                items: ["node-2"],
              },
            ],
            redeploy: [
              { stackId: "s1", stackName: "shop", holdsClaims: true },
              { stackId: "s2", stackName: "blog", holdsClaims: false },
            ],
          }),
        ),
      );
    });
    await waitUnderFakeClock(() =>
      expect(screen.getByText("Port 8443 is in use on node-2.")).toBeTruthy(),
    );
    expect(within(dialog).queryByText("Checking…")).toBeNull();
    expect(within(dialog).getByText("shop")).toBeTruthy();
    expect(within(dialog).getByText("blog")).toBeTruthy();
    expect(within(dialog).getByText("Stacks to redeploy")).toBeTruthy();
    expect(
      within(dialog).getByText(
        "Redeploy them after this finishes so they join the ingress network.",
      ),
    ).toBeTruthy();
    // Reading the preview switched nothing on.
    expect(writes).toHaveLength(0);
    expect(confirmOf(dialog, "Turn on ingress").disabled).toBe(false);

    job = jobBody({
      items: [
        step("token", "Make the join token", "done"),
        step("network", "Make the network", "running"),
      ],
      output: [
        { seq: 1, at: "2026-11-26T10:00:01Z", text: "join token made" },
        { seq: 2, at: "2026-11-26T10:00:02Z", text: "making the ingress network" },
      ],
    });
    fireEvent.click(confirmOf(dialog, "Turn on ingress"));
    await waitUnderFakeClock(() =>
      expect(screen.getByRole("dialog", { name: "Turning ingress on…" })).toBeTruthy(),
    );
    expect(writes).toEqual([{ method: "POST", path: `${BASE}/enable` }]);
    await waitUnderFakeClock(() => expect(screen.getByText("Make the network")).toBeTruthy());
    expect(jobReads).toBe(1);
    const progress = screen.getByRole("dialog", { name: "Turning ingress on…" });
    // The running step is the reconnecting dot, which holds still for a user who asks for less motion.
    expect(progress.querySelector("span[aria-hidden='true'].animate-pulse")).toBeTruthy();
    const log = within(progress).getByRole("log");
    expect(log.getAttribute("aria-live")).toBe("polite");
    expect(log.textContent).toContain("join token made");
    expect(log.textContent).toContain("making the ingress network");
    // The one button waits for the end of the job.
    const close = within(progress).getByRole("button", {
      name: "Close details",
    }) as HTMLButtonElement;
    expect(close.disabled).toBe(true);

    // Two seconds on, the job is read again; it is done now and so is the status.
    job = jobBody({
      state: "done",
      finishedAt: "2026-11-26T10:00:05Z",
      items: [
        step("token", "Make the join token", "done"),
        step("network", "Make the network", "done"),
      ],
    });
    status = statusBody({ enabled: true });
    await advance(2000);
    await waitUnderFakeClock(() => expect(screen.getByText("Ingress is on.")).toBeTruthy());
    expect(jobReads).toBe(2);
    expect(close.disabled).toBe(false);
    // An ended job is not read again.
    await advance(6000);
    expect(jobReads).toBe(2);
    // It was started once.
    expect(writes).toHaveLength(1);

    fireEvent.click(close);
    await waitUnderFakeClock(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitUnderFakeClock(() => expect(screen.getByText("On")).toBeTruthy());
    expect(screen.getByText("2 of 3 proxy nodes are ready.")).toBeTruthy();
  });

  it("holds the dialog open while the job runs", async () => {
    const dialog = await open("Turn on ingress", "Turn on ingress?");
    await waitUnderFakeClock(() =>
      expect(confirmOf(dialog, "Turn on ingress").disabled).toBe(false),
    );
    fireEvent.click(confirmOf(dialog, "Turn on ingress"));
    await waitUnderFakeClock(() =>
      expect(screen.getByRole("dialog", { name: "Turning ingress on…" })).toBeTruthy(),
    );
    await waitUnderFakeClock(() => expect(jobReads).toBe(1));
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    await advance(50);
    expect(screen.getByRole("dialog", { name: "Turning ingress on…" })).toBeTruthy();
  });

  it("keeps the dialog held when one read of the job fails, and ends it only when the job has", async () => {
    jobAnswer = (n) =>
      n === 1
        ? Response.json({ type: "about:blank", title: "x", status: 502 }, { status: 502 })
        : Response.json(
            jobBody({
              state: n < 4 ? "running" : "done",
              finishedAt: n < 4 ? "" : "2026-11-26T10:00:05Z",
            }),
          );
    const dialog = await open("Turn on ingress", "Turn on ingress?");
    await waitUnderFakeClock(() =>
      expect(confirmOf(dialog, "Turn on ingress").disabled).toBe(false),
    );
    fireEvent.click(confirmOf(dialog, "Turn on ingress"));
    await waitUnderFakeClock(() => expect(jobReads).toBe(1));
    await advance(100);

    // The read missed: the job is still running, so nothing says it ended and the dialog cannot be closed.
    const progress = screen.getByRole("dialog", { name: "Turning ingress on…" });
    expect(
      (within(progress).getByRole("button", { name: /close/i }) as HTMLButtonElement).disabled,
    ).toBe(true);
    expect(within(progress).queryByText(/can't read this job any more/)).toBeNull();
    fireEvent.keyDown(progress, { key: "Escape" });
    await advance(50);
    expect(screen.getByRole("dialog", { name: "Turning ingress on…" })).toBeTruthy();

    await advance(2000);
    await waitUnderFakeClock(() => expect(jobReads).toBeGreaterThanOrEqual(2));
    await advance(6000);
    await waitUnderFakeClock(() =>
      expect((screen.getByRole("button", { name: /close/i }) as HTMLButtonElement).disabled).toBe(
        false,
      ),
    );
  });

  it("ends the wait for a job the server will not show any more", async () => {
    jobAnswer = () =>
      Response.json({ type: "about:blank", title: "x", status: 404 }, { status: 404 });
    const dialog = await open("Turn on ingress", "Turn on ingress?");
    await waitUnderFakeClock(() =>
      expect(confirmOf(dialog, "Turn on ingress").disabled).toBe(false),
    );
    fireEvent.click(confirmOf(dialog, "Turn on ingress"));
    await waitUnderFakeClock(() =>
      expect(screen.getByText(/can't read this job any more/)).toBeTruthy(),
    );
    expect((screen.getByRole("button", { name: /close/i }) as HTMLButtonElement).disabled).toBe(
      false,
    );
  });

  it("draws an error finding as 'This can't go ahead' and keeps the confirm off", async () => {
    previewAnswer = () =>
      Response.json(
        enablePreview({
          findings: [
            {
              code: "ingress_no_manager",
              severity: "error",
              message: "No manager node is connected.",
              items: [],
            },
          ],
        }),
      );
    const dialog = await open("Turn on ingress", "Turn on ingress?");
    await waitUnderFakeClock(() => expect(screen.getByText("This can't go ahead")).toBeTruthy());
    expect(within(dialog).getByText("No manager node is connected.")).toBeTruthy();
    const confirm = confirmOf(dialog, "Turn on ingress");
    expect(confirm.disabled).toBe(true);
    fireEvent.click(confirm);
    await advance(50);
    expect(writes).toHaveLength(0);
  });

  it("links the HTTP-01 refusals to HTTPS settings", async () => {
    previewAnswer = () =>
      Response.json(
        enablePreview({
          findings: [
            {
              code: "ingress_http01_conflict",
              severity: "error",
              message: "TierPlan runs on this swarm and its own certificate uses HTTP-01.",
              items: [],
            },
            {
              code: "ingress_http01_unknown",
              severity: "error",
              message: "TierPlan can't tell where it runs.",
              items: [],
            },
            {
              code: "ingress_no_manager",
              severity: "error",
              message: "No manager node is connected.",
              items: [],
            },
          ],
        }),
      );
    const dialog = await open("Turn on ingress", "Turn on ingress?");
    await waitUnderFakeClock(() => expect(screen.getByText("This can't go ahead")).toBeTruthy());
    const links = within(dialog).getAllByRole("link", { name: "Open HTTPS settings" });
    expect(links).toHaveLength(2);
    for (const link of links) expect(link.getAttribute("href")).toBe("/settings/https");
    const conflict = within(dialog)
      .getByText("TierPlan runs on this swarm and its own certificate uses HTTP-01.")
      .closest("li") as HTMLElement;
    expect(within(conflict).getByRole("link", { name: "Open HTTPS settings" })).toBeTruthy();
    const unknown = within(dialog)
      .getByText("TierPlan can't tell where it runs.")
      .closest("li") as HTMLElement;
    expect(within(unknown).getByRole("link", { name: "Open HTTPS settings" })).toBeTruthy();
    const other = within(dialog)
      .getByText("No manager node is connected.")
      .closest("li") as HTMLElement;
    expect(within(other).queryByRole("link")).toBeNull();
  });

  it("shows the carve-out note on enable", async () => {
    const note =
      "TierPlan's own certificate uses the HTTP-01 challenge. The ingress sends only /.well-known/acme-challenge/ requests for tp.example.com to TierPlan, over the tierplan-self network, so the certificate keeps renewing.";
    previewAnswer = () => Response.json(enablePreview({ notice: note }));
    const dialog = await open("Turn on ingress", "Turn on ingress?");
    const text = await waitUnderFakeClock(() => within(dialog).getByText(note));
    const alert = text.closest("[data-slot=alert]") as HTMLElement;
    expect(alert).toBeTruthy();
    // Neutral: not a warning and not a refusal.
    expect(alert.className).not.toContain("border-status-warning");
    expect(alert.className).not.toContain("text-destructive");
    expect(confirmOf(dialog, "Turn on ingress").disabled).toBe(false);
  });

  it("shows the sentence of a refused preview in the dialog, naming who holds the environment, and no toast", async () => {
    const sentence = "ana is deploying a stack to this environment.";
    previewAnswer = () => problem(409, "operation_locked", sentence);
    const dialog = await open("Turn on ingress", "Turn on ingress?");
    await waitUnderFakeClock(() => expect(within(dialog).getByText(sentence)).toBeTruthy());
    expect(confirmOf(dialog, "Turn on ingress").disabled).toBe(true);
    expect(screen.getAllByText(sentence)).toHaveLength(1);
  });

  it("shows the server's sentence when the confirm finds the environment locked, with no toast", async () => {
    const sentence = "ana is deploying a stack to this environment.";
    startAnswer = () => problem(409, "operation_locked", sentence);
    const dialog = await open("Turn on ingress", "Turn on ingress?");
    await waitUnderFakeClock(() =>
      expect(confirmOf(dialog, "Turn on ingress").disabled).toBe(false),
    );
    fireEvent.click(confirmOf(dialog, "Turn on ingress"));
    await waitUnderFakeClock(() => expect(within(dialog).getByText(sentence)).toBeTruthy());
    expect(screen.getAllByText(sentence)).toHaveLength(1);
    // Still on the preview: nothing was started, so there is no job to read.
    expect(screen.getByRole("dialog", { name: "Turn on ingress?" })).toBeTruthy();
    expect(jobReads).toBe(0);
  });

  it("names 10 stacks to redeploy and counts the rest", async () => {
    previewAnswer = () =>
      Response.json(
        enablePreview({
          redeploy: Array.from({ length: 12 }, (_, i) => ({
            stackId: `s${i}`,
            stackName: `stack-${i}`,
            holdsClaims: true,
          })),
        }),
      );
    const dialog = await open("Turn on ingress", "Turn on ingress?");
    await waitUnderFakeClock(() => expect(within(dialog).getByText("stack-0")).toBeTruthy());
    expect(within(dialog).getByText("stack-9")).toBeTruthy();
    expect(within(dialog).queryByText("stack-10")).toBeNull();
    expect(within(dialog).getByText("and 2 more")).toBeTruthy();
  });

  it("says so, in the job's own sentence, when the job failed, and offers Close", async () => {
    job = jobBody({
      state: "failed",
      finishedAt: "2026-11-26T10:00:05Z",
      items: [
        step("token", "Make the join token", "done"),
        step("network", "Make the network", "failed", {
          message: "The network could not be made: no free address block.",
        }),
      ],
    });
    const dialog = await open("Turn on ingress", "Turn on ingress?");
    await waitUnderFakeClock(() =>
      expect(confirmOf(dialog, "Turn on ingress").disabled).toBe(false),
    );
    fireEvent.click(confirmOf(dialog, "Turn on ingress"));
    await waitUnderFakeClock(() =>
      expect(
        screen.getByText("The network could not be made: no free address block."),
      ).toBeTruthy(),
    );
    expect(
      screen.getByText(
        "Nothing is lost. Turn it on again to carry on, or off to remove what was made.",
      ),
    ).toBeTruthy();
    const close = screen.getByRole("button", { name: "Close" }) as HTMLButtonElement;
    expect(close.disabled).toBe(false);
    fireEvent.click(close);
    await waitUnderFakeClock(() => expect(screen.queryByRole("dialog")).toBeNull());
  });
});

describe("turning ingress off", () => {
  beforeEach(() => {
    status = statusBody({ enabled: true });
    previewAnswer = () => Response.json({ findings: [], removes: {}, unreachable: ["node-3"] });
    job = jobBody({ kind: "ingress_disable" });
  });

  it("previews first, confirms with the destructive button and starts one job", async () => {
    const dialog = await open("Turn off ingress", "Turn off ingress?");
    expect(
      within(dialog).getByText(
        "Routes stop being served on this environment. Stacks and their labels are not changed.",
      ),
    ).toBeTruthy();
    expect(requests).toContain(`POST ${BASE}/disable/preview`);
    expect(requests).not.toContain(`POST ${BASE}/enable/preview`);
    await waitUnderFakeClock(() => expect(within(dialog).getByText(/node-3/)).toBeTruthy());
    expect(writes).toHaveLength(0);
    const confirm = confirmOf(dialog, "Turn off ingress");
    expect(confirm.className).toContain("destructive");
    fireEvent.click(confirm);
    await waitUnderFakeClock(() =>
      expect(screen.getByRole("dialog", { name: "Turning ingress off…" })).toBeTruthy(),
    );
    expect(writes).toEqual([{ method: "POST", path: `${BASE}/disable` }]);

    job = jobBody({ kind: "ingress_disable", state: "done", finishedAt: "2026-11-26T10:00:05Z" });
    await advance(2000);
    await waitUnderFakeClock(() => expect(screen.getByText("Ingress is off.")).toBeTruthy());
  });

  it("sends nothing when the person keeps ingress on", async () => {
    const dialog = await open("Turn off ingress", "Turn off ingress?");
    await waitUnderFakeClock(() => expect(within(dialog).getByText(/node-3/)).toBeTruthy());
    fireEvent.click(within(dialog).getByRole("button", { name: "Keep ingress on" }));
    await waitUnderFakeClock(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(writes).toHaveLength(0);
    expect(jobReads).toBe(0);
  });

  it("refuses the switch-off while routes are live", async () => {
    previewAnswer = () =>
      Response.json({
        findings: [
          {
            code: "ingress_routes_live",
            severity: "error",
            message: "Routes are still live: app.example.com (shop).",
            items: ["app.example.com"],
          },
        ],
        removes: {},
        unreachable: [],
      });
    const dialog = await open("Turn off ingress", "Turn off ingress?");
    await waitUnderFakeClock(() => expect(screen.getByText("This can't go ahead")).toBeTruthy());
    expect(confirmOf(dialog, "Turn off ingress").disabled).toBe(true);
  });
});

describe("turning ingress off while TierPlan's own route is on", () => {
  const release =
    "TierPlan's own route on tp.example.com is released first. Direct access on https://tierplan.internal:7480 keeps working.";

  function selfRoute(host: string): Json {
    return {
      here: "yes",
      on: true,
      host,
      state: "serving",
      reason: "",
      directAddress: "https://tierplan.internal:7480",
      kind: "standalone",
      attached: true,
      network: "tierplan-self",
      carveOut: false,
    };
  }

  beforeEach(() => {
    previewAnswer = () =>
      Response.json({ findings: [], removes: {}, unreachable: [], notice: release });
    job = jobBody({ kind: "ingress_disable" });
  });

  it("shows the release-first note and the route warning on disable", async () => {
    status = statusBody({ enabled: true, selfRoute: selfRoute(window.location.host) });
    const dialog = await open("Turn off ingress", "Turn off ingress?");
    const note = await waitUnderFakeClock(() => within(dialog).getByText(release));
    const neutral = note.closest("[data-slot=alert]") as HTMLElement;
    expect(neutral.className).not.toContain("border-status-warning");
    const warning = within(dialog).getByText(
      "You're using TierPlan through this route. This page may stop answering as the route goes. The change still goes through. Open https://tierplan.internal:7480 to see the result.",
    );
    expect((warning.closest("[data-slot=alert]") as HTMLElement).className).toContain(
      "border-status-warning",
    );
    // The warning never holds the switch back (D-11).
    expect(confirmOf(dialog, "Turn off ingress").disabled).toBe(false);
  });

  it("draws the note but no warning when the page came another way", async () => {
    status = statusBody({ enabled: true, selfRoute: selfRoute("tp.example.com") });
    const dialog = await open("Turn off ingress", "Turn off ingress?");
    await waitUnderFakeClock(() => expect(within(dialog).getByText(release)).toBeTruthy());
    expect(within(dialog).queryByText(/You're using TierPlan through this route/)).toBeNull();
  });
});

describe("a reader", () => {
  it("sees the status and a button disabled with its reason, and never requests a job", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    status = statusBody({ enabled: true });
    mount();
    const button = await waitUnderFakeClock(() =>
      screen.getByRole("button", { name: "Turn off ingress" }),
    );
    expect(screen.getByText("On")).toBeTruthy();
    expect(screen.getByText("2 of 3 proxy nodes are ready.")).toBeTruthy();
    expect(button.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(button);
    await advance(50);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getAllByText("Needs Admin").length).toBeGreaterThan(0);
    expect(requests.filter((r) => r.includes("/jobs/") || r.startsWith("POST"))).toEqual([]);
  });
});

// The self route's previews (D-17, UI-SPEC S1b, S1c, A-05) are an ordered list of items, not findings. The dialog is
// driven directly here with fixed answers: what the server says is a given, what the dialog draws is under test.

const ITEMS_ALL: ReviewItem[] = [
  { code: "self_hostname", state: "ok", message: "tp.example.com is free and under example.com." },
  {
    code: "self_certificate",
    state: "warning",
    message: "No certificate yet. TierPlan orders one when you turn the route on.",
  },
  {
    code: "self_network",
    state: "warning",
    message: "TierPlan is not on tierplan-self yet.",
    snippet: { kind: "stack_file", text: "services:\n  tierplan:" },
  },
  {
    code: "self_origin",
    state: "ok",
    message: "TierPlan will also accept https://tp.example.com.",
  },
  {
    code: "self_carve_out",
    state: "ok",
    message: "The ingress sends only acme-challenge requests for tp.example.com to TierPlan.",
  },
];

function SelfHarness({
  kind,
  items,
  withForm = false,
  onChecked,
}: {
  kind: ReviewKind;
  items: ReviewItem[];
  withForm?: boolean;
  onChecked?: () => void;
}) {
  const preview = useMutation({
    gcTime: 0,
    mutationFn: async (): Promise<ReviewPreview> => {
      onChecked?.();
      return { findings: [], redeploy: [], unreachable: [], items };
    },
  });
  const start = useMutation({
    gcTime: 0,
    mutationFn: async (): Promise<IngressStarted> => ({ jobId: "j1" }),
  });
  return (
    <ReviewDialog
      envId={ENV}
      kind={kind}
      open
      onOpenChange={() => {}}
      preview={preview}
      start={start}
      bodyValues={{ direct: "tp.lan:7480", host: "tp.example.com" }}
      form={
        withForm
          ? {
              fields: <input aria-label="Hostname" defaultValue="tp.example.com" />,
              canCheck: true,
              check: () => preview.mutate(),
            }
          : undefined
      }
    />
  );
}

const TITLES: Partial<Record<ReviewKind, string>> = {
  selfEnable: "Turn on TierPlan's own route?",
  selfDisable: "Turn off TierPlan's own route?",
  disable: "Turn off ingress?",
  vipEnable: "Turn on virtual IP?",
};

async function openSelf(kind: ReviewKind, items: ReviewItem[], withForm = false) {
  render(
    <QueryClientProvider client={client}>
      <SelfHarness kind={kind} items={items} withForm={withForm} />
    </QueryClientProvider>,
  );
  return waitUnderFakeClock(() => screen.getByRole("dialog", { name: TITLES[kind] }));
}

// openVip opens a kind that has no check label of its own, with a form, to see the fallback.
function openVip() {
  return openSelf("vipEnable", [], true);
}

function headingsOf(dialog: HTMLElement): string[] {
  const list = within(dialog).getByRole("list");
  return within(list)
    .getAllByRole("listitem")
    .map((li) => li.querySelector(".text-sm.font-medium")?.textContent ?? "");
}

describe("an ordered self route preview", () => {
  it("draws the items in the order the server sends them, each with its word", async () => {
    const dialog = await openSelf("selfEnable", ITEMS_ALL);
    await waitUnderFakeClock(() => within(dialog).getByRole("list"));
    expect(headingsOf(dialog)).toEqual([
      "Hostname",
      "Domain and certificate",
      "Self network",
      "Origin",
      "Port 80 for TierPlan's certificate",
    ]);
    const list = within(dialog).getByRole("list");
    expect(list.tagName).toBe("OL");
    const words = within(list)
      .getAllByRole("listitem")
      .map((li) => li.querySelector(".sr-only")?.textContent);
    expect(words).toEqual(["OK", "Warning", "Warning", "OK", "OK"]);
    expect(within(dialog).getByText("tp.example.com is free and under example.com.")).toBeTruthy();
  });

  it("does not regroup the items by severity", async () => {
    const reversed = [ITEMS_ALL[3], ITEMS_ALL[1], ITEMS_ALL[0]] as ReviewItem[];
    const dialog = await openSelf("selfEnable", reversed);
    await waitUnderFakeClock(() => within(dialog).getByRole("list"));
    expect(headingsOf(dialog)).toEqual(["Origin", "Domain and certificate", "Hostname"]);
  });

  it("skips an item the server does not send", async () => {
    const without = ITEMS_ALL.filter((item) => item.code !== "self_carve_out");
    const dialog = await openSelf("selfEnable", without);
    await waitUnderFakeClock(() => within(dialog).getByRole("list"));
    const list = within(dialog).getByRole("list");
    expect(within(list).getAllByRole("listitem")).toHaveLength(4);
    expect(within(dialog).queryByText("Port 80 for TierPlan's certificate")).toBeNull();
  });
});

describe("an ordered preview that refuses, and the self route's words", () => {
  const REFUSED: ReviewItem[] = [
    {
      code: "self_hostname",
      state: "ok",
      message: "tp.example.com is free and under example.com.",
    },
    {
      code: "self_network",
      state: "refused",
      message: "TierPlan runs as a plain container. Deploy it as a swarm service, then try again.",
    },
  ];

  it("blocks once: one destructive alert, the refused sentence drawn once, the confirm off", async () => {
    const dialog = await openSelf("selfDisable", REFUSED);
    await waitUnderFakeClock(() => within(dialog).getByRole("list"));
    const alerts = within(dialog).getAllByRole("alert");
    expect(alerts).toHaveLength(1);
    expect(within(alerts[0] as HTMLElement).getByText("This can't go ahead")).toBeTruthy();
    expect(
      within(alerts[0] as HTMLElement).getByText(
        "Fix the item marked Refused. Nothing has been changed.",
      ),
    ).toBeTruthy();
    expect(within(dialog).getAllByText(/Deploy it as a swarm service/)).toHaveLength(1);
    expect(confirmOf(dialog, "Turn off own route").disabled).toBe(true);
  });

  it("lets a warning through: the confirm is on when nothing is refused", async () => {
    const dialog = await openSelf("selfDisable", ITEMS_ALL);
    await waitUnderFakeClock(() => within(dialog).getByRole("list"));
    expect(within(dialog).queryByText("This can't go ahead")).toBeNull();
    expect(confirmOf(dialog, "Turn off own route").disabled).toBe(false);
  });

  it("returns to the form with the blocked list above it when a checked hostname is refused", async () => {
    const dialog = await openSelf("selfEnable", REFUSED, true);
    expect(within(dialog).queryByRole("list")).toBeNull();
    fireEvent.click(within(dialog).getByRole("button", { name: "Check hostname" }));
    await waitUnderFakeClock(() => within(dialog).getByRole("list"));
    expect(within(dialog).getByLabelText("Hostname")).toBeTruthy();
    expect(within(dialog).getByText("This can't go ahead")).toBeTruthy();
    expect(within(dialog).getAllByText(/Deploy it as a swarm service/)).toHaveLength(1);
  });

  it("names the form's check button for the kind, and falls back to the shared one", async () => {
    const self = await openSelf("selfEnable", [], true);
    expect(within(self).getByRole("button", { name: "Check hostname" })).toBeTruthy();
    cleanup();
    const vip = await openVip();
    expect(within(vip).getByRole("button", { name: "Check address" })).toBeTruthy();
    expect(within(vip).queryByRole("button", { name: "Check hostname" })).toBeNull();
  });

  it("draws a snippet under its item in a copyable block, with the title", async () => {
    const dialog = await openSelf("selfEnable", ITEMS_ALL);
    await waitUnderFakeClock(() => within(dialog).getByRole("list"));
    expect(within(dialog).getByText("Add TierPlan to the self network")).toBeTruthy();
    const block = within(dialog).getByRole("region", { name: "the stack file lines" });
    expect(block.querySelector("code")?.textContent).toBe("services:\n  tierplan:");
    expect(within(dialog).getByRole("button", { name: /Copy the stack file lines/i })).toBeTruthy();
    expect(within(dialog).getByText(/If the service has no/)).toBeTruthy();
  });

  it("confirms the self kinds with the primary button and a disable with the destructive one", async () => {
    const off = await openSelf("selfDisable", ITEMS_ALL);
    await waitUnderFakeClock(() => within(off).getByRole("list"));
    expect(confirmOf(off, "Turn off own route").className).not.toContain("bg-destructive");
    cleanup();
    const on = await openSelf("selfEnable", ITEMS_ALL);
    await waitUnderFakeClock(() => within(on).getByRole("list"));
    expect(confirmOf(on, "Turn on own route").className).not.toContain("bg-destructive");
    cleanup();
    const plain = await openSelf("disable", []);
    await waitUnderFakeClock(() =>
      expect(confirmOf(plain, "Turn off ingress").disabled).toBe(false),
    );
    expect(confirmOf(plain, "Turn off ingress").className).toContain("bg-destructive");
  });
});
