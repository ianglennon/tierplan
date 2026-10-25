// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { CircleX, Lock, SquareStack } from "lucide-react";
import { useState } from "react";

import { AdoptRequestError, useAdoptStack } from "@/api/stack-adopt";
import { isUnmanagedGone, managedStackIdOf, unmanagedStackQuery } from "@/api/unmanaged-stacks";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { useEnvName } from "@/features/resources/use-env-name";
import { EditorFooter } from "@/features/service-editor/editor-footer";
import { EnvValuesNotice } from "@/features/stack-adopt/env-values-notice";
import {
  NotAdoptableNote,
  OrphanIdNote,
  PortainerAdvice,
} from "@/features/stack-unmanaged/unmanaged-notes";
import { useDocumentTitle } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

type Translate = (key: string, options?: Record<string, unknown>) => string;

interface AdoptPageProps {
  envId: string;
  // The swarm namespace or the Compose project of the stack to adopt. The router built it, encoded.
  name: string;
  // What to do once the stack is stored: the route replaces this page with the new stack's Overview, so Back
  // returns to the list and not to an adoption that is over.
  onAdopted: (stackId: string) => void;
}

function Breadcrumb({ envId, name }: { envId: string; name: string }) {
  const t = useT();
  const link =
    "rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none";
  return (
    <nav aria-label={t("stacks.detail.breadcrumb.label")}>
      <ol className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground [&>li:not(:last-child)]:after:ms-2 [&>li:not(:last-child)]:after:content-['/']">
        <li>
          <Link to="/env/$envId/stacks" params={{ envId }} className={link}>
            {t("stackAdopt.page.breadcrumbRoot")}
          </Link>
        </li>
        <li className="min-w-0">
          <Link to="/env/$envId/unmanaged-stacks/$name" params={{ envId, name }} className={link}>
            <span className="break-all">{name}</span>
          </Link>
        </li>
        <li>
          <span aria-current="page">{t("stackAdopt.page.breadcrumbCurrent")}</span>
        </li>
      </ol>
    </nav>
  );
}

// The page for a stack that is gone, or that someone adopted while this page was open.
function Gone({ envId, managedStackId }: { envId: string; managedStackId?: string }) {
  const t = useT();
  return (
    <Empty>
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <SquareStack aria-hidden="true" />
        </EmptyMedia>
        <EmptyTitle role="heading" aria-level={2}>
          {t("stackAdopt.gone.heading")}
        </EmptyTitle>
        <EmptyDescription>{t("stackAdopt.gone.body")}</EmptyDescription>
      </EmptyHeader>
      <EmptyContent className="flex-row flex-wrap justify-center">
        <Button asChild variant="outline" className="max-md:h-11">
          <Link to="/env/$envId/stacks" params={{ envId }}>
            {t("stackAdopt.gone.back")}
          </Link>
        </Button>
        {managedStackId ? (
          <Button asChild variant="outline" className="max-md:h-11">
            <Link to="/env/$envId/stacks/$stackId" params={{ envId, stackId: managedStackId }}>
              {t("stackAdopt.error.openStack")}
            </Link>
          </Button>
        ) : null}
      </EmptyContent>
    </Empty>
  );
}

// refusalText words an adoption the server refused, in the page's words. The server decides every refusal; this only
// says what it means. The lock line and the one naming the stack that holds the name carry a link of their own.
function refusalText(t: Translate, err: AdoptRequestError, name: string, envName: string): string {
  switch (err.type) {
    case "adopt_stale":
      return t("stackAdopt.error.stale", { env: envName });
    case "stack_already_managed":
      return t("stackAdopt.error.alreadyManaged", { name });
    case "stack_not_adoptable":
      return err.detail || t("stackAdopt.error.notAdoptable", { name });
    case "rebuild_failed":
      return t("stackGenerated.error.body", {
        name,
        reason: err.detail || t("stackGenerated.error.reasonUnknown"),
      });
  }
  if (err.status === 0) return t("stackAdopt.error.lost");
  if (err.status === 403) return t("stackAdopt.error.permission", { env: envName });
  return t("common.mutationFailed");
}

// AdoptPage is the page an Env Admin adopts an unmanaged stack from (UI-SPEC "Adopt page", STK-13, D-14): what
// adopting does, the note about environment values, and a footer with Cancel and Adopt stack. Pressing Adopt sends the
// fingerprint of what the page last read and nothing else: the server rebuilds the definition itself, stores the
// stack with its first revision, and sends nothing to the engine. It is never retried by itself. When what runs has
// changed since the page read it, the server refuses and the page offers Check again, which reads it again.
export function AdoptPage({ envId, name, onAdopted }: AdoptPageProps) {
  const t = useT();
  const envName = useEnvName(envId);
  const needs = useNeeds("stacks.create", envId);
  const query = useQuery(unmanagedStackQuery(envId, name));
  const adopt = useAdoptStack(envId, name);
  const [refusal, setRefusal] = useState<AdoptRequestError | null>(null);
  // True once the stack is stored: the lists are read again and this name is managed now, so the page must not
  // turn into the gone state while the route takes the person to the new stack.
  const [stored, setStored] = useState(false);
  const { data } = query;
  const gone = !stored && isUnmanagedGone(query.error);
  useDocumentTitle(t("stackAdopt.page.title", { name }));

  async function onAdopt() {
    if (!data || adopt.isPending) return;
    setRefusal(null);
    try {
      const made = await adopt.mutateAsync({ fingerprint: data.fingerprint });
      setStored(true);
      toastSuccess(t("stackAdopt.toast.adopted", { name, env: envName }));
      onAdopted(made.id);
    } catch (e) {
      setRefusal(e instanceof AdoptRequestError ? e : new AdoptRequestError(0));
    }
  }

  // Why Adopt stack is off, in the order that matters: the role it takes, a stack that cannot be adopted, and a
  // stack the page has not read yet. Absent when the person may adopt.
  let reason: string | undefined;
  if (!needs.allowed) reason = needs.reason;
  else if (data && !data.adoptable) reason = data.reason;
  else if (!data) reason = t("stackAdopt.checking", { name });
  const off = !needs.allowed || !data?.adoptable;

  if (gone) {
    return (
      <div className="flex flex-col gap-6">
        <header>
          <Breadcrumb envId={envId} name={name} />
        </header>
        <Gone envId={envId} managedStackId={managedStackIdOf(query.error)} />
      </div>
    );
  }

  const adoptButton = (
    <Button
      type="button"
      className="max-md:h-11"
      aria-disabled={off || undefined}
      aria-busy={adopt.isPending || undefined}
      onClick={() => {
        if (!off) void onAdopt();
      }}
    >
      {adopt.isPending ? (
        <>
          <Spinner aria-hidden="true" />
          {t("stackAdopt.footer.adopting")}
        </>
      ) : (
        t("stackAdopt.footer.adopt")
      )}
    </Button>
  );

  return (
    <div className="flex flex-col gap-6">
      <header className="flex flex-col gap-2">
        <Breadcrumb envId={envId} name={name} />
        <h2 className="line-clamp-2 min-w-0 break-all text-2xl font-semibold">
          {t("stackAdopt.page.title", { name })}
        </h2>
        <p className="max-w-prose text-sm text-muted-foreground">
          {t("stackAdopt.page.body", { env: envName })}
        </p>
      </header>

      {query.isError && !data ? (
        <Alert variant="destructive">
          <CircleX aria-hidden="true" />
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span className="font-medium">{t("stackAdopt.loadFailed.title", { name })}</span>
            <span>{t("stackAdopt.loadFailed.body")}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {t("stackAdopt.loadFailed.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}

      {data ? (
        <>
          {!data.adoptable && data.reason ? <NotAdoptableNote reason={data.reason} /> : null}
          {data.orphan ? <OrphanIdNote /> : null}
          {data.portainer ? <PortainerAdvice /> : null}
        </>
      ) : query.isError ? null : (
        <Skeleton className="h-16 w-full" aria-hidden="true" />
      )}
      <EnvValuesNotice />

      {refusal ? (
        <Refusal
          err={refusal}
          envId={envId}
          name={name}
          envName={envName}
          onCheck={() => {
            setRefusal(null);
            void query.refetch();
          }}
        />
      ) : null}

      <EditorFooter>
        <p className="me-auto min-w-0 text-sm break-words text-muted-foreground">
          {t("stackAdopt.footer.nothing", { env: envName })}
        </p>
        <Button asChild variant="outline" className="max-md:h-11">
          <Link to="/env/$envId/unmanaged-stacks/$name" params={{ envId, name }}>
            {t("stackAdopt.footer.cancel")}
          </Link>
        </Button>
        <NeedsTooltip reason={reason}>{adoptButton}</NeedsTooltip>
      </EditorFooter>
    </div>
  );
}

interface RefusalProps {
  err: AdoptRequestError;
  envId: string;
  name: string;
  envName: string;
  onCheck: () => void;
}

// Refusal is the alert under the page's notes that says why the server did not adopt the stack, with the one thing
// the person can do about it: look again, open the stack that already holds the name, or open the operation that
// holds the environment.
function Refusal({ err, envId, name, envName, onCheck }: RefusalProps) {
  const t = useT();
  const link =
    "rounded-sm text-accent-ink underline underline-offset-3 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none";
  const stackId = err.fieldOf("stackId");

  if (err.type === "operation_locked") {
    const job = err.fieldOf("job");
    const started = err.fieldOf("started");
    return (
      <Alert variant="warning" data-slot="operation-lock">
        <Lock aria-hidden="true" />
        <AlertDescription className="flex flex-wrap items-center gap-x-3 gap-y-1 text-status-warning-text">
          <span className="break-words">
            {t("stackConverge.lock.before", { env: envName, stack: err.fieldOf("stack") })}{" "}
            {started ? <RelativeTime value={started} /> : null}
            {t("stackConverge.lock.after")}
          </span>
          {stackId && job ? (
            <Link
              to="/env/$envId/stacks/$stackId/deploys/$jobId"
              params={{ envId, stackId, jobId: job }}
              className={link}
            >
              {t("stackConverge.lock.openIt")}
            </Link>
          ) : stackId ? (
            <Link to="/env/$envId/stacks/$stackId" params={{ envId, stackId }} className={link}>
              {t("stackConverge.lock.openIt")}
            </Link>
          ) : null}
        </AlertDescription>
      </Alert>
    );
  }

  return (
    <Alert variant="destructive" role="alert">
      <CircleX aria-hidden="true" />
      <AlertDescription className="flex flex-wrap items-center gap-3 break-words">
        <span>{refusalText(t, err, name, envName)}</span>
        {err.type === "adopt_stale" ? (
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            onClick={onCheck}
          >
            {t("stackAdopt.error.checkAgain")}
          </Button>
        ) : null}
        {err.type === "stack_already_managed" && stackId ? (
          <Button asChild variant="outline" size="sm" className="max-md:h-11">
            <Link to="/env/$envId/stacks/$stackId" params={{ envId, stackId }}>
              {t("stackAdopt.error.openStack")}
            </Link>
          </Button>
        ) : null}
      </AlertDescription>
    </Alert>
  );
}
