{{/* Chart name, overridable. */}}
{{- define "ubixvault.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified app name. */}}
{{- define "ubixvault.fullname" -}}
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

{{- define "ubixvault.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "ubixvault.labels" -}}
helm.sh/chart: {{ include "ubixvault.chart" . }}
{{ include "ubixvault.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "ubixvault.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ubixvault.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* ServiceAccount name to use. */}}
{{- define "ubixvault.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "ubixvault.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* Image reference; tag defaults to the chart appVersion. */}}
{{- define "ubixvault.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/* In-cluster URL the backup job uses to reach the vault. */}}
{{- define "ubixvault.backupAddress" -}}
{{- $scheme := ternary "https" "http" .Values.tls.enabled -}}
{{- printf "%s://%s:%v" $scheme (include "ubixvault.fullname" .) .Values.service.port -}}
{{- end -}}

{{/* Name of the Secret holding the auto-unseal KEK (existing or chart-created). */}}
{{- define "ubixvault.autoUnsealSecretName" -}}
{{- if .Values.autoUnseal.existingSecret -}}
{{- .Values.autoUnseal.existingSecret -}}
{{- else -}}
{{- printf "%s-auto-unseal" (include "ubixvault.fullname" .) -}}
{{- end -}}
{{- end -}}
