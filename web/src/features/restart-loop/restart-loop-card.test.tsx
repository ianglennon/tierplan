// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { RestartLoopCard } from "@/features/restart-loop/restart-loop-card";
import "@/i18n";
import { makeMe } from "@/test-me";

// The Restart-loop detection card of Settings > Environments > {env} (plan 06-35, RES-13, D-14, Q3, Q5):
// two numbers, their bounds, the retention helper and a save. Env Admin only.

interface Sent {
  method: string;
  path: string;
  body: unknown;
}

let sent: Sent[];
let settings: { failures: number; windowMinutes: number; historyLimit: number };
let putAnswer: (body: { failures: number; windowMinutes: number }) => Response;
let me = makeMe({ envs: { e1: "admin" } });

beforeEach(() => {
  sent = [];
  me = makeMe({ envs: { e1: "admin" } });
  settings = { failures: 3, windowMinutes: 10, historyLimit: 5 };
  putAnswer = (body) => Response.json({ ...settings, ...body });
  setCsrfToken("csrf-1");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const body =
        req.method === "PUT"
          ? await req
              .clone()
              .json()
              .catch(() => undefined)
          : undefined;
      sent.push({ method: req.method, path, body });
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/env/e1/restart-loop-settings") {
        return req.method === "PUT" ? putAnswer(body) : Response.json(settings);
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  setCsrfToken(null);
});

async function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <RestartLoopCard envId="e1" />
      <Toaster />
    </QueryClientProvider>,
  );
  await act(async () => {});
}

const failures = () =>
  screen.findByRole("textbox", { name: "Failed tasks" }) as Promise<HTMLInputElement>;
const window_ = () => screen.getByRole("textbox", { name: "Within (minutes)" }) as HTMLInputElement;
const puts = () => sent.filter((s) => s.method === "PUT");

function type(el: HTMLInputElement, value: string) {
  fireEvent.change(el, { target: { value } });
}

describe("RestartLoopCard", () => {
  it("shows its title, caption and both fields with the environment's values and helpers", async () => {
    await mount();
    expect(await screen.findByRole("heading", { name: "Restart-loop detection" })).toBeTruthy();
    expect(
      screen.getByText("Flag a service, or a container with a restart policy, that keeps failing."),
    ).toBeTruthy();
    expect((await failures()).value).toBe("3");
    expect(window_().value).toBe("10");
    expect(screen.getByText("How many failures inside the window raise the flag.")).toBeTruthy();
    expect(
      screen.getByText("The flag clears after a full window with no new failures."),
    ).toBeTruthy();
  });

  it("says how many failed tasks Docker keeps per slot, from its history limit", async () => {
    await mount();
    await failures();
    expect(
      screen.getByText(
        "Docker keeps 4 failed tasks per slot. Coaming also counts failures it saw in earlier checks, so a value above 4 is met reliably only by loops slower than its 15-second check.",
      ),
    ).toBeTruthy();
  });

  it("leaves the retention line out while the history limit is not known", async () => {
    settings.historyLimit = 0;
    await mount();
    await failures();
    expect(screen.queryByText(/Docker keeps/)).toBeNull();
  });

  it.each([
    ["1", "Enter a whole number from 2 to 50."],
    ["51", "Enter a whole number from 2 to 50."],
    ["3.5", "Enter a whole number from 2 to 50."],
    ["", "Enter a whole number from 2 to 50."],
    ["abc", "Enter a whole number from 2 to 50."],
  ])("refuses %j for Failed tasks and sends nothing", async (value, message) => {
    await mount();
    type(await failures(), value);
    fireEvent.click(screen.getByRole("button", { name: "Save settings" }));
    expect(await screen.findByText(message)).toBeTruthy();
    expect(puts()).toEqual([]);
  });

  it.each(["0", "121", "1.5", ""])(
    "refuses %j for Within (minutes) and sends nothing",
    async (value) => {
      await mount();
      await failures();
      type(window_(), value);
      fireEvent.click(screen.getByRole("button", { name: "Save settings" }));
      expect(await screen.findByText("Enter a whole number from 1 to 120.")).toBeTruthy();
      expect(puts()).toEqual([]);
    },
  );

  it("accepts the bounds themselves", async () => {
    await mount();
    type(await failures(), "50");
    type(window_(), "120");
    fireEvent.click(screen.getByRole("button", { name: "Save settings" }));
    await waitFor(() => expect(puts()).toHaveLength(1));
    expect(puts()[0]?.body).toEqual({ failures: 50, windowMinutes: 120 });
  });

  it("saves the two numbers and toasts 'Settings saved.'", async () => {
    await mount();
    type(await failures(), "5");
    type(window_(), "20");
    fireEvent.click(screen.getByRole("button", { name: "Save settings" }));
    await waitFor(() => expect(puts()).toHaveLength(1));
    expect(puts()[0]).toEqual({
      method: "PUT",
      path: "/api/env/e1/restart-loop-settings",
      body: { failures: 5, windowMinutes: 20 },
    });
    expect(await screen.findByText("Settings saved.")).toBeTruthy();
  });

  it("shows the server's refusal inline and keeps what was typed", async () => {
    putAnswer = () => Response.json({ status: 422, type: "failures_invalid" }, { status: 422 });
    await mount();
    type(await failures(), "5");
    fireEvent.click(screen.getByRole("button", { name: "Save settings" }));
    expect(await screen.findByText("Enter a whole number from 2 to 50.")).toBeTruthy();
    expect((await failures()).value).toBe("5");
  });

  it("shows a save error inline when the server could not be reached", async () => {
    putAnswer = () => new Response("boom", { status: 500 });
    await mount();
    type(await failures(), "5");
    fireEvent.click(screen.getByRole("button", { name: "Save settings" }));
    expect(
      await screen.findByText("Couldn't save the settings. Check the connection and try again."),
    ).toBeTruthy();
  });

  it("renders nothing, and reads nothing, for someone without environments.configure", async () => {
    me = makeMe({ envs: { e1: "operator" } });
    await mount();
    await act(async () => {});
    expect(screen.queryByRole("heading", { name: "Restart-loop detection" })).toBeNull();
    expect(sent.filter((s) => s.path.endsWith("restart-loop-settings"))).toEqual([]);
  });
});
