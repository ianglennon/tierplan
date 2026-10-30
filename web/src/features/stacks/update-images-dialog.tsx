// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { CircleX, Info } from "lucide-react";
import { useId, useState } from "react";

import {
  type ReviewRequestError,
  reviewKey,
  reviewQuery,
  type StackDetail,
  type StackService,
  stackServicesQuery,
} from "@/api/stacks";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { TruncatedText } from "@/features/resources/resource-table";
import { DigestChip } from "@/features/stacks/digest-chip";
import { useT } from "@/i18n/use-t";

// How long the review a lookup just made stays in the cache, so that the Review page the dialog opens reads
// it and does not look every tag up a second time. The page keeps nothing once it is closed, and this is only
// the hand-over.
const HANDOVER_MS = 15_000;

interface UpdateImagesDialogProps {
  stack: Pick<StackDetail, "id" | "envId" | "name" | "target">;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

// Why a service is listed but cannot be ticked, or the empty string for one that can.
type Skip = "pinnedInFile" | "notFromRegistry" | "notDeployed" | "";

// skipOf is why the image of a service cannot be updated here (UI-SPEC "Update images dialog"): its reference
// carries its own digest in the compose file, it runs an image no registry has, or the engine runs nothing for
// it. A digest the file writes is the author's pin: only editing the file moves it.
export function skipOf(s: Pick<StackService, "image" | "pinned" | "pinState">): Skip {
  if (s.pinned === "") return "notDeployed";
  if (s.pinState === "local") return "notFromRegistry";
  if (s.image.includes("@")) return "pinnedInFile";
  return "";
}

// digestOf is the digest an image the engine runs carries: what follows the @, or the whole of an image ID.
function digestOf(ref: string): string {
  const at = ref.lastIndexOf("@");
  return at >= 0 ? ref.slice(at + 1) : ref;
}

// refusalOf words a lookup the server refused or could not make.
function refusalOf(error: ReviewRequestError): string {
  if (error.status === 0) return "stackUpdate.refusal.unreachable";
  switch (error.type) {
    case "services_invalid":
    case "target_changed":
      return `stackUpdate.refusal.${error.type}`;
  }
  return error.status === 403 ? "stackUpdate.refusal.permission" : "stackUpdate.refusal.other";
}

// UpdateImagesDialog is "Update images in {name}" (D-16, STK-09, UI-SPEC "Update images dialog"): the services
// the stack runs, each with its image and the digest it runs now, the ones that can move ticked. Review image
// updates looks the chosen tags up again (the same review a deploy opens, in update mode) and opens that
// review; nothing is deployed from here. A tag whose registry cannot be asked marks its row and leaves the
// rest to go on. A service that cannot move is listed, off, with the reason. Names, images and notes are text.
export function UpdateImagesDialog({ stack, open, onOpenChange }: UpdateImagesDialogProps) {
  const t = useT();
  const id = useId();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const services = useQuery({
    ...stackServicesQuery(stack.envId, stack.id, { target: stack.target }),
    enabled: open,
  });
  // The services the person unticked, and the ones whose lookup failed (which are unticked too, and say why).
  const [unticked, setUnticked] = useState<readonly string[]>([]);
  const [lost, setLost] = useState<Readonly<Record<string, "unreachable" | "unresolved">>>({});
  const [submitting, setSubmitting] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);

  // Nothing is worked out while the dialog is closed: the rows are the stack page's own cache.
  const items = open
    ? [...(services.data?.items ?? [])].sort((a, b) => a.short.localeCompare(b.short))
    : [];
  const eligible = items.filter((s) => skipOf(s) === "");
  const chosen = eligible.map((s) => s.short).filter((name) => !unticked.includes(name));
  const loading = services.isPending && !services.data;
  const loadFailed = services.isError && !services.data;
  const empty = !loading && !loadFailed && eligible.length === 0;
  const reason = empty
    ? t("stackUpdate.dialog.none")
    : chosen.length === 0
      ? t("stackUpdate.dialog.tickOne")
      : undefined;
  const reasonId = `${id}-reason`;
  const blocked = loading || loadFailed || reason !== undefined;

  function change(next: boolean) {
    if (!next) {
      setUnticked([]);
      setLost({});
      setFailure(null);
    }
    onOpenChange(next);
  }

  function toggle(name: string, on: boolean) {
    setUnticked((have) =>
      on ? have.filter((n) => n !== name) : [...have.filter((n) => n !== name), name],
    );
  }

  // submit looks the chosen tags up (a review in update mode) and, when every one could be, opens that review.
  // A service whose registry could not be asked is marked and unticked, and the dialog stays for the others.
  async function submit() {
    if (submitting || blocked) return;
    setSubmitting(true);
    setFailure(null);
    try {
      const review = await queryClient.fetchQuery({
        ...reviewQuery(stack.envId, stack.id, "update", chosen),
        staleTime: 0,
        gcTime: HANDOVER_MS,
      });
      const failed: Record<string, "unreachable" | "unresolved"> = {};
      for (const row of review.images ?? []) {
        if (
          chosen.includes(row.service) &&
          (row.state === "unreachable" || row.state === "unresolved")
        ) {
          failed[row.service] = row.state;
        }
      }
      if (Object.keys(failed).length > 0) {
        queryClient.removeQueries({ queryKey: reviewKey(stack.envId, stack.id, "update", chosen) });
        setLost(failed);
        setUnticked((have) => [...new Set([...have, ...Object.keys(failed)])]);
        return;
      }
      change(false);
      void navigate({
        to: "/env/$envId/stacks/$stackId/review",
        params: { envId: stack.envId, stackId: stack.id },
        search: { mode: "update", services: chosen.join(",") },
      });
    } catch (e) {
      setFailure(
        t("stackUpdate.dialog.lookupFailed", {
          name: stack.name,
          reason: t(refusalOf(e as ReviewRequestError)),
        }),
      );
    } finally {
      setSubmitting(false);
    }
  }

  const noteOf = (s: StackService): string => {
    const skip = skipOf(s);
    if (skip) return t(`stackUpdate.notes.${skip}`);
    const failedLookup = lost[s.short];
    return failedLookup ? t(`stackUpdate.notes.${failedLookup}`) : "";
  };

  return (
    <Dialog open={open} onOpenChange={change}>
      <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-160">
        <DialogHeader>
          <DialogTitle className="break-all">
            {t("stackUpdate.dialog.title", { name: stack.name })}
          </DialogTitle>
          <DialogDescription>{t("stackUpdate.dialog.body")}</DialogDescription>
        </DialogHeader>

        {loading ? (
          <div className="flex flex-col gap-2">
            <div data-testid="update-skeleton" aria-hidden="true" className="flex flex-col gap-2">
              <Skeleton className="h-11 w-full" />
              <Skeleton className="h-11 w-full" />
              <Skeleton className="h-11 w-full" />
            </div>
            <p role="status" className="text-sm text-muted-foreground">
              {t("stackUpdate.dialog.loading")}
            </p>
          </div>
        ) : null}

        {loadFailed ? (
          <Alert variant="destructive" role="alert">
            <CircleX aria-hidden="true" />
            <AlertDescription className="flex flex-wrap items-center gap-3">
              <span className="break-words">
                {t("stackUpdate.dialog.loadFailed", { name: stack.name })}
              </span>
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="max-md:h-11"
                disabled={services.isFetching}
                onClick={() => void services.refetch()}
              >
                {t("stackUpdate.dialog.retry")}
              </Button>
            </AlertDescription>
          </Alert>
        ) : null}

        {empty ? (
          <Alert role="status">
            <Info aria-hidden="true" />
            <AlertTitle>{t("stackUpdate.dialog.none")}</AlertTitle>
            <AlertDescription>{t("stackUpdate.dialog.noneBody")}</AlertDescription>
          </Alert>
        ) : null}

        {items.length > 0 ? (
          <>
            <div className="flex flex-wrap items-center gap-2">
              <Button
                type="button"
                variant="ghost"
                size="sm"
                className="max-md:h-11"
                disabled={eligible.length === 0 || submitting}
                onClick={() => setUnticked([])}
              >
                {t("stackUpdate.dialog.selectAll")}
              </Button>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                className="max-md:h-11"
                disabled={eligible.length === 0 || submitting}
                onClick={() => setUnticked(eligible.map((s) => s.short))}
              >
                {t("stackUpdate.dialog.clearSelection")}
              </Button>
              <span aria-live="polite" className="ms-auto text-sm text-muted-foreground">
                {t("stackUpdate.dialog.selected", { count: chosen.length })}
              </span>
            </div>
            <div
              data-slot="target-list"
              className="max-h-60 overflow-y-auto rounded-lg ring-1 ring-foreground/10"
            >
              <table
                className="w-full text-sm"
                aria-label={t("stackUpdate.dialog.listLabel", { name: stack.name })}
              >
                <thead className="sticky top-0 bg-muted text-xs text-muted-foreground">
                  <tr>
                    <th scope="col" className="w-10 px-2 py-1">
                      <span className="sr-only">{t("stackUpdate.columns.select")}</span>
                    </th>
                    <th scope="col" className="px-2 py-1 text-start font-semibold">
                      {t("stackUpdate.columns.service")}
                    </th>
                    <th scope="col" className="px-2 py-1 text-start font-semibold">
                      {t("stackUpdate.columns.image")}
                    </th>
                    <th scope="col" className="px-2 py-1 text-start font-semibold">
                      {t("stackUpdate.columns.running")}
                    </th>
                    <th scope="col" className="px-2 py-1 text-start font-semibold">
                      {t("stackUpdate.columns.note")}
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {items.map((s) => {
                    const skip = skipOf(s) !== "";
                    const boxId = `${id}-${s.short}`;
                    return (
                      <tr
                        key={s.short}
                        data-skipped={skip || undefined}
                        className={`h-11 border-t ${skip ? "text-muted-foreground" : ""}`}
                      >
                        <td className="px-2 py-1">
                          <Checkbox
                            id={boxId}
                            aria-label={t("stackUpdate.row.select", { service: s.short })}
                            checked={!skip && !unticked.includes(s.short)}
                            disabled={skip || submitting}
                            onCheckedChange={(next) => toggle(s.short, next === true)}
                            className="max-md:size-6"
                          />
                        </td>
                        <td className="px-2 py-1 font-mono text-xs">
                          <TruncatedText mono className="max-w-40">
                            {s.short}
                          </TruncatedText>
                        </td>
                        <td className="px-2 py-1 font-mono text-xs">
                          <TruncatedText mono className="max-w-48">
                            {s.image}
                          </TruncatedText>
                        </td>
                        <td className="px-2 py-1">
                          {s.pinned === "" ? (
                            <>
                              <span aria-hidden="true">—</span>
                              <span className="sr-only">{t("stackUpdate.row.none")}</span>
                            </>
                          ) : (
                            <DigestChip
                              digest={digestOf(s.pinned)}
                              variant={s.pinState === "local" ? "local" : "pinned"}
                            />
                          )}
                        </td>
                        <td className="px-2 py-1 text-xs">
                          {lost[s.short] ? (
                            <span className="text-status-offline-text">{noteOf(s)}</span>
                          ) : (
                            noteOf(s)
                          )}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          </>
        ) : null}

        {failure ? (
          <Alert variant="destructive" role="alert">
            <CircleX aria-hidden="true" />
            <AlertDescription className="flex flex-wrap items-center gap-3">
              <span className="break-words">{failure}</span>
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="max-md:h-11"
                disabled={submitting}
                onClick={() => void submit()}
              >
                {t("stackUpdate.dialog.retry")}
              </Button>
            </AlertDescription>
          </Alert>
        ) : null}

        <DialogFooter>
          {reason && !loading && !loadFailed ? (
            <p id={reasonId} className="me-auto min-w-0 text-sm break-words text-muted-foreground">
              {reason}
            </p>
          ) : null}
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            onClick={() => change(false)}
          >
            {t("stackUpdate.dialog.cancel")}
          </Button>
          <Button
            type="button"
            className="max-md:h-11 aria-disabled:cursor-not-allowed aria-disabled:opacity-50"
            aria-disabled={blocked || submitting ? true : undefined}
            aria-describedby={reason && !loading && !loadFailed ? reasonId : undefined}
            onClick={() => void submit()}
          >
            {submitting ? (
              <>
                <Spinner aria-hidden="true" />
                {t("stackUpdate.dialog.submitting")}
              </>
            ) : (
              t("stackUpdate.dialog.submit")
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
