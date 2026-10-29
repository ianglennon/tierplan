// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { ChevronRight, CirclePause, Info, Webhook } from "lucide-react";
import { useEffect, useId, useRef, useState } from "react";

import { environmentsQuery } from "@/api/environments";
import type {
  StackGit,
  StackGitWebhook,
  StackGitWebhookFlavour,
  StackGitWebhookSecret,
} from "@/api/stack-git";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { SetUpWebhookDialog } from "@/features/stack-git/set-up-webhook-dialog";
import { CopyValueRow, ShowOnceSecret } from "@/features/stack-git/show-once-secret";
import { RemoveWebhookDialog, RotateSecretDialog } from "@/features/stack-git/webhook-dialogs";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// A character that is in no message, to cut the sentence where the relative time goes. The time is a component (a
// <time> with a tooltip), so it cannot be put into the string by i18next.
const SLOT = "\u0001";

interface WebhookCardProps {
  envId: string;
  stackId: string;
  stackName: string;
  git: StackGit;
}

// WebhookCard is the Webhook card of the Git tab (UI-SPEC "Git tab", D-15, GIT-05): whether the stack has a webhook
// and, when it has, its git host, its address with the secret part masked, the last four characters of its secret
// and its last delivery. Setting one up shows the address and the secret once, in a panel held in this component's
// state only: it is cleared by Done, by leaving the stack and by unmounting, and it is in no query, no storage, no
// toast and no address. Only an Admin of the environment manages the webhook; anyone who can read the stack sees it.
export function WebhookCard({ envId, stackId, stackName, git }: WebhookCardProps) {
  const t = useT();
  const headingId = useId();
  const heading = useRef<HTMLHeadingElement>(null);
  const needs = useNeeds("git.manage", envId);
  const environments = useQuery(environmentsQuery);
  const [setupOpen, setSetupOpen] = useState(false);
  const [rotateOpen, setRotateOpen] = useState(false);
  const [removeOpen, setRemoveOpen] = useState(false);
  // What the last create answered, held until Done. It is the only copy of the full address and the secret here.
  const [shown, setShown] = useState<StackGitWebhookSecret | null>(null);
  const webhook = git.webhook;

  // Another stack, or another environment, is another webhook: nothing shown for this one carries over.
  // biome-ignore lint/correctness/useExhaustiveDependencies: the ids are the trigger, not the body
  useEffect(() => {
    setShown(null);
    setSetupOpen(false);
    setRotateOpen(false);
    setRemoveOpen(false);
  }, [envId, stackId]);

  const canManage = needs.allowed;
  // Permissions not known yet: the button is off, with no reason to give.
  const unknown = !needs.allowed && !needs.reason;
  const envName = environments.data?.items?.find((e) => e.id === envId)?.name;
  const reason = needs.reason
    ? envName
      ? t("stackGit.webhook.needsOnEnvironment", { need: needs.reason, name: envName })
      : needs.reason
    : undefined;

  function done() {
    setShown(null);
    heading.current?.focus();
  }

  const setUpButton = (
    <Button
      type="button"
      variant="outline"
      className={TOUCH_HEIGHT}
      disabled={unknown}
      onClick={() => setSetupOpen(true)}
    >
      {t("stackGit.webhook.setUp")}
    </Button>
  );

  const flavour = webhook?.flavour ?? shown?.flavour;
  const hostName = webhook
    ? webhook.flavour === "generic"
      ? t("stackGit.webhook.hostFallback")
      : t(`stackGit.webhook.host.${webhook.flavour}`)
    : "";

  // A button of the configured card: plain for an Admin, with the reason for anyone else.
  function action(label: string, onClick: () => void, className = "") {
    const button = (
      <Button
        type="button"
        variant="outline"
        className={`${TOUCH_HEIGHT} ${className}`}
        disabled={unknown}
        onClick={onClick}
      >
        {label}
      </Button>
    );
    return canManage || unknown ? button : <NeedsTooltip reason={reason}>{button}</NeedsTooltip>;
  }

  return (
    <section aria-labelledby={headingId}>
      <Card className="gap-4 [--card-spacing:--spacing(4)] md:[--card-spacing:--spacing(6)]">
        <CardHeader>
          <CardTitle>
            <h3
              id={headingId}
              ref={heading}
              tabIndex={-1}
              className="flex items-center gap-2 text-base font-semibold outline-none"
            >
              <Webhook aria-hidden="true" className="size-4" />
              {t("stackGit.webhook.title")}
            </h3>
          </CardTitle>
          <CardDescription>{t("stackGit.webhook.body")}</CardDescription>
        </CardHeader>
        <CardContent className="flex min-w-0 flex-col gap-4">
          {shown ? <ShowOnceSecret id={`${headingId}-once`} secret={shown} onDone={done} /> : null}
          {webhook ? (
            <>
              <WebhookRows webhook={webhook} idPrefix={headingId} />
              <div className="flex flex-wrap gap-2">
                {action(t("stackGit.webhook.rotateAction"), () => setRotateOpen(true))}
                {action(
                  t("stackGit.webhook.removeAction"),
                  () => setRemoveOpen(true),
                  "text-destructive",
                )}
              </div>
            </>
          ) : null}
          {!webhook && !shown ? (
            <>
              <p className="text-sm text-muted-foreground">{t("stackGit.webhook.none")}</p>
              <div>
                {canManage || unknown ? (
                  setUpButton
                ) : (
                  <NeedsTooltip reason={reason}>{setUpButton}</NeedsTooltip>
                )}
              </div>
            </>
          ) : null}
          {webhook || shown ? <WebhookNote gitRef={git.ref} /> : null}
          {flavour ? (
            <SetupHelp flavour={flavour} placement={webhook?.placement ?? shown?.placement} />
          ) : null}
        </CardContent>
      </Card>
      <SetUpWebhookDialog
        envId={envId}
        stackId={stackId}
        open={setupOpen}
        onOpenChange={setSetupOpen}
        onCreated={setShown}
      />
      <RotateSecretDialog
        envId={envId}
        stackId={stackId}
        host={hostName}
        open={rotateOpen}
        onOpenChange={setRotateOpen}
        onRotated={setShown}
      />
      <RemoveWebhookDialog
        envId={envId}
        stackId={stackId}
        stackName={stackName}
        pollEnabled={git.pollEnabled}
        pollMinutes={git.pollMinutes}
        open={removeOpen}
        onOpenChange={setRemoveOpen}
        onRemoved={() => setShown(null)}
      />
    </section>
  );
}

interface WebhookRowsProps {
  webhook: StackGitWebhook;
  idPrefix: string;
}

// The rows of a webhook that is set up. Every value is what the git route answers: the address has its secret part
// masked, so Copy address copies it without the secret.
function WebhookRows({ webhook, idPrefix }: WebhookRowsProps) {
  const t = useT();
  const [before = "", after = ""] = t("stackGit.webhook.rows.accepted", { when: SLOT }).split(SLOT);
  const [rejectedBefore = "", rejectedAfter = ""] = t("stackGit.webhook.rows.rejected", {
    when: SLOT,
  }).split(SLOT);
  const delivered = webhook.lastAt && webhook.lastResult;
  return (
    <div className="flex min-w-0 flex-col gap-3">
      <dl className="grid min-w-0 grid-cols-[auto_1fr] items-baseline gap-x-6 gap-y-2 text-sm">
        <dt className="text-muted-foreground">{t("stackGit.webhook.rows.host")}</dt>
        <dd>{t(`stackGit.webhook.host.${webhook.flavour}`)}</dd>
        <dt className="text-muted-foreground">{t("stackGit.webhook.rows.secret")}</dt>
        <dd className="font-mono text-xs">
          {"••••"}
          {webhook.last4}
        </dd>
        <dt className="text-muted-foreground">{t("stackGit.webhook.rows.lastDelivery")}</dt>
        <dd>
          {delivered ? (
            webhook.lastResult === "accepted" ? (
              <>
                {before}
                <RelativeTime value={webhook.lastAt} />
                {after}
              </>
            ) : (
              <>
                {rejectedBefore}
                <RelativeTime value={webhook.lastAt} />
                {rejectedAfter}
              </>
            )
          ) : (
            t("stackGit.webhook.rows.never")
          )}
        </dd>
      </dl>
      {webhook.paused ? (
        <p className="flex items-start gap-2 text-sm text-muted-foreground">
          <CirclePause
            aria-hidden="true"
            className="mt-0.5 size-4 shrink-0 text-status-warning-text"
          />
          <span>
            {webhook.owner.name
              ? t("stackGit.webhook.rows.pausedNote", { owner: webhook.owner.name })
              : t("stackGit.webhook.rows.pausedNoteNoOwner")}
          </span>
        </p>
      ) : null}
      <CopyValueRow
        id={`${idPrefix}-address`}
        label={t("stackGit.webhook.rows.address")}
        value={webhook.address}
        copyLabel={t("stackGit.webhook.rows.copyAddress")}
        copiedLabel={t("stackGit.webhook.rows.copiedAddress")}
      />
    </div>
  );
}

function WebhookNote({ gitRef }: { gitRef: string }) {
  const t = useT();
  return (
    <Alert role="note">
      <Info aria-hidden="true" />
      <AlertDescription>{t("stackGit.webhook.note", { ref: gitRef })}</AlertDescription>
    </Alert>
  );
}

interface SetupHelpProps {
  flavour: StackGitWebhookFlavour;
  placement?: "path" | "header";
}

// "How to add it": the steps in the git host, closed until asked for.
function SetupHelp({ flavour, placement }: SetupHelpProps) {
  const t = useT();
  const [open, setOpen] = useState(false);
  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <CollapsibleTrigger asChild>
        <Button type="button" variant="ghost" size="sm" className="-ml-2 max-md:h-11">
          <ChevronRight
            aria-hidden="true"
            className={
              open
                ? "rotate-90 transition-transform motion-reduce:transition-none"
                : "transition-transform motion-reduce:transition-none"
            }
          />
          {t("stackGit.webhook.help.trigger")}
        </Button>
      </CollapsibleTrigger>
      <CollapsibleContent className="pt-2 text-sm text-muted-foreground">
        <p>{t(`stackGit.webhook.help.${flavour}`)}</p>
        {flavour === "generic" && placement === "header" ? (
          <p className="mt-2">{t("stackGit.webhook.help.genericHeader")}</p>
        ) : null}
      </CollapsibleContent>
    </Collapsible>
  );
}
