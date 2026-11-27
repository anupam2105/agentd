{{/*
Expand the name of the chart.
*/}}
{{- define "agentd.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully-qualified app name. Truncated to 63 chars to satisfy DNS-1123.
*/}}
{{- define "agentd.fullname" -}}
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

{{/*
Chart label — used by the chart itself.
*/}}
{{- define "agentd.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels shared by every resource.
*/}}
{{- define "agentd.labels" -}}
helm.sh/chart: {{ include "agentd.chart" . }}
{{ include "agentd.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
Selector labels — a narrow subset that never changes across upgrades.
*/}}
{{- define "agentd.selectorLabels" -}}
app.kubernetes.io/name: {{ include "agentd.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Name of the service account to use.
*/}}
{{- define "agentd.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "agentd.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Container image reference — tag defaults to Chart.AppVersion when empty.
*/}}
{{- define "agentd.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/*
Env var sourced from existingSecret (optional key).
  include "agentd.secretEnv" (list . "ENV_NAME" "secret-key")
*/}}
{{- define "agentd.secretEnv" -}}
{{- $root := index . 0 -}}
{{- if $root.Values.existingSecret }}
- name: {{ index . 1 }}
  valueFrom:
    secretKeyRef:
      name: {{ $root.Values.existingSecret }}
      key: {{ index . 2 }}
      optional: true
{{- end }}
{{- end -}}

{{- define "agentd.configMap" -}}
{{- .Values.existingConfigMap | default (printf "%s-config" (include "agentd.fullname" .)) -}}
{{- end -}}

{{/*
DOCKER_HOST for the selected sandbox mode.
*/}}
{{- define "agentd.dockerHost" -}}
{{- if eq .Values.sandbox.mode "dind" -}}
unix:///var/run/dind/docker.sock
{{- else if eq .Values.sandbox.mode "hostSocket" -}}
unix:///var/run/docker.sock
{{- else if eq .Values.sandbox.mode "external" -}}
{{- required "sandbox.external.dockerHost is required when sandbox.mode=external" .Values.sandbox.external.dockerHost -}}
{{- end -}}
{{- end -}}

{{- define "agentd.assertConfig" -}}
{{- if not (has .Values.sandbox.mode (list "dind" "hostSocket" "external" "none")) -}}
{{- fail "sandbox.mode must be one of: dind, hostSocket, external, none" -}}
{{- end -}}
{{- end -}}
