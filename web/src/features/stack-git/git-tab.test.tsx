// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useRouterState } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { StackGit } from "@/api/stack-git";
import type { StackDetail } from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { GitTab } from "@/features/stack-git/git-tab";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The Git tab and its Updates card (plan 09-18, GIT-06, D-14): polling on or off and every how many minutes, saved with
// the card's own button.

const ENV = "e1";
const DEPLOYED = "0123456789abcdef0123456789abcdef01234567";
const CHECKED = "2026-10-28T10:00:00Z";

const toasts = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }));
vi.mock("@/lib/toast", () => ({
  toastSuccess: toasts.success,
  toastError: toasts.error,
  toastInfo: toasts.info,
}));

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
    checkedAt: CHECKED,
    checking: false,
    autoDeploy: false,
    autoDeployPaused: false,
    pollEnabled: true,
    pollMinutes: 5,
    ...over,
  };
}

function makeStack(git: StackGit | undefined, over: Partial<StackDetail> = {}): StackDetail {
  return {
    id: "s1",
    name: "web",
    target: "standalone",
    source: git ? "git" : "inline",
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
    ...over,
  } as StackDetail;
}

let git: StackGit;
let stack: StackDetail;
let role: "viewer" | "operator";
// How the git route answers: undefined is the summary, a number a failure with that status, "pending" never.
let gitAnswer: number | "pending" | undefined;
// How the update route answers.
let putAnswer: () => Response;
let puts: unknown[];
let checks: number;
let client: QueryClient;

beforeEach(() => {
  git = makeGit();
  stack = makeStack(git);
  role = "operator";
  gitAnswer = undefined;
  puts = [];
  checks = 0;
  putAnswer = () => Response.json(git);
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  toasts.success.mockReset();
  toasts.error.mockReset();
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: role } }));
      if (path === `/api/env/${ENV}/stacks/s1/git/updates` && req.method === "PUT") {
        puts.push(await req.json());
        return putAnswer();
      }
      if (path === `/api/env/${ENV}/stacks/s1/git/check` && req.method === "POST") {
        checks++;
        // The check the server accepted is running.
        git = { ...git, checking: true };
        stack = makeStack(git);
        return Response.json({ checking: true }, { status: 202 });
      }
      if (path === `/api/env/${ENV}/stacks/s1/git`) {
        if (gitAnswer === "pending") return new Promise<Response>(() => {});
        if (typeof gitAnswer === "number") {
          return Response.json(
            { type: "internal", title: "Error", status: gitAnswer, detail: "no" },
            { status: gitAnswer },
          );
        }
        return Response.json(git);
      }
      if (path === `/api/env/${ENV}/stacks/s1`) return Response.json(stack);
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function Where() {
  const path = useRouterState({ select: (s) => s.location.pathname });
  return <span data-testid="where">{path}</span>;
}

function mount(path = `/env/${ENV}/stacks/s1/git`) {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter path={path}>
        <TooltipProvider>
          <GitTab envId={ENV} stackId="s1" />
          <Where />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

// The card once the settings have loaded.
async function card() {
  await screen.findByRole("region", { name: "Updates" });
  await waitFor(() => expect(document.querySelector('[aria-busy="true"]')).toBeNull());
  const c = screen.getByRole("region", { name: "Updates" });
  // An Operator's controls come on once the server has said what the person may do.
  if (role === "operator" && !c.textContent?.includes("A fixed commit")) {
    await waitFor(() => expect(toggle(c).hasAttribute("disabled")).toBe(false));
  }
  return c;
}

const toggle = (c: HTMLElement) => within(c).getByRole("switch", { name: "Check on a schedule" });
const field = (c: HTMLElement) =>
  within(c).getByRole("spinbutton", { name: "Check every (minutes)" }) as HTMLInputElement;
const save = (c: HTMLElement) => within(c).getByRole("button", { name: "Save update settings" });

function type(c: HTMLElement, value: string) {
  fireEvent.change(field(c), { target: { value } });
}

describe("GitTab Updates card", () => {
  it("shows the switch, the number field, the helper and the buttons of a git stack", async () => {
    mount();
    const c = await card();
    expect(within(c).getByText("How TierPlan learns that the repository changed.")).toBeTruthy();
    expect(toggle(c).getAttribute("aria-checked")).toBe("true");
    expect(field(c).value).toBe("5");
    expect(field(c).min).toBe("1");
    expect(field(c).max).toBe("1440");
    expect(field(c).step).toBe("1");
    expect(field(c).getAttribute("inputmode")).toBe("numeric");
    const helper = within(c).getByText(/Stacks on the same repository share one check/);
    expect(field(c).getAttribute("aria-describedby")).toContain(helper.id);
    expect(helper.textContent).toContain("Keep this on even with a webhook");
    expect((save(c) as HTMLButtonElement).disabled).toBe(true);
    expect(within(c).getByRole("button", { name: "Check now" })).toBeTruthy();
    expect(within(c).getByText(/Last checked/)).toBeTruthy();
    expect(within(c).getByRole("time").getAttribute("datetime")).toBe(CHECKED);
  });

  it("says when the stack was never checked", async () => {
    git = makeGit({ checkedAt: undefined, status: "unknown" });
    mount();
    const c = await card();
    expect(within(c).getByText("Not checked yet")).toBeTruthy();
    expect(within(c).queryByRole("time")).toBeNull();
  });

  it("saves the minutes with its own button and then rests", async () => {
    mount();
    const c = await card();
    git = makeGit({ pollMinutes: 15 });
    type(c, "15");
    expect((save(c) as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(save(c));
    await waitFor(() => expect(puts).toEqual([{ pollEnabled: true, pollMinutes: 15 }]));
    await waitFor(() => expect(toasts.success).toHaveBeenCalledWith("Saved the update settings."));
    await waitFor(() => expect((save(c) as HTMLButtonElement).disabled).toBe(true));
    expect(field(c).value).toBe("15");
  });

  it("turns polling off and keeps the number", async () => {
    mount();
    const c = await card();
    git = makeGit({ pollEnabled: false });
    fireEvent.click(toggle(c));
    expect(field(c).disabled).toBe(true);
    fireEvent.click(save(c));
    await waitFor(() => expect(puts).toEqual([{ pollEnabled: false, pollMinutes: 5 }]));
    await waitFor(() => expect(toggle(c).getAttribute("aria-checked")).toBe("false"));
  });

  it("accepts 1 and 1440 and refuses 0, 1441, a fraction, a negative number and an empty field", async () => {
    mount();
    const c = await card();
    for (const ok of ["1", "1440"]) {
      type(c, ok);
      expect((save(c) as HTMLButtonElement).disabled).toBe(false);
      expect(within(c).queryByRole("alert")).toBeNull();
    }
    for (const bad of ["0", "1441", "2.5", "-3", ""]) {
      type(c, bad);
      expect((save(c) as HTMLButtonElement).disabled).toBe(true);
      const alert = within(c).getByRole("alert");
      expect(alert.textContent).toBe("Use a whole number from 1 to 1440.");
      expect(field(c).getAttribute("aria-invalid")).toBe("true");
      expect(field(c).getAttribute("aria-describedby")).toContain(alert.id);
    }
    expect(puts).toEqual([]);
  });

  it("lets polling be turned off while the number is invalid, and keeps the stored minutes", async () => {
    mount();
    const c = await card();
    type(c, "");
    expect(within(c).getByRole("alert")).toBeTruthy();
    fireEvent.click(toggle(c));
    // The field is off, so its number is no longer judged.
    expect(within(c).queryByRole("alert")).toBeNull();
    expect((save(c) as HTMLButtonElement).disabled).toBe(false);
    // Turning it back on brings the invalid number back and holds the save.
    fireEvent.click(toggle(c));
    expect(within(c).getByRole("alert")).toBeTruthy();
    expect((save(c) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(toggle(c));
    git = makeGit({ pollEnabled: false });
    fireEvent.click(save(c));
    await waitFor(() => expect(puts).toEqual([{ pollEnabled: false, pollMinutes: 5 }]));
  });

  it("shows the server's refusal of the number like a typed one and sends nothing more", async () => {
    putAnswer = () =>
      Response.json(
        { type: "poll_interval", title: "Unprocessable Entity", status: 422, detail: "x" },
        { status: 422 },
      );
    mount();
    const c = await card();
    type(c, "20");
    fireEvent.click(save(c));
    await waitFor(() =>
      expect(within(c).getByRole("alert").textContent).toBe("Use a whole number from 1 to 1440."),
    );
    expect(toasts.error).not.toHaveBeenCalled();
  });

  it("says so when the save failed and keeps what was typed", async () => {
    putAnswer = () =>
      Response.json(
        { type: "internal", title: "Error", status: 500, detail: "no" },
        { status: 500 },
      );
    mount();
    const c = await card();
    type(c, "20");
    fireEvent.click(save(c));
    await waitFor(() =>
      expect(toasts.error).toHaveBeenCalledWith("Couldn't save the update settings. Try again."),
    );
    expect(field(c).value).toBe("20");
  });

  it("shows the settings to a Viewer and lets nothing change", async () => {
    role = "viewer";
    mount();
    const c = await card();
    await waitFor(() => expect(toggle(c).hasAttribute("disabled")).toBe(true));
    expect(field(c).disabled).toBe(true);
    // The reason is a Tooltip on the button, which stays in the tab order.
    await waitFor(() => expect(save(c).getAttribute("aria-disabled")).toBe("true"));
    fireEvent.click(save(c));
    expect(puts).toEqual([]);
  });

  it("asks the server to check when Check now is pressed", async () => {
    mount();
    const c = await card();
    fireEvent.click(within(c).getByRole("button", { name: "Check now" }));
    await waitFor(() => expect(checks).toBe(1));
    await waitFor(() =>
      expect(
        (within(c).getByRole("button", { name: "Checking…" }) as HTMLButtonElement).disabled,
      ).toBe(true),
    );
  });

  it("shows a fixed commit a line instead of the controls", async () => {
    git = makeGit({ refKind: "commit", ref: DEPLOYED });
    stack = makeStack(git);
    mount();
    const c = await card();
    expect(within(c).getByText("A fixed commit has nothing to check.")).toBeTruthy();
    expect(within(c).queryByRole("switch")).toBeNull();
    expect(within(c).queryByRole("spinbutton")).toBeNull();
    expect(within(c).queryByRole("button")).toBeNull();
  });

  it("shows card skeletons with the controls off while the settings load", async () => {
    gitAnswer = "pending";
    mount();
    const c = await screen.findByRole("region", { name: "Updates" });
    expect(c.querySelector('[aria-busy="true"]')).not.toBeNull();
    expect(within(c).getByRole("switch").hasAttribute("disabled")).toBe(true);
    expect((save(c) as HTMLButtonElement).disabled).toBe(true);
    expect(
      (within(c).getByRole("button", { name: "Check now" }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });

  it("says the settings could not be loaded and loads them again on retry", async () => {
    gitAnswer = 500;
    mount();
    expect(await screen.findByText("Couldn't load the git settings.")).toBeTruthy();
    expect(screen.getByText("Check the connection and try again.")).toBeTruthy();
    gitAnswer = undefined;
    fireEvent.click(screen.getByRole("button", { name: "Retry loading git settings" }));
    const c = await card();
    expect(field(c).value).toBe("5");
    expect(screen.queryByText("Couldn't load the git settings.")).toBeNull();
  });

  it("sends a stack that is not from git to its Overview", async () => {
    stack = makeStack(undefined);
    gitAnswer = 409;
    mount();
    await waitFor(() =>
      expect(screen.getByTestId("where").textContent).toBe(`/env/${ENV}/stacks/s1`),
    );
  });
});
