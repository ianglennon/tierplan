// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ExposePanel } from "@/features/stack-editor/expose/expose-panel";
import { makeMe } from "@/test-me";
import "@/i18n";
import { settled } from "@/test-settle";

// The Expose panel (plan 27-24, D-18, U-12, D-20): it reads the routes of one service out of the
// compose text and hands every whole edit back through onEdit. The only request it may make is the
// read of the environment's registered domains.

const ENV = "e1";
const FILE =
  'services:\n  db:\n    image: postgres:17\n  api:\n    image: nginx:1.27-alpine\n    deploy:\n      labels:\n        tierplan.ingress.host: api.apps.example.com\n        tierplan.ingress.port: "8080"\n';

let client: QueryClient;
let fetched: string[];
// What the domains read answers: a list of suffixes, or a status to fail with.
let domains: string[] | number;

beforeEach(() => {
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  fetched = [];
  domains = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      fetched.push(`${req.method} ${path}`);
      if (path === `/api/env/${ENV}/ingress/domains`) {
        if (typeof domains === "number") {
          return Response.json({ type: "x", title: "x", status: domains }, { status: domains });
        }
        return Response.json({ items: domains.map((suffix) => ({ id: suffix, suffix })) });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(
  over: {
    text?: string;
    target?: "swarm" | "standalone";
    readOnly?: boolean;
    gitSource?: boolean;
  } = {},
) {
  const onEdit = vi.fn<(text: string) => void>();
  render(
    <QueryClientProvider client={client}>
      <ExposePanel
        id="expose"
        envId={ENV}
        text={over.text ?? FILE}
        target={over.target ?? "swarm"}
        readOnly={over.readOnly ?? false}
        gitSource={over.gitSource}
        onEdit={onEdit}
        onClose={() => {}}
      />
    </QueryClientProvider>,
  );
  return onEdit;
}

const host = () => screen.getByRole("textbox", { name: "Hostname" }) as HTMLInputElement;
const port = () => screen.getByRole("spinbutton", { name: "Port" }) as HTMLInputElement;
const path = () => screen.getByRole("textbox", { name: "Path" }) as HTMLInputElement;

describe("the service and where the labels go", () => {
  it("lists every service and starts on the first one that has a route", async () => {
    mount();
    const select = screen.getByRole("combobox", { name: "Service" });
    expect(select.textContent).toContain("api");
    fireEvent.keyDown(select, { key: "Enter" });
    const names = (await screen.findAllByRole("option")).map((o) => o.textContent);
    expect(names).toEqual(["db", "api"]);
  });

  it("starts on the first service when none has a route", () => {
    mount({ text: "services:\n  db:\n    image: postgres:17\n  api:\n    image: nginx\n" });
    expect(screen.getByRole("combobox", { name: "Service" }).textContent).toContain("db");
  });

  it("names the block for a swarm stack and for a host stack", () => {
    mount();
    const line = (text: string) =>
      screen.getByText((_, el) => el?.tagName === "P" && el.textContent === text);
    expect(line("Routes are written to deploy.labels for this swarm stack.")).toBeTruthy();
    cleanup();
    mount({ target: "standalone" });
    expect(line("Routes are written to labels for this host stack.")).toBeTruthy();
  });
});

describe("committing a field", () => {
  it("writes the hostname on blur or Enter, never on a keystroke", () => {
    const onEdit = mount();
    fireEvent.change(host(), { target: { value: "shop.apps.example.com" } });
    expect(onEdit).not.toHaveBeenCalled();
    fireEvent.keyDown(host(), { key: "Enter" });
    expect(onEdit).toHaveBeenCalledTimes(1);
    expect(onEdit.mock.calls[0]?.[0]).toContain("tierplan.ingress.host: shop.apps.example.com");
  });

  it("writes the port on blur and the path with its slash", () => {
    const onEdit = mount();
    fireEvent.change(port(), { target: { value: "9090" } });
    fireEvent.blur(port());
    expect(onEdit.mock.calls[0]?.[0]).toContain('tierplan.ingress.port: "9090"');
    onEdit.mockClear();
    fireEvent.change(path(), { target: { value: "/shop" } });
    fireEvent.blur(path());
    expect(onEdit.mock.calls[0]?.[0]).toContain("tierplan.ingress.path: /shop");
  });

  it("writes the scheme and the strip switch at once", () => {
    const onEdit = mount();
    fireEvent.click(screen.getByRole("radio", { name: "https" }));
    expect(onEdit).toHaveBeenCalledTimes(1);
    expect(onEdit.mock.calls[0]?.[0]).toContain("tierplan.ingress.scheme: https");
    onEdit.mockClear();
    fireEvent.click(
      screen.getByRole("switch", { name: "Remove the path prefix before passing the request on" }),
    );
    expect(onEdit).toHaveBeenCalledTimes(1);
    expect(onEdit.mock.calls[0]?.[0]).toContain('tierplan.ingress.strip: "true"');
  });

  it("does not write an empty hostname and says it is required", () => {
    const onEdit = mount();
    fireEvent.change(host(), { target: { value: "" } });
    fireEvent.blur(host());
    expect(onEdit).not.toHaveBeenCalled();
    expect(screen.getByText("Hostname is required.")).toBeTruthy();
    expect(host().getAttribute("aria-invalid")).toBe("true");
  });

  it("does not write an empty port and says it is required", () => {
    const onEdit = mount();
    fireEvent.change(port(), { target: { value: "" } });
    fireEvent.blur(port());
    expect(onEdit).not.toHaveBeenCalled();
    expect(screen.getByText("Port is required.")).toBeTruthy();
  });

  it("does not write a port outside 1 to 65535 or a fraction", () => {
    const onEdit = mount();
    for (const bad of ["70000", "0", "80.5"]) {
      fireEvent.change(port(), { target: { value: bad } });
      fireEvent.blur(port());
      expect(screen.getByText("Use a port from 1 to 65535.")).toBeTruthy();
    }
    expect(onEdit).not.toHaveBeenCalled();
    fireEvent.change(port(), { target: { value: "8081" } });
    fireEvent.blur(port());
    expect(screen.queryByText("Use a port from 1 to 65535.")).toBeNull();
    expect(onEdit).toHaveBeenCalledTimes(1);
  });

  it("changes nothing when a field loses focus unchanged", () => {
    const onEdit = mount();
    fireEvent.focus(host());
    fireEvent.blur(host());
    expect(onEdit).not.toHaveBeenCalled();
  });

  it("labels every field and gives each helper text", () => {
    mount();
    for (const name of ["Hostname", "Path"]) {
      const el = screen.getByRole("textbox", { name });
      const helper = document.getElementById(el.getAttribute("aria-describedby") ?? "");
      expect(helper?.textContent?.length).toBeGreaterThan(0);
    }
    expect(
      screen.getByText("Requests below this path go to the service. Empty means the whole host."),
    ).toBeTruthy();
    expect(screen.getByText("How the proxy talks to the service.")).toBeTruthy();
    expect(screen.getByRole("group", { name: "Route default" })).toBeTruthy();
  });

  it("shows the routes and changes nothing when read-only", () => {
    const onEdit = mount({ readOnly: true });
    expect(host().readOnly).toBe(true);
    expect(host().getAttribute("aria-readonly")).toBe("true");
    fireEvent.click(screen.getByRole("radio", { name: "https" }));
    expect(onEdit).not.toHaveBeenCalled();
  });
});

describe("what the panel asks of the server", () => {
  it("makes no request other than the read of the registered domains", async () => {
    mount();
    fireEvent.change(host(), { target: { value: "shop.apps.example.com" } });
    fireEvent.blur(host());
    fireEvent.click(screen.getByRole("radio", { name: "https" }));
    await settled(client);
    for (const request of fetched) expect(request).toBe(`GET /api/env/${ENV}/ingress/domains`);
  });
});

// A compose file with one route at the given hostname.
const withHost = (name: string) =>
  `services:\n  api:\n    image: nginx\n    deploy:\n      labels:\n        tierplan.ingress.host: ${name}\n        tierplan.ingress.port: "8080"\n`;

const OUTSIDE =
  "This name isn't under a domain registered for this environment. The check will refuse it.";

describe("registered-domain suggestions (D-20)", () => {
  it("offers a chip per registered domain for a name under none, and a chip appends its suffix", async () => {
    domains = ["apps.example.com", "dev.example.com"];
    const onEdit = mount({ text: withHost("shop") });
    const first = await screen.findByRole("button", { name: "Add .apps.example.com" });
    expect(screen.getByRole("button", { name: "Add .dev.example.com" })).toBeTruthy();
    fireEvent.click(first);
    expect(onEdit).toHaveBeenCalledTimes(1);
    expect(onEdit.mock.calls[0]?.[0]).toContain("tierplan.ingress.host: shop.apps.example.com");
  });

  it("warns, without the colour of an error, when the name is outside every domain", async () => {
    domains = ["apps.example.com"];
    mount({ text: withHost("shop.other.org") });
    const warning = await screen.findByText(OUTSIDE);
    expect(warning.className).not.toContain("text-destructive");
    expect(warning.getAttribute("role")).toBeNull();
  });

  it("shows neither a chip nor a warning for a name under a registered domain", async () => {
    domains = ["apps.example.com"];
    mount({ text: withHost("shop.apps.example.com") });
    await settled(client);
    expect(screen.queryByText(OUTSIDE)).toBeNull();
    expect(screen.queryByRole("button", { name: /^Add \./ })).toBeNull();
  });

  it("matches a suffix on a dot boundary, and not the suffix alone", async () => {
    domains = ["apps.example.com"];
    mount({ text: withHost("notapps.example.com") });
    expect(await screen.findByText(OUTSIDE)).toBeTruthy();
    cleanup();
    mount({ text: withHost("apps.example.com") });
    expect(await screen.findByText(OUTSIDE)).toBeTruthy();
  });

  it("offers at most three chips", async () => {
    domains = ["a.example.com", "b.example.com", "c.example.com", "d.example.com"];
    mount({ text: withHost("shop") });
    await screen.findByRole("button", { name: "Add .a.example.com" });
    expect(screen.getAllByRole("button", { name: /^Add \./ })).toHaveLength(3);
  });

  it("reacts to what is typed before it is written", async () => {
    domains = ["apps.example.com"];
    mount({ text: withHost("shop") });
    await screen.findByText(OUTSIDE);
    fireEvent.change(host(), { target: { value: "shop.apps.example.com" } });
    expect(screen.queryByText(OUTSIDE)).toBeNull();
    expect(screen.queryByRole("button", { name: /^Add \./ })).toBeNull();
  });

  it("offers no chip to a panel that cannot edit", async () => {
    domains = ["apps.example.com"];
    mount({ text: withHost("shop"), readOnly: true });
    await screen.findByText(OUTSIDE);
    expect(screen.queryByRole("button", { name: /^Add \./ })).toBeNull();
  });

  it("leaves the hint out when the read is refused, and the field still commits", async () => {
    domains = 403;
    const onEdit = mount({ text: withHost("shop") });
    await settled(client);
    expect(screen.queryByText(OUTSIDE)).toBeNull();
    expect(screen.queryByRole("button", { name: /^Add \./ })).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
    fireEvent.change(host(), { target: { value: "shop.anywhere.org" } });
    fireEvent.blur(host());
    expect(onEdit).toHaveBeenCalledTimes(1);
  });

  it("leaves the hint out while the read is pending", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => new Promise<Response>(() => {})),
    );
    const onEdit = mount({ text: withHost("shop") });
    expect(screen.queryByText(OUTSIDE)).toBeNull();
    expect(screen.queryByRole("button", { name: /^Add \./ })).toBeNull();
    fireEvent.change(host(), { target: { value: "shop.anywhere.org" } });
    fireEvent.blur(host());
    expect(onEdit).toHaveBeenCalledTimes(1);
  });
});

// Labels in the block the target does not read, or in both (plan 27-26, D-18, U-14).
const WRONG_LIST =
  "services:\n  web:\n    image: nginx\n    labels:\n      - tierplan.ingress.host=shop.apps.example.com\n      - tierplan.ingress.port=8080\n";
const WRONG_MAP =
  'services:\n  web:\n    image: nginx\n    labels:\n      tierplan.ingress.host: shop.apps.example.com\n      tierplan.ingress.port: "8080"\n';
const BOTH =
  'services:\n  web:\n    image: nginx\n    labels:\n      tierplan.ingress.host: old.apps.example.com\n      tierplan.ingress.port: "80"\n    deploy:\n      labels:\n        tierplan.ingress.host: shop.apps.example.com\n        tierplan.ingress.port: "8080"\n';

describe("labels in the wrong place (D-18)", () => {
  it("warns and moves them to deploy.labels in one click, keeping the list form", () => {
    const onEdit = mount({ text: WRONG_LIST });
    expect(screen.getByText("These labels are in the wrong place")).toBeTruthy();
    expect(
      screen.getByText(
        (_, el) =>
          el?.tagName === "SPAN" &&
          el.textContent ===
            "web has ingress labels in labels, but a swarm stack reads deploy.labels. TierPlan ignores them.",
      ),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Move to deploy.labels" }));
    expect(onEdit).toHaveBeenCalledTimes(1);
    const next = onEdit.mock.calls[0]?.[0] ?? "";
    expect(next).toContain(
      "deploy:\n      labels:\n        - tierplan.ingress.host=shop.apps.example.com",
    );
    expect(next).not.toMatch(/\n {4}labels:/);
  });

  it("clears the warning once the working copy holds the move", () => {
    const onEdit = mount({ text: WRONG_MAP });
    fireEvent.click(screen.getByRole("button", { name: "Move to deploy.labels" }));
    cleanup();
    mount({ text: onEdit.mock.calls[0]?.[0] ?? "" });
    expect(screen.queryByText("These labels are in the wrong place")).toBeNull();
    expect(host().value).toBe("shop.apps.example.com");
  });

  it("moves a host stack's labels from deploy.labels to labels", () => {
    const onEdit = mount({
      target: "standalone",
      text: 'services:\n  web:\n    image: nginx\n    deploy:\n      labels:\n        tierplan.ingress.host: a.example.com\n        tierplan.ingress.port: "80"\n',
    });
    fireEvent.click(screen.getByRole("button", { name: "Move to labels" }));
    const next = onEdit.mock.calls[0]?.[0] ?? "";
    expect(next).toContain("    labels:\n      tierplan.ingress.host: a.example.com");
    expect(next).not.toContain("deploy");
  });

  it("offers no confirm: the move is made at once", () => {
    const onEdit = mount({ text: WRONG_MAP });
    fireEvent.click(screen.getByRole("button", { name: "Move to deploy.labels" }));
    expect(onEdit).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});

describe("two copies of the ingress labels (D-18)", () => {
  it("warns and removes the copy the target ignores", () => {
    const onEdit = mount({ text: BOTH });
    expect(screen.getByText("Two copies of the ingress labels")).toBeTruthy();
    expect(screen.queryByText("These labels are in the wrong place")).toBeNull();
    // The route shown is the one the target reads.
    expect(host().value).toBe("shop.apps.example.com");
    fireEvent.click(screen.getByRole("button", { name: "Remove the ignored copy" }));
    expect(onEdit).toHaveBeenCalledTimes(1);
    const next = onEdit.mock.calls[0]?.[0] ?? "";
    expect(next).not.toContain("old.apps.example.com");
    expect(next).toContain("shop.apps.example.com");
  });
});

// Read-only modes, locked values and a YAML error (plan 27-26, D-20, A-09, Pitfall 9).
const NOT_EDITABLE = [
  "Add a route",
  /^Remove route/,
  /^Move to/,
  "Remove the ignored copy",
  /^Add \./,
] as const;

describe("a panel that cannot edit (D-20)", () => {
  it("says the labels live in Git and offers no change", async () => {
    domains = ["apps.example.com"];
    const onEdit = mount({ readOnly: true, gitSource: true });
    expect(
      screen.getByText("This stack comes from a Git repository. Change the labels there."),
    ).toBeTruthy();
    expect(host().readOnly).toBe(true);
    expect(host().getAttribute("aria-readonly")).toBe("true");
    expect(port().readOnly).toBe(true);
    await settled(client);
    for (const name of NOT_EDITABLE) expect(screen.queryByRole("button", { name })).toBeNull();
    fireEvent.click(screen.getByRole("radio", { name: "https" }));
    fireEvent.click(screen.getByRole("switch", { name: /Remove the path prefix/ }));
    expect(onEdit).not.toHaveBeenCalled();
  });

  it("says what a person without stacks.update needs, from the server's own list", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        const path = new URL(req.url).pathname;
        if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "viewer" } }));
        return Response.json({ items: [] });
      }),
    );
    mount({ readOnly: true });
    expect(await screen.findByText("Read-only. Needs Operator.")).toBeTruthy();
    for (const name of NOT_EDITABLE) expect(screen.queryByRole("button", { name })).toBeNull();
  });

  it("still says it is read-only when the reason is unknown", () => {
    mount({ readOnly: true });
    expect(screen.getByText("Read-only.")).toBeTruthy();
  });

  it("draws no notice for a panel that can edit", () => {
    mount();
    expect(screen.queryByText(/Read-only/)).toBeNull();
    expect(screen.queryByText(/Git repository/)).toBeNull();
  });
});

describe("a value the panel cannot rewrite (Pitfall 9)", () => {
  // Built apart so the linter does not read a compose variable as a broken template string.
  const REF = "$" + "{APP_HOST}";
  const VARIABLE = `services:\n  api:\n    image: nginx\n    deploy:\n      labels:\n        tierplan.ingress.host: "${REF}"\n        tierplan.ingress.port: "8080"\n`;
  const ANCHOR =
    'x-labels: &ingress\n  tierplan.ingress.host: shop.apps.example.com\nservices:\n  api:\n    image: nginx\n    deploy:\n      labels:\n        <<: *ingress\n        tierplan.ingress.port: "8080"\n';
  const NOTE = "Set by a variable or anchor. Change it in the file.";

  it("shows a variable's host read-only with its sentence, and edits the others around it", () => {
    const onEdit = mount({ text: VARIABLE });
    expect(host().value).toBe(REF);
    expect(host().readOnly).toBe(true);
    expect(host().getAttribute("aria-readonly")).toBe("true");
    expect(screen.getAllByText(NOTE)).toHaveLength(1);
    expect(port().readOnly).toBe(false);
    fireEvent.change(host(), { target: { value: "other.example.com" } });
    fireEvent.blur(host());
    expect(onEdit).not.toHaveBeenCalled();
    fireEvent.change(port(), { target: { value: "9090" } });
    fireEvent.blur(port());
    const next = onEdit.mock.calls[0]?.[0] ?? "";
    expect(next).toContain('tierplan.ingress.port: "9090"');
    expect(next).toContain(`"${REF}"`);
  });

  it("shows a value that comes from an anchor read-only", () => {
    const onEdit = mount({ text: ANCHOR });
    expect(host().value).toBe("shop.apps.example.com");
    expect(host().readOnly).toBe(true);
    expect(screen.getAllByText(NOTE).length).toBeGreaterThan(0);
    fireEvent.change(host(), { target: { value: "x.example.com" } });
    fireEvent.blur(host());
    expect(onEdit).not.toHaveBeenCalled();
  });
});

describe("a compose file that is not YAML", () => {
  it("shows only the neutral YAML error", () => {
    mount({ text: "services:\n  api: [unclosed\n" });
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toBe(
      "The compose file has a YAML error, so the panel can't read it. Fix it in the editor first.",
    );
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.queryByRole("combobox")).toBeNull();
    expect(screen.queryByRole("button", { name: "Add a route" })).toBeNull();
    expect(alert.className).not.toContain("text-destructive");
  });
});
