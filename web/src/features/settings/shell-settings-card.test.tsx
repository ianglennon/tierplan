// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { ShellSettingsCard } from "@/features/settings/shell-settings-card";
import "@/i18n";
import { makeMe } from "@/test-me";

// The "Container shells" card on Settings > General (plan 04-23, D-29, D-31, E7).

const toastSuccess = vi.hoisted(() => vi.fn());
vi.mock("@/lib/toast", () => ({ toastSuccess, toastError: vi.fn() }));

interface Sent {
  method: string;
  path: string;
  body?: unknown;
}
let sent: Sent[];
let held: { idleMinutes: number; maxPerUser: number };
let me: ReturnType<typeof makeMe>;
let getAnswer: () => Response | Promise<Response>;
let patchAnswer: (body: Record<string, number>) => Response | Promise<Response>;

beforeEach(() => {
  sent = [];
  held = { idleMinutes: 30, maxPerUser: 10 };
  me = makeMe({ instanceAdmin: true });
  getAnswer = () => Response.json(held);
  patchAnswer = (body) => {
    held = { ...held, ...body };
    return Response.json(held);
  };
  toastSuccess.mockClear();
  setCsrfToken(null);
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const body =
        req.method === "PATCH"
          ? await req
              .clone()
              .json()
              .catch(() => undefined)
          : undefined;
      sent.push({ method: req.method, path: url.pathname, body });
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/settings/shells") {
        return req.method === "PATCH" ? patchAnswer(body as Record<string, number>) : getAnswer();
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  queryClient.clear();
});

function renderCard() {
  return render(
    <QueryClientProvider client={queryClient}>
      <ShellSettingsCard />
    </QueryClientProvider>,
  );
}

const patches = () => sent.filter((s) => s.method === "PATCH");

async function loaded() {
  const idle = await screen.findByLabelText("Close idle shells after (minutes)");
  await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
  await act(async () => {});
  return {
    idle: idle as HTMLInputElement,
    cap: screen.getByLabelText("Open shells per person") as HTMLInputElement,
  };
}

const save = () => screen.getByRole("button", { name: "Save settings" });

function type(input: HTMLInputElement, value: string) {
  fireEvent.change(input, { target: { value } });
}

describe("the Container shells card", () => {
  it("shows the title, the caption, both fields with their helper text and a disabled Save", async () => {
    renderCard();
    const { idle, cap } = await loaded();
    expect(screen.getByRole("heading", { name: "Container shells" })).toBeTruthy();
    expect(screen.getByText("Limits for shells opened from the browser.")).toBeTruthy();
    expect(
      screen.getByText(
        "A shell with no input or output for this long is closed, after a warning 1 minute before.",
      ),
    ).toBeTruthy();
    expect(screen.getByText("The most shells one person can have open at once.")).toBeTruthy();
    expect(idle.type).toBe("number");
    expect(cap.type).toBe("number");
    expect(save().hasAttribute("disabled")).toBe(true);
  });

  it("holds the current values, which are the defaults 30 and 10 until they are changed, never blank", async () => {
    renderCard();
    const { idle, cap } = await loaded();
    expect(idle.value).toBe("30");
    expect(cap.value).toBe("10");
  });

  it("shows skeleton fields and a Save that is disabled until the settings load", async () => {
    let release: (r: Response) => void = () => {};
    getAnswer = () => new Promise<Response>((resolve) => (release = resolve));
    renderCard();
    expect(await screen.findByText("Loading the shell limits…")).toBeTruthy();
    expect(screen.queryByLabelText("Close idle shells after (minutes)")).toBeNull();
    expect(save().hasAttribute("disabled")).toBe(true);
    await act(async () => release(Response.json(held)));
    expect(await screen.findByLabelText("Close idle shells after (minutes)")).toBeTruthy();
  });

  it("says so, with Try again, when the limits cannot be loaded", async () => {
    getAnswer = () => Response.json({}, { status: 500 });
    renderCard();
    expect(await screen.findByText("Couldn't load the shell limits.")).toBeTruthy();
    getAnswer = () => Response.json(held);
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByLabelText("Close idle shells after (minutes)")).toBeTruthy();
  });

  it("saves a change, sending only the limit that changed, and says 'Settings saved.'", async () => {
    renderCard();
    const { idle } = await loaded();
    type(idle, "45");
    await waitFor(() => expect(save().hasAttribute("disabled")).toBe(false));
    fireEvent.click(save());
    await waitFor(() => expect(patches()).toHaveLength(1));
    expect(patches()[0]?.body).toEqual({ idleMinutes: 45 });
    await waitFor(() => expect(toastSuccess).toHaveBeenCalledWith("Settings saved."));
    await waitFor(() => expect(save().hasAttribute("disabled")).toBe(true));
    expect(idle.value).toBe("45");
  });

  it("refuses a value out of range with its own message, and keeps Save disabled", async () => {
    renderCard();
    const { idle, cap } = await loaded();
    type(idle, "4");
    expect(await screen.findByText("Enter a whole number from 5 to 240.")).toBeTruthy();
    expect(idle.getAttribute("aria-invalid")).toBe("true");
    expect(save().hasAttribute("disabled")).toBe(true);
    type(idle, "241");
    expect(await screen.findByText("Enter a whole number from 5 to 240.")).toBeTruthy();
    type(cap, "51");
    expect(await screen.findByText("Enter a whole number from 1 to 50.")).toBeTruthy();
    type(cap, "0");
    expect(await screen.findByText("Enter a whole number from 1 to 50.")).toBeTruthy();
    type(cap, "");
    expect(await screen.findByText("Enter a whole number from 1 to 50.")).toBeTruthy();
    expect(patches()).toHaveLength(0);
  });

  it("with one field valid and the other invalid, shows only the invalid field's error and keeps Save disabled", async () => {
    renderCard();
    const { idle, cap } = await loaded();
    type(idle, "60"); // a valid change
    type(cap, "99"); // an invalid one
    expect(await screen.findByText("Enter a whole number from 1 to 50.")).toBeTruthy();
    expect(screen.queryByText("Enter a whole number from 5 to 240.")).toBeNull();
    expect(idle.getAttribute("aria-invalid")).toBe("false");
    expect(save().hasAttribute("disabled")).toBe(true);
    type(cap, "10"); // back to what the server holds: the valid change alone is enough
    await waitFor(() => expect(save().hasAttribute("disabled")).toBe(false));
    expect(screen.queryByText("Enter a whole number from 1 to 50.")).toBeNull();
  });

  it("keeps Save disabled for an unchanged form, and again after the change is undone", async () => {
    renderCard();
    const { idle } = await loaded();
    expect(save().hasAttribute("disabled")).toBe(true);
    type(idle, "31");
    await waitFor(() => expect(save().hasAttribute("disabled")).toBe(false));
    type(idle, "30");
    await waitFor(() => expect(save().hasAttribute("disabled")).toBe(true));
  });

  it("accepts the boundary values 5 minutes and 1 shell, and 240 and 50", async () => {
    renderCard();
    const { idle, cap } = await loaded();
    type(idle, "5");
    type(cap, "1");
    await waitFor(() => expect(save().hasAttribute("disabled")).toBe(false));
    expect(screen.queryByText(/Enter a whole number/)).toBeNull();
    fireEvent.click(save());
    await waitFor(() => expect(patches()[0]?.body).toEqual({ idleMinutes: 5, maxPerUser: 1 }));
    await waitFor(() => expect(save().hasAttribute("disabled")).toBe(true));
    // The saved values start the form again, so its fields are new elements.
    const next = await loaded();
    type(next.idle, "240");
    type(next.cap, "50");
    await waitFor(() => expect(save().hasAttribute("disabled")).toBe(false));
    expect(screen.queryByText(/Enter a whole number/)).toBeNull();
  });

  it("says the generic failure inline when saving fails, and keeps what was typed", async () => {
    patchAnswer = () => Response.json({ title: "x", status: 500 }, { status: 500 });
    renderCard();
    const { idle } = await loaded();
    type(idle, "45");
    await waitFor(() => expect(save().hasAttribute("disabled")).toBe(false));
    fireEvent.click(save());
    expect(await screen.findByText("Something went wrong. Try again.")).toBeTruthy();
    expect(idle.value).toBe("45");
    expect(toastSuccess).not.toHaveBeenCalled();
  });

  it("puts a range error the server found on its field", async () => {
    patchAnswer = () =>
      Response.json(
        {
          type: "validation_failed",
          title: "Unprocessable Entity",
          status: 422,
          errors: [{ location: "body.idleMinutes", message: "expected number >= 5" }],
        },
        { status: 422 },
      );
    renderCard();
    const { idle } = await loaded();
    type(idle, "45");
    await waitFor(() => expect(save().hasAttribute("disabled")).toBe(false));
    fireEvent.click(save());
    expect(await screen.findByText("Enter a whole number from 5 to 240.")).toBeTruthy();
  });

  it("is read only for someone who may read the settings but not change them", async () => {
    me = makeMe({ permissions: { global: ["settings.read"], environments: {} } });
    renderCard();
    const { idle, cap } = await loaded();
    expect(idle.readOnly).toBe(true);
    expect(cap.readOnly).toBe(true);
    expect(screen.queryByRole("button", { name: "Save settings" })).toBeNull();
    expect(screen.getByText("You can view these settings but not change them.")).toBeTruthy();
  });
});
