// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { STACK_GIT_POLL_MAX_MS, STACK_GIT_POLL_MS, type StackGit } from "@/api/stack-git";
import { type StackDetail, stackQuery } from "@/api/stacks";
import { TooltipProvider } from "@/components/ui/tooltip";
import { GitStrip } from "@/features/stack-git/git-strip";
import { SyncErrorAlert } from "@/features/stack-git/sync-error-alert";
import { StackDetailsCard } from "@/features/stacks/stack-details-card";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";
import { waitUnderFakeClock } from "@/test-settle";

const ENV = "e1";
const DEPLOYED = "0123456789abcdef0123456789abcdef01234567";
const HEAD = "fedcba9876543210fedcba9876543210fedcba98";
const CHECKED = "2026-10-28T10:00:00Z";

function makeGit(over: Partial<StackGit> = {}): StackGit {
  return {
    repoId: "r1",
    repoName: "stacks repo",
    repoHost: "github.com",
    repoPath: "acme/stacks",
    refKind: "branch",
    ref: "main",
    folder: "",
    deployedSha: DEPLOYED,
    headSha: DEPLOYED,
    behind: 0,
    behindCapped: false,
    status: "in_sync",
    checkedAt: CHECKED,
    checking: false,
    autoDeploy: false,
    autoDeployPaused: false,
    ...over,
  };
}

function makeStack(git: StackGit | undefined, over: Partial<StackDetail> = {}): StackDetail {
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
    ...over,
  } as StackDetail;
}

let served: StackDetail;
let posts: string[];
let gets: number;
let postStatus: number;
let client: QueryClient;
// What the server shows once the check it accepted is running.
let onCheck: (() => void) | undefined;

beforeEach(() => {
  posts = [];
  gets = 0;
  postStatus = 202;
  onCheck = undefined;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "viewer" } }));
      if (req.method === "POST" && path === `/api/env/${ENV}/stacks/s1/git/check`) {
        posts.push(path);
        if (postStatus !== 202) {
          return Response.json(
            { type: "internal", title: "Error", status: postStatus, detail: "no" },
            { status: postStatus },
          );
        }
        onCheck?.();
        return Response.json({ checking: true }, { status: 202 });
      }
      if (path === "/api/environments") {
        return Response.json({ items: [{ id: ENV, name: "Prod", envType: "swarm" }] });
      }
      if (path === `/api/env/${ENV}/stacks/s1`) {
        gets++;
        return Response.json(served);
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  vi.useRealTimers();
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

// Harness reads the stack as the page does and hands it to the strip.
function Harness({ part = "strip" }: { part?: "strip" | "alert" | "details" }) {
  const { data } = useQuery(stackQuery(ENV, "s1"));
  if (!data) return null;
  if (part === "alert") return <SyncErrorAlert stack={data} />;
  if (part === "details") return <StackDetailsCard stack={data} />;
  return <GitStrip stack={data} />;
}

async function mount(git: StackGit | undefined, over: Partial<StackDetail> = {}) {
  // A second mount in one test starts from what the server answers, not from what the first one cached.
  client.clear();
  served = makeStack(git, over);
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <Harness />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
  return screen.findByRole("region", { name: "Git sync" });
}

const status = (strip: HTMLElement) => within(strip).getByRole("status");

// The element a Tooltip hangs on is the focusable one around the text, not the text node's own element.
function focusTrigger(el: HTMLElement) {
  const trigger = el.closest<HTMLElement>("[tabindex]") ?? el;
  trigger.focus();
}

describe("GitStrip", () => {
  it("shows the source, the deployed commit, the status and the check time of a stack in sync", async () => {
    const strip = await mount(makeGit());
    for (const label of ["Source", "Deployed", "Status", "Checked"]) {
      expect(within(strip).getByText(label)).toBeTruthy();
    }
    expect(within(strip).getByText("main").className).toContain("font-mono");
    expect(within(strip).getByText("branch")).toBeTruthy();
    expect(within(strip).getByText("stacks repo")).toBeTruthy();
    expect(status(strip).textContent).toContain("In sync");
    expect(within(strip).getByRole("time").getAttribute("datetime")).toBe(CHECKED);
    expect(within(strip).getByRole("button", { name: "Check now" })).toBeTruthy();
  });

  it("links the deployed commit on a known host and shows plain text on another", async () => {
    const strip = await mount(makeGit());
    const link = within(strip).getByRole("link", {
      name: "Open commit 0123456 on github.com (opens in a new tab)",
    });
    expect(link.getAttribute("href")).toBe(`https://github.com/acme/stacks/commit/${DEPLOYED}`);
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    cleanup();
    const other = await mount(makeGit({ repoHost: "gitea.example.com" }));
    expect(within(other).queryByRole("link", { name: /Open commit/ })).toBeNull();
    expect(within(other).getByText("0123456").tagName).toBe("CODE");
  });

  it("names the folder under the repository only when it is not the root", async () => {
    const root = await mount(makeGit());
    expect(within(root).queryByText("apps/web")).toBeNull();
    cleanup();
    const folder = await mount(makeGit({ folder: "apps/web" }));
    expect(within(folder).getByText("apps/web").className).toContain("font-mono");
  });

  it("links the repository name to its sheet for a person who can read git, and shows text otherwise", async () => {
    const strip = await mount(makeGit());
    expect(within(strip).getByText("stacks repo").tagName).toBe("SPAN");
    cleanup();
    vi.mocked(fetch).mockImplementation(async (req) => {
      const path = new URL((req as Request).url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
      return Response.json(served);
    });
    const admin = await mount(makeGit());
    const link = await within(admin).findByRole("link", { name: "stacks repo" });
    expect(link.getAttribute("href")).toBe("/git?repo=r1");
  });

  it("says Behind by n commits with the Tooltip that names what changed", async () => {
    const strip = await mount(makeGit({ status: "behind", behind: 3, headSha: HEAD }));
    expect(status(strip).textContent).toContain("Behind by 3 commits");
    focusTrigger(within(strip).getByText("Behind by 3 commits"));
    const tip = await screen.findByRole("tooltip");
    expect(tip.textContent).toBe(
      "3 commits changed this stack's folder since 0123456. main is at fedcba9.",
    );
  });

  it("reads Behind by 1 commit for one, and a floor for a walk that stopped at its cap", async () => {
    const one = await mount(makeGit({ status: "behind", behind: 1, headSha: HEAD }));
    expect(status(one).textContent).toContain("Behind by 1 commit");
    expect(status(one).textContent).not.toContain("commits");
    cleanup();
    const capped = await mount(
      makeGit({ status: "behind", behind: 2000, behindCapped: true, headSha: HEAD }),
    );
    expect(status(capped).textContent).toContain("Behind by 2000+ commits");
  });

  it("says In sync with a Tooltip when the head moved without changing the folder", async () => {
    const strip = await mount(makeGit({ headSha: HEAD, folder: "apps/web" }));
    expect(status(strip).textContent).toContain("In sync");
    focusTrigger(within(strip).getByText("In sync"));
    const tip = await screen.findByRole("tooltip");
    expect(tip.textContent).toBe("main is at fedcba9, but nothing in apps/web changed.");
  });

  it("has no Tooltip on In sync when the head is the deployed commit", async () => {
    const strip = await mount(makeGit());
    focusTrigger(within(strip).getByText("In sync"));
    expect(within(strip).getByText("In sync").closest("[tabindex]")).toBeNull();
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("says Error with the reason under it, and nothing of the server's own text", async () => {
    const strip = await mount(
      makeGit({
        status: "error",
        reasonCode: "ref_gone",
        reasonDetail: "The branch or tag no longer exists.",
      }),
    );
    const region = status(strip);
    expect(within(region).getByText("Error")).toBeTruthy();
    expect(within(region).getByText("main no longer exists.")).toBeTruthy();
  });

  it("says why a fetch failed from the fixed sentence the server stored", async () => {
    const strip = await mount(
      makeGit({
        status: "error",
        reasonCode: "fetch_failed",
        reasonDetail: "The server refused the credential.",
      }),
    );
    expect(
      within(status(strip)).getByText(
        "Bayplan couldn't fetch the repository: the server refused the credential.",
      ),
    ).toBeTruthy();
  });

  it("shows a Fixed commit without Check now", async () => {
    const strip = await mount(makeGit({ refKind: "commit", ref: DEPLOYED }));
    expect(status(strip).textContent).toContain("Fixed commit");
    expect(within(strip).getByText("commit")).toBeTruthy();
    expect(within(strip).queryByRole("button", { name: "Check now" })).toBeNull();
    expect(within(strip).getByRole("link", { name: "Git settings" })).toBeTruthy();
  });

  it("says Not deployed yet and Never for a stack that has had nothing", async () => {
    const strip = await mount(
      makeGit({ deployedSha: "", headSha: "", status: "unknown", checkedAt: undefined }),
    );
    expect(within(strip).getByText("Not deployed yet")).toBeTruthy();
    expect(within(strip).getByText("Never")).toBeTruthy();
    expect(status(strip).textContent).toContain("Not compared yet");
    expect(within(strip).queryByRole("link", { name: /Open commit/ })).toBeNull();
  });

  it("notes a stack that is behind because its source changed, without a count", async () => {
    const strip = await mount(
      makeGit({ status: "behind", behind: 1, reasonCode: "source_changed", headSha: HEAD }),
    );
    expect(status(strip).textContent).toContain("Source changed");
    expect(status(strip).textContent).not.toContain("Behind by");
  });

  it("links to the Git settings", async () => {
    const strip = await mount(makeGit());
    expect(within(strip).getByRole("link", { name: "Git settings" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1/git",
    );
  });

  it("shows a skeleton of the same bar while a git stack's summary is not there", async () => {
    const strip = await mount(undefined);
    expect(strip.getAttribute("aria-busy")).toBe("true");
    expect(within(strip).queryByRole("button")).toBeNull();
  });

  it("draws nothing for a stack that is not from git", async () => {
    served = makeStack(undefined, { source: "inline" });
    render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <Harness />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
    await waitFor(() => expect(gets).toBeGreaterThan(0));
    expect(screen.queryByRole("region", { name: "Git sync" })).toBeNull();
  });

  it("clamps a long ref and a long repository name to two lines, with the whole in a Tooltip", async () => {
    const ref = `feature/${"x".repeat(200)}`;
    const name = `${"long-repository-name-".repeat(6)}end`;
    const strip = await mount(makeGit({ ref, repoName: name, folder: "a/b/c" }));
    const refEl = within(strip).getByText(ref);
    expect(refEl.className).toContain("line-clamp-2");
    expect(refEl.className).toContain("break-all");
    expect(within(strip).getByText(name).className).toContain("line-clamp-2");
    refEl.focus();
    expect((await screen.findByRole("tooltip")).textContent).toBe(ref);
  });

  it("stacks the status first and gives the buttons 44px under 768px", async () => {
    const strip = await mount(makeGit());
    const statusItem = within(strip).getByText("Status").parentElement;
    expect(statusItem?.className).toContain("max-md:order-first");
    expect(within(strip).getByRole("button", { name: "Check now" }).className).toContain(
      "max-md:h-11",
    );
    expect(within(strip).getByRole("link", { name: "Git settings" }).className).toContain(
      "max-md:h-11",
    );
  });
});

describe("Check now", () => {
  it("asks the server to check, shows Checking… in the status and disables itself until the answer", async () => {
    const strip = await mount(makeGit());
    // The server is checking once it has accepted the request.
    onCheck = () => {
      served = makeStack(makeGit({ checking: true }));
    };
    vi.useFakeTimers();
    fireEvent.click(within(strip).getByRole("button", { name: "Check now" }));
    await waitUnderFakeClock(() => expect(posts).toHaveLength(1));
    await waitUnderFakeClock(() => expect(status(strip).textContent).toContain("Checking…"));
    const button = within(strip).getByRole("button", { name: "Checking…" });
    expect((button as HTMLButtonElement).disabled).toBe(true);

    // A press while it runs starts nothing.
    fireEvent.click(button);
    expect(posts).toHaveLength(1);

    // The check ends: the next read of the stack, 2 s later, has the answer.
    served = makeStack(
      makeGit({ status: "behind", behind: 1, headSha: HEAD, checkedAt: "2026-10-28T11:00:00Z" }),
    );
    await act(async () => {
      await vi.advanceTimersByTimeAsync(STACK_GIT_POLL_MS);
    });
    await waitUnderFakeClock(() =>
      expect(status(strip).textContent).toContain("Behind by 1 commit"),
    );
    const again = within(strip).getByRole("button", { name: "Check now" });
    expect((again as HTMLButtonElement).disabled).toBe(false);
  });

  it("stops reading the stack every 2 s after 30 s however long the server says it checks", async () => {
    const strip = await mount(makeGit());
    onCheck = () => {
      served = makeStack(makeGit({ checking: true }));
    };
    vi.useFakeTimers();
    fireEvent.click(within(strip).getByRole("button", { name: "Check now" }));
    await waitUnderFakeClock(() => expect(posts).toHaveLength(1));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(STACK_GIT_POLL_MAX_MS + 1_000);
    });
    const after = gets;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(STACK_GIT_POLL_MS * 3);
    });
    expect(gets).toBe(after);
  });

  it("tells the person when the check could not be started and lets them try again", async () => {
    const strip = await mount(makeGit());
    postStatus = 500;
    fireEvent.click(within(strip).getByRole("button", { name: "Check now" }));
    await waitFor(() => expect(posts).toHaveLength(1));
    await waitFor(() =>
      expect(
        (within(strip).getByRole("button", { name: "Check now" }) as HTMLButtonElement).disabled,
      ).toBe(false),
    );
  });
});

// The part of the page a test looks at, mounted the way the page mounts it.
async function mountPart(
  part: "alert" | "details",
  git: StackGit,
  over: Partial<StackDetail> = {},
) {
  client.clear();
  served = makeStack(git, over);
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <Harness part={part} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("SyncErrorAlert", () => {
  // One fixture per reason code of the generated union: its sentence, as the alert and the strip say it.
  const cases: [NonNullable<StackGit["reasonCode"]>, Partial<StackGit>, string][] = [
    [
      "fetch_failed",
      { reasonDetail: "The server refused the credential." },
      "Bayplan couldn't fetch the repository: the server refused the credential.",
    ],
    ["host_key_changed", {}, "The host key for github.com has changed."],
    ["host_key_untrusted", {}, "Host key not trusted yet. Confirm the key for github.com."],
    ["ref_gone", {}, "main no longer exists."],
    ["folder_gone", { folder: "apps/web" }, "apps/web no longer exists at main."],
    [
      "checks_failed",
      { headSha: HEAD, reasonDetail: "compose.yaml: undefined variable DB_PASS" },
      "Commit fedcba9 didn't pass the checks: compose.yaml: undefined variable DB_PASS.",
    ],
    [
      "auto_deploy_failed",
      { headSha: HEAD, reasonDetail: "The deploy timed out." },
      "The automatic deploy of fedcba9 failed: The deploy timed out.",
    ],
    [
      "auto_deploy_blocked",
      { headSha: HEAD, reasonDetail: "compose.yaml has local edits that were not reviewed" },
      "Commit fedcba9 is waiting: compose.yaml has local edits that were not reviewed. Nothing was deployed.",
    ],
  ];

  it.each(cases)("says why for %s, in the alert and in the strip", async (code, over, sentence) => {
    await mountPart("alert", makeGit({ status: "error", reasonCode: code, ...over }));
    const alert = await screen.findByRole("status");
    expect(within(alert).getByText("web can't sync with stacks repo")).toBeTruthy();
    expect(
      within(alert).getByText(`${sentence} The last good version, 0123456, is still running.`),
    ).toBeTruthy();
    cleanup();
    const strip = await mount(makeGit({ status: "error", reasonCode: code, ...over }));
    expect(within(status(strip)).getByText(sentence)).toBeTruthy();
  });

  it("covers every reason code the server can send but source_changed", () => {
    // source_changed is a note on a stack that is behind, not an error, and has no sentence.
    expect(cases.map(([code]) => code).sort()).toEqual(
      [
        "auto_deploy_blocked",
        "auto_deploy_failed",
        "checks_failed",
        "fetch_failed",
        "folder_gone",
        "host_key_changed",
        "host_key_untrusted",
        "ref_gone",
      ].sort(),
    );
  });

  it("is a polite status with Check now and Open git settings, and no way to dismiss it", async () => {
    await mountPart("alert", makeGit({ status: "error", reasonCode: "ref_gone" }));
    const alert = await screen.findByRole("status");
    expect(alert.getAttribute("aria-live")).toBe("polite");
    expect(within(alert).getByRole("button", { name: "Check now" })).toBeTruthy();
    expect(
      within(alert).getByRole("link", { name: "Open git settings" }).getAttribute("href"),
    ).toBe("/env/e1/stacks/s1/git");
    expect(within(alert).getAllByRole("button")).toHaveLength(1);
  });

  it("asks for a check from its Check now", async () => {
    await mountPart("alert", makeGit({ status: "error", reasonCode: "ref_gone" }));
    const alert = await screen.findByRole("status");
    fireEvent.click(within(alert).getByRole("button", { name: "Check now" }));
    await waitFor(() => expect(posts).toHaveLength(1));
  });

  it("leaves out the last good version when nothing was deployed from the repository", async () => {
    await mountPart(
      "alert",
      makeGit({ status: "error", reasonCode: "ref_gone", deployedSha: "", headSha: "" }),
    );
    const alert = await screen.findByRole("status");
    expect(within(alert).getByText("main no longer exists.")).toBeTruthy();
    expect(alert.textContent).not.toContain("last good version");
  });

  it("names a repository it cannot read as the repository", async () => {
    await mountPart("alert", makeGit({ status: "error", reasonCode: "ref_gone", repoName: "" }));
    expect(await screen.findByText("web can't sync with its repository")).toBeTruthy();
  });

  it.each([
    ["in sync", makeGit()],
    ["behind", makeGit({ status: "behind", behind: 2, headSha: HEAD })],
    [
      "behind because its source changed",
      makeGit({ status: "behind", behind: 1, reasonCode: "source_changed" }),
    ],
    ["not checked yet", makeGit({ status: "unknown", checkedAt: undefined })],
  ])("is not shown for a stack that is %s", async (_name, git) => {
    await mountPart("alert", git);
    await waitFor(() => expect(gets).toBeGreaterThan(0));
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("draws the reason of a hostile repository as text", async () => {
    await mountPart(
      "alert",
      makeGit({
        status: "error",
        reasonCode: "checks_failed",
        headSha: HEAD,
        reasonDetail: "<script>alert(1)</script>",
      }),
    );
    const alert = await screen.findByRole("status");
    expect(alert.textContent).toContain("<script>alert(1)</script>");
    expect(document.querySelector("script")).toBeNull();
  });
});

describe("the Details rows of a git stack", () => {
  const row = (label: string) => {
    const dt = screen.getByText(label, { selector: "dt" });
    return dt.nextElementSibling as HTMLElement;
  };

  it("replaces Source with the git rows", async () => {
    await mountPart(
      "details",
      makeGit({
        folder: "apps/web",
        composeFiles: ["compose.yaml", "compose.prod.yaml"],
        autoDeploy: true,
        status: "behind",
        behind: 2,
        headSha: HEAD,
      } as Partial<StackGit>),
    );
    await screen.findByText("Repository", { selector: "dt" });
    expect(row("Source").textContent).toBe("Git");
    expect(row("Repository").textContent).toBe("stacks repo");
    expect(row("Ref").textContent).toBe("main branch");
    expect(row("Ref").querySelector("code")?.textContent).toBe("main");
    expect(row("Folder").textContent).toBe("apps/web");
    expect(row("Compose files").textContent).toBe("compose.yamlcompose.prod.yaml");
    expect(row("Auto-deploy").textContent).toBe("On");
    expect(row("Checked").querySelector("time")?.getAttribute("datetime")).toBe(CHECKED);
  });

  it("reads Repository root for the root folder", async () => {
    await mountPart(
      "details",
      makeGit({ folder: "", composeFiles: ["compose.yaml"] } as Partial<StackGit>),
    );
    await screen.findByText("Folder", { selector: "dt" });
    expect(row("Folder").textContent).toBe("Repository root");
  });

  it("shows the deployed commit in full with Copy and a link on a known host", async () => {
    await mountPart("details", makeGit());
    await screen.findByText("Deployed commit", { selector: "dt" });
    const cell = row("Deployed commit");
    expect(cell.textContent).toContain(DEPLOYED);
    expect(within(cell).getByRole("button", { name: /Copy/ })).toBeTruthy();
    expect(
      within(cell).getByRole("link", { name: /Open commit 0123456 on github.com/ }),
    ).toBeTruthy();
  });

  it("shows the latest commit only while the stack is behind", async () => {
    await mountPart("details", makeGit({ status: "behind", behind: 2, headSha: HEAD }));
    await screen.findByText("Latest commit", { selector: "dt" });
    expect(row("Latest commit").textContent).toContain("fedcba9");
    cleanup();
    await mountPart("details", makeGit());
    await screen.findByText("Deployed commit", { selector: "dt" });
    expect(screen.queryByText("Latest commit", { selector: "dt" })).toBeNull();
  });

  it("says when auto-deploy is off or paused, and when nothing is deployed or checked", async () => {
    await mountPart(
      "details",
      makeGit({ deployedSha: "", headSha: "", status: "unknown", checkedAt: undefined }),
    );
    await screen.findByText("Auto-deploy", { selector: "dt" });
    expect(row("Auto-deploy").textContent).toBe("Off. Waits for review.");
    expect(row("Deployed commit").textContent).toBe("Not deployed yet");
    expect(row("Checked").textContent).toBe("Never");
    cleanup();
    await mountPart("details", makeGit({ autoDeploy: true, autoDeployPaused: true }));
    await screen.findByText("Auto-deploy", { selector: "dt" });
    expect(row("Auto-deploy").textContent).toBe("Paused");
  });

  it("keeps the stack's own Source row for a stack that is not from git", async () => {
    served = makeStack(undefined, { source: "inline" });
    render(
      <QueryClientProvider client={client}>
        <TestRouter>
          <TooltipProvider>
            <Harness part="details" />
          </TooltipProvider>
        </TestRouter>
      </QueryClientProvider>,
    );
    await screen.findByText("Source", { selector: "dt" });
    expect(row("Source").textContent).toBe("Inline");
    expect(screen.queryByText("Repository", { selector: "dt" })).toBeNull();
  });
});
