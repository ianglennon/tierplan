// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute, redirect, useRouter } from "@tanstack/react-router";
import { Eye, EyeOff, Info } from "lucide-react";
import { type FormEvent, useRef, useState } from "react";

import { api, setCsrfToken } from "@/api/client";
import { instanceQuery, loadInstance } from "@/api/instance";
import { queryClient } from "@/api/query-client";
import { AuthLayout } from "@/components/auth-layout";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group";
import { Spinner } from "@/components/ui/spinner";
import { lowerAscii, MAX_USERNAME_LENGTH } from "@/features/setup/schemas";
import { meQueryOptions, UnauthenticatedError, useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { withBase } from "@/lib/base-path";
import { safeNext } from "@/lib/redirect";
import { cn } from "@/lib/utils";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface LoginSearch {
  next?: string;
  expired?: boolean;
}

function LoginPage() {
  const t = useT();
  const router = useRouter();
  const { next, expired } = Route.useSearch();
  const { data: instance } = useQuery(instanceQuery);
  useDocumentTitle(t("auth.login.tabTitle"));

  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [remember, setRemember] = useState(false);
  const [showPassword, setShowPassword] = useState(false);
  const [pending, setPending] = useState(false);
  // The i18n key of the form error, or null. Every failed sign-in shows the
  // same one (D-30): the screen never learns why the server said no.
  const [errorKey, setErrorKey] = useState<string | null>(null);

  const submitting = useRef(false);
  const usernameRef = useRef<HTMLInputElement>(null);
  const passwordRef = useRef<HTMLInputElement>(null);

  const title = instance?.instanceName || t("common.brand.wordmark");
  const days = instance?.rememberDays ?? 30;

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (submitting.current) return;
    if (username === "" || password === "") {
      setErrorKey("auth.login.error");
      (username === "" ? usernameRef : passwordRef).current?.focus();
      return;
    }
    submitting.current = true;
    setPending(true);
    setErrorKey(null);
    try {
      const { data } = await api.POST("/api/auth/login", {
        body: { username, password, remember },
      });
      if (!data) {
        setErrorKey("auth.login.error");
        return;
      }
      setCsrfToken(data.csrfToken);
      // Whatever a previous user's session cached must not be shown to this
      // one (WR-15). This also drops the old /api/me answer, so the app shell
      // loads the permissions that sign-in does not carry.
      queryClient.clear();
      router.history.push(withBase(safeNext(next)));
    } catch {
      setErrorKey("common.errors.unreachable");
    } finally {
      submitting.current = false;
      setPending(false);
    }
  }

  return (
    <AuthLayout title={title} subtitle={t("auth.login.subtitle")}>
      {expired ? (
        <Alert role="status">
          <Info aria-hidden="true" />
          <AlertDescription>{t("auth.login.expired")}</AlertDescription>
        </Alert>
      ) : null}
      {errorKey ? (
        <Alert variant="destructive">
          <AlertDescription>{t(errorKey)}</AlertDescription>
        </Alert>
      ) : null}
      <form noValidate onSubmit={onSubmit}>
        <fieldset disabled={pending} className="contents">
          <FieldGroup className="gap-4">
            <Field>
              <FieldLabel htmlFor="login-username">{t("auth.login.username.label")}</FieldLabel>
              <Input
                id="login-username"
                ref={usernameRef}
                name="username"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                // Not case-sensitive (D-35): show what will be matched.
                onBlur={() => setUsername((v) => lowerAscii(v))}
                className={TOUCH_HEIGHT}
                autoComplete="username"
                autoCapitalize="off"
                autoCorrect="off"
                spellCheck={false}
                maxLength={MAX_USERNAME_LENGTH}
              />
            </Field>
            <Field>
              <FieldLabel htmlFor="login-password">{t("auth.login.password.label")}</FieldLabel>
              <InputGroup className="h-11 md:h-8">
                <InputGroupInput
                  id="login-password"
                  ref={passwordRef}
                  name="password"
                  type={showPassword ? "text" : "password"}
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  autoComplete="current-password"
                  className="h-full"
                />
                <InputGroupAddon align="inline-end">
                  <InputGroupButton
                    size="icon-xs"
                    aria-label={t(showPassword ? "auth.login.hide" : "auth.login.show")}
                    onClick={() => setShowPassword((v) => !v)}
                  >
                    {showPassword ? <EyeOff aria-hidden="true" /> : <Eye aria-hidden="true" />}
                  </InputGroupButton>
                </InputGroupAddon>
              </InputGroup>
            </Field>
            <Field orientation="horizontal">
              <Checkbox
                id="login-remember"
                checked={remember}
                onCheckedChange={(v) => setRemember(v === true)}
              />
              <FieldLabel htmlFor="login-remember" className="font-normal">
                {t("auth.login.remember", { days })}
              </FieldLabel>
            </Field>
            <Button type="submit" className={cn("w-full", TOUCH_HEIGHT)}>
              {pending ? (
                <>
                  <Spinner aria-label={t("auth.login.pending")} />
                  {t("auth.login.pending")}
                </>
              ) : (
                t("auth.login.submit")
              )}
            </Button>
          </FieldGroup>
        </fieldset>
      </form>
    </AuthLayout>
  );
}

export const Route = createFileRoute("/login")({
  validateSearch: (search: Record<string, unknown>): LoginSearch => ({
    next: typeof search.next === "string" ? search.next : undefined,
    expired: search.expired === true || search.expired === "true" ? true : undefined,
  }),
  beforeLoad: async ({ search }) => {
    const instance = await loadInstance();
    if (instance.needsSetup) throw redirect({ to: "/setup" });
    // Already signed in: skip the form.
    try {
      await queryClient.fetchQuery({ ...meQueryOptions, staleTime: 0 });
    } catch (err) {
      if (err instanceof UnauthenticatedError) return;
      throw err;
    }
    throw redirect({ href: withBase(safeNext(search.next)) });
  },
  component: LoginPage,
});
