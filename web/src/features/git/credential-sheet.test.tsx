// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type { GitCredential } from "@/api/git";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { CredentialSheet } from "@/features/git/credential-sheet";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const TOKEN = "ghp_0123456789abcdefWXYZ";
const PEM_KEY = "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----";

let client: QueryClient;
let me: ReturnType<typeof makeMe>;
let requests: { method: string; path: string; body: Record<string, unknown> }[];
let writeStatus: number;
let writeBody: unknown;
let closed: number;
let saved: GitCredential[];

const instance = { kind: "instance", envId: null, envName: null };

const credential = (extra: Record<string, unknown> = {}): GitCredential =>
  ({
    id: "c1",
    name: "gitea-homelab",
    scope: instance,
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
  }) as GitCredential;

const sshCredential = (extra: Record<string, unknown> = {}) =>
  credential({
    type: "ssh",
    username: "git",
    last4: "",
    fingerprint: "SHA256:AbCdEfGh0123456789",
    publicKey: "ssh-ed25519 AAAAC3Nza deploy@bayplan",
    ...extra,
  });

beforeEach(() => {
  me = makeMe({ instanceAdmin: true });
  requests = [];
  writeStatus = 200;
  writeBody = credential();
  closed = 0;
  saved = [];
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/environments") {
        return Response.json({
          items: [
            { id: "e1", name: "Lab", envType: "standalone" },
            { id: "e2", name: "Prod", envType: "swarm" },
          ],
        });
      }
      if (req.method === "POST" || req.method === "PUT") {
        requests.push({
          method: req.method,
          path: url.pathname,
          body: (await req.json()) as Record<string, unknown>,
        });
        return Response.json(writeBody, { status: writeStatus });
      }
      return Response.json({ items: [] });
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

function Harness({
  mode,
  existing,
  scopes,
}: {
  mode: "add" | "edit" | "view";
  existing?: GitCredential;
  scopes?: string[];
}) {
  const [open, setOpen] = useState(true);
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        reopen
      </button>
      {open ? (
        <CredentialSheet
          mode={mode}
          credential={existing}
          scopes={scopes}
          onSaved={(c) => saved.push(c)}
          onClose={() => {
            closed += 1;
            setOpen(false);
          }}
        />
      ) : null}
    </>
  );
}

function mount(mode: "add" | "edit" | "view" = "add", existing?: GitCredential, scopes?: string[]) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <Harness mode={mode} existing={existing} scopes={scopes} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const field = (name: string | RegExp) => screen.getByLabelText(name) as HTMLInputElement;
const type = (name: string | RegExp, value: string) =>
  fireEvent.change(field(name), { target: { value } });
const choose = (name: string) => fireEvent.click(screen.getByRole("radio", { name }));
const save = () => fireEvent.click(screen.getByRole("button", { name: "Save credential" }));
const saveChanges = () => fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

describe("CredentialSheet add mode", () => {
  it("opens on HTTPS token with the type toggle, the fields in order and the helper", async () => {
    mount();
    expect(await screen.findByText("Add a git credential")).toBeTruthy();
    expect(
      screen.getByText("Stored encrypted. The secret can't be shown again after you save."),
    ).toBeTruthy();
    expect(screen.getAllByRole("radio").map((r) => r.textContent)).toEqual([
      "HTTPS token",
      "SSH deploy key",
    ]);
    for (const label of ["Name", "Scope", "Username", "Token"]) {
      expect(screen.getByLabelText(label)).toBeTruthy();
    }
    expect(
      screen.getByText(/Use a personal access token, or a deploy or project token/),
    ).toBeTruthy();
    await waitFor(() =>
      expect(screen.getByLabelText("Scope").textContent).toContain("All environments (instance)"),
    );
  });

  it("switches the type, clears the secret field and swaps the helper", async () => {
    mount();
    await screen.findByText("Add a git credential");
    type("Token", TOKEN);
    choose("SSH deploy key");
    const key = field("Private key");
    expect(key.value).toBe("");
    expect(key.getAttribute("autocomplete")).toBe("off");
    expect(screen.getByText(/Use a deploy key with read access/)).toBeTruthy();
    expect(screen.queryByLabelText("Username")).toBeNull();
    fireEvent.change(key, { target: { value: PEM_KEY } });
    choose("HTTPS token");
    expect(field("Token").value).toBe("");
    expect(screen.getByText(/Use a personal access token/)).toBeTruthy();
  });

  it("hides the token by default and the eye button shows only what was typed", async () => {
    mount();
    await screen.findByText("Add a git credential");
    const token = field("Token");
    expect(token.type).toBe("password");
    expect(token.getAttribute("autocomplete")).toBe("new-password");
    fireEvent.click(screen.getByRole("button", { name: "Show token" }));
    expect(field("Token").type).toBe("text");
    fireEvent.click(screen.getByRole("button", { name: "Hide token" }));
    expect(field("Token").type).toBe("password");
  });

  it("shows the field errors when nothing is filled in", async () => {
    mount();
    await screen.findByText("Add a git credential");
    save();
    expect(await screen.findByText("Enter a name.")).toBeTruthy();
    expect(screen.getByText("Enter a username.")).toBeTruthy();
    expect(screen.getByText("Enter a token.")).toBeTruthy();
    expect(requests).toEqual([]);
  });

  it("saves an HTTPS token, toasts, closes, and the token is gone when the sheet opens again", async () => {
    mount();
    await screen.findByText("Add a git credential");
    type("Name", "gitea-homelab");
    type("Username", "deploy");
    type("Token", TOKEN);
    save();
    await waitFor(() => expect(closed).toBe(1));
    expect(requests).toEqual([
      {
        method: "POST",
        path: "/api/git/credentials",
        body: { name: "gitea-homelab", type: "https", username: "deploy", secret: TOKEN },
      },
    ]);
    expect(await screen.findByText("Added credential gitea-homelab.")).toBeTruthy();
    expect(document.body.textContent).not.toContain(TOKEN);
    expect(saved.map((c) => c.id)).toEqual(["c1"]);
    fireEvent.click(screen.getByRole("button", { name: "reopen" }));
    expect(((await screen.findByLabelText("Token")) as HTMLInputElement).value).toBe("");
    expect(field("Name").value).toBe("");
  });

  it("keeps the typed token out of storage, the URL and the caches after save", async () => {
    mount();
    await screen.findByText("Add a git credential");
    type("Name", "gitea-homelab");
    type("Username", "deploy");
    type("Token", TOKEN);
    save();
    await waitFor(() => expect(closed).toBe(1));
    await waitFor(() => expect(client.getMutationCache().getAll()).toHaveLength(0));
    expect(window.location.href).not.toContain(TOKEN);
    expect(JSON.stringify(Object.entries(localStorage))).not.toContain(TOKEN);
    expect(JSON.stringify(Object.entries(sessionStorage))).not.toContain(TOKEN);
    expect(
      JSON.stringify(
        client
          .getQueryCache()
          .getAll()
          .map((q) => q.state.data),
      ),
    ).not.toContain(TOKEN);
  });

  it("keeps the typed token out of storage and the URL after Cancel, asking first", async () => {
    mount();
    await screen.findByText("Add a git credential");
    type("Name", "gitea-homelab");
    type("Token", TOKEN);
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(await screen.findByText("Discard changes?")).toBeTruthy();
    expect(screen.getByText("The credential hasn't been saved.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Discard changes" }));
    await waitFor(() => expect(closed).toBe(1));
    expect(requests).toEqual([]);
    expect(window.location.href).not.toContain(TOKEN);
    expect(JSON.stringify(Object.entries(localStorage))).not.toContain(TOKEN);
    expect(JSON.stringify(Object.entries(sessionStorage))).not.toContain(TOKEN);
    expect(document.body.textContent).not.toContain(TOKEN);
  });

  it("saves an environment credential through the environment route", async () => {
    me = makeMe({ envs: { e1: "admin" } });
    writeBody = credential({ scope: { kind: "environment", envId: "e1", envName: "Lab" } });
    mount();
    await screen.findByText("Add a git credential");
    await waitFor(() => expect(screen.getByLabelText("Scope").textContent).toContain("Lab"));
    type("Name", "lab-token");
    type("Username", "deploy");
    type("Token", TOKEN);
    save();
    await waitFor(() => expect(requests).toHaveLength(1));
    expect(requests[0]?.path).toBe("/api/env/e1/git/credentials");
  });

  it("offers only the scopes the repository sheet allows when it is opened over it", async () => {
    mount("add", undefined, ["e2", "instance"]);
    await screen.findByText("Add a git credential");
    await waitFor(() => expect(screen.getByLabelText("Scope").textContent).toContain("Prod"));
    fireEvent.keyDown(screen.getByLabelText("Scope"), { key: "Enter" });
    const options = (await screen.findAllByRole("option")).map((o) => o.textContent);
    expect(options).toEqual(["Prod", "All environments (instance)"]);
  });

  it("sends an SSH key with no username and shows its public key and fingerprint after saving", async () => {
    writeBody = sshCredential();
    mount();
    await screen.findByText("Add a git credential");
    choose("SSH deploy key");
    type("Name", "deploy-key");
    fireEvent.change(field("Private key"), { target: { value: PEM_KEY } });
    save();
    await waitFor(() => expect(requests).toHaveLength(1));
    expect(requests[0]?.body).toEqual({ name: "deploy-key", type: "ssh", secret: PEM_KEY });
    // The sheet stays open on the saved credential, so the public key can be copied.
    expect(await screen.findByText("ssh-ed25519 AAAAC3Nza deploy@bayplan")).toBeTruthy();
    expect(screen.getByText("SHA256:AbCdEfGh0123456789")).toBeTruthy();
    expect(document.body.textContent).not.toContain("BEGIN OPENSSH PRIVATE KEY");
    expect(saved).toHaveLength(1);
  });
});

describe("CredentialSheet server refusals", () => {
  async function refuse(code: string, location = "body.secret") {
    writeStatus = 422;
    writeBody = {
      title: "Unprocessable Entity",
      status: 422,
      type: "validation_failed",
      errors: [{ message: code, location }],
    };
    mount();
    await screen.findByText("Add a git credential");
    choose("SSH deploy key");
    type("Name", "deploy-key");
    fireEvent.change(field("Private key"), { target: { value: PEM_KEY } });
    save();
  }

  it("words key_passphrase", async () => {
    await refuse("key_passphrase");
    expect(
      await screen.findByText("That key needs a passphrase. Use a key without one."),
    ).toBeTruthy();
    expect(closed).toBe(0);
  });

  it("words key_invalid", async () => {
    await refuse("key_invalid");
    expect(await screen.findByText("That isn't a private key Bayplan can read.")).toBeTruthy();
    expect(field("Private key").getAttribute("aria-invalid")).toBe("true");
  });

  it("words a name that is taken as an alert", async () => {
    writeStatus = 409;
    writeBody = { title: "Conflict", status: 409, type: "name_taken" };
    mount();
    await screen.findByText("Add a git credential");
    type("Name", "gitea-homelab");
    type("Username", "deploy");
    type("Token", TOKEN);
    save();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "A credential named gitea-homelab already exists in this scope.",
    );
    expect(closed).toBe(0);
  });
});

describe("CredentialSheet edit mode", () => {
  it("keeps the saved token when the field is left empty, and says which one", async () => {
    mount("edit", credential());
    expect(await screen.findByText("Edit gitea-homelab")).toBeTruthy();
    const token = field("New token (optional)");
    expect(token.placeholder).toBe("Leave empty to keep the one ending WXYZ");
    expect(field("Name").value).toBe("gitea-homelab");
    expect(field("Username").value).toBe("deploy");
    fireEvent.change(field("Username"), { target: { value: "deploy2" } });
    saveChanges();
    await waitFor(() => expect(closed).toBe(1));
    expect(requests).toEqual([
      {
        method: "PUT",
        path: "/api/git/credentials/c1",
        body: { name: "gitea-homelab", type: "https", username: "deploy2" },
      },
    ]);
    expect(await screen.findByText("Saved gitea-homelab.")).toBeTruthy();
  });

  it("sends a replacement token only when one was typed", async () => {
    mount("edit", credential());
    await screen.findByText("Edit gitea-homelab");
    type("New token (optional)", TOKEN);
    saveChanges();
    await waitFor(() => expect(requests).toHaveLength(1));
    expect(requests[0]?.body).toMatchObject({ secret: TOKEN });
  });

  it("shows a saved SSH key as its public key with Copy and the fingerprint, and keeps it when empty", async () => {
    mount("edit", sshCredential());
    await screen.findByText("Edit gitea-homelab");
    expect(screen.getByText("Public key")).toBeTruthy();
    expect(screen.getByText("ssh-ed25519 AAAAC3Nza deploy@bayplan")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Copy the public key" })).toBeTruthy();
    expect(screen.getByText("SHA256:AbCdEfGh0123456789")).toBeTruthy();
    expect(screen.getByText("Add this to your repository's deploy keys.")).toBeTruthy();
    expect(field("New private key (optional)").placeholder).toBe(
      "Leave empty to keep the saved key",
    );
  });

  it("needs a new secret when the type is switched", async () => {
    mount("edit", credential());
    await screen.findByText("Edit gitea-homelab");
    choose("SSH deploy key");
    saveChanges();
    expect(await screen.findByText("Enter a private key.")).toBeTruthy();
    expect(requests).toEqual([]);
  });

  it("does not change the scope", async () => {
    mount("edit", credential());
    await screen.findByText("Edit gitea-homelab");
    expect(screen.getByLabelText("Scope").hasAttribute("disabled")).toBe(true);
    expect(
      screen.getByText("A credential's scope can't change. Create a new one instead."),
    ).toBeTruthy();
  });
});

describe("CredentialSheet view mode", () => {
  it("shows the details of a credential the caller may only see, with Edit unavailable and the reason", async () => {
    mount("view", credential({ readOnly: true }));
    const details = await screen.findByRole("table", { name: "Credential details" });
    expect(within(details).getByText("gitea-homelab")).toBeTruthy();
    expect(screen.getByText("You can see this credential but not change it.")).toBeTruthy();
    expect(screen.getByText("••••WXYZ")).toBeTruthy();
    const edit = screen.getByRole("button", { name: "Edit" });
    expect(edit.getAttribute("aria-disabled")).toBe("true");
    expect(screen.getAllByText("Needs an instance administrator").length).toBeGreaterThan(0);
    expect(document.body.textContent).not.toContain(TOKEN);
  });
});
