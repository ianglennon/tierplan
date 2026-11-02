// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

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
      if (
        req.method === "POST" &&
        (url.pathname === `/api/env/${ENV}/secrets` || url.pathname === `/api/env/${ENV}/configs`)
      ) {
        posted.push({ path: url.pathname, body: (await req.json()) as Record<string, unknown> });
        return answer();
      }
      if (
        url.pathname === `/api/env/${ENV}/secrets` ||
        url.pathname === `/api/env/${ENV}/configs`
      ) {
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

function Host({
  startOpen = true,
  kind = "secret",
}: {
  startOpen?: boolean;
  kind?: "secret" | "config";
}) {
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
      <ObjectSheet envId={ENV} kind={kind} open={open} onOpenChange={onOpenChange} />
    </>
  );
}

function mount(startOpen = true, kind: "secret" | "config" = "secret") {
  render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <Host startOpen={startOpen} kind={kind} />
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

// The config mode of the sheet (plan 06-25, RES-08, D-24, D-25, D-28): the same sheet with the
// content in a text area, an optional Go template driver and a limit of 1000 KB.
describe("the config mode", () => {
  const createConfig = () => screen.getByRole("button", { name: "Create config" });
  const content = () => screen.getByLabelText("Content") as HTMLTextAreaElement;

  async function open() {
    mount(true, "config");
    return sheet();
  }

  function fillConfig(nameText: string, text?: string) {
    fireEvent.change(name(), { target: { value: nameText } });
    if (text !== undefined) fireEvent.change(content(), { target: { value: text } });
  }

  async function choose(file: File) {
    fireEvent.click(screen.getByRole("radio", { name: "Upload a file" }));
    fireEvent.change(document.querySelector("input[type='file']") as HTMLInputElement, {
      target: { files: [file] },
    });
  }

  it("says configs aren't encrypted and points to secrets", async () => {
    const dialog = await open();
    expect(within(dialog).getByText("Create a config")).toBeTruthy();
    expect(
      within(dialog).getByText(
        "Configs aren't encrypted. Don't put passwords or keys in one. Use a secret instead.",
      ),
    ).toBeTruthy();
  });

  it("has Name, Labels, Value source, a 12-row Content text area, Templating and the size line", async () => {
    await open();
    expect(name()).toBeTruthy();
    expect(screen.getByRole("button", { name: "Add label" })).toBeTruthy();
    expect(screen.getByRole("radio", { name: "Type it" })).toBeTruthy();
    expect(content().tagName).toBe("TEXTAREA");
    expect(content().rows).toBe(12);
    expect(screen.getByRole("combobox", { name: "Templating (optional)" }).textContent).toContain(
      "None",
    );
    expect(
      screen.getByText("Go template fills in values such as the service name when a task starts."),
    ).toBeTruthy();
    expect(screen.getByText("0 KB of 1000 KB")).toBeTruthy();
    expect(screen.queryByLabelText("Value")).toBeNull();
  });

  it("posts the name, the labels and the content as base64 once, with no templating, and toasts", async () => {
    await open();
    fillConfig("app.yaml", "name: api\nport: 8080\n");
    fireEvent.click(screen.getByRole("button", { name: "Add label" }));
    fireEvent.change(screen.getByLabelText("Label name"), { target: { value: "team" } });
    fireEvent.change(screen.getByLabelText("Label value"), { target: { value: "web" } });
    fireEvent.click(createConfig());
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({
      path: `/api/env/${ENV}/configs`,
      body: {
        name: "app.yaml",
        labels: { team: "web" },
        data: b64("name: api\nport: 8080\n"),
        source: "typed",
      },
    });
    await waitFor(() => expect(openCalls).toEqual([false]));
    await waitFor(() => expect(document.body.textContent).toContain("Created config app.yaml."));
  });

  it("sends templating golang when Go template is chosen", async () => {
    await open();
    fillConfig("tmpl.conf", "host={{ .Service.Name }}");
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Templating (optional)" }), {
      key: "Enter",
    });
    fireEvent.click(await screen.findByRole("option", { name: "Go template" }));
    fireEvent.click(createConfig());
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]?.body).toMatchObject({ name: "tmpl.conf", templating: "golang" });
  });

  it("puts the text of a UTF-8 file in the text area and says the content came from a file", async () => {
    await open();
    fillConfig("nginx.conf");
    await choose(new File(["server { listen 80; }\n"], "nginx.conf"));
    await waitFor(() => expect(content().value).toBe("server { listen 80; }\n"));
    fireEvent.click(createConfig());
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]?.body).toMatchObject({
      name: "nginx.conf",
      data: b64("server { listen 80; }\n"),
      source: "file",
    });
  });

  it("says the content came from the keyboard again once the text is edited", async () => {
    await open();
    fillConfig("nginx.conf");
    await choose(new File(["a=1\n"], "a.conf"));
    await waitFor(() => expect(content().value).toBe("a=1\n"));
    fireEvent.change(content(), { target: { value: "a=2\n" } });
    fireEvent.click(createConfig());
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]?.body).toMatchObject({ data: b64("a=2\n"), source: "typed" });
  });

  it("keeps a file that is not text as it is, with its name and size, and sends its bytes", async () => {
    await open();
    fillConfig("blob.bin");
    const bytes = new Uint8Array([0xff, 0xfe, 0x00, 0x01]);
    await choose(new File([bytes], "blob.bin"));
    await screen.findByText("blob.bin (4 B)");
    expect(screen.queryByLabelText("Content")).toBeNull();
    fireEvent.click(createConfig());
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]?.body).toMatchObject({
      name: "blob.bin",
      data: btoa(String.fromCharCode(...bytes)),
      source: "file",
    });
  });

  it("asks for content, or for a file", async () => {
    await open();
    fillConfig("app.yaml");
    fireEvent.click(createConfig());
    await screen.findByText("Enter a value.");
    fireEvent.click(screen.getByRole("radio", { name: "Upload a file" }));
    fireEvent.click(createConfig());
    await screen.findByText("Choose a file.");
    expect(posted).toHaveLength(0);
  });

  it("takes 1000 KB less a byte and refuses more without sending", async () => {
    await open();
    fillConfig("big.conf", "x".repeat(1_023_999));
    expect(screen.queryByText("Configs can be at most 1000 KB.")).toBeNull();
    fireEvent.change(content(), { target: { value: "x".repeat(1_024_000) } });
    await waitFor(() =>
      expect(screen.getAllByText("Configs can be at most 1000 KB.").length).toBeGreaterThan(0),
    );
    fireEvent.click(createConfig());
    expect(posted).toHaveLength(0);
  });

  it("says a name that is taken at the name and keeps what was typed", async () => {
    answer = () => problem(409, "name_taken");
    await open();
    fillConfig("app.yaml", "a=1");
    fireEvent.click(createConfig());
    await screen.findByText("A config named app.yaml already exists.");
    expect(openCalls).toEqual([]);
    expect(content().value).toBe("a=1");
  });

  it("puts a refused templating at the templating field and a too-big answer at the content", async () => {
    answer = () => problem(422, "templating_invalid");
    await open();
    fillConfig("app.yaml", "a=1");
    fireEvent.click(createConfig());
    await screen.findByText("Choose None or Go template.");
    cleanup();
    posted = [];
    answer = () => problem(422, "value_too_large");
    await open();
    fillConfig("app.yaml", "a=1");
    fireEvent.click(createConfig());
    await screen.findByText("Configs can be at most 1000 KB.");
  });

  it("says in the words of configs that the environment is not a swarm", async () => {
    answer = () => problem(409, "not_a_swarm");
    await open();
    fillConfig("app.yaml", "a=1");
    fireEvent.click(createConfig());
    await screen.findByText("This environment isn't a swarm, so it has no configs.");
  });

  it("asks before throwing away what was typed, in the words of configs, and clears it when discarded", async () => {
    await open();
    fillConfig("app.yaml", "a=1");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await screen.findByText("Discard this config?");
    expect(
      screen.getByText("The config hasn't been created yet. What you entered will be lost."),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Discard" }));
    await waitFor(() => expect(openCalls).toEqual([false]));
    fireEvent.click(screen.getByRole("button", { name: "reopen" }));
    await sheet();
    expect(name().value).toBe("");
    expect(content().value).toBe("");
  });

  it("keeps nothing of the content in a toast or in storage", async () => {
    await open();
    fillConfig("app.yaml", "unique-config-text-91");
    fireEvent.click(createConfig());
    await waitFor(() => expect(openCalls).toEqual([false]));
    await waitFor(() => expect(document.body.textContent).toContain("Created config app.yaml."));
    expect(document.body.textContent).not.toContain("unique-config-text-91");
    expect(JSON.stringify({ ...localStorage })).not.toContain("unique-config-text-91");
    expect(JSON.stringify({ ...sessionStorage })).not.toContain("unique-config-text-91");
  });
});
