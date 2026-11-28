#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# Keeps certificate verification on everywhere (D-06, D-10, T-02-32, TLS-12).
#
# 1. Go: the switch that turns off Go's built-in TLS certificate verification is
#    confined to two reviewed files: agent/internal/connect/tls.go (the agent) and
#    ingress/internal/channel/tls.go (the ingress supervisor, D-32). Each one turns
#    the built-in check off only to put a mandatory, stricter one in its place
#    (VerifyConnection: an SPKI pin, a pinned authority verified for the host, or
#    the system roots). Any other Go file that names the setting, in code, a test
#    or a comment, fails the gate: the setting is dangerous enough that every use
#    must be in a place that replaces verification with a check.
# 2. Everything else that talks TLS: no file under e2e/, scripts/ or docs/proxy/,
#    and no Vite config (web/vite*.ts), may switch verification off. Clients trust
#    the server's own certificate, a generated CA or an SPKI pin instead. The
#    patterns are the Playwright, Node, Caddy, Traefik, nginx and curl spellings
#    of "do not verify" (see PATTERNS below), and `secure: false` in web/vite*.ts,
#    where it turns off verification of the dev proxy's upstream. `secure: false`
#    elsewhere (a cookie assertion in an e2e spec) is not a TLS setting and is not
#    matched, nor is docker's --insecure-registry.
# 3. The ingress renderer's goldens: the servers transport of TierPlan's own route
#    (t-self, Phase 28 D-14) never carries the skip-verification key. Each
#    tierplan.yml under internal/ingressrender/testdata/golden is read block by
#    block: the lines of a `t-self:` mapping, up to the next line at the same or a
#    lower indent, must not name the key in any letter case (Traefik reads keys
#    without regard to case). The user transport t-user-https skips verification on
#    purpose (Phase 24 D-13) and is not matched by this rule.
#
# Usage: scripts/insecure-tls-check.sh [directory]
# The directory defaults to the repository root. Dependency and VCS directories
# are not scanned, nor .claude (local agent worktrees). Paths in the output are relative to the directory.
set -euo pipefail

ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
# A space-separated list of the files allowed to name the setting.
ALLOWED="agent/internal/connect/tls.go ingress/internal/channel/tls.go"
# Spelled in two halves so this file does not trip the gate it implements.
SETTING="Insecure""SkipVerify"

cd "${ROOT}"

bad=()
while IFS= read -r file; do
  file="${file#./}"
  case " ${ALLOWED} " in
    *" ${file} "*) continue ;;
  esac
  bad+=("${file}")
done < <(grep -rl --include='*.go' --exclude-dir=.git --exclude-dir=.claude --exclude-dir=node_modules --exclude-dir=vendor -e "${SETTING}" . 2>/dev/null | sort || true)

if [ "${#bad[@]}" -gt 0 ]; then
  for f in "${bad[@]}"; do
    echo "INSECURE TLS: ${f} names ${SETTING}; only ${ALLOWED// /, } may" >&2
  done
  exit 1
fi

# --- Everything that is not Go: e2e, Vite, scripts and the shipped proxy configs ---
# Each entry is "label|extended regex", matched case-insensitively. The names are
# spelled in two halves so this file does not trip the gate it implements.
PATTERNS=(
  "ignoreHTTPS""Errors|ignoreHTTPS""Errors"
  "reject""Unauthorized|reject""Unauthorized"
  "NODE_TLS_REJECT""_UNAUTHORIZED|NODE_TLS_REJECT""_UNAUTHORIZED"
  "tls_insecure_skip""_verify|tls_insecure_skip""_verify"
  "insecure""SkipVerify|insecure""SkipVerify"
  "proxy_ssl_verify"" off|proxy_ssl_verify""[[:space:]]+off"
  # curl given the skip-verification flag on the same line, as a whole word or inside a
  # group of short flags. Ending the flag at a space, quote or line end keeps docker's
  # --insecure-registry out.
  "curl skip-verification flag|curl[^#]*[[:space:]](-[a-zA-Z]*k[a-zA-Z]*|--insec""ure)([[:space:]]|\$|\"|')"
)
SCAN_DIRS=()
for d in e2e scripts docs/proxy; do
  [ -d "${d}" ] && SCAN_DIRS+=("${d}")
done
# The dev server config is the only place `secure: false` is a TLS setting.
VITE_FILES=()
for f in web/vite*.ts; do
  [ -f "${f}" ] && VITE_FILES+=("${f}")
done

hits=0
report() {
  echo "INSECURE TLS: ${1#./} uses $2" >&2
  hits=$((hits + 1))
}
GREP_COMMON=(-rIiE --exclude-dir=.git --exclude-dir=.claude --exclude-dir=node_modules --exclude-dir=vendor --exclude-dir=.devdata)
for entry in "${PATTERNS[@]}"; do
  label="${entry%%|*}"
  regex="${entry#*|}"
  if [ "${#SCAN_DIRS[@]}" -gt 0 ]; then
    while IFS= read -r file; do
      report "${file}" "${label}"
    done < <(grep -l "${GREP_COMMON[@]}" -e "${regex}" "${SCAN_DIRS[@]}" 2>/dev/null | sort || true)
  fi
  if [ "${#VITE_FILES[@]}" -gt 0 ]; then
    while IFS= read -r file; do
      report "${file}" "${label}"
    done < <(grep -lIiE -e "${regex}" "${VITE_FILES[@]}" 2>/dev/null | sort || true)
  fi
done
if [ "${#VITE_FILES[@]}" -gt 0 ]; then
  while IFS= read -r file; do
    report "${file}" "secure: false"
  done < <(grep -lIE -e '(^|[^[:alnum:]_])secure[[:space:]]*:[[:space:]]*false' "${VITE_FILES[@]}" 2>/dev/null | sort || true)
fi

# --- The renderer's goldens: t-self, TierPlan's own route, always verifies (D-14) ---
GOLDEN_DIR="internal/ingressrender/testdata/golden"
# Traefik's spelling of the key, in two halves like SETTING.
TRAEFIK_KEY="insecure""SkipVerify"
if [ -d "${GOLDEN_DIR}" ]; then
  while IFS= read -r file; do
    # Prints 1 when a t-self block (block or flow style, the name quoted or not) names the key.
    found="$(awk -v key="${TRAEFIK_KEY}" '
      function indent(line) { match(line, /^[ ]*/); return RLENGTH }
      {
        if (inblk) {
          if ($0 ~ /^[[:space:]]*(#.*)?$/) next
          if (indent($0) > ind) {
            if (index(tolower($0), tolower(key))) hit = 1
            next
          }
          inblk = 0
        }
        if ($0 ~ /^[ ]*["'"'"']?t-self["'"'"']?[[:space:]]*:/) {
          inblk = 1
          ind = indent($0)
          if (index(tolower($0), tolower(key))) hit = 1
        }
      }
      END { print hit ? 1 : 0 }
    ' "${file}")"
    if [ "${found}" = "1" ]; then
      echo "INSECURE TLS: ${file#./} t-self carries ${TRAEFIK_KEY}" >&2
      hits=$((hits + 1))
    fi
  done < <(find "${GOLDEN_DIR}" -name tierplan.yml -type f 2>/dev/null | sort)
fi
[ "${hits}" -eq 0 ] || exit 1
