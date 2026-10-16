// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { TriangleAlert } from "lucide-react";
import { useId, useMemo, useState } from "react";
import { Controller, useForm } from "react-hook-form";
import type { NetworkRow, VolumeRow } from "@/api/resources";
import { useCreateVolume, type VolumeRequestError } from "@/api/volumes";
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
import {
  generatedRows,
  type PresetFields,
  type PresetType,
  presetOptions,
} from "@/features/volumes/presets";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";
import { z } from "@/lib/zod-config";

// The kinds of volume the Type select offers. None is a plain volume of the driver.
export const TYPES = ["none", "nfs", "smb", "host"] as const satisfies readonly PresetType[];
export type VolumeType = (typeof TYPES)[number];

// The server's own limit on a name; the pattern is the engine's.
const MAX_NAME = 255;
const DEFAULT_DRIVER = "local";

// Messages are i18n keys, never literals (D-65). A swarm needs a node, because a volume
// lives on one.
function makeSchema(needsNode: boolean) {
  return z
    .object({
      name: z
        .string()
        .trim()
        .min(1, "volumes.errors.nameRequired")
        .max(MAX_NAME, "volumes.errors.nameInvalid")
        .regex(/^[A-Za-z0-9][A-Za-z0-9_.-]*$/, "volumes.errors.nameInvalid"),
      nodeId: z.string(),
      driver: z.string().trim(),
      type: z.enum(TYPES),
      // The preset fields. Only the ones of the chosen Type are checked and sent.
      server: z.string().trim(),
      path: z.string().trim(),
      nfsOptions: z.string().trim(),
      share: z.string().trim(),
      username: z.string().trim(),
      password: z.string(),
      smbOptions: z.string().trim(),
      folder: z.string().trim(),
    })
    .superRefine((v, ctx) => {
      const issue = (path: keyof typeof v, message: string) =>
        ctx.addIssue({ code: "custom", path: [path], message });
      if (needsNode && v.nodeId === "") issue("nodeId", "volumes.errors.nodeRequired");
      if ((v.type === "nfs" || v.type === "smb") && v.server === "") {
        issue("server", "volumes.errors.serverRequired");
      }
      if (v.type === "nfs" && !v.path.startsWith("/")) issue("path", "volumes.errors.pathInvalid");
      if (v.type === "smb" && v.share.replace(/^\/+/, "") === "") {
        issue("share", "volumes.errors.shareRequired");
      }
      if (v.type === "host" && !v.folder.startsWith("/")) {
        issue("folder", "volumes.errors.pathInvalid");
      }
    });
}
type Values = z.infer<ReturnType<typeof makeSchema>>;

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface CreateVolumeDialogProps {
  envId: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // Whether the environment is a swarm: a volume then needs a node to live on.
  swarm: boolean;
  // The rows the nodes to create on are read from. Every node holds a network of its own
  // (bridge, host, none), so the networks name nodes that have no volume yet; the
  // volumes are there too.
  networks: readonly NetworkRow[];
  volumes: readonly VolumeRow[];
}

// CreateVolumeDialog is the form that makes a volume (UI-SPEC "Volumes"): one scrolling
// form of at most 90svh. Every refusal stays inside it; the dialog closes only when the
// volume was made.
export function CreateVolumeDialog({
  envId,
  open,
  onOpenChange,
  swarm,
  networks,
  volumes,
}: CreateVolumeDialogProps) {
  const t = useT();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-140">
        <DialogHeader>
          <DialogTitle>{t("volumes.create.title")}</DialogTitle>
          <DialogDescription>{t("volumes.create.body")}</DialogDescription>
        </DialogHeader>
        <CreateVolumeForm
          envId={envId}
          swarm={swarm}
          networks={networks}
          volumes={volumes}
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
  volumes: readonly VolumeRow[];
  onClose: () => void;
}

// What a refusal of the whole request comes to: the sentence for the alert, and
// whether it is the agent's policy, which has a notice of its own.
interface Refusal {
  message: string;
  agentPolicy: boolean;
}

interface NodeChoice {
  id: string;
  name: string;
}

// nodesOf lists the nodes a volume can be made on, by node ID so a name two hosts share
// does not hide one: the nodes the local networks and the volumes came from.
function nodesOf(networks: readonly NetworkRow[], volumes: readonly VolumeRow[]): NodeChoice[] {
  const byId = new Map<string, string>();
  for (const n of networks) {
    if (n.scope === "local" && n.nodeId && !byId.has(n.nodeId)) byId.set(n.nodeId, n.node);
  }
  for (const v of volumes) {
    if (v.nodeId && !byId.has(v.nodeId)) byId.set(v.nodeId, v.node);
  }
  return [...byId].map(([id, name]) => ({ id, name })).sort((a, b) => a.name.localeCompare(b.name));
}

function CreateVolumeForm({ envId, swarm, networks, volumes, onClose }: FormProps) {
  const t = useT();
  const ids = useId();
  const create = useCreateVolume(envId);
  const [labels, setLabels] = useState<KeyValueRow[]>([]);
  const [options, setOptions] = useState<KeyValueRow[]>([]);
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const nodes = useMemo(() => nodesOf(networks, volumes), [networks, volumes]);
  const schema = useMemo(() => makeSchema(swarm), [swarm]);
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: {
      name: "",
      nodeId: "",
      driver: DEFAULT_DRIVER,
      type: "none",
      server: "",
      path: "",
      nfsOptions: "rw",
      share: "",
      username: "",
      password: "",
      smbOptions: "",
      folder: "",
    },
    mode: "onSubmit",
  });
  const errors = form.formState.errors;

  const messageOf = (field: keyof Values) => {
    const message = errors[field]?.message;
    return typeof message === "string" ? message : undefined;
  };
  const nameKey = messageOf("name");
  const nameError =
    nameKey === "volumes.errors.nameTaken"
      ? t(nameKey, { name: form.getValues("name").trim() })
      : nameKey
        ? t(nameKey)
        : undefined;
  const nodeKey = messageOf("nodeId");
  const nodeError = nodeKey ? t(nodeKey) : undefined;
  const errorOf = (field: keyof Values) => {
    const key = messageOf(field);
    return key ? t(key) : undefined;
  };

  const type = form.watch("type");
  const watched = form.watch();
  const chosenNode = nodes.find((n) => n.id === watched.nodeId);
  // The preset's fields as typed, for the options that will be sent and the rows that
  // show them.
  const fieldsOf = (password: string): PresetFields => ({
    server: watched.server,
    path: watched.path,
    share: watched.share,
    username: watched.username,
    password,
    options: type === "smb" ? watched.smbOptions : watched.nfsOptions,
    folder: watched.folder,
  });
  // The rows under a preset show a typed password as dots, so the screen never holds it
  // in the clear; what is sent is built from the real one.
  const shown = generatedRows(presetOptions(type, fieldsOf(watched.password ? "••••••••" : "")));
  const primary = {
    nfs: watched.server || watched.path,
    smb: watched.server || watched.share,
    host: watched.folder,
    none: "",
  }[type];

  function refusalOf(err: VolumeRequestError): Refusal | null {
    switch (err.type) {
      case "name_taken":
        form.setError("name", { type: "server", message: "volumes.errors.nameTaken" });
        form.setFocus("name");
        return null;
      case "node_required":
        form.setError("nodeId", { type: "server", message: "volumes.errors.nodeRequired" });
        return null;
      case "agent_policy_denied":
        return { message: "", agentPolicy: true };
      case "agent_host_policy":
        return { message: t("volumes.errors.hostPolicy"), agentPolicy: false };
      case "host_read_only":
        return { message: t("volumes.errors.hostReadOnly"), agentPolicy: false };
      case "node_unavailable":
        return { message: t("volumes.errors.nodeUnreachable"), agentPolicy: false };
    }
    if (err.status === 403) return { message: t("volumes.errors.permission"), agentPolicy: false };
    return { message: t("volumes.errors.generic"), agentPolicy: false };
  }

  function submit(values: Values) {
    if (create.isPending) return;
    setRefusal(null);
    const labelMap = entriesOf(labels);
    const optionMap =
      values.type === "none"
        ? entriesOf(options)
        : presetOptions(values.type, {
            server: values.server,
            path: values.path,
            share: values.share,
            username: values.username,
            password: values.password,
            options: values.type === "smb" ? values.smbOptions : values.nfsOptions,
            folder: values.folder,
          });
    create.mutate(
      {
        name: values.name,
        ...(swarm && values.nodeId ? { nodeId: values.nodeId } : {}),
        ...(values.driver && values.driver !== DEFAULT_DRIVER ? { driver: values.driver } : {}),
        ...(Object.keys(optionMap).length > 0 ? { driverOptions: optionMap } : {}),
        ...(Object.keys(labelMap).length > 0 ? { labels: labelMap } : {}),
      },
      {
        onSuccess: (made) => {
          toastSuccess(t("volumes.create.success", { name: made.name }));
          onClose();
        },
        onError: (err) => setRefusal(refusalOf(err)),
      },
    );
  }

  const pending = create.isPending;
  const nameTaken = errors.name?.type === "server";

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
            <FieldLabel htmlFor={`${ids}-name`}>{t("volumes.create.name.label")}</FieldLabel>
            <Input
              id={`${ids}-name`}
              autoComplete="off"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              aria-invalid={Boolean(nameError)}
              aria-describedby={`${ids}-name-helper${nameError ? ` ${ids}-name-error` : ""}`}
              className={cn("font-mono", TOUCH_HEIGHT)}
              {...form.register("name", {
                onChange: () => {
                  if (nameTaken) form.clearErrors("name");
                },
              })}
            />
            <FieldDescription id={`${ids}-name-helper`}>
              {t("volumes.create.name.helper")}
            </FieldDescription>
            {nameError ? <FieldError id={`${ids}-name-error`}>{nameError}</FieldError> : null}
          </Field>

          {swarm ? (
            <Field data-invalid={nodeError ? true : undefined}>
              <FieldLabel htmlFor={`${ids}-node`}>{t("volumes.create.node.label")}</FieldLabel>
              <Controller
                control={form.control}
                name="nodeId"
                render={({ field }) => (
                  <Select value={field.value} onValueChange={field.onChange}>
                    <SelectTrigger
                      id={`${ids}-node`}
                      className="w-full max-md:h-11"
                      aria-invalid={Boolean(nodeError)}
                    >
                      <SelectValue placeholder={t("volumes.create.node.placeholder")} />
                    </SelectTrigger>
                    <SelectContent>
                      {nodes.map((n) => (
                        <SelectItem key={n.id} value={n.id}>
                          {n.name}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                )}
              />
              <FieldDescription>{t("volumes.create.node.helper")}</FieldDescription>
              {nodeError ? <FieldError>{nodeError}</FieldError> : null}
            </Field>
          ) : null}

          <Field>
            <FieldLabel htmlFor={`${ids}-driver`}>{t("volumes.create.driver.label")}</FieldLabel>
            <Input
              id={`${ids}-driver`}
              autoComplete="off"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              className={cn("font-mono", TOUCH_HEIGHT)}
              {...form.register("driver")}
            />
          </Field>

          <Field>
            <FieldLabel htmlFor={`${ids}-type`}>{t("volumes.create.type.label")}</FieldLabel>
            <Controller
              control={form.control}
              name="type"
              render={({ field }) => (
                <Select
                  value={field.value}
                  onValueChange={(next) => {
                    field.onChange(next);
                    // The password is not kept once its preset is left, and the errors of
                    // the fields that are gone go with them.
                    form.setValue("password", "");
                    form.clearErrors(["server", "path", "share", "folder"]);
                  }}
                >
                  <SelectTrigger id={`${ids}-type`} className="w-full max-md:h-11">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {TYPES.map((k) => (
                      <SelectItem key={k} value={k}>
                        {t(`volumes.create.type.${k}`)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
            />
          </Field>

          {type === "nfs" || type === "smb" ? (
            <Field data-invalid={errorOf("server") ? true : undefined}>
              <FieldLabel htmlFor={`${ids}-server`}>{t("volumes.create.server.label")}</FieldLabel>
              <Input
                id={`${ids}-server`}
                autoComplete="off"
                autoCapitalize="off"
                autoCorrect="off"
                spellCheck={false}
                aria-invalid={Boolean(errorOf("server"))}
                className={cn("font-mono", TOUCH_HEIGHT)}
                {...form.register("server")}
              />
              {errorOf("server") ? <FieldError>{errorOf("server")}</FieldError> : null}
            </Field>
          ) : null}

          {type === "nfs" ? (
            <>
              <Field data-invalid={errorOf("path") ? true : undefined}>
                <FieldLabel htmlFor={`${ids}-path`}>{t("volumes.create.nfsPath.label")}</FieldLabel>
                <Input
                  id={`${ids}-path`}
                  placeholder="/srv/data"
                  autoComplete="off"
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                  aria-invalid={Boolean(errorOf("path"))}
                  className={cn("font-mono", TOUCH_HEIGHT)}
                  {...form.register("path")}
                />
                {errorOf("path") ? <FieldError>{errorOf("path")}</FieldError> : null}
              </Field>
              <Field>
                <FieldLabel htmlFor={`${ids}-nfs-options`}>
                  {t("volumes.create.mountOptions.label")}
                </FieldLabel>
                <Input
                  id={`${ids}-nfs-options`}
                  autoComplete="off"
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                  className={cn("font-mono", TOUCH_HEIGHT)}
                  {...form.register("nfsOptions")}
                />
              </Field>
            </>
          ) : null}

          {type === "smb" ? (
            <>
              <Field data-invalid={errorOf("share") ? true : undefined}>
                <FieldLabel htmlFor={`${ids}-share`}>{t("volumes.create.share.label")}</FieldLabel>
                <Input
                  id={`${ids}-share`}
                  autoComplete="off"
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                  aria-invalid={Boolean(errorOf("share"))}
                  className={cn("font-mono", TOUCH_HEIGHT)}
                  {...form.register("share")}
                />
                {errorOf("share") ? <FieldError>{errorOf("share")}</FieldError> : null}
              </Field>
              <Field>
                <FieldLabel htmlFor={`${ids}-username`}>
                  {t("volumes.create.username.label")}
                </FieldLabel>
                <Input
                  id={`${ids}-username`}
                  autoComplete="off"
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                  className={cn("font-mono", TOUCH_HEIGHT)}
                  {...form.register("username")}
                />
              </Field>
              <Field>
                <FieldLabel htmlFor={`${ids}-password`}>
                  {t("volumes.create.password.label")}
                </FieldLabel>
                <Input
                  id={`${ids}-password`}
                  type="password"
                  autoComplete="new-password"
                  className={TOUCH_HEIGHT}
                  {...form.register("password")}
                />
              </Field>
              <Field>
                <FieldLabel htmlFor={`${ids}-smb-options`}>
                  {t("volumes.create.mountOptions.label")}
                </FieldLabel>
                <Input
                  id={`${ids}-smb-options`}
                  autoComplete="off"
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                  className={cn("font-mono", TOUCH_HEIGHT)}
                  {...form.register("smbOptions")}
                />
              </Field>
              <Alert variant="warning" role="status">
                <TriangleAlert aria-hidden="true" />
                <AlertDescription className="break-words text-foreground">
                  {t("volumes.create.smbWarning")}
                </AlertDescription>
              </Alert>
            </>
          ) : null}

          {type === "host" ? (
            <Field data-invalid={errorOf("folder") ? true : undefined}>
              <FieldLabel htmlFor={`${ids}-folder`}>{t("volumes.create.folder.label")}</FieldLabel>
              <Input
                id={`${ids}-folder`}
                placeholder="/srv/data"
                autoComplete="off"
                autoCapitalize="off"
                autoCorrect="off"
                spellCheck={false}
                aria-invalid={Boolean(errorOf("folder"))}
                aria-describedby={`${ids}-folder-helper`}
                className={cn("font-mono", TOUCH_HEIGHT)}
                {...form.register("folder")}
              />
              <FieldDescription id={`${ids}-folder-helper`}>
                {swarm && chosenNode
                  ? t("volumes.create.folder.helperNode", { node: chosenNode.name })
                  : t("volumes.create.folder.helperHost")}
              </FieldDescription>
              {errorOf("folder") ? <FieldError>{errorOf("folder")}</FieldError> : null}
            </Field>
          ) : null}

          {type === "none" ? (
            <Field>
              <FieldLabel>{t("volumes.create.options.label")}</FieldLabel>
              <KeyValueEditor
                value={options}
                onChange={setOptions}
                labels={{
                  add: t("volumes.create.options.add"),
                  key: t("volumes.create.options.key"),
                  value: t("volumes.create.options.value"),
                  remove: (key) =>
                    key
                      ? t("volumes.create.options.remove", { name: key })
                      : t("volumes.create.options.removeEmpty"),
                }}
              />
            </Field>
          ) : primary ? (
            <fieldset className="flex min-w-0 flex-col gap-1 rounded-md border bg-muted/40 p-3">
              <legend className="px-1 text-xs font-medium text-muted-foreground">
                {t("volumes.create.generated.label")}
              </legend>
              <dl className="flex flex-col gap-1 text-sm">
                {shown.map((row) => (
                  <div key={row.name} className="flex flex-wrap gap-x-2">
                    <dt className="font-mono text-muted-foreground">{row.name}</dt>
                    <dd className="min-w-0 font-mono break-all">{row.value}</dd>
                  </div>
                ))}
              </dl>
            </fieldset>
          ) : null}

          <Field>
            <FieldLabel>{t("volumes.create.labels.label")}</FieldLabel>
            <KeyValueEditor
              value={labels}
              onChange={setLabels}
              labels={{
                add: t("volumes.create.labels.add"),
                key: t("volumes.create.labels.key"),
                value: t("volumes.create.labels.value"),
                remove: (key) =>
                  key
                    ? t("volumes.create.labels.remove", { name: key })
                    : t("volumes.create.labels.removeEmpty"),
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
          {t("volumes.create.cancel")}
        </Button>
        <Button type="submit" className={TOUCH_HEIGHT} disabled={pending}>
          {pending ? (
            <>
              <Spinner aria-hidden="true" />
              {t("volumes.create.pending")}
            </>
          ) : (
            t("volumes.create.submit")
          )}
        </Button>
      </DialogFooter>
    </form>
  );
}
