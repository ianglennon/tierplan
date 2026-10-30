// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { AppSidebar } from "@/components/app-sidebar";
import { SidebarProvider } from "@/components/ui/sidebar";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { GitPage, type GitSearch, type GitSearchPatch } from "@/features/git/git-page";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";
import { settled } from "@/test-settle";

// The switcher and the version footer have their own tests and their own endpoints; the sidebar
// test below is about which links it draws.
vi.mock("@/components/env-switcher", () => ({ EnvSwitcher: () => null }));
vi.mock("@/components/version-footer", () => ({ VersionFooter: () => null }));

let client: QueryClient;
let me: ReturnType<typeof makeMe>;
let repos: unknown[];
let credentials: unknown[];
let reposStatus: number;
let reposCalls: number;
let credentialsStatus: number;
let tests: string[];
let testResult: unknown;
let deleted: string[];
let deleteStatus: number;
let deleteBody: unknown;
let search: GitSearch;
let changes: GitSearchPatch[];

const instance = { kind: "instance", envId: null, envName: null };
const lab = { kind: "environment", envId: "e1", envName: "Lab" };

const repo = (name: string, extra: Record<string, unknown> = {}) => ({
  id: name,
  name,
  scope: instance,
  url: "https://git.example.com/acme/stacks.git",
  host: "git.example.com",
  caPem: "",
  hasCa: false,
  credential: null,
  hostKey: null,
  lastTest: null,
  readOnly: false,
  usedBy: 0,
  createdAt: "2026-10-01T10:00:00Z",
  updatedAt: "2026-10-01T10:00:00Z",
  ...extra,
});

beforeEach(() => {
  me = makeMe({ instanceAdmin: true });
  repos = [];
  credentials = [];
  reposStatus = 200;
  reposCalls = 0;
  credentialsStatus = 200;
  tests = [];
  testResult = {};
  deleted = [];
  deleteStatus = 204;
  deleteBody = undefined;
  search = {};
  changes = [];
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (req.method === "DELETE") {
        deleted.push(url.pathname);
        return deleteStatus === 204
          ? new Response(null, { status: 204 })
          : Response.json(deleteBody, { status: deleteStatus });
      }
      if (/^\/api\/(env\/[^/]+\/)?git\/repos\/[^/]+\/test$/.test(url.pathname)) {
        tests.push(url.pathname);
        return Response.json(testResult);
      }
      if (url.pathname === "/api/git/repos") {
        reposCalls += 1;
        return reposStatus === 200
          ? Response.json({ items: repos })
          : Response.json({ title: "boom", status: reposStatus }, { status: reposStatus });
      }
      if (url.pathname === "/api/git/credentials") {
        return credentialsStatus === 200
          ? Response.json({ items: credentials })
          : Response.json(
              { title: "boom", status: credentialsStatus },
              { status: credentialsStatus },
            );
      }
      if (url.pathname === "/api/environments") {
        return Response.json({
          items: [
            { id: "e1", name: "Lab", envType: "standalone" },
            { id: "e2", name: "Prod", envType: "swarm" },
          ],
        });
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

function mount(tab: "repositories" | "credentials" = "repositories") {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <GitPage tab={tab} search={search} onSearchChange={(patch) => changes.push(patch)} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const bodyRows = () => screen.getAllByRole("row").slice(1);

describe("GitPage repositories", () => {
  it("lists instance repositories first, then by name, with the columns of the UI-SPEC", async () => {
    repos = [
      repo("zeta"),
      repo("env-repo", { scope: lab }),
      repo("alpha", {
        url: "git@git.example.com:acme/stacks.git",
        credential: { id: "c1", name: "deploy-key", scope: instance },
        hostKey: {
          type: "ssh-ed25519",
          fingerprint: "SHA256:abcdefghijklmnop",
          trustedAt: "2026-10-01T10:00:00Z",
        },
        hasCa: true,
      }),
    ];
    mount();
    await screen.findByText("alpha");
    expect(bodyRows().map((r) => within(r).getAllByRole("cell")[0]?.textContent)).toEqual([
      "alpha",
      "zeta",
      "env-repo",
    ]);
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent?.trim())).toEqual([
      "Name",
      "Scope",
      "Address",
      "Credential",
      "Host key",
      "Last test",
      "Used by",
      "Actions",
    ]);
    const alpha = bodyRows()[0] as HTMLElement;
    expect(within(alpha).getByText("Instance")).toBeTruthy();
    expect(within(alpha).getByText("git@git.example.com:acme/stacks.git")).toBeTruthy();
    expect(within(alpha).getByText("Custom CA")).toBeTruthy();
    expect(within(alpha).getByText("deploy-key")).toBeTruthy();
    expect(within(alpha).getByText("Trusted")).toBeTruthy();
    expect(within(alpha).getByText("SHA256:abcde")).toBeTruthy();
    expect(within(alpha).getByText("Not tested")).toBeTruthy();
    // A public repository over https has no credential and no host key.
    const zeta = bodyRows()[1] as HTMLElement;
    expect(within(zeta).getByText("None (public)")).toBeTruthy();
    expect(within(zeta).getByText("—")).toBeTruthy();
    expect(within(bodyRows()[2] as HTMLElement).getByText("Lab")).toBeTruthy();
    expect(screen.getByText("3 repositories")).toBeTruthy();
  });

  it("says Built-in for an ssh host with shipped keys and Not trusted yet for any other", async () => {
    repos = [
      repo("gh", { url: "git@github.com:acme/stacks.git" }),
      repo("own", { url: "ssh://git@git.example.com/acme/stacks.git" }),
    ];
    mount();
    await screen.findByText("gh");
    expect(within(bodyRows()[0] as HTMLElement).getByText("Built-in")).toBeTruthy();
    expect(within(bodyRows()[1] as HTMLElement).getByText("Not trusted yet")).toBeTruthy();
  });

  it("renders a name and an address that hold markup as text", async () => {
    repos = [repo("<img src=x onerror=alert(1)>", { url: "https://x.example/<b>a</b>.git" })];
    mount();
    expect(await screen.findByText("<img src=x onerror=alert(1)>")).toBeTruthy();
    expect(screen.getByText("https://x.example/<b>a</b>.git")).toBeTruthy();
    expect(document.querySelector("img")).toBeNull();
    expect(document.querySelector("b")).toBeNull();
  });

  it("opens the add form from Add repository", async () => {
    repos = [repo("alpha")];
    mount();
    await screen.findByText("alpha");
    fireEvent.click(screen.getByRole("button", { name: "Add repository" }));
    expect(changes).toEqual([{ repo: "new" }]);
  });
});

const credential = (name: string, extra: Record<string, unknown> = {}) => ({
  id: name,
  name,
  scope: instance,
  type: "https",
  username: "deploy",
  last4: "wxyz",
  fingerprint: "",
  publicKey: "",
  readOnly: false,
  usedBy: 0,
  lastFailure: null,
  lastUseAt: null,
  createdAt: "2026-10-01T10:00:00Z",
  updatedAt: "2026-10-01T10:00:00Z",
  ...extra,
});

const openMenu = (name: string) => {
  const actions = screen.getByRole("button", { name: `Actions for ${name}` });
  fireEvent.pointerDown(actions, { button: 0, ctrlKey: false, pointerType: "mouse" });
  fireEvent.click(actions);
};

describe("GitPage credentials", () => {
  it("lists instance credentials first, then by name, with the columns of the UI-SPEC", async () => {
    credentials = [
      credential("zeta", {
        type: "ssh",
        last4: "",
        fingerprint: "SHA256:abcdefghijklmnopqrstuvwxyz",
        username: "git",
      }),
      credential("env-cred", { scope: lab }),
      credential("alpha", { usedBy: 2, lastUseAt: "2026-10-01T10:00:00Z" }),
      credential("refused", { lastFailure: { at: "2026-10-01T10:00:00Z", code: "denied" } }),
    ];
    repos = [
      repo("r-one", { credential: { id: "alpha", name: "alpha", scope: instance } }),
      repo("r-two", { credential: { id: "alpha", name: "alpha", scope: instance } }),
    ];
    mount("credentials");
    await screen.findByText("alpha");
    expect(bodyRows().map((r) => within(r).getAllByRole("cell")[0]?.textContent)).toEqual([
      "alpha",
      "refused",
      "zeta",
      "env-cred",
    ]);
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent?.trim())).toEqual([
      "Name",
      "Scope",
      "Type",
      "Identity",
      "Secret",
      "Used by",
      "Last use",
      "Actions",
    ]);
    const alpha = bodyRows()[0] as HTMLElement;
    expect(within(alpha).getByText("HTTPS token")).toBeTruthy();
    expect(within(alpha).getByText("deploy")).toBeTruthy();
    expect(within(alpha).getByText("••••wxyz")).toBeTruthy();
    expect(within(alpha).getByText("2 repositories")).toBeTruthy();
    const zeta = bodyRows()[2] as HTMLElement;
    expect(within(zeta).getByText("SSH deploy key")).toBeTruthy();
    expect(within(zeta).getByText("Private key saved")).toBeTruthy();
    expect(within(zeta).getByText("SHA256:abcdefghijklmnopqrstuvwxyz")).toBeTruthy();
    expect(within(bodyRows()[1] as HTMLElement).getByText(/Refused/)).toBeTruthy();
    expect(screen.getByText("4 credentials")).toBeTruthy();
  });

  it("opens the Add credential form", async () => {
    credentials = [credential("alpha")];
    mount("credentials");
    await screen.findByText("alpha");
    fireEvent.click(screen.getByRole("button", { name: "Add credential" }));
    expect(changes).toEqual([{ credential: "new" }]);
  });

  it("lists the repositories that use a credential in the Used by popover", async () => {
    credentials = [credential("alpha", { usedBy: 2 })];
    repos = [
      repo("r-one", { credential: { id: "alpha", name: "alpha", scope: instance } }),
      repo("r-two", { credential: { id: "alpha", name: "alpha", scope: instance } }),
    ];
    mount("credentials");
    await screen.findByText("alpha");
    fireEvent.click(screen.getByRole("button", { name: "Used by alpha" }));
    expect(await screen.findByText("r-one")).toBeTruthy();
    expect(screen.getByText("r-two")).toBeTruthy();
  });
});

describe("GitPage delete", () => {
  it("asks 'Delete “name”?' with a destructive button and deletes the repository", async () => {
    repos = [repo("alpha")];
    mount();
    await screen.findByText("alpha");
    openMenu("alpha");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Delete…" }));
    expect(await screen.findByText("Delete “alpha”?")).toBeTruthy();
    expect(
      screen.getByText(
        "Stacks can't be created from it any more. No stack uses it, so nothing else changes.",
      ),
    ).toBeTruthy();
    const confirm = screen.getByRole("button", { name: "Delete repository" });
    expect(confirm.className).toContain("bg-destructive-fill");
    fireEvent.click(confirm);
    await waitFor(() => expect(deleted).toEqual(["/api/git/repos/alpha"]));
    expect(await screen.findByText("Deleted repository alpha.")).toBeTruthy();
  });

  it("asks 'Delete “name”?' and deletes an environment credential through its route", async () => {
    credentials = [credential("lab-cred", { scope: lab })];
    mount("credentials");
    await screen.findByText("lab-cred");
    openMenu("lab-cred");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Delete…" }));
    expect(await screen.findByText("Delete “lab-cred”?")).toBeTruthy();
    expect(screen.getByText("Nothing uses it, so no repository changes.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Delete credential" }));
    await waitFor(() => expect(deleted).toEqual(["/api/env/e1/git/credentials/lab-cred"]));
  });

  it("keeps a credential in use from being deleted and says why", async () => {
    credentials = [credential("alpha", { usedBy: 2 })];
    mount("credentials");
    await screen.findByText("alpha");
    openMenu("alpha");
    const item = await screen.findByRole("menuitem", { name: "Delete…" });
    expect(item.getAttribute("aria-disabled")).toBe("true");
    expect(
      screen.getAllByText("Used by 2 repositories. Pick another credential for them first.").length,
    ).toBeGreaterThan(0);
    fireEvent.click(item);
    expect(screen.queryByText("Delete “alpha”?")).toBeNull();
    expect(deleted).toEqual([]);
  });

  it("keeps a repository in use from being deleted and says why", async () => {
    repos = [repo("alpha", { usedBy: 3 })];
    mount();
    await screen.findByText("alpha");
    openMenu("alpha");
    const item = await screen.findByRole("menuitem", { name: "Delete…" });
    expect(item.getAttribute("aria-disabled")).toBe("true");
    expect(
      screen.getAllByText("Used by 3 stacks. Point them at another repository first.").length,
    ).toBeGreaterThan(0);
    fireEvent.click(item);
    expect(deleted).toEqual([]);
  });

  it("shows the server's refusal inside the dialog when the credential became in use", async () => {
    credentials = [credential("alpha")];
    deleteStatus = 409;
    deleteBody = {
      title: "Conflict",
      status: 409,
      type: "credential_in_use",
      errors: [{ location: "conflict.count", message: "2" }],
    };
    mount("credentials");
    await screen.findByText("alpha");
    openMenu("alpha");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Delete…" }));
    fireEvent.click(await screen.findByRole("button", { name: "Delete credential" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "Used by 2 repositories. Pick another credential for them first.",
    );
  });
});

describe("GitPage empty, loading and error states", () => {
  it("offers Add repository to someone who can manage when there are none", async () => {
    mount();
    expect(await screen.findByText("No repositories")).toBeTruthy();
    expect(
      screen.getByText(
        "Add a repository so stacks can be created from git. A public repository needs no credential.",
      ),
    ).toBeTruthy();
    const buttons = screen.getAllByRole("button", { name: "Add repository" });
    fireEvent.click(buttons[buttons.length - 1] as HTMLElement);
    expect(changes).toEqual([{ repo: "new" }]);
  });

  it("tells everyone else to ask an administrator, with no Add button at all", async () => {
    me = makeMe({ envs: { e1: "viewer" } });
    mount();
    expect(await screen.findByText("No repositories apply here")).toBeTruthy();
    expect(
      screen.getByText(
        "Ask an administrator to add a repository connection before you create a stack from git.",
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Add repository" })).toBeNull();
  });

  it("says no credential applies to a viewer, and offers Add credential to an administrator", async () => {
    me = makeMe({ envs: { e1: "viewer" } });
    mount("credentials");
    expect(await screen.findByText("No git credentials apply here")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Add credential" })).toBeNull();
    cleanup();
    me = makeMe({ instanceAdmin: true });
    client.clear();
    mount("credentials");
    expect(await screen.findByText("No git credentials")).toBeTruthy();
    expect(
      screen.getByText(
        "Add a token or a deploy key for a private repository. Public repositories work without one.",
      ),
    ).toBeTruthy();
  });

  it("says nothing matches the search, with Clear filters", async () => {
    repos = [repo("alpha")];
    search = { q: "zzz" };
    mount();
    expect(await screen.findByText("No repositories match “zzz”.")).toBeTruthy();
    fireEvent.click(screen.getAllByRole("button", { name: "Clear filters" }).at(-1) as HTMLElement);
    expect(changes).toContainEqual({ q: undefined, scope: undefined });
  });

  it("says no credential matches the search", async () => {
    credentials = [credential("alpha")];
    search = { q: "zzz" };
    mount("credentials");
    expect(await screen.findByText("No credentials match “zzz”.")).toBeTruthy();
  });

  it("shows 8 skeleton rows at once, with the tabs and the toolbar", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => new Promise(() => {})),
    );
    client.setQueryData(["me"], makeMe({ instanceAdmin: true }));
    mount();
    await waitFor(() => expect(screen.getAllByRole("row")).toHaveLength(9));
    expect(screen.getByRole("table").getAttribute("aria-busy")).toBe("true");
    expect(screen.getByRole("searchbox", { name: "Search repositories" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Repositories" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Credentials" })).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Git" })).toBeTruthy();
  });

  it("shows the error with Retry loading repositories, and Retry reads the list again", async () => {
    reposStatus = 500;
    mount();
    expect(await screen.findByText("Couldn't load repositories.")).toBeTruthy();
    expect(screen.getByText("Check the connection and try again.")).toBeTruthy();
    const before = reposCalls;
    fireEvent.click(screen.getByRole("button", { name: "Retry loading repositories" }));
    await waitFor(() => expect(reposCalls).toBeGreaterThan(before));
  });

  it("shows the credentials error with its own words", async () => {
    credentialsStatus = 500;
    mount("credentials");
    expect(await screen.findByText("Couldn't load git credentials.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry loading credentials" })).toBeTruthy();
  });

  it("shows the Gone state for a repository that is not listed any more, and Back closes it", async () => {
    repos = [repo("alpha")];
    search = { repo: "missing" };
    mount();
    expect(await screen.findByText("That repository is gone")).toBeTruthy();
    expect(screen.getByText("It was deleted while you were looking at it.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Back to repositories" }));
    expect(changes).toContainEqual({ repo: undefined });
  });

  it("shows the Gone state for a credential that is not listed any more", async () => {
    credentials = [credential("alpha")];
    search = { credential: "missing" };
    mount("credentials");
    expect(await screen.findByText("That credential is gone")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Back to credentials" })).toBeTruthy();
  });

  it("shows field skeletons in the sheet while the list it needs loads", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => new Promise(() => {})),
    );
    client.setQueryData(["me"], makeMe({ instanceAdmin: true }));
    search = { repo: "alpha" };
    mount();
    const sheet = await screen.findByRole("dialog");
    expect(sheet.querySelector('[aria-busy="true"]')).toBeTruthy();
  });
});

describe("GitPage partial, plural and overflow rows", () => {
  it("shows 0 and 1 as plurals", async () => {
    repos = [repo("none"), repo("one", { usedBy: 1 })];
    credentials = [credential("c-none"), credential("c-one", { usedBy: 1 })];
    mount();
    await screen.findByText("none");
    expect(within(bodyRows()[0] as HTMLElement).getByText("0 stacks")).toBeTruthy();
    expect(within(bodyRows()[1] as HTMLElement).getByText("1 stack")).toBeTruthy();
    cleanup();
    mount("credentials");
    await screen.findByText("c-none");
    expect(within(bodyRows()[0] as HTMLElement).getByText("0 repositories")).toBeTruthy();
    expect(within(bodyRows()[1] as HTMLElement).getByText("1 repository")).toBeTruthy();
  });

  it("truncates a long address inside a cell that scrolls sideways, and keeps the whole value in the page", async () => {
    const long = `https://git.example.com/${"a".repeat(200)}.git`;
    repos = [repo("alpha", { url: long })];
    mount();
    const cell = await screen.findByText(long);
    expect(cell.className).toContain("truncate");
    const wrapper = screen.getByRole("table").parentElement as HTMLElement;
    expect(wrapper.className).toContain("overflow-x-auto");
  });

  it("lists up to 10 repositories behind a Used by count, then 'and n more'", async () => {
    credentials = [credential("alpha", { usedBy: 12 })];
    repos = Array.from({ length: 12 }, (_, i) =>
      repo(`repo-${String(i).padStart(2, "0")}`, {
        credential: { id: "alpha", name: "alpha", scope: instance },
      }),
    );
    mount("credentials");
    await screen.findByText("alpha");
    fireEvent.click(screen.getByRole("button", { name: "Used by alpha" }));
    expect(await screen.findByText("repo-09")).toBeTruthy();
    expect(screen.queryByText("repo-10")).toBeNull();
    expect(screen.getByText("and 2 more")).toBeTruthy();
  });

  it("shows a failed last test as text", async () => {
    repos = [
      repo("alpha", { lastTest: { at: "2026-10-01T10:00:00Z", ok: false, code: "unreachable" } }),
      repo("beta", { lastTest: { at: "2026-10-01T10:00:00Z", ok: true, code: "" } }),
    ];
    mount();
    await screen.findByText("alpha");
    expect(within(bodyRows()[0] as HTMLElement).getByText("Failed")).toBeTruthy();
    expect(within(bodyRows()[1] as HTMLElement).getByText("Passed")).toBeTruthy();
  });
});

describe("GitPage visibility and permissions", () => {
  it("shows a viewer the page without Add buttons, and View details only", async () => {
    me = makeMe({ envs: { e1: "viewer" } });
    repos = [repo("alpha", { readOnly: true })];
    credentials = [credential("c1", { readOnly: true })];
    mount();
    await screen.findByText("alpha");
    expect(screen.queryByRole("button", { name: "Add repository" })).toBeNull();
    openMenu("alpha");
    expect((await screen.findAllByRole("menuitem")).map((i) => i.textContent)).toEqual([
      "View details",
    ]);
  });

  it("lets an environment Admin manage their own rows and see instance rows read-only", async () => {
    me = makeMe({ envs: { e1: "admin" } });
    repos = [repo("inst", { readOnly: true }), repo("lab-repo", { scope: lab })];
    mount();
    await screen.findByText("lab-repo");
    expect(
      screen.getByRole("button", { name: "Actions for inst" }).getAttribute("aria-describedby"),
    ).toBeTruthy();
    expect(screen.getByText("Needs an instance administrator")).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Actions for lab-repo" }).getAttribute("aria-describedby"),
    ).toBeNull();
    openMenu("lab-repo");
    expect((await screen.findAllByRole("menuitem")).map((i) => i.textContent)).toEqual([
      "Edit",
      "Test",
      "Delete…",
    ]);
  });

  it("opens an instance repository as read-only details for an environment Admin, with Edit unavailable and why", async () => {
    me = makeMe({ envs: { e1: "admin" } });
    repos = [repo("inst", { readOnly: true })];
    search = { repo: "inst" };
    mount();
    expect(await screen.findByText("You can see this repository but not change it.")).toBeTruthy();
    const edit = screen.getByRole("button", { name: "Edit" });
    expect(edit.getAttribute("aria-disabled")).toBe("true");
    expect(screen.getAllByText("Needs an instance administrator").length).toBeGreaterThan(0);
  });

  it("says which role an environment repository needs when the user is a viewer there", async () => {
    me = makeMe({ envs: { e1: "viewer" } });
    repos = [repo("lab-repo", { scope: lab, readOnly: true })];
    search = { repo: "lab-repo" };
    mount();
    await screen.findByText("You can see this repository but not change it.");
    expect(screen.getAllByText("Needs Admin on Lab").length).toBeGreaterThan(0);
  });

  it("lists only the environments the user can see in the Scope filter", async () => {
    me = makeMe({ envs: { e1: "viewer" } });
    repos = [repo("alpha", { readOnly: true })];
    mount();
    await screen.findByText("alpha");
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Scope" }), { key: "Enter" });
    const options = (await screen.findAllByRole("option")).map((o) => o.textContent);
    expect(options).toEqual(["All scopes", "Instance", "Lab"]);
  });

  it("filters by scope and counts the match in the caption", async () => {
    repos = [repo("alpha"), repo("beta", { scope: lab })];
    search = { scope: "e1" };
    mount();
    await screen.findByText("beta");
    expect(screen.queryByText("alpha")).toBeNull();
    expect(screen.getByText("1 of 2 repositories")).toBeTruthy();
  });

  it("drops ?repo=new for someone who cannot add a repository", async () => {
    me = makeMe({ envs: { e1: "viewer" } });
    repos = [repo("alpha", { readOnly: true })];
    search = { repo: "new" };
    mount();
    await waitFor(() => expect(changes).toContainEqual({ repo: undefined }));
    expect(screen.queryByText("Add a repository")).toBeNull();
  });
});

describe("GitPage Test row action", () => {
  it("tests a saved repository and toasts the branches and tags", async () => {
    repos = [repo("alpha")];
    testResult = {
      branchCount: 12,
      tagCount: 4,
      branches: [],
      tags: [],
      defaultBranch: "main",
      failure: null,
      steps: {},
    };
    mount();
    await screen.findByText("alpha");
    openMenu("alpha");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Test" }));
    await waitFor(() => expect(tests).toEqual(["/api/git/repos/alpha/test"]));
    expect(await screen.findByText("Passed: found 12 branches and 4 tags.")).toBeTruthy();
  });

  it("toasts the mapped reason of a failed test, never the server's words", async () => {
    repos = [repo("alpha")];
    testResult = {
      branchCount: 0,
      tagCount: 0,
      branches: null,
      tags: null,
      defaultBranch: "",
      failure: { code: "auth_refused", host: "git.example.com" },
      steps: {},
    };
    mount();
    await screen.findByText("alpha");
    openMenu("alpha");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Test" }));
    expect(
      await screen.findByText(
        "Failed: git.example.com refused the credential. Check the token or the deploy key.",
      ),
    ).toBeTruthy();
  });
});

describe("GitPage row highlight", () => {
  it("highlights a row that appears while the page is open, then lets go of it", async () => {
    repos = [repo("alpha")];
    mount();
    await screen.findByText("alpha");
    expect(bodyRows()[0]?.getAttribute("data-changed")).toBeNull();
    repos = [repo("alpha"), repo("beta")];
    await client.invalidateQueries({ queryKey: ["git"] });
    await screen.findByText("beta");
    const row = bodyRows().find((r) => r.textContent?.includes("beta")) as HTMLElement;
    expect(row.getAttribute("data-changed")).toBe("true");
    expect(
      bodyRows()
        .find((r) => r.textContent?.includes("alpha"))
        ?.getAttribute("data-changed"),
    ).toBeNull();
    await waitFor(() => expect(row.getAttribute("data-changed")).toBeNull());
  });

  it("does not highlight the rows of the first load", async () => {
    repos = [repo("alpha"), repo("beta")];
    mount();
    await screen.findByText("beta");
    for (const row of bodyRows()) expect(row.getAttribute("data-changed")).toBeNull();
  });
});

function mountSidebar() {
  const root = createRootRoute({
    component: () => (
      <TooltipProvider>
        <SidebarProvider>
          <AppSidebar />
        </SidebarProvider>
      </TooltipProvider>
    ),
  });
  const router = createRouter({
    routeTree: root.addChildren([
      createRoute({ getParentRoute: () => root, path: "/" }),
      createRoute({ getParentRoute: () => root, path: "/environments" }),
      createRoute({ getParentRoute: () => root, path: "/registries" }),
      createRoute({ getParentRoute: () => root, path: "/git" }),
    ]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

describe("the sidebar item", () => {
  it("shows Git below Registries to a viewer of one environment", async () => {
    me = makeMe({ envs: { e1: "viewer" } });
    mountSidebar();
    const git = await screen.findByRole("link", { name: "Git" });
    const registries = screen.getByRole("link", { name: "Registries" });
    expect(registries.compareDocumentPosition(git) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(git.getAttribute("href")).toBe("/git");
  });

  it("does not show Git to someone with git.read nowhere", async () => {
    me = makeMe({});
    mountSidebar();
    await waitFor(() => expect(client.getQueryData(["me"])).toBeDefined());
    await settled(client);
    expect(screen.queryByRole("link", { name: "Git" })).toBeNull();
  });
});
