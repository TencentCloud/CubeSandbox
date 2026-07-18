{{/* Common template helpers for CubeSandbox chart. */}}
{{- define "cube.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "cube.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "cube.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
app.kubernetes.io/name: {{ include "cube.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "cube.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cube.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- /*
Render "<repository>:<tag>" for an image dict. Legacy helper used everywhere
in the chart. Does NOT apply global.imageRegistry; call sites for
Cube-owned images should use `cube.cubeImage` instead.
*/}}
{{- define "cube.image" -}}
{{- printf "%s:%s" .repository .tag -}}
{{- end -}}

{{- /*
Render "<repository>:<tag>" for a Cube-owned image with optional
$.Values.global.imageRegistry override applied to the registry portion of
.repository. Call as:
  include "cube.cubeImage" (dict "image" .Values.images.master "context" $)
When global.imageRegistry is empty the output is identical to cube.image;
setting it rewrites the leading registry host (segment before the first "/")
so the same chart can be republished to any private registry without editing
each per-image entry. Everything after the first "/" (the repository path)
is preserved.
*/}}
{{- define "cube.cubeImage" -}}
{{- $image := .image -}}
{{- $ctx := .context -}}
{{- $repo := $image.repository -}}
{{- $override := (default (dict) $ctx.Values.global).imageRegistry | default "" -}}
{{- if $override -}}
  {{- $parts := splitList "/" $repo -}}
  {{- if gt (len $parts) 1 -}}
    {{- $repo = printf "%s/%s" (trimSuffix "/" $override) (join "/" (rest $parts)) -}}
  {{- else -}}
    {{- $repo = printf "%s/%s" (trimSuffix "/" $override) $repo -}}
  {{- end -}}
{{- end -}}
{{- printf "%s:%s" $repo $image.tag -}}
{{- end -}}

{{- define "cube.timezoneEnv" -}}
{{- with .Values.global.timezone }}
- name: TZ
  value: {{ . | quote }}
{{- end }}
{{- end -}}

{{- define "cube.controlPlanePlacement" -}}
{{- with .Values.placement.controlPlane.nodeSelector }}
nodeSelector:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.placement.controlPlane.tolerations }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}

{{- define "cube.computePlacement" -}}
{{- with .Values.placement.compute.nodeSelector }}
nodeSelector:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.placement.compute.affinity }}
affinity:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.placement.compute.tolerations }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}

{{- define "cube.pvmPlacement" -}}
{{- with .Values.placement.pvm.nodeSelector }}
nodeSelector:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.placement.pvm.affinity }}
affinity:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.placement.pvm.tolerations }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}

{{/* Proxy Service FQDN and cluster-DNS enablement helpers. */}}

{{- define "cube.nodeServiceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- printf "%s-node" (include "cube.fullname" .) -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "cube.masterName" -}}
{{- printf "%s-master" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.apiName" -}}
{{- printf "%s-api" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.cubemastercliName" -}}
{{- printf "%s-cubemastercli" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.webuiName" -}}
{{- printf "%s-webui" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.nodeName" -}}
{{- printf "%s-node" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.nodeInstallerName" -}}
{{- printf "%s-node-installer" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.nodeBootstrapName" -}}
{{- printf "%s-node-bootstrap" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.nodePvmName" -}}
{{- printf "%s-node-pvm" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.proxyName" -}}
{{- printf "%s-proxy" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.proxyEnabled" -}}
{{- if and .Values.cubeProxy.enabled (or .Values.controlPlane.enabled (not .Values.externalControlPlane.enabled)) -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "cube.proxyServiceFQDN" -}}
{{- printf "%s.%s.svc.%s" (include "cube.proxyName" .) .Release.Namespace (include "cube.clusterDomain" .) -}}
{{- end -}}

{{- define "cube.lifecycleManagerName" -}}
{{- printf "%s-lifecycle-manager" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.lifecycleManagerEnabled" -}}
{{- $lcm := default dict .Values.lifecycleManager -}}
{{- if and (dig "enabled" true $lcm) (eq (include "cube.proxyEnabled" .) "true") -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "cube.lifecycleManagerFQDN" -}}
{{- printf "%s.%s.svc.%s" (include "cube.lifecycleManagerName" .) .Release.Namespace (include "cube.clusterDomain" .) -}}
{{- end -}}

{{- define "cube.lifecycleManagerAddr" -}}
{{- printf "%s:%v" (include "cube.lifecycleManagerFQDN" .) .Values.lifecycleManager.service.port -}}
{{- end -}}

{{- define "cube.configureClusterDNS" -}}
{{- if and .Values.cubeProxy.configureClusterDNS (eq (include "cube.proxyEnabled" .) "true") -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "cube.cubemastercliEnabled" -}}
{{- $cubemastercli := default dict .Values.cubemastercli -}}
{{- if and (dig "enabled" true $cubemastercli) (or .Values.controlPlane.enabled .Values.externalControlPlane.enabled) -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "cube.mysqlName" -}}
{{- printf "%s-mysql" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.redisName" -}}
{{- printf "%s-redis" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.secretName" -}}
{{- printf "%s-secret" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.masterConfigSecretName" -}}
{{- printf "%s-master-config" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.masterStoragePVCName" -}}
{{- if .Values.controlPlane.master.persistence.existingClaim -}}
{{- .Values.controlPlane.master.persistence.existingClaim -}}
{{- else -}}
{{- printf "%s-master-storage" (include "cube.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "cube.mysqlPVCName" -}}
{{- if .Values.mysql.persistence.existingClaim -}}
{{- .Values.mysql.persistence.existingClaim -}}
{{- else -}}
{{- printf "%s-mysql-data" (include "cube.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "cube.redisPVCName" -}}
{{- if .Values.redis.persistence.existingClaim -}}
{{- .Values.redis.persistence.existingClaim -}}
{{- else -}}
{{- printf "%s-redis-data" (include "cube.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
Resolve PVC storageClassName for a stateful component (master / mysql / redis).

Call as:
  include "cube.persistenceStorageClassName" (dict "root" . "component" .Values.mysql.persistence)

Precedence:
  1. component.storageClassName if non-empty (always wins)
  2. else persistence.storageClassName (top-level convenience key)
  3. else empty → omit the field so the cluster default StorageClass applies

Do not confuse with storageClass.create / storageClass.name (those create a
chart-owned StorageClass). This helper only picks which SC name a PVC binds to.
*/}}
{{- define "cube.persistenceStorageClassName" -}}
{{- $component := "" -}}
{{- if and .component (hasKey .component "storageClassName") -}}
{{- $component = .component.storageClassName | toString -}}
{{- end -}}
{{- if $component -}}
{{- $component -}}
{{- else if and .root.Values.persistence .root.Values.persistence.storageClassName -}}
{{- .root.Values.persistence.storageClassName | toString -}}
{{- end -}}
{{- end -}}

{{- define "cube.proxyCertSecretName" -}}
{{- if and (eq .Values.cubeProxy.tls.mode "existingSecret") .Values.cubeProxy.tls.existingSecret -}}
{{- .Values.cubeProxy.tls.existingSecret -}}
{{- else if .Values.cubeProxy.tls.secretName -}}
{{- .Values.cubeProxy.tls.secretName -}}
{{- else -}}
{{- printf "%s-proxy-certs" (include "cube.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "cube.egressCASecretName" -}}
{{- if and (eq .Values.cubeEgress.ca.mode "existingSecret") .Values.cubeEgress.ca.existingSecret -}}
{{- .Values.cubeEgress.ca.existingSecret -}}
{{- else if .Values.cubeEgress.ca.secretName -}}
{{- .Values.cubeEgress.ca.secretName -}}
{{- else -}}
{{- printf "%s-egress-ca" (include "cube.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "cube.masterEndpoint" -}}
{{- if .Values.externalControlPlane.enabled -}}
{{- .Values.externalControlPlane.masterEndpoint -}}
{{- else -}}
{{- printf "%s.%s.svc.%s:%v" (include "cube.masterName" .) .Release.Namespace (include "cube.clusterDomain" .) .Values.controlPlane.master.service.port -}}
{{- end -}}
{{- end -}}

{{- define "cube.cubemastercliMasterEndpoint" -}}
{{- if .Values.externalControlPlane.enabled -}}
{{- .Values.externalControlPlane.masterEndpoint -}}
{{- else if and .Values.controlPlane.enabled .Values.controlPlane.master.enabled -}}
{{- include "cube.masterEndpoint" . -}}
{{- end -}}
{{- end -}}

{{- define "cube.cubemastercliMasterAddress" -}}
{{- $endpoint := include "cube.cubemastercliMasterEndpoint" . -}}
{{- $withoutHTTP := trimPrefix "http://" (trimPrefix "https://" $endpoint) -}}
{{- $hostPort := first (splitList "/" $withoutHTTP) -}}
{{- regexReplaceAll ":[0-9]+$" $hostPort "" -}}
{{- end -}}

{{- define "cube.cubemastercliMasterPort" -}}
{{- $endpoint := include "cube.cubemastercliMasterEndpoint" . -}}
{{- $withoutHTTP := trimPrefix "http://" (trimPrefix "https://" $endpoint) -}}
{{- $hostPort := first (splitList "/" $withoutHTTP) -}}
{{- $port := regexFind "[0-9]+$" $hostPort -}}
{{- default "8089" $port -}}
{{- end -}}

{{- define "cube.apiEndpoint" -}}
{{- if .Values.externalControlPlane.enabled -}}
{{- .Values.externalControlPlane.apiEndpoint -}}
{{- else -}}
{{- printf "http://%s.%s.svc.%s:%v" (include "cube.apiName" .) .Release.Namespace (include "cube.clusterDomain" .) .Values.controlPlane.api.service.port -}}
{{- end -}}
{{- end -}}

{{- define "cube.mysqlHost" -}}
{{- if .Values.mysql.host -}}{{ .Values.mysql.host }}{{- else -}}{{ include "cube.mysqlName" . }}.{{ .Release.Namespace }}.svc.{{ include "cube.clusterDomain" . }}{{- end -}}
{{- end -}}

{{- define "cube.mysqlBuiltinEnabled" -}}
{{- if and .Values.controlPlane.enabled .Values.mysql.enabled (not .Values.mysql.host) -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "cube.redisHost" -}}
{{- if .Values.redis.host -}}{{ .Values.redis.host }}{{- else -}}{{ include "cube.redisName" . }}.{{ .Release.Namespace }}.svc.{{ include "cube.clusterDomain" . }}{{- end -}}
{{- end -}}

{{- define "cube.redisBuiltinEnabled" -}}
{{- if and (or .Values.controlPlane.enabled (eq (include "cube.proxyEnabled" .) "true")) .Values.redis.enabled (not .Values.redis.host) -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "cube.egressNetProbeCommand" -}}
set -e
iface="${CUBE_INGRESS_IFACE:-cube-dev}"
table="${CUBE_EGRESS_NET_ROUTE_TABLE:-100}"
chain="${CUBE_EGRESS_NET_CHAIN:-TRANSPROXY}"
ip link show "${iface}" >/dev/null
ip rule show | grep -q "iif ${iface} ipproto tcp dport 80 lookup ${table}"
ip rule show | grep -q "iif ${iface} ipproto tcp dport 443 lookup ${table}"
ip route show table "${table}" | grep -Eq "local (default|0\\.0\\.0\\.0/0) dev lo"
iptables -t mangle -S "${chain}" | grep -q -- "--dport 80"
iptables -t mangle -S "${chain}" | grep -q -- "--dport 443"
{{- end -}}

{{- define "cube.secretEnabled" -}}
{{- if or (and .Values.controlPlane.enabled (or .Values.controlPlane.master.enabled .Values.controlPlane.api.enabled)) (eq (include "cube.proxyEnabled" .) "true") (eq (include "cube.mysqlBuiltinEnabled" .) "true") (eq (include "cube.redisBuiltinEnabled" .) "true") -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{/*
Cluster domain used to build cluster-local DNS names (e.g. cluster.local).
Priority: .Values.global.clusterDomain > .Values.cubeNode.dns.clusterDomain > cluster.local.
Set global.clusterDomain when the cluster is configured with kubelet
--cluster-domain=<something-other-than-cluster.local>.
*/}}
{{- define "cube.clusterDomain" -}}
{{- $global := (default (dict) .Values.global).clusterDomain -}}
{{- $cubeNode := (default (dict) (default (dict) .Values.cubeNode).dns).clusterDomain -}}
{{- default (default "cluster.local" $cubeNode) $global -}}
{{- end -}}

{{/*
Port that CubeAPI binds on, extracted from controlPlane.api.bind (default
"0.0.0.0:3000"). Used for both containerPort and probes so operators can
change bind without editing multiple places.
*/}}
{{- define "cube.apiBindPort" -}}
{{- $bind := default "0.0.0.0:3000" .Values.controlPlane.api.bind -}}
{{- $port := regexFind "[0-9]+$" $bind -}}
{{- default "3000" $port -}}
{{- end -}}

{{/*
cube-node always uses OpenKruise Advanced DaemonSet (hard dependency).
*/}}
{{- define "cube.nodeDaemonSetAPIVersion" -}}
apps.kruise.io/v1beta1
{{- end -}}

{{/*
Kubernetes API path prefix for the cube-node Advanced DaemonSet (health-test).
*/}}
{{- define "cube.nodeDaemonSetAPIPath" -}}
/apis/apps.kruise.io/v1beta1/namespaces
{{- end -}}

{{/*
Big Pod: shared volumeMounts for component install/run containers.
Toolbox is mounted whole at the fixed path (InPlace-stable).
*/}}
{{- define "cube.nodeToolboxVolumeMounts" -}}
- name: toolbox
  mountPath: /usr/local/services/cubetoolbox
- name: data-cubelet
  mountPath: {{ .Values.hostPaths.dataCubelet }}
  mountPropagation: Bidirectional
- name: data-log
  mountPath: {{ .Values.hostPaths.dataLog }}
- name: data-cube-shim
  mountPath: {{ .Values.hostPaths.dataCubeShim }}
  mountPropagation: Bidirectional
- name: data-snapshot-pack
  mountPath: {{ .Values.hostPaths.dataSnapshotPack }}
- name: tmp-cube
  mountPath: {{ .Values.hostPaths.tmpCube }}
  mountPropagation: Bidirectional
- name: run-containerd
  mountPath: /run/containerd
- name: run-vc
  mountPath: /run/vc
- name: cube-pid
  mountPath: /run/cube-node
{{- end -}}

{{- define "cube.nodeDataplaneVolumeMounts" -}}
{{- include "cube.nodeToolboxVolumeMounts" . }}
- name: bootstrap-state
  mountPath: {{ .Values.hostPaths.bootstrapState }}
- name: dev
  mountPath: /dev
- name: sys
  mountPath: /sys
- name: lib-modules
  mountPath: /lib/modules
  readOnly: true
{{- end -}}

{{/*
Privileged securityContext shared by cubelet / network-agent / placeholder slots.
Must stay identical across frozen Big Pod containers (securityContext is not InPlace).
*/}}
{{- define "cube.nodeDataplaneSecurityContext" -}}
privileged: {{ .Values.security.privileged }}
capabilities:
  add:
    - SYS_ADMIN
    - NET_ADMIN
    - SYS_MODULE
    - SYS_RESOURCE
    - IPC_LOCK
    - SYS_PTRACE
{{- end -}}

{{/*
Installer: toolbox only (no dataplane mounts).
*/}}
{{- define "cube.installerVolumeMounts" -}}
- name: toolbox
  mountPath: /usr/local/services/cubetoolbox
- name: bootstrap-state
  mountPath: {{ .Values.hostPaths.bootstrapState }}
{{- end -}}

{{- define "cube.installerComponentEnv" -}}
{{- include "cube.timezoneEnv" . }}
- name: TOOLBOX_ROOT
  value: /usr/local/services/cubetoolbox
- name: IMAGE_ROOT
  value: /opt/cube-image
- name: STATE_DIR
  value: {{ .Values.hostPaths.bootstrapState | quote }}
- name: CUBE_PVM_ENABLE
  value: {{ ternary "1" "0" .Values.cubeNode.pvmGuestKernel.enabled | quote }}
{{- end -}}

{{/*
Bootstrap: host mutation mounts for pvm / node-init.
*/}}
{{- define "cube.bootstrapHostVolumeMounts" -}}
- name: host-root
  mountPath: /host
- name: dev
  mountPath: /dev
- name: sys
  mountPath: /sys
- name: lib-modules
  mountPath: /lib/modules
  readOnly: true
- name: bootstrap-state
  mountPath: {{ .Values.hostPaths.bootstrapState }}
{{- end -}}

{{- define "cube.bootstrapDataVolumeMounts" -}}
- name: data-cubelet
  mountPath: {{ .Values.hostPaths.dataCubelet }}
  mountPropagation: Bidirectional
- name: data-log
  mountPath: {{ .Values.hostPaths.dataLog }}
- name: data-cube-shim
  mountPath: {{ .Values.hostPaths.dataCubeShim }}
  mountPropagation: Bidirectional
- name: data-snapshot-pack
  mountPath: {{ .Values.hostPaths.dataSnapshotPack }}
- name: tmp-cube
  mountPath: {{ .Values.hostPaths.tmpCube }}
  mountPropagation: Bidirectional
{{- end -}}

{{- define "cube.bootstrapVolumes" -}}
- name: host-root
  hostPath:
    path: {{ .Values.hostPaths.root }}
- name: dev
  hostPath:
    path: {{ .Values.hostPaths.dev }}
- name: sys
  hostPath:
    path: {{ .Values.hostPaths.sys }}
- name: lib-modules
  hostPath:
    path: {{ .Values.hostPaths.libModules }}
- name: bootstrap-state
  hostPath:
    path: {{ .Values.hostPaths.bootstrapState }}
    type: DirectoryOrCreate
- name: data-cubelet
  hostPath:
    path: {{ .Values.hostPaths.dataCubelet }}
    type: DirectoryOrCreate
- name: data-log
  hostPath:
    path: {{ .Values.hostPaths.dataLog }}
    type: DirectoryOrCreate
- name: data-cube-shim
  hostPath:
    path: {{ .Values.hostPaths.dataCubeShim }}
    type: DirectoryOrCreate
- name: data-snapshot-pack
  hostPath:
    path: {{ .Values.hostPaths.dataSnapshotPack }}
    type: DirectoryOrCreate
- name: tmp-cube
  hostPath:
    path: {{ .Values.hostPaths.tmpCube }}
    type: DirectoryOrCreate
{{- end -}}

{{- define "cube.nodeComponentCommonEnv" -}}
{{- include "cube.timezoneEnv" . }}
- name: TOOLBOX_ROOT
  value: /usr/local/services/cubetoolbox
- name: IMAGE_ROOT
  value: /opt/cube-image
- name: CUBE_PID_DIR
  value: /run/cube-node
- name: STATE_DIR
  value: {{ .Values.hostPaths.bootstrapState | quote }}
- name: CUBE_MASTER_ENDPOINT
  value: {{ include "cube.masterEndpoint" . | quote }}
- name: CUBE_SANDBOX_NODE_ID
  valueFrom:
    fieldRef:
      fieldPath: spec.nodeName
- name: CUBE_SANDBOX_ENDPOINT_IP
  valueFrom:
    fieldRef:
      fieldPath: status.podIP
- name: CUBE_PVM_ENABLE
  value: {{ ternary "1" "0" .Values.cubeNode.pvmGuestKernel.enabled | quote }}
- name: CUBE_SANDBOX_AUTO_DETECT_ETH
  value: {{ .Values.cubeNode.network.autoDetectEthName | quote }}
- name: CUBE_SANDBOX_ETH_NAME
  value: {{ .Values.cubeNode.network.ethName | quote }}
- name: CUBE_SANDBOX_NETWORK_CIDR
  value: {{ .Values.cubeNode.network.cidr | quote }}
- name: CUBE_SANDBOX_DNS_SERVERS
  {{- if .Values.cubeNode.dns.sandbox.nameservers }}
  value: {{ join "," .Values.cubeNode.dns.sandbox.nameservers | quote }}
  {{- else }}
  value: ""
  {{- end }}
- name: CUBE_SANDBOX_DNS_FOLLOW_NODE
  value: {{ ternary "true" "false" (and .Values.cubeNode.dns.sandbox.followNodeDns (not .Values.cubeNode.dns.sandbox.nameservers)) | quote }}
{{- end -}}

{{/*
Selective toolbox sync helper kept for reference / one-off jobs.
Current chart uses per-component /opt/cube-image copy instead.
*/}}
{{- define "cube.stageToolboxScript" -}}
set -euo pipefail
echo "cube.stageToolboxScript is superseded by per-component install containers" >&2
exit 1
{{- end -}}
