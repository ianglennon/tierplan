// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { ChevronDown } from "lucide-react";
import { useId, useMemo, useState } from "react";

import { environmentsQuery, recentQuery } from "@/api/environments";
import {
  type CredentialRequestError,
  type CredentialTestFromEnvRequest,
  type CredentialTestFromEnvResult,
  useTestCredentialFromEnv,
} from "@/api/registries";
import { isValidTagName } from "@/api/tags";
import { AgentUpdateNotice } from "@/components/agent-update-notice";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { failureCopy, registryHost } from "@/features/pulls/failure-copy";
import { hasPermission, useMe } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

interface TestFromEnvProps {
  // The values of the sheet to test, ready to send, or null when they are not valid (the
  // sheet has then shown what is wrong). The repository to read is added here.
  prepare: () => Promise<CredentialTestFromEnvRequest | null>;
  // The test repository as typed; the button is disabled while it is empty.
  repository: string;
  // True while there is no secret to try: a new credential with none typed.
  needsSecret: boolean;
  // Called with a refusal that names form fields (422), so the sheet can show them where they
  // belong; answers true when it did, and the popover then shows nothing of its own.
  onFieldErrors?: (err: CredentialRequestError) => boolean;
  // The environment the credential belongs to, absent for an instance credential. Such a
  // credential is tested from its own environment only, through the environment's routes.
  credentialEnvId?: string;
}

// The codes the answer carries that are not a node's failure to pull: they have a notice or
// a sentence of their own.
const AGENT_TOO_OLD = "agent_policy_denied";
// A node that did not answer is worded as the pull form words it.
const NODE_GONE = "node_unavailable";
// The codes this check words itself, because the pull form's sentence for them is about a
// pull and a node, and a manifest check pulls nothing (G-05-4c).
const OWN_REASONS = new Set([
  "pull_denied",
  "pull_unauthorized",
  "image_not_found",
  "pull_failed",
  "registry_http_fallback",
]);
// The codes the pull form words well for a manifest check too, with the ref from the answer.
const SHARED_REASONS = new Set([
  "registry_unreachable",
  "registry_tls",
  "registry_http",
  "rate_limited",
  "invalid_reference",
  "cancelled",
]);

// reasonOf is the mapped text for a failed code, in the words of UI-SPEC "Shared refusals and
// failures". Nothing the registry or the engine said is ever in it: the server sends a code.
function reasonOf(t: ReturnType<typeof useT>, result: CredentialTestFromEnvResult): string {
  const host = registryHost(result.ref);
  if (result.code === NODE_GONE) {
    return failureCopy(t, "node_unreachable", {
      credential: null,
      ref: result.ref,
      node: result.node,
    }).text;
  }
  if (SHARED_REASONS.has(result.code)) {
    return failureCopy(t, result.code, { credential: null, ref: result.ref, node: result.node })
      .text;
  }
  // A code this list does not know is worded as the check's pull_failed: never "check the node".
  const code = OWN_REASONS.has(result.code) ? result.code : "pull_failed";
  return t(`registries.test.fromEnv.reasons.${code}`, {
    host,
    node: result.node,
    tag: result.tag,
    repo: result.repository,
  });
}

// TestFromEnv is the sheet's "Test from an environment…": a popover with an Environment
// select and a button that has one node of that environment read the test repository's
// manifest with the credential, through its agent (D-13). It catches the firewall, DNS and
// CA problems of the node that pulls, which the server's own test cannot see. On a swarm the
// server picks one manager and the result names the node.
export function TestFromEnv({
  prepare,
  repository,
  needsSecret,
  onFieldErrors,
  credentialEnvId,
}: TestFromEnvProps) {
  const t = useT();
  const ids = useId();
  const me = useMe();
  const environments = useQuery(environmentsQuery);
  const recent = useQuery(recentQuery);
  const tester = useTestCredentialFromEnv();
  const [open, setOpen] = useState(false);
  const [chosen, setChosen] = useState<string | undefined>();
  // The tag to read, as typed; empty leaves the choice to the server. tagInvalid is set by
  // the check before sending or by the server's 422 tag_invalid, and cleared on typing.
  const [tag, setTag] = useState("");
  const [tagInvalid, setTagInvalid] = useState(false);
  const [outcome, setOutcome] = useState<
    | { kind: "result"; result: CredentialTestFromEnvResult; envId: string }
    | { kind: "error"; message: string }
    | null
  >(null);

  // The environments a check may run from: those the person may pull on, and for the
  // credential of an environment only that one.
  const choices = useMemo(
    () =>
      (environments.data?.items ?? [])
        .filter((e) =>
          credentialEnvId ? e.id === credentialEnvId : hasPermission(me.data, "images.pull", e.id),
        )
        .sort((a, b) => a.name.localeCompare(b.name) || a.id.localeCompare(b.id)),
    [environments.data, me.data, credentialEnvId],
  );
  const selected =
    (recent.data ?? []).find((id) => choices.some((e) => e.id === id)) ?? choices[0]?.id;
  const envId = chosen && choices.some((e) => e.id === chosen) ? chosen : selected;
  const environment = choices.find((e) => e.id === envId);

  const noRepository = repository.trim() === "";
  const blockedBy = noRepository
    ? t("registries.test.fromEnv.needRepository")
    : needsSecret
      ? t("registries.test.needSecret")
      : null;

  function clear() {
    setOutcome(null);
    tester.reset();
  }

  async function run() {
    if (tester.isPending || !envId) return;
    clear();
    const wanted = tag.trim();
    if (wanted !== "" && !isValidTagName(wanted)) {
      setTagInvalid(true);
      return;
    }
    const body = await prepare();
    if (!body) return;
    tester.mutate(
      {
        body: {
          ...body,
          repository: repository.trim(),
          ...(wanted !== "" ? { tag: wanted } : {}),
        },
        runEnvId: envId,
        credentialEnvId,
      },
      {
        onSuccess: (result) => setOutcome({ kind: "result", result, envId }),
        onError: (err: CredentialRequestError) => {
          // A refused tag belongs to this popover's own field, not to the sheet's.
          if (err.status === 422 && err.fields.some((f) => f.location === "body.tag")) {
            setTagInvalid(true);
            return;
          }
          if (onFieldErrors?.(err)) {
            setOpen(false);
            return;
          }
          setOutcome({
            kind: "error",
            message:
              err.status === 429
                ? t("registries.test.errors.rateLimited")
                : err.status === 403
                  ? t("registries.test.errors.permission")
                  : err.status === 503 || err.type === "node_unavailable"
                    ? failureCopy(t, "node_unreachable", {
                        credential: null,
                        ref: repository,
                        node: environment?.name ?? "",
                      }).text
                    : t("registries.test.errors.generic"),
          });
        },
      },
    );
  }

  const result = outcome?.kind === "result" ? outcome.result : null;
  const where = result
    ? result.node
    : t("registries.test.fromEnv.whereBefore", { environment: environment?.name ?? "" });

  return (
    <Popover
      open={open}
      onOpenChange={(next) => {
        setOpen(next);
        if (!next && !tester.isPending) clear();
      }}
    >
      <PopoverTrigger asChild>
        <Button
          type="button"
          variant="outline"
          className="h-11 md:h-8"
          disabled={blockedBy !== null}
          aria-describedby={blockedBy ? `${ids}-need` : undefined}
        >
          {t("registries.test.fromEnv.button")}
          <ChevronDown aria-hidden="true" />
        </Button>
      </PopoverTrigger>
      {blockedBy ? (
        <span id={`${ids}-need`} className="text-xs text-muted-foreground">
          {blockedBy}
        </span>
      ) : null}
      <PopoverContent align="start" className="w-80 gap-3">
        {choices.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            {t("registries.test.fromEnv.noEnvironment")}
          </p>
        ) : (
          <>
            <Field>
              <FieldLabel htmlFor={`${ids}-env`}>
                {t("registries.test.fromEnv.environment")}
              </FieldLabel>
              <Select
                value={envId ?? ""}
                onValueChange={(next) => {
                  setChosen(next);
                  clear();
                }}
                disabled={tester.isPending || choices.length === 1}
              >
                <SelectTrigger id={`${ids}-env`} className="w-full max-md:h-11">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {choices.map((e) => (
                    <SelectItem key={e.id} value={e.id}>
                      {e.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </Field>
            <Field data-invalid={tagInvalid || undefined}>
              <FieldLabel htmlFor={`${ids}-tag`}>
                {t("registries.test.fromEnv.tag.label")}
              </FieldLabel>
              <Input
                id={`${ids}-tag`}
                value={tag}
                onChange={(e) => {
                  setTag(e.target.value);
                  setTagInvalid(false);
                  clear();
                }}
                placeholder={t("registries.test.fromEnv.tag.placeholder")}
                autoComplete="off"
                autoCapitalize="off"
                autoCorrect="off"
                spellCheck={false}
                disabled={tester.isPending}
                aria-invalid={tagInvalid || undefined}
                aria-describedby={`${ids}-tag-help`}
                className="h-11 font-mono md:h-8"
              />
              <FieldDescription id={`${ids}-tag-help`}>
                {t("registries.test.fromEnv.tag.helper")}
              </FieldDescription>
              {tagInvalid ? (
                <FieldError>{t("registries.test.fromEnv.tag.invalid")}</FieldError>
              ) : null}
            </Field>
            <p className="text-xs text-muted-foreground">
              {t("registries.test.fromEnv.caption", { where })}
            </p>
            <Button
              type="button"
              variant="outline"
              className="max-md:h-11"
              disabled={tester.isPending || !environment}
              onClick={() => void run()}
            >
              {tester.isPending ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("registries.test.fromEnv.pending")}
                </>
              ) : (
                t("registries.test.fromEnv.run", { environment: environment?.name ?? "" })
              )}
            </Button>
          </>
        )}
        <div role="status" className="flex flex-col gap-2 text-sm">
          {outcome?.kind === "error" ? (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{outcome.message}</AlertDescription>
            </Alert>
          ) : null}
          {result && result.code === AGENT_TOO_OLD && outcome?.kind === "result" ? (
            <AgentUpdateNotice envId={outcome.envId} />
          ) : null}
          {result && result.state === "passed" ? (
            <p>{t("registries.test.fromEnv.passed", { node: result.node, ref: result.ref })}</p>
          ) : null}
          {result && result.state === "failed" && result.code !== AGENT_TOO_OLD ? (
            <p>
              {t("registries.test.fromEnv.failed", {
                node: result.node,
                ref: result.ref,
                reason: reasonOf(t, result),
              })}
            </p>
          ) : null}
        </div>
      </PopoverContent>
    </Popover>
  );
}
