{{/*
Expand the name of the chart.
*/}}
{{- define "mosfs-csi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "mosfs-csi.fullname" -}}
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
Create chart name and version as used by the chart label.
*/}}
{{- define "mosfs-csi.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "mosfs-csi.labels" -}}
helm.sh/chart: {{ include "mosfs-csi.chart" . }}
{{ include "mosfs-csi.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "mosfs-csi.selectorLabels" -}}
app.kubernetes.io/name: {{ include "mosfs-csi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "mosfs-csi.controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.controller.create }}
{{- default (include "mosfs-csi.fullname" .) .Values.serviceAccount.controller.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.controller.name }}
{{- end }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "mosfs-csi.snapshotController.serviceAccountName" -}}
{{- if .Values.serviceAccount.snapshotController.create }}
{{- default (include "mosfs-csi.fullname" .) .Values.serviceAccount.snapshotController.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.snapshotController.name }}
{{- end }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "mosfs-csi.node.serviceAccountName" -}}
{{- if .Values.serviceAccount.node.create }}
{{- default (include "mosfs-csi.fullname" .) .Values.serviceAccount.node.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.node.name }}
{{- end }}
{{- end }}

{{/*
secret fullname
*/}}
{{- define "mosfs-csi.secretFullname" -}}
{{ include "mosfs-csi.fullname" . }}-secret
{{- end }}
