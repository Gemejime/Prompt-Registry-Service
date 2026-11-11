{{/*
Expand the name of the chart.
*/}}
{{- define "rag-prompt-registry.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "rag-prompt-registry.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart label value.
*/}}
{{- define "rag-prompt-registry.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "rag-prompt-registry.labels" -}}
helm.sh/chart: {{ include "rag-prompt-registry.chart" . }}
{{ include "rag-prompt-registry.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "rag-prompt-registry.selectorLabels" -}}
app.kubernetes.io/name: {{ include "rag-prompt-registry.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Secret name helper.
*/}}
{{- define "rag-prompt-registry.secretName" -}}
{{- $envSecret := "" -}}
{{- if .Values.env -}}
{{- $envSecret = get .Values.env "secretName" | default "" -}}
{{- end -}}
{{- if .Values.secrets.existingSecret }}
{{- .Values.secrets.existingSecret }}
{{- else if $envSecret }}
{{- $envSecret }}
{{- else }}
{{- printf "%s-secret" (include "rag-prompt-registry.fullname" .) }}
{{- end }}
{{- end }}

