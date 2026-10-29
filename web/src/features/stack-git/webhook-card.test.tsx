// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type {
  StackGit,
  StackGitWebhook,
  StackGitWebhookInput,
  StackGitWebhookSecret,
} from "@/api/stack-git";
import type { StackDetail } from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { GitTab } from "@/features/stack-git/git-tab";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The Webhook card of the Git tab (plan 09-24, GIT-05, D-15): set up, rotate and remove a stack's webhook, with the
// address and the secret shown once and kept nowhere.

const ENV = "e1";
const DEPLOYED = "0123456789abcdef0123456789abcdef01234567";
// Values that must be found only inside the show-once panel.
const SECRET = "s3cr3t-value-9f8e7d6c5b4a";
const TOKEN_URL = "https://bayplan.example/hooks/git/tok-0123456789abcdefghij";
const MASKED = "https://bayplan.example/hooks/git/••••";

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

function makeWebhook(over: Partial<StackGitWebhook> = {}): StackGitWebhook {
  return {
    flavour: "github",
    placement: "header",
    address: MASKED,
    last4: "6c5b",
    owner: { id: "u1", name: "Ada" },
    paused: false,
    ...over,
  };
}

function makeGit(over: Partial<StackGit> = {}): StackGit {
  return {
    repoId: "r1",
    repoName: "stacks repo",
    repoHost: "github.com",
    repoPath: "acme/stacks",
    refKind: "branch",
    ref: "main",
    folder: "",
    composeFiles: ["compose.yaml"],
    deployedSha: DEPLOYED,
    headSha: DEPLOYED,
    behind: 0,
    behindCapped: false,
    status: "in_sync",
    checkedAt: "2026-10-28T10:00:00Z",
    checking: false,
    autoDeploy: false,
    autoDeployPaused: false,
    pollEnabled: true,
    pollMinutes: 5,
    ...over,
  };
}

function makeStack(git: StackGit): StackDetail {
  return {
    id: "s1",
    name: "web",
    target: "standalone",
    source: "git",
    state: "running",
    lastDeployedAt: "",
    lastDeployedBy: "",
    hasDraft: false,
    envId: ENV,
    sourceRef: "",
    createdBy: "ada",
    createdAt: "2026-10-19T10:00:00Z",
    stopped: false,
    git,
  } as StackDetail;
}

let git: StackGit;
let role: "viewer" | "operator" | "admin";
// What the create route answers, and the body it was sent.
let createAnswer: () => Response;
let creates: StackGitWebhookInput[];
let client: QueryClient;
let writeText: ReturnType<typeof vi.fn>;

function answerOf(over: Partial<StackGitWebhookSecret> = {}): StackGitWebhookSecret {
  return { flavour: "github", placement: "header", url: TOKEN_URL, secret: SECRET, ...over };
}

beforeEach(() => {
  git = makeGit();
  role = "admin";
  creates = [];
  createAnswer = () => Response.json(answerOf(), { headers: { "Cache-Control": "no-store" } });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  toasts.success.mockReset();
  toasts.error.mockReset();
  toasts.info.mockReset();
  writeText = vi.fn(async () => {});
  Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
  localStorage.clear();
  sessionStorage.clear();
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: role } }));
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Homelab" }] });
      }
      if (path === `/api/env/${ENV}/stacks/s1/git/webhook` && req.method === "POST") {
        creates.push((await req.json()) as StackGitWebhookInput);
        const res = createAnswer();
        if (res.ok) {
          const body = (await res.clone().json()) as StackGitWebhookSecret;
          git = {
            ...git,
            webhook: makeWebhook({ flavour: body.flavour, placement: body.placement }),
          };
        }
        return res;
      }
      if (path === `/api/env/${ENV}/stacks/s1/git`) return Response.json(git);
      if (path === `/api/env/${ENV}/stacks/s1`) return Response.json(makeStack(git));
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount() {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter path={`/env/${ENV}/stacks/s1/git`}>
        <TooltipProvider>
          <GitTab envId={ENV} stackId="s1" />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

// The card once the git settings have loaded.
async function card() {
  const c = await screen.findByRole("region", { name: "Webhook" });
  return c;
}

// Everything the browser keeps about this page that a secret must not be in.
function keptState(): string {
  const queries = client
    .getQueryCache()
    .getAll()
    .map((q) => ({ key: q.queryKey, data: q.state.data, error: q.state.error }));
  const mutations = client
    .getMutationCache()
    .getAll()
    .map((m) => ({ variables: m.state.variables, data: m.state.data, error: m.state.error }));
  return JSON.stringify({
    queries,
    mutations,
    local: { ...localStorage },
    session: { ...sessionStorage },
    href: window.location.href,
    toasts: [...toasts.success.mock.calls, ...toasts.error.mock.calls, ...toasts.info.mock.calls],
  });
}

async function openSetUp(c: HTMLElement) {
  const button = await within(c).findByRole("button", { name: "Set up webhook" });
  await waitFor(() => expect(button.getAttribute("aria-disabled")).toBeNull());
  await waitFor(() => expect((button as HTMLButtonElement).disabled).toBe(false));
  fireEvent.click(button);
  return screen.findByRole("dialog", { name: "Set up a webhook" });
}

describe("WebhookCard set up", () => {
  it("says there is no webhook yet and offers to set one up", async () => {
    mount();
    const c = await card();
    expect(
      within(c).getByText("Lets your git host tell Bayplan the moment something is pushed."),
    ).toBeTruthy();
    expect(within(c).getByText("No webhook yet.")).toBeTruthy();
    expect(within(c).getByRole("button", { name: "Set up webhook" })).toBeTruthy();
    expect(within(c).queryByText(/A webhook only tells Bayplan/)).toBeNull();
  });

  it("offers the four git hosts, and the generic one a place for the secret", async () => {
    mount();
    const dialog = await openSetUp(await card());
    expect(
      within(dialog).getByText(
        "Pick your git host. Bayplan creates a secret address for this stack.",
      ),
    ).toBeTruthy();
    const hosts = within(dialog).getByRole("radiogroup", { name: "Git host" });
    expect(
      within(hosts)
        .getAllByRole("radio")
        .map((r) => r.getAttribute("id")),
    ).toEqual([
      "webhook-host-github",
      "webhook-host-gitlab",
      "webhook-host-gitea",
      "webhook-host-generic",
    ]);
    for (const name of ["GitHub", "GitLab", "Gitea or Forgejo", "Other (generic)"]) {
      expect(within(hosts).getByRole("radio", { name })).toBeTruthy();
    }
    expect(within(dialog).queryByRole("radiogroup", { name: "Where the secret goes" })).toBeNull();
    fireEvent.click(within(hosts).getByRole("radio", { name: "Other (generic)" }));
    const where = within(dialog).getByRole("radiogroup", { name: "Where the secret goes" });
    expect(
      within(where).getByRole("radio", { name: "In the address" }).getAttribute("aria-checked"),
    ).toBe("true");
    expect(within(where).getByRole("radio", { name: "In an Authorization header" })).toBeTruthy();
  });

  it("creates a GitHub webhook and shows its address and secret once, with Copy buttons", async () => {
    mount();
    const c = await card();
    const dialog = await openSetUp(c);
    fireEvent.click(within(dialog).getByRole("button", { name: "Create webhook" }));

    const panel = await within(c).findByRole("status", { name: "Copy the secret now" });
    expect(creates).toEqual([{ flavour: "github" }]);
    expect(
      within(c).getByText("Bayplan can't show it again. If you lose it, rotate the secret."),
    ).toBeTruthy();

    const address = within(panel).getByLabelText("Webhook address") as HTMLInputElement;
    expect(address.value).toBe(TOKEN_URL);
    expect(address.readOnly).toBe(true);
    const secret = within(panel).getByLabelText("Webhook secret, shown once") as HTMLInputElement;
    expect(secret.value).toBe(SECRET);
    expect(secret.readOnly).toBe(true);
    expect(toasts.success).toHaveBeenCalledWith("Created the webhook.");

    fireEvent.click(within(panel).getByRole("button", { name: "Copy address" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(TOKEN_URL));
    expect(await within(panel).findByRole("button", { name: "Copied address" })).toBeTruthy();
    fireEvent.click(within(panel).getByRole("button", { name: "Copy secret" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(SECRET));
    expect(await within(panel).findByRole("button", { name: "Copied secret" })).toBeTruthy();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("clears the values on Done, puts focus on the card heading and keeps nothing", async () => {
    mount();
    const c = await card();
    const dialog = await openSetUp(c);
    fireEvent.click(within(dialog).getByRole("button", { name: "Create webhook" }));
    await within(c).findByText("Copy the secret now");

    // While the panel is up the secret is on screen and nowhere else.
    const shownState = keptState();
    expect(shownState).not.toContain(SECRET);
    expect(shownState).not.toContain("tok-0123456789abcdefghij");
    await waitFor(() => expect(client.getMutationCache().getAll()).toHaveLength(0));

    fireEvent.click(within(c).getByRole("button", { name: "Done" }));
    expect(within(c).queryByText("Copy the secret now")).toBeNull();
    expect(document.body.innerHTML).not.toContain(SECRET);
    expect(document.body.innerHTML).not.toContain("tok-0123456789abcdefghij");
    expect(document.activeElement).toBe(within(c).getByRole("heading", { name: "Webhook" }));
    expect(keptState()).not.toContain(SECRET);

    // What stays is the masked summary the git route answers.
    const address = within(c).getByLabelText("Address") as HTMLInputElement;
    expect(address.value).toBe(MASKED);
  });

  it("clears the values when the card goes away", async () => {
    const view = mount();
    const c = await card();
    const dialog = await openSetUp(c);
    fireEvent.click(within(dialog).getByRole("button", { name: "Create webhook" }));
    await within(c).findByText("Copy the secret now");
    view.unmount();
    expect(document.body.innerHTML).not.toContain(SECRET);
    expect(keptState()).not.toContain(SECRET);
  });

  it("sends the placement for a generic webhook and shows no Secret row when the secret is in the address", async () => {
    createAnswer = () =>
      Response.json(answerOf({ flavour: "generic", placement: "path", secret: "" }));
    mount();
    const c = await card();
    const dialog = await openSetUp(c);
    fireEvent.click(within(dialog).getByRole("radio", { name: "Other (generic)" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Create webhook" }));
    await within(c).findByText("Copy the secret now");
    expect(creates).toEqual([{ flavour: "generic", placement: "path" }]);
    expect((within(c).getByLabelText("Webhook address") as HTMLInputElement).value).toBe(TOKEN_URL);
    expect(within(c).queryByLabelText("Webhook secret, shown once")).toBeNull();
    expect(within(c).queryByRole("button", { name: "Copy secret" })).toBeNull();
    expect(keptState()).not.toContain("tok-0123456789abcdefghij");
  });

  it("sends the header placement for a generic webhook and shows its secret", async () => {
    createAnswer = () => Response.json(answerOf({ flavour: "generic", placement: "header" }));
    mount();
    const c = await card();
    const dialog = await openSetUp(c);
    fireEvent.click(within(dialog).getByRole("radio", { name: "Other (generic)" }));
    fireEvent.click(within(dialog).getByRole("radio", { name: "In an Authorization header" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Create webhook" }));
    await within(c).findByText("Copy the secret now");
    expect(creates).toEqual([{ flavour: "generic", placement: "header" }]);
    expect((within(c).getByLabelText("Webhook secret, shown once") as HTMLInputElement).value).toBe(
      SECRET,
    );
  });

  it("says so inside the dialog when the server refuses, and shows no panel", async () => {
    createAnswer = () =>
      Response.json(
        { type: "webhook_exists", title: "Conflict", status: 409, detail: "x" },
        { status: 409 },
      );
    mount();
    const c = await card();
    const dialog = await openSetUp(c);
    fireEvent.click(within(dialog).getByRole("button", { name: "Create webhook" }));
    expect((await within(dialog).findByRole("alert")).textContent).toBe(
      "This stack already has a webhook.",
    );
    expect(within(c).queryByText("Copy the secret now")).toBeNull();
    expect(toasts.success).not.toHaveBeenCalled();
  });

  it("shows the empty card to an Operator and gives the reason on Set up webhook", async () => {
    role = "operator";
    mount();
    const c = await card();
    const setUp = () => within(c).getByRole("button", { name: "Set up webhook" });
    await waitFor(() => expect(setUp().getAttribute("aria-disabled")).toBe("true"));
    expect(setUp().getAttribute("aria-describedby")).toBeTruthy();
    await waitFor(() =>
      expect(within(c).getAllByText("Needs Admin on Homelab").length).toBeGreaterThan(0),
    );
    fireEvent.click(setUp());
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(creates).toEqual([]);
  });
});
