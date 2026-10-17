{{/*
Expand the name of the chart.
*/}}
{{- define "retycfs-csi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name, truncated to the DNS label limit.
*/}}
{{- define "retycfs-csi.fullname" -}}
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

{{- define "retycfs-csi.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
The CSI driver name is compiled into the binary and cannot be changed here.
*/}}
{{- define "retycfs-csi.driverName" -}}
csi.retycfs.com
{{- end }}

{{- define "retycfs-csi.labels" -}}
helm.sh/chart: {{ include "retycfs-csi.chart" . }}
{{ include "retycfs-csi.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "retycfs-csi.selectorLabels" -}}
app.kubernetes.io/name: {{ include "retycfs-csi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "retycfs-csi.image" -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) }}
{{- end }}

{{- define "retycfs-csi.controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.controller.create }}
{{- default (printf "%s-controller" (include "retycfs-csi.fullname" .)) .Values.serviceAccount.controller.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.controller.name }}
{{- end }}
{{- end }}

{{- define "retycfs-csi.node.serviceAccountName" -}}
{{- if .Values.serviceAccount.node.create }}
{{- default (printf "%s-node" (include "retycfs-csi.fullname" .)) .Values.serviceAccount.node.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.node.name }}
{{- end }}
{{- end }}

{{/*
Name of the Secret carrying the cluster-wide identity, or "" when the driver runs without one.
*/}}
{{- define "retycfs-csi.credentialsSecretName" -}}
{{- if .Values.credentials.existingSecret }}
{{- .Values.credentials.existingSecret }}
{{- else if and .Values.credentials.create .Values.credentials.token }}
{{- printf "%s-credentials" (include "retycfs-csi.fullname" .) }}
{{- end }}
{{- end }}

{{/*
True when at least one StorageClass uses per-namespace Secrets (the provisioner then needs to read them).
*/}}
{{- define "retycfs-csi.usesTenantSecrets" -}}
{{- $uses := false }}
{{- range .Values.storageClasses }}{{ if .tenantSecretName }}{{ $uses = true }}{{ end }}{{ end }}
{{- $uses }}
{{- end }}

{{/*
envFrom block for the cluster-wide identity, if any.
*/}}
{{- define "retycfs-csi.credentialsEnvFrom" -}}
{{- with include "retycfs-csi.credentialsSecretName" . }}
envFrom:
  - secretRef:
      name: {{ . }}
{{- end }}
{{- end }}

{{/*
The StorageClasses this release renders, as a JSON list of names: those with a tenantSecretName,
and the others only when a cluster-wide identity exists.
*/}}
{{- define "retycfs-csi.storageClassNames" -}}
{{- $global := include "retycfs-csi.credentialsSecretName" . }}
{{- $names := list }}
{{- range .Values.storageClasses }}
{{- if or .tenantSecretName $global }}{{ $names = append $names .name }}{{ end }}
{{- end }}
{{- $names | toJson }}
{{- end }}

{{/*
"true" when csi-provisioner and csi-resizer run with --leader-election: more than one controller
replica, or the flag set by hand in either sidecar's extraArgs.
*/}}
{{- define "retycfs-csi.leaderElection" -}}
{{- $c := .Values.controller }}
{{- or (gt (int $c.replicas) 1) (has "--leader-election" $c.provisioner.extraArgs) (has "--leader-election" $c.resizer.extraArgs) }}
{{- end }}

{{/*
Secret and key holding RETYC_CACHE_REDIS_URL for the node plugin, as "name/key", or "" with the
memory cache. Fails on cache.store: redis without a Redis.
*/}}
{{- define "retycfs-csi.redisURLSecret" -}}
{{- $c := .Values.cache }}
{{- if eq $c.store "redis" }}
{{- if and $c.redis.subchart.enabled $c.redis.subchart.existingSecret }}
{{- $auth := dig "auth" "existingSecret" "" (.Values.redis | default dict) }}
{{- if ne $auth $c.redis.subchart.existingSecret }}
{{- fail (printf "cache.redis.subchart.existingSecret is %q: set redis.auth.existingSecret to the same Secret (now %q), for the Redis subchart to read its password there" $c.redis.subchart.existingSecret $auth) }}
{{- end }}
{{- printf "%s/RETYC_CACHE_REDIS_URL" $c.redis.subchart.existingSecret }}
{{- else if $c.redis.subchart.enabled }}
{{- printf "%s/RETYC_CACHE_REDIS_URL" (include "retycfs-csi.redisAuthSecretName" .) }}
{{- else if $c.redis.external.existingSecret }}
{{- printf "%s/%s" $c.redis.external.existingSecret $c.redis.external.existingSecretKey }}
{{- else if $c.redis.external.url }}
{{- printf "%s/RETYC_CACHE_REDIS_URL" (include "retycfs-csi.redisAuthSecretName" .) }}
{{- else }}
{{- fail "cache.store is redis: enable cache.redis.subchart or set cache.redis.external.url or existingSecret" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Name of the Secret the chart creates for Redis (subchart password and URL, or the external URL).
Built from the release name alone: the subchart's auth.existingSecret refers to it the same way.
*/}}
{{- define "retycfs-csi.redisAuthSecretName" -}}
{{- printf "%s-redis-auth" .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Service of the Redis subchart, as the CloudPirates chart names it (cloudpirates.fullname).
*/}}
{{- define "retycfs-csi.redisSubchartHost" -}}
{{- $r := .Values.redis | default dict }}
{{- $name := "" }}
{{- if $r.fullnameOverride }}
{{- $name = $r.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $base := $r.nameOverride | default "redis" }}
{{- if contains $base .Release.Name }}
{{- $name = .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name = printf "%s-%s" .Release.Name $base | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- printf "%s.%s.svc" $name (($r.namespaceOverride | default .Release.Namespace)) }}
{{- end }}
