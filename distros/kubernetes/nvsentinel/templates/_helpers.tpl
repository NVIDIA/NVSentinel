{{/*
TTL from mongodb-store.collectionExpirySeconds (works when the subchart is disabled).
Nil/empty → 2592000. Strings must be base-10 digits in 0–2147483647 (int("abc") is 0).
*/}}
{{- define "nvsentinel.collectionExpirySeconds" -}}
{{- $store := index .Values "mongodb-store" | default dict -}}
{{- $raw := index $store "collectionExpirySeconds" -}}
{{- if or (kindIs "invalid" $raw) (eq ($raw | toString) "") -}}
{{- $raw = 2592000 -}}
{{- end -}}
{{- if kindIs "string" $raw -}}
{{- if not (regexMatch "^[0-9]+$" $raw) -}}
{{- fail (printf "mongodb-store.collectionExpirySeconds must be an integer from 0 through 2147483647, got %v" $raw) -}}
{{- end -}}
{{- if or (gt (len $raw) 10) (and (eq (len $raw) 10) (gt $raw "2147483647")) -}}
{{- fail (printf "mongodb-store.collectionExpirySeconds must be an integer from 0 through 2147483647, got %v" $raw) -}}
{{- end -}}
{{- end -}}
{{- $v := int $raw -}}
{{- if or (lt $v 0) (gt $v 2147483647) -}}
{{- fail (printf "mongodb-store.collectionExpirySeconds must be an integer from 0 through 2147483647, got %v" $raw) -}}
{{- end -}}
{{- $v -}}
{{- end }}

{{/*
<release>-external-mongodb-setup-<ttl>-<scriptHash> so a TTL or init-script
change is a new Job, not a patch on a completed one.
*/}}
{{- define "nvsentinel.externalMongoInitJobName" -}}
{{- $ttl := include "nvsentinel.collectionExpirySeconds" . | toString -}}
{{- /* A completed Job is immutable, so every value that shapes its pod (the
     script, the datastore settings behind image, TLS and auth, pull secrets
     and placement) is part of the name: a change renders a new Job instead of
     patching the old one. */ -}}
{{- $hash := print (include "nvsentinel.externalMongoInitEval" .) (toJson .Values.global.datastore) (toJson .Values.global.imagePullSecrets) (toJson .Values.global.systemNodeSelector) (toJson .Values.global.systemNodeTolerations) | sha256sum | trunc 8 -}}
{{- /* The suffix carries the hash, so it is the release name that gives way
     to the 63-character limit, never the hash. */ -}}
{{- $suffix := printf "-external-mongodb-setup-%s-%s" $ttl $hash -}}
{{- printf "%s%s" (.Release.Name | trunc (int (sub 63 (len $suffix))) | trimSuffix "-") $suffix -}}
{{- end }}

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
{{- include "nvsentinel.labelsWithName" (dict "context" . "name" (include "nvsentinel.name" .)) -}}
{{- end }}

{{/*
Common labels for an object named distinctly from the release, such as the
external MongoDB setup Job. Pass the name here rather than appending a second
app.kubernetes.io/name after "nvsentinel.labels": Helm's own parser keeps the
last of a duplicated mapping key, but the strict parsers in Flux's post-renderer
and Argo CD's kustomize reject the whole release.

Usage: include "nvsentinel.labelsWithName" (dict "context" $ "name" "my-object")
*/}}
{{- define "nvsentinel.labelsWithName" -}}
helm.sh/chart: {{ include "nvsentinel.chart" .context }}
app.kubernetes.io/name: {{ .name }}
app.kubernetes.io/instance: {{ .context.Release.Name }}
{{- if .context.Chart.AppVersion }}
app.kubernetes.io/version: {{ .context.Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .context.Release.Service }}
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
{{- $isPostgres := and .Values.global.datastore
                        (eq .Values.global.datastore.provider "postgresql") -}}
{{- $useExternal := and .Values.global.datastore
                        (eq .Values.global.datastore.provider "mongodb")
                        (not .Values.global.mongodbStore.enabled) -}}
{{- if $isPostgres -}}
false
{{- else if $useExternal -}}
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
Whether PostgreSQL should use the bundled client certificate. A credentials
Secret selects password authentication instead.
*/}}
{{- define "nvsentinel.datastore.postgresClientCertEnabled" -}}
{{- $isPostgres := and .Values.global.datastore (eq .Values.global.datastore.provider "postgresql") -}}
{{- $hasPasswordSecret := and $isPostgres .Values.global.datastore.credentialsFromSecret .Values.global.datastore.credentialsFromSecret.name -}}
{{- if and $isPostgres (not $hasPasswordSecret) -}}
true
{{- else -}}
false
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
{{- $isPostgresPassword := and .Values.global.datastore
                                (eq .Values.global.datastore.provider "postgresql")
                                (ne (include "nvsentinel.datastore.postgresClientCertEnabled" .) "true") -}}
{{- if not $isPostgresPassword -}}
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
{{- end -}}

{{/*
Same path resolution as nvsentinel.mongodb.certMountPath but reads
.Values.mongodbStore.clientCertMountPath (event-exporter subchart layout).
Use this only from charts that store the path under mongodbStore.
*/}}
{{- define "nvsentinel.mongodb.certMountPathFromMongoStore" -}}
{{- $isPostgresPassword := and .Values.global.datastore
                                (eq .Values.global.datastore.provider "postgresql")
                                (ne (include "nvsentinel.datastore.postgresClientCertEnabled" .) "true") -}}
{{- if not $isPostgresPassword -}}
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
{{- end -}}

{{/*
Name of existing Secret that holds MONGODB_URI for external MongoDB (provider mongodb).
Required whenever global.datastore is enabled with provider mongodb. Returns empty when unset.
*/}}
{{- define "nvsentinel.datastore.mongodbUriSecretName" -}}
{{- if and .Values.global.datastore .Values.global.datastore.credentialsFromSecret .Values.global.datastore.credentialsFromSecret.name -}}
{{- .Values.global.datastore.credentialsFromSecret.name | trim -}}
{{- end -}}
{{- end -}}

{{/*
Extra envFrom entry for the configured datastore credentials Secret.
Indent with nindent 12 to match sibling configMapRef under envFrom.
*/}}
{{- define "nvsentinel.datastore.secretEnvFrom" -}}
{{- if .Values.global.datastore -}}
{{- $sn := "" -}}
{{- if eq .Values.global.datastore.provider "mongodb" -}}
{{- $sn = include "nvsentinel.datastore.mongodbUriSecretName" . | trim -}}
{{- else if eq .Values.global.datastore.provider "postgresql" -}}
{{- $sn = include "nvsentinel.datastore.postgresPasswordSecretName" . | trim -}}
{{- end -}}
{{- if $sn }}
- secretRef:
    name: {{ $sn | quote }}
    optional: false
{{- end }}
{{- end }}
{{- end -}}

{{/*
Name of existing Secret that holds DATASTORE_PASSWORD for PostgreSQL password
authentication. Returns empty when unset.
*/}}
{{- define "nvsentinel.datastore.postgresPasswordSecretName" -}}
{{- if and .Values.global.datastore .Values.global.datastore.credentialsFromSecret .Values.global.datastore.credentialsFromSecret.name -}}
{{- .Values.global.datastore.credentialsFromSecret.name | trim -}}
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

{{/*
platform-connector health-event socket authentication.

The socket is the only place node identity is established: nothing downstream
re-checks which node an event names. These helpers give every cross-node
publisher the same projected token so the server-side allowlist and the
client-side credential cannot drift apart.
*/}}

{{/*
Renders "true" when node-binding authentication is on, "" otherwise, so it can
be used directly in an `if`.
*/}}
{{- define "nvsentinel.pcAuth.enabled" -}}
{{- $auth := ((.Values.global).platformConnectorAuth) | default dict -}}
{{- $enabled := $auth.enabled -}}
{{- /*
Must be a real YAML boolean. Go-template truthiness would otherwise decide this
for us: the string "false" is truthy and would ENABLE auth, while null and 0 are
falsy and would silently DISABLE it. platform-connector's own parser cannot
catch either, because the chart has already coerced the value into a valid
"true"/"false" by the time it reaches the ConfigMap. Fail the render instead.
*/ -}}
{{- if not (kindIs "bool" $enabled) -}}
{{- fail (printf "global.platformConnectorAuth.enabled must be a boolean (true or false), got %s %#v. Quoted strings, null and numbers are refused because they would silently enable or disable authentication." (kindOf $enabled) $enabled) -}}
{{- end -}}
{{- if $enabled -}}true{{- end -}}
{{- end -}}

{{/*
Node-binding enforcement mode: "enforce" (default) rejects a violating
request, "audit" records it and lets the request through. Consumed only by
platform-connector's own ConfigMap; publishers do not need it.
*/}}
{{- define "nvsentinel.pcAuth.mode" -}}
{{- $auth := ((.Values.global).platformConnectorAuth) | default dict -}}
{{- /*
`default` treats false, 0, "" and nil as empty, so `$auth.mode | default
"enforce"` would silently accept a typo'd non-string value by defaulting it
away instead of rejecting it. Check presence explicitly, then validate
whatever was actually supplied.
*/ -}}
{{- $mode := "enforce" -}}
{{- if hasKey $auth "mode" -}}
{{- $mode = index $auth "mode" -}}
{{- end -}}
{{- if not (kindIs "string" $mode) -}}
{{- fail (printf "global.platformConnectorAuth.mode must be a string (\"enforce\" or \"audit\"), got %s %#v." (kindOf $mode) $mode) -}}
{{- end -}}
{{- if not (or (eq $mode "enforce") (eq $mode "audit")) -}}
{{- fail (printf "global.platformConnectorAuth.mode must be \"enforce\" or \"audit\", got %q." $mode) -}}
{{- end -}}
{{- $mode -}}
{{- end -}}

{{/*
Renders "true" when a validator that never reached a verdict (API server
unreachable, or timed out) should fall back to node-local scope instead of
rejecting the request; "" otherwise. Does not affect a rejected credential,
which is always rejected. Consumed only by platform-connector's own
ConfigMap.
*/}}
{{- define "nvsentinel.pcAuth.failOpenOnUnavailable" -}}
{{- $auth := ((.Values.global).platformConnectorAuth) | default dict -}}
{{- /*
Same reasoning as nvsentinel.pcAuth.mode: `default` would treat an explicit
0 as absent and silently coerce it to false rather than rejecting the wrong
type, so presence is checked explicitly first.
*/ -}}
{{- $failOpen := false -}}
{{- if hasKey $auth "failOpenOnUnavailable" -}}
{{- $failOpen = index $auth "failOpenOnUnavailable" -}}
{{- end -}}
{{- if not (kindIs "bool" $failOpen) -}}
{{- fail (printf "global.platformConnectorAuth.failOpenOnUnavailable must be a boolean (true or false), got %s %#v." (kindOf $failOpen) $failOpen) -}}
{{- end -}}
{{- if $failOpen -}}true{{- end -}}
{{- end -}}

{{/*
Audience the projected tokens are minted for and that platform-connector
requires. Defined once: a token minted for one audience and checked against
another is rejected at runtime with nothing in the rendered manifests to show
why.
*/}}
{{- define "nvsentinel.pcAuth.audience" -}}
{{- if (include "nvsentinel.pcAuth.enabled" .) -}}
{{- required "global.platformConnectorAuth.audience is required when platform-connector auth is enabled" (((.Values.global).platformConnectorAuth).audience) -}}
{{- end -}}
{{- end -}}

{{/*
Directory the projected platform-connector token is mounted at.
*/}}
{{- define "nvsentinel.pcAuth.mountPath" -}}
{{- if (include "nvsentinel.pcAuth.enabled" .) -}}
{{- required "global.platformConnectorAuth.tokenMountPath is required when platform-connector auth is enabled" (((.Values.global).platformConnectorAuth).tokenMountPath) -}}
{{- end -}}
{{- end -}}

{{/*
Full path of the projected token file, for the publishers' --*-token-path flags.
*/}}
{{- define "nvsentinel.pcAuth.tokenPath" -}}
{{- printf "%s/token" (include "nvsentinel.pcAuth.mountPath" .) -}}
{{- end -}}

{{/*
Projected token volume for a cross-node publisher. Indent with `nindent 8`
alongside sibling entries under `volumes:`.
*/}}
{{- define "nvsentinel.pcAuth.volume" -}}
- name: platform-connector-token
  projected:
    sources:
      - serviceAccountToken:
          audience: {{ include "nvsentinel.pcAuth.audience" . | quote }}
          expirationSeconds: {{ include "nvsentinel.pcAuth.expirationSeconds" . }}
          path: token
{{- end -}}

{{/*
Matching volumeMount. Indent with `nindent 12` under `volumeMounts:`.
*/}}
{{- define "nvsentinel.pcAuth.volumeMount" -}}
- name: platform-connector-token
  mountPath: {{ include "nvsentinel.pcAuth.mountPath" . }}
  readOnly: true
{{- end -}}

{{/*
JSON array of the canonical usernames allowed to name nodes other than their
own, for the platform-connector ConfigMap.

Entries are passed through verbatim — the namespace is never filled in on the
operator's behalf, because an entry that silently became
"system:serviceaccount:default:x" would grant cross-node reach to an account
nobody meant to name. The two checks below turn the misconfigurations that
would otherwise surface as runtime rejections into a failed render.
*/}}
{{- define "nvsentinel.pcAuth.crossNodeUsernames" -}}
{{- if not (include "nvsentinel.pcAuth.enabled" .) -}}
[]
{{- else -}}
{{- $auth := (((.Values.global).platformConnectorAuth)) | default dict -}}
{{- /*
The bundled cluster-scoped monitors are DERIVED from the rendered namespace
rather than listed. Their ServiceAccount names are fixed by this chart and the
namespace is a fact the chart already knows, so writing them out by hand only
created a way to be wrong: a hardcoded "nvsentinel" installed into any other
namespace renders successfully and then has every one of its events rejected at
runtime. Only monitors that are actually enabled are included.
*/ -}}
{{- $ns := .Release.Namespace -}}
{{- $derived := list -}}
{{- range $key, $sa := dict "cspHealthMonitor" "csp-health-monitor" "kubernetesObjectMonitor" "kubernetes-object-monitor" "nvcreCertificationMonitor" "nvcre-certification-monitor" "slurmDrainMonitor" "slurm-drain-monitor" "healthEventsAnalyzer" "health-events-analyzer" -}}
  {{- if (index (($.Values.global) | default dict) $key | default dict).enabled -}}
    {{- $derived = append $derived (printf "system:serviceaccount:%s:%s" $ns $sa) -}}
  {{- end -}}
{{- end -}}
{{- /*
lifecycle-manager is derived separately from the table above because it is
gated on a FEATURE rather than on the component: the chart is enabled in plenty
of installs that never publish a health event, and only the MaintenanceRequest
controller opens the socket. An MR may name any node in the cluster, so once
that controller is on this is a cross-node publisher.

The name is taken from the subchart rather than written out here. Its
ServiceAccount is a fixed "lifecycle-manager" only because the subchart ships
fullNameOverride; overriding that or serviceAccount.name would otherwise leave
this allowlist naming an identity that does not exist — the same silent
runtime rejection the derived namespace above exists to prevent.
*/ -}}
{{- $lm := (index $.Values "lifecycle-manager") | default dict -}}
{{- if and ((index (($.Values.global) | default dict) "lifecycleManager") | default dict).enabled ((($lm.controllers) | default dict).maintenanceRequest | default dict).enabled -}}
{{- $lmSA := include "lifecycle-manager.serviceAccountName" (dict "Values" $lm "Chart" (dict "Name" "lifecycle-manager") "Release" $.Release) -}}
{{- if eq $lmSA "default" -}}
{{- fail "lifecycle-manager must use a dedicated ServiceAccount when the MaintenanceRequest controller and platform-connector authentication are enabled; set lifecycle-manager.serviceAccount.name when lifecycle-manager.serviceAccount.create is false" -}}
{{- end -}}
{{- $derived = append $derived (printf "system:serviceaccount:%s:%s" $ns $lmSA) -}}
{{- end -}}
{{- /*
crossNodeServiceAccounts is now only for callers this chart does not ship. It
may be absent (no extra callers) but never null, which is an ambiguous way of
writing "none".
*/ -}}
{{- $extra := list -}}
{{- if hasKey $auth "crossNodeServiceAccounts" -}}
{{- $extra = index $auth "crossNodeServiceAccounts" -}}
{{- if kindIs "invalid" $extra -}}
{{- fail "global.platformConnectorAuth.crossNodeServiceAccounts is null. Write an explicit [] to add no callers beyond the bundled monitors, or list the canonical usernames of your own cross-node publishers." -}}
{{- end -}}
{{- if not (kindIs "slice" $extra) -}}
{{- fail (printf "global.platformConnectorAuth.crossNodeServiceAccounts must be a list, got %s %#v." (kindOf $extra) $extra) -}}
{{- end -}}
{{- end -}}
{{- range $sa := $extra -}}
  {{- if not (regexMatch "^system:serviceaccount:[a-z0-9]([-a-z0-9]*[a-z0-9])?:[a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*$" $sa) -}}
    {{- fail (printf "global.platformConnectorAuth.crossNodeServiceAccounts entry %q is not a canonical Kubernetes username; want \"system:serviceaccount:<namespace>:<name>\". The namespace must be a DNS-1123 label and the name a DNS-1123 subdomain, so stray whitespace or capitals are refused rather than trimmed: an entry that does not match exactly can never equal the username TokenReview reports" $sa) -}}
  {{- end -}}
{{- $seg := splitList ":" $sa -}}
{{- if gt (len (index $seg 2)) 63 -}}
{{- fail (printf "global.platformConnectorAuth.crossNodeServiceAccounts entry %q has a %d-character namespace; Kubernetes limits it to 63" $sa (len (index $seg 2))) -}}
{{- end -}}
{{- if gt (len (index $seg 3)) 253 -}}
{{- fail (printf "global.platformConnectorAuth.crossNodeServiceAccounts entry %q has a %d-character ServiceAccount name; Kubernetes limits it to 253" $sa (len (index $seg 3))) -}}
{{- end -}}
{{- end -}}
{{- concat $derived $extra | uniq | toJson -}}
{{- end -}}
{{- end -}}

{{/*
deployment platform connector helpers. The facts the publishers share (gRPC
port, TLS mode) live under global.platformConnectorDeployment, the only place
subcharts can read them; server-only knobs stay under platformConnector.deployment.
*/}}

{{- define "nvsentinel.pcDeployment.name" -}}
platform-connector-deployment
{{- end }}

{{/*
Renders "true" when the deployment platform connector is enabled, ""
otherwise, so it can be used directly in an `if`. An absent value is off, as
before the key existed (for example on a --reuse-values upgrade).
*/}}
{{- define "nvsentinel.pcDeployment.enabled" -}}
{{- $v := (((.Values.platformConnector).deployment) | default dict).enabled -}}
{{- if kindIs "invalid" $v -}}
{{- $v = false -}}
{{- end -}}
{{- include "nvsentinel.strictBool" (list "platformConnector.deployment.enabled" $v "enable or disable the deployment platform connector") -}}
{{- end }}

{{/*
Renders "true" when the node-local platform connector DaemonSet is enabled,
"" otherwise, so it can be used directly in an `if`. An absent value is on, as
before the key existed (for example on a --reuse-values upgrade).
*/}}
{{- define "nvsentinel.pcDaemonset.enabled" -}}
{{- $v := (((.Values.platformConnector).daemonset) | default dict).enabled -}}
{{- if kindIs "invalid" $v -}}
{{- $v = true -}}
{{- end -}}
{{- include "nvsentinel.strictBool" (list "platformConnector.daemonset.enabled" $v "add or remove the node-local ingestion path") -}}
{{- end }}

{{/*
A values toggle that must be a real YAML boolean: renders "true" when set,
"" when false, and fails the render for anything else. Go-template
truthiness would otherwise decide for us: the string "false" (for example
from --set-string) is truthy, while null and 0 are falsy, so a wrong type
would silently flip the toggle. Called with (list "<values path>" <value>
"<what a wrong type would silently do>").
*/}}
{{- define "nvsentinel.strictBool" -}}
{{- $name := index . 0 -}}
{{- $v := index . 1 -}}
{{- if not (kindIs "bool" $v) -}}
{{- fail (printf "%s must be a boolean (true or false), got %s %#v. Quoted strings and numbers are refused because they would silently %s." $name (kindOf $v) $v (index . 2)) -}}
{{- end -}}
{{- if $v -}}true{{- end -}}
{{- end }}

{{/*
Validated TLS mode: "required" (cert-manager issued server certificate) or the
explicitly named "insecureDevelopmentMode". Anything else fails the render:
the token crosses the pod network in gRPC metadata, so a silently plaintext
listener must not be reachable through a typo.
*/}}
{{- define "nvsentinel.pcDeployment.tlsMode" -}}
{{- $mode := ((((.Values.global).platformConnectorDeployment).tls).mode) | default "required" -}}
{{- if not (or (eq $mode "required") (eq $mode "insecureDevelopmentMode")) -}}
{{- fail (printf "global.platformConnectorDeployment.tls.mode must be \"required\" or \"insecureDevelopmentMode\", got %q." $mode) -}}
{{- end -}}
{{- $mode -}}
{{- end }}

{{/*
gRPC port the server listens on and the publishers dial; also the Service
and NetworkPolicy port. Read from here everywhere so the listener, the
Service, the allow rule and the publishers cannot disagree.
*/}}
{{- define "nvsentinel.pcDeployment.grpcPort" -}}
{{- $port := include "nvsentinel.positiveInt" (list "global.platformConnectorDeployment.grpcPort" ((((.Values.global).platformConnectorDeployment).grpcPort) | default 50051)) -}}
{{- if gt ($port | int64) (int64 65535) -}}
{{- fail (printf "global.platformConnectorDeployment.grpcPort must be at most 65535, got %s" $port) -}}
{{- end -}}
{{- $port -}}
{{- end }}

{{/*
Selector labels. Deliberately distinct from nvsentinel.selectorLabels: the
DaemonSet's selector matches on app.kubernetes.io/name, so a Deployment pod
carrying the same name label would be claimed by the DaemonSet controller.
*/}}
{{- define "nvsentinel.pcDeployment.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nvsentinel.pcDeployment.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "nvsentinel.pcDeployment.labels" -}}
{{- include "nvsentinel.labelsWithName" (dict "context" . "name" (include "nvsentinel.pcDeployment.name" .)) -}}
{{- end }}

{{/*
Secret written by the cert-manager Certificate. cert-manager copies the
issuing CA into ca.crt of the same secret, which is how the publishing
clients get their trust bundle (janitor-provider / janitor pattern).
*/}}
{{- define "nvsentinel.pcDeployment.certSecretName" -}}
{{- printf "%s-grpc-cert" (include "nvsentinel.pcDeployment.name" .) -}}
{{- end }}

{{/*
Why the gRPC rule admits every source, as a sentence for the policy annotation
and the release notes; empty when no host-network publisher targets the
Deployment. Two publishers can run on the host network: the gpu monitor
(useHostNetworking or the external-hostengine DCGM mode) and the preflight
checks, when the workload pods they are injected into run on the host network
(preflight.hostNetworkWorkloads). The gpu subchart is only loaded while it is
enabled, so its helper is called behind the same toggle.
*/}}
{{- define "nvsentinel.pcDeployment.hostNetworkPublisherReason" -}}
{{- $reasons := list -}}
{{- $gpu := index .Values "gpu-health-monitor" | default dict -}}
{{- $gpuEnabled := ((((.Values.global) | default dict).gpuHealthMonitor) | default dict).enabled -}}
{{- if and $gpuEnabled (eq ($gpu.publishTo | default "socket") "deployment") -}}
{{- $ctx := dict "Values" (merge (deepCopy $gpu) (dict "global" ((.Values.global) | default dict))) -}}
{{- if eq (include "gpu-health-monitor.useHostNetworking" $ctx) "true" -}}
{{- $reasons = append $reasons "gpu-health-monitor publishes from the host network (useHostNetworking or the external-hostengine DCGM mode)" -}}
{{- end -}}
{{- end -}}
{{- $hostNetworkWorkloads := (index .Values "preflight" | default dict).hostNetworkWorkloads -}}
{{- if and (include "nvsentinel.pcDeployment.preflightPublisher" .) (not (kindIs "invalid" $hostNetworkWorkloads)) -}}
{{- if include "nvsentinel.strictBool" (list "preflight.hostNetworkWorkloads" $hostNetworkWorkloads "open the deployment platform connector's gRPC rule to every source") -}}
{{- $reasons = append $reasons "the preflight checks publish from host-network workload pods (preflight.hostNetworkWorkloads)" -}}
{{- end -}}
{{- end -}}
{{- join ", and " $reasons -}}
{{- end }}

{{/*
"true" when the preflight checks publish to the deployment platform
connector: preflight is enabled and its publishTo is "deployment". The
webhook then stamps the health-publisher label on the tenant pods it injects
into, so the gRPC rule admits labelled pods in the namespaces that
preflight.namespaceSelector selects.
*/}}
{{- define "nvsentinel.pcDeployment.preflightPublisher" -}}
{{- $enabled := ((((.Values.global) | default dict).preflight) | default dict).enabled -}}
{{- if and $enabled (eq ((index .Values "preflight" | default dict).publishTo | default "socket") "deployment") -}}true{{- end -}}
{{- end }}

{{/*
A values number as a plain positive integer, refusing anything else. Helm
reads numbers from values files as floats and prints a million as "1e+06",
which the binaries refuse to parse; a string is accepted when it holds an
integer. Called with (list "<values path>" <value>).
*/}}
{{- define "nvsentinel.positiveInt" -}}
{{- $name := index . 0 -}}
{{- $v := index . 1 -}}
{{- $ok := false -}}
{{- if or (kindIs "float64" $v) (kindIs "int64" $v) (kindIs "int" $v) -}}
{{- $ok = and (gt ($v | int64) (int64 0)) (eq (toString ($v | int64 | float64)) (toString ($v | float64))) -}}
{{- else if kindIs "string" $v -}}
{{- $ok = and (gt ($v | int64) (int64 0)) (eq (toString ($v | int64)) $v) -}}
{{- end -}}
{{- if not $ok -}}
{{- fail (printf "%s must be a positive integer, got %#v" $name $v) -}}
{{- end -}}
{{- $v | int64 -}}
{{- end }}

{{/*
Datastore wiring of both platform connector roles: the client certificate
env vars, the ConfigMap and Secret envFrom, the client certificate volumes
and mounts, and the PostgreSQL certificate permission fix. The DaemonSet and
the Deployment render these from the same helpers, so their store paths
cannot drift apart.
*/}}

{{/*
The MongoDB client certificate directory the platform connector mounts: the
explicit value, else the chart's derived path (empty when there is none).
*/}}
{{- define "nvsentinel.platformConnector.mongoCertMountPath" -}}
{{- .Values.platformConnector.mongodbStore.clientCertMountPath | default (include "nvsentinel.mongodb.certMountPath" . | trim) -}}
{{- end }}

{{- define "nvsentinel.platformConnector.datastoreEnv" -}}
{{- if and .Values.global.datastore (eq .Values.global.datastore.provider "postgresql") -}}
{{- if eq (include "nvsentinel.platformConnector.pgClientCert" .) "true" -}}
- name: POSTGRESQL_CLIENT_CERT_MOUNT_PATH
  value: {{ .Values.platformConnector.postgresqlStore.clientCertMountPath }}
{{- end -}}
{{- else -}}
- name: MONGODB_CLIENT_CERT_MOUNT_PATH
  value: {{ include "nvsentinel.platformConnector.mongoCertMountPath" . }}
{{- end }}
{{- end }}

{{- define "nvsentinel.platformConnector.datastoreEnvFrom" -}}
- configMapRef:
    name: {{ if .Values.global.datastore }}{{ .Release.Name }}-datastore-config{{ else }}mongodb-config{{ end }}
    optional: true
{{- include "nvsentinel.datastore.secretEnvFrom" . }}
{{- end }}

{{- define "nvsentinel.platformConnector.datastoreVolumeMounts" -}}
{{- $mongoPC := include "nvsentinel.platformConnector.mongoCertMountPath" . -}}
{{- if and .Values.global.datastore (eq .Values.global.datastore.provider "postgresql") -}}
{{- if eq (include "nvsentinel.platformConnector.pgClientCert" .) "true" -}}
- name: client-certs-fixed
  mountPath: {{ .Values.platformConnector.postgresqlStore.clientCertMountPath }}
  readOnly: true
{{- end }}
{{- else if and (eq (include "nvsentinel.mongodb.hasCertVolume" .) "true") $mongoPC -}}
- name: mongo-app-client-cert
  mountPath: {{ $mongoPC }}
  readOnly: true
{{- end }}
{{- end }}

{{- define "nvsentinel.platformConnector.datastoreVolumes" -}}
{{- if and .Values.global.datastore (eq .Values.global.datastore.provider "postgresql") -}}
{{- if eq (include "nvsentinel.platformConnector.pgClientCert" .) "true" -}}
- name: postgresql-client-cert-original
  secret:
    secretName: postgresql-client-cert
    optional: false
- name: client-certs-fixed
  emptyDir: {}
{{- end }}
{{- else -}}
{{- include "nvsentinel.mongodb.certVolume" . }}
{{- end }}
{{- end }}

{{/*
nvsentinel.platformConnector.pgClientCert: "true" when the platform connector
mounts a PostgreSQL client certificate: provider postgresql, a mount path set,
and certificate rather than password authentication (the certificate Secret
only exists then).
*/}}
{{- define "nvsentinel.platformConnector.pgClientCert" -}}
{{- if and .Values.platformConnector.postgresqlStore.clientCertMountPath (eq (include "nvsentinel.datastore.postgresClientCertEnabled" .) "true") -}}
true
{{- else -}}
false
{{- end -}}
{{- end }}

{{/*
The PostgreSQL client certificate permission fix, rendered under
`initContainers:` with `nindent 8`. Called with (dict "context" . "runAsUser"
<uid> "runAsGroup" <gid>): the copy runs as the user of the container that
reads the key, since it makes the key private.
*/}}
{{- define "nvsentinel.platformConnector.pgCertInitContainer" -}}
{{- $user := . -}}
{{- with .context -}}
{{- if eq (include "nvsentinel.platformConnector.pgClientCert" .) "true" -}}
- name: fix-cert-permissions
  image: "{{ .Values.global.initContainerImage.repository }}:{{ .Values.global.initContainerImage.tag }}"
  imagePullPolicy: {{ .Values.global.initContainerImage.pullPolicy }}
  securityContext:
    runAsUser: {{ $user.runAsUser }}
    {{- with $user.runAsGroup }}
    runAsGroup: {{ . }}
    {{- end }}
  command:
    - sh
    - -c
    - |
      echo "Copying PostgreSQL client certificates with correct permissions..."
      cp /etc/ssl/client-certs-original/tls.crt /etc/ssl/client-certs-fixed/
      cp /etc/ssl/client-certs-original/ca.crt /etc/ssl/client-certs-fixed/
      cp /etc/ssl/client-certs-original/tls.key /etc/ssl/client-certs-fixed/
      chmod 644 /etc/ssl/client-certs-fixed/tls.crt
      chmod 644 /etc/ssl/client-certs-fixed/ca.crt
      chmod 600 /etc/ssl/client-certs-fixed/tls.key
      echo "Certificate permissions fixed:"
      ls -la /etc/ssl/client-certs-fixed/
  volumeMounts:
    - name: postgresql-client-cert-original
      mountPath: /etc/ssl/client-certs-original
      readOnly: true
    - name: client-certs-fixed
      mountPath: /etc/ssl/client-certs-fixed
{{- end }}
{{- end }}
{{- end }}

{{- define "nvsentinel.pcAuth.expirationSeconds" -}}
{{- $v := (((.Values.global).platformConnectorAuth)).tokenExpirationSeconds -}}
{{- if kindIs "invalid" $v -}}
{{- fail "global.platformConnectorAuth.tokenExpirationSeconds is required when platform-connector auth is enabled" -}}
{{- end -}}
{{- if not (or (kindIs "float64" $v) (kindIs "int" $v) (kindIs "int64" $v)) -}}
{{- fail (printf "global.platformConnectorAuth.tokenExpirationSeconds must be an integer, got %s %#v." (kindOf $v) $v) -}}
{{- end -}}
{{- /*
YAML numbers reach templates as float64, so a fractional value passes a bare
numeric check and then renders into an integer Kubernetes field, which the API
server rejects when the pod is created.
*/ -}}
{{- if ne (float64 $v) (floor (float64 $v)) -}}
{{- fail (printf "global.platformConnectorAuth.tokenExpirationSeconds must be a whole number of seconds, got %v." $v) -}}
{{- end -}}
{{- /*
Kubernetes rejects a projected ServiceAccount token lifetime below 10 minutes or
above 2^32 seconds (core validation, volume projection). Out-of-range values
render fine and are then refused by the API server when the pod is created, so
the workload never starts and the reason is a long way from the values file.
*/ -}}
{{- if lt (float64 $v) 600.0 -}}
{{- fail (printf "global.platformConnectorAuth.tokenExpirationSeconds is %v, but Kubernetes rejects a projected token lifetime under 600 seconds (10 minutes)." $v) -}}
{{- end -}}
{{- if gt (float64 $v) 4294967296.0 -}}
{{- fail (printf "global.platformConnectorAuth.tokenExpirationSeconds is %v, but Kubernetes rejects a projected token lifetime over 2^32 seconds." $v) -}}
{{- end -}}
{{- int64 $v -}}
{{- end -}}

{{/*
direct publishing: wiring for publishTo "deployment", where a publisher sends
health events straight to the deployment platform connector instead of the
node-local socket. Shared by csp-health-monitor and health-events-analyzer,
which render only under this chart and include these with their own context
(.Values is the subchart's, so publishTo is the subchart's); the standalone
charts carry their own copy under their own prefix.

nvsentinel.publish.enabled renders "true" when the including chart publishes
directly to the deployment platform connector, "" otherwise, so it can be used
directly in an `if`; any other publishTo fails the render.
*/}}
{{- define "nvsentinel.publish.enabled" -}}
{{- $v := .Values.publishTo | default "socket" -}}
{{- if not (or (eq $v "socket") (eq $v "deployment")) -}}
{{- fail (printf "%s.publishTo must be \"socket\" or \"deployment\", got %q." .Chart.Name $v) -}}
{{- end -}}
{{- if eq $v "deployment" -}}true{{- end -}}
{{- end -}}

{{/*
Environment read by the shared publishing clients (identical names in Go and
Python): HEALTH_PUBLISH_TARGET set switches the client to direct mode, which
then ignores the socket flags. The caller token is the socket path's projected
token (nvsentinel.pcAuth.volume). Empty on the socket path; indent with
`nindent 12` under `env:`.
*/}}
{{- define "nvsentinel.publish.envVars" -}}
{{- if include "nvsentinel.publish.enabled" . -}}
- name: HEALTH_PUBLISH_TARGET
  value: "{{ include "nvsentinel.pcDeployment.name" . }}.{{ .Release.Namespace }}.svc.cluster.local:{{ include "nvsentinel.pcDeployment.grpcPort" . }}"
- name: HEALTH_PUBLISH_TOKEN_PATH
  value: {{ include "nvsentinel.pcAuth.tokenPath" . | quote }}
{{- if eq (include "nvsentinel.pcDeployment.tlsMode" .) "required" }}
- name: HEALTH_PUBLISH_TLS_CA_FILE
  value: "/etc/nvsentinel/platform-connector-deployment-ca/ca.crt"
{{- else }}
- name: HEALTH_PUBLISH_INSECURE
  value: "true"
{{- end }}
{{- end -}}
{{- end -}}

{{/*
The CA bundle from the server's cert-manager Secret, mounted while TLS is
required. Empty on the socket path and in insecureDevelopmentMode.
*/}}
{{- define "nvsentinel.publish.volumeMounts" -}}
{{- if and (include "nvsentinel.publish.enabled" .) (eq (include "nvsentinel.pcDeployment.tlsMode" .) "required") -}}
- name: platform-connector-deployment-ca
  mountPath: /etc/nvsentinel/platform-connector-deployment-ca
  readOnly: true
{{- end -}}
{{- end -}}

{{/*
Matching volume.
*/}}
{{- define "nvsentinel.publish.volumes" -}}
{{- if and (include "nvsentinel.publish.enabled" .) (eq (include "nvsentinel.pcDeployment.tlsMode" .) "required") -}}
- name: platform-connector-deployment-ca
  secret:
    secretName: {{ include "nvsentinel.pcDeployment.certSecretName" . }}
    items:
      - key: ca.crt
        path: ca.crt
{{- end -}}
{{- end -}}
