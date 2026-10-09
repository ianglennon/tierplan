// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useId, useState } from "react";
import { Controller, useForm } from "react-hook-form";
import { z } from "zod";

import { type NetworkRequestError, useCreateNetwork } from "@/api/networks";
import type { NetworkRow } from "@/api/resources";
import { AgentUpdateNotice } from "@/components/agent-update-notice";
import { entriesOf, KeyValueEditor, type KeyValueRow } from "@/components/key-value-editor";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// The drivers a network can be created with, in the order the select lists them.
export const DRIVERS = ["bridge", "overlay", "macvlan", "ipvlan"] as const;
export type Driver = (typeof DRIVERS)[number];

// The server's own limit on a name; the pattern is the engine's.
const MAX_NAME = 128;

// Messages are i18n keys, never literals (D-65).
const schema = z.object({
  name: z
    .string()
    .trim()
    .min(1, "networks.errors.nameRequired")
    .max(MAX_NAME, "networks.errors.nameInvalid")
    .regex(/^[A-Za-z0-9][A-Za-z0-9_.-]*$/, "networks.errors.nameInvalid"),
  driver: z.enum(DRIVERS),
  attachable: z.boolean(),
  internal: z.boolean(),
});
type Values = z.infer<typeof schema>;

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface CreateNetworkDialogProps {
  envId: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // Whether the environment is a swarm: overlay is then the default driver and
  // attachable is available.
  swarm: boolean;
  // The networks the list has loaded, for the overlap warning.
  networks: readonly NetworkRow[];
}

// CreateNetworkDialog is the form that makes a network (UI-SPEC "Networks"): one
// scrolling form of at most 90svh. Every refusal stays inside it; the dialog closes
// only when the network was made.
export function CreateNetworkDialog({
  envId,
  open,
  onOpenChange,
  swarm,
  networks,
}: CreateNetworkDialogProps) {
  const t = useT();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{t("networks.create.title")}</DialogTitle>
          <DialogDescription>{t("networks.create.body")}</DialogDescription>
        </DialogHeader>
        <CreateNetworkForm
          envId={envId}
          swarm={swarm}
          networks={networks}
          onClose={() => onOpenChange(false)}
        />
      </DialogContent>
    </Dialog>
  );
}

interface FormProps {
  envId: string;
  swarm: boolean;
  networks: readonly NetworkRow[];
  onClose: () => void;
}

// What a refusal of the whole request comes to: the sentence for the alert, and
// whether it is the agent's policy, which has a notice of its own.
interface Refusal {
  message: string;
  agentPolicy: boolean;
}

function CreateNetworkForm({ envId, swarm, onClose }: FormProps) {
  const t = useT();
  const ids = useId();
  const create = useCreateNetwork(envId);
  const [labels, setLabels] = useState<KeyValueRow[]>([]);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: {
      name: "",
      driver: swarm ? "overlay" : "bridge",
      attachable: false,
      internal: false,
    },
    mode: "onSubmit",
  });
  const errors = form.formState.errors;
  const driver = form.watch("driver");
  // Attachable is for an overlay network on a swarm; for any other it is off
  // and cannot be turned on.
  const canAttach = swarm && driver === "overlay";
  const nameError = typeof errors.name?.message === "string" ? t(errors.name.message) : undefined;

  function refusalOf(err: NetworkRequestError): Refusal | null {
    if (err.type === "name_taken") {
      form.setError("name", { type: "server", message: "networks.errors.nameTaken" });
      form.setFocus("name");
      return null;
    }
    if (err.type === "agent_policy_denied") return { message: "", agentPolicy: true };
    if (err.type === "host_read_only") {
      return { message: t("networks.errors.hostReadOnly"), agentPolicy: false };
    }
    if (err.type === "node_unavailable") {
      return { message: t("networks.errors.nodeUnreachable"), agentPolicy: false };
    }
    if (err.type === "node_required") {
      return { message: t("networks.errors.nodeRequired"), agentPolicy: false };
    }
    if (err.status === 403) {
      return { message: t("networks.errors.permission"), agentPolicy: false };
    }
    return { message: t("networks.errors.generic"), agentPolicy: false };
  }

  function submit(values: Values) {
    if (create.isPending) return;
    setRefusal(null);
    const entries = entriesOf(labels);
    create.mutate(
      {
        name: values.name,
        driver: values.driver,
        ...(canAttach && values.attachable ? { attachable: true } : {}),
        ...(values.internal ? { internal: true } : {}),
        ...(Object.keys(entries).length > 0 ? { labels: entries } : {}),
      },
      {
        onSuccess: (made) => {
          toastSuccess(t("networks.create.success", { name: made.name }));
          onClose();
        },
        onError: (err) => setRefusal(refusalOf(err)),
      },
    );
  }

  const pending = create.isPending;
  const nameTaken =
    errors.name?.type === "server" && errors.name.message === "networks.errors.nameTaken";

  return (
    <form noValidate onSubmit={form.handleSubmit(submit)} className="flex flex-col gap-4">
      {refusal?.agentPolicy ? <AgentUpdateNotice envId={envId} /> : null}
      {refusal && !refusal.agentPolicy ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{refusal.message}</AlertDescription>
        </Alert>
      ) : null}
      <fieldset disabled={pending} className="contents">
        <FieldGroup className="gap-4">
          <Field data-invalid={nameError ? true : undefined}>
            <FieldLabel htmlFor={`${ids}-name`}>{t("networks.create.name.label")}</FieldLabel>
            <Input
              id={`${ids}-name`}
              autoComplete="off"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              aria-invalid={Boolean(nameError)}
              aria-describedby={nameError ? `${ids}-name-error` : undefined}
              className={TOUCH_HEIGHT}
              {...form.register("name", {
                onChange: () => {
                  if (nameTaken) form.clearErrors("name");
                },
              })}
            />
            {nameError ? (
              <FieldError id={`${ids}-name-error`}>
                {nameTaken
                  ? t("networks.errors.nameTaken", { name: form.getValues("name").trim() })
                  : nameError}
              </FieldError>
            ) : null}
          </Field>

          <Field>
            <FieldLabel htmlFor={`${ids}-driver`}>{t("networks.create.driver.label")}</FieldLabel>
            <Controller
              control={form.control}
              name="driver"
              render={({ field }) => (
                <Select value={field.value} onValueChange={field.onChange}>
                  <SelectTrigger id={`${ids}-driver`} className="w-full max-md:h-11">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {DRIVERS.map((d) => (
                      <SelectItem key={d} value={d}>
                        {d}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
            />
          </Field>

          <Field orientation="horizontal">
            <Controller
              control={form.control}
              name="attachable"
              render={({ field }) => (
                <Switch
                  id={`${ids}-attachable`}
                  checked={canAttach && field.value}
                  onCheckedChange={field.onChange}
                  disabled={!canAttach}
                  aria-describedby={`${ids}-attachable-helper`}
                />
              )}
            />
            <div className="flex flex-col gap-0.5">
              <FieldLabel htmlFor={`${ids}-attachable`}>
                {t("networks.create.attachable.label")}
              </FieldLabel>
              <FieldDescription id={`${ids}-attachable-helper`}>
                {t("networks.create.attachable.helper")}
              </FieldDescription>
            </div>
          </Field>

          <Field orientation="horizontal">
            <Controller
              control={form.control}
              name="internal"
              render={({ field }) => (
                <Switch
                  id={`${ids}-internal`}
                  checked={field.value}
                  onCheckedChange={field.onChange}
                  aria-describedby={`${ids}-internal-helper`}
                />
              )}
            />
            <div className="flex flex-col gap-0.5">
              <FieldLabel htmlFor={`${ids}-internal`}>
                {t("networks.create.internal.label")}
              </FieldLabel>
              <FieldDescription id={`${ids}-internal-helper`}>
                {t("networks.create.internal.helper")}
              </FieldDescription>
            </div>
          </Field>

          <Field>
            <FieldLabel>{t("networks.create.labels.label")}</FieldLabel>
            <KeyValueEditor
              value={labels}
              onChange={setLabels}
              labels={{
                add: t("networks.create.labels.add"),
                key: t("networks.create.labels.key"),
                value: t("networks.create.labels.value"),
                remove: (key) =>
                  key
                    ? t("networks.create.labels.remove", { name: key })
                    : t("networks.create.labels.removeEmpty"),
              }}
            />
          </Field>
        </FieldGroup>
      </fieldset>
      <DialogFooter>
        <Button
          type="button"
          variant="outline"
          className={TOUCH_HEIGHT}
          onClick={onClose}
          disabled={pending}
        >
          {t("networks.create.cancel")}
        </Button>
        <Button type="submit" className={TOUCH_HEIGHT} disabled={pending}>
          {pending ? (
            <>
              <Spinner aria-hidden="true" />
              {t("networks.create.pending")}
            </>
          ) : (
            t("networks.create.submit")
          )}
        </Button>
      </DialogFooter>
    </form>
  );
}
