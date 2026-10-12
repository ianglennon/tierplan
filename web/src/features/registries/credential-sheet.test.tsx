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
import { CredentialSheet } from "@/features/registries/credential-sheet";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const SECRET = "ghp_0123456789abcdefWXYZ";

let client: QueryClient;
let me: ReturnType<typeof makeMe>;
let posted: Record<string, unknown>[];
let patched: Record<string, unknown>[];
let patchStatus: number;
let patchBody: unknown;
let postStatus: number;
let postBody: unknown;
let stored: Record<string, unknown> | null;
let closed: number;

const credential = (extra: Record<string, unknown> = {}) => ({
  id: "c1",
  scope: { kind: "instance", envId: null, envName: null },
  name: "ghcr-acme",
  type: "ghcr",
  host: "ghcr.io",
  prefix: "acme",
  username: "deploy",
  last4: "WXYZ",
  hubFallback: false,
  expiresAt: null,
  lastTest: null,
  lastFailure: null,
  canManage: true,
  createdAt: "2026-10-01T10:00:00Z",
  updatedAt: "2026-10-01T10:00:00Z",
  ...extra,
});

beforeEach(() => {
  me = makeMe({ instanceAdmin: true });
  posted = [];
  patched = [];
  patchStatus = 200;
  patchBody = credential({ username: "deploy2", updatedAt: "2026-10-12T10:00:00Z" });
  postStatus = 201;
  postBody = credential();
  stored = credential();
  closed = 0;
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/registries" && req.method === "POST") {
        posted.push((await req.json()) as Record<string, unknown>);
        return Response.json(postBody, { status: postStatus });
      }
      if (url.pathname === "/api/registries") return Response.json({ items: [] });
      if (url.pathname === "/api/registries/c1" && req.method === "PATCH") {
        patched.push((await req.json()) as Record<string, unknown>);
        return Response.json(patchBody, { status: patchStatus });
      }
      if (url.pathname === "/api/registries/c1") {
        return stored
          ? Response.json(stored)
          : Response.json(
              { title: "Not found", status: 404, type: "credential_not_found" },
              { status: 404 },
            );
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

// Holds the open state the way the page does: the sheet is there while the search
// parameter is.
function Harness({ mode, credentialId }: { mode: "add" | "edit" | "view"; credentialId?: string }) {
  const [open, setOpen] = useState(true);
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        reopen
      </button>
      {open ? (
        <CredentialSheet
          mode={mode}
          credentialId={credentialId}
          onClose={() => {
            closed += 1;
            setOpen(false);
          }}
        />
      ) : null}
    </>
  );
}

function mount(mode: "add" | "edit" | "view" = "add", credentialId?: string) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <Harness mode={mode} credentialId={credentialId} />
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

describe("CredentialSheet add mode", () => {
  it("opens with the type toggle, the fields in order and the explanation of the secret", async () => {
    mount();
    expect(await screen.findByText("Add a registry credential")).toBeTruthy();
    expect(
      screen.getByText("Stored encrypted. The secret can't be shown again after you save."),
    ).toBeTruthy();
    const labels = [
      "Name",
      "Scope",
      "Registry",
      "Namespace (optional)",
      "Username",
      "Secret",
      "Expires (optional)",
    ];
    for (const label of labels) expect(screen.getByLabelText(label)).toBeTruthy();
    expect(screen.getAllByRole("radio").map((r) => r.textContent)).toEqual([
      "Docker Hub",
      "GHCR",
      "GitLab",
      "Generic v2",
    ]);
    expect(screen.getByLabelText("Scope").textContent).toContain("All environments (instance)");
  });

  it("fills a read-only host and says which token to use for Docker Hub", async () => {
    mount();
    await screen.findByText("Add a registry credential");
    expect(field("Registry").value).toBe("docker.io");
    expect(field("Registry").readOnly).toBe(true);
    expect(screen.getByText(/Use a Docker Hub access token, not your password/)).toBeTruthy();
    expect(screen.getByRole("checkbox", { name: "Use for all Docker Hub pulls" })).toBeTruthy();
  });

  it("switches the host and the helper with the type, and only Docker Hub has the Hub checkbox", async () => {
    mount();
    await screen.findByText("Add a registry credential");
    choose("GHCR");
    expect(field("Registry").value).toBe("ghcr.io");
    expect(field("Registry").readOnly).toBe(true);
    expect(
      screen.getByText(/personal access token \(classic\) with the read:packages scope/),
    ).toBeTruthy();
    expect(screen.queryByRole("checkbox", { name: "Use for all Docker Hub pulls" })).toBeNull();
    choose("GitLab");
    expect(field("Registry").value).toBe("registry.gitlab.com");
    expect(field("Registry").readOnly).toBe(false);
    expect(screen.getByText(/deploy token with read_registry/)).toBeTruthy();
    choose("Generic v2");
    expect(field("Registry").value).toBe("");
    expect(screen.getByText(/Amazon ECR tokens expire after 12 hours/)).toBeTruthy();
  });

  it("keeps a host that was typed when the type changes, and keeps the other fields", async () => {
    mount();
    await screen.findByText("Add a registry credential");
    choose("Generic v2");
    type("Registry", "reg.example.com:5000");
    type("Name", "mine");
    choose("GitLab");
    expect(field("Registry").value).toBe("reg.example.com:5000");
    expect(field("Name").value).toBe("mine");
  });

  it("shows the field errors of the UI-SPEC when nothing is filled in", async () => {
    mount();
    await screen.findByText("Add a registry credential");
    choose("Generic v2");
    save();
    expect(await screen.findByText("Enter a name.")).toBeTruthy();
    expect(screen.getByText("Enter the registry address.")).toBeTruthy();
    expect(screen.getByText("Enter a username.")).toBeTruthy();
    expect(screen.getByText("Enter a secret.")).toBeTruthy();
    expect(posted).toEqual([]);
  });

  it("names the format of a host, a namespace and a date that do not fit", async () => {
    mount();
    await screen.findByText("Add a registry credential");
    choose("Generic v2");
    type("Name", "x");
    type("Username", "u");
    type("Secret", SECRET);
    type("Registry", "https://reg.example.com/");
    type("Namespace (optional)", "/Acme/");
    type("Expires (optional)", "2020-01-01");
    save();
    expect(
      await screen.findByText("Use a host like ghcr.io or registry.example.com:5000."),
    ).toBeTruthy();
    expect(screen.getByText("Use a path like acme or acme/team.")).toBeTruthy();
    expect(screen.getByText("Pick a date in the future.")).toBeTruthy();
    expect(posted).toEqual([]);
  });

  it("hides the secret by default and the show button toggles it with aria-pressed", async () => {
    mount();
    await screen.findByText("Add a registry credential");
    const secret = field("Secret");
    expect(secret.type).toBe("password");
    expect(secret.getAttribute("autocomplete")).toBe("new-password");
    expect(secret.getAttribute("spellcheck")).toBe("false");
    const show = screen.getByRole("button", { name: "Show secret" });
    expect(show.getAttribute("aria-pressed")).toBe("false");
    fireEvent.click(show);
    expect(field("Secret").type).toBe("text");
    const hide = screen.getByRole("button", { name: "Hide secret" });
    expect(hide.getAttribute("aria-pressed")).toBe("true");
    fireEvent.click(hide);
    expect(field("Secret").type).toBe("password");
  });

  it("saves the credential, toasts, closes, and the secret is gone when the sheet is opened again", async () => {
    mount();
    await screen.findByText("Add a registry credential");
    choose("GHCR");
    type("Name", "ghcr-acme");
    type("Namespace (optional)", "acme");
    type("Username", "deploy");
    type("Secret", SECRET);
    save();
    await waitFor(() => expect(closed).toBe(1));
    expect(posted).toEqual([
      { name: "ghcr-acme", type: "ghcr", prefix: "acme", username: "deploy", secret: SECRET },
    ]);
    expect(await screen.findByText("Added credential ghcr-acme.")).toBeTruthy();
    // The toast does not carry the secret, and neither does the page.
    expect(document.body.textContent).not.toContain(SECRET);
    fireEvent.click(screen.getByRole("button", { name: "reopen" }));
    expect(((await screen.findByLabelText("Secret")) as HTMLInputElement).value).toBe("");
    expect(field("Name").value).toBe("");
  });

  it("keeps the secret out of the query and mutation caches", async () => {
    mount();
    await screen.findByText("Add a registry credential");
    choose("GHCR");
    type("Name", "ghcr-acme");
    type("Username", "deploy");
    type("Secret", SECRET);
    save();
    await waitFor(() => expect(closed).toBe(1));
    await waitFor(() => expect(client.getMutationCache().getAll()).toHaveLength(0));
    expect(
      JSON.stringify(
        client
          .getQueryCache()
          .getAll()
          .map((q) => q.state.data),
      ),
    ).not.toContain(SECRET);
    expect(window.location.href).not.toContain(SECRET);
    expect(JSON.stringify(Object.entries(localStorage))).not.toContain(SECRET);
  });

  it("sends the fallback flag, the host of a generic registry and the expiry", async () => {
    mount();
    await screen.findByText("Add a registry credential");
    fireEvent.click(screen.getByRole("checkbox", { name: "Use for all Docker Hub pulls" }));
    type("Name", "hub");
    type("Username", "me");
    type("Secret", SECRET);
    save();
    await waitFor(() => expect(posted).toHaveLength(1));
    expect(posted[0]).toMatchObject({ type: "dockerhub", hubFallback: true });
    expect(posted[0]).not.toHaveProperty("host");
  });

  it("shows hub_prefix_required from the server as an error on the Namespace field and stays open", async () => {
    postStatus = 422;
    postBody = {
      title: "Unprocessable Entity",
      status: 422,
      type: "validation_failed",
      errors: [{ message: "hub_prefix_required", location: "body.prefix", value: "" }],
    };
    mount();
    await screen.findByText("Add a registry credential");
    type("Name", "hub");
    type("Username", "me");
    type("Secret", SECRET);
    save();
    expect(await screen.findByText(/Docker Hub needs a namespace like acme, or tick/)).toBeTruthy();
    expect(closed).toBe(0);
    expect(field("Namespace (optional)").getAttribute("aria-invalid")).toBe("true");
  });

  it("shows a name that is taken inline, as an alert", async () => {
    postStatus = 409;
    postBody = { title: "Conflict", status: 409, type: "credential_name_taken" };
    mount();
    await screen.findByText("Add a registry credential");
    choose("GHCR");
    type("Name", "ghcr-acme");
    type("Username", "me");
    type("Secret", SECRET);
    save();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "A credential named ghcr-acme already exists in all environments.",
    );
    expect(closed).toBe(0);
  });

  it("clears the secret on Cancel, after asking whether to discard what was typed", async () => {
    mount();
    await screen.findByText("Add a registry credential");
    type("Secret", SECRET);
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(await screen.findByText("Discard changes?")).toBeTruthy();
    expect(screen.getByText("The credential hasn't been saved.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Discard changes" }));
    await waitFor(() => expect(closed).toBe(1));
    fireEvent.click(screen.getByRole("button", { name: "reopen" }));
    expect(((await screen.findByLabelText("Secret")) as HTMLInputElement).value).toBe("");
  });

  it("closes at once when nothing was typed", async () => {
    mount();
    await screen.findByText("Add a registry credential");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(closed).toBe(1));
  });

  it("goes through the same question when Escape is pressed", async () => {
    mount();
    await screen.findByText("Add a registry credential");
    type("Name", "typed");
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(await screen.findByText("Discard changes?")).toBeTruthy();
    expect(closed).toBe(0);
    fireEvent.click(screen.getByRole("button", { name: "Keep editing" }));
    expect(field("Name").value).toBe("typed");
  });
});

describe("CredentialSheet view mode", () => {
  it("shows the credential as a definition list with the secret only as its last characters", async () => {
    mount("view", "c1");
    const table = await screen.findByRole("table", { name: "Credential details" });
    expect(within(table).getByText("ghcr-acme")).toBeTruthy();
    expect(within(table).getByText("ghcr.io/acme")).toBeTruthy();
    expect(within(table).getByText("deploy")).toBeTruthy();
    expect(within(table).getByText("••••WXYZ")).toBeTruthy();
    expect(within(table).getByText("Instance")).toBeTruthy();
    expect(within(table).getByText("GHCR")).toBeTruthy();
    // No secret field and no reveal control anywhere.
    expect(document.querySelector('input[type="password"]')).toBeNull();
    expect(screen.queryByLabelText("Secret")).toBeNull();
    expect(screen.queryByRole("button", { name: /show|reveal/i })).toBeNull();
  });

  it("shows the mask alone when the server kept no last characters", async () => {
    stored = credential({ last4: "" });
    mount("view", "c1");
    const table = await screen.findByRole("table", { name: "Credential details" });
    expect(within(table).getByText("••••")).toBeTruthy();
  });

  it("has Close as its only footer button, and says the credential can't be changed", async () => {
    stored = credential({ canManage: false });
    mount("view", "c1");
    await screen.findByRole("table", { name: "Credential details" });
    expect(screen.getByText("You can see this credential but not change it.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Save credential" })).toBeNull();
    fireEvent.click(screen.getAllByRole("button", { name: "Close" }).at(-1) as HTMLElement);
    await waitFor(() => expect(closed).toBe(1));
  });

  it("shows skeleton fields while it loads", () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => new Promise(() => {})),
    );
    mount("view", "c1");
    expect(document.querySelectorAll('[data-slot="skeleton"]').length).toBeGreaterThan(0);
    expect(screen.queryByRole("table", { name: "Credential details" })).toBeNull();
  });

  it("says so when the credential does not exist or cannot be seen", async () => {
    stored = null;
    mount("view", "c1");
    expect(
      await screen.findByText("This credential doesn't exist, or you can't see it."),
    ).toBeTruthy();
  });

  it("draws a name that holds markup as text", async () => {
    stored = credential({ name: "<img src=x onerror=alert(1)>" });
    mount("view", "c1");
    await screen.findAllByText("<img src=x onerror=alert(1)>");
    expect(document.querySelector("img")).toBeNull();
  });
});

describe("CredentialSheet edit mode", () => {
  const saveChanges = () => fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

  it("opens as Edit {name} with the scope fixed and the secret optional, never shown", async () => {
    mount("edit", "c1");
    expect(await screen.findByText("Edit ghcr-acme")).toBeTruthy();
    expect(
      screen.getByText("Stored encrypted. The secret can't be shown again after you save."),
    ).toBeTruthy();
    expect(field("Name").value).toBe("ghcr-acme");
    expect(field("Username").value).toBe("deploy");
    expect(field("Namespace (optional)").value).toBe("acme");
    // The scope is read-only, and says why.
    expect(screen.getByRole("combobox", { name: "Scope" }).hasAttribute("disabled")).toBe(true);
    expect(
      screen.getByText("A credential's scope can't change. Create a new one instead."),
    ).toBeTruthy();
    // The secret field is empty, optional, and tells which one is kept.
    const secret = field("New secret (optional)");
    expect(secret.value).toBe("");
    expect(secret.placeholder).toBe("Leave empty to keep the one ending WXYZ");
    expect(document.body.textContent).not.toContain("ghp_");
  });

  it("says the stored secret is kept when no last characters were stored", async () => {
    stored = credential({ last4: "" });
    mount("edit", "c1");
    await screen.findByText("Edit ghcr-acme");
    expect(field("New secret (optional)").placeholder).toBe(
      "Leave empty to keep the stored secret",
    );
  });

  it("shows skeleton fields and a disabled Save while the credential loads", () => {
    stored = null;
    mount("edit", "c1");
    expect(screen.getByRole("button", { name: "Save changes" }).hasAttribute("disabled")).toBe(
      true,
    );
    expect(document.querySelectorAll('[data-slot="skeleton"]').length).toBeGreaterThan(0);
  });

  it("saves a changed username without sending the secret, toasts and closes", async () => {
    mount("edit", "c1");
    await screen.findByText("Edit ghcr-acme");
    type("Username", "deploy2");
    saveChanges();
    await waitFor(() => expect(closed).toBe(1));
    expect(patched).toEqual([
      {
        name: "ghcr-acme",
        type: "ghcr",
        prefix: "acme",
        username: "deploy2",
        hubFallback: false,
        expires: "",
      },
    ]);
    expect(patched[0]).not.toHaveProperty("secret");
    expect(await screen.findByText("Saved ghcr-acme.")).toBeTruthy();
  });

  it("sends a new secret once, and it is gone from the form, the caches and the page afterwards", async () => {
    mount("edit", "c1");
    await screen.findByText("Edit ghcr-acme");
    type("New secret (optional)", SECRET);
    saveChanges();
    await waitFor(() => expect(closed).toBe(1));
    expect(patched[0]).toMatchObject({ secret: SECRET });
    await waitFor(() => expect(client.getMutationCache().getAll()).toHaveLength(0));
    expect(
      JSON.stringify(
        client
          .getQueryCache()
          .getAll()
          .map((q) => q.state.data),
      ),
    ).not.toContain(SECRET);
    expect(document.body.textContent).not.toContain(SECRET);
    fireEvent.click(screen.getByRole("button", { name: "reopen" }));
    expect(
      ((await screen.findByLabelText("New secret (optional)")) as HTMLInputElement).value,
    ).toBe("");
  });

  it("does not ask for a secret", async () => {
    mount("edit", "c1");
    await screen.findByText("Edit ghcr-acme");
    saveChanges();
    await waitFor(() => expect(patched).toHaveLength(1));
    expect(screen.queryByText("Enter a secret.")).toBeNull();
  });

  it("asks before discarding a change, from Cancel and from Escape", async () => {
    mount("edit", "c1");
    await screen.findByText("Edit ghcr-acme");
    type("Username", "changed");
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(await screen.findByText("Discard changes?")).toBeTruthy();
    expect(closed).toBe(0);
    fireEvent.click(screen.getByRole("button", { name: "Keep editing" }));
    expect(field("Username").value).toBe("changed");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(await screen.findByRole("button", { name: "Discard changes" }));
    await waitFor(() => expect(closed).toBe(1));
    expect(patched).toHaveLength(0);
  });

  it("closes at once when nothing changed", async () => {
    mount("edit", "c1");
    await screen.findByText("Edit ghcr-acme");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(closed).toBe(1));
  });
});

describe("CredentialSheet conflicts", () => {
  const taken = (type: string, name?: string) => {
    patchStatus = 409;
    patchBody = {
      title: "Conflict",
      status: 409,
      type,
      ...(name ? { errors: [{ location: "conflict.credential", message: name }] } : {}),
    };
  };
  const saveChanges = () => fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

  it("shows a name another credential has as an alert in the edit sheet and saves nothing", async () => {
    taken("credential_name_taken", "other");
    mount("edit", "c1");
    await screen.findByText("Edit ghcr-acme");
    type("Name", "other");
    saveChanges();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "A credential named other already exists in all environments.",
    );
    expect(closed).toBe(0);
    expect(field("Name").value).toBe("other");
  });

  it("names the host and prefix another credential already covers", async () => {
    taken("host_prefix_taken", "ghcr-acme-2");
    mount("edit", "c1");
    await screen.findByText("Edit ghcr-acme");
    type("Namespace (optional)", "team");
    saveChanges();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "All environments already has a credential for ghcr.io/team. Edit that one instead.",
    );
    expect(closed).toBe(0);
  });

  it("names the credential that already uses all Docker Hub pulls, from the server's answer", async () => {
    stored = credential({ type: "dockerhub", host: "docker.io", prefix: "acme", name: "hub-ns" });
    taken("hub_fallback_taken", "hub-all");
    mount("edit", "c1");
    await screen.findByText("Edit hub-ns");
    fireEvent.click(screen.getByRole("checkbox", { name: "Use for all Docker Hub pulls" }));
    saveChanges();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "“hub-all” already uses all Docker Hub pulls for all environments. Untick it there first.",
    );
    expect(closed).toBe(0);
  });

  it("names the credential in the add sheet too", async () => {
    postStatus = 409;
    postBody = {
      title: "Conflict",
      status: 409,
      type: "hub_fallback_taken",
      errors: [{ location: "conflict.credential", message: "hub-all" }],
    };
    mount();
    await screen.findByText("Add a registry credential");
    fireEvent.click(screen.getByRole("checkbox", { name: "Use for all Docker Hub pulls" }));
    type("Name", "hub-two");
    type("Username", "me");
    type("Secret", SECRET);
    save();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("“hub-all” already uses all Docker Hub pulls");
    expect(closed).toBe(0);
  });

  it("shows a lost permission as an alert in the edit sheet", async () => {
    patchStatus = 403;
    patchBody = { title: "Forbidden", status: 403 };
    mount("edit", "c1");
    await screen.findByText("Edit ghcr-acme");
    type("Username", "x");
    saveChanges();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("You no longer have permission to do that.");
    expect(closed).toBe(0);
  });
});
