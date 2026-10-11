{{- define "siliconrouter.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "siliconrouter.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "siliconrouter.selector" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: router
{{- end -}}

{{- define "siliconrouter.scheme" -}}
{{- if .Values.tls.secretName }}https{{ else }}http{{ end -}}
{{- end -}}

{{/* The config file: the user's config plus listen, TLS and cluster settings. */}}
{{- define "siliconrouter.config" -}}
{{- $cfg := deepCopy .Values.config -}}
{{- $_ := set $cfg "listen" (printf ":%d" (int .Values.service.port)) -}}
{{- if .Values.tls.secretName -}}
{{- $_ := set $cfg "tls" (dict "cert_file" "/etc/siliconrouter/tls/tls.crt" "key_file" "/etc/siliconrouter/tls/tls.key") -}}
{{- end -}}
{{- if gt (int .Values.replicaCount) 1 -}}
{{- $full := include "siliconrouter.fullname" . -}}
{{- $peers := list -}}
{{- range $i := until (int .Values.replicaCount) -}}
{{- $peers = append $peers (printf "%s://%s-%d.%s-headless.%s.svc:%d" (include "siliconrouter.scheme" $) $full $i $full $.Release.Namespace (int $.Values.service.port)) -}}
{{- end -}}
{{- $_ := set $cfg "cluster" (dict "peers" $peers "secret_env" .Values.clusterSecretKey) -}}
{{- end -}}
{{- toYaml $cfg -}}
{{- end -}}
