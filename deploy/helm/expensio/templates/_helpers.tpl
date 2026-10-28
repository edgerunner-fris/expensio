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

{{/*
一个 HorizontalPodAutoscaler。调用方传入 root、name 与该服务的覆盖值 map。

为什么集中在一个 helper：六个业务服务与管理台的 HPA 结构完全相同，差异只有
名称与覆盖值；复制七份模板只会在改阈值时漏改其中一份。min/max 与 CPU 目标
在这里校验——写错时 HPA 会被 API Server 拒绝或永远不扩容，提早在模板阶段
失败更容易定位。
*/}}
{{- define "expensio.hpa" -}}
{{- $root := .root -}}
{{- $as := $root.Values.autoscaling -}}
{{- $o := .override | default dict -}}
{{- $min := $as.minReplicas -}}
{{- $max := $as.maxReplicas -}}
{{- $cpu := $as.targetCPUUtilizationPercentage -}}
{{- $mem := $as.targetMemoryUtilizationPercentage -}}
{{- if hasKey $o "minReplicas" }}{{ $min = $o.minReplicas }}{{ end -}}
{{- if hasKey $o "maxReplicas" }}{{ $max = $o.maxReplicas }}{{ end -}}
{{- if hasKey $o "targetCPUUtilizationPercentage" }}{{ $cpu = $o.targetCPUUtilizationPercentage }}{{ end -}}
{{- if hasKey $o "targetMemoryUtilizationPercentage" }}{{ $mem = $o.targetMemoryUtilizationPercentage }}{{ end -}}
{{- if not $cpu -}}
{{- fail (printf "autoscaling: %s 的 targetCPUUtilizationPercentage 必须设置" .name) -}}
{{- end -}}
{{- if gt (int $min) (int $max) -}}
{{- fail (printf "autoscaling: %s 的 minReplicas(%v) 大于 maxReplicas(%v)" .name $min $max) -}}
{{- end -}}
{{- $behavior := $as.behavior -}}
{{- if hasKey $o "behavior" }}{{ $behavior = $o.behavior }}{{ end -}}
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: {{ printf "%s-%s" (include "expensio.fullname" $root) .name | trunc 63 | trimSuffix "-" }}
  labels:
    {{- include "expensio.labels" $root | nindent 4 }}
    app.kubernetes.io/component: {{ .name }}
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: {{ printf "%s-%s" (include "expensio.fullname" $root) .name | trunc 63 | trimSuffix "-" }}
  minReplicas: {{ int $min }}
  maxReplicas: {{ int $max }}
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: {{ int $cpu }}
    {{- if $mem }}
    - type: Resource
      resource:
        name: memory
        target:
          type: Utilization
          averageUtilization: {{ int $mem }}
    {{- end }}
  {{- with $behavior }}
  behavior:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- end -}}
