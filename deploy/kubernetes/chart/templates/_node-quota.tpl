{{/* Only supplied quota fields become env vars; preserve explicit zero/empty values. */}}
{{- define "cube.nodeQuotaEnv" -}}
{{- $quota := .Values.cubeNode.quota -}}
{{/* Old releases upgraded with --reuse-values may not have the new key. */}}
{{- if kindIs "invalid" $quota -}}
{{- $quota = dict -}}
{{- end -}}
{{- if not (kindIs "map" $quota) -}}
{{- fail "cubeNode.quota must be a map" -}}
{{- end -}}
{{- $keys := list "mcpu_limit" "mem_limit" "mvm_limit" "creation_concurrent_num" "paused_resource_release_ratio" -}}
{{- range $key, $value := $quota -}}
{{- if not (has $key $keys) -}}
{{- fail (printf "unknown cubeNode.quota field: %s" $key) -}}
{{- end -}}
{{/* A null field keeps the image default, just like an omitted field. */}}
{{- if not (kindIs "invalid" $value) -}}
{{- $text := toString $value -}}
{{- if eq $key "mem_limit" -}}
{{- if not (regexMatch "^$|^[+]?(?:[0-9]+(?:[.][0-9]*)?|[.][0-9]+)(?:[eE][+-]?[0-9]+|[numkMGTP]|[KMGTPE]i|E)?$" $text) -}}
{{- fail "cubeNode.quota.mem_limit must be a non-negative Kubernetes memory quantity or empty" -}}
{{- end -}}
{{- else if eq $key "paused_resource_release_ratio" -}}
{{- if or (not (regexMatch "^[+]?(?:[0-9]+(?:[.][0-9]*)?|[.][0-9]+)(?:[eE][+-]?[0-9]+)?$" $text)) (lt (float64 $value) 0.0) (gt (float64 $value) 1.0) -}}
{{- fail "cubeNode.quota.paused_resource_release_ratio must be a number between 0 and 1" -}}
{{- end -}}
{{- else -}}
{{/* Values files may decode integers as floats; format without exponent notation. */}}
{{- if kindIs "float64" $value -}}
{{- if ne $value (floor $value) -}}
{{- fail (printf "cubeNode.quota.%s must be a non-negative integer" $key) -}}
{{- end -}}
{{- $text = printf "%.0f" $value -}}
{{- end -}}
{{- if not (regexMatch "^(0|[1-9][0-9]*)$" $text) -}}
{{- fail (printf "cubeNode.quota.%s must be a non-negative integer" $key) -}}
{{- end -}}
{{- end }}
- name: CUBELET_QUOTA_{{ $key | upper }}
  value: {{ $text | quote }}
{{- end -}}
{{- end -}}
{{- end -}}
