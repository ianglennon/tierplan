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

function Harness() {
  const [open, setOpen] = useState(true);
  return open ? (
    <RepositorySheet
      mode="add"
      onClose={() => {
        closed += 1;
        setOpen(false);
      }}
    />
  ) : null;
}

function mount() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <Harness />
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
