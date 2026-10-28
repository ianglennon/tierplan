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
import { GitPage } from "@/features/git/git-page";
import { RepositorySheet } from "@/features/git/repository-sheet";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

let client: QueryClient;
let me: ReturnType<typeof makeMe>;
let repos: unknown[];
let addressTests: Record<string, unknown>[];
let addressResult: unknown;
let savedTests: string[];
let savedResult: unknown;
let posted: Record<string, unknown>[];
let putBodies: Record<string, unknown>[];
let trusted: { path: string; body: Record<string, unknown> }[];
let trustStatus: number;
let closed: number;

const instance = { kind: "instance", envId: null, envName: null };

const KEY = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPresented";
const FINGERPRINT = "SHA256:Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5MGFiY2RlZmdoaWo";
const SAVED_FINGERPRINT = "SHA256:b2xka2V5b2xka2V5b2xka2V5b2xka2V5b2xka2V5MTI";

const step = (state: string) => ({ state });
const noKey = { state: "none", type: "", fingerprint: "", key: "", savedFingerprint: "" };

const untrusted = {
  branchCount: 0,
  branches: null,
  defaultBranch: "",
  failure: { code: "host_key_untrusted", host: "git.example.com" },
  hostKey: {
    state: "untrusted",
    type: "ssh-ed25519",
    fingerprint: FINGERPRINT,
    key: KEY,
    savedFingerprint: "",
  },
  steps: { reachable: step("passed"), signedIn: step("not_checked"), refs: step("not_checked") },
  tagCount: 0,
  tags: null,
};
const trustedPass = {
  branchCount: 3,
  branches: ["main"],
  defaultBranch: "main",
  failure: null,
  hostKey: {
    state: "trusted",
    type: "ssh-ed25519",
    fingerprint: FINGERPRINT,
    key: "",
    savedFingerprint: "",
  },
  steps: { reachable: step("passed"), signedIn: step("passed"), refs: step("passed") },
  tagCount: 1,
  tags: ["v1"],
};
const builtinPass = {
  ...trustedPass,
  hostKey: { ...noKey, state: "builtin", type: "ssh-ed25519", fingerprint: "SHA256:builtin" },
};
const changedFail = {
  ...untrusted,
  failure: { code: "host_key_changed", host: "git.example.com" },
  hostKey: {
    state: "changed",
    type: "ssh-ed25519",
    fingerprint: FINGERPRINT,
    key: KEY,
    savedFingerprint: SAVED_FINGERPRINT,
  },
};

const repo = (name: string, extra: Record<string, unknown> = {}): GitRepo =>
  ({
    id: name,
    name,
    scope: instance,
    url: "ssh://git@git.example.com/acme/stacks.git",
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
  }) as GitRepo;

const savedKey = {
  type: "ssh-ed25519",
  fingerprint: SAVED_FINGERPRINT,
  trustedAt: "2026-10-01T10:00:00Z",
};
const changedTest = { ok: false, code: "host_key_changed", at: "2026-10-27T10:00:00Z" };

beforeEach(() => {
  me = makeMe({ instanceAdmin: true });
  repos = [];
  addressTests = [];
  addressResult = untrusted;
  savedTests = [];
  savedResult = changedFail;
  posted = [];
  putBodies = [];
  trusted = [];
  trustStatus = 200;
  closed = 0;
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
        addressTests.push((await req.json()) as Record<string, unknown>);
        return Response.json(addressResult);
      }
      if (/^\/api\/(env\/[^/]+\/)?git\/repos\/[^/]+\/test$/.test(url.pathname)) {
        savedTests.push(url.pathname);
        return Response.json(savedResult);
      }
      if (/\/trust-host-key$/.test(url.pathname) && req.method === "POST") {
        trusted.push({ path: url.pathname, body: (await req.json()) as Record<string, unknown> });
        return Response.json(repo("stacks", { hostKey: savedKey }), { status: trustStatus });
      }
      if (url.pathname === "/api/git/repos" && req.method === "POST") {
        const body = (await req.json()) as Record<string, unknown>;
        posted.push(body);
        return Response.json(repo(String(body.name)), { status: 201 });
      }
      if (url.pathname === "/api/git/repos") return Response.json({ items: repos });
      if (/^\/api\/(env\/[^/]+\/)?git\/repos\/[^/]+$/.test(url.pathname) && req.method === "PUT") {
        putBodies.push((await req.json()) as Record<string, unknown>);
        return Response.json(repo("stacks"));
      }
      if (url.pathname === "/api/git/credentials") return Response.json({ items: [] });
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

function Harness({ target }: { target?: GitRepo }) {
  const [open, setOpen] = useState(true);
  return open ? (
    <RepositorySheet
      mode={target ? "edit" : "add"}
      repo={target}
      onClose={() => {
        closed += 1;
        setOpen(false);
      }}
    />
  ) : null;
}

function mountSheet(target?: GitRepo) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <Harness target={target} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

function mountPage() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <GitPage search={{}} onSearchChange={() => {}} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const field = (name: string | RegExp) => screen.getByLabelText(name) as HTMLInputElement;
const type = (name: string | RegExp, value: string) =>
  fireEvent.change(field(name), { target: { value } });
const runTest = () => fireEvent.click(screen.getByRole("button", { name: "Test repository" }));
const save = () =>
  fireEvent.click(screen.getByRole("button", { name: /^Save (repository|changes)$/ }));

async function testNewSshRepository() {
  mountSheet();
  await screen.findByText("Add a repository");
  type("Name", "stacks");
  type("Repository address", "ssh://git@git.example.com/acme/stacks.git");
  runTest();
  return screen.findByText("Confirm this host's key");
}

describe("the host key confirm", () => {
  it("shows the key type and fingerprint of a new host, as a status that takes no focus", async () => {
    const title = await testNewSshRepository();
    const alert = title.closest('[data-slot="alert"]') as HTMLElement;
    expect(alert.getAttribute("role")).toBe("status");
    expect(
      within(alert).getByText(
        "git.example.com presented a ssh-ed25519 key. Check the fingerprint against the one your git host publishes before you trust it.",
      ),
    ).toBeTruthy();
    const print = within(alert).getByText(FINGERPRINT);
    expect(print.tagName).toBe("CODE");
    expect(print.className).toContain("font-mono");
    expect(print.className).toContain("break-all");
    expect(within(alert).getByRole("button", { name: "Copy fingerprint" })).toBeTruthy();
    expect(within(alert).getByRole("button", { name: "Trust this key" })).toBeTruthy();
    expect(within(alert).getByRole("button", { name: "Don't trust it" })).toBeTruthy();
    expect(
      within(alert).getByText(
        "Bayplan keeps this key with the repository and stops if it ever changes.",
      ),
    ).toBeTruthy();
    // It moves no focus: the Alert and its buttons are not the active element.
    expect(alert.contains(document.activeElement)).toBe(false);
    // No credential is asked of the host before the key is trusted: the test sent no key.
    expect(addressTests).toEqual([{ url: "ssh://git@git.example.com/acme/stacks.git" }]);
  });

  it("holds a trusted key in the form and saves it with the repository", async () => {
    await testNewSshRepository();
    fireEvent.click(screen.getByRole("button", { name: "Trust this key" }));
    expect(await screen.findByText("Key trusted. It is saved with the repository.")).toBeTruthy();
    expect(screen.queryByText("Confirm this host's key")).toBeNull();
    // Nothing has been written yet.
    expect(posted).toEqual([]);
    expect(trusted).toEqual([]);
    save();
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toEqual({
      name: "stacks",
      url: "ssh://git@git.example.com/acme/stacks.git",
      hostKey: KEY,
    });
    await waitFor(() => expect(closed).toBe(1));
  });

  it("sends the trusted key with the next Test so the sign-in can be checked", async () => {
    await testNewSshRepository();
    fireEvent.click(screen.getByRole("button", { name: "Trust this key" }));
    await screen.findByText("Key trusted. It is saved with the repository.");
    addressResult = trustedPass;
    runTest();
    await screen.findByText("Found 3 branches and 1 tags. Default branch: main.");
    expect(addressTests[1]).toEqual({
      url: "ssh://git@git.example.com/acme/stacks.git",
      hostKey: KEY,
    });
    expect(screen.getByText("Key trusted. It is saved with the repository.")).toBeTruthy();
  });

  it("drops the pending key and the prompt on Don't trust it", async () => {
    await testNewSshRepository();
    fireEvent.click(screen.getByRole("button", { name: "Don't trust it" }));
    await waitFor(() => expect(screen.queryByText("Confirm this host's key")).toBeNull());
    expect(screen.queryByText("Key trusted. It is saved with the repository.")).toBeNull();
    save();
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).not.toHaveProperty("hostKey");
  });

  it("forgets a trusted key when the address moves to another host", async () => {
    await testNewSshRepository();
    fireEvent.click(screen.getByRole("button", { name: "Trust this key" }));
    await screen.findByText("Key trusted. It is saved with the repository.");
    type("Repository address", "ssh://git@other.example.com/acme/stacks.git");
    save();
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).not.toHaveProperty("hostKey");
  });

  it("discards a trusted key that was never saved when the sheet is closed", async () => {
    await testNewSshRepository();
    fireEvent.click(screen.getByRole("button", { name: "Trust this key" }));
    await screen.findByText("Key trusted. It is saved with the repository.");
    cleanup();
    mountSheet();
    await screen.findByText("Add a repository");
    expect(screen.queryByText("Key trusted. It is saved with the repository.")).toBeNull();
    expect(posted).toEqual([]);
  });

  it("says a built-in host matches its built-in key, with nothing to confirm", async () => {
    addressResult = builtinPass;
    mountSheet();
    await screen.findByText("Add a repository");
    type("Repository address", "git@github.com:acme/stacks.git");
    runTest();
    expect(await screen.findByText("Matches the built-in key for github.com.")).toBeTruthy();
    expect(screen.queryByText("Confirm this host's key")).toBeNull();
  });

  it("shows a stored key as Trusted with the start of its fingerprint, the whole in a tooltip", async () => {
    mountSheet(repo("stacks", { hostKey: savedKey }));
    await screen.findByText("Edit stacks");
    const line = await screen.findByText(SAVED_FINGERPRINT.slice(0, 12));
    expect(line.closest("p")?.textContent).toContain("Trusted");
  });

  it("shows nothing about a host key for an https address", async () => {
    addressResult = {
      ...trustedPass,
      hostKey: noKey,
    };
    mountSheet();
    await screen.findByText("Add a repository");
    type("Repository address", "https://git.example.com/acme/stacks.git");
    runTest();
    await screen.findByText("Found 3 branches and 1 tags. Default branch: main.");
    expect(screen.queryByText("Confirm this host's key")).toBeNull();
    expect(screen.queryByText(/built-in key/)).toBeNull();
  });
});

describe("the Host key cell", () => {
  it("reads — for https, Built-in, Trusted, Not trusted yet and Changed", async () => {
    repos = [
      repo("a-https", { url: "https://git.example.com/acme/a.git" }),
      repo("b-builtin", { url: "git@github.com:acme/b.git" }),
      repo("c-trusted", { hostKey: savedKey }),
      repo("d-new"),
      repo("e-changed", { hostKey: savedKey, lastTest: changedTest }),
    ];
    mountPage();
    await screen.findByText("a-https");
    const cell = (name: string) => {
      const row = screen.getByText(name).closest("tr") as HTMLElement;
      return within(row).getAllByRole("cell")[4] as HTMLElement;
    };
    expect(cell("a-https").textContent).toBe("—");
    expect(cell("b-builtin").textContent).toBe("Built-in");
    expect(cell("c-trusted").textContent).toBe(`Trusted${SAVED_FINGERPRINT.slice(0, 12)}`);
    expect(cell("d-new").textContent).toBe("Not trusted yet");
    expect(cell("e-changed").textContent).toBe("Changed");
  });
});
