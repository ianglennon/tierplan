// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight } from "lucide-react";
import { useCallback, useEffect, useId, useMemo, useState } from "react";
import { Controller, useForm } from "react-hook-form";
import { addressSpaceQuery } from "@/api/address-space";
import { type NetworkRequestError, useCreateNetwork } from "@/api/networks";
import type { NetworkRow } from "@/api/resources";
import { AgentUpdateNotice } from "@/components/agent-update-notice";
import { entriesOf, KeyValueEditor, type KeyValueRow } from "@/components/key-value-editor";
import { NeedsTooltip } from "@/components/needs-tooltip";
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
import { containsAddress, containsCidr, parseCidr } from "@/features/networks/cidr";
import { SubnetPicker } from "@/features/subnet-picker/subnet-picker";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";
import { z } from "@/lib/zod-config";

// The drivers a network can be created with, in the order the select lists them.
export const DRIVERS = ["bridge", "overlay", "macvlan", "ipvlan"] as const;
export type Driver = (typeof DRIVERS)[number];

// The server's own limit on a name; the pattern is the engine's.
const MAX_NAME = 128;

// The fields of the form that sit in Advanced, so an error in one opens it.
const ADVANCED_FIELDS = ["subnet", "gateway", "ipRange"] as const;
type AdvancedField = (typeof ADVANCED_FIELDS)[number];

// Messages are i18n keys, never literals (D-65). A local-scope driver on a swarm
// needs a node; the addressing is checked as the server checks it, so the common
// mistakes are caught before a request goes out.
function makeSchema(needsNode: (driver: Driver) => boolean) {
  return z
    .object({
      name: z
        .string()
        .trim()
        .min(1, "networks.errors.nameRequired")
        .max(MAX_NAME, "networks.errors.nameInvalid")
        .regex(/^[A-Za-z0-9][A-Za-z0-9_.-]*$/, "networks.errors.nameInvalid"),
      driver: z.enum(DRIVERS),
      nodeId: z.string(),
      attachable: z.boolean(),
      internal: z.boolean(),
      ipv6: z.boolean(),
      subnet: z.string().trim(),
      gateway: z.string().trim(),
      ipRange: z.string().trim(),
    })
    .superRefine((v, ctx) => {
      const issue = (path: string, message: string) =>
        ctx.addIssue({ code: "custom", path: [path], message });
      if (needsNode(v.driver) && v.nodeId === "") issue("nodeId", "networks.errors.nodeRequired");
      if (v.subnet === "") {
        // A gateway or a range has nothing to be inside of.
        if (v.gateway !== "" || v.ipRange !== "") issue("subnet", "networks.errors.subnetInvalid");
        return;
      }
      if (parseCidr(v.subnet) === null) {
        issue("subnet", "networks.errors.subnetInvalid");
        return;
      }
      if (v.gateway !== "" && !containsAddress(v.subnet, v.gateway)) {
        issue("gateway", "networks.errors.gatewayOutside");
      }
      if (v.ipRange !== "" && !containsCidr(v.subnet, v.ipRange)) {
        issue("ipRange", "networks.errors.rangeOutside");
      }
    });
}
type Values = z.infer<ReturnType<typeof makeSchema>>;

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface CreateNetworkDialogProps {
  envId: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // Whether the environment is a swarm: overlay is then the default driver,
  // attachable is available and a local driver asks for a node.
  swarm: boolean;
  // The networks the list has loaded: the nodes to create on are the ones they came from.
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
      <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-140">
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

interface NodeChoice {
  id: string;
  name: string;
}

// nodesOf lists the nodes a local network can be made on: the ones the loaded
// networks are local to, by node ID, so a name two hosts share does not hide one.
function nodesOf(networks: readonly NetworkRow[]): NodeChoice[] {
  const byId = new Map<string, string>();
  for (const n of networks) {
    if (n.scope === "local" && n.nodeId && !byId.has(n.nodeId)) byId.set(n.nodeId, n.node);
  }
  return [...byId].map(([id, name]) => ({ id, name })).sort((a, b) => a.name.localeCompare(b.name));
}

function CreateNetworkForm({ envId, swarm, networks, onClose }: FormProps) {
  const t = useT();
  const ids = useId();
  const create = useCreateNetwork(envId);
  const [labels, setLabels] = useState<KeyValueRow[]>([]);
  const [options, setOptions] = useState<KeyValueRow[]>([]);
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [advanced, setAdvanced] = useState(false);
  // Whether a finding blocks the create, and whether the person ticked Create it anyway for it.
  const [blocking, setBlocking] = useState(false);
  const [createAnyway, setCreateAnyway] = useState(false);
  const manage = useNeeds("networks.manage", envId);
  const space = useQuery(addressSpaceQuery(envId));
  const nodes = useMemo(() => nodesOf(networks), [networks]);

  // A local driver on a swarm is made on one node; an overlay is made by a manager.
  const needsNode = (d: Driver) => swarm && d !== "overlay";
  const schema = useMemo(() => makeSchema((d) => swarm && d !== "overlay"), [swarm]);
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: {
      name: "",
      driver: swarm ? "overlay" : "bridge",
      nodeId: "",
      attachable: false,
      internal: false,
      ipv6: false,
      subnet: "",
      gateway: "",
      ipRange: "",
    },
    mode: "onSubmit",
  });
  const errors = form.formState.errors;
  const driver = form.watch("driver");
  // Attachable is for an overlay network on a swarm; for any other it is off
  // and cannot be turned on.
  const canAttach = swarm && driver === "overlay";
  const askNode = needsNode(driver);

  const messageOf = (field: keyof Values) => {
    const message = errors[field]?.message;
    return typeof message === "string" ? message : undefined;
  };
  const nameKey = messageOf("name");
  const nameError =
    nameKey === "networks.errors.nameTaken"
      ? t(nameKey, { name: form.getValues("name").trim() })
      : nameKey
        ? t(nameKey)
        : undefined;
  const errorOf = (field: keyof Values) => {
    const key = messageOf(field);
    return key ? t(key) : undefined;
  };

  function refusalOf(err: NetworkRequestError): Refusal | null {
    const field = (name: keyof Values, key: string, open = false) => {
      form.setError(name, { type: "server", message: key });
      if (open) setAdvanced(true);
      return null;
    };
    switch (err.type) {
      case "name_taken":
        form.setFocus("name");
        return field("name", "networks.errors.nameTaken");
      case "subnet_invalid":
        return field("subnet", "networks.errors.subnetInvalid", true);
      case "gateway_outside_subnet":
        return field("gateway", "networks.errors.gatewayOutside", true);
      case "iprange_outside_subnet":
        return field("ipRange", "networks.errors.rangeOutside", true);
      case "node_required":
        return field("nodeId", "networks.errors.nodeRequired");
      case "subnet_overlap":
        setAdvanced(true);
        return { message: t("subnetPicker.errors.overlapRefused"), agentPolicy: false };
      case "agent_policy_denied":
        return { message: "", agentPolicy: true };
      case "host_read_only":
        return { message: t("networks.errors.hostReadOnly"), agentPolicy: false };
      case "node_unavailable":
        return { message: t("networks.errors.nodeUnreachable"), agentPolicy: false };
      case "attachable_overlay_only":
        return { message: t("networks.errors.attachableOverlayOnly"), agentPolicy: false };
      case "overlay_needs_swarm":
        return { message: t("networks.errors.overlayNeedsSwarm"), agentPolicy: false };
    }
    if (err.status === 403) return { message: t("networks.errors.permission"), agentPolicy: false };
    return { message: t("networks.errors.generic"), agentPolicy: false };
  }

  function submit(values: Values) {
    if (create.isPending || (blocking && !createAnyway)) return;
    setRefusal(null);
    const labelMap = entriesOf(labels);
    const optionMap = entriesOf(options);
    create.mutate(
      {
        name: values.name,
        driver: values.driver,
        ...(askNode && values.nodeId ? { nodeId: values.nodeId } : {}),
        ...(canAttach && values.attachable ? { attachable: true } : {}),
        ...(values.internal ? { internal: true } : {}),
        ...(values.ipv6 ? { ipv6: true } : {}),
        ...(values.subnet ? { subnet: values.subnet } : {}),
        ...(values.subnet && blocking && createAnyway ? { createAnyway: true } : {}),
        ...(values.gateway ? { gateway: values.gateway } : {}),
        ...(values.ipRange ? { ipRange: values.ipRange } : {}),
        ...(Object.keys(labelMap).length > 0 ? { labels: labelMap } : {}),
        ...(Object.keys(optionMap).length > 0 ? { options: optionMap } : {}),
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

  // An error in a field hidden under Advanced opens it, so it is seen.
  function invalid(found: typeof errors) {
    if (ADVANCED_FIELDS.some((f: AdvancedField) => found[f])) setAdvanced(true);
  }

  // The picker reports whether a finding blocks; a tick belongs to the overlap it was given for,
  // so it goes when the overlap does. A finding opens Advanced, where it can be read.
  const onBlockingChange = useCallback((now: boolean) => {
    setBlocking(now);
    if (!now) setCreateAnyway(false);
  }, []);
  const onFindingsChange = useCallback((count: number) => {
    if (count > 0) setAdvanced(true);
  }, []);

  // A pool at 70% or more opens Advanced by itself, with the reason, because a smaller subnet is
  // the person's way to keep room (UI-SPEC default 6).
  const pools = space.data?.pools ?? [];
  const crowded = pools.filter((p) => p.state === "getting_full" || p.state === "almost_full");
  const worstPercent = crowded.length > 0 ? Math.max(...crowded.map((p) => p.percent)) : null;
  const isCrowded = worstPercent !== null;
  useEffect(() => {
    if (isCrowded) setAdvanced(true);
  }, [isCrowded]);
  // The mask of the swarm's own pool is the size an overlay network gets by default.
  const defaultMask =
    swarm && driver === "overlay"
      ? (pools.find((p) => p.source === "swarm") ?? pools[0])?.mask
      : undefined;
  const nodeMissing = askNode && form.watch("nodeId") === "";
  const blocked = blocking && !createAnyway;

  const pending = create.isPending;
  const nameTaken = errors.name?.type === "server";

  return (
    <form noValidate onSubmit={form.handleSubmit(submit, invalid)} className="flex flex-col gap-4">
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
            {nameError ? <FieldError id={`${ids}-name-error`}>{nameError}</FieldError> : null}
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

          {askNode ? (
            <Field data-invalid={errorOf("nodeId") ? true : undefined}>
              <FieldLabel htmlFor={`${ids}-node`}>{t("networks.create.node.label")}</FieldLabel>
              <Controller
                control={form.control}
                name="nodeId"
                render={({ field }) => (
                  <Select value={field.value} onValueChange={field.onChange}>
                    <SelectTrigger
                      id={`${ids}-node`}
                      className="w-full max-md:h-11"
                      aria-invalid={Boolean(errorOf("nodeId"))}
                    >
                      <SelectValue placeholder={t("networks.create.node.placeholder")} />
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
              {errorOf("nodeId") ? <FieldError>{errorOf("nodeId")}</FieldError> : null}
            </Field>
          ) : null}

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

          <div className="flex flex-col gap-4">
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="group/advanced w-fit max-md:h-11"
              aria-expanded={advanced}
              aria-controls={`${ids}-advanced`}
              onClick={() => setAdvanced((open) => !open)}
            >
              <ChevronRight
                aria-hidden="true"
                className="transition-transform group-aria-expanded/advanced:rotate-90"
              />
              {t("networks.create.advanced.trigger")}
            </Button>
            {/* Kept mounted while closed, so a check and a blocking overlap are not lost. */}
            <div
              id={`${ids}-advanced`}
              hidden={!advanced}
              className={cn("flex-col gap-4", advanced ? "flex" : "hidden")}
            >
              {worstPercent !== null ? (
                <p className="text-sm text-muted-foreground">
                  {t("subnetPicker.poolUsed", { percent: worstPercent })}
                </p>
              ) : null}
              <Controller
                control={form.control}
                name="subnet"
                render={({ field }) => (
                  <SubnetPicker
                    envId={envId}
                    envType={swarm ? "swarm" : "standalone"}
                    value={field.value}
                    onChange={field.onChange}
                    onBlur={field.onBlur}
                    inputRef={field.ref}
                    inputId={`${ids}-subnet`}
                    driver={driver}
                    nodeId={askNode && form.watch("nodeId") ? form.watch("nodeId") : undefined}
                    nodeMissing={nodeMissing}
                    defaultMask={defaultMask}
                    pools={pools.map((p) => p.cidr)}
                    canCreateAnyway={manage.allowed}
                    createAnyway={createAnyway}
                    onCreateAnywayChange={setCreateAnyway}
                    onBlockingChange={onBlockingChange}
                    onFindingsChange={onFindingsChange}
                    error={errorOf("subnet")}
                  />
                )}
              />

              <Field data-invalid={errorOf("gateway") ? true : undefined}>
                <FieldLabel htmlFor={`${ids}-gateway`}>
                  {t("networks.create.gateway.label")}
                </FieldLabel>
                <Input
                  id={`${ids}-gateway`}
                  autoComplete="off"
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                  aria-invalid={Boolean(errorOf("gateway"))}
                  className={cn("font-mono", TOUCH_HEIGHT)}
                  {...form.register("gateway")}
                />
                {errorOf("gateway") ? <FieldError>{errorOf("gateway")}</FieldError> : null}
              </Field>

              <Field data-invalid={errorOf("ipRange") ? true : undefined}>
                <FieldLabel htmlFor={`${ids}-iprange`}>
                  {t("networks.create.ipRange.label")}
                </FieldLabel>
                <Input
                  id={`${ids}-iprange`}
                  autoComplete="off"
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                  aria-invalid={Boolean(errorOf("ipRange"))}
                  className={cn("font-mono", TOUCH_HEIGHT)}
                  {...form.register("ipRange")}
                />
                {errorOf("ipRange") ? <FieldError>{errorOf("ipRange")}</FieldError> : null}
              </Field>

              <Field orientation="horizontal">
                <Controller
                  control={form.control}
                  name="ipv6"
                  render={({ field }) => (
                    <Switch
                      id={`${ids}-ipv6`}
                      checked={field.value}
                      onCheckedChange={field.onChange}
                    />
                  )}
                />
                <FieldLabel htmlFor={`${ids}-ipv6`}>{t("networks.create.ipv6.label")}</FieldLabel>
              </Field>

              <Field>
                <FieldLabel>{t("networks.create.options.label")}</FieldLabel>
                <KeyValueEditor
                  value={options}
                  onChange={setOptions}
                  labels={{
                    add: t("networks.create.options.add"),
                    key: t("networks.create.options.key"),
                    value: t("networks.create.options.value"),
                    remove: (key) =>
                      key
                        ? t("networks.create.options.remove", { name: key })
                        : t("networks.create.options.removeEmpty"),
                  }}
                />
              </Field>
            </div>
          </div>
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
        <NeedsTooltip reason={blocked ? t("subnetPicker.createAnyway.blocked") : undefined}>
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
        </NeedsTooltip>
      </DialogFooter>
    </form>
  );
}
