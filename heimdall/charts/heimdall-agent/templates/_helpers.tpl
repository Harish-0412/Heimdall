{{/* Chart name, truncated to fit Kubernetes name limits. */}}
{{- define "heimdall-agent.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified name. Kept to 40 characters so derived names
("<fullname>-preview-manager", "<fullname>-namespaces") stay under 63.
*/}}
{{- define "heimdall-agent.fullname" -}}
{{- if contains .Chart.Name .Release.Name }}
{{- .Release.Name | trunc 40 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 40 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "heimdall-agent.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "heimdall-agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "heimdall-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "heimdall-agent.labels" -}}
helm.sh/chart: {{ include "heimdall-agent.chart" . }}
{{ include "heimdall-agent.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: heimdall
{{- end }}

{{- define "heimdall-agent.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "heimdall-agent.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- required "serviceAccount.name is required when serviceAccount.create is false" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* The ClusterRole the agent binds inside each preview namespace. */}}
{{- define "heimdall-agent.previewManager" -}}
{{- printf "%s-preview-manager" (include "heimdall-agent.fullname" .) }}
{{- end }}

{{- define "heimdall-agent.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}
{{- end }}

{{/* The agent's username as admission sees it. */}}
{{- define "heimdall-agent.username" -}}
{{- printf "system:serviceaccount:%s:%s" .Release.Namespace (include "heimdall-agent.serviceAccountName" .) }}
{{- end }}

{{/* CEL: the given object is a preview namespace (prefix and label). */}}
{{- define "heimdall-agent.celIsPreview" -}}
{{ .obj }}.metadata.name.startsWith(variables.prefix) && has({{ .obj }}.metadata.labels) && 'heimdall.dev/preview' in {{ .obj }}.metadata.labels && {{ .obj }}.metadata.labels['heimdall.dev/preview'] == 'true'
{{- end }}
