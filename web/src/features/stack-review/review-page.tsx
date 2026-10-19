// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { CircleX, Info } from "lucide-react";
import { useId, useState } from "react";

import {
  type DeployRequestError,
  type ReviewMode,
  type ReviewRequestError,
  reviewQuery,
  type StackReview,
  stackQuery,
  useDeployStack,
} from "@/api/stacks";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useEnvName } from "@/features/resources/use-env-name";
import { EditorFooter } from "@/features/service-editor/editor-footer";
import { BlockingList } from "@/features/stack-review/blocking-list";
import { ImagePinTable } from "@/features/stack-review/image-pin-table";
import { OtherResourcesTable } from "@/features/stack-review/other-resources-table";
import { ReviewSummary } from "@/features/stack-review/review-summary";
import { ServiceDiffList } from "@/features/stack-review/service-diff-list";
import { WarningsList } from "@/features/stack-review/warnings-list";
import { YamlDiff } from "@/features/stack-review/yaml-diff";
import { useT } from "@/i18n/use-t";
import { toastInfo } from "@/lib/toast";

type Translate = (key: string, options?: Record<string, unknown>) => string;

interface ReviewPageProps {
  envId: string;
  stackId: string;
  // What is reviewed: the saved draft (deploy), or a fresh digest for the named services (update).
  mode?: ReviewMode;
  services?: string[];
  // What to do once the server has started the deploy: the route replaces this page with the
  // deploy's console, so Back returns to the editor and not to a finished review.
  onDeployed: (jobId: string) => void;
}

// deployRefusal words a deploy the server refused. The server decides every refusal; this only
// says what it means in the page's words (T-06-60). A draft or digests that moved since the
// review are the same advice: look again.
function deployRefusal(t: Translate, err: DeployRequestError): string {
  switch (err.type) {
    case "draft_changed":
      return t("stackReview.deployErrors.draftChanged");
    case "pins_invalid":
      return t("stackReview.deployErrors.pinsInvalid");
    case "privileged_compose":
      return t("stackReview.deployErrors.needsAdmin");
    case "stack_name_clash":
      return t("stackReview.deployErrors.nameClash");
    case "target_changed":
    case "target_engine_missing":
      return t("stackReview.deployErrors.targetChanged");
    case "too_many_deploys":
      return t("stackReview.deployErrors.tooMany");
  }
  if (err.status === 0) return t("stackReview.deployErrors.lost");
  if (err.status === 403) return t("stackReview.deployErrors.permission");
  return t("stackReview.deployErrors.generic");
}

// listOf joins the kinds of host access into one phrase: "bind mounts and host networking".
function listOf(items: string[]): string {
  return new Intl.ListFormat("en", { style: "long", type: "conjunction" }).format(items);
}

// noChange says whether a deploy of the review would do nothing: no service to add, change or
// remove, and no network, secret or config but the external ones the stack only names.
function noChange(data: StackReview): boolean {
  return (
    (data.services ?? []).length === 0 && (data.objects ?? []).every((o) => o.change === "external")
  );
}

// deployReason is why Deploy stack is off, in the order that matters: host access the caller may
// not deploy, an image the registry cannot be asked about, errors, and a stack with nothing to
// change. Warnings are never a reason (D-10). Absent when a deploy may start. The server refuses
// every one of these again at the deploy; this only says why the button waits.
function deployReason(t: Translate, data: StackReview): string | undefined {
  const kinds = data.needsAdmin ?? [];
  if (kinds.length > 0) return t("stackReview.reasons.needsAdmin", { kinds: listOf(kinds) });
  const lost = (data.images ?? []).find(
    (i) => i.state === "unreachable" || i.state === "unresolved",
  );
  if (lost) return t("stackReview.reasons.lookup", { ref: lost.image });
  const errors = (data.problems ?? []).filter((p) => p.severity === "error").length;
  if (errors > 0) return t("stackReview.reasons.errors", { count: errors });
  if (noChange(data)) return t("stackReview.reasons.nothing");
  return undefined;
}

// loadRefusal words a review the server would not make.
function loadRefusal(t: Translate, err: Error | null): string {
  const type = (err as ReviewRequestError | null)?.type;
  if (type === "target_changed" || type === "target_engine_missing") {
    return t("stackReview.error.targetChanged");
  }
  return t("stackReview.error.body");
}

// ReviewPage is the second step of a deploy (STK-06, D-08, D-10, D-11): what the deploy would
// change against what is running, field by field, and the images it would run. Nothing deploys
// that the person did not see: Deploy stack sends the version of the draft and the digests this
// page was made of, and the server refuses a draft that moved since. The page is capped at
// 1152px; every value is text.
export function ReviewPage({
  envId,
  stackId,
  mode = "deploy",
  services = [],
  onDeployed,
}: ReviewPageProps) {
  const t = useT();
  const envName = useEnvName(envId);
  const stack = useQuery(stackQuery(envId, stackId));
  const review = useQuery(reviewQuery(envId, stackId, mode, services));
  const deploy$ = useDeployStack(envId, stackId);
  const [deployError, setDeployError] = useState<string | null>(null);
  const [tab, setTab] = useState<string | null>(null);
  const name = stack.data?.name;
  const data = review.data;
  const update = mode === "update";

  // Deploy sends what was reviewed and nothing else. The button is pending after the first click
  // and the request is never repeated (T-07-73).
  function deploy() {
    if (!data || deploy$.isPending || deploy$.isSuccess) return;
    setDeployError(null);
    deploy$.mutate(
      {
        draftVersion: data.draftVersion,
        pins: data.pins,
        mode,
        ...(update ? { services } : {}),
      },
      {
        onSuccess: (answer) => onDeployed(answer.jobId),
        onError: (err) => setDeployError(deployRefusal(t, err)),
      },
    );
  }

  const reason = data ? deployReason(t, data) : undefined;
  const blocked = !data || reason !== undefined;
  const starting = deploy$.isPending || deploy$.isSuccess;
  const reasonId = useId();
  const problems = data?.problems ?? [];
  const changed = data?.services ?? [];
  const unchanged = data !== undefined && noChange(data);
  const yaml = data?.yaml;
  const yamlChanged = yaml?.changedLines ?? 0;
  // Changes (against what is running) is the page's first tab, unless there is nothing to show
  // there and the text still differs from the last deploy.
  const shown = tab ?? (unchanged && yamlChanged > 0 ? "yaml" : "changes");
  const firstDeploy = changed.length > 0 && changed.every((s) => s.change === "added");
  const failed = review.isError && !data;

  // Pressing Deploy stack while it is off says why, for a touch screen that has no hover.
  function pressed() {
    if (reason) toastInfo(reason);
  }

  const deployButton = (
    <Button
      type="button"
      className="max-md:h-11"
      aria-disabled={blocked || starting ? true : undefined}
      aria-describedby={reason ? reasonId : undefined}
      onClick={blocked || starting ? pressed : deploy}
    >
      {starting ? (
        <>
          <Spinner aria-hidden="true" />
          {t("stackReview.footer.starting")}
        </>
      ) : (
        t("stackReview.footer.deploy")
      )}
    </Button>
  );

  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-6">
      <div className="flex flex-col gap-1">
        {name === undefined ? (
          <Skeleton className="h-7 w-64" />
        ) : (
          <h3 className="text-xl font-semibold break-all">
            {update ? t("stackReview.titleUpdate", { name }) : t("stackReview.title", { name })}
          </h3>
        )}
        <p className="text-sm text-muted-foreground">
          {update ? t("stackReview.bodyUpdate") : t("stackReview.body", { env: envName })}
        </p>
        {data || failed ? null : (
          <p role="status" className="text-sm text-muted-foreground">
            {t("stackReview.loading")}
          </p>
        )}
      </div>
      {failed ? (
        <Alert variant="destructive">
          <CircleX aria-hidden="true" />
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("stackReview.error.title")}</span>
            <span>{loadRefusal(t, review.error)}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={review.isFetching}
              onClick={() => void review.refetch()}
            >
              {t("stackReview.error.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : (
        <>
          <BlockingList problems={problems} envId={envId} stackId={stackId} />
          <WarningsList problems={problems} envId={envId} stackId={stackId} />
          {firstDeploy ? (
            <Alert role="status">
              <Info aria-hidden="true" />
              <AlertTitle>{t("stackReview.firstDeploy.title")}</AlertTitle>
              <AlertDescription>{t("stackReview.firstDeploy.body")}</AlertDescription>
            </Alert>
          ) : null}
          <ReviewSummary summary={data?.summary} />
          <ImagePinTable images={data ? (data.images ?? []) : undefined} mode={mode} />
          {unchanged ? (
            <Empty className="border">
              <EmptyHeader>
                <EmptyTitle role="heading" aria-level={3}>
                  {t("stackReview.noChanges.title")}
                </EmptyTitle>
                <EmptyDescription>
                  {update ? t("stackReview.noChanges.bodyUpdate") : t("stackReview.noChanges.body")}
                </EmptyDescription>
              </EmptyHeader>
            </Empty>
          ) : null}
          {data && (!unchanged || yamlChanged > 0) ? (
            <Tabs value={shown} onValueChange={setTab} className="gap-4">
              <TabsList variant="line" aria-label={t("stackReview.changes.label")} className="h-9">
                <TabsTrigger value="changes" className="max-md:h-11">
                  {t("stackReview.changes.tab")}
                </TabsTrigger>
                <TabsTrigger value="yaml" className="max-md:h-11">
                  {yamlChanged > 0
                    ? t("stackYamlDiff.tabCount", { count: yamlChanged })
                    : t("stackYamlDiff.tab")}
                </TabsTrigger>
              </TabsList>
              <TabsContent value="changes" className="flex flex-col gap-4">
                <ServiceDiffList services={changed} />
                <OtherResourcesTable objects={data.objects ?? []} />
              </TabsContent>
              <TabsContent value="yaml">
                <YamlDiff
                  hunks={yaml?.hunks}
                  lines={yaml?.lines}
                  caption={t("stackYamlDiff.caption")}
                />
              </TabsContent>
            </Tabs>
          ) : null}
        </>
      )}
      {deployError ? (
        <Alert variant="destructive">
          <CircleX aria-hidden="true" />
          <AlertDescription className="break-words">{deployError}</AlertDescription>
        </Alert>
      ) : null}
      <EditorFooter>
        {reason ? (
          <p id={reasonId} className="me-auto min-w-0 text-sm break-words text-muted-foreground">
            {reason}
          </p>
        ) : null}
        <Button asChild variant="outline" className="max-md:h-11">
          <Link to="/env/$envId/stacks/$stackId/editor" params={{ envId, stackId }}>
            {t("stackReview.footer.back")}
          </Link>
        </Button>
        {reason ? (
          <Tooltip>
            <TooltipTrigger asChild>{deployButton}</TooltipTrigger>
            <TooltipContent>{reason}</TooltipContent>
          </Tooltip>
        ) : (
          deployButton
        )}
      </EditorFooter>
    </div>
  );
}
