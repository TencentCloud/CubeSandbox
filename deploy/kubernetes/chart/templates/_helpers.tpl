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

{{- define "cube.image" -}}
{{- printf "%s:%s" .repository .tag -}}
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

{{- define "cube.proxyName" -}}
{{- printf "%s-proxy-node" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.proxyEnabled" -}}
{{- if and .Values.cubeProxy.enabled (or .Values.controlPlane.enabled (not .Values.externalControlPlane.enabled)) -}}true{{- else -}}false{{- end -}}
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
{{- printf "%s.%s.svc.cluster.local:%v" (include "cube.masterName" .) .Release.Namespace .Values.controlPlane.master.service.port -}}
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
{{- printf "http://%s.%s.svc.cluster.local:%v" (include "cube.apiName" .) .Release.Namespace .Values.controlPlane.api.service.port -}}
{{- end -}}
{{- end -}}

{{- define "cube.mysqlHost" -}}
{{- if .Values.mysql.host -}}{{ .Values.mysql.host }}{{- else -}}{{ include "cube.mysqlName" . }}.{{ .Release.Namespace }}.svc.cluster.local{{- end -}}
{{- end -}}

{{- define "cube.mysqlBuiltinEnabled" -}}
{{- if and .Values.controlPlane.enabled .Values.mysql.enabled (not .Values.mysql.host) -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "cube.redisHost" -}}
{{- if .Values.redis.host -}}{{ .Values.redis.host }}{{- else -}}{{ include "cube.redisName" . }}.{{ .Release.Namespace }}.svc.cluster.local{{- end -}}
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
