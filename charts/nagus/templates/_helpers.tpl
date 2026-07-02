{{/* Expand the name of the chart. */}}
{{- define "nagus.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified app name. */}}
{{- define "nagus.fullname" -}}
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

{{- define "nagus.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "nagus.labels" -}}
helm.sh/chart: {{ include "nagus.chart" . }}
{{ include "nagus.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: nagus
{{- end -}}

{{- define "nagus.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nagus.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "nagus.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "nagus.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* image ref: repository:tag (tag defaults to appVersion). */}}
{{- define "nagus.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/* The Secret name that carries eBay credentials (existing, external, or none). */}}
{{- define "nagus.ebaySecretName" -}}
{{- if .Values.ebay.existingSecret -}}
{{- .Values.ebay.existingSecret -}}
{{- else if .Values.externalSecret.enabled -}}
{{- default (printf "%s-ebay" (include "nagus.fullname" .)) .Values.externalSecret.targetName -}}
{{- end -}}
{{- end -}}

{{/* The local basic-auth Secret name carrying the postgres role credentials. */}}
{{- define "nagus.dbSecretName" -}}
{{- if .Values.storage.postgres.existingSecret -}}
{{- .Values.storage.postgres.existingSecret -}}
{{- else -}}
{{- printf "%s-db" (include "nagus.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/* Effective ingest interval: demo mode overrides serve.ingestInterval. */}}
{{- define "nagus.ingestInterval" -}}
{{- if .Values.demo.enabled -}}
{{- .Values.demo.ingestInterval -}}
{{- else -}}
{{- .Values.serve.ingestInterval -}}
{{- end -}}
{{- end -}}
