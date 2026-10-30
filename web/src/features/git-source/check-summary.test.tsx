// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClientProvider } from "@tanstack/react-query";
import {
  act,
  cleanup,
  fireEvent,
  render,
  renderHook,
  screen,
  within,
} from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { GitRequestError, type GitStackCheckRequest } from "@/api/git";
import type { StackProblem } from "@/api/stacks";
import {
  CheckSummary,
  type GitCheckState,
  gitCheckDebounceMs,
  useGitSourceCheck,
} from "@/features/git-source/check-summary";
import { checkOf, ENV, newClient } from "@/features/git-source/test-fixture";
import "@/i18n";

// The check of the chosen files on the From git page (plan 09-16, GIT-08): it waits 400 ms for the
// last change, sends one request, drops an answer that is out of date, and says what the folder
// deploys and what is wrong with it. The clock is fake and the server a stubbed fetch.

const request = (over: Partial<GitStackCheckRequest> = {}): GitStackCheckRequest => ({
  repoId: "r1",
  refKind: "branch",
  ref: "main",
  folder: "stacks/web",
  composeFiles: ["compose.yaml"],
  ...over,
});

const problem = (over: Partial<StackProblem> = {}): StackProblem => ({
  severity: "error",
  code: "x",
  message: "m",
  file: "compose.yaml",
  line: 3,
  col: 1,
  endLine: 3,
  endCol: 2,
  ...over,
});

const settled = (over: Partial<GitCheckState> = {}): GitCheckState => ({
  checking: false,
  retry: () => {},
  ...over,
});

afterEach(() => {
  cleanup();
});

describe("useGitSourceCheck", () => {
  let sent: Record<string, unknown>[];
  let answers: Array<() => Response | Promise<Response>>;

  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    setCsrfToken(null);
    sent = [];
    answers = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        if (new URL(req.url).pathname === `/api/env/${ENV}/git-stacks/check`) {
          sent.push(await req.clone().json());
          const next = answers.shift();
          return next ? next() : Response.json(checkOf({ services: ["web"] }));
        }
        return Response.json({}, { status: 404 });
      }),
    );
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  function mountHook(initial: GitStackCheckRequest | null) {
    const client = newClient();
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    );
    return renderHook(({ req }) => useGitSourceCheck(ENV, req), {
      wrapper,
      initialProps: { req: initial },
    });
  }

  const wait = (ms: number) =>
    act(async () => {
      await vi.advanceTimersByTimeAsync(ms);
    });

  it("sends nothing while there is nothing to check", async () => {
    const hook = mountHook(null);
    await wait(gitCheckDebounceMs * 2);
    expect(sent).toEqual([]);
    expect(hook.result.current.checking).toBe(false);
    expect(hook.result.current.check).toBeUndefined();
  });

  it("sends one request, 400 ms after the last of several quick changes", async () => {
    const hook = mountHook(request());
    await wait(100);
    hook.rerender({ req: request({ composeFiles: ["compose.yaml", "a.yaml"] }) });
    await wait(100);
    hook.rerender({ req: request({ composeFiles: ["compose.yaml", "b.yaml"] }) });
    await wait(gitCheckDebounceMs - 1);
    expect(sent).toEqual([]);
    expect(hook.result.current.checking).toBe(true);

    await wait(1);
    await wait(0);
    expect(sent).toEqual([request({ composeFiles: ["compose.yaml", "b.yaml"] })]);
    expect(hook.result.current.check?.services).toEqual(["web"]);
    expect(hook.result.current.checking).toBe(false);
  });

  it("keeps the last answer while the next check runs, and drops an answer that is out of date", async () => {
    let release: (r: Response) => void = () => {};
    answers.push(() => new Promise<Response>((resolve) => (release = resolve)));
    const hook = mountHook(request());
    await wait(gitCheckDebounceMs);
    expect(sent).toHaveLength(1);

    // The request changes while the first is still on its way.
    hook.rerender({ req: request({ folder: "stacks/db" }) });
    await wait(gitCheckDebounceMs);
    await wait(0);
    expect(sent).toHaveLength(2);
    expect(hook.result.current.check?.services).toEqual(["web"]);

    // The late answer of the first request does not replace the newer one.
    await act(async () => {
      release(Response.json(checkOf({ services: ["stale"] })));
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(hook.result.current.check?.services).toEqual(["web"]);
  });

  it("clears the result when there is nothing left to check", async () => {
    const hook = mountHook(request());
    await wait(gitCheckDebounceMs);
    await wait(0);
    expect(hook.result.current.check).toBeDefined();
    hook.rerender({ req: null });
    await wait(0);
    expect(hook.result.current.check).toBeUndefined();
    expect(hook.result.current.checking).toBe(false);
  });

  it("reports a failed check, keeps the last answer and checks again on retry", async () => {
    answers.push(() => Response.json({ type: "ref_missing", status: 422 }, { status: 422 }));
    const hook = mountHook(request());
    await wait(gitCheckDebounceMs);
    await wait(0);
    expect(hook.result.current.error?.type).toBe("ref_missing");
    expect(hook.result.current.checking).toBe(false);

    act(() => hook.result.current.retry());
    await wait(0);
    await wait(0);
    expect(sent).toHaveLength(2);
    expect(hook.result.current.error).toBeUndefined();
    expect(hook.result.current.check?.services).toEqual(["web"]);
  });
});

describe("CheckSummary", () => {
  function renderSummary(state: GitCheckState, onAddOverlay?: (path: string) => void) {
    return render(<CheckSummary state={state} repoName="homelab" onAddOverlay={onAddOverlay} />);
  }

  it("lists the services found, the first five by name and the rest counted", () => {
    renderSummary(
      settled({
        check: checkOf({ services: ["web", "db", "cache", "queue", "worker", "mail", "proxy"] }),
      }),
    );
    const region = screen.getByRole("status");
    expect(region.textContent).toBe("Found 7 services: web, db, cache, queue, worker and 2 more.");
    expect(within(region).getByText("web").tagName).toBe("CODE");
  });

  it("says one service in the singular and none when the files did not load", () => {
    const { unmount } = renderSummary(settled({ check: checkOf({ services: ["web"] }) }));
    expect(screen.getByRole("status").textContent).toBe("Found 1 service: web.");
    unmount();
    renderSummary(settled({ check: checkOf({ services: [] }) }));
    expect(screen.getByRole("status").textContent).toBe("No services found.");
  });

  it("puts blocking problems in an alert to fix in the repository and warnings in another, each with file and line", () => {
    renderSummary(
      settled({
        check: checkOf({
          services: ["web"],
          problems: [
            problem({ message: "db is not a service.", file: "compose.yaml", line: 12 }),
            problem({
              severity: "warning",
              code: "env_secret_like",
              message: "DB_PASSWORD looks like a secret.",
              file: ".env",
              line: 2,
            }),
          ],
        }),
      }),
    );
    const blocking = screen.getByRole("group", { name: /Fix these in the repository/ });
    expect(within(blocking).getByText("db is not a service.")).toBeTruthy();
    expect(within(blocking).getByText("compose.yaml:12")).toBeTruthy();
    expect(blocking.textContent).toContain("Blocking: ");
    const groups = screen.getAllByRole("group");
    expect(groups).toHaveLength(2);
    const warning = groups[1] as HTMLElement;
    expect(within(warning).getByText("DB_PASSWORD looks like a secret.")).toBeTruthy();
    expect(within(warning).getByText(".env:2")).toBeTruthy();
    expect(warning.textContent).toContain("Warning: ");
  });

  it("shows the submodule, LFS, escaping, link and missing-file rows with their paths", () => {
    renderSummary(
      settled({
        check: checkOf({
          services: ["web"],
          missing: ["secrets/db_pw.txt"],
          problems: [
            problem({
              code: "git_submodule",
              message: "vendor/lib is a submodule. Coaming doesn't support submodules.",
              file: "compose.yaml",
              line: 5,
            }),
            problem({
              code: "git_lfs_pointer",
              message: "data/big.bin is a Git LFS pointer. Coaming doesn't support Git LFS.",
              file: "compose.yaml",
              line: 9,
            }),
            problem({
              code: "path_illegal",
              message: "../x points outside the stack folder. Coaming only reads files inside it.",
              file: "compose.yaml",
              line: 14,
            }),
            problem({
              code: "git_symlink_outside",
              message: "conf is a link to somewhere outside the stack folder.",
              file: "compose.yaml",
              line: 18,
            }),
            problem({
              code: "repo_file_missing",
              message: "secrets/db_pw.txt isn't in the repository.",
              file: "compose.yaml",
              line: 22,
            }),
          ],
        }),
      }),
    );
    const blocking = screen.getByRole("group", { name: /Fix these in the repository/ });
    for (const line of [5, 9, 14, 18, 22]) {
      expect(within(blocking).getByText(`compose.yaml:${line}`)).toBeTruthy();
    }
    expect(within(blocking).getByText(/is a submodule/)).toBeTruthy();
    expect(within(blocking).getByText(/Git LFS pointer/)).toBeTruthy();
    expect(within(blocking).getByText(/points outside the stack folder/)).toBeTruthy();
    expect(within(blocking).getByText(/is a link to somewhere outside/)).toBeTruthy();
  });

  it("offers Add overlay file on a missing file, with its path", () => {
    const onAdd = vi.fn();
    renderSummary(
      settled({
        check: checkOf({
          missing: ["secrets/db_pw.txt"],
          problems: [
            problem({
              code: "repo_file_missing",
              message: "secrets/db_pw.txt isn't in the repository.",
            }),
            problem({ code: "git_submodule", message: "vendor is a submodule.", line: 1 }),
          ],
        }),
      }),
      onAdd,
    );
    const buttons = screen.getAllByRole("button", { name: "Add overlay file" });
    expect(buttons).toHaveLength(1);
    fireEvent.click(buttons[0] as HTMLElement);
    expect(onAdd).toHaveBeenCalledWith("secrets/db_pw.txt");
  });

  it("offers the path a missing-file problem is about when another begins the same (WR-E02)", () => {
    const onAdd = vi.fn();
    renderSummary(
      settled({
        check: checkOf({
          missing: [".env", ".env.prod"],
          problems: [
            problem({
              code: "repo_file_missing",
              message: ".env.prod isn't in the repository.",
            }),
          ],
        }),
      }),
      onAdd,
    );
    fireEvent.click(screen.getByRole("button", { name: "Add overlay file" }));
    expect(onAdd).toHaveBeenCalledWith(".env.prod");
  });

  it("draws a message with markup in it as text", () => {
    renderSummary(
      settled({
        check: checkOf({
          problems: [problem({ message: "<img src=x onerror=alert(1)> is bad", file: "<b>f</b>" })],
        }),
      }),
    );
    expect(document.querySelector("img")).toBeNull();
    expect(document.querySelector("b")).toBeNull();
    expect(screen.getByText("<img src=x onerror=alert(1)> is bad")).toBeTruthy();
  });

  it("says it is checking, busy, before the first answer", () => {
    const { container } = renderSummary(settled({ checking: true }));
    expect(screen.getByText("Checking the files…")).toBeTruthy();
    expect(container.querySelector("[aria-busy='true']")).not.toBeNull();
  });

  it("keeps the last result, marked busy, while the next check runs", () => {
    const { container } = renderSummary(
      settled({ checking: true, check: checkOf({ services: ["web"] }) }),
    );
    expect(screen.getByRole("status").textContent).toBe("Found 1 service: web.");
    expect(screen.queryByText("Checking the files…")).toBeNull();
    expect(container.querySelector("[aria-busy='true']")).not.toBeNull();
  });

  it("says why a check failed and checks again on Retry", () => {
    const retry = vi.fn();
    renderSummary(settled({ error: new GitRequestError(422, "ref_missing"), retry }));
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain(
      "Couldn't check the files: That branch or tag isn't in homelab any more.",
    );
    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    expect(retry).toHaveBeenCalledTimes(1);
  });

  it("does not show a result that a failed check leaves behind", () => {
    renderSummary(
      settled({ error: new GitRequestError(0), check: checkOf({ services: ["web"] }) }),
    );
    expect(screen.queryByText(/Found 1 service/)).toBeNull();
    expect(screen.getByRole("alert").textContent).toContain(
      "The server didn't answer. Check the connection and try again.",
    );
  });
});
