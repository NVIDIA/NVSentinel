{{/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/}}

{{/*
Expand the name of the chart.
*/}}
{{- define "preflight.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "preflight.fullname" -}}
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
{{- define "preflight.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "preflight.labels" -}}
helm.sh/chart: {{ include "preflight.chart" . }}
{{ include "preflight.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "preflight.selectorLabels" -}}
app.kubernetes.io/name: {{ include "preflight.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "preflight.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "preflight.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Webhook name for MutatingWebhookConfiguration
*/}}
{{- define "preflight.webhookName" -}}
{{ include "preflight.name" . }}.nvsentinel.nvidia.com
{{- end }}

{{/*
Certificate secret name
*/}}
{{- define "preflight.certSecretName" -}}
{{ include "preflight.fullname" . }}-webhook-tls
{{- end }}

{{/*
Certificate DNS names
*/}}
{{- define "preflight.certDnsNames" -}}
- {{ include "preflight.fullname" . }}
- {{ include "preflight.fullname" . }}.{{ .Release.Namespace }}
- {{ include "preflight.fullname" . }}.{{ .Release.Namespace }}.svc
- {{ include "preflight.fullname" . }}.{{ .Release.Namespace }}.svc.cluster.local
{{- end }}

{{/*
DCGM service endpoint - uses global.dcgm.service.endpoint with fallback to local
*/}}
{{- define "preflight.dcgmEndpoint" -}}
{{- if and .Values.global .Values.global.dcgm .Values.global.dcgm.service }}
{{- .Values.global.dcgm.service.endpoint | default .Values.dcgm.service.endpoint }}
{{- else }}
{{- .Values.dcgm.service.endpoint }}
{{- end }}
{{- end }}

{{/*
DCGM service port - uses global.dcgm.service.port with fallback to local
*/}}
{{- define "preflight.dcgmPort" -}}
{{- if and .Values.global .Values.global.dcgm .Values.global.dcgm.service }}
{{- .Values.global.dcgm.service.port | default .Values.dcgm.service.port }}
{{- else }}
{{- .Values.dcgm.service.port }}
{{- end }}
{{- end }}

{{/*
DCGM hostengine address - combines endpoint and port
*/}}
{{- define "preflight.dcgmHostengineAddr" -}}
{{- printf "%s:%v" (include "preflight.dcgmEndpoint" .) (include "preflight.dcgmPort" .) }}
{{- end }}

{{/*
DCGM diagnostic level
*/}}
{{- define "preflight.dcgmDiagLevel" -}}
{{- .Values.dcgm.diagLevel | default 1 }}
{{- end }}

{{/*
Event processing strategy
*/}}
{{- define "preflight.processingStrategy" -}}
{{- .Values.dcgm.processingStrategy | default "EXECUTE_REMEDIATION" }}
{{- end }}

{{/*
Platform connector socket path for health event reporting
Uses global.socketPath with unix:// prefix
*/}}
{{- define "preflight.connectorSocket" -}}
{{- if and .Values.global .Values.global.socketPath }}
{{- printf "unix://%s" .Values.global.socketPath }}
{{- else }}
{{- "unix:///var/run/nvsentinel.sock" }}
{{- end }}
{{- end }}

