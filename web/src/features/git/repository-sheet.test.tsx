// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { GitRepo } from "@/api/git";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { RepositorySheet } from "@/features/git/repository-sheet";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

let client: QueryClient;
let me: ReturnType<typeof makeMe>;
let posted: Record<string, unknown>[];
let tested: { path: string; body: Record<string, unknown> }[];
let testStatus: number;
let testBody: unknown;
let testGate: Promise<void> | null;
let closed: number;
let credentials: unknown[];
let credentialsPosted: Record<string, unknown>[];
let put: { path: string; body: Record<string, unknown> }[];
let putStatus: number;
let putBody: unknown;

const instance = { kind: "instance", envId: null, envName: null };
const lab = { kind: "environment", envId: "e1", envName: "Lab" };

const credential = (
  id: string,
  name: string,
  scope: unknown,
  extra: Record<string, unknown> = {},
) => ({
  id,
  name,
  scope,
  type: "https",
  username: "deploy",
  last4: "WXYZ",
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

const step = (state: string) => ({ state });
const passed = {
  branchCount: 12,
  branches: ["main", "dev"],
  defaultBranch: "main",
  failure: null,
  hostKey: { state: "none", type: "", fingerprint: "", key: "", savedFingerprint: "" },
  steps: { reachable: step("passed"), signedIn: step("not_needed"), refs: step("passed") },
  tagCount: 4,
  tags: ["v1"],
};
const failed = {
  branchCount: 0,
  branches: null,
  defaultBranch: "",
  failure: { code: "unreachable", host: "git.example.com" },
  hostKey: { state: "none", type: "", fingerprint: "", key: "", savedFingerprint: "" },
  steps: { reachable: step("failed"), signedIn: step("not_checked"), refs: step("not_checked") },
  tagCount: 0,
  tags: null,
};

beforeEach(() => {
  me = makeMe({ instanceAdmin: true });
  posted = [];
  tested = [];
  testStatus = 200;
  testBody = passed;
  testGate = null;
  closed = 0;
  credentials = [];
  credentialsPosted = [];
  put = [];
  putStatus = 200;
  putBody = {};
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/environments") {
        return Response.json({ items: [{ id: "e1", name: "Lab", envType: "standalone" }] });
      }
      if (url.pathname === "/api/git/repos/test" && req.method === "POST") {
        tested.push({ path: url.pathname, body: (await req.json()) as Record<string, unknown> });
        if (testGate) await testGate;
        return Response.json(testBody, { status: testStatus });
      }
      if (url.pathname === "/api/git/repos" && req.method === "POST") {
        const body = (await req.json()) as Record<string, unknown>;
        posted.push(body);
        return Response.json(
          {
            id: "r1",
            name: body.name,
            url: body.url,
            scope: { kind: "instance", envId: null, envName: null },
          },
          { status: 201 },
        );
      }
      if (url.pathname === "/api/git/repos") return Response.json({ items: [] });
      if (url.pathname === "/api/git/credentials" && req.method === "POST") {
        const body = (await req.json()) as Record<string, unknown>;
        credentialsPosted.push(body);
        return Response.json(
          {
            ...credential("cNew", "new-cred", instance),
            name: body.name,
            type: body.type,
            username: body.username,
          },
          { status: 201 },
        );
      }
      if (url.pathname === "/api/git/credentials") return Response.json({ items: credentials });
      if (/^\/api\/(env\/[^/]+\/)?git\/repos\/[^/]+$/.test(url.pathname) && req.method === "PUT") {
        put.push({ path: url.pathname, body: (await req.json()) as Record<string, unknown> });
        return Response.json(putBody, { status: putStatus });
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

function Harness({ repo }: { repo?: GitRepo }) {
  const [open, setOpen] = useState(true);
  return open ? (
    <RepositorySheet
      mode={repo ? "edit" : "add"}
      repo={repo}
      onClose={() => {
        closed += 1;
        setOpen(false);
      }}
    />
  ) : null;
}

function mount(repo?: GitRepo) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <Harness repo={repo} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const field = (name: string | RegExp) => screen.getByLabelText(name) as HTMLInputElement;
const type = (name: string | RegExp, value: string) =>
  fireEvent.change(field(name), { target: { value } });
const save = () => fireEvent.click(screen.getByRole("button", { name: "Save repository" }));
const runTest = () => fireEvent.click(screen.getByRole("button", { name: "Test repository" }));

describe("RepositorySheet address rules", () => {
  it("refuses a user name and password in the address with the UI-SPEC sentence", async () => {
    mount();
    await screen.findByText("Add a repository");
    type("Name", "stacks");
    type("Repository address", "https://alice:secret@git.example.com/acme/stacks.git");
    save();
    expect(
      await screen.findByText(
        "Remove the username and password from the address and add them as a credential.",
      ),
    ).toBeTruthy();
    expect(posted).toEqual([]);
  });

  it("refuses an address that is not https, ssh or the scp form", async () => {
    mount();
    await screen.findByText("Add a repository");
    type("Name", "stacks");
    type("Repository address", "http://git.example.com/acme/stacks.git");
    save();
    expect(
      await screen.findByText("Use an https:// or ssh:// address, or git@host:path."),
    ).toBeTruthy();
    expect(posted).toEqual([]);
  });

  it("asks for a name and an address when nothing is filled in", async () => {
    mount();
    await screen.findByText("Add a repository");
    save();
    expect(await screen.findByText("Enter a name.")).toBeTruthy();
    expect(screen.getByText("Enter the repository address.")).toBeTruthy();
  });

  it("has no control to skip certificate checks, and says so under Advanced", async () => {
    mount();
    await screen.findByText("Add a repository");
    fireEvent.click(screen.getByRole("button", { name: "Advanced" }));
    expect(
      await screen.findByText("Bayplan has no option to skip certificate checks."),
    ).toBeTruthy();
    expect(screen.getByLabelText("Custom CA certificate (PEM)")).toBeTruthy();
    expect(screen.queryByRole("switch")).toBeNull();
    expect(screen.queryByText(/insecure|skip tls|skip verification/i)).toBeNull();
  });
});

describe("RepositorySheet test", () => {
  it("runs the test on the typed values and shows the three steps and the summary", async () => {
    mount();
    await screen.findByText("Add a repository");
    type("Repository address", "https://git.example.com/acme/stacks.git");
    runTest();
    const summary = await screen.findByText("Found 12 branches and 4 tags. Default branch: main.");
    expect(tested).toEqual([
      { path: "/api/git/repos/test", body: { url: "https://git.example.com/acme/stacks.git" } },
    ]);
    const steps = summary.closest("div")?.parentElement as HTMLElement;
    const list = within(steps).getAllByRole("listitem").slice(0, 3);
    expect(list.map((li) => li.textContent)).toEqual([
      "Repository reachablePassed",
      "Signed inNot needed",
      "Branches and tags foundPassed",
    ]);
    // The summary is announced politely.
    expect(summary.closest('[aria-live="polite"]')).toBeTruthy();
  });

  it("lists the branches and tags behind Show branches and tags, with how many more", async () => {
    testBody = { ...passed, branchCount: 25, branches: ["main", "dev"], tagCount: 4, tags: ["v1"] };
    mount();
    await screen.findByText("Add a repository");
    type("Repository address", "https://git.example.com/acme/stacks.git");
    runTest();
    await screen.findByText(/Found 25 branches/);
    fireEvent.click(screen.getByRole("button", { name: "Show branches and tags" }));
    expect(await screen.findByText("main")).toBeTruthy();
    expect(screen.getByText("v1")).toBeTruthy();
    expect(screen.getByText("and 23 more")).toBeTruthy();
    expect(screen.getByText("and 3 more")).toBeTruthy();
  });

  it("shows Testing… and clears the previous result while a test runs", async () => {
    mount();
    await screen.findByText("Add a repository");
    type("Repository address", "https://git.example.com/acme/stacks.git");
    runTest();
    await screen.findByText(/Found 12 branches/);
    let release: () => void = () => undefined;
    testGate = new Promise((resolve) => {
      release = resolve;
    });
    runTest();
    expect(await screen.findByRole("button", { name: "Testing…" })).toBeTruthy();
    expect(screen.queryByText(/Found 12 branches/)).toBeNull();
    release();
    expect(await screen.findByText(/Found 12 branches/)).toBeTruthy();
  });

  it("does not test an address the rules refuse", async () => {
    mount();
    await screen.findByText("Add a repository");
    type("Repository address", "https://alice:secret@git.example.com/a.git");
    runTest();
    expect(
      await screen.findByText(
        "Remove the username and password from the address and add them as a credential.",
      ),
    ).toBeTruthy();
    expect(tested).toEqual([]);
  });

  it("keeps Save enabled after a failed test and shows the failure and the warning", async () => {
    testBody = failed;
    mount();
    await screen.findByText("Add a repository");
    type("Name", "stacks");
    type("Repository address", "https://git.example.com/acme/stacks.git");
    runTest();
    expect(
      await screen.findByText(
        "Failed: Couldn't reach git.example.com. Check the address, DNS and firewall.",
      ),
    ).toBeTruthy();
    expect(
      screen.getByText("The last test failed. You can still save this repository."),
    ).toBeTruthy();
    const saveButton = screen.getByRole("button", { name: "Save repository" });
    expect((saveButton as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(saveButton);
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({
      name: "stacks",
      url: "https://git.example.com/acme/stacks.git",
    });
    await waitFor(() => expect(closed).toBe(1));
  });

  it("keeps the warning inside the field group so the layout does not collapse (G-05-4a)", async () => {
    testBody = failed;
    mount();
    await screen.findByText("Add a repository");
    type("Repository address", "https://git.example.com/acme/stacks.git");
    runTest();
    const warning = await screen.findByText(
      "The last test failed. You can still save this repository.",
    );
    expect(warning.closest('[data-slot="field-group"]')).toBeTruthy();
  });

  it("words a rate limit and a refused permission, never the server's text", async () => {
    testStatus = 429;
    testBody = { title: "slow down", status: 429, type: "rate_limited" };
    mount();
    await screen.findByText("Add a repository");
    type("Repository address", "https://git.example.com/acme/stacks.git");
    runTest();
    expect(await screen.findByText("Too many tests. Wait a minute and try again.")).toBeTruthy();
    expect(screen.queryByText("slow down")).toBeNull();
  });
});

describe("RepositorySheet save", () => {
  it("saves a public repository, toasts and closes", async () => {
    mount();
    await screen.findByText("Add a repository");
    type("Name", "homelab-stacks");
    type("Repository address", "https://git.example.com/acme/stacks.git");
    save();
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({
      name: "homelab-stacks",
      url: "https://git.example.com/acme/stacks.git",
    });
    expect(await screen.findByText("Added repository homelab-stacks.")).toBeTruthy();
    await waitFor(() => expect(closed).toBe(1));
  });
});

const openCredentials = async () => {
  fireEvent.keyDown(screen.getByRole("combobox", { name: "Credential" }), { key: "Enter" });
  return screen.findAllByRole("option");
};

describe("RepositorySheet credential pick", () => {
  beforeEach(() => {
    credentials = [
      credential("c-inst", "inst-cred", instance),
      credential("c-lab", "lab-cred", lab),
      credential("c-prod", "prod-cred", { kind: "environment", envId: "e2", envName: "Prod" }),
    ];
  });

  it("lists None, then this scope's credentials, then Instance credentials, for an environment repository", async () => {
    me = makeMe({ envs: { e1: "admin" } });
    mount();
    await screen.findByText("Add a repository");
    await waitFor(() => expect(screen.getByLabelText("Scope").textContent).toContain("Lab"));
    const options = await openCredentials();
    expect(options.map((o) => o.textContent)).toEqual([
      "None (public repository)",
      "lab-cred",
      "inst-cred",
      "Add a credential…",
    ]);
    expect(screen.getByText("This scope")).toBeTruthy();
    expect(screen.getByText("Instance")).toBeTruthy();
  });

  it("lists only Instance credentials for an instance repository", async () => {
    mount();
    await screen.findByText("Add a repository");
    await waitFor(() =>
      expect(screen.getByLabelText("Scope").textContent).toContain("All environments (instance)"),
    );
    const options = await openCredentials();
    expect(options.map((o) => o.textContent)).toEqual([
      "None (public repository)",
      "inst-cred",
      "Add a credential…",
    ]);
  });

  it("does not offer Add a credential… to someone who may not add one in that scope", async () => {
    me = makeMe({ allEnvironments: "viewer", envs: { e1: "admin" } });
    mount();
    await screen.findByText("Add a repository");
    await waitFor(() => expect(screen.getByLabelText("Scope").textContent).toContain("Lab"));
    const options = await openCredentials();
    expect(options.map((o) => o.textContent)).not.toContain("Add a credential…");
  });

  it("saves the picked credential by its ID", async () => {
    mount();
    await screen.findByText("Add a repository");
    type("Name", "stacks");
    type("Repository address", "https://git.example.com/acme/stacks.git");
    const options = await openCredentials();
    fireEvent.click(options.find((o) => o.textContent === "inst-cred") as HTMLElement);
    save();
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({
      name: "stacks",
      url: "https://git.example.com/acme/stacks.git",
      credentialId: "c-inst",
    });
  });

  it("opens the credential sheet over the repository sheet and returns with the new credential selected", async () => {
    mount();
    await screen.findByText("Add a repository");
    type("Name", "stacks");
    type("Repository address", "git@git.example.com:acme/stacks.git");
    const options = await openCredentials();
    fireEvent.click(options.find((o) => o.textContent === "Add a credential…") as HTMLElement);
    expect(await screen.findByText("Add a git credential")).toBeTruthy();
    const credentialSheet = screen.getByText("Add a git credential").closest('[role="dialog"]');
    expect(credentialSheet).toBeTruthy();
    const inSheet = within(credentialSheet as HTMLElement);
    fireEvent.change(inSheet.getByLabelText("Name"), { target: { value: "new-cred" } });
    fireEvent.change(inSheet.getByLabelText("Username"), { target: { value: "deploy" } });
    fireEvent.change(inSheet.getByLabelText("Token"), {
      target: { value: "ghp_0123456789abcdef" },
    });
    fireEvent.click(inSheet.getByRole("button", { name: "Save credential" }));
    await waitFor(() => expect(credentialsPosted).toHaveLength(1));
    await waitFor(() => expect(screen.queryByText("Add a git credential")).toBeNull());
    // The repository sheet is still open with its values, and the new credential is picked.
    expect(screen.getByText("Add a repository")).toBeTruthy();
    expect(field("Name").value).toBe("stacks");
    expect(field("Repository address").value).toBe("git@git.example.com:acme/stacks.git");
    expect(screen.getByRole("combobox", { name: "Credential" }).textContent).toContain("new-cred");
    expect(closed).toBe(0);
  });

  it("hints, in muted text, when an SSH key meets an https address and the reverse, and still saves", async () => {
    credentials = [
      credential("c-key", "deploy-key", instance, {
        type: "ssh",
        last4: "",
        fingerprint: "SHA256:abc",
        publicKey: "ssh-ed25519 AAAA",
      }),
      credential("c-tok", "token", instance),
    ];
    mount();
    await screen.findByText("Add a repository");
    type("Name", "stacks");
    type("Repository address", "https://git.example.com/acme/stacks.git");
    let options = await openCredentials();
    fireEvent.click(options.find((o) => o.textContent === "deploy-key") as HTMLElement);
    expect(
      await screen.findByText(
        "This credential is an SSH key. Use an HTTPS token with an https:// address, or change the address to the ssh form.",
      ),
    ).toBeTruthy();
    type("Repository address", "git@git.example.com:acme/stacks.git");
    expect(screen.queryByText(/This credential is an SSH key\./)).toBeNull();
    options = await openCredentials();
    fireEvent.click(options.find((o) => o.textContent === "token") as HTMLElement);
    expect(
      await screen.findByText(
        "This credential is an HTTPS token. Use an SSH deploy key with an ssh address.",
      ),
    ).toBeTruthy();
    const saveButton = screen.getByRole("button", { name: "Save repository" });
    expect((saveButton as HTMLButtonElement).disabled).toBe(false);
  });
});

const savedRepo = (extra: Record<string, unknown> = {}) =>
  ({
    id: "r1",
    name: "stacks",
    scope: instance,
    url: "https://git.example.com/acme/stacks.git",
    host: "git.example.com",
    caPem: "",
    hasCa: false,
    credential: { id: "c-inst", name: "inst-cred", scope: instance },
    hostKey: null,
    lastTest: null,
    readOnly: false,
    usedBy: 0,
    createdAt: "2026-10-01T10:00:00Z",
    updatedAt: "2026-10-01T10:00:00Z",
    ...extra,
  }) as GitRepo;

describe("RepositorySheet edit", () => {
  beforeEach(() => {
    credentials = [credential("c-inst", "inst-cred", instance)];
    putBody = savedRepo();
  });

  it("re-sends the credential when the address moves to another host", async () => {
    mount(savedRepo());
    await screen.findByText("Edit stacks");
    type("Repository address", "https://other.example.com/acme/stacks.git");
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(put).toHaveLength(1));
    expect(put[0]).toEqual({
      path: "/api/git/repos/r1",
      body: {
        name: "stacks",
        url: "https://other.example.com/acme/stacks.git",
        caPem: "",
        credentialId: "c-inst",
      },
    });
  });

  it("sends an empty credentialId when None is picked", async () => {
    mount(savedRepo());
    await screen.findByText("Edit stacks");
    const options = await openCredentials();
    fireEvent.click(
      options.find((o) => o.textContent === "None (public repository)") as HTMLElement,
    );
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(put).toHaveLength(1));
    expect(put[0]?.body.credentialId).toBe("");
  });

  it("words credential_reselect inline on the Credential field", async () => {
    putStatus = 422;
    putBody = {
      title: "Unprocessable Entity",
      status: 422,
      type: "validation_failed",
      errors: [{ message: "credential_reselect", location: "body.credentialId" }],
    };
    mount(savedRepo());
    await screen.findByText("Edit stacks");
    type("Repository address", "https://other.example.com/acme/stacks.git");
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    const message = await screen.findByText(
      "The address moved to another server. Pick the credential again, or choose none.",
    );
    expect(message.id).toContain("credentialId-error");
    expect(closed).toBe(0);
  });
});
