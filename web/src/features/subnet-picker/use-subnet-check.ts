// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { checkSubnet, type SubnetCheck, type SubnetCheckRequest } from "@/api/address-space";
import { parseCidr } from "@/features/networks/cidr";

// How long after the last change the server is asked (D-15). The server holds the person to 120 a
// minute, so typing never gets near it.
export const CHECK_DEBOUNCE_MS = 400;

// The wait itself, as a value tests can set to 0 so a test needs no clock. Nothing else changes it.
export const checkTiming = { debounceMs: CHECK_DEBOUNCE_MS };

export interface SubnetCheckInput {
  // The subnet as typed. A string that is not a prefix is not sent: the form says what is wrong
  // with it, and the server is asked about the tasks alone.
  subnet: string;
  // How many tasks are expected, when the person gave a number that fits.
  tasks?: number;
  // The driver and, for a local one on a swarm, the node: they decide which pools and which
  // networks the subnet is checked against.
  driver?: SubnetCheckRequest["driver"];
  nodeId?: string;
}

export interface SubnetCheckState {
  // The last answer, null when there is nothing to check, while nothing has answered yet and
  // after a check that failed.
  data: SubnetCheck | null;
  // True from a change until the answer to it has come.
  pending: boolean;
  // True when the last check could not be made. It never blocks anything.
  error: boolean;
  // Asks for the lowest free block of a mask. It resolves with the answer, or null when the
  // question failed (suggestError is then true).
  suggest: (mask: number) => Promise<SubnetCheck | null>;
  suggesting: boolean;
  suggestError: boolean;
  clearSuggestError: () => void;
}

// useSubnetCheck asks the server what the typed subnet collides with and what size the expected
// tasks need, 400 ms after the last change. A newer question cancels a stale one, and the
// question after a Suggest runs at once. It reads only, so a repeat is harmless.
export function useSubnetCheck(
  envId: string,
  input: SubnetCheckInput,
  enabled = true,
): SubnetCheckState {
  const [data, setData] = useState<SubnetCheck | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState(false);
  const [suggesting, setSuggesting] = useState(false);
  const [suggestError, setSuggestError] = useState(false);
  // The subnet a Suggest just put in the field: the check of it needs no wait.
  const immediate = useRef<string | null>(null);
  const suggestion = useRef<AbortController | null>(null);

  const typed = input.subnet.trim();
  const subnet = parseCidr(typed) === null ? undefined : typed;
  const { tasks, driver, nodeId } = input;
  const body = useMemo<SubnetCheckRequest | null>(
    () =>
      subnet === undefined && tasks === undefined
        ? null
        : {
            ...(driver ? { driver } : {}),
            ...(nodeId ? { nodeId } : {}),
            ...(subnet ? { subnet } : {}),
            ...(tasks === undefined ? {} : { tasks }),
          },
    [subnet, tasks, driver, nodeId],
  );

  useEffect(() => {
    if (!enabled || body === null) {
      setData(null);
      setPending(false);
      setError(false);
      return;
    }
    const controller = new AbortController();
    setPending(true);
    const now = immediate.current !== null && immediate.current === subnet;
    immediate.current = null;
    const timer = setTimeout(
      async () => {
        try {
          const answer = await checkSubnet(envId, body, controller.signal);
          if (controller.signal.aborted) return;
          setData(answer);
          setError(false);
        } catch {
          if (controller.signal.aborted) return;
          setData(null);
          setError(true);
        }
        setPending(false);
      },
      now ? 0 : checkTiming.debounceMs,
    );
    return () => {
      clearTimeout(timer);
      controller.abort();
    };
  }, [envId, body, enabled, subnet]);

  useEffect(() => () => suggestion.current?.abort(), []);

  const suggest = useCallback(
    async (mask: number): Promise<SubnetCheck | null> => {
      suggestion.current?.abort();
      const controller = new AbortController();
      suggestion.current = controller;
      setSuggesting(true);
      setSuggestError(false);
      try {
        const answer = await checkSubnet(
          envId,
          {
            ...(driver ? { driver } : {}),
            ...(nodeId ? { nodeId } : {}),
            ...(tasks === undefined ? {} : { tasks }),
            mask,
            suggest: true,
          },
          controller.signal,
        );
        if (controller.signal.aborted) return null;
        if (answer.suggestion) immediate.current = answer.suggestion.subnet;
        setSuggesting(false);
        return answer;
      } catch {
        if (controller.signal.aborted) return null;
        setSuggesting(false);
        setSuggestError(true);
        return null;
      }
    },
    [envId, driver, nodeId, tasks],
  );

  const clearSuggestError = useCallback(() => setSuggestError(false), []);

  return { data, pending, error, suggest, suggesting, suggestError, clearSuggestError };
}
