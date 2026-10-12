// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { type FormEvent, useId, useMemo, useState } from "react";
import { Trans } from "react-i18next";

import { environmentsQuery, recentQuery } from "@/api/environments";
import {
  type ConsideredCredential,
  type ResolveResult,
  useResolveCredential,
} from "@/api/registries";
import { Button } from "@/components/ui/button";
import { Field, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import {
  Table,
  TableBody,
  TableCaption,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { SettingsCard } from "@/features/environments/settings-card";
import { hasPermission, useMe, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// The server's own limit on a reference.
const MAX_REF = 512;

// What stands in the result region instead of an answer: the field the error belongs to,
// or the generic refusal for a failure the person cannot fix by typing.
type Failure = "environment" | "ref" | "permission" | "generic";

// repoOf is a normalised reference without its tag or digest: docker.io/library/nginx.
function repoOf(normalized: string): string {
  const noDigest = normalized.split("@")[0] ?? normalized;
  const colon = noDigest.lastIndexOf(":");
  return colon > noDigest.lastIndexOf("/") ? noDigest.slice(0, colon) : noDigest;
}

// CredentialTester is the "Which credential?" card (UI-SPEC "Which credential? tester",
// D-10): pick an environment, type an image, and see which stored credential a pull would
// use and why. The server answers with the function pulls use, so the two cannot drift. A
// result stays until either field changes or a new check starts, and is announced once.
export function CredentialTester() {
  const t = useT();
  const ids = useId();
  const me = useMe();
  const canAdd = usePermission("registries.instance");
  const environments = useQuery(environmentsQuery);
  // Only the environments whose images the caller may read can be asked about.
  const readable = useMemo(
    () =>
      (environments.data?.items ?? [])
        .filter((e) => hasPermission(me.data, "images.read", e.id))
        .sort((a, b) => a.name.localeCompare(b.name) || a.id.localeCompare(b.id)),
    [environments.data, me.data],
  );
  // The environment the person was last in (the one the switcher shows), else the first they
  // can read. This page is not under an environment, so the address does not say.
  const recent = useQuery(recentQuery);
  const selected =
    (recent.data ?? []).find((id) => readable.some((e) => e.id === id)) ?? readable[0]?.id;
  const [chosen, setChosen] = useState<string | undefined>();
  const envId = chosen && readable.some((e) => e.id === chosen) ? chosen : selected;

  const [ref, setRef] = useState("");
  const [failure, setFailure] = useState<Failure | null>(null);
  const resolve = useResolveCredential(envId ?? "");
  const pending = resolve.isPending;
  const result = resolve.isSuccess ? resolve.data : undefined;

  function clear() {
    setFailure(null);
    resolve.reset();
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    if (pending) return;
    clear();
    const typed = ref.trim();
    if (!envId) {
      setFailure("environment");
      return;
    }
    if (typed === "" || typed.length > MAX_REF) {
      setFailure("ref");
      return;
    }
    resolve.mutate(typed, {
      onError: (err) => {
        if (err.type === "ref_invalid") setFailure("ref");
        else if (err.status === 403) setFailure("permission");
        else setFailure("generic");
      },
    });
  }

  const refError = failure === "ref" ? t("registries.tester.errors.ref") : undefined;
  const envError =
    failure === "environment" ? t("registries.tester.errors.environment") : undefined;
  const inline =
    failure === "permission" || failure === "generic"
      ? t(`registries.tester.errors.${failure}`)
      : undefined;

  return (
    <SettingsCard title={t("registries.tester.title")}>
      <p className="text-sm text-muted-foreground">{t("registries.tester.body")}</p>
      <form noValidate onSubmit={submit} className="flex flex-col gap-4">
        <FieldGroup className="gap-4">
          <Field data-invalid={envError ? true : undefined}>
            <FieldLabel htmlFor={`${ids}-env`}>{t("registries.tester.environment")}</FieldLabel>
            <Select
              value={envId ?? ""}
              onValueChange={(next) => {
                setChosen(next);
                clear();
              }}
              disabled={readable.length === 0}
            >
              <SelectTrigger
                id={`${ids}-env`}
                aria-invalid={Boolean(envError)}
                className="w-full max-md:h-11"
              >
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {readable.map((e) => (
                  <SelectItem key={e.id} value={e.id}>
                    {e.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {envError ? <FieldError>{envError}</FieldError> : null}
          </Field>
          <Field data-invalid={refError ? true : undefined}>
            <FieldLabel htmlFor={`${ids}-ref`}>{t("registries.tester.image")}</FieldLabel>
            <Input
              id={`${ids}-ref`}
              value={ref}
              onChange={(e) => {
                setRef(e.target.value);
                clear();
              }}
              autoComplete="off"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              placeholder={t("registries.tester.imagePlaceholder")}
              aria-invalid={Boolean(refError)}
              aria-describedby={refError ? `${ids}-ref-error` : undefined}
              className="font-mono max-md:h-11"
            />
            {refError ? <FieldError id={`${ids}-ref-error`}>{refError}</FieldError> : null}
          </Field>
        </FieldGroup>
        <div>
          <Button type="submit" variant="outline" className="max-md:h-11" disabled={pending}>
            {pending ? <Spinner aria-hidden="true" /> : null}
            {pending ? t("registries.tester.pending") : t("registries.tester.submit")}
          </Button>
        </div>
      </form>
      <div role="status" className="flex min-w-0 flex-col gap-3 text-sm">
        {inline ? <p className="text-destructive">{inline}</p> : null}
        {result ? <Answer result={result} canAdd={canAdd} /> : null}
      </div>
    </SettingsCard>
  );
}

// Ref is a reference in mono, wrapped anywhere so a long one never widens the card.
function RefText({ children }: { children?: string }) {
  return <span className="font-mono break-all">{children}</span>;
}

function scopeName(t: (key: string) => string, scope: { kind: string; envName?: string | null }) {
  return scope.kind === "instance" ? t("registries.tester.scope.instance") : (scope.envName ?? "");
}

function Answer({ result, canAdd }: { result: ResolveResult; canAdd: boolean }) {
  const t = useT();
  const cred = result.credential;
  const showsReadAs = result.normalized !== result.input;
  const considered = result.considered ?? [];
  return (
    <>
      {cred ? (
        <>
          <p>
            <Trans
              i18nKey="registries.tester.matched"
              ns="registries"
              values={{ ref: result.normalized, name: cred.name, scope: scopeName(t, cred.scope) }}
              components={{ ref: <RefText /> }}
            />
          </p>
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1">
            <dt className="text-muted-foreground">{t("registries.tester.rows.registry")}</dt>
            <dd className="font-mono break-all">{cred.host}</dd>
            <dt className="text-muted-foreground">{t("registries.tester.rows.matchedOn")}</dt>
            <dd>
              {t(`registries.tester.on.${result.matchedOn}`, {
                prefix: cred.prefix,
                host: cred.host,
              })}
            </dd>
            <dt className="text-muted-foreground">{t("registries.tester.rows.scope")}</dt>
            <dd>{scopeName(t, cred.scope)}</dd>
          </dl>
        </>
      ) : (
        <>
          <p>
            <Trans
              i18nKey="registries.tester.anonymous"
              ns="registries"
              values={{ ref: result.normalized }}
              components={{ ref: <RefText /> }}
            />
          </p>
          <p className="text-muted-foreground">
            {t("registries.tester.noMatch", { path: repoOf(result.normalized) })}
          </p>
          {canAdd ? (
            <p>
              <Link
                to="/registries"
                search={{ credential: "new" }}
                className="underline underline-offset-4"
              >
                {t("registries.tester.addCredential")}
              </Link>
            </p>
          ) : null}
        </>
      )}
      {showsReadAs ? (
        <p className="font-mono text-xs break-all text-muted-foreground">
          {t("registries.tester.readAs", { ref: result.normalized })}
        </p>
      ) : null}
      {considered.length > 1 ? <Considered rows={considered} /> : null}
    </>
  );
}

function Considered({ rows }: { rows: ConsideredCredential[] }) {
  const t = useT();
  return (
    <div className="overflow-x-auto">
      <Table>
        <TableCaption className="mt-0 mb-2 text-left font-medium text-foreground">
          {t("registries.tester.considered.caption")}
        </TableCaption>
        <TableHeader>
          <TableRow>
            <TableHead>{t("registries.tester.considered.columns.credential")}</TableHead>
            <TableHead>{t("registries.tester.considered.columns.scope")}</TableHead>
            <TableHead>{t("registries.tester.considered.columns.matches")}</TableHead>
            <TableHead>{t("registries.tester.considered.columns.result")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((r) => (
            <TableRow key={r.credentialId}>
              <TableCell className="break-all">{r.name}</TableCell>
              <TableCell>{scopeName(t, r.scope)}</TableCell>
              <TableCell>
                {r.matches
                  ? t("registries.tester.considered.yes")
                  : t("registries.tester.considered.no")}
              </TableCell>
              <TableCell>{t(`registries.tester.considered.results.${r.result}`)}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}
