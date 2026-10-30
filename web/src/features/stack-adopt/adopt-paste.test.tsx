// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { AdoptPage } from "@/features/stack-adopt/adopt-page";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// Adopting from the original compose file (plan 08-15, STK-13, D-12): the paste source, its check against what runs,
// the variables of the file, and what is kept of any of it (nothing, beyond the page's own state).

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

const ENV = "e1";
const PATH = `/api/env/${ENV}/unmanaged-stacks/legacy`;

interface Sent {
  method: string;
  path: string;
  body?: Record<string, unknown>;
}

let sent: Sent[];
// What a check of a pasted file answers: a function of the body it was sent.
let pasteAnswer: (body: Record<string, unknown>) => { status: number; body: unknown };
let adoptAnswer: { status: number; body: unknown };
let client: QueryClient;
const onAdopted = vi.fn();

const FILE = "# the stack\nservices:\n  web:\n    image: nginx:1.27\n";

const spec = (replicas: number) => ({
  Name: "legacy_web",
  Mode: { Replicated: { Replicas: replicas } },
  TaskTemplate: { ContainerSpec: { Image: "nginx:1.27" } },
});

const checkBody = (over: Record<string, unknown> = {}) => ({
  fingerprint: "fp-1",
  source: "rebuild",
  clean: true,
  matches: false,
  blocked: false,
  gaps: [],
  gapCount: 0,
  ownershipOnly: [],
  objects: [],
  counts: { networks: 0, volumes: 0, secrets: 0, configs: 0 },
  policy: { kinds: [], entries: [] },
  portainer: false,
  orphan: false,
  notes: [],
  problems: [],
  variables: [],
  ...over,
});

// matching is a pasted file that differs from what runs in nothing but the ownership labels.
const matching = (over: Record<string, unknown> = {}) =>
  checkBody({ source: "paste", fingerprint: "fp-paste", matches: true, ...over });

// differing is a pasted file whose first deploy would change the replicas of web (two settings: replicas and a label).
const differing = (over: Record<string, unknown> = {}) =>
  checkBody({
    source: "paste",
    fingerprint: "fp-paste",
    clean: false,
    gapCount: 1,
    gaps: [
      {
        name: "legacy_web",
        short: "web",
        change: "changed",
        current: spec(2),
        next: spec(3),
        hiddenChanges: [],
        settings: 2,
      },
    ],
    ...over,
  });

const variable = (name: string, over: Record<string, unknown> = {}) => ({
  name,
  value: "",
  secret: false,
  source: "unset",
  default: "",
  hasDefault: false,
  required: true,
  unused: false,
  places: [],
  ...over,
});

beforeEach(() => {
  sent = [];
  pasteAnswer = () => ({ status: 200, body: matching() });
  adoptAnswer = { status: 201, body: { id: "s9", name: "legacy", envId: ENV } };
  toasts.success.mockReset();
  toasts.error.mockReset();
  toasts.info.mockReset();
  onAdopted.mockReset();
  client = new QueryClient({ defaultOptions: { queries: { retry: false, retryDelay: 0 } } });
  setCsrfToken("csrf");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      const body =
        req.method === "POST"
          ? await req
              .clone()
              .json()
              .catch(() => undefined)
          : undefined;
      sent.push({ method: req.method, path, body });
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (/^\/api\/env\/e1\/unmanaged-stacks\/[^/]+$/.test(path)) {
        return Response.json({
          name: path.split("/").pop(),
          target: "swarm",
          state: "running",
          servicesRunning: 1,
          servicesTotal: 1,
          services: [],
          foundBy: { label: "com.docker.stack.namespace", value: "legacy" },
          orphan: false,
          portainer: false,
          owner: "",
          adoptable: true,
          reason: "",
          fingerprint: "fp-1",
        });
      }
      if (path.endsWith("/adopt-check") && req.method === "POST") {
        if (body?.source === "paste") {
          const a = pasteAnswer(body);
          return Response.json(a.body, { status: a.status });
        }
        return Response.json(checkBody());
      }
      if (path.endsWith("/adopt") && req.method === "POST") {
        return Response.json(adoptAnswer.body, { status: adoptAnswer.status });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  client.clear();
});

function mount(name = "legacy") {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <AdoptPage envId={ENV} name={name} onAdopted={onAdopted} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

// Only the requests to these exact paths count: the SPA sends others (such as the recent environments) on its own.
const pasteChecks = () =>
  sent.filter(
    (s) => s.method === "POST" && s.path === `${PATH}/adopt-check` && s.body?.source === "paste",
  );
const adoptRequests = () => sent.filter((s) => s.method === "POST" && s.path === `${PATH}/adopt`);

function adoptButton() {
  const footer = document.querySelector("[data-slot=editor-footer]") as HTMLElement;
  return within(footer).getByRole("button", { name: /Adopt stack|Adopting/ });
}

// reasonOf is the sentence the Adopt button is off for, which is its accessible description; empty when it is on.
function reasonOf(): string {
  const id = adoptButton().getAttribute("aria-describedby");
  return id ? (document.getElementById(id)?.textContent ?? "") : "";
}

// startFromPaste opens the page and chooses "I have the original compose file".
async function startFromPaste(name = "legacy") {
  mount(name);
  const radio = await screen.findByRole("radio", { name: /I have the original compose file/ });
  fireEvent.click(radio);
  return screen.findByLabelText("Original compose file");
}

const typeFile = (box: HTMLElement, text: string) =>
  fireEvent.change(box, { target: { value: text } });

// theSourceCard is the card "Start from".
function theSourceCard() {
  return screen.getByRole("heading", { name: "Start from" }).closest("section") as HTMLElement;
}

describe("Start from", () => {
  it("offers the rebuild as the default and the original file as the other choice, with their helpers", async () => {
    mount();

    const group = await screen.findByRole("radiogroup", { name: "Start from" });
    const rebuild = within(group).getByRole("radio", { name: /Rebuild from the running services/ });
    const paste = within(group).getByRole("radio", { name: /I have the original compose file/ });
    expect(rebuild.getAttribute("aria-checked")).toBe("true");
    expect(paste.getAttribute("aria-checked")).toBe("false");
    expect(screen.getByText("Coaming builds the compose file from what's running.")).toBeTruthy();
    expect(screen.getByText("Paste it and Coaming checks it against what's running.")).toBeTruthy();
    expect(screen.queryByLabelText("Original compose file")).toBeNull();
    expect(pasteChecks()).toEqual([]);
  });

  it("shows the empty paste field with its placeholder, and Adopt says what to do first", async () => {
    const box = (await startFromPaste()) as HTMLTextAreaElement;

    expect(box.getAttribute("placeholder")).toBe("Paste compose.yaml here");
    expect(box.value).toBe("");
    await waitFor(() =>
      expect(reasonOf()).toBe("Paste a compose file first, or rebuild from the running services."),
    );
    expect(pasteChecks()).toEqual([]);
  });
});

describe("a pasted file that matches", () => {
  it("is checked after the pause, says it matches, and is adopted as written with the fingerprint of that check", async () => {
    const box = await startFromPaste();

    typeFile(box, FILE);

    expect(await within(theSourceCard()).findByText("Matches what's running.")).toBeTruthy();
    expect(pasteChecks().map((r) => r.body)).toEqual([
      { source: "paste", compose: FILE, vars: [] },
    ]);
    await waitFor(() => expect(reasonOf()).toBe(""));

    fireEvent.click(adoptButton());

    await waitFor(() => expect(onAdopted).toHaveBeenCalledWith("s9"));
    // The text goes as it was typed, byte for byte, with the fingerprint of the check and no verdict.
    expect(adoptRequests().map((r) => r.body)).toEqual([
      { fingerprint: "fp-paste", source: "paste", compose: FILE, vars: [] },
    ]);
  });
});

describe("a pasted file that differs", () => {
  it("says in how many settings, may be adopted as it is, and offers the rebuild instead", async () => {
    pasteAnswer = () => ({ status: 200, body: differing() });
    const box = await startFromPaste();

    typeFile(box, FILE);

    const card = theSourceCard();
    expect(
      await within(card).findByText("Differs from what's running in 2 settings."),
    ).toBeTruthy();
    // Differences never block (D-20): Adopt is on, and the check card lists them.
    await waitFor(() => expect(reasonOf()).toBe(""));
    expect(screen.getByText("1 difference will change on the first deploy.")).toBeTruthy();

    fireEvent.click(within(card).getByRole("button", { name: "Show differences" }));
    const gaps = screen.getByRole("region", { name: "Differences by service" });
    await waitFor(() => expect(document.activeElement).toBe(gaps));
    expect(within(gaps).getByText("web")).toBeTruthy();

    fireEvent.click(within(card).getByRole("button", { name: "Use the rebuild instead" }));

    await waitFor(() =>
      expect(
        screen
          .getByRole("radio", { name: /Rebuild from the running services/ })
          .getAttribute("aria-checked"),
      ).toBe("true"),
    );
    expect(screen.queryByLabelText("Original compose file")).toBeNull();
    await waitFor(() => expect(reasonOf()).toBe(""));
    fireEvent.click(adoptButton());
    await waitFor(() => expect(onAdopted).toHaveBeenCalledWith("s9"));
    expect(adoptRequests().map((r) => r.body)).toEqual([
      { fingerprint: "fp-1", source: "rebuild" },
    ]);
  });
});

// theResult is the live region of the paste field.
const secretText = "hunter2-very-secret";
const fileWithVars = `services:
  web:
    image: nginx:\${TAG}
    environment:
      PW: \${PW}
`;

// answerWithVars answers a check of a file that uses TAG and PW: a variable is required until it has a value in the body.
function answerWithVars(body: Record<string, unknown>) {
  const sentVars = (body.vars as { name: string; value: string; secret: boolean }[]) ?? [];
  const has = (n: string) => sentVars.some((v) => v.name === n && v.value !== "");
  const variables = [
    variable("TAG", { required: !has("TAG"), hasDefault: false }),
    variable("PW", {
      required: !has("PW"),
      secret: sentVars.find((v) => v.name === "PW")?.secret ?? false,
    }),
  ];
  const missing = variables.filter((v) => v.required).length;
  return {
    status: 200,
    body: missing
      ? checkBody({
          source: "paste",
          fingerprint: "fp-paste",
          clean: false,
          blocked: true,
          variables,
          problems: [
            {
              severity: "error",
              code: "variable_required",
              message: "A required variable has no value.",
              file: "compose.yaml",
              line: 3,
              col: 1,
              endLine: 3,
              endCol: 2,
            },
          ],
        })
      : matching({ variables }),
  };
}

describe("pasted variables", () => {
  it("lists each variable of the file with its label, its default or 'Required' as placeholder, and a Secret switch", async () => {
    pasteAnswer = (body) => {
      const a = answerWithVars(body);
      const rows = (a.body as { variables: Record<string, unknown>[] }).variables;
      rows[0] = { ...rows[0], name: "TAG", hasDefault: true, default: "1.27", required: false };
      return a;
    };
    const box = await startFromPaste();

    typeFile(box, fileWithVars);

    const tag = (await screen.findByLabelText("Value of TAG")) as HTMLInputElement;
    const pw = screen.getByLabelText("Value of PW") as HTMLInputElement;
    expect(tag.getAttribute("placeholder")).toBe("1.27");
    expect(pw.getAttribute("placeholder")).toBe("Required");
    expect(screen.getByRole("switch", { name: "Secret: TAG" })).toBeTruthy();
    expect(screen.getByRole("switch", { name: "Secret: PW" })).toBeTruthy();
    expect(
      screen.getByText(
        "Used to check your file against what's running. They are saved with the stack.",
      ),
    ).toBeTruthy();
  });

  it("checks the file again with a value typed, and sends a secret one as a password field the browser does not remember", async () => {
    pasteAnswer = answerWithVars;
    const box = await startFromPaste();
    typeFile(box, fileWithVars);
    const pw = (await screen.findByLabelText("Value of PW")) as HTMLInputElement;
    const tag = screen.getByLabelText("Value of TAG") as HTMLInputElement;
    expect(pw.type).toBe("text");

    fireEvent.change(tag, { target: { value: "1.27" } });
    fireEvent.change(pw, { target: { value: secretText } });
    fireEvent.click(screen.getByRole("switch", { name: "Secret: PW" }));

    await waitFor(() => {
      const last = pasteChecks().at(-1)?.body;
      expect(last?.vars).toEqual([
        { name: "PW", value: secretText, secret: true },
        { name: "TAG", value: "1.27", secret: false },
      ]);
    });
    const field = screen.getByLabelText("Value of PW") as HTMLInputElement;
    expect(field.type).toBe("password");
    expect(field.getAttribute("autocomplete")).toBe("new-password");
    expect(await within(theSourceCard()).findByText("Matches what's running.")).toBeTruthy();
  });

  it("flags a variable that still has no value once the check has answered, tied to its input", async () => {
    pasteAnswer = answerWithVars;
    const box = await startFromPaste();

    typeFile(box, fileWithVars);

    const message = await screen.findByText("TAG has no value. Fill it in to check your file.");
    const input = screen.getByLabelText("Value of TAG");
    expect(input.getAttribute("aria-invalid")).toBe("true");
    expect(input.getAttribute("aria-describedby")).toBe(message.id);
  });
});

describe("what blocks Adopt for a pasted file", () => {
  it("says how many variables to fill in, and stops saying it when they are", async () => {
    pasteAnswer = answerWithVars;
    const box = await startFromPaste();

    typeFile(box, fileWithVars);

    await waitFor(() => expect(reasonOf()).toBe("Fill in 2 variables first."));
    fireEvent.change(await screen.findByLabelText("Value of TAG"), { target: { value: "1.27" } });
    await waitFor(() => expect(reasonOf()).toBe("Fill in 1 variable first."));
    fireEvent.change(screen.getByLabelText("Value of PW"), { target: { value: secretText } });
    await waitFor(() => expect(reasonOf()).toBe(""));
  });

  it("says to fix the errors of a file that does not parse, lists them, and offers the rebuild", async () => {
    pasteAnswer = () => ({
      status: 200,
      body: checkBody({
        source: "paste",
        fingerprint: "fp-paste",
        clean: false,
        blocked: true,
        problems: [
          {
            severity: "error",
            code: "yaml_invalid",
            message: "This isn't valid YAML.",
            file: "compose.yaml",
            line: 4,
            col: 1,
            endLine: 4,
            endCol: 2,
          },
          {
            severity: "warning",
            code: "x",
            message: "Only a warning.",
            file: "compose.yaml",
            line: 1,
            col: 1,
            endLine: 1,
            endCol: 2,
          },
        ],
      }),
    });
    const box = await startFromPaste();

    typeFile(box, "services: [unclosed");

    expect(await screen.findByText("Line 4: This isn't valid YAML.")).toBeTruthy();
    expect(screen.queryByText(/Only a warning/)).toBeNull();
    await waitFor(() =>
      expect(reasonOf()).toBe(
        "Fix the errors in the pasted file first, or rebuild from the running services.",
      ),
    );
    expect(screen.getByText("Fix your file to see what will change.")).toBeTruthy();
    fireEvent.click(
      within(document.querySelector("[data-slot=pasted-problems]") as HTMLElement).getByRole(
        "button",
        {
          name: "Use the rebuild instead",
        },
      ),
    );
    await waitFor(() => expect(reasonOf()).toBe(""));
  });

  it("treats a paste of nothing but blanks as no paste at all, and sends no check", async () => {
    const box = await startFromPaste();

    typeFile(box, "  \n\n ");

    await waitFor(() =>
      expect(reasonOf()).toBe("Paste a compose file first, or rebuild from the running services."),
    );
    expect(screen.getByText("Paste your file to see what will change.")).toBeTruthy();
    expect(pasteChecks()).toEqual([]);
  });

  it("holds Adopt while a changed file waits to be checked, and never adopts a file the person has not seen checked", async () => {
    const box = await startFromPaste();
    typeFile(box, FILE);
    await within(theSourceCard()).findByText("Matches what's running.");
    await waitFor(() => expect(reasonOf()).toBe(""));

    typeFile(box, `${FILE}# one more line\n`);

    expect(reasonOf()).toBe("Checking legacy…");
    fireEvent.click(adoptButton());
    expect(adoptRequests()).toEqual([]);
    await waitFor(() => expect(reasonOf()).toBe(""));
  });
});

describe("what is kept of a pasted file", () => {
  it("sets the textarea so that the browser neither fills, remembers nor corrects it, with 12 rows that scroll inside it", async () => {
    const box = (await startFromPaste()) as HTMLTextAreaElement;

    expect(box.getAttribute("autocomplete")).toBe("off");
    expect(box.getAttribute("spellcheck")).toBe("false");
    expect(box.getAttribute("autocapitalize")).toBe("off");
    expect(box.rows).toBe(12);
    expect(box.getAttribute("wrap")).toBe("off");
    expect(box.className).toContain("overflow-auto");
    expect(box.className).toContain("font-mono");
    typeFile(box, Array.from({ length: 2000 }, (_, i) => `# line ${i}`).join("\n"));
    expect(box.rows).toBe(12);
  });

  it("clears the file and the values on Cancel, on unmount, on a change of stack and when the stack is stored", async () => {
    pasteAnswer = answerWithVars;
    const view = mount();
    fireEvent.click(await screen.findByRole("radio", { name: /I have the original compose file/ }));
    typeFile(await screen.findByLabelText("Original compose file"), fileWithVars);
    fireEvent.change(await screen.findByLabelText("Value of TAG"), {
      target: { value: "1.27" },
    });

    // Cancel
    fireEvent.click(screen.getByRole("link", { name: "Cancel" }));
    expect(screen.queryByLabelText("Original compose file")).toBeNull();
    fireEvent.click(screen.getByRole("radio", { name: /I have the original compose file/ }));
    expect((screen.getByLabelText("Original compose file") as HTMLTextAreaElement).value).toBe("");
    expect(screen.queryByLabelText("Value of TAG")).toBeNull();

    // A change of stack
    typeFile(screen.getByLabelText("Original compose file"), fileWithVars);
    view.rerender(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <AdoptPage envId={ENV} name="other" onAdopted={onAdopted} />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
    expect(await screen.findByRole("heading", { name: "Adopt other", level: 2 })).toBeTruthy();
    expect(screen.queryByLabelText("Original compose file")).toBeNull();
    expect(
      screen
        .getByRole("radio", { name: /Rebuild from the running services/ })
        .getAttribute("aria-checked"),
    ).toBe("true");

    // Unmount and come back
    view.unmount();
    mount();
    fireEvent.click(await screen.findByRole("radio", { name: /I have the original compose file/ }));
    expect(
      ((await screen.findByLabelText("Original compose file")) as HTMLTextAreaElement).value,
    ).toBe("");
  });

  it("writes it nowhere but the page: no storage, no address, no toast, no query, no mutation", async () => {
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    const replaceState = vi.spyOn(window.history, "replaceState");
    const pushState = vi.spyOn(window.history, "pushState");
    pasteAnswer = answerWithVars;
    mount();
    fireEvent.click(await screen.findByRole("radio", { name: /I have the original compose file/ }));
    typeFile(await screen.findByLabelText("Original compose file"), fileWithVars);
    fireEvent.change(await screen.findByLabelText("Value of TAG"), {
      target: { value: "1.27" },
    });
    fireEvent.change(screen.getByLabelText("Value of PW"), { target: { value: secretText } });
    fireEvent.click(screen.getByRole("switch", { name: "Secret: PW" }));
    await waitFor(() => expect(reasonOf()).toBe(""));

    const heldWhileTyped = () =>
      JSON.stringify([
        client
          .getQueryCache()
          .getAll()
          .map((q) => [q.queryKey, q.state.data]),
        client
          .getMutationCache()
          .getAll()
          .map((m) => m.state.variables),
      ]);
    expect(heldWhileTyped()).not.toContain(secretText);
    expect(heldWhileTyped()).not.toContain("image: nginx:");

    fireEvent.click(adoptButton());
    await waitFor(() => expect(onAdopted).toHaveBeenCalledWith("s9"));

    // The adoption itself carried the text, once, to the server.
    expect(adoptRequests()).toHaveLength(1);
    expect(JSON.stringify(adoptRequests()[0]?.body)).toContain(secretText);
    // Nothing else did: not the storages, the address, the toast, nor either cache.
    const everything = JSON.stringify([
      setItem.mock.calls,
      replaceState.mock.calls,
      pushState.mock.calls,
      window.location.href,
      toasts.success.mock.calls,
      toasts.error.mock.calls,
      toasts.info.mock.calls,
    ]);
    expect(everything).not.toContain(secretText);
    expect(everything).not.toContain("image: nginx:");
    expect(everything).not.toContain("1.27");
    await waitFor(() => expect(heldWhileTyped()).not.toContain(secretText));
    expect(heldWhileTyped()).not.toContain("image: nginx:");
    expect(
      (screen.queryByLabelText("Original compose file") as HTMLTextAreaElement | null)?.value ?? "",
    ).toBe("");
  });

  it("does not leave the file in the mutation either when the server refuses it", async () => {
    adoptAnswer = {
      status: 422,
      body: {
        type: "pasted_file_invalid",
        title: "x",
        status: 422,
        detail: "A required variable has no value.",
      },
    };
    const box = await startFromPaste();
    typeFile(box, `${FILE}# ${secretText}\n`);
    await within(theSourceCard()).findByText("Matches what's running.");
    await waitFor(() => expect(reasonOf()).toBe(""));

    fireEvent.click(adoptButton());

    expect(
      await screen.findByText(
        "The pasted file doesn't pass the check: A required variable has no value.",
      ),
    ).toBeTruthy();
    await waitFor(() =>
      expect(
        JSON.stringify(
          client
            .getMutationCache()
            .getAll()
            .map((m) => m.state.variables),
        ),
      ).not.toContain(secretText),
    );
    // The page still holds the file, so the person can fix it.
    expect((screen.getByLabelText("Original compose file") as HTMLTextAreaElement).value).toContain(
      secretText,
    );
  });
});

describe("choosing a file", () => {
  it("reads it into the field, and 'Remove pasted file' empties the field again", async () => {
    const box = (await startFromPaste()) as HTMLTextAreaElement;
    expect(screen.queryByRole("button", { name: "Remove pasted file" })).toBeNull();

    fireEvent.change(screen.getByLabelText("Choose a compose file"), {
      target: { files: [new File([FILE], "docker-compose.yml", { type: "text/yaml" })] },
    });

    await waitFor(() => expect(box.value).toBe(FILE));
    fireEvent.click(screen.getByRole("button", { name: "Remove pasted file" }));
    expect(box.value).toBe("");
    expect(screen.queryByRole("button", { name: "Remove pasted file" })).toBeNull();
  });

  it("refuses a file over 1 MiB and keeps what was there", async () => {
    const box = (await startFromPaste()) as HTMLTextAreaElement;
    typeFile(box, FILE);

    fireEvent.change(screen.getByLabelText("Choose a compose file"), {
      target: { files: [new File([new Uint8Array(1024 * 1024 + 1)], "big.yml")] },
    });

    expect(
      await screen.findByText(
        "That file is over 1 MiB. Rebuild from the running services instead.",
      ),
    ).toBeTruthy();
    expect(box.value).toBe(FILE);
  });
});

describe("a pasted check that fails", () => {
  it("says so in the check card, keeps Adopt off, and runs again on Retry check", async () => {
    pasteAnswer = () => ({ status: 500, body: { type: "about:blank", title: "x", status: 500 } });
    const box = await startFromPaste();

    typeFile(box, FILE);

    const retry = await screen.findByRole("button", { name: "Retry check" });
    expect(reasonOf()).toBe("Adopt is off until legacy has been checked.");
    pasteAnswer = () => ({ status: 200, body: matching() });
    fireEvent.click(retry);

    expect(await within(theSourceCard()).findByText("Matches what's running.")).toBeTruthy();
    expect(pasteChecks()).toHaveLength(2);
  });
});
