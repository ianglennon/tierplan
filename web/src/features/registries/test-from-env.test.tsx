// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import type {
  CredentialRequestError,
  CredentialTestFromEnvRequest,
  CredentialTestFromEnvResult,
} from "@/api/registries";
import { TooltipProvider } from "@/components/ui/tooltip";
import { TestFromEnv } from "@/features/registries/test-from-env";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// Test from an environment (plan 05-13, IMG-01, D-13): a node's engine reads the manifest
// with the credential, through its agent, and the answer names the node.

interface Sent {
  method: string;
  path: string;
  body?: Record<string, unknown>;
}

let sent: Sent[];
let answer: () => Response;
let recent: string[];
let me: ReturnType<typeof makeMe>;
let client: QueryClient;

const REPO = "localhost:5000/private/app";

const passed: CredentialTestFromEnvResult = {
  nodeId: "n1",
  node: "alpha",
  state: "passed",
  code: "",
  digest: "sha256:abc",
  platforms: ["linux/amd64"],
  repository: REPO,
  tag: "latest",
  ref: `${REPO}:latest`,
  testedAt: "2026-10-12T10:00:00Z",
};
const failed = (
  code: string,
  over: Partial<CredentialTestFromEnvResult> = {},
): CredentialTestFromEnvResult => ({
  ...passed,
  ...over,
  state: "failed",
  code,
  digest: "",
  platforms: [],
});

const base: CredentialTestFromEnvRequest = {
  type: "generic",
  host: "localhost:5000",
  username: "bot",
  secret: "hunter2-hunter2",
};

beforeEach(() => {
  sent = [];
  me = makeMe({ instanceAdmin: true });
  recent = [];
  answer = () => Response.json(passed);
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const entry: Sent = {
        method: req.method,
        path: url.pathname,
        body:
          req.method === "POST"
            ? await req
                .clone()
                .json()
                .catch(() => undefined)
            : undefined,
      };
      sent.push(entry);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/me/recent-environments")
        return Response.json({ environmentIds: recent });
      if (url.pathname === "/api/environments") {
        return Response.json({
          items: [
            { id: "e2", name: "Prod", envType: "swarm" },
            { id: "e1", name: "Lab", envType: "standalone" },
          ],
        });
      }
      if (req.method === "POST" && url.pathname.endsWith("/registries/test-from-env"))
        return answer();
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(
  over: Partial<{
    repository: string;
    needsSecret: boolean;
    onFieldErrors: (err: CredentialRequestError) => boolean;
    credentialEnvId: string;
    prepare: () => Promise<CredentialTestFromEnvRequest | null>;
  }> = {},
) {
  const prepare = over.prepare ?? vi.fn(async () => base);
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <TestFromEnv
            prepare={prepare}
            repository={over.repository ?? "private/app"}
            needsSecret={over.needsSecret ?? false}
            onFieldErrors={over.onFieldErrors}
            credentialEnvId={over.credentialEnvId}
          />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
  return prepare;
}

const trigger = () => screen.getByRole("button", { name: /Test from an environment/ });
const posts = () => sent.filter((s) => s.method === "POST");

async function open() {
  fireEvent.click(trigger());
  await waitFor(() => expect(screen.getByRole("combobox", { name: "Environment" })).toBeTruthy());
}
const runButton = () => screen.getByRole("button", { name: /Run test from/ });

describe("TestFromEnv", () => {
  it("is disabled with Enter a test repository while the field is empty", () => {
    mount({ repository: "  " });
    expect((trigger() as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText("Enter a test repository")).toBeTruthy();
    expect(trigger().getAttribute("aria-describedby")).toBeTruthy();
  });

  it("is disabled while a new credential has no secret", () => {
    mount({ needsSecret: true });
    expect((trigger() as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText("Enter a secret.")).toBeTruthy();
  });

  it("lists the environments the person may pull on, defaults to the first and says where it runs", async () => {
    mount();
    await open();
    await waitFor(() => expect(screen.getByRole("combobox").textContent).toContain("Lab"));
    expect(runButton().textContent).toBe("Run test from Lab");
    expect(
      screen.getByText(
        "Runs on a node of Lab through its agent, so it also catches firewall, DNS and CA problems that TierPlan's own server can't see.",
      ),
    ).toBeTruthy();
    expect(posts()).toHaveLength(0);
  });

  it("starts with the environment the person was last in", async () => {
    recent = ["e2"];
    mount();
    await open();
    await waitFor(() => expect(runButton().textContent).toBe("Run test from Prod"));
  });

  it("sends the values and the repository to the environment's node and names the node in the answer", async () => {
    const prepare = mount();
    await open();
    await waitFor(() => expect(runButton().textContent).toBe("Run test from Lab"));
    fireEvent.click(runButton());

    await waitFor(() =>
      expect(
        screen.getByText("alpha can pull localhost:5000/private/app:latest with this credential."),
      ).toBeTruthy(),
    );
    expect(prepare).toHaveBeenCalledOnce();
    expect(posts()).toEqual([
      {
        method: "POST",
        path: "/api/registries/test-from-env",
        body: { ...base, repository: "private/app", envId: "e1" },
      },
    ]);
    // Now the node is known, the caption names it.
    expect(
      screen.getByText(/^Runs on alpha through its agent, so it also catches firewall, DNS/),
    ).toBeTruthy();
  });

  it("hands a 422 that names a field to the sheet and shows nothing of its own", async () => {
    answer = () =>
      Response.json(
        {
          title: "Unprocessable",
          status: 422,
          type: "validation_failed",
          errors: [{ location: "body.secret", message: "secret_required", value: "" }],
        },
        { status: 422 },
      );
    const onFieldErrors = vi.fn((_err: CredentialRequestError) => true);
    mount({ onFieldErrors });
    await open();
    await waitFor(() => expect(runButton().textContent).toBe("Run test from Lab"));
    fireEvent.click(runButton());
    await waitFor(() => expect(onFieldErrors).toHaveBeenCalledOnce());
    const err = onFieldErrors.mock.calls[0]?.[0] as CredentialRequestError;
    expect(err.status).toBe(422);
    expect(err.fields.map((f) => [f.location, f.message])).toEqual([
      ["body.secret", "secret_required"],
    ]);
    expect(screen.queryByText("Couldn't run the test. Try again.")).toBeNull();
  });

  it("sends nothing while the sheet's values are not valid", async () => {
    mount({ prepare: async () => null });
    await open();
    await waitFor(() => expect(runButton().textContent).toBe("Run test from Lab"));
    fireEvent.click(runButton());
    await waitFor(() => expect(runButton().hasAttribute("disabled")).toBe(false));
    expect(posts()).toHaveLength(0);
  });

  it("tests a credential of an environment through that environment's route, from that environment only", async () => {
    mount({ credentialEnvId: "e2" });
    await open();
    await waitFor(() => expect(runButton().textContent).toBe("Run test from Prod"));
    // A single choice is not offered as a choice.
    expect(screen.getByRole("combobox").hasAttribute("disabled")).toBe(true);
    fireEvent.click(runButton());
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]).toEqual({
      method: "POST",
      path: "/api/env/e2/registries/test-from-env",
      body: { ...base, repository: "private/app" },
    });
  });

  it("only offers environments the person may pull on", async () => {
    me = makeMe({ envs: { e1: "operator" } });
    mount();
    await open();
    await waitFor(() => expect(runButton().textContent).toBe("Run test from Lab"));
    expect(screen.getByRole("combobox").hasAttribute("disabled")).toBe(true);
  });

  it("words a failure with the node, the ref it read and the mapped reason, never a code", async () => {
    const lead = "alpha couldn't read localhost:5000/private/app:latest with this credential: ";
    const cases: [string, string][] = [
      [
        "pull_denied",
        "localhost:5000 refused this credential for that repository. Check its scopes.",
      ],
      ["pull_unauthorized", "localhost:5000 refused this username and secret."],
      [
        "registry_unreachable",
        "alpha couldn't reach localhost:5000. Check its network, DNS and firewall.",
      ],
      [
        "registry_tls",
        "alpha doesn't trust the certificate from localhost:5000. Its Docker daemon needs the registry's CA (certs.d) or an insecure-registries entry. See the docs.",
      ],
      [
        "registry_http",
        "localhost:5000 doesn't offer HTTPS, which Docker on alpha requires. Its Docker daemon needs an insecure-registries entry. See the docs.",
      ],
      ["rate_limited", "localhost:5000's pull limit was reached. Try again later."],
      [
        "invalid_reference",
        "Docker on alpha doesn't accept localhost:5000/private/app:latest as an image reference.",
      ],
      ["cancelled", "Cancelled before it finished on alpha."],
      [
        "image_not_found",
        "localhost:5000 has no such repository, or no tag latest in it. Check both.",
      ],
      [
        "pull_failed",
        "Docker on alpha gave an answer TierPlan doesn't recognise. The server log has it.",
      ],
      [
        "registry_http_fallback",
        "Docker on alpha retried localhost:5000 over plain HTTP, because the name resolves to a loopback address or is listed in insecure-registries, and that answer hid the real one. Check that the tag latest exists and that the sign-in works.",
      ],
      ["node_unavailable", "alpha isn't reachable right now. Try again when it's back."],
      [
        "something_new",
        "Docker on alpha gave an answer TierPlan doesn't recognise. The server log has it.",
      ],
    ];
    for (const [code, reason] of cases) {
      cleanup();
      answer = () => Response.json(failed(code));
      mount();
      await open();
      await waitFor(() => expect(runButton().textContent).toBe("Run test from Lab"));
      fireEvent.click(runButton());
      await waitFor(() => expect(screen.getByText(lead + reason)).toBeTruthy());
      expect(document.body.textContent).not.toContain(code);
      expect(document.body.textContent).not.toContain("Check the node");
    }
  });

  it("names the tag it read for a registry that has no latest", async () => {
    const ref = "dev.homelab.org.uk:5443/team/busybox:latest";
    answer = () =>
      Response.json(
        failed("image_not_found", {
          repository: "dev.homelab.org.uk:5443/team/busybox",
          tag: "latest",
          ref,
        }),
      );
    mount();
    await open();
    await waitFor(() => expect(runButton().textContent).toBe("Run test from Lab"));
    fireEvent.click(runButton());
    await waitFor(() =>
      expect(
        screen.getByText(
          `alpha couldn't read ${ref} with this credential: dev.homelab.org.uk:5443 has no such repository, or no tag latest in it. Check both.`,
        ),
      ).toBeTruthy(),
    );
  });

  it("says which tag a passed check read", async () => {
    answer = () => Response.json({ ...passed, tag: "1.36.1", ref: `${REPO}:1.36.1` });
    mount();
    await open();
    await waitFor(() => expect(runButton().textContent).toBe("Run test from Lab"));
    fireEvent.click(runButton());
    await waitFor(() =>
      expect(
        screen.getByText("alpha can pull localhost:5000/private/app:1.36.1 with this credential."),
      ).toBeTruthy(),
    );
  });

  describe("Tag field", () => {
    const INVALID =
      "A tag is 1 to 128 letters, digits, underscores, dots or dashes, and doesn't start with a dot or dash.";
    const tagInput = () => screen.getByLabelText("Tag (optional)") as HTMLInputElement;

    it("sends the typed tag, and no tag while the field is empty", async () => {
      mount();
      await open();
      await waitFor(() => expect(runButton().textContent).toBe("Run test from Lab"));
      expect(tagInput().placeholder).toBe("latest");
      fireEvent.click(runButton());
      await waitFor(() => expect(posts()).toHaveLength(1));
      expect(posts()[0]?.body).toEqual({ ...base, repository: "private/app", envId: "e1" });

      fireEvent.change(tagInput(), { target: { value: "1.36.1" } });
      await waitFor(() => expect(runButton().hasAttribute("disabled")).toBe(false));
      fireEvent.click(runButton());
      await waitFor(() => expect(posts()).toHaveLength(2));
      expect(posts()[1]?.body).toEqual({
        ...base,
        repository: "private/app",
        tag: "1.36.1",
        envId: "e1",
      });
    });

    it("shows an invalid tag on the field and sends nothing", async () => {
      mount();
      await open();
      await waitFor(() => expect(runButton().textContent).toBe("Run test from Lab"));
      fireEvent.change(tagInput(), { target: { value: "bad tag!" } });
      fireEvent.click(runButton());
      await waitFor(() => expect(screen.getByText(INVALID)).toBeTruthy());
      expect(tagInput().getAttribute("aria-invalid")).toBe("true");
      expect(posts()).toHaveLength(0);
      // Typing again takes the error away.
      fireEvent.change(tagInput(), { target: { value: "ok" } });
      await waitFor(() => expect(screen.queryByText(INVALID)).toBeNull());
    });

    it("puts a 422 tag_invalid on the same field and does not hand it to the sheet", async () => {
      answer = () =>
        Response.json(
          {
            title: "Unprocessable",
            status: 422,
            type: "validation_failed",
            errors: [{ location: "body.tag", message: "tag_invalid", value: "x" }],
          },
          { status: 422 },
        );
      const onFieldErrors = vi.fn((_err: CredentialRequestError) => true);
      mount({ onFieldErrors });
      await open();
      await waitFor(() => expect(runButton().textContent).toBe("Run test from Lab"));
      fireEvent.change(tagInput(), { target: { value: "x" } });
      fireEvent.click(runButton());
      await waitFor(() => expect(screen.getByText(INVALID)).toBeTruthy());
      expect(onFieldErrors).not.toHaveBeenCalled();
      expect(screen.queryByText("Couldn't run the test. Try again.")).toBeNull();
    });

    it("clears the last result when the tag changes", async () => {
      mount();
      await open();
      await waitFor(() => expect(runButton().textContent).toBe("Run test from Lab"));
      fireEvent.click(runButton());
      await waitFor(() => expect(screen.getByText(/can pull/)).toBeTruthy());
      fireEvent.change(tagInput(), { target: { value: "2" } });
      await waitFor(() => expect(screen.queryByText(/can pull/)).toBeNull());
    });
  });

  it("shows the agent-update notice for an agent too old for the check", async () => {
    answer = () => Response.json(failed("agent_policy_denied"));
    mount();
    await open();
    await waitFor(() => expect(runButton().textContent).toBe("Run test from Lab"));
    fireEvent.click(runButton());
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("This host's agent needs an update")).toBeTruthy();
    expect(document.body.textContent).not.toContain("couldn't pull");
    expect(document.body.textContent).not.toContain("agent_policy_denied");
  });

  it("says why when the server answers an error", async () => {
    answer = () =>
      Response.json(
        { type: "rate_limited", title: "Too Many Requests", status: 429 },
        { status: 429, headers: { "Content-Type": "application/problem+json" } },
      );
    mount();
    await open();
    await waitFor(() => expect(runButton().textContent).toBe("Run test from Lab"));
    fireEvent.click(runButton());
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("Too many tests. Wait a minute and try again.");
  });

  it("shows no environment to run from when the person may pull on none", async () => {
    me = makeMe({ envs: { e1: "viewer" } });
    mount();
    fireEvent.click(trigger());
    await waitFor(() =>
      expect(screen.getByText("There is no environment to run the test from.")).toBeTruthy(),
    );
  });
});
