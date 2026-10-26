# shellcheck shell=bash
# ---------------------------------------------------------------
# lib-state-sync.sh — reconcile Terraform state with the REAL cloud
# environment on every create.sh / destroy.sh run.
#
# Why this exists: state here is LOCAL (no remote backend), and the
# expensive stateful resources are frequently changed out-of-band
# (console / kubectl / manual ops). When that happens the local state
# diverges from reality and a plain `terraform apply` either reports
# "... already exists" (resource exists in the cloud but is missing
# from state) or works off stale attributes. This library closes that
# gap automatically, in two layers, scoped to the stateful resources:
#
#   tencentcloud_mysql_instance.mysql
#   tencentcloud_redis_instance.redis
#   tencentcloud_kubernetes_cluster.tke[0]
#   tencentcloud_kubernetes_node_pool.tke[0]
#   tencentcloud_instance.jumpserver
#   tencentcloud_instance.compute[*]
#
#   Layer 1 (ss_refresh_stateful): `terraform apply -refresh-only` over
#     the in-state subset — pulls out-of-band ATTRIBUTE changes into
#     state and drops resources deleted out-of-band. Never creates or
#     destroys anything (refresh-only).
#
#   Layer 2 (ss_import_stateful): for each stateful address MISSING from
#     state, discover the real cloud id (via tccli) and `terraform
#     import` it, so apply/destroy operate on it instead of colliding
#     with it.
#
# Everything here is BEST-EFFORT: if no tccli runner is reachable, jq is
# absent, or a discovery/import fails, the step is skipped with a note
# and the caller proceeds exactly as before. No function aborts the
# parent script (all return 0), which matters under `set -euo pipefail`.
#
# Deliberately scoped to the stateful set: cheap/derivable resources
# (networking, k8s objects) are recreated quickly by the normal flow and
# secret-bearing resources (mysql_account, tcr_token, kubernetes_secret)
# cannot be imported faithfully because the API never returns their
# plaintext — importing them would only plant blanks.
#
# Reads globals from the including script: SSH_PRI_KEY, and the colour
# vars (CYAN/GREEN/YELLOW/NC) when present. Resolves region, jumpserver
# IP and credentials itself so it is independent of the caller's flow.
# ---------------------------------------------------------------

# ---------------------------------------------------------------
# Shared console helpers (used by create.sh and destroy.sh). _draw_box relies on
# the colour vars (e.g. NC) defined by the including script at call time.
# ---------------------------------------------------------------
# _display_width — return the terminal column width of a string. ASCII counts as
#   1; only genuinely East-Asian-Wide / fullwidth / emoji code points count as 2.
#   Narrow symbols that most terminals render in a single column (arrows →, check
#   marks ✓, ≥, bullets •, accents, …) are correctly counted as 1, so box-drawing
#   borders line up. Requires a UTF-8 locale (bash indexes $s by character and
#   `printf %d "'<char>"` yields the Unicode code point).
_display_width() {
	local s="$1" w=0 i ch cp
	for ((i = 0; i < ${#s}; i++)); do
		ch="${s:i:1}"
		if [[ "$ch" == [[:ascii:]] ]]; then
			w=$((w + 1))
			continue
		fi
		printf -v cp '%d' "'$ch"
		# Wide ranges (no inline comments allowed inside (( ))): Hangul Jamo;
		# CJK radicals..symbols; Kana..CJK compat; CJK ext A; CJK unified; Yi;
		# Hangul syllables; CJK compat ideographs; vertical/compat forms;
		# fullwidth forms & signs; emoji & pictographs; CJK ext B+.
		if ((cp >= 0x1100 && (cp <= 0x115F ||
			(cp >= 0x2E80 && cp <= 0x303E) ||
			(cp >= 0x3041 && cp <= 0x33FF) ||
			(cp >= 0x3400 && cp <= 0x4DBF) ||
			(cp >= 0x4E00 && cp <= 0x9FFF) ||
			(cp >= 0xA000 && cp <= 0xA4CF) ||
			(cp >= 0xAC00 && cp <= 0xD7A3) ||
			(cp >= 0xF900 && cp <= 0xFAFF) ||
			(cp >= 0xFE10 && cp <= 0xFE19) ||
			(cp >= 0xFE30 && cp <= 0xFE6F) ||
			(cp >= 0xFF00 && cp <= 0xFF60) ||
			(cp >= 0xFFE0 && cp <= 0xFFE6) ||
			(cp >= 0x1F300 && cp <= 0x1FAFF) ||
			(cp >= 0x20000 && cp <= 0x3FFFD)))); then
			w=$((w + 2))
		else
			w=$((w + 1))
		fi
	done
	printf '%d' "$w"
}

# _draw_box — print the given lines inside an aligned box drawn with ─│┌┐└┘.
#   The inner width is computed from the widest line (display width), so the
#   right border lines up even when lines contain emoji/CJK/wide symbols.
#   Usage: _draw_box "${COLOR}" "line 1" "line 2" ...
_draw_box() {
	local color="$1"
	shift
	local -a lines=("$@")
	local pad=2 # spaces inside the box on each side of the text
	local maxw=0 lw
	local line
	for line in "${lines[@]}"; do
		lw=$(_display_width "$line")
		[ "$lw" -gt "$maxw" ] && maxw="$lw"
	done
	local inner=$((maxw + pad * 2))

	# Build the horizontal border of the right length
	local bar="" i
	for ((i = 0; i < inner; i++)); do
		bar="${bar}─"
	done

	echo -e "${color}┌${bar}┐${NC}"
	for line in "${lines[@]}"; do
		lw=$(_display_width "$line")
		local right=$((inner - pad - lw))
		printf "${color}│%*s%s%*s│${NC}\n" "$pad" "" "$line" "$right" ""
	done
	echo -e "${color}└${bar}┘${NC}"
}

# ---------------------------------------------------------------
# Shared .env loader (used by create.sh's load_saved_env and destroy.sh).
# ---------------------------------------------------------------
# _env_value "KEY='value'  # comment" — extract the VALUE from one .env line,
#   stripping the surrounding single quotes AND ignoring any trailing inline
#   "# comment". Entries are written as KEY='value'; a few legitimately carry a
#   trailing "# ..." note: env.example ships them (e.g. on
#   TENCENTCLOUD_COMPUTE_INSTANCE_TYPE / TENCENTCLOUD_COMPUTE_NODE_COUNT) and is
#   meant to be copied to .env, and .env files written by older create.sh
#   versions appended such a note too. Without dropping it the note would leak
#   into the value (and, on those legacy files, grow on every save/load
#   round-trip). Echoes the cleaned value.
_env_value() {
	local val="${1#*=}"
	# Trim leading whitespace so the quote/comment handling below is reliable.
	val="${val#"${val%%[![:space:]]*}"}"
	case "$val" in
	\'*)
		# Single-quoted value (how every entry is written): take only what is
		# INSIDE the quotes — so a trailing "# comment" is ignored, and a value a
		# previous (buggy) load had appended the comment into is repaired. Values
		# never contain a single quote in this scheme.
		val="${val#\'}"
		val="${val%%\'*}"
		;;
	*)
		# Unquoted value (hand-edited .env): drop a shell-style inline comment
		# (whitespace followed by '#') and trim surrounding whitespace. A '#' not
		# preceded by whitespace stays literal (e.g. inside a password).
		val="${val%%[[:space:]]#*}"
		val="${val#"${val%%[![:space:]]*}"}"
		val="${val%"${val##*[![:space:]]}"}"
		;;
	esac
	printf '%s' "$val"
}

# _load_env_file FILE — preload KEY='value' selections from a saved .env, filling
#   in ONLY variables that are not already set in the environment (an explicit
#   override always wins). No-op (returns 0) when FILE is absent. Each line is
#   parsed with _env_value. Shared by create.sh's load_saved_env and destroy.sh
#   so both honor the exact same .env format from a single place.
_load_env_file() {
	local file="$1" line key
	[ -f "$file" ] || return 0
	while IFS= read -r line || [ -n "$line" ]; do
		case "$line" in
		"" | \#*) continue ;;
		esac
		key="${line%%=*}"
		# Only fill in variables that are not already set in the environment.
		[ -n "${!key:-}" ] && continue
		export "${key}=$(_env_value "$line")"
	done <"$file"
}

# Resource addresses this library manages. Order matters for import:
# the node pool needs its cluster id, which is cheapest to read once the
# cluster is in state.
SS_STATEFUL_RESOURCES=(
	tencentcloud_mysql_instance.mysql
	tencentcloud_redis_instance.redis
	tencentcloud_kubernetes_cluster.tke
	tencentcloud_kubernetes_node_pool.tke
	tencentcloud_instance.jumpserver
	tencentcloud_instance.compute
)

# Cloud-side names the resources are created with (see main.tf). Used to
# look an existing resource up by name when it is missing from state.
SS_MYSQL_NAME="cubesandbox-mysql"
SS_REDIS_NAME="cubesandbox-redis"
SS_JUMPSERVER_NAME="cubesandbox-jumpserver"
SS_COMPUTE_NAME="cubesandbox-compute"

# Report accumulators, populated during the import layer and rendered by
# ss_print_sync_summary: addresses adopted from the cloud this run
# (supplemented) and addresses that exist in the cloud but could not be imported
# (failed). SS_DISCOVERY_RAN flips to 1 once a tccli runner + jq were available,
# so the summary can tell "absent in cloud" apart from "cloud not checked".
SS_R_SUPPLEMENTED=()
SS_R_FAILED=()
SS_DISCOVERY_RAN=0

ss_log()  { echo -e "  ${CYAN:-}$*${NC:-}"; }
ss_ok()   { echo -e "  ${GREEN:-}$*${NC:-}"; }
ss_warn() { echo -e "  ${YELLOW:-}$*${NC:-}"; }

# ss_in_list NEEDLE [ELEM...] — true when NEEDLE equals one of the ELEMs.
ss_in_list() {
	local needle="$1" e
	shift
	for e in "$@"; do
		[ "$e" = "$needle" ] && return 0
	done
	return 1
}

# ss_region — resolved Tencent Cloud region (matches setup_env's mapping
# and the provider default).
ss_region() {
	echo "${TF_VAR_region:-${TENCENTCLOUD_REGION:-ap-guangzhou}}"
}

# ss_js_ip — jumpserver public IP from terraform outputs (only present
# once the jumpserver is in state). Empty on a first creation.
ss_js_ip() {
	terraform output -raw jumpserver_public_ip 2>/dev/null || true
}

# ss_have_runner — true when a tccli runner is reachable: either the
# jumpserver (SSH on 443) or a locally-installed tccli. Discovery is
# impossible without one, so the import layer no-ops when this is false.
ss_have_runner() {
	local js
	js="$(ss_js_ip)"
	if [ -n "$js" ] && nc -z -w 3 "$js" 443 2>/dev/null; then
		return 0
	fi
	command -v tccli >/dev/null 2>&1
}

# ss_tccli — run a tccli command with credentials + region. Prefers the
# jumpserver (same VPC/region; installs tccli there on demand) and falls
# back to a local tccli. Echoes raw JSON; returns 127 when no runner is
# available. Mirrors destroy.sh's tccli_run but is self-contained so the
# two scripts share one implementation.
ss_tccli() {
	local region js a args_str="" script _to
	region="$(ss_region)"
	js="$(ss_js_ip)"
	if [ -n "$js" ] && nc -z -w 3 "$js" 443 2>/dev/null; then
		# Feed a remote script over stdin to `bash -s` so the cloud credentials
		# travel as exported env vars INSIDE the piped script rather than on the
		# remote command line (argv is world-readable via `ps`; CWE-214). printf
		# is a shell builtin, so the secrets are not exposed in local argv either.
		for a in "$@"; do args_str+=$(printf ' %q' "$a"); done
		printf -v script 'export TENCENTCLOUD_SECRET_ID=%q TENCENTCLOUD_SECRET_KEY=%q TENCENTCLOUD_REGION=%q\ncommand -v tccli >/dev/null 2>&1 || pip3 install -q tccli -i https://mirrors.tencent.com/pypi/simple/ >/dev/null 2>&1\ntccli%s\n' \
			"${TENCENTCLOUD_SECRET_ID:-}" "${TENCENTCLOUD_SECRET_KEY:-}" "${region}" "${args_str}"
		_to=""
		command -v timeout >/dev/null 2>&1 && _to="timeout 240"
		printf '%s' "$script" | $_to ssh -i "${SSH_PRI_KEY:-}" -p 443 \
			-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
			-o ConnectTimeout=15 -o ServerAliveInterval=15 -o ServerAliveCountMax=4 \
			-o BatchMode=yes -o LogLevel=ERROR \
			root@"${js}" "bash -s" 2>/dev/null
		return $?
	fi
	if command -v tccli >/dev/null 2>&1; then
		env TENCENTCLOUD_SECRET_ID="${TENCENTCLOUD_SECRET_ID:-}" \
			TENCENTCLOUD_SECRET_KEY="${TENCENTCLOUD_SECRET_KEY:-}" \
			TENCENTCLOUD_REGION="${region}" \
			tccli "$@" 2>/dev/null
		return $?
	fi
	return 127
}

# ss_resource_in_state RES — true when at least one instance of resource
# RES is tracked in state (matches "RES", "RES[0]", 'RES["k"]').
ss_resource_in_state() {
	terraform state list 2>/dev/null | grep -qE "^$(ss_escape_re "$1")(\[|\$)"
}

# ss_addr_in_state ADDR — true when the EXACT address (e.g. compute[0])
# is in state.
ss_addr_in_state() {
	terraform state list 2>/dev/null | grep -qxF "$1"
}

# ss_escape_re — escape regex metacharacters in a literal terraform
# address so it can anchor a grep -E pattern.
ss_escape_re() {
	printf '%s' "$1" | sed 's/[.[\*^$()+?{}|]/\\&/g'
}

# ss_state_id ADDR — the cloud id stored for ADDR in state, or empty.
ss_state_id() {
	terraform state show "$1" 2>/dev/null |
		awk -F'"' '/^[[:space:]]*id[[:space:]]*=/{print $2; exit}'
}

# ---------------------------------------------------------------
# Layer 1 — refresh
# ---------------------------------------------------------------

# ss_refresh_stateful — `terraform apply -refresh-only` over the stateful
# resources that are currently in state. Pulls out-of-band attribute
# changes into state and prunes resources deleted out-of-band. Never
# proposes create/destroy (refresh-only). Targeted at the cloud
# resources only, so it needs neither cluster connectivity nor the k8s
# provider.
ss_refresh_stateful() {
	local -a targets=()
	local res
	for res in "${SS_STATEFUL_RESOURCES[@]}"; do
		if ss_resource_in_state "$res"; then
			targets+=("-target=${res}")
		fi
	done
	if [ "${#targets[@]}" -eq 0 ]; then
		return 0
	fi

	# Match config count to state so refresh-only doesn't warn about
	# instances "not in configuration": create_tke when the cluster is in
	# state, and compute_node_count to the number of compute nodes in
	# state. These overrides are scoped to this one command.
	local create_tke="false" compute_count
	if ss_resource_in_state tencentcloud_kubernetes_cluster.tke; then
		create_tke="true"
	fi
	compute_count="$(terraform state list 2>/dev/null | grep -cE '^tencentcloud_instance\.compute\[' || true)"
	compute_count="${compute_count:-0}"

	ss_log "Refreshing state from the real environment (refresh-only, ${#targets[@]} stateful target(s))..."
	TF_VAR_create_tke="$create_tke" TF_VAR_compute_node_count="$compute_count" \
		terraform apply -refresh-only -auto-approve -input=false "${targets[@]}" >/dev/null 2>&1 ||
		ss_warn "refresh-only reported issues (continuing); re-run with TENCENTCLOUD_VERBOSE=1 to see details"
	return 0
}

# ---------------------------------------------------------------
# Layer 2 — import (cloud has it, state doesn't)
# ---------------------------------------------------------------

# ss_import ADDR IMPORT_ID [VAR=val ...] — import ADDR unless it is
# already in state. Extra args are per-command env overrides (e.g.
# TF_VAR_create_tke=true so a count-gated address resolves). Best-effort.
ss_import() {
	local addr="$1" id="$2"
	shift 2
	if [ -z "$id" ]; then
		return 0
	fi
	if ss_addr_in_state "$addr"; then
		return 0
	fi
	ss_log "Importing out-of-band ${addr} <- ${id}"
	if env "$@" terraform import -input=false "$addr" "$id" >/dev/null 2>&1; then
		ss_ok "✓ imported ${addr}"
		SS_R_SUPPLEMENTED+=("$addr")
	else
		ss_warn "⚠ import ${addr} failed (best-effort, skipping); re-run with TENCENTCLOUD_VERBOSE=1 for details"
		SS_R_FAILED+=("$addr")
	fi
	return 0
}

ss_discover_mysql_id() {
	ss_tccli cdb DescribeDBInstances --InstanceNames "$SS_MYSQL_NAME" 2>/dev/null |
		jq -r '.Items[]? | .InstanceId' 2>/dev/null | head -n1 || true
}

ss_discover_redis_id() {
	ss_tccli redis DescribeInstances --Limit 100 2>/dev/null |
		jq -r --arg n "$SS_REDIS_NAME" '.InstanceSet[]? | select(.InstanceName==$n) | .InstanceId' 2>/dev/null |
		head -n1 || true
}

ss_discover_tke_cluster_id() {
	ss_tccli tke DescribeClusters --Limit 100 2>/dev/null |
		jq -r --arg n "${TF_VAR_tke_cluster_name:-cubesandbox-terraform-tke}" \
			'.Clusters[]? | select(.ClusterName==$n) | .ClusterId' 2>/dev/null | head -n1 || true
}

ss_discover_tke_nodepool_id() {
	local cluster_id="$1"
	[ -z "$cluster_id" ] && return 0
	ss_tccli tke DescribeClusterNodePools --ClusterId "$cluster_id" 2>/dev/null |
		jq -r --arg n "${TF_VAR_tke_cluster_name:-cubesandbox-terraform-tke}-pool" \
			'.NodePoolSet[]? | select(.Name==$n) | .NodePoolId' 2>/dev/null | head -n1 || true
}

# ss_discover_cvm_ids NAME — space-separated ids of RUNNING/STARTING CVMs
# named NAME, sorted for a deterministic index mapping.
ss_discover_cvm_ids() {
	local name="$1"
	ss_tccli cvm DescribeInstances --Limit 100 \
		--Filters "[{\"Name\":\"instance-name\",\"Values\":[\"${name}\"]}]" 2>/dev/null |
		jq -r '.InstanceSet[]? | select(.InstanceState!="TERMINATED") | .InstanceId' 2>/dev/null |
		sort | tr '\n' ' ' || true
}

# ss_import_stateful — adopt stateful resources that exist in the cloud
# but are missing from state.
ss_import_stateful() {
	if ! ss_have_runner; then
		ss_warn "No tccli runner (jumpserver unreachable and no local tccli); skipping cloud import."
		return 0
	fi
	if ! command -v jq >/dev/null 2>&1; then
		ss_warn "jq not found locally; skipping cloud import (discovery needs jq)."
		return 0
	fi
	SS_DISCOVERY_RAN=1

	# Singletons.
	ss_addr_in_state tencentcloud_mysql_instance.mysql ||
		ss_import tencentcloud_mysql_instance.mysql "$(ss_discover_mysql_id)"
	ss_addr_in_state tencentcloud_redis_instance.redis ||
		ss_import tencentcloud_redis_instance.redis "$(ss_discover_redis_id)"
	ss_addr_in_state tencentcloud_instance.jumpserver ||
		ss_import tencentcloud_instance.jumpserver \
			"$(ss_discover_cvm_ids "$SS_JUMPSERVER_NAME" | awk '{print $1}')"

	# TKE cluster + node pool are count-gated on var.create_tke, so the
	# address only resolves with create_tke=true; pass it for the import
	# command. Discovery returns empty when no such cluster exists, so a
	# non-TKE deployment self-skips.
	local cluster_id
	cluster_id="$(ss_state_id "tencentcloud_kubernetes_cluster.tke[0]")"
	if [ -z "$cluster_id" ]; then
		cluster_id="$(ss_discover_tke_cluster_id)"
		ss_import "tencentcloud_kubernetes_cluster.tke[0]" "$cluster_id" TF_VAR_create_tke=true
	fi
	if [ -n "$cluster_id" ] && ! ss_addr_in_state "tencentcloud_kubernetes_node_pool.tke[0]"; then
		local np_id
		np_id="$(ss_discover_tke_nodepool_id "$cluster_id")"
		if [ -n "$np_id" ]; then
			ss_import "tencentcloud_kubernetes_node_pool.tke[0]" "${cluster_id}#${np_id}" TF_VAR_create_tke=true
		fi
	fi

	ss_import_compute
	return 0
}

# ss_import_compute — fill the MISSING compute[i] slots (within the
# desired count) with cloud compute nodes not already tracked in state.
# Capped at the desired count so we never adopt extras that a later apply
# would then destroy. No-op when the desired count is unknown/zero.
ss_import_compute() {
	local desired="${TF_VAR_compute_node_count:-0}"
	case "$desired" in
	'' | *[!0-9]*) desired=0 ;;
	esac
	[ "$desired" -gt 0 ] 2>/dev/null || return 0

	# Ids already tracked in any compute[i] slot. Use a string set instead of
	# a bash 4 associative array so macOS bash 3.2 works.
	local managed=" "
	local addr id idx
	while IFS= read -r addr; do
		[ -n "$addr" ] || continue
		id="$(ss_state_id "$addr")"
		if [ -n "$id" ]; then
			managed="${managed}${id} "
		fi
	done < <(terraform state list 2>/dev/null | grep -E '^tencentcloud_instance\.compute\[' || true)

	# Cloud compute ids not yet tracked anywhere.
	local -a unmanaged=()
	for id in $(ss_discover_cvm_ids "$SS_COMPUTE_NAME"); do
		[ -n "$id" ] || continue
		case "$managed" in
		*" ${id} "*) ;;
		*) unmanaged+=("$id") ;;
		esac
	done
	[ "${#unmanaged[@]}" -gt 0 ] || return 0

	# Pair each unmanaged cloud id with the next empty slot < desired.
	local u=0
	for ((idx = 0; idx < desired && u < ${#unmanaged[@]}; idx++)); do
		ss_addr_in_state "tencentcloud_instance.compute[${idx}]" && continue
		ss_import "tencentcloud_instance.compute[${idx}]" "${unmanaged[$u]}" \
			"TF_VAR_compute_node_count=${desired}"
		u=$((u + 1))
	done
	return 0
}

# ---------------------------------------------------------------
# Summary
# ---------------------------------------------------------------

# ss_print_sync_summary — after refresh+import, report each managed stateful
# resource as one of:
#   ✓ satisfied      — already tracked in state (nothing to do)
#   + supplemented   — existed in the cloud but missing from state → imported
#   ✗ import failed  — exists in the cloud but could not be adopted
#   · to be created  — absent from both state and cloud → the apply will create it
#                      (shown as "cloud not checked" when no tccli runner was
#                      available, since we could not confirm it is truly absent)
# The desired set mirrors what the upcoming apply targets: the singletons, the
# selected compute-node count (and any compute already in state), and the TKE
# cluster + node pool when this deployment manages TKE.
ss_print_sync_summary() {
	local -a desired=(
		tencentcloud_mysql_instance.mysql
		tencentcloud_redis_instance.redis
		tencentcloud_instance.jumpserver
	)

	# Compute slots: cover the selected count and anything already in state.
	local want instate n i
	want="${TF_VAR_compute_node_count:-0}"
	case "$want" in '' | *[!0-9]*) want=0 ;; esac
	instate="$(terraform state list 2>/dev/null | grep -cE '^tencentcloud_instance\.compute\[' || true)"
	instate="${instate:-0}"
	n="$want"
	if [ "$instate" -gt "$n" ] 2>/dev/null; then n="$instate"; fi
	for ((i = 0; i < n; i++)); do desired+=("tencentcloud_instance.compute[${i}]"); done

	# TKE only when this deployment manages it (intended, or already in state).
	if [ "${TF_VAR_create_tke:-}" = "true" ] || [ "${TF_VAR_create_tke:-}" = "1" ] ||
		ss_addr_in_state "tencentcloud_kubernetes_cluster.tke[0]"; then
		desired+=(
			tencentcloud_kubernetes_cluster.tke[0]
			tencentcloud_kubernetes_node_pool.tke[0]
		)
	fi

	local -a sat=() sup=() fail=() pend=()
	local addr
	for addr in "${desired[@]}"; do
		if ss_in_list "$addr" "${SS_R_FAILED[@]+"${SS_R_FAILED[@]}"}"; then
			fail+=("$addr")
		elif ss_in_list "$addr" "${SS_R_SUPPLEMENTED[@]+"${SS_R_SUPPLEMENTED[@]}"}"; then
			sup+=("$addr")
		elif ss_addr_in_state "$addr"; then
			sat+=("$addr")
		else
			pend+=("$addr")
		fi
	done

	ss_log "State sync summary:"
	# The same reconciliation runs ahead of both create and destroy, so adapt the
	# wording to the caller's intent (SS_MODE): for a destroy the "absent in cloud"
	# resources are already gone (nothing to destroy) rather than pending creation.
	if [ "${SS_MODE:-create}" = "destroy" ]; then
		ss_print_group "${CYAN:-}"   "Tracked in state (will be destroyed)"                          "•" "${sat[@]+"${sat[@]}"}"
		ss_print_group "${CYAN:-}"   "Adopted from cloud, were missing from state (will be destroyed)" "+" "${sup[@]+"${sup[@]}"}"
		if [ "${#fail[@]}" -gt 0 ]; then
			ss_print_group "${YELLOW:-}" "Exist in cloud but could not be adopted (will NOT be destroyed)" "✗" "${fail[@]+"${fail[@]}"}"
		fi
		if [ "$SS_DISCOVERY_RAN" = "1" ]; then
			ss_print_group "${GREEN:-}" "Already absent in cloud (nothing to destroy)" "✓" "${pend[@]+"${pend[@]}"}"
		elif [ "${#pend[@]}" -gt 0 ]; then
			ss_print_group "${YELLOW:-}" "Not in state (cloud not checked — no tccli runner)" "?" "${pend[@]+"${pend[@]}"}"
		fi
	else
		ss_print_group "${GREEN:-}"  "Already satisfied (tracked in state)" "✓" "${sat[@]+"${sat[@]}"}"
		ss_print_group "${GREEN:-}"  "Supplemented (imported from cloud, were missing from state)" "+" "${sup[@]+"${sup[@]}"}"
		if [ "${#fail[@]}" -gt 0 ]; then
			ss_print_group "${YELLOW:-}" "Exist in cloud but could not be adopted (import failed)" "✗" "${fail[@]+"${fail[@]}"}"
		fi
		if [ "$SS_DISCOVERY_RAN" = "1" ]; then
			ss_print_group "${YELLOW:-}" "To be created by terraform apply (absent in cloud)" "·" "${pend[@]+"${pend[@]}"}"
		elif [ "${#pend[@]}" -gt 0 ]; then
			ss_print_group "${YELLOW:-}" "Not yet in state (cloud not checked — no tccli runner)" "?" "${pend[@]+"${pend[@]}"}"
		fi
	fi
}

# ss_print_group COLOR HEADER ICON [ADDR...] — print one summary category: a
# "HEADER: <count>" line followed by one indented "<icon> <addr>" line per
# resource. Tolerates an empty resource list (count 0, no detail lines).
ss_print_group() {
	local color="$1" header="$2" icon="$3"; shift 3
	echo -e "    ${color}${header}: $#${NC:-}"
	local addr
	for addr in "$@"; do echo -e "      ${color}${icon} ${addr}${NC:-}"; done
}

# ---------------------------------------------------------------
# Orchestrator
# ---------------------------------------------------------------

# ss_sync_state — reconcile state with reality before plan/apply/destroy.
# Refresh first (adopt attribute drift / deletions on resources we
# already track), then import (adopt resources that exist only in the
# cloud). Safe to call on a first creation: with empty state and no
# runner both layers no-op. Always returns 0.
ss_sync_state() {
	# A first creation has no state file at all → nothing to reconcile.
	if ! terraform state list >/dev/null 2>&1; then
		return 0
	fi
	if [ -z "$(terraform state list 2>/dev/null)" ]; then
		return 0
	fi
	ss_log "Syncing Terraform state with the real environment..."
	ss_refresh_stateful
	ss_import_stateful
	ss_print_sync_summary
	ss_ok "✓ State sync complete"
	return 0
}
