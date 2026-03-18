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

{{/*
MongoDB client certificate secret name.
Returns (in priority order):
  1. global.datastore.auth.clientCertSecretName  (x509 auth with user-provided cert)
  2. global.datastore.certificates.secretName     (legacy configurable name)
  3. mongo-app-client-cert-secret                 (default: cert-manager generated)
*/}}
{{- define "nvsentinel.certificates.secretName" -}}
{{- if and .Values.global.datastore .Values.global.datastore.auth .Values.global.datastore.auth.clientCertSecretName -}}
{{ .Values.global.datastore.auth.clientCertSecretName }}
{{- else if and .Values.global.datastore .Values.global.datastore.certificates .Values.global.datastore.certificates.secretName -}}
{{ .Values.global.datastore.certificates.secretName }}
{{- else -}}
mongo-app-client-cert-secret
{{- end -}}
{{- end -}}

{{/*
Renders the MongoDB certificate volume definition for a pod spec.
Handles three cases:
  1. External MongoDB with x509 auth  → user-provided client cert secret (tls.crt, tls.key, ca.crt)
  2. External MongoDB with scram + CA → user-provided CA cert secret (ca.crt only)
  3. Internal MongoDB (default)       → cert-manager generated secret (optional: true)
Returns empty string if no cert volume is needed (external MongoDB, no certs configured).
*/}}
{{- define "nvsentinel.mongodb.certVolume" -}}
{{- $useExternal := and .Values.global.datastore
                        (eq .Values.global.datastore.provider "mongodb")
                        (not .Values.global.mongodbStore.enabled) -}}
{{- if $useExternal -}}
  {{- $authMechanism := "scram" -}}
  {{- if and .Values.global.datastore.auth .Values.global.datastore.auth.mechanism -}}
  {{- $authMechanism = .Values.global.datastore.auth.mechanism -}}
  {{- end -}}
  {{- $clientCertSecret := "" -}}
  {{- if and .Values.global.datastore.auth .Values.global.datastore.auth.clientCertSecretName -}}
  {{- $clientCertSecret = .Values.global.datastore.auth.clientCertSecretName -}}
  {{- end -}}
  {{- $caSecret := "" -}}
  {{- if and .Values.global.datastore.tls .Values.global.datastore.tls.caSecretName -}}
  {{- $caSecret = .Values.global.datastore.tls.caSecretName -}}
  {{- end -}}
  {{- if and (eq $authMechanism "x509") (ne $clientCertSecret "") -}}
- name: mongo-app-client-cert
  secret:
    secretName: {{ $clientCertSecret }}
    {{- include "nvsentinel.certificates.volumeItems" . | nindent 4 }}
    optional: false
  {{- else if ne $caSecret "" -}}
- name: mongo-app-client-cert
  secret:
    secretName: {{ $caSecret }}
    items:
    - key: ca.crt
      path: ca.crt
    optional: false
  {{- end -}}
  {{- /* else: no cert volume — external MongoDB with no custom CA or client certs configured */}}
{{- else -}}
- name: mongo-app-client-cert
  secret:
    secretName: {{ include "nvsentinel.certificates.secretName" . }}
    {{- include "nvsentinel.certificates.volumeItems" . | nindent 4 }}
    optional: true
{{- end -}}
{{- end -}}

{{/*
Returns "true" if a MongoDB cert volume will be rendered by nvsentinel.mongodb.certVolume,
"false" otherwise. Use this to conditionally render the corresponding volume mount.
*/}}
{{- define "nvsentinel.mongodb.hasCertVolume" -}}
{{- $useExternal := and .Values.global.datastore
                        (eq .Values.global.datastore.provider "mongodb")
                        (not .Values.global.mongodbStore.enabled) -}}
{{- if $useExternal -}}
  {{- $authMechanism := "scram" -}}
  {{- if and .Values.global.datastore.auth .Values.global.datastore.auth.mechanism -}}
  {{- $authMechanism = .Values.global.datastore.auth.mechanism -}}
  {{- end -}}
  {{- $clientCertSecret := "" -}}
  {{- if and .Values.global.datastore.auth .Values.global.datastore.auth.clientCertSecretName -}}
  {{- $clientCertSecret = .Values.global.datastore.auth.clientCertSecretName -}}
  {{- end -}}
  {{- $caSecret := "" -}}
  {{- if and .Values.global.datastore.tls .Values.global.datastore.tls.caSecretName -}}
  {{- $caSecret = .Values.global.datastore.tls.caSecretName -}}
  {{- end -}}
  {{- if or (and (eq $authMechanism "x509") (ne $clientCertSecret "")) (ne $caSecret "") -}}
true
  {{- else -}}
false
  {{- end -}}
{{- else -}}
true
{{- end -}}
{{- end -}}

{{/*
Returns the effective MongoDB cert mount path for a pod.
- Returns .Values.clientCertMountPath if explicitly set (covers x509 client-cert and CA-only modes).
- Returns /etc/ssl/mongo-ca only for external MongoDB SCRAM + custom CA (caSecretName set,
  clientCertMountPath empty). Do not use this fallback for in-cluster MongoDB when
  clientCertMountPath is empty — e.g. values-tilt-mongodb-tls-disabled.yaml disables TLS
  by setting clientCertMountPath to ""; hasCertVolume is still true in-cluster, but there
  is no CA secret and no file at /etc/ssl/mongo-ca/ca.crt.
- Returns empty string otherwise.
*/}}
{{- define "nvsentinel.mongodb.certMountPath" -}}
{{- if .Values.clientCertMountPath -}}
{{ .Values.clientCertMountPath }}
{{- else -}}
  {{- $useExternal := and .Values.global.datastore
                          (eq .Values.global.datastore.provider "mongodb")
                          (not .Values.global.mongodbStore.enabled) -}}
  {{- if $useExternal -}}
    {{- $caSecret := "" -}}
    {{- if and .Values.global.datastore.tls .Values.global.datastore.tls.caSecretName -}}
    {{- $caSecret = .Values.global.datastore.tls.caSecretName -}}
    {{- end -}}
    {{- if ne $caSecret "" -}}
/etc/ssl/mongo-ca
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{/*
Same path resolution as nvsentinel.mongodb.certMountPath but reads
.Values.mongodbStore.clientCertMountPath (event-exporter subchart layout).
Use this only from charts that store the path under mongodbStore.
*/}}
{{- define "nvsentinel.mongodb.certMountPathFromMongoStore" -}}
{{- if .Values.mongodbStore.clientCertMountPath -}}
{{ .Values.mongodbStore.clientCertMountPath }}
{{- else -}}
  {{- $useExternal := and .Values.global.datastore
                          (eq .Values.global.datastore.provider "mongodb")
                          (not .Values.global.mongodbStore.enabled) -}}
  {{- if $useExternal -}}
    {{- $caSecret := "" -}}
    {{- if and .Values.global.datastore.tls .Values.global.datastore.tls.caSecretName -}}
    {{- $caSecret = .Values.global.datastore.tls.caSecretName -}}
    {{- end -}}
    {{- if ne $caSecret "" -}}
/etc/ssl/mongo-ca
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{/*
MongoDB client certificate volume items
Maps configurable source keys to standard destination paths
*/}}
{{- define "nvsentinel.certificates.volumeItems" -}}
{{- $certKey := "tls.crt" -}}
{{- $keyKey := "tls.key" -}}
{{- $caKey := "ca.crt" -}}
{{- if and .Values.global.datastore .Values.global.datastore.certificates -}}
  {{- $certKey = .Values.global.datastore.certificates.certKey | default "tls.crt" -}}
  {{- $keyKey = .Values.global.datastore.certificates.keyKey | default "tls.key" -}}
  {{- $caKey = .Values.global.datastore.certificates.caKey | default "ca.crt" -}}
{{- end -}}
items:
  - key: {{ $certKey }}
    path: tls.crt
  - key: {{ $keyKey }}
    path: tls.key
  - key: {{ $caKey }}
    path: ca.crt
{{- end -}}
