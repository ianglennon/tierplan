// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { IngressCard } from "@/features/ingress/ingress-card";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";
import { waitUnderFakeClock } from "@/test-settle";

// The Ingress card's stuck states and its extra lines (D-05, UI-SPEC S6 card 1): a switch that failed or did not
// finish offers both directions, and the lines that only apply sometimes are drawn only then.

const ENV = "e1";
const BASE = `/api/env/${ENV}/ingress`;

type Json = Record<string, unknown>;

let status: Json;
let previews: string[];
let me: ReturnType<typeof makeMe>;
let client: QueryClient;

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

beforeEach(() => {
  vi.useFakeTimers({
    toFake: ["Date", "setTimeout", "clearTimeout", "setInterval", "clearInterval"],
  });
  setCsrfToken(null);
  status = statusBody();
  previews = [];
  me = makeMe({ envs: { [ENV]: "admin" } });
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false } },
  });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(me);
      if (path === `${BASE}/status`) return Response.json(status);
      if (path === `${BASE}/enable/preview` || path === `${BASE}/disable/preview`) {
        previews.push(path);
        return Response.json({ findings: [], redeploy: [], creates: {} });
      }
      if (path === `${BASE}/enable`) return Response.json({ jobId: "j1" }, { status: 202 });
      if (path === `${BASE}/jobs/j1`) {
        return Response.json({
          id: "j1",
          kind: "ingress_enable",
          state: "running",
          items: [],
          output: [],
          createdAt: "2026-11-26T10:00:00Z",
          finishedAt: "",
          startedBy: "u1",
        });
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
          <IngressCard envId={ENV} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

function button(name: string): HTMLButtonElement {
  return screen.getByRole("button", { name }) as HTMLButtonElement;
}

describe("a switch that failed or did not finish", () => {
  it("shows Turning on failed with its warning and offers both directions", async () => {
    status = statusBody({ enableFailed: true });
    mount();
    await waitUnderFakeClock(() => expect(screen.getByText("Turning on failed")).toBeTruthy());
    expect(
      screen.getByText(
        "Turning ingress on failed. The objects it made are still in the swarm. Turn it on again to carry on, or off to remove them.",
      ),
    ).toBeTruthy();
    expect(button("Turn on ingress")).toBeTruthy();
    expect(button("Turn off ingress")).toBeTruthy();
  });

  it("opens the review on the disable preview from Turn off ingress", async () => {
    status = statusBody({ enableFailed: true });
    mount();
    await waitUnderFakeClock(() => expect(screen.getByText("Turning on failed")).toBeTruthy());
    fireEvent.click(button("Turn off ingress"));
    const dialog = await waitUnderFakeClock(() =>
      screen.getByRole("dialog", { name: "Turn off ingress?" }),
    );
    expect(dialog).toBeTruthy();
    await waitUnderFakeClock(() => expect(previews).toEqual([`${BASE}/disable/preview`]));
  });

  it("opens the review on the enable preview from Turn on ingress", async () => {
    status = statusBody({ enableFailed: true });
    mount();
    await waitUnderFakeClock(() => expect(screen.getByText("Turning on failed")).toBeTruthy());
    fireEvent.click(button("Turn on ingress"));
    await waitUnderFakeClock(() => screen.getByRole("dialog", { name: "Turn on ingress?" }));
    await waitUnderFakeClock(() => expect(previews).toEqual([`${BASE}/enable/preview`]));
  });

  it("shows Turning off didn't finish with its warning and offers both directions", async () => {
    status = statusBody({ disablingIncomplete: true });
    mount();
    await waitUnderFakeClock(() =>
      expect(screen.getByText("Turning off didn't finish")).toBeTruthy(),
    );
    expect(
      screen.getByText(
        "Turning ingress off didn't finish. Something is left in the swarm. Turn it off again to remove it.",
      ),
    ).toBeTruthy();
    expect(button("Turn on ingress")).toBeTruthy();
    expect(button("Turn off ingress")).toBeTruthy();
  });

  it("offers one direction and no warning when nothing is stuck", async () => {
    mount();
    await waitUnderFakeClock(() => expect(screen.getByText("Off")).toBeTruthy());
    expect(button("Turn on ingress")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Turn off ingress" })).toBeNull();
    expect(screen.queryByText(/failed/)).toBeNull();
    expect(screen.queryByText(/certificate cache volume/)).toBeNull();
  });
});

describe("the lines that apply only sometimes", () => {
  it("draws the certificate cache volumes still to be wiped, in the plural", async () => {
    status = statusBody({ pendingWipes: 2 });
    mount();
    await waitUnderFakeClock(() =>
      expect(
        screen.getByText(
          "2 certificate cache volumes are still to be wiped on nodes TierPlan couldn't reach.",
        ),
      ).toBeTruthy(),
    );
  });

  it("names the first 10 stacks to redeploy and says how many more", async () => {
    const redeploy = Array.from({ length: 12 }, (_, i) => ({
      stackId: `s${i}`,
      stackName: `stack-${i}`,
      holdsClaims: false,
    }));
    status = statusBody({ enabled: true, redeploy });
    mount();
    await waitUnderFakeClock(() =>
      expect(
        screen.getByText("12 stacks need a redeploy to join the ingress network."),
      ).toBeTruthy(),
    );
    for (let i = 0; i < 10; i += 1) expect(screen.getByText(`stack-${i}`)).toBeTruthy();
    expect(screen.queryByText("stack-10")).toBeNull();
    expect(screen.queryByText("stack-11")).toBeNull();
    expect(screen.getByText("and 2 more")).toBeTruthy();
  });

  it("does not draw the redeploy line while ingress is off", async () => {
    status = statusBody({
      redeploy: [{ stackId: "s1", stackName: "shop", holdsClaims: true }],
    });
    mount();
    await waitUnderFakeClock(() => expect(screen.getByText("Off")).toBeTruthy());
    expect(screen.queryByText(/need a redeploy|needs a redeploy/)).toBeNull();
  });
});

describe("a job started in this session", () => {
  it("shows the running line with Show progress and disables both buttons", async () => {
    status = statusBody({ enableFailed: true });
    mount();
    await waitUnderFakeClock(() => expect(screen.getByText("Turning on failed")).toBeTruthy());
    fireEvent.click(button("Turn on ingress"));
    const dialog = await waitUnderFakeClock(() =>
      screen.getByRole("dialog", { name: "Turn on ingress?" }),
    );
    await waitUnderFakeClock(() => {
      const confirm = within(dialog).getByRole("button", {
        name: "Turn on ingress",
      }) as HTMLButtonElement;
      expect(confirm.disabled).toBe(false);
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Turn on ingress" }));
    await waitUnderFakeClock(() => screen.getByRole("dialog", { name: "Turning ingress on…" }));

    // The dialog is modal, so the card behind it is hidden from the accessibility tree: read it by text.
    await waitUnderFakeClock(() =>
      expect(screen.getByText("Show progress", { selector: "button" })).toBeTruthy(),
    );
    expect(screen.getAllByText("Turning ingress on…").length).toBeGreaterThan(1);
    for (const name of ["Turn on ingress", "Turn off ingress"]) {
      const card = screen.getAllByText(name, { selector: "button" });
      expect(card.length).toBeGreaterThan(0);
      for (const el of card) expect((el as HTMLButtonElement).disabled).toBe(true);
    }
  });
});
