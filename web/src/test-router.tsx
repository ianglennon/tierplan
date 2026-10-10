// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

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
export function TestRouter({ children }: { children: ReactNode }) {
  const [router] = useState(() => {
    const root = createRootRoute();
    const tree = root.addChildren([
      createRoute({ getParentRoute: () => root, path: "/env/$envId/containers/$containerId" }),
      createRoute({ getParentRoute: () => root, path: "/env/$envId/networks/$networkId" }),
    ]);
    return createRouter({
      routeTree: tree,
      history: createMemoryHistory({ initialEntries: ["/"] }),
    });
  });
  return <RouterContextProvider router={router}>{children}</RouterContextProvider>;
}
