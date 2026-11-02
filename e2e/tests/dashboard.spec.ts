// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Golden path 3 (D-80): the dashboard tiles equal what the dind daemon reports,
// and a network created behind TierPlan's back shows up in the events feed and
// in the Networks tile without a page reload (ENV-07).

import type { Page } from "@playwright/test";

import { AUTH_STATE, at } from "./env";
import { compose, dind, dindCount, docker, expect, openFirstEnvironment, test } from "./helpers";

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

const envIdOf = openFirstEnvironment;

// The network this spec creates behind TierPlan's back carries a name of its own
// per run and this label, so a run finds and removes what an earlier one left and
// the feed row asserted below is this run's, not an older run's.
const NETWORK_LABEL = "tierplan.e2e=dashboard";

test("the tiles equal the engine's counts and a new network appears live", async ({ page }) => {
  // Something in every list, with no registry to reach: an image imported from
  // an empty archive, a container created from it, a named volume.
  // The spec must be safe to run twice on one engine (D-15, D-23), so first clear
  // what an earlier run left: its container, every network it labelled, the image
  // it imported (a second import would leave the first untagged, and the Images
  // tile would count it) and any untagged image. Only the test engine is touched.
  // The counts below are taken after this.
  dind("docker rm -f e2e-box >/dev/null 2>&1 || true");
  dind(
    `docker network ls -q --filter label=${NETWORK_LABEL} | xargs -r docker network rm >/dev/null 2>&1 || true`,
  );
  dind("docker rmi -f e2e/empty:1 >/dev/null 2>&1 || true");
  dind("docker image prune -f >/dev/null 2>&1 || true");
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
  const network = `e2e-dash-${Date.now().toString(36)}`;
  try {
    await liveNetworkAppears(page, envId, network, counts);
  } finally {
    // containers.spec.ts starts from the engine as it was before this one.
    dind(`docker network rm ${network} >/dev/null 2>&1 || true`);
  }
});

async function liveNetworkAppears(
  page: Page,
  envId: string,
  network: string,
  counts: { containers: number; images: number; volumes: number; networks: number },
): Promise<void> {
  dind(`docker network create --label ${NETWORK_LABEL} ${network}`);

  const row = page
    .getByRole("listitem")
    .filter({ hasText: "Network create" })
    .filter({ hasText: network });
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
    await page.goto(at(`/env/${envId}/${list.path}`));
    await expect(page.getByRole("heading", { name: list.title, level: 1 })).toBeVisible();
    // A list that groups its rows (volumes, by stack) repeats a count in each group
    // header, so the page's own caption is the first match.
    await expect(
      page.getByText(countText(list.n, list.noun), { exact: true }).first(),
    ).toBeVisible();
  }
}

test("TierPlan reads its own container ID from inside the container", async ({ page }) => {
  const envId = await envIdOf(page);
  const res = await page.request.get(at(`/api/env/${envId}/summary`));
  expect(res.ok()).toBe(true);
  const summary = (await res.json()) as { selfContainerId?: string };

  // What the engine hosting the stack calls TierPlan's container, against what
  // TierPlan read from /proc/self/mountinfo inside it (coverage D5, RESEARCH A2).
  const fullId = docker("inspect", "-f", "{{.Id}}", compose("ps", "-q", "tierplan").trim());
  expect(fullId).toMatch(/^[0-9a-f]{64}$/);
  expect(summary.selfContainerId, "selfContainerId").toBeTruthy();
  expect(summary.selfContainerId?.length).toBeGreaterThanOrEqual(12);
  expect(fullId.startsWith(summary.selfContainerId ?? "x")).toBe(true);
});
