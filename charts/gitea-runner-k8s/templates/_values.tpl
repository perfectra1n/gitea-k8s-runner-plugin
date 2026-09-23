{{/*
bjw-s common values computed from this chart's own values. Keys the user sets
directly in bjw-s form are merged on top by common.yaml.
*/}}

{{- define "gitea-runner-k8s.fullname" -}}
{{- include "bjw-s.common.lib.chart.names.fullname" . -}}
{{- end -}}

{{- define "gitea-runner-k8s.jobNamespace" -}}
{{- default .Release.Namespace .Values.jobNamespace -}}
{{- end -}}

{{- define "gitea-runner-k8s.socket" -}}unix:///run/plugin/plugin.sock{{- end -}}

{{- define "gitea-runner-k8s.cacheHost" -}}
{{- printf "%s-cache.%s.svc.%s" (include "gitea-runner-k8s.fullname" .) .Release.Namespace .Values.clusterDomain -}}
{{- end -}}

{{/* Runner labels: one per class and alias, all routed to the k8s plugin. */}}
{{- define "gitea-runner-k8s.labels" -}}
{{- $labels := list -}}
{{- range .Values.classes -}}
  {{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" .name) -}}
    {{- fail (printf "classes: %q is not a valid class name (lowercase DNS label)" .name) -}}
  {{- end -}}
  {{- $path := printf "/podspecs/%s/podspec.yaml" .name -}}
  {{- $labels = append $labels (printf "%s:k8s:%s" .name $path) -}}
  {{- range .aliases -}}
    {{- $labels = append $labels (printf "%s:k8s:%s" . $path) -}}
  {{- end -}}
{{- end -}}
{{- $labels = concat $labels .Values.runner.extraLabels -}}
{{- toJson $labels -}}
{{- end -}}

{{- define "gitea-runner-k8s.runnerConfig" -}}
{{- $cfg := dict
  "log" (dict "level" .Values.runner.logLevel)
  "runner" (dict
    "capacity" .Values.runner.capacity
    "timeout" .Values.runner.timeout
    "labels" (include "gitea-runner-k8s.labels" . | fromJsonArray))
  "cache" (dict
    "enabled" .Values.cache.enabled
    "dir" "/data/cache"
    "host" (include "gitea-runner-k8s.cacheHost" .)
    "port" .Values.cache.port)
  "plugins" (dict "k8s" (dict
    "address" (include "gitea-runner-k8s.socket" .)
    "options" .Values.plugin.options))
-}}
{{- toYaml (mustMergeOverwrite $cfg .Values.runner.config) -}}
{{- end -}}

{{- define "gitea-runner-k8s.values" -}}
{{- $fullname := include "gitea-runner-k8s.fullname" . -}}
{{- $jobNs := include "gitea-runner-k8s.jobNamespace" . -}}
{{- if not .Values.gitea.url }}{{ fail "gitea.url is required" }}{{ end -}}
{{- if and (not .Values.gitea.token) (not .Values.gitea.existingSecret) }}{{ fail "set gitea.token or gitea.existingSecret" }}{{ end -}}
{{- if not .Values.classes }}{{ fail "classes: define at least one job class" }}{{ end -}}
serviceAccount:
  main: {}

controllers:
  main:
    type: deployment
    replicas: 1
    strategy: Recreate
    serviceAccount:
      identifier: main
    initContainers:
      plugin:
        image:
          repository: {{ .Values.plugin.image.repository }}
          tag: {{ .Values.plugin.image.tag | default .Chart.AppVersion | quote }}
          pullPolicy: {{ .Values.plugin.image.pullPolicy }}
        # Native sidecar: up before the runner, stopped after it.
        restartPolicy: Always
        args:
          - --listen={{ include "gitea-runner-k8s.socket" . }}
          - --namespace={{ $jobNs }}
          # Stable across pod restarts, so a new pod sweeps the old one's orphans.
          - --instance={{ $fullname }}
          - --ready-timeout={{ .Values.plugin.options.ready_timeout | default "10m" }}
        {{- with .Values.plugin.resources }}
        resources: {{- toYaml . | nindent 10 }}
        {{- end }}
        securityContext:
          allowPrivilegeEscalation: false
          readOnlyRootFilesystem: true
          capabilities: {drop: [ALL]}
    containers:
      main:
        image:
          repository: {{ .Values.runner.image.repository }}
          tag: {{ .Values.runner.image.tag | default .Chart.AppVersion | quote }}
          pullPolicy: {{ .Values.runner.image.pullPolicy }}
        env:
          CONFIG_FILE: /config/config.yaml
          GITEA_INSTANCE_URL: {{ .Values.gitea.url | quote }}
          GITEA_RUNNER_NAME: {{ .Values.runner.name | default $fullname | quote }}
          GITEA_RUNNER_REGISTRATION_TOKEN:
            valueFrom:
              secretKeyRef:
                name: {{ .Values.gitea.existingSecret | default (printf "%s-registration" $fullname) }}
                key: {{ if .Values.gitea.existingSecret }}{{ .Values.gitea.existingSecretKey }}{{ else }}token{{ end }}
        {{- if .Values.cache.enabled }}
        ports:
          - name: cache
            containerPort: {{ .Values.cache.port }}
        {{- end }}
        {{- with .Values.runner.resources }}
        resources: {{- toYaml . | nindent 10 }}
        {{- end }}

{{- if .Values.cache.enabled }}
service:
  cache:
    controller: main
    suffix: cache
    ports:
      http:
        port: {{ .Values.cache.port }}
        targetPort: {{ .Values.cache.port }}
{{- end }}

{{- if not .Values.gitea.existingSecret }}
secrets:
  registration:
    suffix: registration
    stringData:
      token: {{ .Values.gitea.token | quote }}
{{- end }}

configMaps:
  config:
    suffix: config
    data:
      config.yaml: |
        {{- include "gitea-runner-k8s.runnerConfig" . | nindent 8 }}
  {{- range .Values.classes }}
  podspec-{{ .name }}:
    suffix: podspec-{{ .name }}
    data:
      podspec.yaml: |
        {{- toYaml .podspec | nindent 8 }}
  {{- end }}

persistence:
  socket:
    type: emptyDir
    advancedMounts:
      main:
        main: [{path: /run/plugin}]
        plugin: [{path: /run/plugin}]
  data:
    {{- if .Values.data.existingClaim }}
    existingClaim: {{ .Values.data.existingClaim }}
    {{- else }}
    type: persistentVolumeClaim
    suffix: data
    size: {{ .Values.data.size }}
    accessMode: {{ .Values.data.accessMode }}
    {{- with .Values.data.storageClass }}
    storageClass: {{ . }}
    {{- end }}
    {{- end }}
    advancedMounts:
      main:
        main: [{path: /data}]
  config:
    type: configMap
    identifier: config
    advancedMounts:
      main:
        main: [{path: /config, readOnly: true}]
  {{- range .Values.classes }}
  podspec-{{ .name }}:
    type: configMap
    identifier: podspec-{{ .name }}
    advancedMounts:
      main:
        plugin: [{path: /podspecs/{{ .name }}, readOnly: true}]
  {{- end }}

# Namespaced where the job pods run, which may differ from the release
# namespace, so these go through rawResources rather than rbac.
rawResources:
  job-role:
    forceRename: {{ $fullname }}-jobs
    manifest:
      apiVersion: rbac.authorization.k8s.io/v1
      kind: Role
      metadata:
        namespace: {{ $jobNs }}
      rules:
        - apiGroups: [batch]
          resources: [jobs]
          verbs: [create, get, list, watch, delete]
        - apiGroups: [""]
          resources: [pods]
          verbs: [get, list, watch]
        - apiGroups: [""]
          resources: [pods/exec]
          verbs: [create, get]
        - apiGroups: [""]
          resources: [pods/log]
          verbs: [get]
        - apiGroups: [""]
          resources: [events]
          verbs: [list, watch]
  job-rolebinding:
    forceRename: {{ $fullname }}-jobs
    manifest:
      apiVersion: rbac.authorization.k8s.io/v1
      kind: RoleBinding
      metadata:
        namespace: {{ $jobNs }}
      roleRef:
        apiGroup: rbac.authorization.k8s.io
        kind: Role
        name: {{ $fullname }}-jobs
      subjects:
        - kind: ServiceAccount
          name: {{ $fullname }}
          namespace: {{ .Release.Namespace }}
{{- end -}}
