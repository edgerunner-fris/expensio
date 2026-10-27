{{/*
基础命名与标签。规则与 helm create 生成的一致，避免 release 名与 chart 名
重复时出现 expensio-expensio。
*/}}
{{- define "expensio.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "expensio.fullname" -}}
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

{{- define "expensio.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "expensio.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: expensio
{{- end -}}

{{- define "expensio.selectorLabels" -}}
app.kubernetes.io/name: {{ include "expensio.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* 服务名：<fullname>-<service>，同时用于 Service、Deployment 与上游地址。 */}}
{{- define "expensio.serviceName" -}}
{{- printf "%s-%s" (include "expensio.fullname" .root) .name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "expensio.image" -}}
{{- printf "%s/%s:%s" .root.Values.images.registry .name .root.Values.images.tag -}}
{{- end -}}
