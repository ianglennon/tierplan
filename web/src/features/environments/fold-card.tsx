// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { TriangleAlert } from "lucide-react";
import { useRef, useState } from "react";

import {
  type EnvironmentItem,
  environmentsQuery,
  FoldLocalError,
  type FoldOffer,
  foldLocal,
  foldOfferQuery,
  refreshAfterFold,
} from "@/api/environments";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

type FailureKey = "notOffered" | "nodeNotListed" | "ingressActive" | "failed";

// Build-time source URL (Vite define). The docs ship with the source; the ingress refusal
// links to the section of docs/install-in-swarm.md that says how to fold a host that has
// ingress set up (D-12). The URL is built from the constant and a fixed path, never from
// anything the server sends.
declare const __TIERPLAN_SOURCE_URL__: string;
const INGRESS_DOCS_PATH = "/blob/main/docs/install-in-swarm.md#fold-a-host-that-has-ingress-set-up";

interface FoldCardProps {
  // The environment whose settings page this is: the swarm, or the local host's own.
  env: EnvironmentItem;
  // Called with the swarm environment's ID once the fold went through, so the page of
  // the environment that was just retired can move to the swarm's.
  onFolded?: (swarmEnvId: string) => void;
  // Where focus goes when the dialog closes and the card's own button is gone, which is
  // what a fold does: the offer ends, and with it the card.
  focusFallback?: () => HTMLElement | null;
}

// ingressParts names what the ingress refusal found and whether ingress is turned on, as
// the {{what}} and {{andOff}} of its sentences (D-17). Only what exists is named, so a
// version counter alone never shows. Without the ingress counts (a POST refused it and the
// offer has not been read again yet) the words are general.
function ingressParts(
  t: (key: string, options?: Record<string, unknown>) => string,
  ingress: FoldOffer["ingress"],
): { what: string; andOff: string } {
  if (!ingress) return { what: t("envsettings.fold.ingressWhat.unknown"), andOff: "" };
  const named = [
    ingress.domains > 0
      ? t("envsettings.fold.ingressWhat.domains", { count: ingress.domains })
      : "",
    ingress.reserved > 0
      ? t("envsettings.fold.ingressWhat.reserved", { count: ingress.reserved })
      : "",
    ingress.hostnames > 0
      ? t("envsettings.fold.ingressWhat.hostnames", { count: ingress.hostnames })
      : "",
    ingress.routes > 0 ? t("envsettings.fold.ingressWhat.routes", { count: ingress.routes }) : "",
    ingress.enabled ? t("envsettings.fold.ingressWhat.enabled") : "",
  ].filter((part) => part !== "");
  return {
    what: named.length > 0 ? named.join(", ") : t("envsettings.fold.ingressWhat.unknown"),
    andOff: ingress.enabled ? t("envsettings.fold.ingressAndOff") : "",
  };
}

// FoldCard is the Local host card (D-48): this TierPlan host is a manager of a swarm that
// is enrolled through agents, so it can be reached through the Docker socket and needs no
// agent. "Fold into {swarm}" opens a plain dialog with the primary confirm, because
// nothing on the host changes and the fold is recorded in the audit log. Confirming posts
// once. A failure keeps the dialog open with an inline alert. It is rendered only while
// the server offers the fold, on the swarm's page and on the local host's own.
//
// Hiding the card is not access control; the server answers 403 without environments.manage.
export function FoldCard({ env, onFolded, focusFallback }: FoldCardProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const environments = useQuery(environmentsQuery);
  const [open, setOpen] = useState(false);
  const [failure, setFailure] = useState<FailureKey | null>(null);
  // The explicit tick a fold that removes stored data needs (D-16).
  const [confirmed, setConfirmed] = useState(false);
  const trigger = useRef<HTMLButtonElement>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);

  // The swarm the host would be folded into: this environment, or for the local host's
  // own page the agent environment that offers it.
  const swarm =
    env.kind === "agent"
      ? env
      : environments.data?.items?.find((e) => e.kind === "agent" && e.foldOffered);

  // The offer comes from the card's own read (D-15): the list carries it only for installs
  // with a Local environment. Nothing renders while it is pending or has failed (U-03).
  const offerRead = useQuery({ ...foldOfferQuery(swarm?.id ?? ""), enabled: !!swarm });
  const read = offerRead.data;
  // The offer the dialog was opened on stays what it shows while the page reads it again
  // after a refusal, so the alert that explains the refusal is not unmounted with it.
  const held = useRef<FoldOffer | undefined>(undefined);
  if (read?.offered) held.current = read;
  const refusal = read?.refusal;
  const offer = read?.offered ? read : open ? held.current : undefined;

  const fold = useMutation({
    mutationFn: (swarmId: string) => foldLocal(swarmId),
    onSuccess: async (_data, swarmId) => {
      await refreshAfterFold(queryClient, swarmId);
      setOpen(false);
      toastSuccess(t("envsettings.fold.done", { swarm: swarm?.name ?? "" }));
      onFolded?.(swarmId);
    },
    onError: (error, swarmId) => {
      if (error instanceof FoldLocalError && error.notOffered) {
        setFailure("notOffered");
        // What is offered has changed, so the page underneath stops offering it.
        void refreshAfterFold(queryClient, swarmId);
        return;
      }
      if (error instanceof FoldLocalError && error.nodeNotListed) {
        setFailure("nodeNotListed");
        return;
      }
      if (error instanceof FoldLocalError && error.ingressActive) {
        setFailure("ingressActive");
        // The card then shows the same refusal, with what was found, and the docs link.
        void queryClient.invalidateQueries({ queryKey: foldOfferQuery(swarmId).queryKey });
        return;
      }
      setFailure("failed");
    },
    onSettled: () => {
      inFlight.current = false;
    },
  });

  if (!swarm || (!offer && !refusal)) return null;

  function change(next: boolean) {
    if (next) {
      setOpen(true);
      return;
    }
    if (fold.isPending) return;
    setFailure(null);
    setConfirmed(false);
    fold.reset();
    setOpen(false);
  }

  // The data-removal block (D-16): only for a Local-environment fold with something
  // counted in it. A socket fold, or an empty Local environment, removes nothing stored.
  const removes = offer?.source === "local_environment" ? offer.removes : undefined;
  const removal = removes
    ? [
        removes.stacks > 0 ? t("envsettings.fold.removes.stacks", { count: removes.stacks }) : null,
        removes.gitRepos > 0
          ? t("envsettings.fold.removes.repos", { count: removes.gitRepos })
          : null,
        removes.registries > 0
          ? t("envsettings.fold.removes.registries", { count: removes.registries })
          : null,
        removes.janitor ? t("envsettings.fold.removes.janitor") : null,
      ].filter((line): line is string => line !== null)
    : [];
  const destructive = removal.length > 0;

  const name = swarm.name;
  const { what, andOff } = ingressParts(t, read?.ingress);
  const docsLink = (
    <a
      href={`${__TIERPLAN_SOURCE_URL__}${INGRESS_DOCS_PATH}`}
      target="_blank"
      rel="noopener noreferrer"
      className="rounded-sm underline underline-offset-3 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
    >
      {t("envsettings.fold.ingressDocsLink")}
    </a>
  );
  return (
    <SettingsCard title={t("envsettings.fold.title")} id="fold">
      {refusal === "fold_ingress_active" ? (
        <Alert variant="warning">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription className="text-foreground break-words">
            {t("envsettings.fold.ingressBlocked", { what, andOff, swarm: name })}{" "}
            {t("envsettings.fold.ingressDocsBefore")} {docsLink}{" "}
            {t("envsettings.fold.ingressDocsAfter")}
          </AlertDescription>
        </Alert>
      ) : refusal === "fold_node_not_listed" ? (
        <Alert variant="warning">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription className="text-foreground break-words">
            {t("envsettings.fold.nodeNotListed", { swarm: name })}
          </AlertDescription>
        </Alert>
      ) : (
        <>
          <Alert>
            <AlertDescription>{t("envsettings.fold.alert", { swarm: name })}</AlertDescription>
          </Alert>
          <div>
            <Button
              ref={trigger}
              type="button"
              className={`${TOUCH_HEIGHT} max-md:w-full`}
              onClick={() => setOpen(true)}
            >
              {t("envsettings.fold.button", { swarm: name })}
            </Button>
          </div>
        </>
      )}
      {offer ? (
        <Dialog open={open} onOpenChange={change}>
          <DialogContent
            showCloseButton={false}
            onCloseAutoFocus={(event) => {
              const target = trigger.current?.isConnected ? trigger.current : focusFallback?.();
              if (!target) return;
              event.preventDefault();
              target.focus();
            }}
          >
            <DialogHeader>
              <DialogTitle>{t("envsettings.fold.dialogTitle", { swarm: name })}</DialogTitle>
              <DialogDescription>{t("envsettings.fold.body")}</DialogDescription>
            </DialogHeader>
            <ul className="flex list-disc flex-col gap-2 pl-4 text-sm">
              <li>{t("envsettings.fold.standby")}</li>
              {offer?.source === "local_environment" ? (
                <li>{t("envsettings.fold.removesLocal")}</li>
              ) : null}
              <li>{t("envsettings.fold.audited")}</li>
            </ul>
            {destructive ? (
              <Alert variant="warning">
                <TriangleAlert aria-hidden="true" />
                <AlertDescription className="flex flex-col gap-2 text-foreground">
                  <p>{t("envsettings.fold.removesIntro")}</p>
                  <ul className="flex list-disc flex-col gap-1 pl-4 break-words">
                    {removal.map((line) => (
                      <li key={line}>{line}</li>
                    ))}
                  </ul>
                  <p>{t("envsettings.fold.keepsContainers")}</p>
                </AlertDescription>
              </Alert>
            ) : null}
            {destructive ? (
              <div className="flex min-h-11 items-start gap-3 md:min-h-0">
                <Checkbox
                  id="fold-understand"
                  checked={confirmed}
                  disabled={fold.isPending}
                  onCheckedChange={(next) => setConfirmed(next === true)}
                  className="mt-0.5"
                />
                <Label htmlFor="fold-understand" className="font-normal">
                  {t("envsettings.fold.understand")}
                </Label>
              </div>
            ) : null}
            {failure ? (
              <Alert variant="destructive" role="alert">
                <AlertDescription className="break-words">
                  {failure === "ingressActive" ? (
                    <>
                      {t("envsettings.fold.ingressActive", { what, andOff, swarm: name })}{" "}
                      {t("envsettings.fold.ingressDocsBefore")} {docsLink}{" "}
                      {t("envsettings.fold.ingressDocsAfter")}
                    </>
                  ) : (
                    t(`envsettings.fold.${failure}`, { swarm: name })
                  )}
                </AlertDescription>
              </Alert>
            ) : null}
            <DialogFooter>
              <DialogClose asChild>
                <Button
                  type="button"
                  variant="outline"
                  disabled={fold.isPending}
                  className={TOUCH_HEIGHT}
                >
                  {t("envsettings.fold.cancel")}
                </Button>
              </DialogClose>
              {failure === "ingressActive" ? null : (
                <Button
                  type="button"
                  variant={destructive ? "destructive" : "default"}
                  disabled={fold.isPending || (destructive && !confirmed)}
                  className={TOUCH_HEIGHT}
                  onClick={() => {
                    if (inFlight.current) return;
                    if (destructive && !confirmed) return;
                    inFlight.current = true;
                    setFailure(null);
                    fold.mutate(swarm.id);
                  }}
                >
                  {fold.isPending ? (
                    <>
                      <Spinner aria-hidden="true" />
                      {t("envsettings.fold.pending")}
                    </>
                  ) : (
                    t("envsettings.fold.confirm", { swarm: name })
                  )}
                </Button>
              )}
            </DialogFooter>
          </DialogContent>
        </Dialog>
      ) : null}
    </SettingsCard>
  );
}
