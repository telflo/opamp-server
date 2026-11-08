{{/*
Shared snippets. A file whose name starts with "_" renders nothing itself.
*/}}

{{/* The chart's name, or nameOverride. */}}
{{- define "opamp-server.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 43 | trimSuffix "-" }}
{{- end }}

{{/*
The resources' base name: fullnameOverride, or the release's name followed by
the chart's unless it already holds it. Cut to 43 characters, so that
"<fullname>-postgres" (52) stays a StatefulSet name whose pods' revision label
fits in 63, and "<fullname>-management" (54) a Deployment's and a Service's
name.
*/}}
{{- define "opamp-server.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 43 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 43 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 43 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/* Labels on every resource. Call with (dict "ctx" . "component" "server"). */}}
{{- define "opamp-server.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .ctx.Chart.Name .ctx.Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" | quote }}
app.kubernetes.io/version: {{ .ctx.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .ctx.Release.Service | quote }}
{{ include "opamp-server.selectorLabels" . }}
{{- end }}

{{/*
The labels a workload selects its pods by, and a Service routes to. Never
change them on an installed release: a selector is immutable.
*/}}
{{- define "opamp-server.selectorLabels" -}}
app.kubernetes.io/name: {{ include "opamp-server.name" .ctx | quote }}
app.kubernetes.io/instance: {{ .ctx.Release.Name | quote }}
app.kubernetes.io/component: {{ .component | quote }}
{{- end }}

{{/* The server's ServiceAccount name. */}}
{{- define "opamp-server.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "opamp-server.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* An image reference: <repository>:<tag>[@<digest>]. Call with (list <image values> <default tag>). */}}
{{- define "opamp-server.imageRef" -}}
{{- $image := index . 0 }}
{{- $tag := default (index . 1) $image.tag }}
{{- if $image.digest }}
{{- printf "%s:%s@%s" $image.repository $tag $image.digest }}
{{- else }}
{{- printf "%s:%s" $image.repository $tag }}
{{- end }}
{{- end }}

{{/* The bundled Postgres's Service: its in-cluster host name. */}}
{{- define "opamp-server.postgresName" -}}
{{- printf "%s-postgres" (include "opamp-server.fullname" .) }}
{{- end }}

{{/* The ConfigMap holding bootstrap.sql and initdb.sh. */}}
{{- define "opamp-server.bootstrapName" -}}
{{- printf "%s-bootstrap" (include "opamp-server.fullname" .) }}
{{- end }}

{{/* The server's Secret's name (required). */}}
{{- define "opamp-server.secretName" -}}
{{- required "auth.existingSecret is required: the name of the Secret holding the server's credentials" .Values.auth.existingSecret }}
{{- end }}

{{/*
"true" when the database URLs are read whole from the server's Secret
(externalDatabase.urlKeys), "" when the chart composes them from the two
passwords.
*/}}
{{- define "opamp-server.wholeUrls" -}}
{{- if and (not .Values.postgres.enabled) .Values.externalDatabase.urlKeys.runtime .Values.externalDatabase.urlKeys.migration }}true{{ end }}
{{- end }}

{{/*
Kubernetes expands $(NAME) in a container's env values (and in its command
and args) from the variables listed before it, and reads "$$" as one "$". So
every plain env value taken from values goes through these: each "$" doubled,
the container sees the value exactly as given, and no value can name another
variable (a secret's, say). envEscape is the bare text, envLiteral the quoted
value.
*/}}
{{- define "opamp-server.envEscape" -}}
{{- toString . | replace "$" "$$" }}
{{- end }}
{{- define "opamp-server.envLiteral" -}}
{{- include "opamp-server.envEscape" . | quote }}
{{- end }}

{{/*
A database URL the chart composes, with Kubernetes' $(VAR) expansion of the
password variable listed before it in the same container: no password is ever
rendered. That reference is the chart's one intended expansion; the parts
taken from values are escaped. Call with (list . <role> <password variable>).
*/}}
{{- define "opamp-server.databaseUrl" -}}
{{- $ := index . 0 }}
{{- $role := index . 1 }}
{{- $variable := index . 2 }}
{{- if $.Values.postgres.enabled }}
{{- printf "postgresql://%s:$(%s)@%s:5432/%s?sslmode=disable" $role $variable (include "opamp-server.envEscape" (include "opamp-server.postgresName" $)) (include "opamp-server.envEscape" $.Values.postgres.database) }}
{{- else }}
{{- $db := $.Values.externalDatabase }}
{{- printf "postgresql://%s:$(%s)@%s:%d/%s?sslmode=%s" $role $variable (include "opamp-server.envEscape" $db.host) (int $db.port) (include "opamp-server.envEscape" $db.database) (include "opamp-server.envEscape" $db.sslmode) }}
{{- end }}
{{- end }}

{{/*
The guards: every value the templates can't check through the schema. Each
fails the render with a fixed message naming the value, never quoting it.
Every template includes it, so rendering any one of them alone (helm template
--show-only) is refused the same way.
*/}}
{{- define "opamp-server.guards" -}}
{{- $v := .Values }}
{{- $_ := include "opamp-server.secretName" . }}
{{- $_ = required "server.publicUrl is required: the agent endpoint's URL as collectors reach it, such as https://opamp.example.com/opamp/api/v1" $v.server.publicUrl }}
{{- if $v.postgres.enabled }}
{{- $_ = required "postgres.auth.existingSecret is required while postgres.enabled is true: the name of the Secret holding postgres-password" $v.postgres.auth.existingSecret }}
{{- else }}
{{- $keys := $v.externalDatabase.urlKeys }}
{{- if or (and $keys.runtime (not $keys.migration)) (and $keys.migration (not $keys.runtime)) }}
{{- fail "externalDatabase.urlKeys.runtime and externalDatabase.urlKeys.migration must be set together, or neither" }}
{{- end }}
{{- if not (include "opamp-server.wholeUrls" .) }}
{{- $_ = required "externalDatabase.host is required while postgres.enabled is false" $v.externalDatabase.host }}
{{- $_ = required "externalDatabase.database is required while postgres.enabled is false" $v.externalDatabase.database }}
{{- $_ = required "externalDatabase.port is required while postgres.enabled is false" $v.externalDatabase.port }}
{{- $_ = required "externalDatabase.sslmode is required while postgres.enabled is false" $v.externalDatabase.sslmode }}
{{- end }}
{{- end }}
{{- $drain := 5 }}
{{- if not (kindIs "invalid" $v.server.shutdownDrainSeconds) }}{{ $drain = int $v.server.shutdownDrainSeconds }}{{ end }}
{{- if lt (int $v.terminationGracePeriodSeconds) (add $drain 22) }}
{{- fail "terminationGracePeriodSeconds must be at least server.shutdownDrainSeconds plus 22" }}
{{- end }}
{{- $agent := $v.service.agent }}
{{- if and (not (kindIs "invalid" $agent.nodePort)) (eq $agent.type "ClusterIP") }}
{{- fail "service.agent.nodePort needs service.agent.type NodePort or LoadBalancer" }}
{{- end }}
{{- if and $agent.loadBalancerSourceRanges (ne $agent.type "LoadBalancer") }}
{{- fail "service.agent.loadBalancerSourceRanges needs service.agent.type LoadBalancer" }}
{{- end }}
{{- range $component := list "agent" "management" }}
{{- $pdb := (index $v.server $component).podDisruptionBudget }}
{{- if $pdb.enabled }}
{{- $min := not (kindIs "invalid" $pdb.minAvailable) }}
{{- $max := not (kindIs "invalid" $pdb.maxUnavailable) }}
{{- if eq $min $max }}
{{- fail (printf "server.%s.podDisruptionBudget needs exactly one of minAvailable and maxUnavailable" $component) }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}

{{/* A PodDisruptionBudget's number (an int) or percentage (a quoted string). */}}
{{- define "opamp-server.intOrString" -}}
{{- if kindIs "string" . }}{{ quote . }}{{ else }}{{ int . }}{{ end }}
{{- end }}

{{/* The locked-down container security context. */}}
{{- define "opamp-server.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
runAsNonRoot: true
runAsUser: {{ int . }}
runAsGroup: {{ int . }}
capabilities:
  drop: ["ALL"]
{{- end }}
