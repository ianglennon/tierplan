// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ReactNode } from "react";

import { VersionFooter } from "@/components/version-footer";

export interface PopoutFrameProps {
  // The name of what the window shows, in the mono heading of the bar.
  title: string;
  // The state of it, beside the name (a ContainerStateCell).
  state?: ReactNode;
  // The environment it belongs to, in muted text.
  environment: string;
  children: ReactNode;
}

// PopoutFrame is the layout of the pop-out windows (D-22): no sidebar and no header
// menu, a 48px bar that says what the window is about, the page, and the one-line
// version footer that PLAT-05 asks for on every page. The logs and the shell
// pop-outs share it.
export function PopoutFrame({ title, state, environment, children }: PopoutFrameProps) {
  return (
    <div className="flex min-h-svh flex-col bg-background">
      <header className="flex h-12 shrink-0 items-center gap-3 border-b px-4">
        <h1 className="min-w-0 truncate font-mono text-base font-semibold" title={title}>
          {title}
        </h1>
        {state ? <div className="shrink-0">{state}</div> : null}
        <span className="min-w-0 truncate text-sm text-muted-foreground">{environment}</span>
      </header>
      <main id="main" tabIndex={-1} className="min-w-0 flex-1 p-4 outline-none">
        {children}
      </main>
      <div className="border-t px-4 py-2">
        <VersionFooter />
      </div>
    </div>
  );
}
