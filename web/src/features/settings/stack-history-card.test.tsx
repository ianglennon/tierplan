// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { StackHistoryCard } from "@/features/settings/stack-history-card";
import "@/i18n";
import { makeMe } from "@/test-me";

// The "Stack history" card on Settings > General (plan 08-06, STK-08, D-01).

const toastSuccess = vi.hoisted(() => vi.fn());
vi.mock("@/lib/toast", () => ({ toastSuccess, toastError: vi.fn() }));

interface Sent {
  method: string;
  path: string;
  search: string;
  body?: unknown;
}
let sent: Sent[];
let held: { kept: number; min: number; max: number; pruned: number };
let me: ReturnType<typeof makeMe>;
let getAnswer: () => Response | Promise<Response>;
let patchAnswer: (body: { kept: number }) => Response | Promise<Response>;
let previewAnswer: (keep: number) => Response | Promise<Response>;

beforeEach(() => {
  sent = [];
  held = { kept: 20, min: 5, max: 200, pruned: 0 };
  me = makeMe({ instanceAdmin: true });
  getAnswer = () => Response.json(held);
  patchAnswer = (body) => {
    held = { ...held, kept: body.kept };
    return Response.json(held);
  };
  previewAnswer = () => Response.json({ revisions: 3, stacks: 1 });
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
      sent.push({ method: req.method, path: url.pathname, search: url.search, body });
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/settings/stack-history/preview") {
        return previewAnswer(Number(url.searchParams.get("keep")));
      }
      if (url.pathname === "/api/settings/stack-history") {
        return req.method === "PATCH" ? patchAnswer(body as { kept: number }) : getAnswer();
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
      <StackHistoryCard />
    </QueryClientProvider>,
  );
}

const patches = () => sent.filter((s) => s.method === "PATCH" && s.path.endsWith("/stack-history"));
const previews = () => sent.filter((s) => s.path === "/api/settings/stack-history/preview");

async function loaded() {
  const field = await screen.findByLabelText("Revisions kept per stack");
  await waitFor(() => expect((field as HTMLInputElement).disabled).toBe(false));
  await waitFor(() => expect(queryClient.getQueryData(["me"])).toBeTruthy());
  await act(async () => {});
  return field as HTMLInputElement;
}

const save = () => screen.getByRole("button", { name: "Save settings" });

function type(input: HTMLInputElement, value: string) {
  fireEvent.change(input, { target: { value } });
}

async function typeAndSave(input: HTMLInputElement, value: string) {
  type(input, value);
  await waitFor(() => expect(save().hasAttribute("disabled")).toBe(false));
  fireEvent.click(save());
}

describe("the Stack history card", () => {
  it("shows the title, the caption, the field with its helper and a disabled Save", async () => {
    renderCard();
    const field = await loaded();
    expect(screen.getByRole("heading", { name: "Stack history" })).toBeTruthy();
    expect(screen.getByText("How many revisions Bayplan keeps for each stack.")).toBeTruthy();
    expect(
      screen.getByText(
        "The deployed revision and the last good one are always kept, even when older. Older revisions are deleted, along with their variables and secret files.",
      ),
    ).toBeTruthy();
    expect(field.type).toBe("number");
    expect(field.inputMode).toBe("numeric");
    expect(field.min).toBe("5");
    expect(field.max).toBe("200");
    expect(field.step).toBe("1");
    expect(field.value).toBe("20");
    expect(field.getAttribute("aria-describedby")).toContain("stack-revisions-kept-helper");
    expect(save().hasAttribute("disabled")).toBe(true);
  });

  it("disables the field and shows a skeleton until the setting loads", async () => {
    let release: (r: Response) => void = () => {};
    getAnswer = () => new Promise<Response>((resolve) => (release = resolve));
    renderCard();
    const field = (await screen.findByLabelText("Revisions kept per stack")) as HTMLInputElement;
    expect(field.disabled).toBe(true);
    expect(screen.getByText("Loading the stack history setting…")).toBeTruthy();
    expect(document.querySelector('[data-slot="skeleton"]')).toBeTruthy();
    expect(save().hasAttribute("disabled")).toBe(true);
    await act(async () => release(Response.json(held)));
    await waitFor(() => expect(field.disabled).toBe(false));
    expect(field.value).toBe("20");
    expect(document.querySelector('[data-slot="skeleton"]')).toBeNull();
  });

  it("says so, with Try again, when the setting cannot be loaded", async () => {
    getAnswer = () => Response.json({}, { status: 500 });
    renderCard();
    expect(await screen.findByText("Couldn't load the stack history setting.")).toBeTruthy();
    getAnswer = () => Response.json(held);
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByLabelText("Revisions kept per stack")).toBeTruthy();
  });

  it("refuses a value out of range or not whole with its message and keeps Save disabled", async () => {
    renderCard();
    const field = await loaded();
    for (const value of ["4", "201", "0", "-5", "7.5"]) {
      type(field, value);
      const alert = await screen.findByRole("alert");
      expect(within(alert).getByText("Use a whole number from 5 to 200.")).toBeTruthy();
      expect(field.getAttribute("aria-invalid")).toBe("true");
      expect(save().hasAttribute("disabled")).toBe(true);
    }
    expect(patches()).toHaveLength(0);
  });

  it("refuses a cleared field the same way and blocks submit", async () => {
    renderCard();
    const field = await loaded();
    type(field, "");
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("Use a whole number from 5 to 200.")).toBeTruthy();
    expect(field.getAttribute("aria-invalid")).toBe("true");
    expect(save().hasAttribute("disabled")).toBe(true);
    fireEvent.submit(field.closest("form") as HTMLFormElement);
    await act(async () => {});
    expect(patches()).toHaveLength(0);
    expect(previews()).toHaveLength(0);
  });

  it("accepts the boundary values 5 and 200", async () => {
    previewAnswer = () => Response.json({ revisions: 0, stacks: 0 });
    renderCard();
    const field = await loaded();
    type(field, "200");
    await waitFor(() => expect(save().hasAttribute("disabled")).toBe(false));
    expect(screen.queryByRole("alert")).toBeNull();
    type(field, "5");
    await waitFor(() => expect(save().hasAttribute("disabled")).toBe(false));
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("keeps Save disabled for an unchanged value, and again after the change is undone", async () => {
    renderCard();
    const field = await loaded();
    expect(save().hasAttribute("disabled")).toBe(true);
    type(field, "21");
    await waitFor(() => expect(save().hasAttribute("disabled")).toBe(false));
    type(field, "20");
    await waitFor(() => expect(save().hasAttribute("disabled")).toBe(true));
  });

  it("saves a raised limit at once, with no confirm and no count asked for", async () => {
    renderCard();
    const field = await loaded();
    await typeAndSave(field, "50");
    await waitFor(() => expect(patches()).toHaveLength(1));
    expect(patches()[0]?.body).toEqual({ kept: 50 });
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(previews()).toHaveLength(0);
    await waitFor(() => expect(toastSuccess).toHaveBeenCalledWith("Settings saved."));
    await waitFor(() => expect(save().hasAttribute("disabled")).toBe(true));
  });

  it("asks before a lowered limit deletes revisions, with Keep limit focused, and names the count", async () => {
    renderCard();
    const field = await loaded();
    await typeAndSave(field, "5");
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Delete older revisions?")).toBeTruthy();
    expect(
      within(dialog).getByText(
        "Lowering this to 5 deletes 3 revisions across 1 stack. They can't be recovered.",
      ),
    ).toBeTruthy();
    const keep = within(dialog).getByRole("button", { name: "Keep limit" });
    await waitFor(() => expect(document.activeElement).toBe(keep));
    expect(previews().map((p) => p.search)).toEqual(["?keep=5"]);
    expect(patches()).toHaveLength(0);

    fireEvent.click(within(dialog).getByRole("button", { name: "Save and delete 3 revisions" }));
    await waitFor(() => expect(patches()).toHaveLength(1));
    expect(patches()[0]?.body).toEqual({ kept: 5 });
    await waitFor(() => expect(toastSuccess).toHaveBeenCalledWith("Settings saved."));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  });

  it("does nothing more on Keep limit: the dialog closes, nothing is sent and the typed value stays", async () => {
    renderCard();
    const field = await loaded();
    await typeAndSave(field, "5");
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Keep limit" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(patches()).toHaveLength(0);
    expect(field.value).toBe("5");
  });

  it("reads one revision across one stack in the singular", async () => {
    previewAnswer = () => Response.json({ revisions: 1, stacks: 1 });
    renderCard();
    const field = await loaded();
    await typeAndSave(field, "10");
    const dialog = await screen.findByRole("alertdialog");
    expect(
      within(dialog).getByText(
        "Lowering this to 10 deletes 1 revision across 1 stack. They can't be recovered.",
      ),
    ).toBeTruthy();
    expect(within(dialog).getByRole("button", { name: "Save and delete 1 revision" })).toBeTruthy();
  });

  it("reads many revisions across many stacks in the plural", async () => {
    previewAnswer = () => Response.json({ revisions: 41, stacks: 6 });
    renderCard();
    const field = await loaded();
    await typeAndSave(field, "8");
    const dialog = await screen.findByRole("alertdialog");
    expect(
      within(dialog).getByText(
        "Lowering this to 8 deletes 41 revisions across 6 stacks. They can't be recovered.",
      ),
    ).toBeTruthy();
    expect(
      within(dialog).getByRole("button", { name: "Save and delete 41 revisions" }),
    ).toBeTruthy();
  });

  it("saves a lowered limit with no dialog when no revision would go", async () => {
    previewAnswer = () => Response.json({ revisions: 0, stacks: 0 });
    renderCard();
    const field = await loaded();
    await typeAndSave(field, "15");
    await waitFor(() => expect(patches()).toHaveLength(1));
    expect(patches()[0]?.body).toEqual({ kept: 15 });
    expect(previews().map((p) => p.search)).toEqual(["?keep=15"]);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("warns that older revisions go at the next prune when the count cannot be read", async () => {
    previewAnswer = () => Response.json({ title: "x", status: 500 }, { status: 500 });
    renderCard();
    const field = await loaded();
    await typeAndSave(field, "5");
    const dialog = await screen.findByRole("alertdialog");
    expect(
      within(dialog).getByText(
        "Older revisions are deleted the next time pruning runs. They can't be recovered.",
      ),
    ).toBeTruthy();
    expect(
      within(dialog).getByRole("button", { name: "Save and delete older revisions" }),
    ).toBeTruthy();
    expect(patches()).toHaveLength(0);
  });

  it("reads every revision list again after a save, so a deleted revision reads as gone", async () => {
    const key = ["env", "e1", "stacks", "s1", "revisions"];
    queryClient.setQueryData(key, { items: [] });
    renderCard();
    const field = await loaded();
    await typeAndSave(field, "50");
    await waitFor(() => expect(queryClient.getQueryState(key)?.isInvalidated).toBe(true));
  });

  it("says the generic failure inline when saving fails, and keeps what was typed", async () => {
    patchAnswer = () => Response.json({ title: "x", status: 500 }, { status: 500 });
    renderCard();
    const field = await loaded();
    await typeAndSave(field, "50");
    expect(await screen.findByText("Something went wrong. Try again.")).toBeTruthy();
    expect(field.value).toBe("50");
    expect(toastSuccess).not.toHaveBeenCalled();
  });

  it("puts a range error the server found on the field", async () => {
    patchAnswer = () =>
      Response.json(
        {
          type: "validation_failed",
          title: "Unprocessable Entity",
          status: 422,
          errors: [{ location: "body.kept", message: "expected number >= 5" }],
        },
        { status: 422 },
      );
    renderCard();
    const field = await loaded();
    await typeAndSave(field, "50");
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("Use a whole number from 5 to 200.")).toBeTruthy();
  });

  it("is read only for someone who may read the settings but not change them", async () => {
    me = makeMe({ permissions: { global: ["settings.read"], environments: {} } });
    renderCard();
    const field = await loaded();
    expect(field.disabled).toBe(true);
    expect(field.value).toBe("20");
    expect(screen.queryByRole("button", { name: "Save settings" })).toBeNull();
    expect(screen.getByText("You can view these settings but not change them.")).toBeTruthy();
  });
});
