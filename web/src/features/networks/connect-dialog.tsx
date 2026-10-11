// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Check, ChevronsUpDown, Info } from "lucide-react";
import { useId, useMemo, useRef, useState } from "react";

import { containerDetailQuery } from "@/api/container-detail";
import { NetworkRequestError, networkDetailQuery, useConnectContainer } from "@/api/networks";
import { containersQuery, networksQuery } from "@/api/resources";
import { AgentUpdateNotice } from "@/components/agent-update-notice";
import { matchesName, TypeToConfirm } from "@/components/type-to-confirm";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
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
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Spinner } from "@/components/ui/spinner";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// The network a container is to join, from the page that opened the dialog.
export interface ConnectNetwork {
  id: string;
  name: string;
  scope: string;
  // The node a local network is on; a swarm-wide network belongs to none.
  nodeId: string;
  // Whether standalone containers can join it. Only a swarm-wide network can lack this.
  attachable: boolean;
  // The node of the page's address, so the attachments are read under the page's own key.
  node?: string;
}

// The container that is to join a network, from the page that opened the dialog.
export interface ConnectContainer {
  id: string;
  name: string;
  nodeId: string;
  // server or agent for one of Bayplan's own containers, when the page knows it.
  selfKind?: string;
}

type ConnectDialogProps = {
  envId: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
} & (
  | { network: ConnectNetwork; container?: undefined }
  | { container: ConnectContainer; network?: undefined }
);

// One thing the combobox offers: a container or a network, told apart by node as well
// as ID, since two nodes can hold different things under one ID.
interface Choice {
  key: string;
  id: string;
  name: string;
  nodeId: string;
  // The node's name, shown beside the name on a swarm.
  node: string;
  // server or agent for one of Bayplan's own containers; empty for anything else.
  selfKind: string;
}

const keyOf = (nodeId: string, id: string) => `${nodeId}|${id}`;

// The addresses are checked as the server checks them, so the common slip is caught
// before a request goes out.
function isIPv4(v: string): boolean {
  const parts = v.split(".");
  return parts.length === 4 && parts.every((p) => /^(0|[1-9]\d{0,2})$/.test(p) && Number(p) <= 255);
}

function isIPv6(v: string): boolean {
  if (!v.includes(":") || !/^[0-9a-fA-F:.]+$/.test(v)) return false;
  try {
    new URL(`http://[${v}]/`);
    return true;
  } catch {
    return false;
  }
}

// ConnectDialog connects a container to a network (UI-SPEC "Connect dialog"). From a
// network's page it picks a container, from a container's page a network; the alias
// and the addresses are optional. A refusal stays inside it with the way forward, and
// the dialog closes only when the container is on the network.
export function ConnectDialog(props: ConnectDialogProps) {
  const t = useT();
  const { open, onOpenChange } = props;
  const title = props.network
    ? t("networks.connect.title", { network: props.network.name })
    : t("networks.connect.titleContainer", { container: props.container.name });
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-120">
        <DialogHeader>
          {/* A long name wraps rather than running off the dialog. */}
          <DialogTitle className="break-words">{title}</DialogTitle>
          <DialogDescription>{t("networks.connect.body")}</DialogDescription>
        </DialogHeader>
        <ConnectForm {...props} onClose={() => onOpenChange(false)} />
      </DialogContent>
    </Dialog>
  );
}

type FormProps = Omit<ConnectDialogProps, "open" | "onOpenChange"> & { onClose: () => void };

// What the choices are, and whether they are still loading, for the page that opened the
// dialog. From a network: the containers not on it yet, and for a local network only
// those of its own node. From a container: the networks it is not on, a swarm-wide one
// only when attachable and a local one only when on the container's node.
function useChoices(props: FormProps): { choices: Choice[]; loading: boolean; blocked: boolean } {
  const { envId, network, container } = props;
  const swarmWide = network?.scope === "swarm";
  const containers = useQuery({ ...containersQuery(envId), enabled: Boolean(network) });
  const attached = useQuery({
    ...networkDetailQuery(envId, network?.id ?? "", network?.node),
    enabled: Boolean(network),
  });
  const networks = useQuery({ ...networksQuery(envId), enabled: Boolean(container) });
  const detail = useQuery({
    ...containerDetailQuery(envId, container?.id ?? "", container?.nodeId),
    enabled: Boolean(container),
  });

  return useMemo(() => {
    if (network) {
      // A swarm-wide network that standalone containers can't join has no choices.
      if (swarmWide && !network.attachable) return { choices: [], loading: false, blocked: true };
      const on = new Set(
        (attached.data?.attachments ?? [])
          .filter((a) => a.kind === "container")
          .map((a) => keyOf(a.nodeId, a.id)),
      );
      const rows = (containers.data?.items ?? [])
        .filter((c) => !on.has(keyOf(c.nodeId, c.id)) && (swarmWide || c.nodeId === network.nodeId))
        .map((c) => ({
          key: keyOf(c.nodeId, c.id),
          id: c.id,
          name: c.name,
          nodeId: c.nodeId,
          node: c.node,
          selfKind: c.selfKind,
        }));
      return { choices: rows, loading: containers.isPending || attached.isPending, blocked: false };
    }
    const on = new Set((detail.data?.networks ?? []).map((n) => n.networkId));
    const rows = (networks.data?.items ?? [])
      .filter(
        (n) =>
          !on.has(n.id) &&
          // The host and none networks take no endpoint.
          n.driver !== "host" &&
          n.driver !== "null" &&
          (n.scope === "swarm" ? n.attachable : n.nodeId === container?.nodeId),
      )
      .map((n) => ({
        key: keyOf(n.nodeId, n.id),
        id: n.id,
        name: n.name,
        nodeId: n.nodeId,
        node: n.node,
        selfKind: "",
      }));
    return { choices: rows, loading: networks.isPending || detail.isPending, blocked: false };
  }, [
    network,
    container,
    swarmWide,
    containers.data,
    containers.isPending,
    attached.data,
    attached.isPending,
    networks.data,
    networks.isPending,
    detail.data,
    detail.isPending,
  ]);
}

function ConnectForm(props: FormProps) {
  const { envId, network, container, onClose } = props;
  const t = useT();
  const ids = useId();
  const swarm = useEnvIsSwarm(envId);
  const connect = useConnectContainer(envId);
  const { choices, loading, blocked } = useChoices(props);
  const [chosen, setChosen] = useState<string | null>(null);
  const [alias, setAlias] = useState("");
  const [ipv4, setIpv4] = useState("");
  const [ipv6, setIpv6] = useState("");
  const [errors, setErrors] = useState<{ ipv4?: string; ipv6?: string }>({});
  const [refusal, setRefusal] = useState<string | null>(null);
  const [agentPolicy, setAgentPolicy] = useState(false);
  // Bayplan's own server and agent containers join a network only with their name typed.
  // The page may know the container is one; if it did not, the server's
  // confirmation_required makes the field appear.
  const [typed, setTyped] = useState("");
  const [asked, setAsked] = useState(false);
  // A second submit can land before the pending state has rendered.
  const inFlight = useRef(false);

  const what = network ? "container" : "network";
  const picked = choices.find((c) => c.key === chosen);
  const pending = connect.isPending;
  const subject = container ?? picked;
  const needName = Boolean(container ? container.selfKind : picked?.selfKind) || asked;
  const nameMissing = needName && !matchesName(typed, subject?.name ?? "");

  function wording(err: NetworkRequestError, target: Choice): string {
    const names = {
      container: container?.name ?? target.name,
      network: network?.name ?? target.name,
    };
    switch (err.type) {
      case "address_invalid":
        return t("networks.connect.errors.addressInvalid");
      case "node_mismatch":
        return t("networks.connect.errors.nodeMismatch", names);
      case "network_not_attachable":
        return t("networks.connect.errors.notAttachable", names);
      case "already_connected":
        return t("networks.connect.errors.alreadyConnected", names);
      case "network_unavailable_on_node":
        return t("networks.connect.errors.unavailableOnNode", names);
      case "network_not_found":
        return t("networks.connect.errors.networkGone", names);
      case "container_not_found":
        return t("networks.connect.errors.containerGone", names);
      case "host_read_only":
        return t("networks.errors.hostReadOnly");
      case "node_unavailable":
      case "node_unreachable":
        return t("networks.errors.nodeUnreachable");
      default:
        return err.status === 403 ? t("networks.errors.permission") : t("networks.errors.generic");
    }
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (!picked || pending || inFlight.current || nameMissing) return;
    const next: { ipv4?: string; ipv6?: string } = {};
    if (ipv4.trim() !== "" && !isIPv4(ipv4.trim())) next.ipv4 = t("networks.connect.errors.ipv4");
    if (ipv6.trim() !== "" && !isIPv6(ipv6.trim())) next.ipv6 = t("networks.connect.errors.ipv6");
    setErrors(next);
    if (next.ipv4 || next.ipv6) return;

    inFlight.current = true;
    setRefusal(null);
    setAgentPolicy(false);
    const aliasText = alias.trim();
    const target = network
      ? { networkId: network.id, containerId: picked.id, nodeId: picked.nodeId }
      : { networkId: picked.id, containerId: container?.id ?? "", nodeId: container?.nodeId ?? "" };
    try {
      await connect.mutateAsync({
        networkId: target.networkId,
        body: {
          containerId: target.containerId,
          nodeId: target.nodeId,
          ...(aliasText ? { alias: aliasText } : {}),
          ...(ipv4.trim() ? { ipv4: ipv4.trim() } : {}),
          ...(ipv6.trim() ? { ipv6: ipv6.trim() } : {}),
          ...(needName ? { confirm: typed.trim() } : {}),
        },
      });
      toastSuccess(
        t("networks.connect.success", {
          container: container?.name ?? picked.name,
          network: network?.name ?? picked.name,
        }),
      );
      onClose();
    } catch (err) {
      const e2 = err instanceof NetworkRequestError ? err : new NetworkRequestError(0);
      if (e2.type === "agent_policy_denied") setAgentPolicy(true);
      else if (e2.type === "confirmation_required") {
        setAsked(true);
        setRefusal(t("networks.connect.own"));
      } else setRefusal(wording(e2, picked));
    } finally {
      inFlight.current = false;
    }
  }

  return (
    <form noValidate onSubmit={(e) => void submit(e)} className="flex flex-col gap-4">
      {agentPolicy ? <AgentUpdateNotice envId={envId} /> : null}
      {refusal ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription className="break-words">{refusal}</AlertDescription>
        </Alert>
      ) : null}
      {blocked && network ? (
        <Alert>
          <Info aria-hidden="true" />
          <AlertDescription className="break-words">
            {t("networks.connect.notAttachable", { name: network.name })}
          </AlertDescription>
        </Alert>
      ) : null}
      <fieldset disabled={pending} className="contents">
        <FieldGroup className="gap-4">
          <Field>
            <FieldLabel htmlFor={`${ids}-pick`}>{t(`networks.connect.${what}.label`)}</FieldLabel>
            <ChoicePicker
              id={`${ids}-pick`}
              what={what}
              choices={choices}
              loading={loading}
              value={chosen}
              onChange={setChosen}
              showNode={swarm}
              disabled={pending || blocked}
            />
            {!loading && choices.length === 0 && !blocked ? (
              <p className="text-sm text-muted-foreground">{t(`networks.connect.${what}.empty`)}</p>
            ) : null}
          </Field>
          <Field>
            <FieldLabel htmlFor={`${ids}-alias`}>{t("networks.connect.alias.label")}</FieldLabel>
            <Input
              id={`${ids}-alias`}
              value={alias}
              onChange={(e) => setAlias(e.target.value)}
              autoComplete="off"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              className={cn("font-mono", TOUCH_HEIGHT)}
            />
            <FieldDescription>{t("networks.connect.alias.helper")}</FieldDescription>
          </Field>
          <Field data-invalid={errors.ipv4 ? true : undefined}>
            <FieldLabel htmlFor={`${ids}-ipv4`}>{t("networks.connect.ipv4.label")}</FieldLabel>
            <Input
              id={`${ids}-ipv4`}
              value={ipv4}
              onChange={(e) => setIpv4(e.target.value)}
              autoComplete="off"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              aria-invalid={Boolean(errors.ipv4)}
              className={cn("font-mono", TOUCH_HEIGHT)}
            />
            {errors.ipv4 ? <FieldError>{errors.ipv4}</FieldError> : null}
          </Field>
          <Field data-invalid={errors.ipv6 ? true : undefined}>
            <FieldLabel htmlFor={`${ids}-ipv6`}>{t("networks.connect.ipv6.label")}</FieldLabel>
            <Input
              id={`${ids}-ipv6`}
              value={ipv6}
              onChange={(e) => setIpv6(e.target.value)}
              autoComplete="off"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              aria-invalid={Boolean(errors.ipv6)}
              className={cn("font-mono", TOUCH_HEIGHT)}
            />
            {errors.ipv6 ? <FieldError>{errors.ipv6}</FieldError> : null}
          </Field>
        </FieldGroup>
        {needName && subject ? (
          <>
            <Alert role="note">
              <AlertDescription>{t("networks.connect.own")}</AlertDescription>
            </Alert>
            <TypeToConfirm
              name={subject.name}
              value={typed}
              onChange={setTyped}
              disabled={pending}
            />
          </>
        ) : null}
      </fieldset>
      <DialogFooter>
        <Button
          type="button"
          variant="outline"
          className={TOUCH_HEIGHT}
          onClick={onClose}
          disabled={pending}
        >
          {t("networks.connect.cancel")}
        </Button>
        <Button type="submit" className={TOUCH_HEIGHT} disabled={pending || !picked || nameMissing}>
          {pending ? (
            <>
              <Spinner aria-hidden="true" />
              {t("networks.connect.pending")}
            </>
          ) : (
            t("networks.connect.submit")
          )}
        </Button>
      </DialogFooter>
    </form>
  );
}

interface ChoicePickerProps {
  id: string;
  what: "container" | "network";
  choices: readonly Choice[];
  loading: boolean;
  value: string | null;
  onChange: (key: string) => void;
  showNode: boolean;
  disabled: boolean;
}

// The list's filter: the text typed must be inside the name or the node. cmdk's own
// filter is fuzzy and also reads the value, which here is an ID.
function matches(_value: string, search: string, keywords?: string[]): number {
  const q = search.trim().toLowerCase();
  if (q === "") return 1;
  return (keywords ?? []).some((k) => k.toLowerCase().includes(q)) ? 1 : 0;
}

// ChoicePicker is the combobox: a button that opens a searchable list. A name too long
// for its row is cut, and whole in a tooltip. While the choices load the list shows a
// spinner row.
function ChoicePicker({
  id,
  what,
  choices,
  loading,
  value,
  onChange,
  showNode,
  disabled,
}: ChoicePickerProps) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const picked = choices.find((c) => c.key === value);
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button
          id={id}
          type="button"
          variant="outline"
          role="combobox"
          aria-expanded={open}
          disabled={disabled}
          className={cn("w-full justify-between font-normal", TOUCH_HEIGHT)}
        >
          <span className={cn("min-w-0 truncate", !picked && "text-muted-foreground")}>
            {picked ? picked.name : t(`networks.connect.${what}.placeholder`)}
          </span>
          <ChevronsUpDown aria-hidden="true" className="shrink-0 opacity-50" />
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-(--radix-popover-trigger-width) p-0">
        <Command filter={matches}>
          <CommandInput placeholder={t(`networks.connect.${what}.placeholder`)} />
          <CommandList>
            {loading ? (
              <div className="flex items-center justify-center gap-2 py-6 text-sm text-muted-foreground">
                <Spinner aria-hidden="true" />
                {t(`networks.connect.${what}.loading`)}
              </div>
            ) : (
              <>
                <CommandEmpty>
                  {t(
                    choices.length === 0
                      ? `networks.connect.${what}.empty`
                      : `networks.connect.${what}.noMatch`,
                  )}
                </CommandEmpty>
                <CommandGroup>
                  {choices.map((c) => (
                    <CommandItem
                      key={c.key}
                      value={c.key}
                      keywords={[c.name, c.node]}
                      data-checked={c.key === value ? "true" : undefined}
                      className="max-md:min-h-11"
                      onSelect={() => {
                        onChange(c.key);
                        setOpen(false);
                      }}
                    >
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <span className="min-w-0 flex-1 truncate">{c.name}</span>
                        </TooltipTrigger>
                        <TooltipContent className="break-all">{c.name}</TooltipContent>
                      </Tooltip>
                      {showNode && c.node ? (
                        <span className="max-w-32 shrink-0 truncate text-xs text-muted-foreground">
                          {c.node}
                        </span>
                      ) : null}
                      {c.key === value ? <Check aria-hidden="true" /> : null}
                    </CommandItem>
                  ))}
                </CommandGroup>
              </>
            )}
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  );
}
