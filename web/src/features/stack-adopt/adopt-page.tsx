// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { CircleX, Lock, SquareStack } from "lucide-react";
import { useEffect, useMemo, useState } from "react";

import {
  AdoptRequestError,
  adoptCheckQuery,
  MAX_PASTED_BYTES,
  type PastedFile,
  useAdoptStack,
  usePastedCheck,
} from "@/api/stack-adopt";
import type { StackVariable } from "@/api/stacks";
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
import { AdoptCheckCard } from "@/features/stack-adopt/adopt-check-card";
import { AdoptSource, type AdoptSourceKind } from "@/features/stack-adopt/adopt-source";
import { EnvValuesNotice } from "@/features/stack-adopt/env-values-notice";
import { OwnershipTable } from "@/features/stack-adopt/ownership-table";
import { PastedFileCheck } from "@/features/stack-adopt/pasted-file-check";
import { type PastedValue, PastedVariables } from "@/features/stack-adopt/pasted-variables";
import { PolicyHits } from "@/features/stack-adopt/policy-hits";
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
    case "pasted_file_invalid":
      return t("stackAdoptPaste.error.invalid", { reason: err.detail });
    case "secret_too_short":
      return t("stackAdoptPaste.error.secretShort");
  }
  if (err.status === 0) return t("stackAdopt.error.lost");
  if (err.status === 403) return t("stackAdopt.error.permission", { env: envName });
  return t("common.mutationFailed");
}

// The refusals of the check that are not a failure to check: the page words them as it words the adoption's own, or
// leaves them to the notes it already shows.
const CHECK_REFUSALS = [
  "stack_already_managed",
  "stack_not_adoptable",
  "unmanaged_stack_not_found",
];

// AdoptPage is the page an Env Admin adopts an unmanaged stack from (UI-SPEC "Adopt page", STK-13, D-14, D-20): what
// adopting does, the note about environment values, the check of what the first deploy would change, and a footer
// with Cancel and Adopt stack. The check runs on entry and changes nothing. Pressing Adopt sends the fingerprint of
// the check the person saw and nothing else: the server rebuilds the definition itself, stores the stack with its
// first revision, and sends nothing to the engine. It is never retried by itself. When what runs has changed since
// the check, the server refuses and the page offers Check again, which runs the check again.
export function AdoptPage(props: AdoptPageProps) {
  // Another stack is another page: the pasted file and the values typed for it are never carried over to it.
  return <AdoptPageBody key={`${props.envId}/${props.name}`} {...props} />;
}

function AdoptPageBody({ envId, name, onAdopted }: AdoptPageProps) {
  const t = useT();
  const envName = useEnvName(envId);
  const needs = useNeeds("stacks.create", envId);
  const query = useQuery(unmanagedStackQuery(envId, name));
  const check = useQuery(adoptCheckQuery(envId, name));
  const adopt = useAdoptStack(envId, name);
  const [refusal, setRefusal] = useState<AdoptRequestError | null>(null);
  // The original compose file, when the person has it (D-12). The text and the values typed for its variables live
  // here and nowhere else: not in a query, a storage, the URL or a toast. They go with the page, on Cancel, on a route
  // change and on unmount, and are cleared once the stack is stored.
  const [source, setSource] = useState<AdoptSourceKind>("rebuild");
  const [pastedText, setPastedText] = useState("");
  const [pastedValues, setPastedValues] = useState<Record<string, PastedValue>>({});
  const [showToken, setShowToken] = useState(0);
  // True once the stack is stored: the lists are read again and this name is managed now, so the page must not
  // turn into the gone state while the route takes the person to the new stack.
  const [stored, setStored] = useState(false);
  const { data } = query;
  const paste = source === "paste";
  const hasText = pastedText.trim() !== "" && pastedText.length <= MAX_PASTED_BYTES;
  // The variables the file uses are those its last settled check named; they stay listed while the next check runs, so
  // a field being typed in does not vanish under the person. A value typed for a name the file no longer uses is
  // not sent.
  const [known, setKnown] = useState<StackVariable[]>([]);
  const usedNames = useMemo(
    () =>
      known
        .filter((v) => !v.unused)
        .map((v) => v.name)
        .join("\u0000"),
    [known],
  );
  const varsKey = useMemo(() => {
    const names = new Set(usedNames.split("\u0000"));
    return JSON.stringify(
      Object.entries(pastedValues)
        .filter(([n, v]) => names.has(n) && v.value !== "")
        .sort(([a], [b]) => (a < b ? -1 : 1))
        .map(([n, v]) => ({ name: n, value: v.value, secret: v.secret })),
    );
  }, [usedNames, pastedValues]);
  // The input keeps its identity while its content is the same: a check belongs to the input it was made for.
  const pastedInput = useMemo<PastedFile | null>(
    () =>
      paste && hasText
        ? { compose: pastedText, vars: JSON.parse(varsKey) as PastedFile["vars"] }
        : null,
    [paste, hasText, pastedText, varsKey],
  );
  const pastedCheck = usePastedCheck(envId, name, pastedInput);
  const settledVariables = pastedCheck.data?.variables;
  useEffect(() => {
    if (settledVariables) setKnown(settledVariables);
  }, [settledVariables]);
  const checkError =
    (paste ? pastedCheck.error : check.error) instanceof AdoptRequestError
      ? ((paste ? pastedCheck.error : check.error) as AdoptRequestError)
      : null;
  // The check that Adopt is sent with: the pasted file's when the person has it, else the rebuild's.
  const checkData = paste ? pastedCheck.data : check.data;
  const checking = paste ? pastedCheck.checking : check.isFetching;
  const gone =
    !stored && (isUnmanagedGone(query.error) || checkError?.type === "unmanaged_stack_not_found");
  useDocumentTitle(t("stackAdopt.page.title", { name }));

  async function onAdopt() {
    if (!data || !checkData || checking || adopt.isPending) return;
    setRefusal(null);
    try {
      // The fingerprint of the check the person saw, not of the page's own read, which polls. A pasted file goes as
      // text, to be checked again by the server; the browser never sends what it concluded about it.
      const made = await adopt.mutateAsync({
        fingerprint: checkData.fingerprint,
        pasted: paste && pastedInput ? pastedInput : undefined,
      });
      // The file and the values typed for it are gone from the page the moment the stack is stored.
      setPastedText("");
      setPastedValues({});
      setKnown([]);
      adopt.reset();
      setStored(true);
      toastSuccess(t("stackAdopt.toast.adopted", { name, env: envName }));
      onAdopted(made.id);
    } catch (e) {
      setRefusal(e instanceof AdoptRequestError ? e : new AdoptRequestError(0));
    }
  }

  // Why Adopt stack is off, in the order that matters: the role it takes, a stack that cannot be adopted, and a
  // stack the page has not read yet. Absent when the person may adopt.
  // For a pasted file the reasons go on: nothing pasted, a check still to come, and a file with nothing to adopt yet.
  const requiredCount = (pastedCheck.data?.variables ?? []).filter(
    (v) => v.required && !v.unused,
  ).length;
  let reason: string | undefined;
  if (!needs.allowed) reason = needs.reason;
  else if (data && !data.adoptable) reason = data.reason;
  else if (!data) reason = t("stackAdopt.checking", { name });
  else if (paste && !pastedInput) reason = t("stackAdoptPaste.off.empty");
  else if (checking) reason = t("stackAdopt.checking", { name });
  else if (!checkData) reason = t("stackAdoptCheck.off.failed", { name });
  else if (paste && checkData.blocked) {
    reason =
      requiredCount > 0
        ? t("stackAdoptPaste.off.fill", { count: requiredCount })
        : t("stackAdoptPaste.off.errors");
  }
  const off = reason !== undefined;
  // The notes under the title come from the newest check there is, the pasted file's or the rebuild's.
  const notes = (paste ? pastedCheck.data : undefined) ?? check.data;
  const checkRefusal = checkError?.type === "stack_already_managed" ? checkError : null;
  const checkFailure =
    checkError && !CHECK_REFUSALS.includes(checkError.type ?? "") ? checkError : null;

  function checkAgain() {
    setRefusal(null);
    void query.refetch();
    if (paste) pastedCheck.retry();
    else void check.refetch();
  }

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
          {data.orphan || notes?.orphan ? <OrphanIdNote /> : null}
          {data.portainer || notes?.portainer ? <PortainerAdvice /> : null}
        </>
      ) : query.isError ? null : (
        <Skeleton className="h-16 w-full" aria-hidden="true" />
      )}
      <PolicyHits entries={(notes?.policy.entries ?? []).map((e) => e.text)} />
      <EnvValuesNotice />

      <AdoptSource
        source={source}
        onSourceChange={(next) => {
          setSource(next);
          setRefusal(null);
        }}
      >
        {paste ? (
          <>
            <PastedFileCheck
              value={pastedText}
              onChange={setPastedText}
              check={pastedCheck.data}
              checking={pastedCheck.checking}
              onUseRebuild={() => setSource("rebuild")}
              onShowDifferences={() => setShowToken((n) => n + 1)}
            />
            <PastedVariables
              variables={known}
              values={pastedValues}
              onChange={(n, next) => setPastedValues((all) => ({ ...all, [n]: next }))}
              settled={!!pastedCheck.data && !pastedCheck.checking}
            />
          </>
        ) : null}
      </AdoptSource>

      <AdoptCheckCard
        name={name}
        target={data?.target === "standalone" ? "standalone" : "swarm"}
        check={checkData}
        checking={checking}
        error={checkFailure}
        onRetry={checkAgain}
        idle={paste && !pastedInput ? t("stackAdoptPaste.card.idle") : undefined}
        showToken={showToken}
      />
      <OwnershipTable name={name} check={checkData} checking={checking} />

      {refusal || checkRefusal ? (
        <Refusal
          err={refusal ?? (checkRefusal as AdoptRequestError)}
          envId={envId}
          name={name}
          envName={envName}
          onCheck={checkAgain}
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
