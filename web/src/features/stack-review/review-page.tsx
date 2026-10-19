// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { CircleX } from "lucide-react";
import { useState } from "react";

import {
  type DeployRequestError,
  type ReviewMode,
  reviewQuery,
  stackQuery,
  useDeployStack,
} from "@/api/stacks";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { useEnvName } from "@/features/resources/use-env-name";
import { EditorFooter } from "@/features/service-editor/editor-footer";
import { ImagePinTable } from "@/features/stack-review/image-pin-table";
import { ReviewSummary } from "@/features/stack-review/review-summary";
import { useT } from "@/i18n/use-t";

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

  const blocked = !data;
  const starting = deploy$.isPending || deploy$.isSuccess;

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
        {data ? null : (
          <p role="status" className="text-sm text-muted-foreground">
            {t("stackReview.loading")}
          </p>
        )}
      </div>
      <ReviewSummary summary={data?.summary} />
      <ImagePinTable images={data ? (data.images ?? []) : undefined} mode={mode} />
      {deployError ? (
        <Alert variant="destructive">
          <CircleX aria-hidden="true" />
          <AlertDescription className="break-words">{deployError}</AlertDescription>
        </Alert>
      ) : null}
      <EditorFooter>
        <Button asChild variant="outline" className="max-md:h-11">
          <Link to="/env/$envId/stacks/$stackId/editor" params={{ envId, stackId }}>
            {t("stackReview.footer.back")}
          </Link>
        </Button>
        <Button
          type="button"
          className="max-md:h-11"
          aria-disabled={blocked || starting ? true : undefined}
          onClick={blocked || starting ? undefined : deploy}
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
      </EditorFooter>
    </div>
  );
}
