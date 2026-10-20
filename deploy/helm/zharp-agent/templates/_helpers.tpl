{{- define "zharp-agent.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "zharp-agent.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else if contains (include "zharp-agent.name" .) .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name (include "zharp-agent.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "zharp-agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "zharp-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "zharp-agent.labels" -}}
{{ include "zharp-agent.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end }}

{{- define "zharp-agent.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "zharp-agent.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "zharp-agent.secretName" -}}
{{- default (include "zharp-agent.fullname" .) .Values.existingSecret }}
{{- end }}

{{/*
A string for the collector config: single-quoted YAML, with "$" doubled so the collector's
${...} expansion leaves regexes and $1 replacements alone.
*/}}
{{- define "zharp-agent.configString" -}}
{{- printf "'%s'" (. | toString | replace "'" "''" | replace "$" "$$") }}
{{- end }}
