# The opamp-server chart

Installs the opamp-server on Kubernetes: the agent endpoint, which
OpenTelemetry Collectors (through the OpAMP supervisor) poll for their
configuration, and the management API, behind a shared secret. By default
it also runs its own Postgres, so one `helm install` gives a working
server; or it uses a Postgres you run.

The repository's [README](https://github.com/telflo/opamp-server#readme)
and [docs](https://github.com/telflo/opamp-server/tree/main/docs) describe
the server itself. This page covers what the chart adds.

## What it installs

- **Two Deployments**, one per component, on the same image and database,
  each with its own replicas, resources and disruption budget:
  - `<fullname>-agent` runs `serve agent`, the agent endpoint. Its pods
    never hold the management secret.
  - `<fullname>-management` runs `serve management`, the management API.

  Each pod runs `migrate up` before it serves (a no-op when the schema is
  current), so either Deployment can run alone. With the bundled Postgres,
  it first waits for Postgres to answer. Readiness is `/ready` and liveness
  `/health`, on the pod's own port. `replicas: 0` stops a component: while
  the agent endpoint is stopped, collectors keep running their last
  configuration; while the management API is, its callers are refused.
- **Two Services:** `<fullname>-agent` (port 51234 by default; its type is
  a value) and `<fullname>-management` (port 51235, always `ClusterIP`).
- **The bundled Postgres** (`postgres.enabled`, default `true`): a
  StatefulSet of `postgres:18`, pinned by digest, with one volume claim and
  a headless Service, `<fullname>-postgres`. It runs the bootstrap (the
  database and its roles, `deploy/bootstrap.sql`) once, in its own pod,
  when it first creates its data, so the superuser's password never leaves
  that pod.
- **A ConfigMap**, `<fullname>-bootstrap`, with `bootstrap.sql` and
  `initdb.sh`, which runs it. Always rendered.
- **A ServiceAccount** (`serviceAccount.create`), with no token mounted:
  the server calls no Kubernetes API.
- Optionally, **NetworkPolicies** and **a PodDisruptionBudget** per
  component.

It never renders a Secret, a Job or hook, an Ingress or an autoscaler, and
writes no annotation of its own. Every container runs as a non-root user,
with a read-only root filesystem, every capability dropped, no privilege
escalation and the `RuntimeDefault` seccomp profile.

## Install

Two steps: create the Secrets, then install.

**1. The Secrets.** The chart reads every credential from Secrets you
create, so no password or key is ever in a values file, the rendered
manifests or Helm's release record. Make each value with
`openssl rand -hex 32`: hex is safe in the database URLs the chart builds.

```sh
mkdir opamp-secrets && chmod 700 opamp-secrets && cd opamp-secrets
for name in management-secret encryption-key owner-password runtime-password postgres-password; do
  openssl rand -hex 32 | tr -d '\n' >"$name"
done

kubectl create namespace opamp
kubectl -n opamp create secret generic opamp-server \
  --from-file=management-secret --from-file=encryption-key \
  --from-file=owner-password --from-file=runtime-password
kubectl -n opamp create secret generic opamp-server-postgres \
  --from-file=postgres-password
```

**Back up `encryption-key` outside the cluster.** Without it, the
enrollment tokens the server stores can't be read again (the server's
[security.md](https://github.com/telflo/opamp-server/blob/main/docs/security.md), "The encryption key"). Give `management-secret` to your management
client (your platform, your tooling). Then delete the folder, or keep it
somewhere safe.

**2. The chart:**

```sh
helm install opamp oci://ghcr.io/telflo/charts/opamp-server --version 0.2.4 -n opamp \
  --set auth.existingSecret=opamp-server \
  --set postgres.auth.existingSecret=opamp-server-postgres \
  --set server.publicUrl=https://opamp.example.com/opamp/api/v1
```

`server.publicUrl` is the agent endpoint's URL as collectors reach it:
each collector is handed it when it enrolls and polls it from then on.
The install refuses to render without it, or without either Secret's
name.

For an image in a registry that needs credentials, create a
`kubernetes.io/dockerconfigjson` Secret and name it in `imagePullSecrets`.

**While the packages are private**, both need credentials: run
`helm registry login ghcr.io` (with `--password-stdin`) before
`helm install`, and give the cluster a pull secret for
`ghcr.io/telflo/opamp-server` in `imagePullSecrets`. Once they are
public, you need neither.

## The Secrets it reads

**The server's Secret** (`auth.existingSecret`, required):

| Key | | Read by |
|---|---|---|
| `management-secret` | required | the management API (`OPAMP_MANAGEMENT_SECRET`): at least 32 bytes |
| `encryption-key` | required | both components, as a file (`OPAMP_ENCRYPTION_KEY_FILE`) |
| `owner-password` | required while the chart builds the URLs | `migrate`, as the schema owner `telflo_opamp_owner`, and the bundled Postgres's bootstrap |
| `runtime-password` | required while the chart builds the URLs | both components' `serve`, as `telflo_opamp`, and the bundled Postgres's bootstrap |
| `management-secrets-previous` | optional | the management API (`OPAMP_MANAGEMENT_SECRETS_PREVIOUS`), during a rotation |
| `encryption-keys-previous` | required while `auth.encryptionKeysPrevious.enabled` | both components, as a file (`OPAMP_ENCRYPTION_KEYS_PREVIOUS_FILE`), during a key rotation |

The chart builds the database URLs from the two passwords, unless an
external Postgres's `externalDatabase.urlKeys` names keys holding the
whole URLs (below). The passwords must be URL-safe (only
`A-Z a-z 0-9 . _ ~ -`): hex is. Each container gets only its own
credentials: the owner's password never reaches `serve`, and the
management secret never reaches the agent endpoint's pods.

**The bundled Postgres's Secret** (`postgres.auth.existingSecret`,
required while `postgres.enabled`): `postgres-password`, the Postgres
superuser's.

**TLS Secrets** (optional, `server.agent.tls.secretName`,
`server.management.tls.secretName`): `kubernetes.io/tls` Secrets, with
`tls.crt` (the certificate, its chain after it) and `tls.key`.

A missing required key stops the pod (`CreateContainerConfigError`).

## Values

| Value | Default | What it is |
|---|---|---|
| `nameOverride` | `""` | Replaces the chart's name in resource names and the `app.kubernetes.io/name` label. |
| `fullnameOverride` | `""` | The resources' base name (`<fullname>`), at most 43 characters. Default: the release's name, followed by `-opamp-server` unless it already holds it. |
| `image.repository` | `ghcr.io/telflo/opamp-server` | The server's image. |
| `image.tag` | `""` | Its tag; empty is the chart's `appVersion`. |
| `image.digest` | `""` | `sha256:…`, pinning the image as `<repository>:<tag>@<digest>`. |
| `image.pullPolicy` | `IfNotPresent` | `Always`, `IfNotPresent` or `Never`. |
| `imagePullSecrets` | `[]` | Pull secrets for every pod, by name: `[{name: my-pull-secret}]`. |
| `server.agent.replicas`, `server.management.replicas` | `1` | That component's pods. `0` stops it. |
| `server.agent.resources`, `server.management.resources` | `{}` | Requests and limits for that component's containers, its init containers included. |
| `server.agent.podDisruptionBudget.enabled`, `server.management.podDisruptionBudget.enabled` | `false` | Render a PodDisruptionBudget for that component. |
| `server.agent.podDisruptionBudget.minAvailable`, `server.management.podDisruptionBudget.minAvailable` | `null` | A number or a percentage; set exactly one of the two. |
| `server.agent.podDisruptionBudget.maxUnavailable`, `server.management.podDisruptionBudget.maxUnavailable` | `1` | A number or a percentage. |
| `terminationGracePeriodSeconds` | `30` | At least 30, and at least `server.shutdownDrainSeconds` plus 22, the time `serve` needs after it drains. The chart refuses less. |
| `auth.existingSecret` | `""` | **Required.** The server's Secret ([above](#the-secrets-it-reads)). |
| `auth.encryptionKeysPrevious.enabled` | `false` | During an encryption key rotation: mount the Secret's `encryption-keys-previous` ([Rotating a secret](#rotating-a-secret)). |
| `server.publicUrl` | `""` | **Required.** The agent endpoint's URL as collectors reach it (`OPAMP_SERVER_URL`): `http` or `https`. |
| `server.agent.tls.secretName` | `""` | A `kubernetes.io/tls` Secret: the agent endpoint, and its probes, use HTTPS. Empty: plain HTTP, for a load balancer that terminates TLS. |
| `server.management.tls.secretName` | `""` | The same, for the management API. |
| `server.heartbeatIntervalSeconds` | `null` | `OPAMP_HEARTBEAT_INTERVAL_SECONDS`, 10 to 300; `null` is the server's default (30). |
| `server.defaultRequestsPerMinute` | `null` | `OPAMP_DEFAULT_REQUESTS_PER_MINUTE`, 1 to 12,000,000 (default 12,000). |
| `server.collectorRequestsPerMinute` | `null` | `OPAMP_COLLECTOR_REQUESTS_PER_MINUTE`, 1 to 600 (default 60). |
| `server.maxConcurrentRequests` | `null` | `OPAMP_MAX_CONCURRENT_REQUESTS`, 1 to 10,000 (default 100). |
| `server.maxConcurrentRequestsPerOrganization` | `null` | `OPAMP_MAX_CONCURRENT_REQUESTS_PER_ORGANIZATION`, 1 to the pod's cap (default 25). |
| `server.shutdownDrainSeconds` | `null` | `OPAMP_SHUTDOWN_DRAIN_SECONDS`, 0 to 30 (default 5). |
| `server.telemetry.otlpEndpoint` | `""` | `OTEL_EXPORTER_OTLP_ENDPOINT` (OTLP over gRPC); telemetry is off while empty. |
| `server.telemetry.serviceName` | `""` | `OTEL_SERVICE_NAME`. |
| `server.telemetry.resourceAttributes` | `""` | `OTEL_RESOURCE_ATTRIBUTES`. |
| `extraEnv` | `[]` | More variables for both components' `serve`, after the chart's: `{name, value}`, or, for anything secret, `{name, valueFrom: {secretKeyRef: …}}`. A `value` is literal: a `$(…)` in it isn't expanded. |
| `podAnnotations` | `{}` | Annotations on both components' pods. |
| `podLabels` | `{}` | More labels on both components' pods. The selector's own keys, `app.kubernetes.io/name`, `instance` and `component`, are refused. |
| `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints` | empty | Both components' pods' scheduling. To match one component's pods, select `app.kubernetes.io/component: agent` or `management`. |
| `priorityClassName` | `""` | The server's pods' priority class. |
| `serviceAccount.create` | `true` | Create the server's ServiceAccount. |
| `serviceAccount.name` | `""` | Its name (empty: `<fullname>`); with `create: false`, an existing account (empty: `default`). |
| `serviceAccount.annotations` | `{}` | Annotations on it, such as a workload identity binding. |
| `service.agent.type` | `ClusterIP` | `ClusterIP`, `NodePort` or `LoadBalancer`. |
| `service.agent.port` | `51234` | The Service's port. |
| `service.agent.nodePort` | `null` | With `NodePort` or `LoadBalancer`: a fixed node port. |
| `service.agent.loadBalancerSourceRanges` | `[]` | With `LoadBalancer`: the client CIDRs allowed in. |
| `service.agent.annotations` | `{}` | Annotations on the agent Service, such as your load balancer controller's. |
| `service.management.annotations` | `{}` | Annotations on the management Service, which is always `ClusterIP`, port 51235. |
| `networkPolicy.enabled` | `false` | Render NetworkPolicies: the agent port is open to all, the management port only to `networkPolicy.management.from`, and the bundled Postgres only to both components' pods. They take effect only where the cluster's network plugin enforces them. |
| `networkPolicy.management.from` | `[]` | NetworkPolicy peers (`podSelector`, `namespaceSelector`, `ipBlock`) allowed to the management port. Empty: none, not even pods in the same namespace. |
| `postgres.enabled` | `true` | Run the bundled Postgres. `false`: use `externalDatabase`. |
| `postgres.image.repository` | `postgres` | |
| `postgres.image.tag` | `"18"` | |
| `postgres.image.digest` | `sha256:edb0c08d…` | The `postgres:18` (18.6) index digest. Empty: the tag alone, for an image loaded into the cluster by hand. |
| `postgres.image.pullPolicy` | `IfNotPresent` | |
| `postgres.auth.existingSecret` | `""` | **Required while enabled.** Its Secret, with `postgres-password`. |
| `postgres.database` | `telflo_opamp` | The database the bootstrap creates. |
| `postgres.persistence.size` | `8Gi` | Its volume claim's size (ReadWriteOnce). |
| `postgres.persistence.storageClassName` | `""` | Empty: the cluster's default storage class. |
| `postgres.resources`, `postgres.podAnnotations`, `postgres.nodeSelector`, `postgres.tolerations`, `postgres.affinity` | empty | The Postgres pod's resources, annotations and scheduling. |
| `externalDatabase.host` | `""` | **Required while `postgres.enabled` is false** (unless `urlKeys`): a host name or IP address. |
| `externalDatabase.port` | `5432` | |
| `externalDatabase.database` | `telflo_opamp` | |
| `externalDatabase.sslmode` | `""` | **Required likewise:** libpq's `sslmode` (`disable`, `allow`, `prefer`, `require`, `verify-ca` or `verify-full`). |
| `externalDatabase.urlKeys.runtime`, `.migration` | `""` | Keys of the server's Secret holding the whole URLs (both, or neither): then the passwords aren't read and the four values above aren't used. |

`values.schema.json` refuses an unknown key (a typo), a port out of range
and a value outside its list. It also accepts `global` and `enabled` at the
root, which a parent chart sets.

## External Postgres

With `postgres.enabled: false` the server uses a Postgres you run, 16 or
later. Run `deploy/bootstrap.sql` in it once first, as a superuser (the
server's [database.md](https://github.com/telflo/opamp-server/blob/main/docs/database.md), "Bootstrap the database and roles",
which also covers a managed Postgres without a superuser). `serve` checks
the result at every start and refuses a role or a database set up more
loosely.

```sh
helm install opamp oci://ghcr.io/telflo/charts/opamp-server --version 0.2.4 -n opamp \
  --set auth.existingSecret=opamp-server \
  --set postgres.enabled=false \
  --set externalDatabase.host=db.internal \
  --set externalDatabase.sslmode=verify-full \
  --set server.publicUrl=https://opamp.example.com/opamp/api/v1
```

You don't need the Postgres Secret. The chart builds
`postgresql://telflo_opamp_owner:…@<host>:<port>/<database>?sslmode=<sslmode>`
for `migrate`, and the same with `telflo_opamp` for `serve`. For anything
else (another role name, an IPv6 address, or parameters such as
`pool_max_conns`), put the whole URLs in the Secret and name their keys:

```sh
kubectl -n opamp create secret generic opamp-server \
  --from-file=management-secret --from-file=encryption-key \
  --from-file=runtime-url --from-file=migration-url
helm install … --set externalDatabase.urlKeys.runtime=runtime-url \
  --set externalDatabase.urlKeys.migration=migration-url
```

**A private CA.** This release of the chart can't mount a CA certificate
(`sslrootcert`). `verify-full` works only when the database's certificate
chains to a public root in the image's own certificate store.

## Reaching the management API

The management API's shared secret opens every organization, so the chart
never exposes it outside the cluster: its Service is always `ClusterIP`.
Your management client reaches it at
`http://<fullname>-management.<namespace>.svc:51235` (`https` with
`server.management.tls.secretName`). With `networkPolicy.enabled`, list
that client in `networkPolicy.management.from`.

From your machine, for a look:

```sh
kubectl -n opamp port-forward svc/opamp-opamp-server-management 51235:51235
curl -sS http://127.0.0.1:51235/health
```

If a client outside the cluster must reach it, create a Service of your
own selecting the management API's pods
(`app.kubernetes.io/instance: <release>`,
`app.kubernetes.io/component: management`) on `targetPort: management`.
Keep it on a private network, with TLS when it crosses one you don't
trust. Never put it on the internet.

## The agent endpoint behind a load balancer

Collectors poll `POST /opamp/api/v1` on the agent port. To take it outside
the cluster, set `service.agent.type: LoadBalancer` (or `NodePort`) and
your load balancer controller's annotations in `service.agent.annotations`.
Terminate TLS at the load balancer, or give the pods a certificate with
`server.agent.tls.secretName`.

The kubelet probes `/ready` on each pod, which takes a pod out of the
Service while it starts, drains or loses its database. Keep `/ready` off a
public load balancer's health check: check the port alone (a TCP check).

## Rotating a secret

A pod reads its Secrets only when it starts, and the chart never sees
their values, so restart the pods after you change one:

```sh
kubectl -n opamp rollout restart deployment/opamp-opamp-server-agent deployment/opamp-opamp-server-management
```

The server's [security.md](https://github.com/telflo/opamp-server/blob/main/docs/security.md) describes each rotation. With the chart:

- **The management secret:** put the old secret in
  `management-secrets-previous` (comma-separated, for several) and the new
  one in `management-secret`, and restart. Once the restart has finished,
  switch callers to the new secret. Once they all send it, remove
  `management-secrets-previous` and restart again.
- **The encryption key.** Every running pod must hold every key another
  pod mints with, and a rolling restart runs old and new pods side by
  side. So, in this order:
  1. Put the new key in the Secret as `encryption-keys-previous`, leaving
     `encryption-key` as it is, and set
     `auth.encryptionKeysPrevious.enabled: true` with `helm upgrade`, which
     restarts the pods.
  2. In one change to the Secret, put the new key in `encryption-key` and
     the old one in `encryption-keys-previous`, then `kubectl rollout
     restart`. New bearers use the new key, and every pod opens both.
  3. Give collectors bearers under the new key (the server's
     [security.md](https://github.com/telflo/opamp-server/blob/main/docs/security.md), "Rotating it"). When no collector presents an old
     bearer, set the value back to `false` with another `helm upgrade`,
     then remove `encryption-keys-previous` from the Secret.

  While the value is `true`, a missing `encryption-keys-previous` stops
  the pod.
- **A database password:** run the bootstrap again with the new one
  (below), then restart.

## Upgrading

Read the release's notes, and **back up the database before every
upgrade**: `migrate up` runs as soon as the new pods start, and rolling a
release back means restoring that backup.

```sh
helm upgrade opamp oci://ghcr.io/telflo/charts/opamp-server --version <version> -n opamp -f my-values.yaml
```

Here `my-values.yaml` holds the values you installed with.

**When a release changes `bootstrap.sql`** (its notes say so), run it
again in the bundled Postgres after the upgrade. It is idempotent, so a
run that changes nothing is harmless. Wait a minute or so for the
ConfigMap's new file to reach the Postgres pod, then:

```sh
kubectl -n opamp exec opamp-opamp-server-postgres-0 -c postgres -- /docker-entrypoint-initdb.d/initdb.sh
```

It uses the passwords in the pod's own environment, so nothing secret
crosses your terminal. It also sets both database passwords again from the
Secret, which is how you change one: update the Secret, restart the
Postgres pod (`kubectl -n opamp delete pod opamp-opamp-server-postgres-0`),
run the command, then restart the server's pods. With an external
Postgres, run the new `deploy/bootstrap.sql` there yourself.

## If the first start failed

The bundled Postgres runs the bootstrap once, when it creates its data,
and never retries it. If either password is unset, empty or not URL-safe,
its pod exits before creating anything, with `opamp-server: owner-password
and runtime-password must be set, non-empty and URL-safe …`. Fix the
Secret and the pod starts cleanly.

If the bootstrap failed anyway (the server's pods never get past
`wait-for-postgres` or `migrate`, and the Postgres pod's log shows the
bootstrap failing), start again from an empty volume:

```sh
helm uninstall opamp -n opamp
kubectl -n opamp delete pvc data-opamp-opamp-server-postgres-0
helm install …
```

Deleting the claim deletes the database: do it only before the server
holds anything you need.

## Bootstrapping another Postgres

The `<fullname>-bootstrap` ConfigMap is always rendered, so a Postgres you
run in the cluster from the official `postgres` image can bootstrap the
same way at its first start:

- Mount the ConfigMap twice, with `items`: `initdb.sh` alone (mode 0555)
  into `/docker-entrypoint-initdb.d`, and `bootstrap.sql` into
  `/opamp-bootstrap` (in the init folder it would also run as a `*.sql`
  file).
- Give the container `OPAMP_OWNER_PASSWORD`, `OPAMP_RUNTIME_PASSWORD` and,
  for a database not named `telflo_opamp`, `OPAMP_DATABASE_NAME`.
- Copy the bundled StatefulSet's container `command` too. It holds the
  first-start guard; without it, a failed first bootstrap is never
  retried.

## Test it locally

From a checkout of the repository, `make smoke` installs this chart from
your tree on a throwaway minikube profile and checks it end to end: both
pods ready, the endpoints, an organization and a fleet, each pod deleted
and back, the bootstrap re-run above, and an upgrade and a rollback. It
writes nothing under your `~/.kube`, and deletes everything when done,
the profile included.

```sh
make smoke                          # the profile opamp-smoke; a new work folder under TMPDIR
make smoke SMOKE_PROFILE=my-smoke   # another profile: it refuses one that exists already
```

It needs Docker, minikube, kubectl, helm, openssl, curl and jq, the
`postgres:18` image already pulled, and 6 GB free in Docker's disk.
`scripts/smoke-minikube.sh --help` lists its options.
