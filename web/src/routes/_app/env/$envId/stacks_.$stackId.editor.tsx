// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";
import { lazy, Suspense } from "react";

import { Skeleton } from "@/components/ui/skeleton";
import { z } from "@/lib/zod-config";

// The editor, with CodeMirror in it, is loaded when this tab is opened and not before, so the
// lists and the other tabs of a stack do not carry it (UI-SPEC "Design System").
const StackEditorPage = lazy(() =>
  import("@/features/stack-editor/editor-page").then((m) => ({ default: m.StackEditorPage })),
);

// The editor's search: the pane (Files, and later Variables and Rendered) and the file open in
// the Files pane. A value that does not fit is dropped rather than refused, so an old or edited
// link still opens. The pane and the file live in the address, never in storage (UI-SPEC "Storage").
export const editorSearchSchema = z.object({
  pane: z.enum(["files", "variables", "rendered"]).optional().catch(undefined),
  file: z.string().max(255).optional().catch(undefined),
  // A line to put the cursor on, from a link in the Review's list of errors; used once.
  line: z.number().int().positive().max(1_000_000).optional().catch(undefined),
});

// The Editor tab of a stack: the tab under the stack's header, so the header and the tab bar stay.
function StackEditorRoute() {
  const { envId, stackId } = Route.useParams();
  const { pane, file, line } = Route.useSearch();
  const navigate = Route.useNavigate();
  return (
    <Suspense fallback={<Skeleton className="h-100 w-full" />}>
      <StackEditorPage
        envId={envId}
        stackId={stackId}
        pane={pane ?? "files"}
        onPaneChange={(next, opened) =>
          void navigate({
            search: (prev) => ({
              ...prev,
              pane: next,
              // A file opened with the pane is the address's file; compose.yaml is no file.
              ...(opened === undefined
                ? {}
                : { file: opened === "compose.yaml" ? undefined : opened }),
            }),
            replace: true,
          })
        }
        file={file}
        onFileChange={(next) =>
          void navigate({ search: (prev) => ({ ...prev, file: next }), replace: true })
        }
        line={line}
        onLineHandled={() =>
          void navigate({ search: (prev) => ({ ...prev, line: undefined }), replace: true })
        }
        onReview={() =>
          void navigate({
            to: "/env/$envId/stacks/$stackId/review",
            params: { envId, stackId },
          })
        }
      />
    </Suspense>
  );
}

export const Route = createFileRoute("/_app/env/$envId/stacks_/$stackId/editor")({
  validateSearch: editorSearchSchema,
  component: StackEditorRoute,
});
