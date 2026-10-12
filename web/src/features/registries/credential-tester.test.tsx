// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { CredentialTester } from "@/features/registries/credential-tester";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The "Which credential?" card (plan 05-08, IMG-02, D-10): it asks the server which stored
// credential a pull of an image would use, and shows the answer and the reasoning.

interface Sent {
  method: string;
  path: string;
  body?: { ref?: string };
}

let sent: Sent[];
let answer: (req: Sent) => Response | Promise<Response>;
let environments: { id: string; name: string }[];
let me: ReturnType<typeof makeMe>;
let client: QueryClient;

const scope = (kind: "instance" | "environment", envName: string | null = null) => ({
  kind,
  envId: kind === "environment" ? "e1" : null,
  envName,
});

const matched = {
  input: "ghcr.io/acme/team/app:1",
  normalized: "ghcr.io/acme/team/app:1",
  credential: {
    id: "c2",
    name: "ghcr-team",
    scope: scope("environment", "Lab"),
    host: "ghcr.io",
    prefix: "acme/team",
  },
  matchedOn: "prefix",
  considered: [
    {
      credentialId: "c2",
      name: "ghcr-team",
      scope: scope("environment", "Lab"),
      matches: true,
      result: "used",
    },
    {
      credentialId: "c1",
      name: "ghcr-acme",
      scope: scope("instance"),
      matches: true,
      result: "shorter_prefix",
    },
  ],
};

const anonymous = {
  input: "nginx",
  normalized: "docker.io/library/nginx:latest",
  credential: null,
  matchedOn: "",
  considered: [],
};

beforeEach(() => {
  sent = [];
  me = makeMe({ instanceAdmin: true, envs: { e1: "viewer" } });
  environments = [
    { id: "e1", name: "Lab" },
    { id: "e2", name: "Prod" },
  ];
  answer = () => Response.json(matched);
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
      if (url.pathname === "/api/environments") {
        return Response.json({
          items: environments.map((e) => ({ ...e, envType: "standalone" })),
        });
      }
      if (url.pathname === "/api/env/e1/registries/resolve" && req.method === "POST")
        return answer(entry);
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

function mount() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <CredentialTester />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const imageField = () => screen.getByLabelText("Image") as HTMLInputElement;
const check = () => screen.getByRole("button", { name: "Check credential" });
// The text of a row of the matched list: "Registry", "Matched on" or "Scope". The candidates
// table has a Scope column of its own, so the rows are looked up inside the list only.
const rowValue = (label: string) => {
  const list = screen.getByRole("status").querySelector("dl") as HTMLElement;
  return within(list).getByText(label).nextElementSibling?.textContent;
};
const resolves = () => sent.filter((s) => s.path.endsWith("/registries/resolve"));

async function ready() {
  mount();
  // The environment select shows the environment the user is in once the list is read.
  await waitFor(() => expect(screen.getByRole("combobox", { name: "Environment" })).toBeTruthy());
  await waitFor(() => expect(screen.getByRole("combobox").textContent).toContain("Lab"));
}

describe("CredentialTester", () => {
  it("opens empty: the title, two fields, an outline button and no result", async () => {
    await ready();
    expect(screen.getByText("Which credential?")).toBeTruthy();
    expect(
      screen.getByText("Check which credential a pull would use for an image, and why."),
    ).toBeTruthy();
    expect(imageField().placeholder).toBe("ghcr.io/acme/app:latest");
    expect(imageField().className).toContain("font-mono");
    expect(check().getAttribute("data-variant")).toBe("outline");
    expect(screen.getByRole("status").textContent).toBe("");
    expect(resolves()).toHaveLength(0);
  });

  it("names the credential a pull would use, why, and the credentials it passed over", async () => {
    await ready();
    fireEvent.change(imageField(), { target: { value: "ghcr.io/acme/team/app:1" } });
    fireEvent.click(check());

    const status = screen.getByRole("status");
    await waitFor(() => expect(status.textContent).toContain("Pulls of"));
    expect(resolves()).toEqual([
      {
        method: "POST",
        path: "/api/env/e1/registries/resolve",
        body: { ref: "ghcr.io/acme/team/app:1" },
      },
    ]);
    expect(status.textContent).toContain("Pulls of ghcr.io/acme/team/app:1 use “ghcr-team” (Lab).");
    // The three rows.
    const rows = within(status);
    expect(rowValue("Registry")).toBe("ghcr.io");
    expect(rowValue("Matched on")).toBe("the prefix acme/team");
    expect(rowValue("Scope")).toBe("Lab");
    // The input was already normalised, so there is no "Read as" line.
    expect(status.textContent).not.toContain("Read as");

    // More than one candidate: the table, with the used one first.
    const table = rows.getByRole("table", { name: "Other credentials considered" });
    const body = within(table)
      .getAllByRole("row")
      .slice(1)
      .map((r) =>
        within(r)
          .getAllByRole("cell")
          .map((c) => c.textContent),
      );
    expect(body).toEqual([
      ["ghcr-team", "Lab", "Yes", "Used"],
      ["ghcr-acme", "Instance", "Yes", "Shorter prefix"],
    ]);
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Credential", "Scope", "Matches", "Result"]);
  });

  it("says a pull is anonymous, shows how the reference was read, and links to Add credential", async () => {
    answer = () => Response.json(anonymous);
    await ready();
    fireEvent.change(imageField(), { target: { value: "nginx" } });
    fireEvent.click(check());

    const status = screen.getByRole("status");
    await waitFor(() => expect(status.textContent).toContain("are anonymous"));
    expect(status.textContent).toContain("Pulls of docker.io/library/nginx:latest are anonymous.");
    expect(status.textContent).toContain("No stored credential matches docker.io/library/nginx.");
    expect(status.textContent).toContain("Read as docker.io/library/nginx:latest");
    const link = within(status).getByRole("link", { name: "Add credential" });
    expect(link.getAttribute("href")).toContain("/registries");
    expect(link.getAttribute("href")).toContain("credential=new");
    // One or no candidates: no table.
    expect(within(status).queryByRole("table")).toBeNull();
  });

  it("offers no Add credential link to someone who cannot add one", async () => {
    me = makeMe({ envs: { e1: "viewer" } });
    answer = () => Response.json(anonymous);
    await ready();
    fireEvent.change(imageField(), { target: { value: "nginx" } });
    fireEvent.click(check());
    await waitFor(() => expect(screen.getByRole("status").textContent).toContain("are anonymous"));
    expect(screen.queryByRole("link", { name: "Add credential" })).toBeNull();
  });

  it("names the Hub fallback and the registry address as the reason", async () => {
    answer = () =>
      Response.json({
        ...anonymous,
        credential: {
          id: "h",
          name: "hub-all",
          scope: scope("instance"),
          host: "docker.io",
          prefix: "",
        },
        matchedOn: "hub_fallback",
        considered: [],
      });
    await ready();
    fireEvent.change(imageField(), { target: { value: "nginx" } });
    fireEvent.click(check());
    const status = screen.getByRole("status");
    await waitFor(() => expect(status.textContent).toContain("use “hub-all” (Instance)"));
    expect(rowValue("Matched on")).toBe("your Docker Hub fallback");

    answer = () =>
      Response.json({
        ...matched,
        credential: { ...matched.credential, prefix: "" },
        matchedOn: "host",
        considered: [],
      });
    fireEvent.change(imageField(), { target: { value: "ghcr.io/x/y" } });
    fireEvent.click(check());
    await waitFor(() => expect(rowValue("Matched on")).toBe("the registry address ghcr.io"));
  });

  it("shows a spinner and Checking… while it asks, and clears the previous result", async () => {
    await ready();
    fireEvent.change(imageField(), { target: { value: "ghcr.io/acme/team/app:1" } });
    fireEvent.click(check());
    await waitFor(() => expect(screen.getByRole("status").textContent).toContain("Pulls of"));

    let release: (r: Response) => void = () => {};
    answer = () => new Promise<Response>((resolve) => (release = resolve));
    fireEvent.click(check());
    const busy = await screen.findByRole("button", { name: "Checking…" });
    expect(busy.hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("status").textContent).toBe("");
    release(Response.json(matched));
    await waitFor(() => expect(screen.getByRole("status").textContent).toContain("Pulls of"));
  });

  it("clears the result when the image changes", async () => {
    await ready();
    fireEvent.change(imageField(), { target: { value: "ghcr.io/acme/team/app:1" } });
    fireEvent.click(check());
    await waitFor(() => expect(screen.getByRole("status").textContent).toContain("Pulls of"));
    fireEvent.change(imageField(), { target: { value: "ghcr.io/acme/team/app:2" } });
    expect(screen.getByRole("status").textContent).toBe("");
  });

  it("asks for an image before it asks the server", async () => {
    await ready();
    fireEvent.click(check());
    expect(
      await screen.findByText("Enter an image like nginx:1.27 or ghcr.io/acme/app:latest."),
    ).toBeTruthy();
    expect(imageField().getAttribute("aria-invalid")).toBe("true");
    expect(resolves()).toHaveLength(0);
  });

  it("shows the reference error when the server does not understand the image", async () => {
    answer = () =>
      Response.json({ type: "ref_invalid", title: "x", status: 422, detail: "d" }, { status: 422 });
    await ready();
    fireEvent.change(imageField(), { target: { value: "Bad Ref!" } });
    fireEvent.click(check());
    expect(
      await screen.findByText("Enter an image like nginx:1.27 or ghcr.io/acme/app:latest."),
    ).toBeTruthy();
    expect(screen.getByRole("status").textContent).toBe("");
  });

  it("shows the generic refusal inline when the server fails", async () => {
    answer = () => Response.json({ title: "boom", status: 500 }, { status: 500 });
    await ready();
    fireEvent.change(imageField(), { target: { value: "nginx" } });
    fireEvent.click(check());
    expect(await screen.findByText("Couldn't check the credential. Try again.")).toBeTruthy();
  });

  it("says Choose an environment when none can be read", async () => {
    environments = [];
    mount();
    await waitFor(() => expect(sent.some((s) => s.path === "/api/environments")).toBe(true));
    fireEvent.change(imageField(), { target: { value: "nginx" } });
    fireEvent.click(check());
    expect(await screen.findByText("Choose an environment.")).toBeTruthy();
    expect(resolves()).toHaveLength(0);
  });

  it("wraps a long reference in mono instead of widening the card", async () => {
    const long = `ghcr.io/${"a".repeat(120)}/app:1`;
    answer = () => Response.json({ ...anonymous, input: long, normalized: long });
    await ready();
    fireEvent.change(imageField(), { target: { value: long } });
    fireEvent.click(check());
    const status = screen.getByRole("status");
    await waitFor(() => expect(status.textContent).toContain("are anonymous"));
    const ref = within(status).getAllByText(long)[0] as HTMLElement;
    expect(ref.className).toContain("break-all");
    expect(ref.className).toContain("font-mono");
  });

  it("renders what a credential is called as text", async () => {
    answer = () =>
      Response.json({
        ...matched,
        credential: {
          id: "c",
          name: "<img src=x onerror=alert(1)>",
          scope: scope("instance"),
          host: "ghcr.io",
          prefix: "",
        },
      });
    await ready();
    fireEvent.change(imageField(), { target: { value: "ghcr.io/acme/team/app:1" } });
    fireEvent.click(check());
    await waitFor(() => expect(screen.getByRole("status").textContent).toContain("<img src=x"));
    expect(document.querySelector("img")).toBeNull();
  });
});
