{{- define "nc.secretName" -}}
{{- default (printf "%s-auth" .Release.Name) .Values.auth.existingSecret -}}
{{- end -}}
{{- define "nc.claimName" -}}
{{- default (printf "%s-data" .Release.Name) .Values.persistence.existingClaim -}}
{{- end -}}
{{- define "nc.labels" -}}
app: next-chapter
app.kubernetes.io/name: next-chapter
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
