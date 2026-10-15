// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link, useBlocker, useNavigate } from "@tanstack/react-router";
import { Boxes, CircleX } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import {
  isServiceGone,
  type ServiceActionError,
  type ServiceReview,
  type ServiceSpec,
  serviceSpecQuery,
  useApplyServiceUpdate,
  useReviewServiceUpdate,
} from "@/api/services";
import { AgentUpdateNotice } from "@/components/agent-update-notice";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
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
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { useEnvName } from "@/features/resources/use-env-name";
import { ApplyProgress } from "@/features/service-editor/apply-progress";
import { editRefusalOf } from "@/features/service-editor/edit-refusal";
import { EditorFooter } from "@/features/service-editor/editor-footer";
import { ReviewStep } from "@/features/service-editor/review-step";
import { ServiceForm } from "@/features/service-editor/service-form";
import { imageOf } from "@/features/service-editor/spec-paths";
import { StackOwnedState } from "@/features/service-editor/stack-owned-state";
import { SpecEditor } from "@/features/spec-editor/spec-editor";
import type { SpecValue } from "@/features/spec-editor/spec-format";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

export type EditorTab = "form" | "spec";

interface ServiceEditorPageProps {
  envId: string;
  serviceId: string;
  // The tab the address names, and what to do to change it. The Form is the default.
  tab?: EditorTab;
  onTabChange?: (tab: EditorTab) => void;
}

type Stage = "edit" | "review" | "apply";

function Breadcrumb({
  envId,
  serviceId,
  name,
}: {
  envId: string;
  serviceId: string;
  name: string | undefined;
}) {
  const t = useT();
  return (
    <nav aria-label={t("specEditor.page.breadcrumb.label")}>
      <ol className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground [&>li:not(:last-child)]:after:ms-2 [&>li:not(:last-child)]:after:content-['/']">
        <li>
          <Link
            to="/env/$envId/services"
            params={{ envId }}
            className="rounded-sm text-accent-ink hover:underline"
          >
            {t("specEditor.page.breadcrumb.services")}
          </Link>
        </li>
        <li className="min-w-0">
          {name === undefined ? (
            <Skeleton className="h-3 w-24" />
          ) : (
            <Link
              to="/env/$envId/services/$serviceId"
              params={{ envId, serviceId }}
              className="rounded-sm break-all text-accent-ink hover:underline"
            >
              {name}
            </Link>
          )}
        </li>
        <li aria-current="page">{t("specEditor.page.breadcrumb.edit")}</li>
      </ol>
    </nav>
  );
}

// The footer of the editing state: the way out, and Review changes, which waits for the spec to
// be loaded and to parse, and reads Checking… while the server judges it.
function EditFooter({
  envId,
  serviceId,
  ready,
  checking,
  onReview,
}: {
  envId: string;
  serviceId: string;
  ready: boolean;
  checking: boolean;
  onReview: () => void;
}) {
  const t = useT();
  return (
    <EditorFooter>
      <Button asChild variant="outline" className="max-md:h-11">
        <Link to="/env/$envId/services/$serviceId" params={{ envId, serviceId }}>
          {t("specEditor.page.cancel")}
        </Link>
      </Button>
      <Button
        type="button"
        className="max-md:h-11"
        disabled={!ready}
        aria-disabled={checking ? true : undefined}
        onClick={checking ? undefined : onReview}
      >
        {checking ? (
          <>
            <Spinner aria-hidden="true" />
            {t("specEditor.page.checking")}
          </>
        ) : (
          t("specEditor.page.review")
        )}
      </Button>
    </EditorFooter>
  );
}

// ServiceEditorPage is the page that replaces a service's page while its settings are changed
// (D-01, D-03): a header, the editor, and a footer, in one of three states that live in memory. In
// the first the Spec tab holds the whole service spec as Docker states it. Review changes sends
// the version it loaded and the spec as parsed, and the server's answer is the second state, the
// difference and what stops or qualifies an apply. Apply changes sends the version and the spec
// the person typed (never the review's masked copy of it) once, and the third state follows the
// update. The server decides every refusal; this words it. Leaving with changes asks first.
export function ServiceEditorPage({
  envId,
  serviceId,
  tab = "form",
  onTabChange,
}: ServiceEditorPageProps) {
  const t = useT();
  const navigate = useNavigate();
  const environment = useEnvName(envId);
  const specQuery = useQuery({
    ...serviceSpecQuery(envId, serviceId),
    refetchOnWindowFocus: false,
  });
  // The spec as it was first read. The events feed reads the service again as it changes, and a
  // spec that changed under the editor would otherwise change the version it sends.
  const [pinned, setPinned] = useState<ServiceSpec>();
  const data = pinned ?? specQuery.data;
  useEffect(() => {
    if (!pinned && specQuery.data) setPinned(specQuery.data);
  }, [pinned, specQuery.data]);
  useDocumentTitle(data?.name);

  const reviewMutation = useReviewServiceUpdate(envId);
  const applyMutation = useApplyServiceUpdate(envId);
  const [stage, setStage] = useState<Stage>("edit");
  const [edited, setEdited] = useState<SpecValue | null>(null);
  const [valid, setValid] = useState(true);
  const [review, setReview] = useState<ServiceReview>();
  const [editError, setEditError] = useState<ServiceActionError | null>(null);
  const [editorKey, setEditorKey] = useState(0);
  // Whether every field of the Form can be written. Only the Form tab has fields.
  const [formValid, setFormValid] = useState(true);
  // The Spec tab opened on the Form's spec cannot show later Form edits, so it is started afresh
  // each time it is opened.
  const [specOpened, setSpecOpened] = useState(0);
  // The switch to the Form was refused because the spec does not parse.
  const [refused, setRefused] = useState(false);
  const refusedRef = useRef<HTMLParagraphElement>(null);
  const turn = useRef(0);

  const current = (data?.spec ?? {}) as SpecValue;
  const spec = edited ?? current;
  const changed = useMemo(
    () => data !== undefined && JSON.stringify(spec) !== JSON.stringify(current),
    [data, spec, current],
  );
  // Changes are lost on leaving while editing or reviewing; once applying, there is nothing to lose.
  const dirty = data !== undefined && stage !== "apply" && (!valid || changed);
  const dirtyRef = useRef(dirty);
  dirtyRef.current = dirty;
  const blocker = useBlocker({
    // Changing the tab changes the address but not the page: only leaving it asks.
    shouldBlockFn: ({ current, next }) => dirtyRef.current && current.pathname !== next.pathname,
    enableBeforeUnload: () => dirtyRef.current,
    withResolver: true,
  });

  const onChange = useCallback((value: SpecValue) => setEdited(value), []);

  // A refused switch puts the message where a person who cannot see it is told, and keeps it
  // there until the spec parses again or the Spec tab is left.
  useEffect(() => {
    if (refused) refusedRef.current?.focus();
  }, [refused]);
  useEffect(() => {
    if (valid) setRefused(false);
  }, [valid]);

  function showTab(next: EditorTab) {
    if (next === tab) return;
    if (next === "form" && !valid) {
      setRefused(true);
      return;
    }
    setRefused(false);
    if (next === "spec") setSpecOpened((k) => k + 1);
    onTabChange?.(next);
  }

  // The review of the spec as it is, or of one the review step has just changed (the auto-rollback
  // box): that one replaces a review still on its way.
  function onReview(changed?: SpecValue) {
    if (!data || !valid || (changed === undefined && reviewMutation.isPending)) return;
    const target = changed ?? spec;
    const mine = ++turn.current;
    setEditError(null);
    setReview(undefined);
    // A new image is looked up, which can take a while: go to the review now, with the lookup
    // showing. Otherwise the button reads Checking… until the server has answered.
    if (imageOf(target) !== imageOf(current)) setStage("review");
    reviewMutation.mutate(
      { serviceId, version: data.version, spec: target },
      {
        onSuccess: (answer) => {
          if (mine !== turn.current) return;
          setReview(answer);
          setStage("review");
        },
        onError: (err) => {
          if (mine !== turn.current) return;
          setEditError(err);
          setStage("edit");
        },
      },
    );
  }

  // The auto-rollback box on the review step changed the spec: the Form and the Spec tab hold the
  // new one, and the difference is worked out again for it.
  function onReviewSpecChange(next: SpecValue) {
    setEdited(next);
    onReview(next);
  }

  function backToEditing() {
    turn.current++;
    applyMutation.reset();
    setReview(undefined);
    setStage("edit");
  }

  function onApply() {
    if (!data || applyMutation.isPending) return;
    setStage("apply");
    applyMutation.mutate(
      { serviceId, version: data.version, spec },
      { onSuccess: () => toastSuccess(t("specEditor.apply.toast", { name: data.name })) },
    );
  }

  async function reload() {
    turn.current++;
    applyMutation.reset();
    reviewMutation.reset();
    const fresh = await specQuery.refetch();
    if (!fresh.data) return;
    setPinned(fresh.data);
    setEdited(null);
    setValid(true);
    setReview(undefined);
    setEditError(null);
    setEditorKey((k) => k + 1);
    setRefused(false);
    setStage("edit");
  }

  const toService = () =>
    void navigate({ to: "/env/$envId/services/$serviceId", params: { envId, serviceId } });

  const discardGuard = (
    <AlertDialog
      open={blocker.status === "blocked"}
      onOpenChange={(open) => {
        if (!open) blocker.reset?.();
      }}
    >
      <AlertDialogContent className="sm:max-w-120">
        <AlertDialogHeader>
          <AlertDialogTitle>{t("specEditor.discard.title")}</AlertDialogTitle>
          <AlertDialogDescription>{t("specEditor.discard.body")}</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel autoFocus>{t("specEditor.discard.keep")}</AlertDialogCancel>
          <AlertDialogAction onClick={() => blocker.proceed?.()}>
            {t("specEditor.discard.discard")}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );

  if (isServiceGone(specQuery.error)) {
    return (
      <div className="flex flex-col gap-6">
        <header>
          <Breadcrumb envId={envId} serviceId={serviceId} name={data?.name} />
        </header>
        <Empty>
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <Boxes aria-hidden="true" />
            </EmptyMedia>
            <EmptyTitle role="heading" aria-level={2}>
              {t("servicedetail.gone.heading")}
            </EmptyTitle>
            <EmptyDescription>{t("servicedetail.gone.body")}</EmptyDescription>
          </EmptyHeader>
          <EmptyContent>
            <Button asChild variant="outline" className="max-md:h-11">
              <Link to="/env/$envId/services" params={{ envId }}>
                {t("servicedetail.gone.back")}
              </Link>
            </Button>
          </EmptyContent>
        </Empty>
      </div>
    );
  }

  if (specQuery.isError && !data) {
    return (
      <div className="flex flex-col gap-6">
        <header>
          <Breadcrumb envId={envId} serviceId={serviceId} name={undefined} />
        </header>
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("servicedetail.error.loadFailed", { name: serviceId })}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={specQuery.isFetching}
              onClick={() => void specQuery.refetch()}
            >
              {t("servicedetail.error.retry", { name: serviceId })}
            </Button>
          </AlertDescription>
        </Alert>
      </div>
    );
  }

  if (!data) {
    return (
      <div className="flex flex-col gap-6">
        <header className="flex flex-col gap-2">
          <Breadcrumb envId={envId} serviceId={serviceId} name={undefined} />
          <Skeleton className="h-8 w-64" />
        </header>
        <div
          role="status"
          aria-busy="true"
          aria-label={t("specEditor.page.loading")}
          className="flex flex-col gap-3"
        >
          <Skeleton className="h-8 w-48" />
          <Skeleton className="h-80 w-full" />
        </div>
        <EditFooter
          envId={envId}
          serviceId={serviceId}
          ready={false}
          checking={false}
          onReview={() => {}}
        />
      </div>
    );
  }

  if (data.stack) {
    return (
      <div className="flex flex-col gap-6">
        <header>
          <Breadcrumb envId={envId} serviceId={serviceId} name={data.name} />
        </header>
        <StackOwnedState
          envId={envId}
          serviceId={serviceId}
          name={data.name}
          stack={data.stack}
          spec={current}
        />
      </div>
    );
  }

  const refusal = editError
    ? editRefusalOf(t, editError, { environment, image: imageOf(spec) })
    : undefined;

  return (
    <div className="flex flex-col gap-6">
      <header className="flex flex-col gap-2">
        <Breadcrumb envId={envId} serviceId={serviceId} name={data.name} />
        <h1 className="line-clamp-2 text-2xl font-semibold break-all">
          {t("specEditor.page.title", { name: data.name })}
        </h1>
        <p className="text-sm text-muted-foreground">{t("specEditor.page.description")}</p>
      </header>
      {/* The editor stays in the page while the review or the apply shows, so its text is
          still there when a person goes back to it. */}
      <div hidden={stage !== "edit"} className="flex flex-col gap-4">
        <Tabs value={tab} onValueChange={(next) => showTab(next as EditorTab)}>
          <TabsList aria-label={t("serviceForm.tabs.label")} className="h-10">
            <TabsTrigger value="form" className="px-4 max-md:min-h-11">
              {t("serviceForm.tabs.form")}
            </TabsTrigger>
            <TabsTrigger value="spec" className="px-4 max-md:min-h-11">
              {t("serviceForm.tabs.spec")}
            </TabsTrigger>
          </TabsList>
          {refused ? (
            <p
              ref={refusedRef}
              tabIndex={-1}
              role="alert"
              className="text-sm text-destructive outline-none"
            >
              {t("serviceForm.tabs.refused")}
            </p>
          ) : null}
          <TabsContent value="form" className="pt-2">
            <ServiceForm
              key={editorKey}
              spec={spec}
              mode="edit"
              envId={envId}
              onSpecChange={onChange}
              onValidityChange={setFormValid}
              onOpenSpec={() => showTab("spec")}
            />
          </TabsContent>
          <TabsContent value="spec" className="pt-2">
            <SpecEditor
              key={`${editorKey}-${specOpened}`}
              initial={current}
              start={spec}
              onChange={onChange}
              onValidityChange={setValid}
            />
          </TabsContent>
        </Tabs>
        {refusal ? (
          <>
            {refusal.agentPolicy ? (
              <AgentUpdateNotice envId={envId} />
            ) : (
              <Alert variant="destructive">
                <CircleX aria-hidden="true" />
                <AlertDescription className="break-words">{refusal.message}</AlertDescription>
              </Alert>
            )}
            {refusal.reload ? (
              <div>
                <Button type="button" className="max-md:h-11" onClick={() => void reload()}>
                  {t("specEditor.page.reload")}
                </Button>
              </div>
            ) : null}
          </>
        ) : null}
        <EditFooter
          envId={envId}
          serviceId={serviceId}
          ready={valid && formValid}
          checking={reviewMutation.isPending}
          onReview={() => onReview()}
        />
      </div>
      {stage === "review" ? (
        <ReviewStep
          envId={envId}
          name={data.name}
          current={current}
          edited={spec}
          review={review}
          applying={applyMutation.isPending}
          onBack={backToEditing}
          onApply={onApply}
          onSpecChange={onReviewSpecChange}
        />
      ) : null}
      {stage === "apply" ? (
        <ApplyProgress
          envId={envId}
          serviceId={serviceId}
          name={data.name}
          status={
            applyMutation.isPending ? "pending" : applyMutation.isSuccess ? "success" : "error"
          }
          error={applyMutation.error ?? undefined}
          lookup={review?.image.changed ?? false}
          onBack={backToEditing}
          onClose={toService}
          onReload={() => void reload()}
        />
      ) : null}
      {discardGuard}
    </div>
  );
}
