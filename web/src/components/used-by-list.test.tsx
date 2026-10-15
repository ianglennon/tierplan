// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { type ObjectUser, ObjectUsersTable, UsedByList } from "@/components/used-by-list";
import "@/i18n";
import { TestRouter } from "@/test-router";

afterEach(cleanup);

const user = (service: string, extra: Partial<ObjectUser> = {}): ObjectUser => ({
  service,
  serviceId: `svc-${service}`,
  stack: "",
  file: `/run/secrets/${service}`,
  uid: "0",
  gid: "0",
  mode: "0444",
  ...extra,
});

describe("UsedByList for a secret or a config", () => {
  it("says the file, owner and mode a service mounts the object with", () => {
    render(
      <UsedByList
        containers={[]}
        services={[{ name: "api", file: "db_password", uid: "0", gid: "0", mode: "0444" }]}
      />,
    );
    expect(screen.getByText(/Service api/)).toBeTruthy();
    expect(screen.getByText(/db_password/)).toBeTruthy();
    expect(screen.getByText(/0 \/ 0/)).toBeTruthy();
    expect(screen.getByText(/0444/)).toBeTruthy();
  });

  it("keeps the plain line for a service without file details", () => {
    render(<UsedByList containers={[]} services={[{ name: "api" }]} />);
    expect(screen.getByText("Service api")).toBeTruthy();
  });
});

describe("ObjectUsersTable", () => {
  function mount(users: ObjectUser[], kind: "secret" | "config" = "secret") {
    render(
      <TestRouter>
        <ObjectUsersTable envId="e1" users={users} kind={kind} />
      </TestRouter>,
    );
  }

  it("has the columns Service, Stack, File, UID / GID and Mode", () => {
    mount([user("api")]);
    const table = screen.getByRole("table", { name: "Used by" });
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Service", "Stack", "File", "UID / GID", "Mode"]);
  });

  it("links the service, shows the stack or a dash, and the owner as UID / GID", () => {
    mount([user("api", { stack: "shop", uid: "1000", gid: "100" }), user("worker")]);
    const link = screen.getByRole("link", { name: "api" });
    expect(link.getAttribute("href")).toBe("/env/e1/services/svc-api");
    const api = link.closest("tr") as HTMLElement;
    expect(within(api).getByText("shop")).toBeTruthy();
    expect(within(api).getByText("1000 / 100")).toBeTruthy();
    const worker = screen.getByRole("link", { name: "worker" }).closest("tr") as HTMLElement;
    expect(within(worker).getByText("—")).toBeTruthy();
  });

  it("keeps the order it is given", () => {
    mount([user("b"), user("a")]);
    const names = screen
      .getAllByRole("row")
      .slice(1)
      .map((r) => r.querySelector("td")?.textContent);
    expect(names).toEqual(["b", "a"]);
  });

  it("says nothing uses the secret or the config when there are no rows", () => {
    mount([], "secret");
    expect(screen.getByText("Nothing uses this secret.")).toBeTruthy();
    cleanup();
    mount([], "config");
    expect(screen.getByText("Nothing uses this config.")).toBeTruthy();
  });
});
