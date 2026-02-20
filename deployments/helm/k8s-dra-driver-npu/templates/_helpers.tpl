{{/*
Expand the name of the chart.
*/}}
{{- define "k8s-dra-driver-npu.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "k8s-dra-driver-npu.fullname" -}}
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
Allow the release namespace to be overridden for multi-namespace deployments in combined charts
*/}}
{{- define "k8s-dra-driver-npu.namespace" -}}
  {{- if .Values.namespaceOverride -}}
    {{- .Values.namespaceOverride -}}
  {{- else -}}
    {{- .Release.Namespace -}}
  {{- end -}}
{{- end -}}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "k8s-dra-driver-npu.chart" -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- printf "%s-%s" $name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "k8s-dra-driver-npu.labels" -}}
helm.sh/chart: {{ include "k8s-dra-driver-npu.chart" . }}
{{ include "k8s-dra-driver-npu.templateLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Template labels
*/}}
{{- define "k8s-dra-driver-npu.templateLabels" -}}
app.kubernetes.io/name: {{ include "k8s-dra-driver-npu.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- if .Values.selectorLabelsOverride }}
{{ toYaml .Values.selectorLabelsOverride }}
{{- end }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "k8s-dra-driver-npu.selectorLabels" -}}
{{- if .Values.selectorLabelsOverride -}}
{{ toYaml .Values.selectorLabelsOverride }}
{{- else -}}
{{ include "k8s-dra-driver-npu.templateLabels" . }}
{{- end }}
{{- end }}

{{/*
Full image name with tag
*/}}
{{- define "k8s-dra-driver-npu.fullimage" -}}
{{- .Values.image.repository -}}:{{- .Values.image.tag | default .Chart.AppVersion -}}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "k8s-dra-driver-npu.serviceAccountName" -}}
{{- $name := printf "%s-service-account" (include "k8s-dra-driver-npu.fullname" .) }}
{{- if .Values.serviceAccount.create }}
{{- default $name .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Create the name of the service account to use for the webhook
*/}}
{{- define "k8s-dra-driver-npu.webhookServiceAccountName" -}}
{{- $name := printf "%s-webhook-service-account" (include "k8s-dra-driver-npu.fullname" .) }}
{{- if .Values.webhook.serviceAccount.create }}
{{- default $name .Values.webhook.serviceAccount.name }}
{{- else }}
{{- default "default-webhook" .Values.webhook.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Get the latest available resource.k8s.io API version
Returns the highest available version or empty string if none found
*/}}
{{- define "k8s-dra-driver-npu.resourceApiVersion" -}}
{{- if .Capabilities.APIVersions.Has "resource.k8s.io/v1" -}}
resource.k8s.io/v1
{{- else if .Capabilities.APIVersions.Has "resource.k8s.io/v1beta2" -}}
resource.k8s.io/v1beta2
{{- else if .Capabilities.APIVersions.Has "resource.k8s.io/v1beta1" -}}
resource.k8s.io/v1beta1
{{- else -}}
{{- end -}}
{{- end -}}

{{/*
The driver name.
*/}}
{{- define "k8s-dra-driver-npu.driverName" -}}
npu.rebellions.ai
{{- end -}}
