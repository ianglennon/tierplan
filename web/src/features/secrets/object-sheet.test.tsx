// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ObjectSheet } from "@/features/secrets/object-sheet";
import "@/i18n";

// The create sheet (plan 06-23, RES-08, D-24, D-25, T-06-113): name, labels and a value typed or
// uploaded; the value is sent once, as base64, and is in no toast, store, address or cache.

const ENV = "e1";

interface Posted {
  path: string;
  body: Record<string, unknown>;
}

let posted: Posted[];
let answer: () => Response | Promise<Response>;
let client: QueryClient;
let onOpenChange: (open: boolean) => void;
let openCalls: boolean[];

const problem = (status: number, type: string) =>
  Response.json({ type, title: "x", status, detail: "d" }, { status });

const b64 = (text: string) => btoa(String.fromCharCode(...new TextEncoder().encode(text)));

beforeEach(() => {
  posted = [];
  openCalls = [];
  answer = () => Response.json({ id: "sid-new", name: "x" }, { status: 201 });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  localStorage.clear();
  sessionStorage.clear();
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (req.method === "POST" && url.pathname === `/api/env/${ENV}/secrets`) {
        posted.push({ path: url.pathname, body: (await req.json()) as Record<string, unknown> });
        return answer();
      }
      if (url.pathname === `/api/env/${ENV}/secrets`) {
        return Response.json({ items: [], total: 0, unfilteredTotal: 0, capped: false });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

function Host({ startOpen = true }: { startOpen?: boolean }) {
  const [open, setOpen] = useState(startOpen);
  onOpenChange = (next) => {
    openCalls.push(next);
    setOpen(next);
  };
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        reopen
      </button>
      <ObjectSheet envId={ENV} kind="secret" open={open} onOpenChange={onOpenChange} />
    </>
  );
}

function mount(startOpen = true) {
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <Host startOpen={startOpen} />
        <Toaster />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

const sheet = () => screen.findByRole("dialog");
const create = () => screen.getByRole("button", { name: "Create secret" });
const name = () => screen.getByLabelText("Name") as HTMLInputElement;
const value = () => screen.getByLabelText("Value") as HTMLInputElement;

function fill(nameText: string, valueText?: string) {
  fireEvent.change(name(), { target: { value: nameText } });
  if (valueText !== undefined) fireEvent.change(value(), { target: { value: valueText } });
}

describe("the create sheet", () => {
  it("says what a secret is and that the value cannot be read back", async () => {
    mount();
    const dialog = await sheet();
    expect(within(dialog).getByText("Create a secret")).toBeTruthy();
    expect(
      within(dialog).getByText(
        "Stored in the swarm's encrypted store. You can't read the value back after you save.",
      ),
    ).toBeTruthy();
  });

  it("has Name with its helper, Labels, Value source and Value", async () => {
    mount();
    await sheet();
    expect(name()).toBeTruthy();
    expect(
      screen.getByText("Letters, numbers, dot, dash or underscore. It can't change later."),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Add label" })).toBeTruthy();
    expect(screen.getByRole("radio", { name: "Type it" })).toBeTruthy();
    expect(value().type).toBe("password");
  });

  it("is not there when closed", () => {
    mount(false);
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});

describe("creating a secret", () => {
  it("posts the name, the labels and the value as base64 once, then closes with a toast that has no value", async () => {
    mount();
    await sheet();
    fill("db-password", "pa$$w0rd-ünique");
    fireEvent.click(screen.getByRole("button", { name: "Add label" }));
    fireEvent.change(screen.getByLabelText("Label name"), { target: { value: "team" } });
    fireEvent.change(screen.getByLabelText("Label value"), { target: { value: "data" } });
    fireEvent.click(create());
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]?.body).toEqual({
      name: "db-password",
      labels: { team: "data" },
      data: b64("pa$$w0rd-ünique"),
      source: "typed",
    });
    await waitFor(() => expect(openCalls).toEqual([false]));
    await waitFor(() => expect(document.body.textContent).toContain("Created secret db-password."));
    expect(document.body.textContent).not.toContain("pa$$w0rd");
  });

  it("sends an uploaded file's bytes and says the value came from a file", async () => {
    mount();
    await sheet();
    fill("tls-key");
    fireEvent.click(screen.getByRole("radio", { name: "Upload a file" }));
    const bytes = new Uint8Array([0, 1, 2, 250, 255]);
    fireEvent.change(document.querySelector("input[type='file']") as HTMLInputElement, {
      target: { files: [new File([bytes], "tls.key")] },
    });
    await screen.findByText("tls.key (5 B)");
    fireEvent.click(create());
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]?.body).toMatchObject({
      name: "tls-key",
      data: btoa(String.fromCharCode(...bytes)),
      source: "file",
    });
  });

  it("sends once however often Create secret is pressed", async () => {
    let release: (r: Response) => void = () => {};
    answer = () => new Promise<Response>((resolve) => (release = resolve));
    mount();
    await sheet();
    fill("db-password", "x");
    fireEvent.click(create());
    fireEvent.click(create());
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(create().getAttribute("aria-disabled")).toBe("true");
    release(Response.json({ id: "sid-new", name: "db-password" }, { status: 201 }));
    await waitFor(() => expect(openCalls).toEqual([false]));
    expect(posted).toHaveLength(1);
  });

  it("does not close while the request is out", async () => {
    answer = () => new Promise<Response>(() => {});
    mount();
    const dialog = await sheet();
    fill("db-password", "x");
    fireEvent.click(create());
    await waitFor(() => expect(posted).toHaveLength(1));
    fireEvent.keyDown(dialog, { key: "Escape" });
    expect(openCalls).toEqual([]);
  });
});

describe("what the form checks before it sends", () => {
  it("asks for a name, and for a name with the right characters", async () => {
    mount();
    await sheet();
    fill("", "x");
    fireEvent.click(create());
    await screen.findByText("Enter a name.");
    fill("bad name!");
    fireEvent.click(create());
    await screen.findByText("Use letters, numbers, dot, dash or underscore.");
    expect(posted).toHaveLength(0);
  });

  it("asks for a value, or for a file", async () => {
    mount();
    await sheet();
    fill("db-password");
    fireEvent.click(create());
    await screen.findByText("Enter a value.");
    fireEvent.click(screen.getByRole("radio", { name: "Upload a file" }));
    fireEvent.click(create());
    await screen.findByText("Choose a file.");
    expect(posted).toHaveLength(0);
  });

  it("refuses a value over 500 KB without sending", async () => {
    mount();
    await sheet();
    fill("db-password", "x".repeat(512_000));
    fireEvent.click(create());
    await waitFor(() =>
      expect(screen.getAllByText("Values can be at most 500 KB.").length).toBeGreaterThan(0),
    );
    expect(posted).toHaveLength(0);
  });
});

describe("what the server can answer", () => {
  async function submit() {
    mount();
    await sheet();
    fill("db-password", "s3cret-value");
    fireEvent.click(create());
    await waitFor(() => expect(posted).toHaveLength(1));
  }

  it("says the name is taken at the name, and keeps the sheet and what was typed", async () => {
    answer = () => problem(409, "name_taken");
    await submit();
    await screen.findByText("A secret named db-password already exists.");
    expect(openCalls).toEqual([]);
    expect(name().value).toBe("db-password");
    expect(value().value).toBe("s3cret-value");
  });

  it("puts a refused name at the name", async () => {
    answer = () => problem(422, "name_invalid");
    await submit();
    await screen.findByText("Use letters, numbers, dot, dash or underscore.");
  });

  it("puts a refused value at the value", async () => {
    answer = () => problem(422, "value_too_large");
    await submit();
    await screen.findByText("Values can be at most 500 KB.");
  });

  it("explains a permission that was lost, a read-only host and no manager, in the sheet", async () => {
    answer = () => problem(403, "forbidden");
    await submit();
    await screen.findByText(
      "You no longer have permission to do that. Your access may have changed.",
    );
    cleanup();
    posted = [];
    answer = () => problem(409, "host_read_only");
    await submit();
    await screen.findByText(/This host is read-only/);
    cleanup();
    posted = [];
    answer = () => problem(503, "no_manager_connected");
    await submit();
    await screen.findByText(/No manager node is connected/);
  });

  it("never puts the value in the answer's message, a toast or storage", async () => {
    answer = () => problem(500, "internal");
    await submit();
    await screen.findByText(/Something went wrong on the server/);
    expect(document.body.textContent).not.toContain("s3cret-value");
    expect(JSON.stringify({ ...localStorage })).not.toContain("s3cret-value");
    expect(JSON.stringify({ ...sessionStorage })).not.toContain("s3cret-value");
  });
});

describe("closing", () => {
  it("closes at once with nothing typed", async () => {
    mount();
    await sheet();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(openCalls).toEqual([false]);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("asks before throwing away what was typed, and Keep editing keeps it", async () => {
    mount();
    await sheet();
    fill("db-password", "s3cret");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    const ask = await screen.findByRole("alertdialog");
    expect(within(ask).getByText("Discard this secret?")).toBeTruthy();
    fireEvent.click(within(ask).getByRole("button", { name: "Keep editing" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(openCalls).toEqual([]);
    expect(value().value).toBe("s3cret");
  });

  it("clears the value when the typed content is discarded, so reopening starts empty", async () => {
    mount();
    await sheet();
    fill("db-password", "s3cret");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    const ask = await screen.findByRole("alertdialog");
    fireEvent.click(within(ask).getByRole("button", { name: "Discard" }));
    await waitFor(() => expect(openCalls).toEqual([false]));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    fireEvent.click(screen.getByRole("button", { name: "reopen" }));
    await sheet();
    expect(name().value).toBe("");
    expect(value().value).toBe("");
  });

  it("clears the value after a successful create, so reopening starts empty", async () => {
    mount();
    await sheet();
    fill("db-password", "s3cret");
    fireEvent.click(create());
    await waitFor(() => expect(openCalls).toEqual([false]));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    fireEvent.click(screen.getByRole("button", { name: "reopen" }));
    await sheet();
    expect(name().value).toBe("");
    expect(value().value).toBe("");
  });

  it("keeps no trace of the create in the query cache once it is done", async () => {
    mount();
    await sheet();
    fill("db-password", "s3cret-cache-check");
    fireEvent.click(create());
    await waitFor(() => expect(openCalls).toEqual([false]));
    await waitFor(() => expect(client.getMutationCache().getAll()).toHaveLength(0));
    expect(
      JSON.stringify(
        client
          .getQueryCache()
          .getAll()
          .map((q) => q.state.data),
      ),
    ).not.toContain("s3cret-cache-check");
  });
});
