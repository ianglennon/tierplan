// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { BrowsePage } from "@/features/tags/browse-page";
import { TagBrowser } from "@/features/tags/tag-browser";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The tag browser (plan 05-18, IMG-03, D-19, D-24a): a repository's tag names, the credential
// they were read with, and a mapped message in place of the table when the registry says no.

interface Sent {
  path: string;
  query: Record<string, string>;
}

let sent: Sent[];
let answer: (req: Sent) => Response | Promise<Response>;
let me: ReturnType<typeof makeMe>;
let client: QueryClient;

const instanceCred = {
  id: "c1",
  name: "ghcr-acme",
  scope: { kind: "instance", envId: null, envName: null },
  host: "ghcr.io",
  prefix: "acme",
};

function list(over: Record<string, unknown> = {}) {
  return {
    repo: "acme/app",
    host: "ghcr.io",
    credential: instanceCred,
    tags: [{ name: "v1.10.0" }, { name: "v1.9.2" }, { name: "latest" }],
    hidden: 0,
    total: 3,
    more: false,
    page: 0,
    capped: false,
    ...over,
  };
}

function problem(status: number, type: string, errors: { location: string; message: string }[]) {
  return Response.json(
    { type, title: "x", status, detail: "x", errors },
    { status, headers: { "Content-Type": "application/problem+json" } },
  );
}

const where = [
  { location: "registry.host", message: "ghcr.io" },
  { location: "registry.repo", message: "acme/app" },
];

beforeEach(() => {
  sent = [];
  me = makeMe({ instanceAdmin: true, envs: { e1: "viewer" } });
  answer = () => Response.json(list());
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/registries/tags") {
        const entry: Sent = { path: url.pathname, query: Object.fromEntries(url.searchParams) };
        sent.push(entry);
        return answer(entry);
      }
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

function mount(node: React.ReactNode) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>{node}</TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const rows = () => screen.getAllByRole("row").slice(1);

describe("TagBrowser", () => {
  it("shows skeleton rows while the list loads, then the tag names in mono", async () => {
    let release: (r: Response) => void = () => {};
    answer = () => new Promise<Response>((resolve) => (release = resolve));
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);

    expect(screen.getAllByRole("row")).toHaveLength(8);
    expect(screen.queryByText("v1.10.0")).toBeNull();

    await waitFor(() => expect(sent).toHaveLength(1));
    release(Response.json(list()));
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(rows().map((r) => r.textContent)).toEqual(["v1.10.0", "v1.9.2", "latest"]);
    expect(screen.getByText("v1.10.0").className).toContain("font-mono");
    expect(screen.getByText("Showing 3 tags")).toBeTruthy();
    expect(sent).toEqual([{ path: "/api/registries/tags", query: { ref: "ghcr.io/acme/app" } }]);
  });

  it("names the credential used, linked to it", async () => {
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText(/Using credential/)).toBeTruthy());
    const line = screen.getByText(/Using credential/);
    expect(line.textContent).toBe("Using credential “ghcr-acme” (Instance)");
    expect(within(line).getByRole("link", { name: "ghcr-acme" }).getAttribute("href")).toBe(
      "/registries?credential=c1",
    );
  });

  it("says Anonymous when no credential was used, and an environment's name for its credential", async () => {
    answer = () => Response.json(list({ credential: null }));
    mount(<TagBrowser repo="nginx" variant="full" />);
    await waitFor(() => expect(screen.getByText("Anonymous")).toBeTruthy());
    cleanup();
    client.clear();

    answer = () =>
      Response.json(
        list({
          credential: {
            ...instanceCred,
            scope: { kind: "environment", envId: "e1", envName: "Lab" },
          },
        }),
      );
    mount(<TagBrowser repo="nginx" variant="full" envId="e1" />);
    await waitFor(() =>
      expect(screen.getByText(/Using credential/).textContent).toBe(
        "Using credential “ghcr-acme” (Lab)",
      ),
    );
    expect(sent.at(-1)?.query).toEqual({ ref: "nginx", envId: "e1" });
  });

  it("shows No tags with a way to try again when the repository has none", async () => {
    answer = () => Response.json(list({ tags: [], total: 0 }));
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("No tags")).toBeTruthy());
    expect(screen.getByText("acme/app has no tags on ghcr.io.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(sent).toHaveLength(2));
  });

  it("truncates a long tag name and keeps it whole in the DOM", async () => {
    const long = `v${"9".repeat(120)}`;
    answer = () => Response.json(list({ tags: [{ name: long }], total: 1 }));
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText(long)).toBeTruthy());
    expect(screen.getByText(long).className).toContain("truncate");
  });

  it("renders a tag name that looks like markup as text", async () => {
    answer = () =>
      Response.json(list({ tags: [{ name: "<img src=x onerror=alert(1)>" }], total: 1 }));
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("<img src=x onerror=alert(1)>")).toBeTruthy());
    expect(document.querySelector("img")).toBeNull();
  });

  it("selects a row in the picker with a click or Enter, and marks it selected", async () => {
    const onSelect = vi.fn();
    mount(
      <TagBrowser repo="ghcr.io/acme/app" variant="picker" onSelect={onSelect} selected="v1.9.2" />,
    );
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    const [first, second] = rows();
    expect(first?.getAttribute("aria-selected")).toBe("false");
    expect(second?.getAttribute("aria-selected")).toBe("true");
    fireEvent.click(first as HTMLElement);
    fireEvent.keyDown(rows()[2] as HTMLElement, { key: "Enter" });
    expect(onSelect.mock.calls).toEqual([["v1.10.0"], ["latest"]]);
  });
});

describe("TagBrowser errors", () => {
  it("login_refused names the credential and links to test it", async () => {
    answer = () =>
      problem(502, "login_refused", [
        ...where,
        { location: "credential.id", message: "c1" },
        { location: "credential.name", message: "ghcr-acme" },
        { location: "credential.scopeKind", message: "instance" },
        { location: "credential.scopeName", message: "" },
      ]);
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "Bayplan couldn't sign in to ghcr.io with credential “ghcr-acme” (Instance). Test it.",
    );
    expect(within(alert).getByRole("link", { name: "Test credential" }).getAttribute("href")).toBe(
      "/registries?credential=c1",
    );
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("needs_login with no credential links to add one, for someone who may", async () => {
    answer = () => problem(502, "needs_login", where);
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "ghcr.io needs a login to list tags, and no stored credential matches acme/app. Add one.",
    );
    expect(
      (await within(alert).findByRole("link", { name: "Add credential" })).getAttribute("href"),
    ).toBe("/registries?credential=new");
  });

  it("needs_login offers no Add credential link to someone who cannot add one", async () => {
    me = makeMe({ envs: { e1: "viewer" } });
    answer = () => problem(502, "needs_login", where);
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    const alert = await screen.findByRole("alert");
    await waitFor(() => expect(alert.textContent).toContain("needs a login"));
    expect(within(alert).queryByRole("link")).toBeNull();
  });

  it("repository_not_found names the repository and the registry, with nothing to retry", async () => {
    answer = () => problem(404, "repository_not_found", where);
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("No repository named acme/app on ghcr.io.");
    expect(within(alert).queryByRole("button")).toBeNull();
  });

  it.each([
    ["registry_unreachable", "Couldn't reach ghcr.io. Check the address and try again."],
    ["rate_limited", "ghcr.io is limiting requests. Wait a moment and try again."],
  ])("%s says so and Retry asks again", async (type, message) => {
    answer = () => problem(502, type, where);
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(message);
    answer = () => Response.json(list());
    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("never shows what the registry said", async () => {
    answer = () =>
      Response.json(
        {
          type: "registry_unreachable",
          title: "x",
          status: 502,
          detail: "SECRET-REGISTRY-TEXT",
          errors: where,
        },
        { status: 502, headers: { "Content-Type": "application/problem+json" } },
      );
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await screen.findByRole("alert");
    expect(document.body.textContent).not.toContain("SECRET-REGISTRY-TEXT");
  });

  it("falls back to a generic message when the server cannot be reached", async () => {
    answer = () => {
      throw new TypeError("network down");
    };
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Couldn't load the tags.");
    expect(within(alert).getByRole("button", { name: "Retry" })).toBeTruthy();
  });
});

describe("TagBrowser filter, hide switch and paging", () => {
  const hideSwitch = () =>
    screen.getByRole("switch", { name: "Hide signature, attestation and cache tags" });
  const filterBox = () => screen.getByRole("searchbox", { name: "Filter tags" });

  it("has a filter and a Hide switch that is on, with how many it hides", async () => {
    answer = () => Response.json(list({ hidden: 4 }));
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(filterBox().getAttribute("placeholder")).toBe("Filter by name");
    expect(hideSwitch().getAttribute("aria-checked")).toBe("true");
    expect(screen.getByText("4 hidden")).toBeTruthy();
  });

  it("shows no hidden count when nothing is hidden or the switch is off", async () => {
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(screen.queryByText(/hidden$/)).toBeNull();
    cleanup();
    client.clear();

    answer = () => Response.json(list({ hidden: 4 }));
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("4 hidden")).toBeTruthy());
    fireEvent.click(hideSwitch());
    await waitFor(() => expect(hideSwitch().getAttribute("aria-checked")).toBe("false"));
    expect(screen.queryByText("4 hidden")).toBeNull();
  });

  it("asks again for the filter text and for the switch turned off", async () => {
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(sent.at(-1)?.query).toEqual({ ref: "ghcr.io/acme/app" });

    fireEvent.change(filterBox(), { target: { value: "alpine" } });
    await waitFor(() =>
      expect(sent.at(-1)?.query).toEqual({ ref: "ghcr.io/acme/app", q: "alpine" }),
    );

    fireEvent.click(hideSwitch());
    await waitFor(() =>
      expect(sent.at(-1)?.query).toEqual({ ref: "ghcr.io/acme/app", q: "alpine", hide: "false" }),
    );
  });

  it("loads 100 more, and says That's every tag. at the end", async () => {
    answer = (req) =>
      req.query.page === "1"
        ? Response.json(list({ tags: [{ name: "v0.1.0" }], total: 4, more: false, page: 1 }))
        : Response.json(list({ total: 4, more: true, page: 0 }));
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(screen.queryByText("That's every tag.")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Load 100 more" }));
    await waitFor(() => expect(screen.getByText("v0.1.0")).toBeTruthy());
    expect(sent.at(-1)?.query).toEqual({ ref: "ghcr.io/acme/app", page: "1" });
    expect(screen.getByText("That's every tag.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Load 100 more" })).toBeNull();
    expect(screen.getByText("Showing 4 tags")).toBeTruthy();
    expect(rows().map((r) => r.textContent)).toEqual(["v1.10.0", "v1.9.2", "latest", "v0.1.0"]);
  });

  it("shows no match for the filter, with a button that clears it", async () => {
    answer = (req) =>
      req.query.q ? Response.json(list({ tags: [], total: 0 })) : Response.json(list());
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    fireEvent.change(filterBox(), { target: { value: "zzz" } });
    await waitFor(() => expect(screen.getByText("No tags match “zzz”.")).toBeTruthy());
    // The filter box has a clear button of the same name; this is the one in the empty state.
    const empty = screen
      .getByText("No tags match “zzz”.")
      .closest("[data-slot='empty']") as HTMLElement;
    fireEvent.click(within(empty).getByRole("button", { name: "Clear filter" }));
    // The unfiltered list was read a moment ago, so it comes back without asking again.
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(sent.map((s) => s.query.q)).toEqual([undefined, "zzz"]);
  });

  it("says only signature and attestation tags are left, with a button that shows them", async () => {
    answer = (req) =>
      req.query.hide === "false"
        ? Response.json(list({ tags: [{ name: "app.sig" }], total: 1, hidden: 1 }))
        : Response.json(list({ tags: [], total: 0, hidden: 3 }));
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() =>
      expect(screen.getByText("Only signature and attestation tags are left.")).toBeTruthy(),
    );
    fireEvent.click(screen.getByRole("button", { name: "Show them" }));
    await waitFor(() => expect(screen.getByText("app.sig")).toBeTruthy());
    expect(hideSwitch().getAttribute("aria-checked")).toBe("false");
  });

  it("keeps the filter and the switch in the address when the page owns them", async () => {
    const onSearchChange = vi.fn();
    mount(
      <BrowsePage
        search={{ ref: "ghcr.io/acme/app", q: "alp", hide: 0 }}
        onSearchChange={onSearchChange}
      />,
    );
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0]?.query).toEqual({ ref: "ghcr.io/acme/app", q: "alp", hide: "false" });
    await waitFor(() => expect(hideSwitch().getAttribute("aria-checked")).toBe("false"));
    expect((filterBox() as HTMLInputElement).value).toBe("alp");

    fireEvent.click(hideSwitch());
    expect(onSearchChange).toHaveBeenLastCalledWith({ hide: undefined });
    fireEvent.change(filterBox(), { target: { value: "alpine" } });
    await waitFor(() => expect(onSearchChange).toHaveBeenLastCalledWith({ q: "alpine" }));
  });
});

describe("BrowsePage", () => {
  it("shows the breadcrumb, the repository as the title and the tag browser", async () => {
    mount(<BrowsePage search={{ ref: "ghcr.io/acme/app" }} />);
    await screen.findByRole("navigation", { name: "Breadcrumb" });
    expect(screen.getByRole("navigation", { name: "Breadcrumb" }).textContent).toBe(
      "RegistriesBrowse",
    );
    expect(
      within(screen.getByRole("navigation", { name: "Breadcrumb" }))
        .getByRole("link", { name: "Registries" })
        .getAttribute("href"),
    ).toBe("/registries");
    expect(screen.getByRole("heading", { level: 1 }).textContent).toBe("ghcr.io/acme/app");
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
  });

  it("asks nothing and offers the way back when there is no repository in the address", async () => {
    mount(<BrowsePage search={{ ref: "" }} />);
    expect(await screen.findByText("No repository to browse")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Back to Registries" })).toBeTruthy();
    expect(sent).toHaveLength(0);
  });

  it("shows no permission to someone who reads no environment", async () => {
    me = makeMe({});
    mount(<BrowsePage search={{ ref: "ghcr.io/acme/app" }} />);
    expect(await screen.findByText("You don't have permission to view this page")).toBeTruthy();
    expect(sent).toHaveLength(0);
    expect(screen.queryByRole("heading", { level: 1, name: "ghcr.io/acme/app" })).toBeNull();
  });
});
