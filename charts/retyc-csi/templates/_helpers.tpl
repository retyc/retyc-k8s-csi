{{/*
Expand the name of the chart.
*/}}
{{- define "retyc-csi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name, truncated to the DNS label limit.
*/}}
{{- define "retyc-csi.fullname" -}}
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

{{- define "retyc-csi.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
The CSI driver name is compiled into the binary and cannot be changed here.
*/}}
{{- define "retyc-csi.driverName" -}}
csi.retyc.com
{{- end }}

{{- define "retyc-csi.labels" -}}
helm.sh/chart: {{ include "retyc-csi.chart" . }}
{{ include "retyc-csi.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "retyc-csi.selectorLabels" -}}
app.kubernetes.io/name: {{ include "retyc-csi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "retyc-csi.image" -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) }}
{{- end }}

{{- define "retyc-csi.controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.controller.create }}
{{- default (printf "%s-controller" (include "retyc-csi.fullname" .)) .Values.serviceAccount.controller.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.controller.name }}
{{- end }}
{{- end }}

{{- define "retyc-csi.node.serviceAccountName" -}}
{{- if .Values.serviceAccount.node.create }}
{{- default (printf "%s-node" (include "retyc-csi.fullname" .)) .Values.serviceAccount.node.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.node.name }}
{{- end }}
{{- end }}

{{/*
Name of the Secret carrying the cluster-wide identity, or "" when the driver runs without one.
*/}}
{{- define "retyc-csi.credentialsSecretName" -}}
{{- if .Values.credentials.existingSecret }}
{{- .Values.credentials.existingSecret }}
{{- else if and .Values.credentials.create .Values.credentials.token }}
{{- printf "%s-credentials" (include "retyc-csi.fullname" .) }}
{{- end }}
{{- end }}

{{/*
True when at least one StorageClass uses per-namespace Secrets (the provisioner then needs to read them).
*/}}
{{- define "retyc-csi.usesTenantSecrets" -}}
{{- $uses := false }}
{{- range .Values.storageClasses }}{{ if .tenantSecretName }}{{ $uses = true }}{{ end }}{{ end }}
{{- $uses }}
{{- end }}

{{/*
envFrom block for the cluster-wide identity, if any.
*/}}
{{- define "retyc-csi.credentialsEnvFrom" -}}
{{- with include "retyc-csi.credentialsSecretName" . }}
envFrom:
  - secretRef:
      name: {{ . }}
{{- end }}
{{- end }}
