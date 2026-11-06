// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterContextProvider,
} from "@tanstack/react-router";
import { type ReactNode, useState } from "react";

// A component under test that draws a Link needs a router above it. This one
// gives it that, with no route to render: the Link builds its address and the
// page under test stays where it is. It is synchronous, so a test can look at the
// page as soon as it has rendered.
export function TestRouter({ children, path = "/" }: { children: ReactNode; path?: string }) {
  const [router] = useState(() => {
    const root = createRootRoute();
    const tree = root.addChildren([
      createRoute({ getParentRoute: () => root, path: "/env/$envId/containers" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/containers/$containerId" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/networks/$networkId" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/services" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/services/new" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/services/$serviceId" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/secrets" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/secrets/$secretId" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/configs" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/configs/$configId" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/nodes" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/nodes/$nodeId" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/stacks" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/stacks/$stackId" }),
      createRoute({
        getParentRoute: () => root,
        path: "/env/$envId/stacks/$stackId/deploys/$jobId",
      }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/stacks/$stackId/review" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/stacks/$stackId/editor" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/stacks/$stackId/git" }),
      createRoute({
        getParentRoute: () => root,
        path: "/env/$envId/stacks/$stackId/connect-git",
      }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/unmanaged-stacks/$name" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/events" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/address-space" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/address-space/guide" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/update-jobs/$jobId" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/janitor" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/janitor/runs/$runId" }),
      createRoute({ getParentRoute: () => root, path: "/settings/environments/$envId" }),
      createRoute({ getParentRoute: () => root, path: "/registries" }),
      createRoute({ getParentRoute: () => root, path: "/git" }),
      createRoute({ getParentRoute: () => root, path: "/git/credentials" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/volumes" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/volumes/$volumeName" }),
    ]);
    return createRouter({
      routeTree: tree,
      history: createMemoryHistory({ initialEntries: [path] }),
    });
  });
  return <RouterContextProvider router={router}>{children}</RouterContextProvider>;
}
