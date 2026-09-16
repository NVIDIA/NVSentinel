{{/*
Manual-PV size: mongodb.persistence.size, or Percona rs0 volumeSpec when usePerconaOperator.
*/}}
{{- define "mongodb-store.persistenceSize" -}}
{{- if .Values.usePerconaOperator -}}
{{- $psmdb := index .Values "psmdb-db" | default dict -}}
{{- $rs0 := index (index $psmdb "replsets" | default dict) "rs0" | default dict -}}
{{- $pvc := index (index $rs0 "volumeSpec" | default dict) "pvc" | default dict -}}
{{- $req := index (index $pvc "resources" | default dict) "requests" | default dict -}}
{{- index $req "storage" | default "8Gi" -}}
{{- else -}}
{{- .Values.mongodb.persistence.size | default "8Gi" -}}
{{- end -}}
{{- end }}

{{/*
true when it is safe to run replSetResizeOplog (a real PVC, not emptyDir/hostPath).
false for Bitnami persistence.enabled=false (emptyDir) and Percona hostPath/emptyDir.
*/}}
{{- define "mongodb-store.oplogResizeEnabled" -}}
{{- if .Values.usePerconaOperator -}}
{{- $psmdb := index .Values "psmdb-db" | default dict -}}
{{- $rs0 := index (index $psmdb "replsets" | default dict) "rs0" | default dict -}}
{{- $vs := index $rs0 "volumeSpec" | default dict -}}
{{- if or (index $vs "hostPath") (index $vs "emptyDir") -}}
false
{{- else if not (index $vs "pvc") -}}
false
{{- else -}}
true
{{- end -}}
{{- else -}}
{{- $en := index .Values.mongodb.persistence "enabled" -}}
{{- if kindIs "invalid" $en -}}
true
{{- else if eq ($en | toString) "false" -}}
false
{{- else -}}
true
{{- end -}}
{{- end -}}
{{- end }}

{{/*
TTL expireAfterSeconds. Nil/empty → 2592000. int(nil)/int("abc") is 0 (immediate expiry).
Strings must be base-10 digits; range 0–2147483647 is checked before int for strings.
*/}}
{{- define "mongodb-store.collectionExpirySeconds" -}}
{{- $raw := .Values.collectionExpirySeconds -}}
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
Oplog size in MiB. The operator sets this; the chart does not derive it from the PVC.
Nil/empty → 990 (MongoDB minimum). Integer >= 990.
*/}}
{{- define "mongodb-store.oplogSizeMB" -}}
{{- $raw := .Values.oplogSizeMB -}}
{{- if or (kindIs "invalid" $raw) (eq ($raw | toString) "") -}}
{{- $raw = 990 -}}
{{- end -}}
{{- $s := $raw | toString -}}
{{- if not (regexMatch "^[0-9]+$" $s) -}}
{{- fail (printf "mongodb-store.oplogSizeMB must be an integer >= 990, got %v" $raw) -}}
{{- end -}}
{{- $mb := int $s -}}
{{- if lt $mb 990 -}}
{{- fail (printf "mongodb-store.oplogSizeMB must be an integer >= 990, got %v" $raw) -}}
{{- end -}}
{{- $mb -}}
{{- end }}

{{/*
New empty members read oplog size from our values, not vendored chart templates.
Bitnami: mongodb.extraFlags must include --oplogSize=<oplogSizeMB> (a mounted
snippet mongodb.conf makes the image skip dbPath/logpath and crashloop).
Percona: psmdb-db.replsets.rs0.configuration must contain oplogSizeMB: <oplogSizeMB>.
*/}}
{{- define "mongodb-store.validateOplogStartup" -}}
{{- $want := include "mongodb-store.oplogSizeMB" . | toString -}}
{{- if .Values.usePerconaOperator -}}
{{- $cfg := "" -}}
{{- $psmdb := index .Values "psmdb-db" | default dict -}}
{{- $rs0 := index (index $psmdb "replsets" | default dict) "rs0" | default dict -}}
{{- $cfg = index $rs0 "configuration" | default "" | toString -}}
{{- if not (regexMatch (printf "(?m)^[ \\t]*oplogSizeMB:[ \\t]*%s[ \\t]*$" $want) $cfg) -}}
{{- fail (printf "mongodb-store.oplogSizeMB is %s but psmdb-db.replsets.rs0.configuration has no matching oplogSizeMB. Put replication.oplogSizeMB: %s in that block. See docs/configuration/mongodb-store.md#oplog-size" $want $want) -}}
{{- end -}}
{{- else -}}
{{- $joined := "" -}}
{{- range (.Values.mongodb.extraFlags | default list) -}}
{{- $joined = printf "%s %s" $joined (. | toString) -}}
{{- end -}}
{{- $wantFlag := printf "--oplogSize=%s" $want -}}
{{- $all := regexFindAll "--oplogSize=[0-9]+" $joined -1 -}}
{{- if eq (len $all) 0 -}}
{{- fail (printf "mongodb-store.oplogSizeMB is %s but mongodb.extraFlags has no %s. Add that flag (do not mount a snippet mongodb.conf). See docs/configuration/mongodb-store.md#oplog-size" $want $wantFlag) -}}
{{- end -}}
{{- range $all -}}
{{- if ne . $wantFlag -}}
{{- fail (printf "mongodb-store.oplogSizeMB is %s but mongodb.extraFlags has %s. Set %s (do not mount a snippet mongodb.conf). See docs/configuration/mongodb-store.md#oplog-size" $want . $wantFlag) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Direct-connection hostnames for replSetResizeOplog (must run on every member).
*/}}
{{- define "mongodb-store.oplogMemberHosts" -}}
{{- $ns := .Release.Namespace -}}
{{- $hosts := list -}}
{{- if .Values.usePerconaOperator -}}
{{- $n := index .Values "psmdb-db" "replsets" "rs0" "size" | default 3 | int -}}
{{- range $i := until $n -}}
{{- $hosts = append $hosts (printf "mongodb-rs0-%d.mongodb-rs0.%s.svc.cluster.local" $i $ns) -}}
{{- end -}}
{{- else -}}
{{- $n := .Values.mongodb.replicaCount | default 3 | int -}}
{{- range $i := until $n -}}
{{- $hosts = append $hosts (printf "mongodb-%d.mongodb-headless.%s.svc.cluster.local" $i $ns) -}}
{{- end -}}
{{- end -}}
{{- join " " $hosts -}}
{{- end }}

{{/*
create-mongodb-database-<ttl>-<scriptHash> so a TTL, oplog size, or init-script
change is a new Job, not a patch on a completed one.
*/}}
{{- define "mongodb-store.initJobName" -}}
{{- $ttl := include "mongodb-store.collectionExpirySeconds" . | toString -}}
{{- $hash := printf "%s\n%s\n%s\n%s\n%s" (include "mongodb-store.oplogSizeMB" .) (include "mongodb-store.oplogMemberHosts" .) (include "mongodb-store.initEval" .) (include "mongodb-store.oplogEval" .) (include "mongodb-store.oplogResizeEnabled" .) | sha256sum | trunc 8 -}}
{{- printf "create-mongodb-database-%s-%s" $ttl $hash | trunc 63 -}}
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
{{- if .Values.serviceAccount.create }}
{{- default (include "nvsentinel.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Refuse a Percona version set that cannot work, and let psmdbVersion declare the
intended one so an operator sets a single value instead of three.

Permits one minor of operator-ahead-of-crVersion skew on purpose: that is
Percona's documented upgrade path, so demanding equality would block the very
staged upgrade Percona requires.
*/}}
{{- define "mongodb-store.validatePsmdbVersions" -}}
{{- if .Values.usePerconaOperator -}}
{{- $semver := "^[0-9]+\\.[0-9]+\\.[0-9]+" -}}
{{- $doc := "See docs/configuration/mongodb-store.md#percona-versions" -}}
{{- $operator := index .Values "psmdb-operator" | default dict -}}
{{- $opTag := (index ($operator.image | default dict) "tag") | default "" | toString -}}
{{- $db := index .Values "psmdb-db" | default dict -}}
{{- $crVersion := (index $db "crVersion") | default "" | toString -}}
{{- $initTag := (index (index $db "initImage" | default dict) "tag") | default "" | toString -}}
{{- $declared := .Values.psmdbVersion | default "" | toString -}}

{{/* One declared version, enforced against every knob that carries it. */}}
{{- if $declared -}}
{{- if and $opTag (ne $opTag $declared) -}}
{{- fail (printf "mongodb-store.psmdbVersion is %s but psmdb-operator.image.tag is %s. Set psmdbVersion alone and leave the tags at the chart default, or make them agree. %s" $declared $opTag $doc) -}}
{{- end -}}
{{- if and $crVersion (ne $crVersion $declared) -}}
{{- fail (printf "mongodb-store.psmdbVersion is %s but psmdb-db.crVersion is %s. Set psmdbVersion alone and leave crVersion at the chart default, or make them agree. %s" $declared $crVersion $doc) -}}
{{- end -}}
{{- if and $initTag (ne $initTag $declared) -}}
{{- fail (printf "mongodb-store.psmdbVersion is %s but psmdb-db.initImage.tag is %s. The init container runs the operator image, so its tag tracks the operator version. %s" $declared $initTag $doc) -}}
{{- end -}}
{{- end -}}

{{/* initImage is the operator image, so a different tag pulls a different operator. */}}
{{- if and $opTag $initTag (ne $opTag $initTag) -}}
{{- fail (printf "psmdb-db.initImage.tag is %s but psmdb-operator.image.tag is %s. The init container runs the operator image, so these must match or the init container runs a different operator build than the operator. %s" $initTag $opTag $doc) -}}
{{- end -}}

{{/* Percona permits upgrading only to the nearest major.minor. */}}
{{- if and (regexMatch $semver $opTag) (regexMatch $semver $crVersion) -}}
{{- $op := semver $opTag -}}
{{- $cr := semver $crVersion -}}
{{- if ne (int $op.Major) (int $cr.Major) -}}
{{- fail (printf "psmdb-operator.image.tag is %s and psmdb-db.crVersion is %s, which differ by a major version. Percona permits upgrading only to the nearest major.minor, so this must be done one step at a time. %s" $opTag $crVersion $doc) -}}
{{- end -}}
{{- $skew := sub (int $op.Minor) (int $cr.Minor) -}}
{{- if lt $skew 0 -}}
{{- fail (printf "psmdb-db.crVersion is %s but psmdb-operator.image.tag is only %s. The custom resource cannot be ahead of the operator that reconciles it. %s" $crVersion $opTag $doc) -}}
{{- end -}}
{{- if gt $skew 1 -}}
{{- fail (printf "psmdb-operator.image.tag is %s but psmdb-db.crVersion is %s, which skips %d minor versions. Percona permits upgrading only to the nearest major.minor: move the operator and crVersion up one minor at a time. %s" $opTag $crVersion $skew $doc) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}
