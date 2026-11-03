// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setCsrfToken } from "@/api/client";
import { SCHEDULE_DEBOUNCE_MS } from "@/api/janitor";
import { TooltipProvider } from "@/components/ui/tooltip";
import { cronOf, type ScheduleDraft, scheduleDraftOf } from "@/features/janitor/policy-diff";
import { ScheduleField, type ScheduleProblem } from "@/features/janitor/schedule-field";
import { formatServerTime } from "@/features/janitor/status-text";
import "@/i18n";
import { waitUnderFakeClock } from "@/test-settle";

const ENV = "e1";

// What the stubbed server answers to schedule-preview, and how often it was asked and about what.
let answer: Record<string, unknown>;
let asked: string[];
let client: QueryClient;

beforeEach(() => {
  setCsrfToken(null);
  asked = [];
  answer = {
    valid: true,
    problem: "",
    nextRuns: ["2026-11-04T03:07:12Z", "2026-11-05T03:07:12Z", "2026-11-06T03:07:12Z"],
    timezone: "Europe/London",
  };
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === `/api/env/${ENV}/janitor/schedule-preview`) {
        asked.push(((await req.json()) as { schedule: string }).schedule);
        return Response.json(answer);
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

const daily = scheduleDraftOf("0 3 * * *", "daily");

function Harness({
  initial = daily,
  locked,
  problem,
}: {
  initial?: ScheduleDraft;
  locked?: string;
  problem?: ScheduleProblem | null;
}) {
  const [value, setValue] = useState(initial);
  return (
    <>
      <ScheduleField
        envId={ENV}
        id="sched"
        value={value}
        onChange={setValue}
        timezone="Europe/London"
        locked={locked}
        problem={problem}
      />
      <span data-testid="cron">{cronOf(value)}</span>
    </>
  );
}

function mount(props: Parameters<typeof Harness>[0] = {}) {
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <Harness {...props} />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

const cron = () => screen.getByTestId("cron").textContent;
const preset = () => screen.getByRole("combobox", { name: "Run" });

async function pick(select: HTMLElement, option: string) {
  fireEvent.keyDown(select, { key: "Enter" });
  fireEvent.click(await screen.findByRole("option", { name: option }));
}

describe("ScheduleFieldPresets", () => {
  it("Daily asks for a time of day, 03:00 to begin with, and writes it as cron", () => {
    mount();
    expect(preset().textContent).toBe("Daily");
    const at = screen.getByLabelText("At") as HTMLInputElement;
    expect(at.type).toBe("time");
    expect(at.value).toBe("03:00");
    expect(cron()).toBe("0 3 * * *");
    fireEvent.change(at, { target: { value: "14:30" } });
    expect(cron()).toBe("30 14 * * *");
    expect(screen.queryByLabelText("On")).toBeNull();
    expect(screen.queryByLabelText("Cron expression")).toBeNull();
  });

  it("Weekly adds the weekday, Monday to Sunday, and the time", async () => {
    mount();
    await pick(preset(), "Weekly");
    expect(cron()).toBe("0 3 * * 1");
    fireEvent.keyDown(screen.getByRole("combobox", { name: "On" }), { key: "Enter" });
    const days = (await screen.findAllByRole("option")).map((o) => o.textContent);
    expect(days).toEqual([
      "Monday",
      "Tuesday",
      "Wednesday",
      "Thursday",
      "Friday",
      "Saturday",
      "Sunday",
    ]);
    fireEvent.click(screen.getByRole("option", { name: "Sunday" }));
    expect(cron()).toBe("0 3 * * 0");
    fireEvent.change(screen.getByLabelText("At"), { target: { value: "07:15" } });
    expect(cron()).toBe("15 7 * * 0");
  });

  it("Hourly and Every 6 hours need no further input", async () => {
    mount();
    await pick(preset(), "Hourly");
    expect(cron()).toBe("0 * * * *");
    expect(screen.queryByLabelText("At")).toBeNull();
    await pick(preset(), "Every 6 hours");
    expect(cron()).toBe("0 */6 * * *");
    expect(screen.queryByLabelText("At")).toBeNull();
    expect(screen.queryByLabelText("Cron expression")).toBeNull();
  });

  it("names the server's time zone in its helper", () => {
    mount();
    expect(
      screen.getByText(
        "Server time (Europe/London). The exact start is spread out a little per environment so many environments don't run at once.",
      ),
    ).toBeTruthy();
  });

  it("shows a saved expression no preset writes as Custom", () => {
    mount({ initial: scheduleDraftOf("*/20 * * * *", "custom") });
    expect(preset().textContent).toBe("Custom (cron)");
    expect((screen.getByLabelText("Cron expression") as HTMLInputElement).value).toBe(
      "*/20 * * * *",
    );
  });
});

describe("ScheduleFieldCustom", () => {
  it("starts the cron input from the expression the old choice wrote, in mono with no spellcheck", async () => {
    mount();
    await pick(preset(), "Custom (cron)");
    const input = screen.getByLabelText("Cron expression") as HTMLInputElement;
    expect(input.value).toBe("0 3 * * *");
    expect(input.getAttribute("spellcheck")).toBe("false");
    expect(input.placeholder).toBe("0 3 * * *");
    expect(input.className).toContain("font-mono");
  });

  it("asks the server for the next runs once typing has paused and shows its times as it gave them", async () => {
    vi.useFakeTimers();
    mount({ initial: scheduleDraftOf("0 3 * * *", "custom") });
    // The expression it opens with is asked about at once: there is no typing to wait for.
    await waitUnderFakeClock(() => expect(asked).toEqual(["0 3 * * *"]));
    asked.length = 0;
    const input = screen.getByLabelText("Cron expression");

    // Two keystrokes inside the wait are one question.
    fireEvent.change(input, { target: { value: "*/2" } });
    fireEvent.change(input, { target: { value: "*/20 * * * *" } });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(SCHEDULE_DEBOUNCE_MS - 1);
    });
    expect(asked).toEqual([]);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    const line = await waitUnderFakeClock(() => {
      const status = screen.getByRole("status");
      expect(status.textContent).toContain("Next runs:");
      return status;
    });
    expect(asked).toEqual(["*/20 * * * *"]);
    const times = ["2026-11-04T03:07:12Z", "2026-11-05T03:07:12Z", "2026-11-06T03:07:12Z"]
      .map((at) => formatServerTime(at, "Europe/London"))
      .join("; ");
    expect(line.textContent).toBe(`Next runs: ${times}.`);
  });

  it("says an expression is not valid in the server's words", async () => {
    vi.useFakeTimers();
    answer = { valid: false, problem: "invalid", nextRuns: null, timezone: "Europe/London" };
    mount({ initial: scheduleDraftOf("nonsense", "custom") });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(SCHEDULE_DEBOUNCE_MS);
    });
    await waitUnderFakeClock(() =>
      expect(screen.getByRole("status").textContent).toBe(
        "Enter a cron expression like 0 3 * * * (minute hour day month weekday).",
      ),
    );
    expect(screen.getByLabelText("Cron expression").getAttribute("aria-invalid")).toBe("true");
  });

  it("says an expression is too frequent in the server's words", async () => {
    vi.useFakeTimers();
    answer = { valid: false, problem: "too_frequent", nextRuns: null, timezone: "Europe/London" };
    mount({ initial: scheduleDraftOf("* * * * *", "custom") });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(SCHEDULE_DEBOUNCE_MS);
    });
    await waitUnderFakeClock(() =>
      expect(screen.getByRole("status").textContent).toBe(
        "Runs can be at most once every 15 minutes.",
      ),
    );
  });

  it("asks nothing for a preset or a blank expression", async () => {
    vi.useFakeTimers();
    mount();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(SCHEDULE_DEBOUNCE_MS * 3);
    });
    expect(asked).toEqual([]);
    expect(screen.queryByRole("status")).toBeNull();
  });
});

describe("ScheduleFieldProblems", () => {
  it("draws a refusal the card knows of in the place of the live line", () => {
    mount({ problem: "too_frequent" });
    expect(screen.getByRole("status").textContent).toBe(
      "Runs can be at most once every 15 minutes.",
    );
  });

  it("asks for a time of day when the time is missing", () => {
    mount({ problem: "time_missing" });
    expect(screen.getByRole("status").textContent).toBe("Pick a time of day.");
  });
});

describe("ScheduleFieldReadOnly", () => {
  const reason = "Needs the janitor policy permission on Prod";

  it("holds the select shut and the inputs read-only, with the reason on each", async () => {
    mount({ locked: reason, initial: scheduleDraftOf("0 3 * * 1", "weekly") });
    fireEvent.keyDown(preset(), { key: "Enter" });
    fireEvent.click(preset());
    expect(screen.queryByRole("option")).toBeNull();
    expect(preset().getAttribute("aria-disabled")).toBe("true");
    const at = screen.getByLabelText("At") as HTMLInputElement;
    expect(at.readOnly).toBe(true);
    fireEvent.change(at, { target: { value: "05:00" } });
    // A read-only input ignores typing in a browser; the field is not asked to change here.
    expect(screen.getAllByText(reason).length).toBeGreaterThan(0);
    expect(within(document.body).getAllByText(reason)[0]).toBeTruthy();
  });
});
