// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { StackDriftCause, StackDriftService } from "@/api/stack-drift";
import type { StackItem } from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { DriftAlert } from "@/features/stack-drift/drift-alert";
import { DriftCause } from "@/features/stack-drift/drift-cause";
import { DriftServiceList } from "@/features/stack-drift/drift-service-list";
import { DriftBadge } from "@/features/stacks/drift-badge";
import "@/i18n";
import { TestRouter } from "@/test-router";

// The cause of drift (plan 08-22, STK-14, D-24): who changed a service in TierPlan and when, how many changes, that it
// came from outside, and that it was there when the stack was adopted. The clock is held, so a relative time reads the
// same on every run.

const NOW = new Date("2026-10-20T14:00:00Z");

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date", "setInterval", "clearInterval"] });
  vi.setSystemTime(NOW);
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

const scaled: StackDriftCause = {
  action: "scaled",
  service: "web",
  by: "alice",
  at: "2026-10-20T12:00:00Z",
  from: "1",
  to: "2",
};

const adopted: StackDriftCause = {
  action: "adopted",
  service: "web",
  by: "",
  at: "",
  from: "",
  to: "",
};

// sentence is the text a cause line reads as, whatever elements its time is in.
function sentence(cause: StackDriftCause | null | undefined, count?: number, stackName?: string) {
  render(
    <TooltipProvider>
      <p data-testid="line">
        <DriftCause cause={cause} count={count} stackName={stackName} />
      </p>
    </TooltipProvider>,
  );
  return screen.getByTestId("line").textContent;
}

describe("the cause line", () => {
  it("says who scaled a service, from what to what, in TierPlan, and when", () => {
    expect(sentence(scaled)).toBe("web was scaled from 1 to 2 by alice in TierPlan, 2 h ago.");
  });

  it("leaves the old count out when the audit row did not say it", () => {
    expect(sentence({ ...scaled, from: "" })).toBe(
      "web was scaled to 2 by alice in TierPlan, 2 h ago.",
    );
  });

  it("says a service was rolled back or edited", () => {
    expect(sentence({ ...scaled, action: "rolled_back", from: "", to: "" })).toBe(
      "web was rolled back by alice in TierPlan, 2 h ago.",
    );
    cleanup();
    expect(sentence({ ...scaled, action: "edited", from: "", to: "" })).toBe(
      "web was edited by alice in TierPlan, 2 h ago.",
    );
  });

  it("counts the changes and names the latest when there are several", () => {
    expect(sentence(scaled, 3)).toBe("3 changes, the latest by alice in TierPlan, 2 h ago.");
  });

  it("says a change came from outside when nothing is known", () => {
    expect(sentence(undefined)).toBe("Changed outside TierPlan.");
    cleanup();
    expect(sentence(null, 0)).toBe("Changed outside TierPlan.");
  });

  it("says a difference was there when the stack was adopted", () => {
    expect(sentence(adopted, 0, "tracer")).toBe(
      "Differed from the rebuilt definition when tracer was adopted.",
    );
  });

  it("shows a name as text and not as markup", () => {
    expect(sentence({ ...scaled, by: "<b>mallory</b>" })).toBe(
      "web was scaled from 1 to 2 by <b>mallory</b> in TierPlan, 2 h ago.",
    );
    expect(screen.getByTestId("line").querySelector("b")).toBeNull();
  });
});

// item is the drift a list carries for a stack.
function item(over: Partial<NonNullable<StackItem["drift"]>>): StackItem["drift"] {
  return {
    state: "drifted",
    revision: 3,
    count: 1,
    causes: 0,
    checkedAt: "2026-10-20T13:59:00Z",
    ...over,
  };
}

// described is what the badge is described by: the words of its Tooltip, which a reader who never opens it gets too.
function described(drift: StackItem["drift"]) {
  render(
    <TooltipProvider>
      <DriftBadge drift={drift} />
    </TooltipProvider>,
  );
  const badge = screen.getByText("Drifted").closest("[aria-describedby]") as HTMLElement;
  return document.getElementById(badge.getAttribute("aria-describedby") ?? "")?.textContent;
}

describe("the Drifted badge", () => {
  it("adds who changed what in TierPlan to its Tooltip", () => {
    expect(described(item({ cause: scaled, causes: 1 }))).toBe(
      "Running state no longer matches revision #3. web was scaled by alice in TierPlan.",
    );
  });

  it("counts the changes when there are several", () => {
    expect(described(item({ count: 4, cause: scaled, causes: 2 }))).toBe(
      "Running state no longer matches revision #3. 2 changes, the latest by alice in TierPlan.",
    );
  });

  it("adds nothing when no cause is known", () => {
    expect(described(item({}))).toBe("Running state no longer matches revision #3.");
  });
});

function service(short: string, over: Partial<StackDriftService> = {}): StackDriftService {
  return {
    name: `tracer_${short}`,
    short,
    change: "changed",
    current: { Mode: { Replicated: { Replicas: 1 } } },
    next: { Mode: { Replicated: { Replicas: 2 } } },
    hiddenChanges: [],
    settings: 1,
    ...over,
  };
}

describe("the drifted services", () => {
  it("say why each differs, or that it changed outside TierPlan", () => {
    render(
      <TooltipProvider>
        <DriftServiceList
          stackName="tracer"
          services={[
            service("web", { cause: scaled }),
            service("api"),
            service("db", { change: "missing", current: {}, next: null }),
            service("cache", { cause: adopted }),
          ]}
        />
      </TooltipProvider>,
    );

    const lines = (name: string) => {
      const row = screen.getByText(name, { selector: '[data-slot="service-name"]' }).closest("li");
      return (row as HTMLElement).querySelector('[data-slot="drift-cause"]')?.textContent;
    };
    expect(lines("web")).toBe("web was scaled from 1 to 2 by alice in TierPlan, 2 h ago.");
    expect(lines("api")).toBe("Changed outside TierPlan.");
    expect(lines("db")).toBe("Changed outside TierPlan.");
    expect(lines("cache")).toBe("Differed from the rebuilt definition when tracer was adopted.");
  });

  it("keep the number of settings that differ plural-aware beside the cause", () => {
    render(
      <TooltipProvider>
        <DriftServiceList
          services={[service("web", { settings: 1 }), service("api", { settings: 3 })]}
        />
      </TooltipProvider>,
    );
    expect(screen.getByText("1 setting differs")).toBeTruthy();
    expect(screen.getByText("3 settings differ")).toBeTruthy();
  });
});

describe("the Overview alert", () => {
  // The alert offers Redeploy to restore, which reads the signed-in user and the stack; nothing here answers.
  beforeEach(() => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => Response.json({}, { status: 404 })),
    );
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  function alert(over: Partial<React.ComponentProps<typeof DriftAlert>["drift"] & object>) {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <DriftAlert
              envId="e1"
              stackId="s1"
              name="tracer"
              drift={{
                state: "drifted",
                revision: 4,
                count: 1,
                causes: 0,
                ...over,
              }}
            />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
    expect(screen.getByText("tracer has drifted from revision #4")).toBeTruthy();
    return document.querySelector('[data-slot="drift-alert"] [data-slot="drift-cause"]')
      ?.textContent;
  }

  it("names the cause", () => {
    expect(alert({ cause: scaled, causes: 1 })).toBe(
      "web was scaled from 1 to 2 by alice in TierPlan, 2 h ago.",
    );
  });

  it("says the change came from outside TierPlan when nothing is known", () => {
    expect(alert({})).toBe("Changed outside TierPlan.");
  });
});
