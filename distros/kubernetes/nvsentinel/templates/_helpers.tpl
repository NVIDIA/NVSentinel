{{/*
Expand the name of the chart.
*/}}
{{- define "nvsentinel.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "nvsentinel.fullname" -}}
{{- "platform-connectors" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "nvsentinel.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "nvsentinel.labels" -}}
helm.sh/chart: {{ include "nvsentinel.chart" . }}
{{ include "nvsentinel.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "nvsentinel.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nvsentinel.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "nvsentinel.serviceAccountName" -}}
{{- include "nvsentinel.fullname" . }}
{{- end }}

{{/*
Audit logging init container
*/}}
{{- define "nvsentinel.auditLogging.initContainer" -}}
- name: fix-audit-log-permissions
  image: "{{ .Values.global.initContainerImage.repository }}:{{ .Values.global.initContainerImage.tag }}"
  imagePullPolicy: {{ .Values.global.initContainerImage.pullPolicy }}
  securityContext:
    runAsUser: 0
  command:
    - sh
    - -c
    - |
      chown 65532:65532 /var/log/nvsentinel
      chmod 770 /var/log/nvsentinel
  volumeMounts:
    - name: audit-logs
      mountPath: /var/log/nvsentinel
{{- end }}

{{/*
Audit logging volume mount for container
*/}}
{{- define "nvsentinel.auditLogging.volumeMount" -}}
- name: audit-logs
  mountPath: /var/log/nvsentinel
{{- end }}

{{/*
Audit logging volume definition
*/}}
{{- define "nvsentinel.auditLogging.volume" -}}
- name: audit-logs
  hostPath:
    path: /var/log/nvsentinel
    type: DirectoryOrCreate
{{- end }}

{{/*
Audit logging environment variables
*/}}
{{- define "nvsentinel.auditLogging.envVars" -}}
- name: AUDIT_ENABLED
  value: "{{ .Values.global.auditLogging.enabled }}"
- name: AUDIT_LOG_REQUEST_BODY
  value: "{{ .Values.global.auditLogging.logRequestBody }}"
- name: AUDIT_LOG_MAX_SIZE_MB
  value: "{{ .Values.global.auditLogging.maxSizeMB }}"
- name: AUDIT_LOG_MAX_BACKUPS
  value: "{{ .Values.global.auditLogging.maxBackups }}"
- name: AUDIT_LOG_MAX_AGE_DAYS
  value: "{{ .Values.global.auditLogging.maxAgeDays }}"
- name: AUDIT_LOG_COMPRESS
  value: "{{ .Values.global.auditLogging.compress }}"
{{- end }}
