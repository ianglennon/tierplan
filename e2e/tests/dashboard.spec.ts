// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Golden path 3 (D-80): the dashboard tiles equal what the dind daemon reports,
// and a network created behind Bayplan's back shows up in the events feed and
// in the Networks tile without a page reload (ENV-07).

import type { Page } from "@playwright/test";

import { AUTH_STATE } from "./env";
import { compose, dind, dindCount, docker, expect, test } from "./helpers";

test.use({ storageState: AUTH_STATE });

// The tile for one count. Its three spans are the label, the value and, for
// containers, the sub-line. While the count loads the value is a skeleton, so
// the span is absent and the assertion keeps polling.
function tile(page: Page, label: string) {
  return page
    .getByRole("region", { name: "Dashboard" })
    .getByRole("link", { name: new RegExp(`^${label}\\b`) });
}

function tileValue(page: Page, label: string) {
  return tile(page, label).locator("span").nth(1);
}

// "1 container", "3 containers": the list header's plural-aware count.
function countText(n: number, noun: string): string {
  return `${n} ${noun}${n === 1 ? "" : "s"}`;
}

async function envIdOf(page: Page): Promise<string> {
  await page.goto("/");
  await expect(page).toHaveURL(/\/env\/[^/]+$/);
  const id = new URL(page.url()).pathname.split("/")[2];
  if (!id) throw new Error(`no environment id in ${page.url()}`);
  return id;
}

test("the tiles equal the engine's counts and a new network appears live", async ({ page }) => {
  // Something in every list, with no registry to reach: an image imported from
  // an empty archive, a container created from it, a named volume.
  dind("tar -cf - -T /dev/null 2>/dev/null | docker import - e2e/empty:1");
  dind("docker create --name e2e-box e2e/empty:1 /nothing");
  dind("docker volume create e2e-vol");

  const counts = {
    containers: dindCount("docker ps -aq"),
    images: dindCount("docker image ls -q"),
    volumes: dindCount("docker volume ls -q"),
    networks: dindCount("docker network ls -q"),
  };
  expect(counts.containers).toBeGreaterThanOrEqual(1);
  expect(counts.images).toBeGreaterThanOrEqual(1);
  expect(counts.volumes).toBeGreaterThanOrEqual(1);

  const envId = await envIdOf(page);

  await expect(tileValue(page, "Containers")).toHaveText(String(counts.containers));
  await expect(tileValue(page, "Images")).toHaveText(String(counts.images));
  await expect(tileValue(page, "Volumes")).toHaveText(String(counts.volumes));
  await expect(tileValue(page, "Networks")).toHaveText(String(counts.networks));

  // The page must live through the change below: a flag set on this document
  // is gone if anything reloads it.
  await page.evaluate(() => {
    (window as unknown as { __noReload: boolean }).__noReload = true;
  });

  await expect(page.getByText("Recent events")).toBeVisible();
  dind("docker network create e2e-net");

  const row = page
    .getByRole("listitem")
    .filter({ hasText: "Network create" })
    .filter({ hasText: "e2e-net" });
  await expect(row).toBeVisible({ timeout: 10_000 });
  await expect(tileValue(page, "Networks")).toHaveText(String(counts.networks + 1), {
    timeout: 10_000,
  });
  expect(
    await page.evaluate(() => (window as unknown as { __noReload?: boolean }).__noReload),
    "the page was reloaded",
  ).toBe(true);

  // Each list view carries the same number in its header.
  const lists = [
    { path: "containers", title: "Containers", noun: "container", n: counts.containers },
    { path: "images", title: "Images", noun: "image", n: counts.images },
    { path: "volumes", title: "Volumes", noun: "volume", n: counts.volumes },
    { path: "networks", title: "Networks", noun: "network", n: counts.networks + 1 },
  ];
  for (const list of lists) {
    await page.goto(`/env/${envId}/${list.path}`);
    await expect(page.getByRole("heading", { name: list.title, level: 1 })).toBeVisible();
    await expect(page.getByText(countText(list.n, list.noun), { exact: true })).toBeVisible();
  }
});

test("Bayplan reads its own container ID from inside the container", async ({ page }) => {
  const envId = await envIdOf(page);
  const res = await page.request.get(`/api/env/${envId}/summary`);
  expect(res.ok()).toBe(true);
  const summary = (await res.json()) as { selfContainerId?: string };

  // What the engine hosting the stack calls Bayplan's container, against what
  // Bayplan read from /proc/self/mountinfo inside it (coverage D5, RESEARCH A2).
  const fullId = docker("inspect", "-f", "{{.Id}}", compose("ps", "-q", "bayplan").trim());
  expect(fullId).toMatch(/^[0-9a-f]{64}$/);
  expect(summary.selfContainerId, "selfContainerId").toBeTruthy();
  expect(summary.selfContainerId?.length).toBeGreaterThanOrEqual(12);
  expect(fullId.startsWith(summary.selfContainerId ?? "x")).toBe(true);
});
