// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Info, TriangleAlert } from "lucide-react";
import { useId, useMemo } from "react";
import type { components } from "@/api/schema";
import type { ServiceReview } from "@/api/services";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { AutoRollbackBox } from "@/features/service-editor/cards/auto-rollback-box";
import { EditorFooter } from "@/features/service-editor/editor-footer";
import { NewObjectsCard } from "@/features/service-editor/new-objects-card";
import { imageOf } from "@/features/service-editor/spec-paths";
import {
  objectsForApply,
  type StagedStore,
  useStagedObjects,
} from "@/features/service-editor/staged";
import { withTypedValues } from "@/features/service-editor/typed-values";
import { autoRollbackState, setAutoRollback } from "@/features/service-editor/update-policy";
import { diffSpecs } from "@/features/spec-editor/diff";
import { SpecDiff } from "@/features/spec-editor/spec-diff";
import type { SpecValue } from "@/features/spec-editor/spec-format";
import { useT } from "@/i18n/use-t";

type Translate = (key: string, options?: Record<string, unknown>) => string;
type Finding = components["schemas"]["ServiceFindingBody"];

interface ReviewStepProps {
  envId: string;
  // Whether the service already exists. A create has nothing to compare with: the table lists what
  // will be made, and the confirmation reads Create service.
  mode?: "edit" | "create";
  // The service's name. Text from Docker, drawn as text.
  name: string;
  // The spec as it was loaded, masked as the server sent it.
  current: SpecValue;
  // What the editor holds. While the review has not answered, the difference is read from it.
  edited: SpecValue;
  // The server's review; absent while the image's digest is being looked up.
  review?: ServiceReview;
  // True while the apply request is out.
  applying: boolean;
  // The objects staged in the editor, and where the values of the missing ones are typed.
  staged: StagedStore;
  onBack: () => void;
  onApply: () => void;
  // The edited spec with a change made on this step (the auto-rollback box). The page keeps it as
  // the editor's spec and asks for a fresh review of it.
  onSpecChange: (spec: SpecValue) => void;
}

// findingText words a host-escalation setting for the list in the refusal: what the setting is,
// in the words of the card it would be set on, from the rule that found it.
function findingText(t: Translate, f: Finding): string {
  const known = [
    "bind_path",
    "volume_device",
    "npipe",
    "cap_add",
    "selinux",
    "seccomp",
    "apparmor",
    "host_network",
    "plugin_runtime",
  ];
  if (known.includes(f.rule)) {
    return t(`specEditor.review.findings.${f.rule}`, { value: f.value });
  }
  return t("specEditor.review.findings.other", { path: f.path, value: f.value });
}

interface ImageRowProps {
  // The image the service runs now; absent for a service that does not exist yet.
  current?: string;
  // The image the spec will carry; absent while its digest is looked up.
  next?: string;
  credential?: string;
}

// ImageRow is the image as it is and as it will be, with the digest the tag resolved to. While the
// digest is being looked up the new side is a skeleton line and says so.
function ImageRow({ current, next, credential }: ImageRowProps) {
  const t = useT();
  return (
    // biome-ignore lint/a11y/useSemanticElements: a labelled group of two values, not a form field set
    <div
      role="group"
      aria-label={t("specEditor.review.image.title")}
      className="rounded-lg border bg-card p-3 text-sm"
    >
      <dl className="grid gap-x-4 gap-y-2 md:grid-cols-[auto_1fr]">
        {current === undefined ? null : (
          <>
            <dt className="text-xs font-semibold text-muted-foreground">
              {t("specEditor.review.image.current")}
            </dt>
            <dd className="font-mono text-xs break-all">{current}</dd>
          </>
        )}
        <dt className="text-xs font-semibold text-muted-foreground">
          {current === undefined
            ? t("specEditor.review.image.created")
            : t("specEditor.review.image.next")}
        </dt>
        <dd className="font-mono text-xs break-all">
          {next === undefined ? (
            <span className="flex flex-col gap-1">
              <Skeleton className="h-4 w-full max-w-md" />
              <span className="font-sans text-muted-foreground">
                {t("specEditor.review.image.lookingUp")}
              </span>
            </span>
          ) : (
            next
          )}
        </dd>
      </dl>
      {credential ? (
        <p className="mt-2 text-xs text-muted-foreground">
          {t("specEditor.review.image.credential", { name: credential })}
        </p>
      ) : null}
    </div>
  );
}

// ReviewStep is the second state of the editor (D-03): the field-level difference between the
// spec as it was loaded and the one the server judged, grouped under the cards of the form, with
// the image and its digest above it and, above that, whatever the server said that stops or
// qualifies an apply, and the auto-rollback box, which is the Update policy card's own field
// (D-12 amended): changing it here hands the page the spec with the choice applied. The server
// decides every refusal; this only words it (T-06-60). Apply is
// unavailable, with the reason, while there is nothing to apply, while the caller may not have the
// settings in the spec, while the host is read-only, while the image cannot be found, and while a
// secret or config the spec names that does not exist has no value (the New objects card asks for it).
export function ReviewStep({
  mode = "edit",
  name,
  current,
  edited,
  review,
  applying,
  staged,
  onBack,
  onApply,
  onSpecChange,
}: ReviewStepProps) {
  const t = useT();
  const create = mode === "create";
  // Re-judge when a value is typed.
  useStagedObjects(staged);
  const rollbackId = useId();
  const next = useMemo(
    () => (review ? withTypedValues(review.spec, edited) : edited),
    [review, edited],
  );
  const rows = useMemo(() => diffSpecs(current, next), [current, next]);
  const currentImage = imageOf(current);
  const nextImage = review ? review.image.ref : imageOf(edited);
  const imageChanged = review ? review.image.changed : currentImage !== nextImage;

  const findings = review?.findings ?? [];
  const missing = {
    networks: review?.missing.networks ?? [],
    secrets: review?.missing.secrets ?? [],
    configs: review?.missing.configs ?? [],
  };
  const missingCount = missing.networks.length + missing.secrets.length + missing.configs.length;
  const lacking = review ? objectsForApply(missing, staged).lacking : 0;
  const refusalList = review
    ? findings.length > 0
      ? findings.map((f) => findingText(t, f)).join(", ")
      : review.refusal.split("\n").join("; ")
    : "";
  const readOnly = review?.hostPolicy.readOnly === true;
  const imageProblem = review?.imageError
    ? t("services.refusal.image_unresolved", {
        ref: review.imageError.ref,
        host: review.imageError.host,
      })
    : undefined;

  // Why Apply cannot be used, in the order that matters; absent when it can.
  let reason: string | undefined;
  if (review) {
    if (!review.changed) reason = t("specEditor.review.nothing");
    else if (!review.allowed) reason = t("needs.role.admin");
    else if (readOnly) reason = t("actions.readOnlyHost");
    else if (imageProblem) reason = imageProblem;
    else if (lacking > 0) reason = t("specEditor.review.newObjects.reason", { count: lacking });
  }
  const lookingUp = review === undefined;
  const blocked = lookingUp || reason !== undefined;
  const hiddenOnly = review?.changed === true && rows.length === 0;

  const applyButton = (
    <Button
      type="button"
      className="max-md:h-11"
      aria-disabled={blocked || applying ? true : undefined}
      onClick={blocked || applying ? undefined : onApply}
    >
      {applying ? (
        <>
          <Spinner aria-hidden="true" />
          {t("specEditor.review.applying")}
        </>
      ) : (
        t(create ? "specEditor.create.review.confirm" : "specEditor.review.apply")
      )}
    </Button>
  );

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-col gap-1">
        <h2 className="text-base font-semibold break-words">
          {create ? t("specEditor.create.review.title", { name }) : t("specEditor.review.title")}
        </h2>
        <p className="text-sm text-muted-foreground">
          {create ? t("specEditor.create.review.body") : t("specEditor.review.body", { name })}
        </p>
      </div>
      {review && !review.allowed ? (
        <Alert variant="warning">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription className="break-words text-status-warning-text">
            {t("specEditor.review.refusal", { list: refusalList })}
          </AlertDescription>
        </Alert>
      ) : null}
      {review?.allowed && findings.length > 0 && !review.hostPolicy.managerAllowsPrivileged ? (
        <Alert variant="warning">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription className="break-words text-status-warning-text">
            {t("services.problems.agent_host_policy")}
          </AlertDescription>
        </Alert>
      ) : null}
      {readOnly ? (
        <Alert variant="warning">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription className="text-status-warning-text">
            {t("actions.readOnlyHost")}
          </AlertDescription>
        </Alert>
      ) : null}
      {imageProblem ? (
        <Alert variant="warning">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription className="break-words text-status-warning-text">
            {imageProblem}
          </AlertDescription>
        </Alert>
      ) : null}
      {review?.warnings && review.warnings.length > 0 ? (
        <Alert role="status">
          <Info aria-hidden="true" />
          <AlertTitle>{t("specEditor.review.notes")}</AlertTitle>
          <AlertDescription>
            <ul className="list-disc ps-4">
              {review.warnings.map((w) => (
                <li key={w} className="break-words">
                  {w}
                </li>
              ))}
            </ul>
          </AlertDescription>
        </Alert>
      ) : null}
      {imageChanged ? (
        <ImageRow
          current={create ? undefined : currentImage}
          next={review ? nextImage : undefined}
          credential={review?.image.credential}
        />
      ) : null}
      {missingCount > 0 ? (
        <NewObjectsCard missing={missing} staged={staged} disabled={applying} />
      ) : null}
      <section className="flex flex-col gap-2">
        <h3 className="text-base font-semibold">{t("serviceForm.update.behaviour")}</h3>
        <AutoRollbackBox
          id={rollbackId}
          state={autoRollbackState(edited)}
          helper="reviewHelper"
          disabled={applying}
          onChange={(ticked) => onSpecChange(setAutoRollback(edited, ticked))}
        />
      </section>
      <section className="flex flex-col gap-2">
        <h3 className="text-base font-semibold">{t("specEditor.review.changes")}</h3>
        {hiddenOnly ? (
          <p className="rounded-lg border p-4 text-sm text-muted-foreground">
            {t("specEditor.review.hiddenOnly")}
          </p>
        ) : (
          <SpecDiff name={name} current={current} next={next} create={create} />
        )}
      </section>
      <EditorFooter>
        <Button type="button" variant="outline" className="max-md:h-11" onClick={onBack}>
          {t("specEditor.review.back")}
        </Button>
        <NeedsTooltip reason={applying ? undefined : reason}>{applyButton}</NeedsTooltip>
      </EditorFooter>
    </div>
  );
}
