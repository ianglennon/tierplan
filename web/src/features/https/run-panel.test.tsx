// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { formatDate } from "@/features/https/copy";
import {
  type ACMERunFrame,
  type ACMERunStreamState,
  type ACMEStepState,
  acmeRunStreamUrl,
  createACMERunStream,
  initialACMERunStream,
  reduceACMERunStream,
} from "@/features/https/https-run-stream";
import { RunPanel } from "@/features/https/run-panel";
import "@/i18n";
import { acmeRunFrame, acmeStep, acmeSteps } from "@/test-https";
import { waitUnderFakeClock } from "@/test-settle";

// The run panel and the stream behind it (plan 14-16, D-09, UI-SPEC E5).

afterEach(cleanup);

function live(over: Partial<ACMERunStreamState> = {}): ACMERunStreamState {
  return { ...initialACMERunStream, status: "live", run: acmeRunFrame(), ...over };
}

function draw(state: ACMERunStreamState, props: { validUntil?: string; names?: string } = {}) {
  return render(
    <TooltipProvider>
      <RunPanel
        stream={state}
        names={props.names ?? "tierplan.example.com"}
        host="tierplan.example.com"
        validUntil={props.validUntil}
      />
    </TooltipProvider>,
  );
}

const steps = () => screen.getByRole("list", { name: "Steps" });
const stateOf = (id: string) =>
  steps().querySelector(`[data-step="${id}"]`)?.getAttribute("data-state");

describe("the reducer", () => {
  it("starts connecting with nothing", () => {
    expect(initialACMERunStream).toEqual({
      status: "connecting",
      finished: false,
      gone: false,
      accessLost: false,
    });
  });

  it("replaces the run with a snapshot and is live; a snapshot of a finished run is finished", () => {
    const next = reduceACMERunStream(initialACMERunStream, {
      type: "snapshot",
      run: acmeRunFrame(),
    });
    expect(next).toMatchObject({ status: "live", finished: false });
    const ended = reduceACMERunStream(initialACMERunStream, {
      type: "snapshot",
      run: acmeRunFrame({ state: "failed" }),
    });
    expect(ended.finished).toBe(true);
  });

  it("replaces one step by its id, keeping the others and their order", () => {
    const start = reduceACMERunStream(initialACMERunStream, {
      type: "snapshot",
      run: acmeRunFrame(),
    });
    const next = reduceACMERunStream(start, {
      type: "item",
      item: acmeStep({ id: "order", label: "Order", state: "running" }),
    });
    expect(next.run?.items?.map((i) => [i.id, i.state])).toEqual([
      ["account", "running"],
      ["order", "running"],
      ["challenge", "waiting"],
      ["validate", "waiting"],
      ["finalize", "waiting"],
      ["stored", "waiting"],
    ]);
  });

  it("ignores a step before any snapshot and adds a step it did not know", () => {
    expect(
      reduceACMERunStream(initialACMERunStream, { type: "item", item: acmeStep() }).run,
    ).toBeUndefined();
    const start = reduceACMERunStream(initialACMERunStream, {
      type: "snapshot",
      run: acmeRunFrame(),
    });
    const next = reduceACMERunStream(start, {
      type: "item",
      item: acmeStep({ id: "extra", state: "done" }),
    });
    expect(next.run?.items).toHaveLength(7);
  });

  it("ends the run with a done frame and never goes back to live", () => {
    const start = reduceACMERunStream(initialACMERunStream, {
      type: "snapshot",
      run: acmeRunFrame(),
    });
    const done = reduceACMERunStream(start, { type: "done", run: acmeRunFrame({ state: "done" }) });
    expect(done).toMatchObject({ finished: true, status: "closed" });
    expect(reduceACMERunStream(done, { type: "status", status: "reconnecting" }).status).toBe(
      "closed",
    );
    expect(
      reduceACMERunStream(done, { type: "snapshot", run: acmeRunFrame({ state: "running" }) })
        .status,
    ).toBe("closed");
  });

  it("records a reconnect, a vanished run and lost access", () => {
    const start = live();
    expect(reduceACMERunStream(start, { type: "status", status: "reconnecting" }).status).toBe(
      "reconnecting",
    );
    expect(
      reduceACMERunStream(start, { type: "status", status: "closed", gone: true }),
    ).toMatchObject({ status: "closed", gone: true });
    expect(
      reduceACMERunStream(start, { type: "status", status: "closed", accessLost: true }),
    ).toMatchObject({ status: "closed", accessLost: true });
  });
});

describe("the address", () => {
  it("names the run, and the last event seen when there is one", () => {
    const plain = new URL(acmeRunStreamUrl("j 1"), "http://x");
    expect(plain.pathname).toBe("/api/https/acme/runs/j%201/stream");
    expect(plain.searchParams.has("lastEventId")).toBe(false);
    expect(new URL(acmeRunStreamUrl("j1", "7"), "http://x").searchParams.get("lastEventId")).toBe(
      "7",
    );
  });
});

describe("the connection", () => {
  beforeEach(() => {
    FakeEventSource.reset();
    vi.stubGlobal("EventSource", FakeEventSource);
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  function open(probe?: () => Promise<number>) {
    const frames: ACMERunFrame[] = [];
    const stream = createACMERunStream({ jobId: "j1", onFrame: (f) => frames.push(f), probe });
    return { stream, source: FakeEventSource.instances[0] as FakeEventSource, frames };
  }

  it("turns the named frames into frames", () => {
    const { source, frames } = open();
    source.sendFrame("snapshot", acmeRunFrame());
    source.sendFrame("item", acmeStep({ id: "order", state: "running" }), "4");
    expect(frames.map((f) => f.type)).toEqual(["snapshot", "item"]);
  });

  it("closes after the done frame and does not reconnect", () => {
    vi.useFakeTimers();
    const { source, frames } = open();
    source.sendFrame("snapshot", acmeRunFrame());
    source.sendFrame("done", acmeRunFrame({ state: "done" }), "9");
    expect(frames.at(-1)?.type).toBe("done");
    expect(source.closed).toBe(true);
    source.fail();
    vi.advanceTimersByTime(60_000);
    expect(FakeEventSource.instances).toHaveLength(1);
  });

  it("reconnects after 1 then 2 seconds, naming the last event it saw", () => {
    vi.useFakeTimers();
    const { source, frames } = open();
    source.sendFrame("snapshot", acmeRunFrame());
    source.sendFrame("item", acmeStep({ id: "order", state: "running" }), "4");
    source.fail();
    expect(frames.at(-1)).toEqual({ type: "status", status: "reconnecting" });
    vi.advanceTimersByTime(999);
    expect(FakeEventSource.instances).toHaveLength(1);
    vi.advanceTimersByTime(1);
    const second = FakeEventSource.instances[1] as FakeEventSource;
    expect(new URL(second.url, "http://x").searchParams.get("lastEventId")).toBe("4");
    second.fail();
    vi.advanceTimersByTime(1999);
    expect(FakeEventSource.instances).toHaveLength(2);
    vi.advanceTimersByTime(1);
    expect(FakeEventSource.instances).toHaveLength(3);
  });

  it("turns a refused reopen into gone when the run is no longer listed (404)", async () => {
    vi.useFakeTimers();
    const probe = vi.fn(async () => 404);
    const { source, frames } = open(probe);
    source.sendFrame("snapshot", acmeRunFrame());
    source.fail(true);
    await waitUnderFakeClock(() =>
      expect(frames.at(-1)).toEqual({ type: "status", status: "closed", gone: true }),
    );
  });

  it("turns a refused reopen into lost access on 403, and keeps trying on a busy server", async () => {
    vi.useFakeTimers();
    const probe = vi.fn(async () => 403);
    const { source, frames } = open(probe);
    source.sendFrame("snapshot", acmeRunFrame());
    source.fail(true);
    await waitUnderFakeClock(() =>
      expect(frames.at(-1)).toEqual({ type: "status", status: "closed", accessLost: true }),
    );
    FakeEventSource.reset();
    const busy = open(async () => 503);
    busy.source.sendFrame("snapshot", acmeRunFrame());
    busy.source.fail(true);
    await vi.advanceTimersByTimeAsync(1000);
    expect(FakeEventSource.instances.length).toBeGreaterThanOrEqual(2);
  });

  it("reports nothing after it is closed", () => {
    const { stream, source, frames } = open();
    stream.close();
    source.sendFrame("snapshot", acmeRunFrame());
    source.fail();
    expect(frames).toEqual([]);
    expect(source.closed).toBe(true);
  });
});

describe("E5: the run panel", () => {
  it("is not drawn before there is a run (empty)", () => {
    const { container } = draw(initialACMERunStream);
    expect(container.innerHTML).toBe("");
  });

  it("draws the six steps in order, with the header announced politely and the list not (loading)", () => {
    draw(live());
    const header = screen.getByText("Issuing certificate").closest("p") as HTMLElement;
    expect(header.getAttribute("aria-live")).toBe("polite");
    expect(steps().getAttribute("aria-live")).toBe("off");
    expect(
      Array.from(steps().querySelectorAll("li")).map((li) => li.getAttribute("data-step")),
    ).toEqual(["account", "order", "challenge", "validate", "finalize", "stored"]);
    expect(
      Array.from(steps().querySelectorAll("li")).map(
        (li) => li.querySelector("span.font-medium")?.firstChild?.textContent,
      ),
    ).toEqual(["Account", "Order", "Challenge", "Validation", "Finalize", "Stored"]);
  });

  it.each<[ACMEStepState, string]>([
    ["waiting", "Not started"],
    ["running", "Running"],
    ["done", "Done"],
    ["failed", "Failed"],
    ["skipped", "Skipped"],
  ])("gives a step in state %s its own mark and words", (state, words) => {
    draw(live({ run: acmeRunFrame({ items: acmeSteps({ order: state }) }) }));
    const li = steps().querySelector('[data-step="order"]') as HTMLElement;
    expect(li.getAttribute("data-state")).toBe(state);
    expect(within(li).getByText(`(${words})`)).toBeTruthy();
    const svg = li.querySelector("svg");
    expect(svg).not.toBeNull();
    if (state === "running") expect(li.querySelector('[data-slot="spinner"]')).not.toBeNull();
    if (state === "done") expect(svg?.getAttribute("class")).toContain("text-status-online-text");
    if (state === "failed") expect(svg?.getAttribute("class")).toContain("text-destructive");
    if (state === "waiting" || state === "skipped")
      expect(svg?.getAttribute("class")).toContain("text-muted-foreground");
  });

  it("keeps a finished run with its words and the date until the next run (populated)", () => {
    const until = new Date(Date.now() + 90 * 86_400_000).toISOString();
    draw(
      live({
        finished: true,
        status: "closed",
        run: acmeRunFrame({
          state: "done",
          items: acmeSteps({
            account: "skipped",
            order: "done",
            challenge: "done",
            validate: "done",
            finalize: "done",
            stored: "done",
          }),
        }),
      }),
      { validUntil: until },
    );
    expect(screen.getByText("Certificate issued.")).toBeTruthy();
    expect(
      screen.getByText(`Now serving a certificate valid until ${formatDate(until)}.`),
    ).toBeTruthy();
    expect(steps().querySelectorAll("li")).toHaveLength(6);
  });

  it("does not claim a date before the status knows the new certificate", () => {
    draw(live({ finished: true, status: "closed", run: acmeRunFrame({ state: "done" }) }));
    expect(screen.queryByText(/Now serving a certificate/)).toBeNull();
  });

  it.each([
    ["issue", "admin", "running", "Issuing certificate"],
    ["renew", "admin", "running", "Renewing certificate"],
    ["renew", "schedule", "running", "Renewing certificate automatically"],
    ["issue", "resume", "running", "Issuing certificate"],
    ["issue", "admin", "done", "Certificate issued."],
    ["renew", "schedule", "done", "Certificate renewed."],
    ["issue", "admin", "failed", "Issuing failed."],
    ["renew", "admin", "failed", "Renewing failed."],
    ["issue", "admin", "cancelled", "Issuing stopped."],
  ] as const)("headlines a %s run started by %s that is %s", (kind, trigger, state, header) => {
    draw(live({ run: acmeRunFrame({ kind, trigger, state }) }));
    expect(screen.getByText(header)).toBeTruthy();
  });

  it("shows earlier steps as done and the account as skipped for a run joined midway (partial)", () => {
    draw(
      live({
        run: acmeRunFrame({
          trigger: "resume",
          items: acmeSteps({
            account: "skipped",
            order: "done",
            challenge: "done",
            validate: "running",
          }),
        }),
      }),
      { names: "tierplan.example.com, agents.example.com" },
    );
    expect(
      ["account", "order", "challenge", "validate", "finalize", "stored"].map(stateOf),
    ).toEqual(["skipped", "done", "done", "running", "waiting", "waiting"]);
    expect(screen.getByText("Account already registered.")).toBeTruthy();
    // Only the step that runs has its hint.
    expect(
      screen.getByText("Waiting for Let's Encrypt to check tierplan.example.com."),
    ).toBeTruthy();
    expect(screen.queryByText("Serving the HTTP-01 token on port 80.")).toBeNull();
  });

  it("words each hint while its step runs, naming the names", () => {
    draw(live({ run: acmeRunFrame({ items: acmeSteps({ order: "running" }) }) }), {
      names: "a.example.com, b.example.com",
    });
    expect(
      screen.getByText("Asking for a certificate for a.example.com, b.example.com."),
    ).toBeTruthy();
  });

  it("marks the failing step with the failure mark and the header says it failed (error)", () => {
    draw(
      live({
        finished: true,
        status: "closed",
        run: acmeRunFrame({
          state: "failed",
          items: acmeSteps({
            account: "skipped",
            order: "done",
            challenge: "done",
            validate: "failed",
          }),
        }),
      }),
    );
    expect(screen.getByText("Issuing failed.")).toBeTruthy();
    const failed = steps().querySelector('[data-step="validate"]') as HTMLElement;
    expect(failed.getAttribute("data-state")).toBe("failed");
    expect(failed.querySelector("svg")?.getAttribute("class")).toContain("text-destructive");
    // The steps after it were not reached.
    expect(stateOf("finalize")).toBe("waiting");
  });

  it("says Reconnecting... while the stream is down, and not once the run has ended (overflow)", () => {
    const { unmount } = draw(live({ status: "reconnecting" }));
    expect(screen.getByText("Reconnecting...")).toBeTruthy();
    unmount();
    draw(live({ status: "reconnecting", finished: true, run: acmeRunFrame({ state: "done" }) }));
    expect(screen.queryByText("Reconnecting...")).toBeNull();
  });

  it("draws names the server sent as text, never as markup", () => {
    draw(live({ run: acmeRunFrame({ items: acmeSteps({ order: "running" }) }) }), {
      names: "<img src=x onerror=alert(1)>",
    });
    expect(steps().querySelector("img")).toBeNull();
    expect(steps().textContent).toContain("<img src=x onerror=alert(1)>");
  });

  it("lets a long hint break anywhere so a narrow screen does not scroll sideways", () => {
    draw(live({ run: acmeRunFrame({ items: acmeSteps({ order: "running" }) }) }), {
      names: `${"long-label.".repeat(12)}example.com`,
    });
    const hint = steps().querySelector('[data-step="order"] .text-xs') as HTMLElement;
    expect(hint.className).toContain("break-all");
    expect((hint.parentElement as HTMLElement).className).toContain("min-w-0");
  });
});
