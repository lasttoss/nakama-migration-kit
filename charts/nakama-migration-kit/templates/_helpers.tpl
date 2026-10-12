{{- define "mkit.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "mkit.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "mkit.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "mkit.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "mkit.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "mkit.profileConfigMap" -}}
{{- if .Values.profile.existingConfigMap -}}
{{- .Values.profile.existingConfigMap -}}
{{- else -}}
{{- printf "%s-profile" (include "mkit.fullname" .) -}}
{{- end -}}
{{- end -}}

# The environment every command needs, in one place, so the importer and the reconciler cannot
# disagree about which target or which title they are talking about.
{{- define "mkit.env" -}}
- name: MIGRATION_TARGET_URL
  value: {{ .Values.nakama.url | quote }}
- name: MIGRATION_TARGET_HTTP_KEY
  value: {{ .Values.nakama.httpKey | quote }}
- name: MIGRATION_TARGET_SERVER_KEY
  value: {{ .Values.nakama.serverKey | quote }}
- name: MIGRATION_PROFILE
  value: /etc/migration/profile.json
- name: MIGRATION_DSN
  valueFrom:
    secretKeyRef:
      name: {{ required "dsn.existingSecret is required: the ledger lives in the title's own database" .Values.dsn.existingSecret }}
      key: {{ .Values.dsn.key }}
{{- if .Values.playfab.secretKeySecret.name }}
- name: PLAYFAB_SECRET_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.playfab.secretKeySecret.name }}
      key: {{ .Values.playfab.secretKeySecret.key }}
{{- end }}
{{- if .Values.playfab.fakeTenant.enabled }}
- name: MIGRATION_SOURCE
  value: faketenant
- name: MIGRATION_FAKETENANT_PLAYERS
  value: {{ .Values.playfab.fakeTenant.players | quote }}
{{- else }}
- name: MIGRATION_SOURCE
  value: playfab
- name: MIGRATION_PLAYFAB_BASE_URL
  value: {{ .Values.playfab.baseURL | quote }}
- name: MIGRATION_PLAYFAB_TITLE
  value: {{ .Values.playfab.titleID | quote }}
{{- end }}
{{- end -}}

{{- define "mkit.podSpec" -}}
serviceAccountName: {{ include "mkit.serviceAccountName" . }}
restartPolicy: Never
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  seccompProfile:
    type: RuntimeDefault
{{- end -}}
