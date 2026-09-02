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
{{- $hash := include "nvsentinel.externalMongoInitEval" . | sha256sum | trunc 8 -}}
{{- printf "%s-external-mongodb-setup-%s-%s" .Release.Name $ttl $hash | trunc 63 | trimSuffix "-" -}}
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
{{- range $key, $sa := dict "cspHealthMonitor" "csp-health-monitor" "kubernetesObjectMonitor" "kubernetes-object-monitor" "slurmDrainMonitor" "slurm-drain-monitor" "healthEventsAnalyzer" "health-events-analyzer" -}}
  {{- if (index (($.Values.global) | default dict) $key | default dict).enabled -}}
    {{- $derived = append $derived (printf "system:serviceaccount:%s:%s" $ns $sa) -}}
  {{- end -}}
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

