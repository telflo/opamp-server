{{/*
One server Deployment: the agent endpoint's (`serve agent`) or the management
API's (`serve management`). Call with (dict "ctx" . "component" "agent") or
"management". Both run the same image on the same database: an init container
runs `migrate up` as the schema's owner (a no-op when the schema is current;
it takes a lock, so pods starting together wait for each other), then `serve`
runs as the server's own role. Each container holds only its own
credentials: the owner's password never reaches serve, and the management
secret never reaches the agent endpoint's pods. A database URL's password
variable is always listed before the URL, so Kubernetes' $(VAR) expansion
fills it in.
*/}}
{{- define "telflo-opamp-server.server" -}}
{{- $ := .ctx }}
{{- $component := .component }}
{{- $agent := eq $component "agent" }}
{{- $secret := include "telflo-opamp-server.secretName" $ }}
{{- $whole := include "telflo-opamp-server.wholeUrls" $ }}
{{- $image := include "telflo-opamp-server.imageRef" (list $.Values.image $.Chart.AppVersion) }}
{{- $s := $.Values.server }}
{{- $c := index $s $component }}
{{- $tls := $c.tls.secretName }}
{{- $previousKeys := $.Values.auth.encryptionKeysPrevious.enabled }}
{{- $scheme := ternary "HTTPS" "HTTP" (not (empty $tls)) }}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ printf "%s-%s" (include "telflo-opamp-server.fullname" $) $component | quote }}
  labels:
    {{- include "telflo-opamp-server.labels" (dict "ctx" $ "component" $component) | nindent 4 }}
spec:
  replicas: {{ int $c.replicas }}
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
  selector:
    matchLabels:
      {{- include "telflo-opamp-server.selectorLabels" (dict "ctx" $ "component" $component) | nindent 6 }}
  template:
    metadata:
      labels:
        {{- include "telflo-opamp-server.labels" (dict "ctx" $ "component" $component) | nindent 8 }}
        {{- with $.Values.podLabels }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      {{- with $.Values.podAnnotations }}
      annotations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
    spec:
      serviceAccountName: {{ include "telflo-opamp-server.serviceAccountName" $ | quote }}
      automountServiceAccountToken: false
      {{- with $.Values.imagePullSecrets }}
      imagePullSecrets:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $.Values.priorityClassName }}
      priorityClassName: {{ . | quote }}
      {{- end }}
      terminationGracePeriodSeconds: {{ int $.Values.terminationGracePeriodSeconds }}
      securityContext:
        runAsNonRoot: true
        # The image's user is the name "nonroot"; Kubernetes can only check
        # runAsNonRoot against a number.
        runAsUser: 65532
        runAsGroup: 65532
        # The mounted Secret files (mode 0440) belong to this group.
        fsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      initContainers:
        {{- if $.Values.postgres.enabled }}
        # `migrate up` doesn't retry a connection, so it waits until the
        # bundled Postgres answers on TCP, which it does only once its first
        # start's bootstrap is done. It holds no credential.
        - name: wait-for-postgres
          image: {{ include "telflo-opamp-server.imageRef" (list $.Values.postgres.image "18") | quote }}
          imagePullPolicy: {{ $.Values.postgres.image.pullPolicy | quote }}
          command:
            - sh
            - -c
            - {{ printf "until pg_isready -q -h %s -p 5432 -U postgres -d postgres; do sleep 2; done" (include "telflo-opamp-server.postgresName" $) | quote }}
          {{- with $c.resources }}
          resources:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          securityContext:
            # 999, the postgres image's own user: its tools look their user up.
            {{- include "telflo-opamp-server.containerSecurityContext" 999 | nindent 12 }}
        {{- end }}
        - name: migrate
          image: {{ $image | quote }}
          imagePullPolicy: {{ $.Values.image.pullPolicy | quote }}
          args: ["migrate", "up"]
          env:
            {{- if $whole }}
            - name: OPAMP_MIGRATION_DATABASE_URL
              valueFrom:
                secretKeyRef:
                  name: {{ $secret | quote }}
                  key: {{ $.Values.externalDatabase.urlKeys.migration | quote }}
            {{- else }}
            - name: OPAMP_OWNER_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: {{ $secret | quote }}
                  key: owner-password
            - name: OPAMP_MIGRATION_DATABASE_URL
              value: {{ include "telflo-opamp-server.databaseUrl" (list $ "telflo_opamp_owner" "OPAMP_OWNER_PASSWORD") | quote }}
            {{- end }}
          {{- with $c.resources }}
          resources:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          securityContext:
            {{- include "telflo-opamp-server.containerSecurityContext" 65532 | nindent 12 }}
      containers:
        - name: serve
          image: {{ $image | quote }}
          imagePullPolicy: {{ $.Values.image.pullPolicy | quote }}
          args: ["serve", {{ $component | quote }}]
          ports:
            - name: {{ $component }}
              containerPort: {{ ternary 51234 51235 $agent }}
              protocol: TCP
          env:
            {{- if $whole }}
            - name: OPAMP_DATABASE_URL
              valueFrom:
                secretKeyRef:
                  name: {{ $secret | quote }}
                  key: {{ $.Values.externalDatabase.urlKeys.runtime | quote }}
            {{- else }}
            - name: OPAMP_RUNTIME_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: {{ $secret | quote }}
                  key: runtime-password
            - name: OPAMP_DATABASE_URL
              value: {{ include "telflo-opamp-server.databaseUrl" (list $ "telflo_opamp" "OPAMP_RUNTIME_PASSWORD") | quote }}
            {{- end }}
            {{- if not $agent }}
            - name: OPAMP_MANAGEMENT_SECRET
              valueFrom:
                secretKeyRef:
                  name: {{ $secret | quote }}
                  key: management-secret
            - name: OPAMP_MANAGEMENT_SECRETS_PREVIOUS
              valueFrom:
                secretKeyRef:
                  name: {{ $secret | quote }}
                  key: management-secrets-previous
                  optional: true
            {{- end }}
            - name: OPAMP_ENCRYPTION_KEY_FILE
              value: /run/secrets/opamp/encryption-key
            {{- if $previousKeys }}
            # During a key rotation (auth.encryptionKeysPrevious.enabled).
            - name: OPAMP_ENCRYPTION_KEYS_PREVIOUS_FILE
              value: /run/secrets/opamp/encryption-keys-previous
            {{- end }}
            {{- if $agent }}
            - name: OPAMP_SERVER_URL
              value: {{ include "telflo-opamp-server.envLiteral" $s.publicUrl }}
            {{- end }}
            {{- if $tls }}
            - name: {{ printf "OPAMP_%s_TLS_CERT_FILE" (upper $component) | quote }}
              value: {{ printf "/run/secrets/opamp-%s-tls/tls.crt" $component | quote }}
            - name: {{ printf "OPAMP_%s_TLS_KEY_FILE" (upper $component) | quote }}
              value: {{ printf "/run/secrets/opamp-%s-tls/tls.key" $component | quote }}
            {{- end }}
            {{- /* The agent endpoint's tunables reach its pods only; the drain both. */}}
            {{- $tunables := dict "shutdownDrainSeconds" "OPAMP_SHUTDOWN_DRAIN_SECONDS" }}
            {{- if $agent }}
            {{- $tunables = merge $tunables (dict "heartbeatIntervalSeconds" "OPAMP_HEARTBEAT_INTERVAL_SECONDS" "defaultRequestsPerMinute" "OPAMP_DEFAULT_REQUESTS_PER_MINUTE" "collectorRequestsPerMinute" "OPAMP_COLLECTOR_REQUESTS_PER_MINUTE" "maxConcurrentRequests" "OPAMP_MAX_CONCURRENT_REQUESTS" "maxConcurrentRequestsPerOrganization" "OPAMP_MAX_CONCURRENT_REQUESTS_PER_ORGANIZATION") }}
            {{- end }}
            {{- range $name, $variable := $tunables }}
            {{- $setting := index $s $name }}
            {{- if not (kindIs "invalid" $setting) }}
            - name: {{ $variable | quote }}
              value: {{ include "telflo-opamp-server.envLiteral" (int $setting | toString) }}
            {{- end }}
            {{- end }}
            {{- with $s.telemetry.otlpEndpoint }}
            - name: OTEL_EXPORTER_OTLP_ENDPOINT
              value: {{ include "telflo-opamp-server.envLiteral" . }}
            {{- end }}
            {{- with $s.telemetry.serviceName }}
            - name: OTEL_SERVICE_NAME
              value: {{ include "telflo-opamp-server.envLiteral" . }}
            {{- end }}
            {{- with $s.telemetry.resourceAttributes }}
            - name: OTEL_RESOURCE_ATTRIBUTES
              value: {{ include "telflo-opamp-server.envLiteral" . }}
            {{- end }}
            {{- /* Plain values literal (escaped as above); valueFrom as given. */}}
            {{- range $.Values.extraEnv }}
            - name: {{ .name | quote }}
              {{- if not (kindIs "invalid" .value) }}
              value: {{ include "telflo-opamp-server.envLiteral" .value }}
              {{- end }}
              {{- with .valueFrom }}
              valueFrom:
                {{- toYaml . | nindent 16 }}
              {{- end }}
            {{- end }}
          # Readiness: /ready pings the database with a 2-second timeout and
          # answers 503 while starting, draining or without its database, so
          # the timeout is 3 s.
          readinessProbe:
            httpGet:
              path: /ready
              port: {{ $component }}
              scheme: {{ $scheme | quote }}
            timeoutSeconds: 3
            periodSeconds: 5
            failureThreshold: 3
          # Liveness: /health touches no database, so a database outage makes
          # the pod unready, never restarted.
          livenessProbe:
            httpGet:
              path: /health
              port: {{ $component }}
              scheme: {{ $scheme | quote }}
            initialDelaySeconds: 10
            timeoutSeconds: 3
            periodSeconds: 10
            failureThreshold: 3
          volumeMounts:
            - name: encryption-key
              mountPath: /run/secrets/opamp
              readOnly: true
            {{- if $tls }}
            - name: {{ printf "%s-tls" $component }}
              mountPath: {{ printf "/run/secrets/opamp-%s-tls" $component | quote }}
              readOnly: true
            {{- end }}
          {{- with $c.resources }}
          resources:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          securityContext:
            {{- include "telflo-opamp-server.containerSecurityContext" 65532 | nindent 12 }}
      volumes:
        # Only the encryption key (and, during a rotation, the previous
        # keys) of the server's Secret: files readable by their owner and the
        # fsGroup. Neither is optional: a missing key stops the pod.
        - name: encryption-key
          secret:
            secretName: {{ $secret | quote }}
            defaultMode: 0440
            items:
              - key: encryption-key
                path: encryption-key
              {{- if $previousKeys }}
              - key: encryption-keys-previous
                path: encryption-keys-previous
              {{- end }}
        {{- if $tls }}
        - name: {{ printf "%s-tls" $component }}
          secret:
            secretName: {{ $tls | quote }}
            defaultMode: 0440
            items:
              - key: tls.crt
                path: tls.crt
              - key: tls.key
                path: tls.key
        {{- end }}
      {{- with $.Values.nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $.Values.tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $.Values.affinity }}
      affinity:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $.Values.topologySpreadConstraints }}
      topologySpreadConstraints:
        {{- toYaml . | nindent 8 }}
      {{- end }}
{{- end }}
