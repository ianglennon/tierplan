// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
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
        return Response.json(job);
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
    expect(screen.getByText("Off")).toBeTruthy();
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
