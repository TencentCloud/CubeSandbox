#!/usr/bin/env bash

set -euo pipefail

# Owner-only perms for anything written locally during teardown (the rewritten
# kubeconfig, destroy logs from mktemp, any refreshed Terraform state that still
# holds secrets), matching create.sh.
umask 077

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

# Set to 1 by best-effort cleanups (recycle bin, etc.) that could not confirm a
# resource was released, so the final summary reminds the user to delete it by
# hand in the console and avoid being billed for orphans.
NEEDS_MANUAL_CLEANUP=0

# Reconcile local Terraform state with the real cloud environment before
# destroying, so out-of-band stateful resources (existing in the cloud
# but missing from state) are adopted and torn down too. See the file
# header for the full rationale.
# shellcheck source=./lib-state-sync.sh
source "${SCRIPT_DIR}/lib-state-sync.sh"

# Preload the selections create.sh saved to .env (credentials, region, passwords,
# etc.) so destroy uses the exact same configuration as the deployment and avoids
# plan drift. Values already set in the environment take precedence (the file only
# fills in what is unset), matching create.sh's load_saved_env.
ENV_FILE="${SCRIPT_DIR}/.env"
if [ -f "${ENV_FILE}" ]; then
	echo -e "${CYAN}Loading saved selections from ${ENV_FILE} (only for unset values)...${NC}"
	# Parse loop lives in lib-state-sync.sh so create.sh and destroy.sh stay in sync.
	_load_env_file "${ENV_FILE}"
fi

# Destroy needs the same credentials and variable mapping as create.sh; otherwise
# the provider cannot locate the resources, and missing vars such as the SSH key /
# region would diverge from the existing state and cause plan drift.
if [ -z "${TENCENTCLOUD_SECRET_ID:-}" ] || [ -z "${TENCENTCLOUD_SECRET_KEY:-}" ]; then
	echo -e "${RED}Error: please set the Tencent Cloud API credentials first${NC}"
	echo "  export TENCENTCLOUD_SECRET_ID=\"your-secret-id\""
	echo "  export TENCENTCLOUD_SECRET_KEY=\"your-secret-key\""
	echo ""
	echo -e "  ${CYAN}Create an API key pair (SecretId / SecretKey) in the console:${NC}"
	echo -e "  ${CYAN}  https://console.cloud.tencent.com/cam/capi${NC}"
	echo -e "  ${CYAN}For the other supported variables, see ${SCRIPT_DIR}/env.example${NC}"
	exit 1
fi

[ -n "${TENCENTCLOUD_REGION:-}" ] && export TF_VAR_region="$TENCENTCLOUD_REGION"
SSH_PUB_KEY="${TENCENTCLOUD_SSH_PUBLIC_KEY_PATH:-$SCRIPT_DIR/.ssh/id_rsa.pub}"
SSH_PRI_KEY="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SCRIPT_DIR/.ssh/id_rsa}"
export TF_VAR_ssh_public_key_path="$SSH_PUB_KEY"
export TF_VAR_ssh_private_key_path="$SSH_PRI_KEY"
[ -n "${TENCENTCLOUD_MYSQL_PASSWORD:-}" ] && export TF_VAR_mysql_root_password="$TENCENTCLOUD_MYSQL_PASSWORD"
[ -n "${TENCENTCLOUD_REDIS_PASSWORD:-}" ] && export TF_VAR_redis_password="$TENCENTCLOUD_REDIS_PASSWORD"
# Mirror create.sh's setup_env topology mapping. ss_sync_state below calls
# ss_import_compute(), which only adopts out-of-band compute CVMs up to
# TF_VAR_compute_node_count (default 0 = none). Without re-exporting the saved
# count here, an out-of-band compute node would not be imported before destroy
# and would survive as a still-billed orphan. tke_node_count is mapped too so
# the desired topology matches the state.
[ -n "${TENCENTCLOUD_COMPUTE_NODE_COUNT:-}" ] && export TF_VAR_compute_node_count="$TENCENTCLOUD_COMPUTE_NODE_COUNT"
export TF_VAR_compute_node_count="${TF_VAR_compute_node_count:-${TENCENTCLOUD_COMPUTE_NODE_COUNT:-2}}"
export TF_VAR_tke_node_count="${TF_VAR_tke_node_count:-${TENCENTCLOUD_TKE_NODE_COUNT:-2}}"

mkdir -p "$SCRIPT_DIR/.kube"

# Make sure the provider plugins are installed (the kubernetes provider requires
# the .kube directory to already exist at init time). When running straight from
# the extracted bundle without having run create.sh yet, .terraform may not exist.
if [ ! -d "$SCRIPT_DIR/.terraform" ]; then
	echo -e "${CYAN}terraform init...${NC}"
	terraform init -input=false >/dev/null || {
		echo -e "${RED}✗ terraform init failed${NC}"
		exit 1
	}
fi

# Reconcile state with the real environment BEFORE the destroy: import
# any stateful resources that exist in the cloud but are missing from the
# local state (out-of-band creations) so `terraform destroy` tears them
# down too instead of leaving paid orphans. Runs before the TKE-cluster
# detection below on purpose, so an imported out-of-band cluster is seen
# there and gets create_tke=true. Best-effort; no-op without a runner.
# SS_MODE=destroy makes the sync summary phrase "absent in cloud" resources as
# "already destroyed (nothing to destroy)" instead of "to be created".
SS_MODE=destroy ss_sync_state || true

# The kubernetes provider reads the kubeconfig from .kube/config. If the TKE
# cluster still exists, write the latest kubeconfig to the local file first so
# terraform can connect to the cluster and delete the cube-* addons, then delete
# the cluster itself (terraform dependencies ensure addons are destroyed first).
if terraform state list 2>/dev/null | grep -q 'tencentcloud_kubernetes_cluster.tke'; then
	echo -e "${CYAN}Detected a TKE cluster; writing the kubeconfig so the addons can be deleted...${NC}"
	if terraform output -raw tke_kube_config 2>/dev/null | grep -q '^apiVersion'; then
		terraform output -raw tke_kube_config 2>/dev/null >"$SCRIPT_DIR/.kube/config" || true
		chmod 600 "$SCRIPT_DIR/.kube/config" 2>/dev/null || true
	fi
	# The addon resources (kubernetes_*) are only included in the plan when
	# create_tke && deploy_tke_addons are true; both must be set to true on
	# destroy, otherwise these resources are treated as count=0 and cannot be
	# deleted (leaving orphaned CLBs, etc.).
	export TF_VAR_create_tke=true
	export TF_VAR_deploy_tke_addons=true
fi

# ---------------------------------------------------------------
# run_destroy — terraform destroy (auto-approved) with a resilient prune-retry:
#   when resources were deleted out-of-band, terraform's refresh/delete fails with
#   ResourceNotFound / unreachable-endpoint errors and aborts before cleaning
#   anything. Drop the offending resources from the state and retry (skipping the
#   failing refresh) so the rest is still torn down. All args are forwarded to
#   `terraform destroy` (e.g. -target=...). Returns non-zero if it ultimately fails.
# ---------------------------------------------------------------
run_destroy() {
	local log attempt max_attempts stale_addrs addr suggested_targets target
	local -a destroy_extra=()
	log="$(mktemp "${TMPDIR:-/tmp}/cubesandbox_destroy.XXXXXX")"
	attempt=1
	max_attempts=4
	while :; do
		if terraform destroy -auto-approve "$@" "${destroy_extra[@]+"${destroy_extra[@]}"}" 2>&1 | tee "$log"; then
			rm -f "$log"
			return 0
		fi

		# A still-Running CVM (e.g. an orphaned TKE node-pool worker the node pool
		# kept behind) blocks deletion of the resources it references (the key pair):
		#   ...does not support the instance `ins-xxxx` which is in the state of `Running`
		# Terminate those instances and retry, instead of giving up.
		local running_cvms
		running_cvms="$(grep -oE 'instance `ins-[a-z0-9]+`' "$log" 2>/dev/null |
			grep -oE 'ins-[a-z0-9]+' | sort -u | tr '\n' ' ')"
		if [ "$attempt" -lt "$max_attempts" ] && [ -n "${running_cvms// /}" ]; then
			echo ""
			echo -e "${YELLOW}Detected running CVM(s) blocking the destroy; terminating them...${NC}"
			terminate_cvms "$running_cvms" || true
			destroy_extra=("-refresh=false")
			attempt=$((attempt + 1))
			echo -e "${CYAN}Retrying terraform destroy (attempt ${attempt}/${max_attempts})...${NC}"
			continue
		fi

		# Terraform refuses targeted plans when unrelated resource addresses have
		# moved (for example after switching TCR resources from singleton to
		# count-gated addresses). It prints exact -target suggestions; add them and
		# retry instead of failing the whole teardown.
		if grep -q 'Moved resource instances excluded by targeting' "$log" 2>/dev/null; then
			suggested_targets="$(
				grep -oE -- '-target="[^"]+"' "$log" 2>/dev/null |
					sed -E 's/-target="([^"]+)"/\1/' | sort -u
			)"
			if [ -n "$suggested_targets" ] && [ "$attempt" -lt "$max_attempts" ]; then
				if echo "$suggested_targets" | grep -qE '(^|\.)(tcr|tcr_|tcr_token_deploy)|tencentcloud_tcr_'; then
					echo ""
					echo -e "${YELLOW}Terraform requires TCR moved targets, but TCR namespace deletion must be ordered.${NC}"
					echo -e "${CYAN}Tearing TCR down via tccli first (repositories → namespace → instance + bucket), then pruning state...${NC}"
					if destroy_tcr "$TCR_INSTANCE_ID" "$TCR_NAMESPACE"; then
						tcr_state_prune
						attempt=$((attempt + 1))
						echo -e "${CYAN}Retrying terraform destroy (attempt ${attempt}/${max_attempts})...${NC}"
						continue
					fi
					echo -e "${YELLOW}⚠ Could not tear TCR down via tccli; not adding TCR targets directly because repositories can block namespace deletion.${NC}"
					suggested_targets="$(echo "$suggested_targets" | grep -vE '(^|\.)(tcr|tcr_|tcr_token_deploy)|tencentcloud_tcr_' || true)"
					[ -n "$suggested_targets" ] || {
						rm -f "$log"
						return 1
					}
				fi
				echo ""
				echo -e "${YELLOW}Terraform needs additional targets for moved resource addresses; retrying with them:${NC}"
				while IFS= read -r target; do
					[ -n "$target" ] || continue
					echo -e "  ${CYAN}-target=${target}${NC}"
					destroy_extra+=("-target=${target}")
				done <<EOF
${suggested_targets}
EOF
				attempt=$((attempt + 1))
				echo -e "${CYAN}Retrying terraform destroy (attempt ${attempt}/${max_attempts})...${NC}"
				continue
			fi
		fi

		if _is_local_apiserver_refused "$log" && [ "$attempt" -lt "$max_attempts" ]; then
			echo ""
			if _reopen_apiserver_tunnel; then
				attempt=$((attempt + 1))
				echo -e "${CYAN}Retrying terraform destroy with the refreshed API Server tunnel (attempt ${attempt}/${max_attempts})...${NC}"
				continue
			fi
			echo -e "${YELLOW}⚠ Could not reopen the API Server tunnel; Kubernetes addons still need manual attention.${NC}"
		fi

		# Resource addresses terraform errored on appear as "  with <addr>,".
		stale_addrs="$(
			grep -E 'with [a-z][A-Za-z0-9_]*\.' "$log" 2>/dev/null |
				sed -E 's/.*with ([^,]+),.*/\1/' | sort -u
		)"
		# Only prune from state when the failure actually looks like "the resource
		# is already gone". Blindly `state rm`-ing every address that appears in an
		# error block would, for a delete that failed for a DIFFERENT reason (a
		# dependency still in use, a transient API error), drop a STILL-EXISTING
		# resource from state and leave it as a billed orphan — the opposite of
		# what destroy.sh is for.
		local gone_marker=0
		grep -qiE 'not[ -]?found|does not exist|NotFound|ResourceNotFound|InvalidParameterValue|InvalidInstanceId|无法找到|不存在|been deleted|已删除' "$log" 2>/dev/null && gone_marker=1
		if [ "$attempt" -ge "$max_attempts" ] || [ -z "$stale_addrs" ] || [ "$gone_marker" != "1" ]; then
			echo ""
			echo -e "${RED}✗ terraform destroy failed.${NC}"
			[ -z "$stale_addrs" ] && echo -e "${YELLOW}  No stale resources were detected to prune automatically.${NC}"
			if grep -qiE 'ResourceInUse|UsedIp|already in use|resource.*in use' "$log" 2>/dev/null; then
				used_ip_blocker_hint
			fi
			if [ -n "$stale_addrs" ] && [ "$gone_marker" != "1" ]; then
				echo -e "${YELLOW}  The error does not look like 'resource already gone', so NOTHING was removed${NC}"
				echo -e "${YELLOW}  from state — pruning here could orphan a still-existing, billable resource.${NC}"
				NEEDS_MANUAL_CLEANUP=1
			fi
			echo -e "${YELLOW}  Inspect the errors above; some resources may need manual cleanup in the console.${NC}"
			rm -f "$log"
			return 1
		fi
		echo ""
		echo -e "${YELLOW}Some resources no longer exist (confirmed not-found). Removing them from${NC}"
		echo -e "${YELLOW}the Terraform state so the destroy can continue with the rest:${NC}"
		printf '%s\n' "$stale_addrs" | while IFS= read -r addr; do
			[ -z "$addr" ] && continue
			echo -e "  ${CYAN}terraform state rm ${addr}${NC}"
			terraform state rm "$addr" >/dev/null 2>&1 || true
		done
		destroy_extra=("-refresh=false")
		attempt=$((attempt + 1))
		echo ""
		echo -e "${CYAN}Retrying terraform destroy (attempt ${attempt}/${max_attempts})...${NC}"
	done
}

# ---------------------------------------------------------------
# Intranet-only kube-apiserver access (mirror of create.sh).
#   The cluster's kube-apiserver is intranet-only, so to let the local
#   kubernetes provider delete the cube-* addons we tunnel through the jumpserver
#   (still alive during phase 1) and point the local kubeconfig at the tunnel.
# ---------------------------------------------------------------
APISERVER_LOCAL_PORT="${APISERVER_LOCAL_PORT:-6443}"
APISERVER_TUNNEL_PID=""
APISERVER_REMOTE_HOSTPORT=""

_close_apiserver_tunnel() {
	[ -n "${APISERVER_TUNNEL_PID}" ] || return 0
	kill "${APISERVER_TUNNEL_PID}" 2>/dev/null || true
	APISERVER_TUNNEL_PID=""
}

_localize_kubeconfig() {
	local kubeconfig="${SCRIPT_DIR}/.kube/config" cur
	[ -f "$kubeconfig" ] || return 0
	cur=$(grep -E '^[[:space:]]*server:[[:space:]]*' "$kubeconfig" | head -n1)
	case "$cur" in
	*"127.0.0.1:${APISERVER_LOCAL_PORT}"*) return 0 ;; # already localized
	esac
	local tmp
	tmp="${kubeconfig}.tmp.$$"
	awk -v port="${APISERVER_LOCAL_PORT}" '
		/^[ \t]*certificate-authority-data:/ { next }
		/^[ \t]*insecure-skip-tls-verify:/ { next }
		/^[ \t]*server:[ \t]*https?:\/\// {
			indent = $0
			sub(/[^ \t].*/, "", indent)
			print indent "server: https://127.0.0.1:" port
			print indent "insecure-skip-tls-verify: true"
			next
		}
		{ print }
	' "$kubeconfig" >"$tmp" && mv "$tmp" "$kubeconfig"
	chmod 600 "$kubeconfig" 2>/dev/null || true
}

_local_apiserver_port_open() {
	if command -v nc >/dev/null 2>&1; then
		nc -z -w 2 127.0.0.1 "${APISERVER_LOCAL_PORT}" 2>/dev/null
		return $?
	fi
	(: >/dev/tcp/127.0.0.1/"${APISERVER_LOCAL_PORT}") >/dev/null 2>&1
}

_start_apiserver_tunnel() {
	local host port
	[ -n "${APISERVER_REMOTE_HOSTPORT}" ] || return 1
	[ -n "${JS_IP:-}" ] || return 1
	host="${APISERVER_REMOTE_HOSTPORT%:*}"
	port="${APISERVER_REMOTE_HOSTPORT##*:}"
	if _local_apiserver_port_open; then
		_localize_kubeconfig
		return 0
	fi
	_close_apiserver_tunnel
	ssh -i "${SSH_PRI_KEY}" -p 443 \
		-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		-o ConnectTimeout=10 -o ExitOnForwardFailure=yes -o BatchMode=yes \
		-o ServerAliveInterval=30 -o ServerAliveCountMax=4 \
		-fN -L "127.0.0.1:${APISERVER_LOCAL_PORT}:${host}:${port}" \
		root@"${JS_IP}" 2>/dev/null || return 1
	APISERVER_TUNNEL_PID=$(pgrep -f "127.0.0.1:${APISERVER_LOCAL_PORT}:${host}:${port}" 2>/dev/null | head -n1 || echo "")
	trap _close_apiserver_tunnel EXIT
	_localize_kubeconfig
	local i
	for i in $(seq 1 10); do
		if _local_apiserver_port_open; then
			return 0
		fi
		sleep 1
	done
	echo -e "  ${YELLOW}⚠ API Server tunnel process started but 127.0.0.1:${APISERVER_LOCAL_PORT} is not accepting connections${NC}"
	_close_apiserver_tunnel
	return 1
}

_reopen_apiserver_tunnel() {
	[ -n "${APISERVER_REMOTE_HOSTPORT:-}" ] || return 1
	echo -e "  ${YELLOW}Kubernetes provider lost the local API Server tunnel; reopening it and retrying...${NC}"
	_start_apiserver_tunnel
}

_is_local_apiserver_refused() {
	local log="$1"
	grep -qiE "127\\.0\\.0\\.1:${APISERVER_LOCAL_PORT}.*(connection refused|connect: connection refused)|dial tcp 127\\.0\\.0\\.1:${APISERVER_LOCAL_PORT}" "$log" 2>/dev/null
}

_ensure_apiserver_tunnel_ready() {
	if _local_apiserver_port_open; then
		return 0
	fi
	_reopen_apiserver_tunnel
}

_set_apiserver_remote_from_value() {
	local value="$1" host port
	value="${value#https://}"
	value="${value#http://}"
	value="${value%%/*}"
	host="${value%:*}"
	port="${value##*:}"
	[ "$port" = "$value" ] && port="443"
	[ -n "$host" ] || return 1
	APISERVER_REMOTE_HOSTPORT="${host}:${port}"
	return 0
}

_recover_apiserver_remote_hostport() {
	local kubeconfig="$1" endpoint fresh server

	endpoint="$(terraform output -raw tke_cluster_endpoint 2>/dev/null || echo "")"
	if [ -n "$endpoint" ] && _set_apiserver_remote_from_value "$endpoint"; then
		return 0
	fi

	fresh="$(terraform output -raw tke_kube_config 2>/dev/null || echo "")"
	if echo "$fresh" | grep -q '^apiVersion'; then
		server="$(printf '%s\n' "$fresh" | grep -E '^[[:space:]]*server:[[:space:]]*https?://' |
			head -n1 | sed -E 's#^[[:space:]]*server:[[:space:]]*https?://##' | tr -d '[:space:]')"
		case "$server" in
		127.0.0.1:* | "") ;;
		*)
			printf '%s\n' "$fresh" >"$kubeconfig"
			chmod 600 "$kubeconfig" 2>/dev/null || true
			_set_apiserver_remote_from_value "$server"
			return $?
			;;
		esac
	fi

	return 1
}

_open_apiserver_tunnel() {
	local kubeconfig="${SCRIPT_DIR}/.kube/config"
	[ -n "${JS_IP:-}" ] || {
		echo -e "  ${YELLOW}⚠ jumpserver IP unknown; cannot tunnel to the intranet API Server${NC}"
		return 1
	}
	[ -f "$kubeconfig" ] || {
		echo -e "  ${YELLOW}⚠ local kubeconfig not found; cannot set up the API Server tunnel${NC}"
		return 1
	}

	local server host port
	server=$(grep -E '^[[:space:]]*server:[[:space:]]*https?://' "$kubeconfig" |
		head -n1 | sed -E 's#^[[:space:]]*server:[[:space:]]*https?://##' | tr -d '[:space:]')
	case "$server" in
	127.0.0.1:*)
		# Already localized (e.g. a previous partial run): just (re)start the tunnel.
		if [ -n "${APISERVER_REMOTE_HOSTPORT}" ] || _recover_apiserver_remote_hostport "$kubeconfig"; then
			_start_apiserver_tunnel
			return $?
		fi
		echo -e "  ${YELLOW}⚠ kubeconfig already points at a local tunnel but the real intranet endpoint could not be recovered${NC}"
		return 1
		;;
	esac
	server="${server%%/*}"
	host="${server%:*}"
	port="${server##*:}"
	[ "$port" = "$server" ] && port="443"
	[ -n "$host" ] || {
		echo -e "  ${YELLOW}⚠ could not parse the intranet API Server endpoint from the kubeconfig${NC}"
		return 1
	}
	APISERVER_REMOTE_HOSTPORT="${host}:${port}"

	echo -e "  ${CYAN}Opening intranet API Server tunnel via jumpserver (${JS_IP})...${NC}"
	_start_apiserver_tunnel || {
		echo -e "  ${YELLOW}⚠ failed to open the API Server tunnel through the jumpserver${NC}"
		return 1
	}
	echo -e "  ${GREEN}✓ Intranet API Server tunnel ready: 127.0.0.1:${APISERVER_LOCAL_PORT} → ${host}:${port}${NC}"
	return 0
}

# ---------------------------------------------------------------
# tccli_run — run a tccli command with credentials + region. Prefers the
#   jumpserver (still alive during the destroy, in the same VPC/region; tccli is
#   installed there on demand) and falls back to a locally-installed tccli (e.g.
#   when cleaning recycle-bin orphans after the jumpserver is already gone).
#   Echoes the command output; returns 127 when no runner is available.
#   Reads globals: JS_IP, SSH_PRI_KEY, REGION, TENCENTCLOUD_SECRET_ID/KEY.
# ---------------------------------------------------------------
tccli_run() {
	# Prefer the jumpserver while it is still reachable.
	if [ -n "${JS_IP:-}" ] && nc -z -w 3 "${JS_IP}" 443 2>/dev/null; then
		# Feed a remote script over stdin to `bash -s` so the cloud credentials
		# travel as exported env vars inside the piped script rather than on the
		# remote command line (argv is world-readable via `ps`; CWE-214).
		local a args_str="" script _to=""
		for a in "$@"; do args_str+=$(printf ' %q' "$a"); done
		printf -v script 'export TENCENTCLOUD_SECRET_ID=%q TENCENTCLOUD_SECRET_KEY=%q TENCENTCLOUD_REGION=%q\ncommand -v tccli >/dev/null 2>&1 || pip3 install -q tccli -i https://mirrors.tencent.com/pypi/simple/ >/dev/null 2>&1\ntccli%s\n' \
			"${TENCENTCLOUD_SECRET_ID}" "${TENCENTCLOUD_SECRET_KEY}" "${REGION}" "${args_str}"
		# Bound the whole SSH op so a hung pip install / network can't block forever.
		command -v timeout >/dev/null 2>&1 && _to="timeout 240"
		printf '%s' "$script" | $_to ssh -i "${SSH_PRI_KEY}" -p 443 \
			-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
			-o ConnectTimeout=15 -o ServerAliveInterval=15 -o ServerAliveCountMax=4 \
			-o BatchMode=yes -o LogLevel=ERROR \
			root@"${JS_IP}" "bash -s" 2>&1
		return $?
	fi
	# Fall back to a local tccli (do not auto-install on the user's machine).
	if command -v tccli >/dev/null 2>&1; then
		env TENCENTCLOUD_SECRET_ID="${TENCENTCLOUD_SECRET_ID}" \
			TENCENTCLOUD_SECRET_KEY="${TENCENTCLOUD_SECRET_KEY}" \
			TENCENTCLOUD_REGION="${REGION}" \
			tccli "$@" 2>&1
		return $?
	fi
	return 127
}

# ---------------------------------------------------------------
# clear_recycle_bin — after MySQL/Redis are destroyed they linger in the Tencent
#   Cloud recycle bin (isolated). Release them immediately so they are fully
#   deleted. Instance IDs default to the captured ones; when empty (e.g. cleaning
#   orphans on a re-run) they are discovered by name from the recycle bin.
#   Best-effort: any failure is logged but does not block the destroy.
# ---------------------------------------------------------------
clear_recycle_bin() {
	local mysql_id="$1" redis_id="$2" out i rc=0

	# Make sure a tccli runner exists; otherwise print the manual commands.
	if ! { [ -n "${JS_IP:-}" ] && nc -z -w 3 "${JS_IP}" 443 2>/dev/null; } && ! command -v tccli >/dev/null 2>&1; then
		echo -e "  ${YELLOW}⚠ No tccli runner available (jumpserver gone and no local tccli).${NC}"
		echo -e "  ${YELLOW}  Clear the recycle bin manually, e.g.:${NC}"
		echo -e "  ${YELLOW}    tccli redis CleanUpInstance --InstanceId <crs-id> --region ${REGION}${NC}"
		echo -e "  ${YELLOW}    tccli cdb OfflineIsolatedInstances --InstanceIds '[\"<cdb-id>\"]' --region ${REGION}${NC}"
		# Without a runner the recycle bin cannot be cleared here; signal manual
		# cleanup (so the caller sets NEEDS_MANUAL_CLEANUP) only when there is a
		# captured instance that still needs releasing.
		{ [ -n "$mysql_id" ] || [ -n "$redis_id" ]; } && return 1
		return 0
	fi

	# Discover the instance IDs by name when they were not captured (orphans).
	# NOTE: these tccli calls are best-effort; the `|| true` keeps `set -e` from
	# aborting the whole destroy when tccli/API returns a non-zero status.
	if [ -z "$redis_id" ]; then
		redis_id="$(tccli_run redis DescribeInstances --Limit 100 2>/dev/null |
			jq -r '.InstanceSet[]? | select(.InstanceName=="cubesandbox-redis") | .InstanceId' 2>/dev/null | head -n1 || true)"
	fi
	if [ -z "$mysql_id" ]; then
		mysql_id="$(tccli_run cdb DescribeDBInstances --InstanceNames cubesandbox-mysql 2>/dev/null |
			jq -r '.Items[]? | .InstanceId' 2>/dev/null | head -n1 || true)"
	fi

	if [ -n "$mysql_id" ]; then
		echo -e "  ${CYAN}Releasing MySQL ${mysql_id} from the recycle bin (cdb OfflineIsolatedInstances)...${NC}"
		local mysql_done=0
		for i in 1 2 3 4 5; do
			out="$(tccli_run cdb OfflineIsolatedInstances --InstanceIds "[\"${mysql_id}\"]" || true)"
			echo "$out" | sed 's/^/    /'
			if echo "$out" | grep -qiE 'not found|does not exist|NotFound|InvalidParameter'; then
				echo -e "  ${GREEN}✓ MySQL already gone from the recycle bin${NC}"
				mysql_done=1
				break
			fi
			if ! echo "$out" | grep -q '"Error"'; then
				echo -e "  ${GREEN}✓ MySQL ${mysql_id} released${NC}"
				mysql_done=1
				break
			fi
			echo -e "  ${YELLOW}  not isolated yet, retrying in 10s (${i}/5)...${NC}"
			sleep 10
		done
		# Could not confirm release after all retries → let the caller flag manual cleanup.
		[ "$mysql_done" = 1 ] || rc=1
	else
		echo -e "  ${CYAN}No MySQL instance found in the recycle bin.${NC}"
	fi

	if [ -n "$redis_id" ]; then
		echo -e "  ${CYAN}Releasing Redis ${redis_id} from the recycle bin (redis CleanUpInstance)...${NC}"
		local redis_done=0
		for i in 1 2 3 4 5; do
			out="$(tccli_run redis CleanUpInstance --InstanceId "${redis_id}" || true)"
			echo "$out" | sed 's/^/    /'
			if echo "$out" | grep -qiE 'not found|does not exist|NotFound'; then
				echo -e "  ${GREEN}✓ Redis already gone from the recycle bin${NC}"
				redis_done=1
				break
			fi
			if echo "$out" | grep -qE '"TaskId"' || ! echo "$out" | grep -q '"Error"'; then
				echo -e "  ${GREEN}✓ Redis ${redis_id} released${NC}"
				redis_done=1
				break
			fi
			echo -e "  ${YELLOW}  not in recycle bin yet, retrying in 10s (${i}/5)...${NC}"
			sleep 10
		done
		[ "$redis_done" = 1 ] || rc=1
	else
		echo -e "  ${CYAN}No Redis instance found in the recycle bin.${NC}"
	fi

	return "$rc"
}

# ---------------------------------------------------------------
# terminate_cvms — terminate the given CVM instance IDs (space-separated) and wait
#   for them to leave the RUNNING state. TKE node pools keep their worker CVMs when
#   the node pool is deleted (delete_keep_instance defaults to true), so those CVMs
#   are orphaned and, while still Running, block deletion of the shared key pair.
#   Best-effort: errors are logged but do not abort the destroy.
# ---------------------------------------------------------------
terminate_cvms() {
	local ids="$1" json a out i left
	ids="$(echo "$ids" | tr -s ' ' | sed 's/^ //; s/ $//')"
	[ -z "$ids" ] && return 0

	json="["
	for a in $ids; do json="${json}\"${a}\","; done
	json="${json%,}]"

	echo -e "  ${CYAN}Terminating leftover TKE worker CVMs: ${ids}${NC}"
	for i in 1 2 3; do
		out="$(tccli_run cvm TerminateInstances --InstanceIds "$json" || true)"
		echo "$out" | sed 's/^/    /'
		echo "$out" | grep -qiE 'not found|does not exist|InvalidInstanceId' && break
		echo "$out" | grep -q '"Error"' || break
		sleep 8
	done

	# Wait (best-effort) for the CVMs to actually leave RUNNING, so the key pair can
	# be deleted later without UnsupportedOperation.InstanceStateRunning.
	for i in $(seq 1 20); do
		left="$(tccli_run cvm DescribeInstances --InstanceIds "$json" 2>/dev/null |
			jq -r '[.InstanceSet[]? | select(.InstanceState=="RUNNING")] | length' 2>/dev/null || echo 0)"
		[ "${left:-0}" = "0" ] && {
			echo -e "  ${GREEN}✓ TKE worker CVMs terminated${NC}"
			break
		}
		echo -e "  ${YELLOW}  waiting for ${left} CVM(s) to terminate... (${i}/20)${NC}"
		sleep 10
	done
}

# ---------------------------------------------------------------
# terminate_vpc_cvms — terminate every CVM in the given VPC except the one to keep
#   (the jumpserver). Orphaned CVMs (e.g. TKE node-pool workers kept on node-pool
#   delete) still reference the shared security group and block its deletion, so
#   they must be cleared before the security group is destroyed. Best-effort.
# ---------------------------------------------------------------
terminate_vpc_cvms() {
	local vpc_id="$1" keep_id="$2" ids id out offset page
	if [ -z "$vpc_id" ]; then
		echo -e "  ${YELLOW}⚠ VPC id unknown; cannot enumerate VPC CVMs (skipping the sweep).${NC}"
		return 0
	fi

	# Page through ALL CVMs in the VPC (the API caps each page at 100).
	ids=""
	offset=0
	while :; do
		page="$(tccli_run cvm DescribeInstances \
			--Filters "[{\"Name\":\"vpc-id\",\"Values\":[\"${vpc_id}\"]}]" \
			--Limit 100 --Offset "${offset}" 2>/dev/null |
			jq -r '.InstanceSet[]? | .InstanceId' 2>/dev/null || true)"
		[ -z "${page//[$'\n\t ']/}" ] && break
		ids="${ids} $(echo "$page" | tr '\n' ' ')"
		# Fewer than a full page → done.
		[ "$(echo "$page" | grep -c .)" -lt 100 ] && break
		offset=$((offset + 100))
		[ "$offset" -ge 1000 ] && break # safety cap
	done

	out=""
	for id in $ids; do
		[ -n "$id" ] || continue
		[ "$id" = "$keep_id" ] && continue
		out="$out $id"
	done

	if [ -n "${out// /}" ]; then
		echo -e "  ${CYAN}Terminating all CVMs in the VPC${keep_id:+ (except jumpserver ${keep_id})}:${out}${NC}"
		terminate_cvms "$out"
	else
		echo -e "  ${CYAN}No CVMs to terminate in the VPC${keep_id:+ (besides the jumpserver)}.${NC}"
	fi
}

# ---------------------------------------------------------------
# ensure_recycle_bin_cleared — verify the MySQL/Redis instances are FULLY removed
#   (no longer in the recycle bin / isolated), retrying the release while the
#   jumpserver is still alive (it runs tccli). The Phase 1 release is async (it
#   returns a TaskId), so this is the final confirmation before the jumpserver is
#   deleted — after which tccli could only run locally. Best-effort.
# ---------------------------------------------------------------
ensure_recycle_bin_cleared() {
	local mysql_id="$1" redis_id="$2" i cnt rc=0
	[ -z "$mysql_id" ] && [ -z "$redis_id" ] && return 0

	if [ -n "$mysql_id" ]; then
		echo -e "  ${CYAN}Confirming MySQL ${mysql_id} is fully removed from the recycle bin...${NC}"
		local mysql_cleared=0
		for i in 1 2 3 4 5 6; do
			cnt="$(tccli_run cdb DescribeDBInstances --InstanceIds "[\"${mysql_id}\"]" 2>/dev/null |
				jq -r '.TotalCount // (.Items | length) // 0' 2>/dev/null || echo 0)"
			if [ "${cnt:-0}" = "0" ]; then
				echo -e "  ${GREEN}✓ MySQL ${mysql_id} fully removed${NC}"
				mysql_cleared=1
				break
			fi
			echo -e "  ${YELLOW}  still present (recycle bin); re-releasing (cdb OfflineIsolatedInstances) and waiting... (${i}/6)${NC}"
			tccli_run cdb OfflineIsolatedInstances --InstanceIds "[\"${mysql_id}\"]" 2>/dev/null | sed 's/^/    /' || true
			sleep 15
		done
		# Still present after all retries → let the caller flag manual cleanup.
		[ "$mysql_cleared" = 1 ] || rc=1
	fi

	if [ -n "$redis_id" ]; then
		echo -e "  ${CYAN}Confirming Redis ${redis_id} is fully removed from the recycle bin...${NC}"
		local redis_cleared=0
		for i in 1 2 3 4 5 6; do
			cnt="$(tccli_run redis DescribeInstances --InstanceId "${redis_id}" 2>/dev/null |
				jq -r '.TotalCount // (.InstanceSet | length) // 0' 2>/dev/null || echo 0)"
			if [ "${cnt:-0}" = "0" ]; then
				echo -e "  ${GREEN}✓ Redis ${redis_id} fully removed${NC}"
				redis_cleared=1
				break
			fi
			echo -e "  ${YELLOW}  still present (recycle bin); re-cleaning (redis CleanUpInstance) and waiting... (${i}/6)${NC}"
			tccli_run redis CleanUpInstance --InstanceId "${redis_id}" 2>/dev/null | sed 's/^/    /' || true
			sleep 15
		done
		[ "$redis_cleared" = 1 ] || rc=1
	fi

	return "$rc"
}

# ---------------------------------------------------------------
# destroy_tcr — fully tear down the TCR instance via tccli, in order:
#     1) delete every image repository (the images) in every namespace
#     2) delete every namespace (now empty) — not just the configured one, so any
#        extra namespaces created out-of-band are also removed
#     3) delete the instance together with its auto-created COS backend bucket
#        (DeleteInstance --DeleteBucket true) — this also removes the instance's
#        tokens and VPC attachments.
#   Why not just terraform? build_images.sh pushes the four component images into
#   the namespace as repositories that are NOT tracked by Terraform, so
#   `terraform destroy` of tencentcloud_tcr_namespace is rejected with
#   FailedOperation.PreconditionFailed ("the project contains repositories, can
#   not be deleted"); and Terraform does not delete the backend COS bucket for an
#   already-deployed instance (delete_bucket is a delete-time flag read from
#   state, which the live instance lacks). Doing the whole teardown here via
#   tccli, while the jumpserver (which runs tccli) is still alive, guarantees the
#   instance is removed with no residual resources.
#   Returns 0 when the instance deletion was accepted (caller should prune the
#   TCR resources from Terraform state); returns non-zero when no tccli runner is
#   available or DeleteInstance failed (caller should fall back to terraform).
#   Reads globals: JS_IP, REGION (via tccli_run).
# ---------------------------------------------------------------
destroy_tcr() {
	local registry_id="$1" ns="$2" repos repo rname out offset page i cnt namespaces nsname
	[ -z "$registry_id" ] && {
		echo -e "  ${CYAN}No TCR instance id; nothing to delete via tccli.${NC}"
		return 1
	}
	[ -z "$ns" ] && ns="cubesandbox-cluster"

	# A tccli runner is required (the repos/bucket cannot be removed any other
	# way). Without one, print the manual commands and let the caller fall back.
	if ! { [ -n "${JS_IP:-}" ] && nc -z -w 3 "${JS_IP}" 443 2>/dev/null; } && ! command -v tccli >/dev/null 2>&1; then
		echo -e "  ${YELLOW}⚠ No tccli runner available (jumpserver gone and no local tccli).${NC}"
		echo -e "  ${YELLOW}  Tear the TCR down manually, e.g.:${NC}"
		echo -e "  ${YELLOW}    tccli tcr DescribeRepositories --RegistryId ${registry_id} --NamespaceName ${ns} --region ${REGION}${NC}"
		echo -e "  ${YELLOW}    tccli tcr DeleteRepository  --RegistryId ${registry_id} --NamespaceName ${ns} --RepositoryName <repo> --region ${REGION}${NC}"
		echo -e "  ${YELLOW}    tccli tcr DeleteNamespace   --RegistryId ${registry_id} --NamespaceName ${ns} --region ${REGION}${NC}"
		echo -e "  ${YELLOW}    tccli tcr DeleteInstance    --RegistryId ${registry_id} --DeleteBucket true --region ${REGION}${NC}"
		return 1
	fi

	# Enumerate EVERY namespace in the instance so we tear them all down (not just
	# the configured one) — the TCR may hold extra namespaces created out-of-band
	# that would otherwise be left behind. Page through them (API caps each page
	# at 100).
	namespaces=""
	offset=0
	while :; do
		page="$(tccli_run tcr DescribeNamespaces --RegistryId "$registry_id" --Limit 100 --Offset "$offset" 2>/dev/null |
			jq -r '.NamespaceList[]? | .Name' 2>/dev/null || true)"
		[ -z "${page//[$'\n\t ']/}" ] && break
		namespaces="${namespaces} $(echo "$page" | tr '\n' ' ')"
		[ "$(echo "$page" | grep -c .)" -lt 100 ] && break
		offset=$((offset + 100))
		[ "$offset" -ge 1000 ] && break # safety cap
	done
	# Always include the configured namespace, in case the listing failed or
	# returned nothing.
	case " ${namespaces} " in
	*" ${ns} "*) ;;
	*) namespaces="${ns} ${namespaces}" ;;
	esac
	echo -e "  ${CYAN}TCR namespaces to delete:${namespaces}${NC}"

	for nsname in $namespaces; do
		[ -n "$nsname" ] || continue

		# 1) Delete every image repository (the images) in the namespace. Page
		#    through them (the API caps each page at 100); .Name is "<ns>/<repo>".
		repos=""
		offset=0
		while :; do
			page="$(tccli_run tcr DescribeRepositories --RegistryId "$registry_id" --NamespaceName "$nsname" --Limit 100 --Offset "$offset" 2>/dev/null |
				jq -r '.RepositoryList[]? | .Name' 2>/dev/null || true)"
			[ -z "${page//[$'\n\t ']/}" ] && break
			repos="${repos} $(echo "$page" | tr '\n' ' ')"
			[ "$(echo "$page" | grep -c .)" -lt 100 ] && break
			offset=$((offset + 100))
			[ "$offset" -ge 1000 ] && break # safety cap
		done
		if [ -n "${repos// /}" ]; then
			echo -e "  ${CYAN}1/3 Deleting TCR images (repositories) in ${nsname}:${repos}${NC}"
			for repo in $repos; do
				[ -n "$repo" ] || continue
				# DeleteRepository wants the bare repo name, so strip the "<ns>/".
				# No --ForceDelete: the intl API has no such param (deletes the repo +
				# its images directly) and the domestic API defaults it to true.
				rname="${repo#"$nsname"/}"
				out="$(tccli_run tcr DeleteRepository --RegistryId "$registry_id" --NamespaceName "$nsname" --RepositoryName "$rname" 2>&1 || true)"
				echo "$out" | sed 's/^/      /'
			done
		else
			echo -e "  ${CYAN}1/3 No TCR images (repositories) found in ${nsname}.${NC}"
		fi

		# 2) Delete the (now empty) namespace. Best-effort; a missing namespace is fine.
		echo -e "  ${CYAN}2/3 Deleting TCR namespace ${nsname}...${NC}"
		out="$(tccli_run tcr DeleteNamespace --RegistryId "$registry_id" --NamespaceName "$nsname" 2>&1 || true)"
		echo "$out" | sed 's/^/      /'
	done

	# 3) Delete the instance together with its backend COS bucket.
	echo -e "  ${CYAN}3/3 Deleting TCR instance ${registry_id} + backend COS bucket (DeleteInstance --DeleteBucket true)...${NC}"
	out="$(tccli_run tcr DeleteInstance --RegistryId "$registry_id" --DeleteBucket true 2>&1 || true)"
	echo "$out" | sed 's/^/      /'
	if echo "$out" | grep -qiE 'not found|does not exist|NotFound'; then
		echo -e "  ${GREEN}✓ TCR instance already gone${NC}"
		return 0
	fi
	if echo "$out" | grep -q '"Error"'; then
		echo -e "  ${YELLOW}⚠ DeleteInstance returned an error; leaving TCR resources in state for the terraform fallback.${NC}"
		return 2
	fi

	# Confirm the instance is actually gone (best-effort) so we leave no residual.
	for i in $(seq 1 12); do
		cnt="$(tccli_run tcr DescribeInstances --Registryids "[\"${registry_id}\"]" 2>/dev/null |
			jq -r '.TotalCount // (.Registries | length) // 0' 2>/dev/null || echo 0)"
		[ "${cnt:-0}" = "0" ] && {
			echo -e "  ${GREEN}✓ TCR instance ${registry_id} deleted with its backend bucket (no residual)${NC}"
			return 0
		}
		echo -e "  ${YELLOW}  waiting for the TCR instance to be deleted... (${i}/12)${NC}"
		sleep 10
	done
	echo -e "  ${YELLOW}⚠ TCR instance ${registry_id} still reported after waiting; deletion was accepted and should finish shortly.${NC}"
	return 0
}

# in_state — true when the given resource address prefix exists in the state.
# Use fixed-string prefix matching so indexed addresses like resource.foo[0] are
# not interpreted as regular expressions.
in_state() { terraform state list 2>/dev/null | awk -v p="$1" 'index($0, p) == 1 { found = 1 } END { exit found ? 0 : 1 }'; }

# state_id_any ADDR... — return the first Terraform state id found for any
# address. Supports both scalar resources and count/indexed resources.
state_id_any() {
	local addr id
	for addr in "$@"; do
		id="$(terraform state show "$addr" 2>/dev/null | awk -F'"' '/^[[:space:]]*id[[:space:]]*=/{print $2; exit}')"
		[ -n "$id" ] && {
			printf '%s\n' "$id"
			return 0
		}
	done
	return 1
}

# tcr_state_prune — drop all TCR resources from the Terraform state. Called after
#   destroy_tcr removed them out-of-band (the instance delete also removes its
#   namespace/repos/token/VPC attachment), so terraform must not try again.
tcr_state_prune() {
	local _res
	for _res in \
		null_resource.tcr_token_deploy \
		tencentcloud_tcr_token.cluster \
		'tencentcloud_tcr_token.cluster[0]' \
		tencentcloud_tcr_token.demo \
		'tencentcloud_tcr_token.demo[0]' \
		tencentcloud_tcr_vpc_attachment.cluster \
		'tencentcloud_tcr_vpc_attachment.cluster[0]' \
		tencentcloud_tcr_vpc_attachment.demo \
		'tencentcloud_tcr_vpc_attachment.demo[0]' \
		tencentcloud_tcr_namespace.cluster \
		'tencentcloud_tcr_namespace.cluster[0]' \
		tencentcloud_tcr_namespace.demo \
		'tencentcloud_tcr_namespace.demo[0]' \
		tencentcloud_tcr_instance.cluster \
		'tencentcloud_tcr_instance.cluster[0]' \
		tencentcloud_tcr_instance.demo \
		'tencentcloud_tcr_instance.demo[0]'; do
		if in_state "$_res"; then
			echo -e "    ${CYAN}terraform state rm ${_res}${NC}"
			terraform state rm "$_res" >/dev/null 2>&1 || true
		fi
	done
}

# ---------------------------------------------------------------
# _js_kubectl — run kubectl on the jumpserver, which holds the kubeconfig and
#   sits inside the VPC (so it reaches the intranet kube-apiserver directly,
#   no tunnel needed). Used during the TKE teardown to drain the namespace
#   before deleting it. Best-effort; mirrors create.sh's _js_kubectl.
#   Reads globals: JS_IP, SSH_PRI_KEY.
# ---------------------------------------------------------------
_js_kubectl() {
	[ -n "${JS_IP:-}" ] || {
		echo ""
		return 1
	}
	ssh -i "${SSH_PRI_KEY}" -p 443 \
		-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		-o ConnectTimeout=5 -o BatchMode=yes -o LogLevel=ERROR \
		root@"${JS_IP}" "kubectl $*" 2>&1 || true
}

# ---------------------------------------------------------------
# _drain_namespace — force-delete every pod in a namespace and wait until none
#   remain, so the namespace itself can be deleted. TKE's Gatekeeper
#   "block-namespace-deletion-rule" rejects deleting a namespace that still
#   contains any pod, and the kubernetes provider removes a Deployment object
#   before its pods finish terminating (background cascade) — so deleting the
#   namespace in the same terraform run races the pod garbage-collection. This
#   also clears any sandbox pods cube-master spawned at runtime (not in tf
#   state). Best-effort: a timeout (or no kubectl) still lets the destroy
#   proceed — the namespace delete itself will surface any real failure.
#   Reads globals: JS_IP.
# ---------------------------------------------------------------
_drain_namespace() {
	local ns="$1" i left
	if [ -z "${JS_IP:-}" ]; then
		echo -e "  ${YELLOW}⚠ jumpserver IP unknown; cannot drain namespace ${ns} (its delete may be blocked by pods)${NC}"
		return 0
	fi
	if ! _js_kubectl get ns "${ns}" -o name 2>/dev/null | grep -qE "^namespace/${ns}$"; then
		echo -e "  ${CYAN}Namespace ${ns} not reachable via jumpserver kubectl; skipping pod drain.${NC}"
		return 0
	fi
	echo -e "  ${CYAN}Draining pods from namespace ${ns} before deleting it...${NC}"
	_js_kubectl -n "${ns}" delete pods --all --force --grace-period=0 --ignore-not-found 2>/dev/null | sed 's/^/    /' || true
	for i in $(seq 1 30); do
		left="$(_js_kubectl -n "${ns}" get pods -o name 2>/dev/null | grep -cE '^pod/' || true)"
		if [ "${left:-0}" = "0" ]; then
			echo -e "  ${GREEN}✓ Namespace ${ns} has no pods left${NC}"
			return 0
		fi
		echo -e "  ${YELLOW}  waiting for ${left} pod(s) in ${ns} to terminate... (${i}/30)${NC}"
		# Re-issue the force delete in case a pod was slow to appear/terminate.
		_js_kubectl -n "${ns}" delete pods --all --force --grace-period=0 --ignore-not-found >/dev/null 2>&1 || true
		sleep 5
	done
	echo -e "  ${YELLOW}⚠ Namespace ${ns} still has pods after waiting; attempting the namespace delete anyway.${NC}"
	return 0
}

echo ""
_draw_box "${YELLOW}" \
	"About to destroy ALL Tencent Cloud resources created by this deployment:" \
	"CVM (jump-server + compute) / TKE / CLB (intranet + internet) / MySQL /" \
	"Redis / TCR / NAT / VPC / subnet / security group / EIP / key pair" \
	"Reverse-of-create order, fail-fast:" \
	"TKE addons → TKE cluster → MySQL/Redis → CVM → TCR → network"
echo ""

# Drop -auto-approve from the forwarded args (kept for backward compatibility:
# it is now a harmless no-op). Each phase below already runs
# `terraform destroy -auto-approve`.
EXTRA_ARGS=()
for _a in "$@"; do
	[ "$_a" = "-auto-approve" ] && continue
	EXTRA_ARGS+=("$_a")
done

# Running destroy.sh IS the confirmation: proceed to tear everything down with no
# interactive prompt.
echo -e "${YELLOW}Proceeding to destroy ALL resources (running destroy.sh confirms the teardown).${NC}"

# _td_status LABEL STATE_ADDR ID [NOTE] — print a one-line teardown status: the
# resource id (+ optional NOTE) and "→ will be destroyed" when it is still
# tracked in the terraform state, or "already destroyed (absent)" when it is gone.
_td_status() {
	local label="$1" addr="$2" id="${3:-}" note="${4:-}"
	if in_state "$addr"; then
		echo -e "    ${CYAN}${label}: ${id:-present}${note:+ ${note}} → will be destroyed${NC}"
	else
		echo -e "    ${GREEN}${label}: already destroyed (absent)${NC}"
	fi
}

_moved_tcr_targets() {
	local _res
	for _res in \
		null_resource.tcr_token_deploy \
		tencentcloud_tcr_token.cluster \
		tencentcloud_tcr_vpc_attachment.cluster \
		tencentcloud_tcr_namespace.cluster \
		tencentcloud_tcr_instance.cluster; do
		in_state "$_res" && printf '%s\n' "-target=$_res"
	done
}

_state_count_prefix() {
	local prefix="$1"
	terraform state list 2>/dev/null | awk -v p="$prefix" 'index($0, p) == 1 { n++ } END { print n + 0 }'
}

_tccli_runner_available() {
	{ [ -n "${JS_IP:-}" ] && nc -z -w 3 "${JS_IP}" 443 2>/dev/null; } || command -v tccli >/dev/null 2>&1
}

_tcr_repo_count_for_report() {
	local registry_id="$1" ns="$2" namespaces nsname offset page repos cnt
	[ -n "$registry_id" ] || {
		echo "unknown"
		return 0
	}
	if ! _tccli_runner_available; then
		echo "unknown"
		return 0
	fi

	namespaces=""
	offset=0
	while :; do
		page="$(tccli_run tcr DescribeNamespaces --RegistryId "$registry_id" --Limit 100 --Offset "$offset" 2>/dev/null |
			jq -r '.NamespaceList[]? | .Name' 2>/dev/null || true)"
		[ -z "${page//[$'\n\t ']/}" ] && break
		namespaces="${namespaces} $(echo "$page" | tr '\n' ' ')"
		[ "$(echo "$page" | grep -c .)" -lt 100 ] && break
		offset=$((offset + 100))
		[ "$offset" -ge 1000 ] && break
	done
	[ -n "$ns" ] || ns="cubesandbox-cluster"
	case " ${namespaces} " in
	*" ${ns} "*) ;;
	*) namespaces="${ns} ${namespaces}" ;;
	esac

	cnt=0
	for nsname in $namespaces; do
		[ -n "$nsname" ] || continue
		offset=0
		while :; do
			repos="$(tccli_run tcr DescribeRepositories --RegistryId "$registry_id" --NamespaceName "$nsname" --Limit 100 --Offset "$offset" 2>/dev/null |
				jq -r '.RepositoryList[]? | .Name' 2>/dev/null || true)"
			[ -z "${repos//[$'\n\t ']/}" ] && break
			cnt=$((cnt + $(echo "$repos" | grep -c .)))
			[ "$(echo "$repos" | grep -c .)" -lt 100 ] && break
			offset=$((offset + 100))
			[ "$offset" -ge 1000 ] && break
		done
	done
	echo "$cnt"
}

dependency_blocker_report() {
	local blockers=0 k8s_count clb_ips tcr_repos cfs_present nat_present subnet_count

	echo ""
	echo -e "${YELLOW}Dependency blocker report (pre-destroy diagnostics):${NC}"

	k8s_count="$(_state_count_prefix kubernetes_)"
	clb_ips="$(for _out in tke_cubemaster_clb_ip tke_cube_api_clb_ip tke_cube_proxy_clb_ip tke_cube_webui_clb_ip; do
		tf_output_value "$_out"
	done | awk 'NF { printf "%s%s", sep, $0; sep=", " }')"
	if [ "${k8s_count:-0}" -gt 0 ]; then
		blockers=$((blockers + 1))
		echo -e "  ${YELLOW}• CLB / Kubernetes addons:${NC} ${k8s_count} kubernetes_* resource(s) still in state${clb_ips:+; CLB IPs: ${clb_ips}}."
		echo -e "    ${CYAN}Action:${NC} Phase 1 deletes addons before the TKE cluster; if it fails, verify the jumpserver tunnel/kubeconfig and remaining pods in namespace cubesandbox."
	else
		echo -e "  ${GREEN}✓ CLB / Kubernetes addons: no kubernetes_* resources in state.${NC}"
	fi

	if in_state tencentcloud_kubernetes_node_pool.tke || [ -n "${TKE_NODE_CVMS// /}" ]; then
		blockers=$((blockers + 1))
		echo -e "  ${YELLOW}• TKE worker CVM:${NC} ${TKE_NODE_CVMS:-node pool still tracked; CVM ids not resolved}."
		echo -e "    ${CYAN}Action:${NC} Phase 1 destroys the node pool and terminates detected worker CVMs before network cleanup."
	else
		echo -e "  ${GREEN}✓ TKE worker CVM: no node pool / worker CVM blocker detected.${NC}"
	fi

	cfs_present=0
	for _res in \
		tencentcloud_cfs_file_system.cubemaster_data \
		tencentcloud_cfs_access_rule.cubemaster_data \
		tencentcloud_cfs_access_group.cubemaster_data; do
		in_state "$_res" && cfs_present=1
	done
	if [ "$cfs_present" -eq 1 ]; then
		blockers=$((blockers + 1))
		echo -e "  ${YELLOW}• CFS:${NC} cubemaster_data CFS resources still exist; the CFS mount target ENI can block subnet deletion."
		echo -e "    ${CYAN}Action:${NC} Phase 4.5 destroys CFS before the NAT/subnet phase."
	else
		echo -e "  ${GREEN}✓ CFS: no CFS resources in state.${NC}"
	fi

	tcr_repos="$(_tcr_repo_count_for_report "$TCR_INSTANCE_ID" "$TCR_NAMESPACE")"
	if in_state tencentcloud_tcr_instance.cluster || in_state tencentcloud_tcr_instance.demo || [ -n "$TCR_INSTANCE_ID" ]; then
		blockers=$((blockers + 1))
		if [ "$tcr_repos" = "unknown" ]; then
			echo -e "  ${YELLOW}• TCR repositories:${NC} TCR ${TCR_INSTANCE_ID:-present} namespace=${TCR_NAMESPACE}; repository count could not be confirmed."
			echo -e "    ${CYAN}Action:${NC} Keep the jumpserver or local tccli available so Phase 3 can delete image repositories before namespace/instance deletion."
		else
			echo -e "  ${YELLOW}• TCR repositories:${NC} ${tcr_repos} repository item(s) detected in TCR ${TCR_INSTANCE_ID:-present}; non-empty repos block namespace deletion."
			echo -e "    ${CYAN}Action:${NC} Phase 3 deletes repositories/namespaces via tccli before pruning TCR state."
		fi
	else
		echo -e "  ${GREEN}✓ TCR repositories: no TCR instance in state/output.${NC}"
	fi

	nat_present=0
	for _res in tencentcloud_route_table_entry.nat tencentcloud_nat_gateway.cluster tencentcloud_eip.nat; do
		in_state "$_res" && nat_present=1
	done
	subnet_count="$(_state_count_prefix tencentcloud_subnet.)"
	if [ "$nat_present" -eq 1 ] || [ "${subnet_count:-0}" -gt 0 ]; then
		blockers=$((blockers + 1))
		echo -e "  ${YELLOW}• NAT / subnet:${NC} NAT=${NAT_GATEWAY_ID:-present/unknown}, subnet resources in state=${subnet_count}."
		echo -e "    ${CYAN}Action:${NC} Earlier phases must clear CLB/CFS/CVM ENIs first; Phase 5 then deletes NAT, routes, EIP and subnets."
	else
		echo -e "  ${GREEN}✓ NAT / subnet: no NAT/subnet resources in state.${NC}"
	fi

	if [ -n "$VPC_ID" ]; then
		echo -e "  ${CYAN}VPC scope:${NC} ${VPC_ID}. If subnet/VPC deletion still fails, check remaining ENI/CVM/CLB/NAT/CFS resources in this VPC."
	fi
	if [ "$blockers" -eq 0 ]; then
		echo -e "  ${GREEN}No obvious dependency blockers detected before destroy.${NC}"
	else
		echo -e "  ${YELLOW}${blockers} potential blocker group(s) detected; destroy.sh will now remove them in dependency order.${NC}"
	fi
}

# Capture the instance IDs / jumpserver access BEFORE anything is destroyed, so the
# recycle-bin cleanup can still address the MySQL/Redis instances afterwards.
echo ""
echo -e "${CYAN}Gathering deployment info before teardown (this can take a moment)...${NC}"
MYSQL_ID="$(terraform output -raw mysql_instance_id 2>/dev/null || echo '')"
REDIS_ID="$(terraform output -raw redis_instance_id 2>/dev/null || echo '')"
JS_IP="$(terraform output -raw jumpserver_public_ip 2>/dev/null || echo '')"
REGION="${TENCENTCLOUD_REGION:-${TF_VAR_region:-ap-guangzhou}}"

# Capture the TCR instance id + namespace so the image repositories pushed by
# build_images.sh (NOT tracked by Terraform) can be deleted before the TCR
# namespace/instance are torn down — otherwise the namespace delete is rejected
# with "the project contains repositories, can not be deleted".
# Terraform 1.x exits non-zero for `terraform output -raw` when the output exists
# but is an empty string. With `set -e`, doing that inside command substitution can
# abort destroy.sh before Phase 1, leaving all resources untouched. Read outputs via
# JSON and tolerate empty/missing values instead.
tf_output_value() {
	local name="$1"
	terraform output -json "$name" 2>/dev/null | jq -r '.value // ""' 2>/dev/null || true
}

TCR_INSTANCE_ID="$(tf_output_value tcr_id)"
TCR_NAMESPACE="$(tf_output_value tcr_namespace)"
if [ -z "$TCR_INSTANCE_ID" ]; then
	TCR_INSTANCE_ID="$(state_id_any \
		'tencentcloud_tcr_instance.cluster[0]' \
		tencentcloud_tcr_instance.cluster \
		'tencentcloud_tcr_instance.demo[0]' \
		tencentcloud_tcr_instance.demo 2>/dev/null || true)"
fi
[ -z "$TCR_NAMESPACE" ] && TCR_NAMESPACE="cubesandbox-cluster"

# Capture the TKE cluster's worker-node CVM IDs while the cluster still exists, so
# the orphaned node-pool workers (kept on node-pool delete) can be terminated later.
TKE_CLUSTER_ID="$(tf_output_value tke_cluster_id)"
TKE_NODE_CVMS=""
if [ -n "$TKE_CLUSTER_ID" ]; then
	echo -e "  ${CYAN}Querying TKE worker CVMs via tccli (installing tccli on the jumpserver if needed; may take ~1 min)...${NC}"
	TKE_NODE_CVMS="$(tccli_run tke DescribeClusterInstances --ClusterId "$TKE_CLUSTER_ID" --Limit 100 2>/dev/null |
		jq -r '.InstanceSet[]? | .InstanceId' 2>/dev/null | tr '\n' ' ' || true)"
	[ -n "${TKE_NODE_CVMS// /}" ] && echo -e "  ${CYAN}TKE worker CVMs detected: ${TKE_NODE_CVMS}${NC}"
fi
echo -e "  ${CYAN}Resolving VPC id from terraform state...${NC}"

# Capture the VPC id and the jumpserver's CVM id (from the state) so ALL CVMs in
# the VPC can be terminated before the security group is deleted, while keeping the
# jumpserver until the very end.
_TF_JSON="$(terraform show -json 2>/dev/null || true)"
VPC_ID="$(echo "$_TF_JSON" | jq -r '.values.root_module.resources[]? | select(.address=="tencentcloud_vpc.cluster") | .values.id' 2>/dev/null | head -n1 || true)"
JS_INSTANCE_ID="$(echo "$_TF_JSON" | jq -r '.values.root_module.resources[]? | select(.address=="tencentcloud_instance.jumpserver") | .values.id' 2>/dev/null | head -n1 || true)"
NAT_GATEWAY_ID="$(echo "$_TF_JSON" | jq -r '.values.root_module.resources[]? | select(.address=="tencentcloud_nat_gateway.cluster") | .values.id' 2>/dev/null | head -n1 || true)"
unset _TF_JSON
# Fallbacks: parse the resource id straight from the state if the JSON path above
# did not resolve it (so the VPC CVM sweep is never silently skipped).
if [ -z "$VPC_ID" ]; then
	VPC_ID="$(terraform state show tencentcloud_vpc.cluster 2>/dev/null | awk -F'"' '/^[[:space:]]*id[[:space:]]*=/{print $2; exit}')"
fi
if [ -z "$JS_INSTANCE_ID" ]; then
	JS_INSTANCE_ID="$(terraform state show tencentcloud_instance.jumpserver 2>/dev/null | awk -F'"' '/^[[:space:]]*id[[:space:]]*=/{print $2; exit}')"
fi
if [ -z "$NAT_GATEWAY_ID" ]; then
	NAT_GATEWAY_ID="$(terraform state show tencentcloud_nat_gateway.cluster 2>/dev/null | awk -F'"' '/^[[:space:]]*id[[:space:]]*=/{print $2; exit}')"
fi

# Teardown status: which resources are still tracked in state (→ will be
# destroyed) and which are already gone. The jump-server is listed LAST because
# it is destroyed last — it is kept alive to run the tccli cleanups for the
# earlier phases, yet it must precede the subnet/VPC it lives in.
echo -e "  ${CYAN}Teardown status (present → will be destroyed; absent → already destroyed):${NC}"
_td_status "MySQL      " tencentcloud_mysql_instance.mysql "$MYSQL_ID"
_td_status "Redis      " tencentcloud_redis_instance.redis "$REDIS_ID"
_td_status "Compute    " tencentcloud_instance.compute ""
if in_state tencentcloud_tcr_instance.cluster || in_state tencentcloud_tcr_instance.demo; then
	echo -e "    ${CYAN}TCR        : ${TCR_INSTANCE_ID:-present} namespace=${TCR_NAMESPACE} → will be destroyed${NC}"
else
	echo -e "    ${GREEN}TCR        : already destroyed (absent)${NC}"
fi
_td_status "NAT gateway" tencentcloud_nat_gateway.cluster "$NAT_GATEWAY_ID"
_td_status "VPC        " tencentcloud_vpc.cluster "$VPC_ID"
_td_status "jumpserver " tencentcloud_instance.jumpserver "${JS_INSTANCE_ID:-$JS_IP}" "(destroyed last — kept for cleanup)"
dependency_blocker_report

# ============================================================
# Teardown runs (mostly) in the REVERSE order of create.sh. create.sh provisions:
#   1) subnet + NAT gateway   2) TCR   3) jump-server + compute nodes
#   4) image build/push on the jump-server (no cloud resource to destroy)
#   5) MySQL + Redis   5b) CFS shared storage   6) TKE cluster + addons
# so destroy runs 6 → 5 → 3 → 2 → 5b (CFS) → 1 (step 4 builds images, nothing to
# tear down). CFS is the one deviation from strict reverse order: it is created at
# step 5b but torn down late (Phase 4.5, just before the subnet) because its NFS
# mount target is an ENI inside the subnet, so the subnet cannot be deleted while
# the CFS share still exists. Each phase waits for completion and is
# fail-fast: if any phase fails the teardown STOPS (remaining resources are left
# intact) so the failure can be inspected and ./destroy.sh re-run to continue.
# The jump-server is intentionally kept alive until phase 3 because it runs the
# tccli recycle-bin / orphan-CVM cleanup over SSH for the earlier phases.
# ============================================================

# remind_manual_cleanup — when automatic teardown cannot remove every resource
#   (e.g. MySQL/Redis stuck in the recycle bin, or leftovers terraform can no
#   longer see), tell the user to delete them by hand in the console so they are
#   not billed for orphaned resources. Safe to call multiple times.
remind_manual_cleanup() {
	echo ""
	echo -e "${YELLOW}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
	echo -e "${YELLOW}  ⚠ IMPORTANT: avoid unexpected billing${NC}"
	echo -e "${YELLOW}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
	echo -e "  ${YELLOW}If destroy.sh could not remove every resource (e.g. MySQL/Redis stuck${NC}"
	echo -e "  ${YELLOW}in the recycle bin / isolated state), log in to the Tencent Cloud${NC}"
	echo -e "  ${YELLOW}console and delete the leftovers by hand so you are not billed:${NC}"
	echo -e "    ${CYAN}• VPC / network      : https://console.cloud.tencent.com/vpc${NC}"
	echo -e "    ${CYAN}• MySQL recycle bin  : https://console.cloud.tencent.com/cdb/recycle${NC}"
	echo -e "    ${CYAN}• Redis recycle bin  : https://console.cloud.tencent.com/redis/recycle${NC}"
	echo -e "  ${YELLOW}Confirm the resources above (including the recycle bin) are fully${NC}"
	echo -e "  ${YELLOW}released before you finish.${NC}"
	echo ""
}

used_ip_blocker_hint() {
	echo -e "  ${YELLOW}Detected ResourceInUse/UsedIp while deleting the subnet/VPC.${NC}"
	echo -e "  ${YELLOW}This means at least one private IP in the VPC/subnet is still attached to a cloud resource.${NC}"
	echo -e "  ${YELLOW}It is usually NOT caused by MySQL/Redis being in the recycle bin; check these VPC dependencies first:${NC}"
	echo -e "    ${CYAN}• ENI / private IPs : https://console.cloud.tencent.com/vpc/eni${NC}"
	echo -e "    ${CYAN}• CLB instances     : https://console.cloud.tencent.com/clb/instance${NC}"
	echo -e "    ${CYAN}• CVM / TKE workers : https://console.cloud.tencent.com/cvm/instance${NC}"
	echo -e "    ${CYAN}• TKE clusters      : https://console.cloud.tencent.com/tke2/cluster${NC}"
	echo -e "    ${CYAN}• NAT gateways      : https://console.cloud.tencent.com/vpc/nat${NC}"
	echo -e "    ${CYAN}• CFS file systems  : https://console.cloud.tencent.com/cfs${NC}"
	echo -e "    ${CYAN}• TCR VPC attach    : https://console.cloud.tencent.com/tcr${NC}"
	if [ -n "${VPC_ID:-}" ]; then
		echo -e "  ${CYAN}VPC scope:${NC} ${VPC_ID}"
		echo -e "  ${YELLOW}Optional tccli checks:${NC}"
		echo -e "    ${CYAN}tccli vpc DescribeNetworkInterfaces --Filters '[{\"Name\":\"vpc-id\",\"Values\":[\"${VPC_ID}\"]}]' --region ${REGION}${NC}"
		echo -e "    ${CYAN}tccli clb DescribeLoadBalancers --Filters '[{\"Name\":\"vpc-id\",\"Values\":[\"${VPC_ID}\"]}]' --region ${REGION}${NC}"
		echo -e "    ${CYAN}tccli cvm DescribeInstances --Filters '[{\"Name\":\"vpc-id\",\"Values\":[\"${VPC_ID}\"]}]' --region ${REGION}${NC}"
	else
		echo -e "  ${YELLOW}VPC id was not resolved from Terraform state; inspect the VPC named cubesandbox-terraform-vpc in the console.${NC}"
	fi
	NEEDS_MANUAL_CLEANUP=1
}

# destroy_fail — print a clear stop message and abort (fail-fast). The auxiliary
#   best-effort cleanups (recycle bin / orphan CVMs) stay non-fatal; only the
#   actual terraform destroy of each phase triggers this.
destroy_fail() {
	echo ""
	echo -e "${RED}✗ $1 failed; stopping teardown (later phases NOT run, their resources left intact).${NC}"
	echo -e "${YELLOW}  Inspect the error above, then re-run ./destroy.sh to continue from here.${NC}"
	remind_manual_cleanup
	exit 1
}

# ============================================================
# Phase 1/6 — reverse of create step 6: TKE addons → node pool → cluster.
#   Addons can only be deleted while the API Server is reachable, so they go
#   first, then the node-pool workers, then the cluster itself.
# ============================================================
echo ""
echo -e "${CYAN}━━━ Phase 1/6: Destroy TKE (addons → node pool → cluster) ━━━${NC}"

# (a) Kubernetes addon resources first (need a reachable API Server).
k8s_targets=()
while IFS= read -r _addr; do
	[ -n "$_addr" ] && k8s_targets+=(-target="$_addr")
done < <(terraform state list 2>/dev/null | grep -E '^kubernetes_' || true)
if [ "${#k8s_targets[@]}" -gt 0 ]; then
	if in_state 'tencentcloud_kubernetes_cluster.tke'; then
		# The apiserver is intranet-only: open the jumpserver tunnel and localize
		# the kubeconfig so the local kubernetes provider can reach the cluster to
		# delete the addons (the jumpserver is still alive at this point).
		if ! _open_apiserver_tunnel; then
			echo -e "  ${RED}✗ Could not open the intranet API Server tunnel; Kubernetes addons cannot be deleted safely.${NC}"
			echo -e "  ${YELLOW}  Fix jumpserver SSH(443) / kubeconfig access and re-run ./destroy.sh.${NC}"
			echo -e "  ${YELLOW}  If you have already deleted the Kubernetes addons and CLBs manually, set${NC}"
			echo -e "  ${YELLOW}  TENCENTCLOUD_FORCE_PRUNE_K8S_STATE=1 to prune kubernetes_* state and continue.${NC}"
			if [ "${TENCENTCLOUD_FORCE_PRUNE_K8S_STATE:-0}" = "1" ]; then
				echo -e "  ${YELLOW}⚠ FORCE_PRUNE_K8S_STATE enabled: pruning kubernetes_* resources from state without API deletion.${NC}"
				while IFS= read -r _addr; do
					[ -n "$_addr" ] || continue
					echo -e "  ${CYAN}terraform state rm ${_addr}${NC}"
					terraform state rm "$_addr" >/dev/null 2>&1 || true
				done < <(terraform state list 2>/dev/null | grep -E '^kubernetes_' || true)
				NEEDS_MANUAL_CLEANUP=1
				k8s_targets=()
			else
				destroy_fail "TKE addon API tunnel setup"
			fi
		fi

		# Keep .kube/config alive until AFTER the cluster is gone. The kubeconfig
		# is written by a local_file.tke_kubeconfig resource that depends_on every
		# kubernetes_* addon (so it is written LAST on create). `terraform destroy
		# -target=kubernetes_*` also pulls in resources that DEPEND ON the targets,
		# so it would delete local_file.tke_kubeconfig FIRST — removing the file the
		# kubernetes provider reads (config_path) and making every addon delete fall
		# back to http://localhost ("connection refused"). Detach it from state so
		# the targeted destroy leaves it (and the on-disk file) alone; the file is
		# managed by this script and removed at the very end of the teardown.
		if [ "${#k8s_targets[@]}" -gt 0 ] && in_state 'local_file.tke_kubeconfig'; then
			echo -e "  ${CYAN}Detaching local_file.tke_kubeconfig from state so .kube/config survives the addon/cluster teardown...${NC}"
			terraform state rm 'local_file.tke_kubeconfig[0]' >/dev/null 2>&1 ||
				terraform state rm 'local_file.tke_kubeconfig' >/dev/null 2>&1 || true
		fi

		# Delete the namespace LAST, and only after its pods are gone: TKE's
		# Gatekeeper "block-namespace-deletion-rule" rejects deleting a namespace
		# that still contains any pod, and the kubernetes provider removes a
		# Deployment object before its pods finish terminating (background
		# cascade). So split the addons: destroy everything EXCEPT the namespace,
		# drain the leftover pods, then destroy the namespace.
		ns_targets=()
		nonns_targets=()
		for _t in "${k8s_targets[@]+"${k8s_targets[@]}"}"; do
			case "$_t" in
			*kubernetes_namespace.*) ns_targets+=("$_t") ;;
			*) nonns_targets+=("$_t") ;;
			esac
		done

		if [ "${#nonns_targets[@]}" -gt 0 ]; then
			echo -e "  ${CYAN}Removing Kubernetes addon resources (${#nonns_targets[@]})...${NC}"
			_ensure_apiserver_tunnel_ready || destroy_fail "TKE addon API tunnel setup"
			run_destroy "${nonns_targets[@]+"${nonns_targets[@]}"}" || destroy_fail "TKE addons destroy"
		fi

		if [ "${#ns_targets[@]}" -gt 0 ]; then
			_drain_namespace cubesandbox
			echo -e "  ${CYAN}Removing the cubesandbox namespace...${NC}"
			_ensure_apiserver_tunnel_ready || destroy_fail "TKE addon API tunnel setup"
			run_destroy "${ns_targets[@]+"${ns_targets[@]}"}" || destroy_fail "TKE namespace destroy"
		fi
	else
		# Cluster already gone → API unreachable; the orphaned k8s resources can
		# only be dropped from state (not a failure).
		echo -e "  ${YELLOW}TKE cluster is gone; pruning ${#k8s_targets[@]} orphaned Kubernetes resources from state...${NC}"
		while IFS= read -r _addr; do
			[ -n "$_addr" ] || continue
			echo -e "  ${CYAN}terraform state rm ${_addr}${NC}"
			terraform state rm "$_addr" >/dev/null 2>&1 || true
		done < <(terraform state list 2>/dev/null | grep -E '^kubernetes_' || true)
	fi
else
	echo -e "  ${CYAN}No Kubernetes addon resources in state; skipping.${NC}"
fi

# (b) the node-pool CVM workers.
if in_state 'tencentcloud_kubernetes_node_pool.tke'; then
	echo -e "  ${CYAN}Removing TKE node pool (CVM workers)...${NC}"
	run_destroy -target=tencentcloud_kubernetes_node_pool.tke || destroy_fail "TKE node-pool destroy"
else
	echo -e "  ${CYAN}No TKE node pool in state; skipping.${NC}"
fi

# (c) the TKE cluster (its inline initial worker node goes with it).
if in_state 'tencentcloud_kubernetes_cluster.tke'; then
	echo -e "  ${CYAN}Removing the TKE cluster...${NC}"
	run_destroy -target=tencentcloud_kubernetes_cluster.tke || destroy_fail "TKE cluster destroy"
else
	echo -e "  ${CYAN}No TKE cluster in state; skipping.${NC}"
fi

# The addons + cluster are gone; the intranet API Server tunnel is no longer needed.
_close_apiserver_tunnel

# (d) Terminate the worker CVMs the node pool kept behind (delete_keep_instance).
#     Best-effort: they only block the shared key pair, deleted later in phase 6.
echo -e "  ${CYAN}Terminating any orphaned TKE worker CVMs...${NC}"
terminate_cvms "$TKE_NODE_CVMS" || echo -e "${YELLOW}⚠ Orphan CVM termination had issues; continuing.${NC}"

# ============================================================
# Phase 2/6 — reverse of create step 5: MySQL + Redis.
# ============================================================
echo ""
echo -e "${CYAN}━━━ Phase 2/6: Destroy MySQL + Redis ━━━${NC}"
phase_db_targets=()
in_state 'tencentcloud_mysql_instance.mysql' && phase_db_targets+=(-target=tencentcloud_mysql_instance.mysql)
in_state 'tencentcloud_redis_instance.redis' && phase_db_targets+=(-target=tencentcloud_redis_instance.redis)
if [ "${#phase_db_targets[@]}" -gt 0 ]; then
	run_destroy "${phase_db_targets[@]+"${phase_db_targets[@]}"}" || destroy_fail "MySQL/Redis destroy"
else
	echo -e "  ${CYAN}No MySQL/Redis in state; skipping.${NC}"
fi
# Release them from the recycle bin (best-effort; the jumpserver runs tccli).
echo -e "  ${CYAN}Clearing the MySQL/Redis recycle bin...${NC}"
clear_recycle_bin "$MYSQL_ID" "$REDIS_ID" || {
	echo -e "${YELLOW}⚠ Recycle-bin cleanup had issues; continuing.${NC}"
	NEEDS_MANUAL_CLEANUP=1
}

# Fully tear down TCR before targeted CVM destroys. When this bundle has switched
# between singleton and count-gated TCR resources, Terraform may require TCR
# moved targets even while destroying unrelated CVMs. Deleting TCR through
# terraform can fail if repositories still exist, so perform the ordered tccli
# cleanup first while the jumpserver is still alive, then prune the state.
if in_state 'tencentcloud_tcr_instance.cluster'; then
	echo -e "  ${CYAN}Tearing down TCR before CVM teardown (images → namespace → instance + backend bucket)...${NC}"
	if destroy_tcr "$TCR_INSTANCE_ID" "$TCR_NAMESPACE"; then
		echo -e "  ${CYAN}Pruning TCR resources from terraform state (deleted out-of-band)...${NC}"
		tcr_state_prune
	else
		echo -e "${YELLOW}⚠ Could not tear TCR down via tccli now; Phase 4 will retry before any terraform TCR fallback.${NC}"
	fi
fi

# ============================================================
# Phase 3/6 — reverse of create step 3: compute nodes, then the jump-server LAST.
#   The jump-server is destroyed at the END of this phase because it runs the
#   tccli recycle-bin / orphan-CVM cleanup over SSH used by phases 1-2.
# ============================================================
echo ""
echo -e "${CYAN}━━━ Phase 3/6: Destroy compute nodes ━━━${NC}"
if in_state 'tencentcloud_instance.compute'; then
	run_destroy -target=tencentcloud_instance.compute || destroy_fail "Compute node destroy"
else
	echo -e "  ${CYAN}No compute nodes in state; skipping.${NC}"
fi

# Before the jump-server goes away: finish the recycle-bin release (it runs tccli)
# and clear any orphaned CVMs still in the VPC (they hold the shared SG/key pair).
echo -e "  ${CYAN}Verifying MySQL/Redis are fully removed from the recycle bin (jumpserver still alive)...${NC}"
ensure_recycle_bin_cleared "$MYSQL_ID" "$REDIS_ID" || {
	echo -e "${YELLOW}⚠ Recycle-bin verification had issues; continuing.${NC}"
	NEEDS_MANUAL_CLEANUP=1
}
echo -e "  ${CYAN}Terminating any remaining CVMs in the VPC (except the jumpserver)...${NC}"
terminate_vpc_cvms "$VPC_ID" "$JS_INSTANCE_ID" || echo -e "${YELLOW}⚠ VPC CVM cleanup had issues; continuing.${NC}"

echo ""
echo -e "${CYAN}━━━ Phase 3/6: Destroy the jump-server ━━━${NC}"
if in_state 'tencentcloud_instance.jumpserver'; then
	run_destroy -target=tencentcloud_instance.jumpserver || destroy_fail "Jump-server destroy"
else
	echo -e "  ${CYAN}No jump-server in state; skipping.${NC}"
fi

# ============================================================
# Phase 4/6 — reverse of create step 2: TCR (token → attachment → namespace →
#   instance; the tcr_token_deploy null_resource is removed with them).
#   Normally TCR was already fully torn down (and pruned from state) in Phase 3
#   while the jumpserver was alive, so this is a no-op. It only does work when
#   that tccli teardown could not run (e.g. a re-run after the jumpserver is
#   already gone): it retries the ordered tccli teardown via a local tccli, and
#   only as a last resort hands what remains to `terraform destroy`.
# ============================================================
echo ""
echo -e "${CYAN}━━━ Phase 4/6: Destroy TCR ━━━${NC}"
# Retry the full ordered tccli teardown if the instance is still in state.
if in_state 'tencentcloud_tcr_instance.cluster'; then
	echo -e "  ${CYAN}TCR status: ${TCR_INSTANCE_ID:-present} (namespace=${TCR_NAMESPACE}) still present; retrying the ordered teardown (images → namespace → instance + bucket)...${NC}"
	if destroy_tcr "$TCR_INSTANCE_ID" "$TCR_NAMESPACE"; then
		echo -e "  ${CYAN}Pruning TCR resources from terraform state (deleted out-of-band)...${NC}"
		tcr_state_prune
	fi
else
	echo -e "  ${GREEN}TCR status: already destroyed (images → namespace → instance + backend bucket)${NC}"
fi
# Whatever TCR resources remain in state get a terraform destroy as a last resort
# (this can only succeed for resources terraform can delete on its own — e.g. the
# token / VPC attachment; the namespace/instance need the tccli path above).
phase_tcr_targets=()
for _res in \
	null_resource.tcr_token_deploy \
	tencentcloud_tcr_token.cluster \
	tencentcloud_tcr_vpc_attachment.cluster \
	tencentcloud_tcr_namespace.cluster \
	tencentcloud_tcr_instance.cluster; do
	in_state "$_res" && phase_tcr_targets+=(-target="$_res")
done
if [ "${#phase_tcr_targets[@]}" -gt 0 ]; then
	run_destroy "${phase_tcr_targets[@]+"${phase_tcr_targets[@]}"}" || destroy_fail "TCR destroy"
else
	echo -e "  ${CYAN}No TCR resources in state; skipping.${NC}"
fi

# ============================================================
# Phase 4.5/6 — CFS shared storage (cube-master /data/CubeMaster/storage).
#   Must be torn down BEFORE the subnet it lives in: the CFS NFS mount target is
#   an ENI inside tencentcloud_subnet.cluster, so destroying the subnet first would
#   fail with "subnet in use". The file system is destroyed before its access
#   rule/group (dependency order within the same targeted run).
# ============================================================
echo ""
echo -e "${CYAN}━━━ Destroy CFS shared storage ━━━${NC}"
phase_cfs_targets=()
for _res in \
	tencentcloud_cfs_file_system.cubemaster_data \
	tencentcloud_cfs_access_rule.cubemaster_data \
	tencentcloud_cfs_access_group.cubemaster_data; do
	in_state "$_res" && phase_cfs_targets+=(-target="$_res")
done
if [ "${#phase_cfs_targets[@]}" -gt 0 ]; then
	run_destroy "${phase_cfs_targets[@]+"${phase_cfs_targets[@]}"}" || destroy_fail "CFS destroy"
else
	echo -e "  ${CYAN}No CFS resources in state; skipping.${NC}"
fi

# ============================================================
# Phase 5/6 — reverse of create step 1 (part 1): NAT gateway + subnet.
#   Delete NAT dependencies first, then subnets in a second targeted destroy.
#   Keeping NAT and subnet in separate terraform runs avoids relying on targeted
#   plan ordering when the subnet is still referenced by NAT/EIP route resources.
# ============================================================
echo ""
echo -e "${CYAN}━━━ Phase 5/6: Destroy NAT gateway + subnet ━━━${NC}"
if in_state 'tencentcloud_nat_gateway.cluster'; then
	echo -e "  ${CYAN}NAT gateway status: ${NAT_GATEWAY_ID:-present} → destroying (also releases its EIP + the NAT route)${NC}"
else
	echo -e "  ${GREEN}NAT gateway status: already destroyed (absent)${NC}"
fi
phase_nat_targets=()
for _res in \
	tencentcloud_route_table_entry.nat \
	tencentcloud_nat_gateway.cluster \
	tencentcloud_eip.nat; do
	in_state "$_res" && phase_nat_targets+=(-target="$_res")
done
if [ "${#phase_nat_targets[@]}" -gt 0 ]; then
	run_destroy "${phase_nat_targets[@]+"${phase_nat_targets[@]}"}" || destroy_fail "NAT gateway/EIP destroy"
else
	echo -e "  ${CYAN}No NAT gateway/EIP resources in state; skipping.${NC}"
fi

phase_subnet_targets=()
in_state tencentcloud_subnet.cluster && phase_subnet_targets+=(-target=tencentcloud_subnet.cluster)
while IFS= read -r _cvm_subnet; do
	[ -n "$_cvm_subnet" ] || continue
	phase_subnet_targets+=(-target="$_cvm_subnet")
done < <(terraform state list 2>/dev/null | grep -E '^tencentcloud_subnet\.cvm\[' || true)
if [ "${#phase_subnet_targets[@]}" -gt 0 ]; then
	run_destroy "${phase_subnet_targets[@]+"${phase_subnet_targets[@]}"}" || destroy_fail "subnet destroy"
else
	echo -e "  ${CYAN}No subnet resources in state; skipping.${NC}"
fi

# ============================================================
# Phase 6/6 — reverse of create step 1 (part 2): VPC, security group, key pair,
#   random suffix. A final full destroy (no -target) also sweeps anything left.
# ============================================================
echo ""
echo -e "${CYAN}━━━ Phase 6/6: Destroy VPC / security group / key pair (final sweep) ━━━${NC}"
run_destroy "${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}" || destroy_fail "Final network destroy"

# Remove the local kubeconfig artifact. It was deliberately kept on disk through
# the teardown (and detached from terraform state in Phase 1) so the kubernetes
# provider could reach the cluster while the addons were deleted; nothing needs
# it now that everything is gone.
rm -f "${SCRIPT_DIR}/.kube/config" 2>/dev/null || true

echo ""
_draw_box "${GREEN}" \
	"✓ All Tencent Cloud resources destroyed" \
	"CVM (jump-server + compute) / TKE / CLB (intranet + internet) / MySQL /" \
	"Redis / TCR / NAT / VPC / subnet / security group / EIP / key pair are gone;" \
	"the jump-server was destroyed last."

# Even on a clean terraform destroy, isolated MySQL/Redis can linger in the
# recycle bin; if any best-effort cleanup flagged trouble, surface the manual
# steps so the user is not silently billed for orphaned resources.
if [ "${NEEDS_MANUAL_CLEANUP:-0}" = "1" ]; then
	remind_manual_cleanup
else
	echo ""
	echo -e "${CYAN}Note: if the console still shows MySQL/Redis/TCR/TKE or other resources in the recycle bin, delete them by hand to avoid billing.${NC}"
fi
