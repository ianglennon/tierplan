// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ExposePanel } from "@/features/stack-editor/expose/expose-panel";
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

function mount(over: { text?: string; target?: "swarm" | "standalone"; readOnly?: boolean } = {}) {
  const onEdit = vi.fn<(text: string) => void>();
  render(
    <QueryClientProvider client={client}>
      <ExposePanel
        id="expose"
        envId={ENV}
        text={over.text ?? FILE}
        target={over.target ?? "swarm"}
        readOnly={over.readOnly ?? false}
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
