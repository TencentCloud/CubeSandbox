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
Official Cube TCR hosts that global.imageRegistry may rewrite.
*/}}
{{- define "cube.officialImageRegistries" -}}
cube-sandbox-int.tencentcloudcr.com,cube-sandbox-cn.tencentcloudcr.com
{{- end -}}

{{- /*
Render "<repository>:<tag>" for a Cube-owned image. Call as:
  include "cube.cubeImage" (dict "image" .Values.images.master "context" $)
When global.imageRegistry is set, the leading host of .repository is
rewritten to it — but only if it is an official Cube TCR host
(cube.officialImageRegistries); other repositories render unchanged.
Without it the output is identical to cube.image.
*/}}
{{- define "cube.cubeImage" -}}
{{- $image := .image -}}
{{- $ctx := .context -}}
{{- $repo := $image.repository -}}
{{- $override := (default (dict) $ctx.Values.global).imageRegistry | default "" -}}
{{- $host := index (splitList "/" $repo) 0 -}}
{{- $official := splitList "," (include "cube.officialImageRegistries" .context) -}}
{{- if and $override (has $host $official) -}}
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

{{- /* Helm tests except node-runtime-test: both plane taints, no nodeSelector. */ -}}
{{- define "cube.testPlacement" -}}
{{- $tolerations := concat (.Values.placement.controlPlane.tolerations | default list) (.Values.placement.compute.tolerations | default list) -}}
{{- with $tolerations }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}

{{- define "cube.pvmPlacement" -}}
{{- $root := . -}}
{{- $gateEnabled := eq (include "cube.startupGateEnabled" .) "true" -}}
{{- with .Values.placement.pvm.nodeSelector }}
nodeSelector:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.placement.pvm.affinity }}
affinity:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- if .Values.placement.pvm.tolerations }}
tolerations:
  {{- toYaml .Values.placement.pvm.tolerations | nindent 2 }}
  {{- if $gateEnabled }}
  - key: {{ $root.Values.bootstrap.pvmHostKernel.startupGate.taintKey }}
    operator: Exists
    effect: {{ $root.Values.bootstrap.pvmHostKernel.startupGate.effect }}
  {{- end }}
{{- else if $gateEnabled }}
tolerations:
  - key: {{ $root.Values.bootstrap.pvmHostKernel.startupGate.taintKey }}
    operator: Exists
    effect: {{ $root.Values.bootstrap.pvmHostKernel.startupGate.effect }}
{{- end }}
{{- end -}}

{{/* Shared env for cube-node-pvm init + hold (fingerprint + gate identity). */}}
{{- define "cube.pvmHostCommonEnv" -}}
- name: NODE_NAME
  valueFrom:
    fieldRef:
      fieldPath: spec.nodeName
- name: POD_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
- name: HOST_ROOT
  value: /host
- name: STATE_DIR
  value: {{ .Values.hostPaths.bootstrapState | quote }}
- name: PVM_ENABLED
  value: "1"
- name: DESIRED_KERNEL_PATTERN
  value: {{ .Values.bootstrap.pvmHostKernel.desiredKernelPattern | quote }}
- name: KERNEL_BOOT_ARGS
  value: {{ .Values.bootstrap.pvmHostKernel.bootArgs | quote }}
- name: STARTUP_GATE_ENABLED
  value: {{ ternary "true" "false" .Values.bootstrap.pvmHostKernel.startupGate.enabled | quote }}
- name: STARTUP_GATE_TAINT_KEY
  value: {{ .Values.bootstrap.pvmHostKernel.startupGate.taintKey | quote }}
- name: STARTUP_GATE_TAINT_EFFECT
  value: {{ .Values.bootstrap.pvmHostKernel.startupGate.effect | quote }}
{{- end -}}

{{/* Proxy Service FQDN and cluster-DNS enablement helpers. */}}

{{- define "cube.nodeServiceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- printf "%s-node" (include "cube.fullname" .) -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "cube.releaseIdentityHash" -}}
{{- printf "%s/%s" .Release.Namespace .Release.Name | sha256sum | trunc 10 -}}
{{- end -}}

{{- define "cube.nodeClusterRoleName" -}}
{{- $base := include "cube.fullname" . | trunc 41 | trimSuffix "-" -}}
{{- printf "cube-node-%s-%s" $base (include "cube.releaseIdentityHash" .) -}}
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

{{- define "cube.cubeopscliName" -}}
{{- printf "%s-cubeopscli" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.webuiName" -}}
{{- printf "%s-webui" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.opsName" -}}
{{- printf "%s-ops" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.opsEnabled" -}}
{{- $ops := default dict .Values.cubeOps -}}
{{- if and .Values.controlPlane.enabled (dig "enabled" true $ops) -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "cube.opsFQDN" -}}
{{- printf "%s.%s.svc.%s" (include "cube.opsName" .) .Release.Namespace (include "cube.clusterDomain" .) -}}
{{- end -}}

{{- define "cube.opsUpstream" -}}
{{- $override := dig "opsUpstream" "" (default dict .Values.webui) | trimSuffix "/" -}}
{{- if $override -}}
{{- $override -}}
{{- else -}}
{{- printf "http://%s:%v" (include "cube.opsFQDN" .) .Values.cubeOps.service.port -}}
{{- end -}}
{{- end -}}

{{- define "cube.webuiProxyUpstream" -}}
{{- $override := dig "proxyUpstream" "" (default dict .Values.webui) | trimSuffix "/" -}}
{{- if $override -}}
{{- $override -}}
{{- else if eq (include "cube.proxyEnabled" .) "true" -}}
{{- printf "http://%s:%v" (include "cube.proxyServiceFQDN" .) .Values.cubeProxy.ports.http.containerPort -}}
{{- else -}}
{{- "" -}}
{{- end -}}
{{- end -}}

{{/*
WebUI OpenResty config: static SPA + /opsapi|/cubeapi/v1/SDK → CubeOps, optional /sandbox/ → CubeProxy.
Rendered into cube-webui-config and checksum'd so edits roll the Deployment.
*/}}
{{- define "cube.webuiNginxConf" -}}
{{- $opsUpstream := include "cube.opsUpstream" . -}}
{{- $proxyUpstream := include "cube.webuiProxyUpstream" . -}}
worker_processes auto;

events {
    worker_connections 1024;
}

http {
    include /usr/local/openresty/nginx/conf/mime.types;
    default_type application/octet-stream;

    sendfile on;
    tcp_nopush on;
    tcp_nodelay on;
    keepalive_timeout 65;

    gzip on;
    gzip_types text/plain text/css application/javascript application/json application/xml;

    map $http_upgrade $connection_upgrade {
        default upgrade;
        ''      '';
    }

    server {
        listen 80;
        server_name _;

        root /usr/share/nginx/html;
        index index.html;

        location = /cubeapi {
            return 308 /cubeapi/;
        }

        {{- if $proxyUpstream }}
        location ^~ /sandbox/ {
            proxy_http_version 1.1;
            proxy_set_header Host $host;
            proxy_set_header X-Real-IP $remote_addr;
            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
            proxy_set_header X-Forwarded-Proto $scheme;
            proxy_set_header Upgrade $http_upgrade;
            proxy_set_header Connection $connection_upgrade;
            proxy_read_timeout 7206s;
            proxy_send_timeout 7206s;

            proxy_pass {{ $proxyUpstream }};
        }
        {{- end }}

        location /opsapi/ {
            proxy_http_version 1.1;
            proxy_set_header Host $host;
            proxy_set_header X-Real-IP $remote_addr;
            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
            proxy_set_header X-Forwarded-Proto $scheme;
            client_max_body_size 8g;
            proxy_request_buffering off;
            proxy_read_timeout 1800s;
            proxy_send_timeout 1800s;

            proxy_pass {{ $opsUpstream }}/api/;
        }

        location /cubeapi/v1/ {
            proxy_http_version 1.1;
            proxy_set_header Host $host;
            proxy_set_header X-Real-IP $remote_addr;
            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
            proxy_set_header X-Forwarded-Proto $scheme;
            client_max_body_size 8g;
            proxy_read_timeout 1800s;
            proxy_send_timeout 1800s;

            rewrite ^/cubeapi/v1/(.*)$ /api/v1/sdk/$1 break;
            proxy_pass {{ $opsUpstream }};
        }

        location = /health {
            proxy_pass {{ $opsUpstream }}/health;
        }

        location ~ ^/(sandboxes|v2/sandboxes|templates|snapshots) {
            if ($http_authorization = "") {
                return 418;
            }
            add_header Vary "Authorization" always;
            add_header Cache-Control "no-store" always;
            proxy_http_version 1.1;
            proxy_set_header Host $host;
            proxy_set_header X-Real-IP $remote_addr;
            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
            proxy_set_header X-Forwarded-Proto $scheme;
            client_max_body_size 8g;
            proxy_read_timeout 1800s;
            proxy_send_timeout 1800s;

            rewrite ^/(.*)$ /api/v1/sdk/$1 break;
            proxy_pass {{ $opsUpstream }};
        }

        error_page 418 = @spa_fallback;
        location @spa_fallback {
            root /usr/share/nginx/html;
            try_files /index.html =404;
            add_header Vary "Authorization" always;
            add_header Cache-Control "no-store, no-cache, must-revalidate" always;
        }

        location /assets/ {
            try_files $uri =404;
            expires 1y;
            add_header Cache-Control "public, immutable";
        }

        location / {
            try_files $uri $uri/ /index.html;
        }
    }
}
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

{{- define "cube.cubeopscliEnabled" -}}
{{- $cubeopscli := default dict .Values.cubeopscli -}}
{{- if and (dig "enabled" true $cubeopscli) (or (eq (include "cube.opsEnabled" .) "true") .Values.externalControlPlane.enabled) -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "cube.mysqlName" -}}
{{- printf "%s-mysql" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.redisName" -}}
{{- printf "%s-redis" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.minioName" -}}
{{- printf "%s-minio" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.secretName" -}}
{{- printf "%s-secret" (include "cube.fullname" .) -}}
{{- end -}}

{{- define "cube.masterConfigSecretName" -}}
{{- printf "%s-master-config" (include "cube.fullname" .) -}}
{{- end -}}

{{/*
cube.adminToken resolves the shared CubeProxy admin token. CubeProxy reads it at
runtime as CUBE_PROXY_ADMIN_TOKEN and the lifecycle manager as CUBE_LCM_ADMIN_TOKEN
from the release Secret key cube-admin-token; CubeMaster needs the same value
rendered into its conf.yaml as cube_proxy_conf.admin_token (sent as
X-Cube-Admin-Token when invalidating a proxy routing cache on sandbox resume).
Priority: explicit lifecycleManager.adminToken → persisted cube-admin-token key
in the release Secret → freshly generated random.

Fresh-install note: Helm renders every manifest in a single pass, so the
randAlphaNum fallback below is evaluated separately per caller. On a brand-new
install the value rendered into the master config Secret can therefore differ
from the one persisted in the release Secret until the next `helm upgrade`
aligns both via lookup. Set lifecycleManager.adminToken explicitly (>=16 chars,
see validate.yaml) to avoid the double generation entirely.
*/}}
{{- define "cube.adminToken" -}}
{{- if .Values.lifecycleManager.adminToken -}}
{{- .Values.lifecycleManager.adminToken -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "cube.secretName" .) -}}
{{- if and $existing $existing.data (index $existing.data "cube-admin-token") -}}
{{- index $existing.data "cube-admin-token" | b64dec -}}
{{- else -}}
{{- randAlphaNum 32 -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "cube.masterStoragePVCName" -}}
{{- if .Values.controlPlane.master.persistence.existingClaim -}}
{{- .Values.controlPlane.master.persistence.existingClaim -}}
{{- else -}}
{{- printf "%s-master-storage" (include "cube.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/* Join a string or list into a comma-separated CubeOps warehouse env value. */}}
{{- define "cube.csvOrString" -}}
{{- if kindIs "string" . -}}
{{- . -}}
{{- else -}}
{{- join "," . -}}
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

{{- define "cube.minioPVCName" -}}
{{- if .Values.minio.persistence.existingClaim -}}
{{- .Values.minio.persistence.existingClaim -}}
{{- else -}}
{{- printf "%s-minio-data" (include "cube.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
Resolve PVC storageClassName for a stateful component (master / mysql / redis / minio).

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

{{- define "cube.volumeCosSecretName" -}}
{{- if .Values.volumeCos.existingSecret -}}
{{- .Values.volumeCos.existingSecret -}}
{{- else if .Values.volumeCos.secretName -}}
{{- .Values.volumeCos.secretName -}}
{{- else -}}
{{- printf "%s-volume-cos" (include "cube.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "cube.volumeS3SecretName" -}}
{{- if ((.Values.volumeS3).existingSecret) -}}
{{- .Values.volumeS3.existingSecret -}}
{{- else if ((.Values.volumeS3).secretName) -}}
{{- .Values.volumeS3.secretName -}}
{{- else -}}
{{- printf "%s-volume-s3" (include "cube.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/* Chart MinIO is explicit: minio.enabled. Only deploys MinIO. */}}
{{- define "cube.minioBuiltinEnabled" -}}
{{- $minio := default dict .Values.minio -}}
{{- if and .Values.controlPlane.enabled (dig "enabled" true $minio) -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{/* Operator-supplied S3 plugin config (not the MinIO bootstrap fill). */}}
{{- define "cube.volumeS3UserProvided" -}}
{{- $volumeS3 := default dict .Values.volumeS3 -}}
{{- if or (ne (($volumeS3.endpoint) | default "") "") (ne (($volumeS3.existingSecret) | default "") "") -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "cube.volumeS3ExternalEnabled" -}}
{{- include "cube.volumeS3UserProvided" . -}}
{{- end -}}

{{- define "cube.volumeS3Enabled" -}}
{{- if or (eq (include "cube.minioBuiltinEnabled" .) "true") (eq (include "cube.volumeS3UserProvided" .) "true") -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "cube.minioEndpoint" -}}
{{- printf "http://%s.%s.svc.%s:%v" (include "cube.minioName" .) .Release.Namespace (include "cube.clusterDomain" .) (.Values.minio.port | default 9000) -}}
{{- end -}}

{{/*
Render a bool env value that defaults to true. Helm's `default` treats
false as empty, so callers must not use `| default true` for these knobs.
*/}}
{{- define "cube.boolEnvDefaultTrue" -}}
{{- if kindIs "bool" . -}}
{{- ternary "true" "false" . -}}
{{- else -}}
true
{{- end -}}
{{- end -}}

{{/*
cubeOps.s3.endpoint > volumeS3.endpoint > chart MinIO.
*/}}
{{- define "cube.opsS3Endpoint" -}}
{{- $s3 := default dict .Values.cubeOps.s3 -}}
{{- if ne (($s3.endpoint) | default "") "" -}}
{{- $s3.endpoint -}}
{{- else if ne (((.Values.volumeS3).endpoint) | default "") "" -}}
{{- .Values.volumeS3.endpoint -}}
{{- else if eq (include "cube.minioBuiltinEnabled" .) "true" -}}
{{- include "cube.minioEndpoint" . -}}
{{- end -}}
{{- end -}}

{{- define "cube.opsS3NodeEndpoint" -}}
{{- $s3 := default dict .Values.cubeOps.s3 -}}
{{- if ne (($s3.nodeEndpoint) | default "") "" -}}
{{- $s3.nodeEndpoint -}}
{{- else -}}
{{- include "cube.opsS3Endpoint" . -}}
{{- end -}}
{{- end -}}

{{- define "cube.opsS3SecretName" -}}
{{- $s3 := default dict .Values.cubeOps.s3 -}}
{{- if ne (($s3.existingSecret) | default "") "" -}}
{{- $s3.existingSecret -}}
{{- else -}}
{{- include "cube.secretName" . -}}
{{- end -}}
{{- end -}}

{{- define "cube.opsS3Bucket" -}}
{{- $s3 := default dict .Values.cubeOps.s3 -}}
{{- $s3.bucket | default "cube-ops" -}}
{{- end -}}

{{- define "cube.opsS3Region" -}}
{{- $s3 := default dict .Values.cubeOps.s3 -}}
{{- if ne (($s3.region) | default "") "" -}}
{{- $s3.region -}}
{{- else if ne (((.Values.volumeS3).region) | default "") "" -}}
{{- .Values.volumeS3.region -}}
{{- else -}}
us-east-1
{{- end -}}
{{- end -}}

{{/*
Effective S3 plugin endpoint. volumeS3.* is the source of truth; when MinIO is
enabled and the operator left volumeS3.endpoint empty, fill from chart MinIO
(same as one-click filling CUBE_S3_* after deploying MinIO).
*/}}
{{- define "cube.volumeS3EffectiveEndpoint" -}}
{{- $volumeS3 := default dict .Values.volumeS3 -}}
{{- if ne (($volumeS3.endpoint) | default "") "" -}}
{{- $volumeS3.endpoint -}}
{{- else if eq (include "cube.minioBuiltinEnabled" .) "true" -}}
{{- include "cube.minioEndpoint" . -}}
{{- end -}}
{{- end -}}

{{/* KEY='value' with ' escaped as '"'"' so bash source of volume-s3.conf is safe. */}}
{{- define "cube.volumeS3ConfAssign" -}}
{{- $sq := "'\"'\"'" -}}
{{ .key }}='{{ .value | toString | replace "'" $sq }}'
{{- end -}}

{{- define "cube.volumeS3ConfBody" -}}
{{- include "cube.volumeS3ConfAssign" (dict "key" "ACCESS_KEY_ID" "value" .accessKeyId) }}
{{ include "cube.volumeS3ConfAssign" (dict "key" "SECRET_ACCESS_KEY" "value" .secretAccessKey) }}
{{ include "cube.volumeS3ConfAssign" (dict "key" "BUCKET" "value" .bucket) }}
{{ include "cube.volumeS3ConfAssign" (dict "key" "ENDPOINT" "value" .endpoint) }}
{{ include "cube.volumeS3ConfAssign" (dict "key" "REGION" "value" .region) }}
{{- if .extraOpts }}
{{ include "cube.volumeS3ConfAssign" (dict "key" "S3FS_EXTRA_OPTS" "value" .extraOpts) }}
{{- end }}
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

{{- define "cube.cubeopscliOpsAddress" -}}
{{- $endpoint := include "cube.opsEndpoint" . -}}
{{- $withoutHTTP := trimPrefix "http://" (trimPrefix "https://" $endpoint) -}}
{{- $hostPort := first (splitList "/" $withoutHTTP) -}}
{{- regexReplaceAll ":[0-9]+$" $hostPort "" -}}
{{- end -}}

{{- define "cube.cubeopscliOpsPort" -}}
{{- $endpoint := include "cube.opsEndpoint" . -}}
{{- $withoutHTTP := trimPrefix "http://" (trimPrefix "https://" $endpoint) -}}
{{- $hostPort := first (splitList "/" $withoutHTTP) -}}
{{- $port := regexFind "[0-9]+$" $hostPort -}}
{{- default "3010" $port -}}
{{- end -}}

{{- define "cube.apiEndpoint" -}}
{{- if .Values.externalControlPlane.enabled -}}
{{- .Values.externalControlPlane.apiEndpoint -}}
{{- else -}}
{{- printf "http://%s.%s.svc.%s:%v" (include "cube.apiName" .) .Release.Namespace (include "cube.clusterDomain" .) .Values.controlPlane.api.service.port -}}
{{- end -}}
{{- end -}}

{{- define "cube.opsEndpoint" -}}
{{- if .Values.externalControlPlane.enabled -}}
{{- .Values.externalControlPlane.opsEndpoint -}}
{{- else -}}
{{- printf "%s.%s.svc.%s:%v" (include "cube.opsName" .) .Release.Namespace (include "cube.clusterDomain" .) .Values.cubeOps.service.port -}}
{{- end -}}
{{- end -}}

{{/*
database.driver selects which connection section the control plane reads:
  mysql    -> mysql.*    (built-in StatefulSet when mysql.host is empty)
  postgres -> postgres.* (external only; the chart never deploys PostgreSQL)
Each engine keeps its own host/port/database/user/password, so no key is
shared between engines and no value is inferred across sections.
*/}}
{{- define "cube.dbDriver" -}}
{{- $driver := ((.Values.database).driver) | default "mysql" -}}
{{- if not (has $driver (list "mysql" "postgres")) -}}
{{- fail (printf "database.driver must be mysql or postgres (got %q)" $driver) -}}
{{- end -}}
{{- $driver -}}
{{- end -}}

{{- define "cube.dbHostRaw" -}}
{{- if eq (include "cube.dbDriver" .) "postgres" -}}
{{- ((.Values.postgres).host) | default "" -}}
{{- else -}}
{{- ((.Values.mysql).host) | default "" -}}
{{- end -}}
{{- end -}}

{{- define "cube.dbHost" -}}
{{- $host := include "cube.dbHostRaw" . -}}
{{- if $host -}}{{ $host }}{{- else -}}{{ include "cube.mysqlName" . }}.{{ .Release.Namespace }}.svc.{{ include "cube.clusterDomain" . }}{{- end -}}
{{- end -}}

{{/* Deprecated alias of cube.dbHost (kept so older includes keep working). */}}
{{- define "cube.mysqlHost" -}}
{{- include "cube.dbHost" . -}}
{{- end -}}

{{- define "cube.dbPort" -}}
{{- if eq (include "cube.dbDriver" .) "postgres" -}}
{{- ((.Values.postgres).port) | default 5432 -}}
{{- else -}}
{{- ((.Values.mysql).port) | default 3306 -}}
{{- end -}}
{{- end -}}

{{- define "cube.dbUser" -}}
{{- if eq (include "cube.dbDriver" .) "postgres" -}}
{{- ((.Values.postgres).user) | default "cube" -}}
{{- else -}}
{{- ((.Values.mysql).user) | default "cube" -}}
{{- end -}}
{{- end -}}

{{- define "cube.dbName" -}}
{{- if eq (include "cube.dbDriver" .) "postgres" -}}
{{- ((.Values.postgres).database) | default "cube_mvp" -}}
{{- else -}}
{{- ((.Values.mysql).database) | default "cube_mvp" -}}
{{- end -}}
{{- end -}}

{{- define "cube.dbPassword" -}}
{{- if eq (include "cube.dbDriver" .) "postgres" -}}
{{- ((.Values.postgres).password) | default "" -}}
{{- else -}}
{{- ((.Values.mysql).password) | default "" -}}
{{- end -}}
{{- end -}}

{{/* Root password only exists for the chart-managed MySQL StatefulSet. */}}
{{- define "cube.dbRootPassword" -}}
{{- ((.Values.mysql).rootPassword) | default "" -}}
{{- end -}}

{{/*
CubeOps / CubeAPI DATABASE_URL. postgres must use a URL scheme;
CUBE_SANDBOX_MYSQL_* alone is always assembled as mysql://.
*/}}
{{- define "cube.databaseURL" -}}
{{- $driver := include "cube.dbDriver" . -}}
{{- $user := include "cube.dbUser" . | urlquery -}}
{{- $pass := include "cube.dbPassword" . | urlquery -}}
{{- $host := include "cube.dbHost" . -}}
{{- $port := include "cube.dbPort" . -}}
{{- $name := include "cube.dbName" . | urlquery -}}
{{- if eq $driver "postgres" -}}
{{- printf "postgresql://%s:%s@%s:%v/%s" $user $pass $host $port $name -}}
{{- else -}}
{{- printf "mysql://%s:%s@%s:%v/%s" $user $pass $host $port $name -}}
{{- end -}}
{{- end -}}

{{- define "cube.mysqlBuiltinEnabled" -}}
{{- /* Built-in StatefulSet is MySQL only; postgres is always external. */ -}}
{{- if and .Values.controlPlane.enabled .Values.mysql.enabled (eq (((.Values.mysql).host) | default "") "") (ne (include "cube.dbDriver" .) "postgres") -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "cube.redisHost" -}}
{{- if .Values.redis.host -}}{{ .Values.redis.host }}{{- else -}}{{ include "cube.redisName" . }}.{{ .Release.Namespace }}.svc.{{ include "cube.clusterDomain" . }}{{- end -}}
{{- end -}}

{{- /* Non-empty redis.masterName selects external Redis Sentinel (skips chart redis). */ -}}
{{- define "cube.redisSentinelEnabled" -}}
{{- if ne (((.Values.redis).masterName) | default "") "" -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- /* Count non-empty comma-separated sentinel endpoints after trim. */ -}}
{{- define "cube.redisSentinelEndpointCount" -}}
{{- $n := 0 -}}
{{- range $p := splitList "," (((.Values.redis).sentinelNodes) | default "") -}}
{{- if ne ($p | trim) "" -}}{{- $n = add $n 1 -}}{{- end -}}
{{- end -}}
{{- $n -}}
{{- end -}}

{{- define "cube.redisBuiltinEnabled" -}}
{{- if and (or .Values.controlPlane.enabled (eq (include "cube.proxyEnabled" .) "true")) .Values.redis.enabled (not .Values.redis.host) (ne (include "cube.redisSentinelEnabled" .) "true") -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- /* Master conf nodes: empty under Sentinel; else host:port. */ -}}
{{- define "cube.redisNodes" -}}
{{- if eq (include "cube.redisSentinelEnabled" .) "true" -}}{{- else -}}{{ printf "%s:%v" (include "cube.redisHost" .) .Values.redis.port }}{{- end -}}
{{- end -}}

{{- define "cube.egressNetProbeCommand" -}}
set -e
iface="${CUBE_INGRESS_IFACE:-cube-dev}"
table="${CUBE_EGRESS_NET_ROUTE_TABLE:-100}"
chain="${CUBE_EGRESS_NET_CHAIN:-TRANSPROXY}"
ip link show "${iface}" >/dev/null
http_mark=0xce010000
https_mark=0xce020000
mark_mask=0xffff0000
ip rule show | grep -q "fwmark ${http_mark}/${mark_mask} lookup ${table}"
ip rule show | grep -q "fwmark ${https_mark}/${mark_mask} lookup ${table}"
ip route show table "${table}" | grep -Eq "local (default|0\\.0\\.0\\.0/0) dev lo"
iptables -t mangle -S "${chain}" | grep -q "${http_mark}"
iptables -t mangle -S "${chain}" | grep -q "${https_mark}"
{{- end -}}

{{- define "cube.secretEnabled" -}}
{{- if or (and .Values.controlPlane.enabled (or .Values.controlPlane.master.enabled .Values.controlPlane.api.enabled (eq (include "cube.opsEnabled" .) "true"))) (eq (include "cube.proxyEnabled" .) "true") (eq (include "cube.mysqlBuiltinEnabled" .) "true") (eq (include "cube.redisBuiltinEnabled" .) "true") (eq (include "cube.minioBuiltinEnabled" .) "true") -}}true{{- else -}}false{{- end -}}
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
cube-node / installer / bootstrap use native apps/v1 DaemonSet.
*/}}
{{- define "cube.nodeDaemonSetAPIVersion" -}}
apps/v1
{{- end -}}

{{/*
cube-node-pvm uses a native apps/v1 DaemonSet.
*/}}
{{- define "cube.nodePvmDaemonSetAPIVersion" -}}
apps/v1
{{- end -}}

{{/*
Render a Deployment strategy block.

Call with the root context to use controlPlane.deploymentStrategy, or with
(dict "root" $ "strategy" .Values.controlPlane.master.deploymentStrategy) for a
component override. type Recreate omits rollingUpdate (required for single-
replica RWO PVC workloads such as cube-master).
*/}}
{{- define "cube.deploymentStrategy" -}}
{{- $strategy := dict -}}
{{- if hasKey . "Values" -}}
{{- $strategy = .Values.controlPlane.deploymentStrategy -}}
{{- else -}}
{{- $strategy = .strategy | default .root.Values.controlPlane.deploymentStrategy -}}
{{- end -}}
strategy:
  type: {{ required "deploymentStrategy.type is required" $strategy.type }}
{{- if ne ($strategy.type | toString) "Recreate" }}
  rollingUpdate:
    maxUnavailable: {{ $strategy.rollingUpdate.maxUnavailable }}
    maxSurge: {{ $strategy.rollingUpdate.maxSurge }}
{{- end }}
{{- end -}}

{{- define "cube.startupGateEnabled" -}}
{{- if and .Values.cubeNode.enabled .Values.bootstrap.pvmHostKernel.enabled .Values.bootstrap.pvmHostKernel.startupGate.enabled -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "cube.pvmPriorityClassName" -}}
{{- $base := include "cube.fullname" . | trunc 42 | trimSuffix "-" -}}
{{- default (printf "cube-pvm-%s-%s" $base (include "cube.releaseIdentityHash" .)) .Values.bootstrap.pvmHostKernel.startupGate.priorityClassName -}}
{{- end -}}

{{/*
Kubernetes API path prefix for the cube-node DaemonSet (health-test).
*/}}
{{- define "cube.nodeDaemonSetAPIPath" -}}
/apis/apps/v1/namespaces
{{- end -}}

{{/*
Big Pod: shared volumeMounts for component install/run containers.
Toolbox is mounted whole at the fixed path.
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
- name: data-cube-shared
  mountPath: {{ .Values.hostPaths.dataCubeShared }}
  mountPropagation: Bidirectional
- name: data-shared
  mountPath: {{ .Values.hostPaths.dataShared }}
  mountPropagation: Bidirectional
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
Privileged securityContext shared by cubelet / placeholder slots.
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
- name: data-cubelet
  mountPath: {{ .Values.hostPaths.dataCubelet }}
{{- end -}}

{{- define "cube.installerComponentEnv" -}}
{{- include "cube.timezoneEnv" . }}
- name: TOOLBOX_ROOT
  value: /usr/local/services/cubetoolbox
- name: IMAGE_ROOT
  value: /opt/cube-image
- name: STATE_DIR
  value: {{ .Values.hostPaths.bootstrapState | quote }}
- name: COMPONENT_VERSIONS_ROOT
  value: {{ printf "%s/root/component_versions" .Values.hostPaths.dataCubelet | quote }}
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
- name: data-cube-shared
  mountPath: {{ .Values.hostPaths.dataCubeShared }}
  mountPropagation: Bidirectional
- name: data-shared
  mountPath: {{ .Values.hostPaths.dataShared }}
  mountPropagation: Bidirectional
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
- name: data-cube-shared
  hostPath:
    path: {{ .Values.hostPaths.dataCubeShared }}
    type: DirectoryOrCreate
- name: data-shared
  hostPath:
    path: {{ .Values.hostPaths.dataShared }}
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
- name: CUBE_OPS_ENDPOINT
  value: {{ include "cube.opsEndpoint" . | quote }}
- name: CUBE_MASTER_HTTP_ADDR
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
- name: CUBE_EGRESS_ADMIN_PORT
  value: {{ .Values.cubeEgress.adminPort | quote }}
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

{{- define "cube.s3lvolEnabled" -}}
{{- if ((.Values.cubeS3lvol).enabled) -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "cube.s3lvolSocketPath" -}}
{{- ((.Values.cubeS3lvol).socketPath) | default "/var/run/s3lvol/s3lvol.sock" -}}
{{- end -}}

{{- define "cube.s3lvolSecretName" -}}
{{- $s3 := default dict ((.Values.cubeS3lvol).s3) -}}
{{- if ne (($s3.existingSecret) | default "") "" -}}
{{- $s3.existingSecret -}}
{{- else if ne (($s3.secretName) | default "") "" -}}
{{- $s3.secretName -}}
{{- else -}}
{{- printf "%s-s3lvol" (include "cube.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/* True when the chart must generate the Secret from chart MinIO root credentials. */}}
{{- define "cube.s3lvolFillsFromMinio" -}}
{{- $s3 := default dict ((.Values.cubeS3lvol).s3) -}}
{{- $userCreds := and (ne (($s3.accessKeyId) | default "") "") (ne (($s3.secretAccessKey) | default "") "") -}}
{{- if and (eq (include "cube.s3lvolEnabled" .) "true") (eq (($s3.existingSecret) | default "") "") (not $userCreds) (eq (include "cube.minioBuiltinEnabled" .) "true") -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{- define "cube.s3lvolEffectiveEndpoint" -}}
{{- $s3 := default dict ((.Values.cubeS3lvol).s3) -}}
{{- if ne (($s3.endpoint) | default "") "" -}}
{{- $s3.endpoint -}}
{{- else if ne (include "cube.volumeS3EffectiveEndpoint" .) "" -}}
{{- include "cube.volumeS3EffectiveEndpoint" . -}}
{{- end -}}
{{- end -}}

{{- define "cube.s3lvolEffectiveBucket" -}}
{{- $s3 := default dict ((.Values.cubeS3lvol).s3) -}}
{{- $s3.bucket | default "cube-s3lvol" -}}
{{- end -}}

{{- define "cube.s3lvolEffectiveRegion" -}}
{{- $s3 := default dict ((.Values.cubeS3lvol).s3) -}}
{{- if ne (($s3.region) | default "") "" -}}
{{- $s3.region -}}
{{- else if ne (((.Values.volumeS3).region) | default "") "" -}}
{{- .Values.volumeS3.region -}}
{{- else -}}
us-east-1
{{- end -}}
{{- end -}}

{{- define "cube.s3lvolPathStyle" -}}
{{- $s3 := default dict ((.Values.cubeS3lvol).s3) -}}
{{- if hasKey $s3 "pathStyle" -}}
{{- if $s3.pathStyle -}}true{{- else -}}false{{- end -}}
{{- else if eq (include "cube.s3lvolFillsFromMinio" .) "true" -}}
true
{{- else -}}
false
{{- end -}}
{{- end -}}

{{/* s3.cfg body. Caller passes dict: accessKeyId, secretAccessKey, endpoint, region, bucket, pathStyle. */}}
{{- define "cube.s3lvolCfgBody" -}}
{{- $e := .endpoint | toString -}}
{{- $host := first (splitList "/" (trimPrefix "https://" (trimPrefix "http://" $e))) -}}
access_key_id="{{ .accessKeyId }}"
secret_access_key="{{ .secretAccessKey }}"
endpoint="{{ $host }}"
region="{{ .region }}"
buckets=["{{ .bucket }}"]
{{- if .pathStyle }}
path_style="true"
{{- end }}
{{- if hasPrefix "http://" $e }}
no_tls="true"
{{- end }}
{{- end -}}

{{- define "cube.s3lvolHasResolvedCreds" -}}
{{- $s3 := default dict ((.Values.cubeS3lvol).s3) -}}
{{- if ne (($s3.existingSecret) | default "") "" -}}
true
{{- else if and (ne (($s3.accessKeyId) | default "") "") (ne (($s3.secretAccessKey) | default "") "") -}}
true
{{- else if and (ne (((.Values.volumeS3).accessKeyId) | default "") "") (ne (((.Values.volumeS3).secretAccessKey) | default "") "") -}}
true
{{- else if eq (include "cube.minioBuiltinEnabled" .) "true" -}}
true
{{- else -}}
false
{{- end -}}
{{- end -}}

{{- define "cube.s3lvolProbeCommand" -}}
sock={{ include "cube.s3lvolSocketPath" . }}; test -S "${sock}" && python3 /opt/s3lvol/scripts/s3lvol_rpc.py --sock "${sock}" --timeout 5 rcow_get_lvstores >/dev/null
{{- end -}}

{{/* Big Pod grace = max(cubeNode, cubeS3lvol) terminationGracePeriodSeconds. */}}
{{- define "cube.nodeTerminationGracePeriodSeconds" -}}
{{- $grace := int (.Values.cubeNode.terminationGracePeriodSeconds | default 30) -}}
{{- if eq (include "cube.s3lvolEnabled" .) "true" -}}
{{- $s3lvolGrace := int (((.Values.cubeS3lvol).terminationGracePeriodSeconds) | default 180) -}}
{{- if gt $s3lvolGrace $grace -}}
{{- $s3lvolGrace -}}
{{- else -}}
{{- $grace -}}
{{- end -}}
{{- else -}}
{{- $grace -}}
{{- end -}}
{{- end -}}
