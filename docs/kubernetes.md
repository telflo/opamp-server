# Run it on Kubernetes

The Helm chart in [`charts/opamp-server/`](../charts/opamp-server/) installs
the server as two Deployments, `<release>-opamp-server-agent` (`serve agent`)
and `<release>-opamp-server-management` (`serve management`), each with its
own `replicas`, `resources` and disruption budget under `server.agent` and
`server.management`, with a Postgres of its own by default or one you run.
Each pod migrates the database before it serves.

```sh
# 1. The Secrets: the chart reads every credential from Secrets you create.
mkdir opamp-secrets && chmod 700 opamp-secrets && cd opamp-secrets
for name in management-secret encryption-key owner-password runtime-password postgres-password; do
  openssl rand -hex 32 | tr -d '\n' >"$name"
done
kubectl create namespace opamp
kubectl -n opamp create secret generic opamp-server \
  --from-file=management-secret --from-file=encryption-key \
  --from-file=owner-password --from-file=runtime-password
kubectl -n opamp create secret generic opamp-server-postgres --from-file=postgres-password

# 2. The chart. publicUrl is the agent endpoint's URL as collectors reach it.
helm install opamp oci://ghcr.io/telflo/charts/opamp-server --version 0.2.4 -n opamp \
  --set auth.existingSecret=opamp-server \
  --set postgres.auth.existingSecret=opamp-server-postgres \
  --set server.publicUrl=https://opamp.example.com/opamp/api/v1

# 3. Check it. Forward the management API to this machine; this keeps
#    running until Ctrl-C, so leave it in this terminal.
kubectl -n opamp port-forward svc/opamp-opamp-server-management 51235:51235

#    Then, in a second terminal, ask it whether it's healthy: it answers ok.
curl -sS http://127.0.0.1:51235/health
```

**Back up `encryption-key` outside the cluster.** Without it, no enrollment
token's bearer can be read again. Give `management-secret` to whatever
calls the management API.

**Each component is sized on its own.** For many collectors and a few
administrators, for example, pass a values file like this with `-f` to
`helm install` or `helm upgrade`:

```yaml
server:
  agent:
    replicas: 6
    resources:
      requests: { cpu: 500m, memory: 256Mi }
    podDisruptionBudget: { enabled: true, maxUnavailable: 1 }
  management:
    replicas: 1
```

`replicas: 0` stops a component until you set it back. An upgrade that
changes only one component's values rolls only that component's pods.

The management API's Service is always `ClusterIP`. The agent endpoint's
is `ClusterIP` by default too. For collectors outside the cluster, set
`service.agent.type=LoadBalancer` with your cloud's annotations in
`service.agent.annotations`, or route an Ingress or Gateway of your own
to it. Terminate TLS at the load balancer, or give the pods a
certificate with `server.agent.tls.secretName`. The
[chart's README](../charts/opamp-server/README.md) has every value, external
Postgres, TLS, upgrades, and `make smoke`, which installs the chart on a
throwaway minikube cluster and checks it end to end.

**The quick start on minikube.** Install the chart as above, but with the
agent endpoint's in-cluster address as `server.publicUrl`, since the
collector runs in the cluster:
`--set server.publicUrl=http://opamp-opamp-server-agent.opamp.svc:51234/opamp/api/v1`.
Keep the port-forward from the install's step 3 running. Then, in a second
terminal, from this checkout, run its `setopt interactivecomments` line
if you use zsh, and follow the [Quick start](../README.md#quick-start-docker)'s steps 4 to 12
with two changes:

- **In step 4,** save the secret from the install's step 1 in place of the
  development one:
  `printf 'Authorization: Bearer %s\n' "$(cat path/to/opamp-secrets/management-secret)" > auth-header`.
- **In step 8,** start the collector in the cluster
  ([deploy/quickstart-collector/kubernetes.yaml](../deploy/quickstart-collector/kubernetes.yaml))
  in place of Compose:

```sh
minikube image build -t opamp-quickstart-collector:dev ../deploy/quickstart-collector
printf 'OPAMP_TOKEN=%s\nCOLLECTOR_GROUP_ID=%s\n' "$(jq -r .tokens.default.bearer fleet.json)" "$group" > collector.env
kubectl -n opamp create secret generic quickstart-collector --from-env-file=collector.env
kubectl -n opamp apply -f ../deploy/quickstart-collector/kubernetes.yaml
kubectl -n opamp logs -f deploy/quickstart-collector   # the collector's log
```

`kubectl -n opamp delete -f ../deploy/quickstart-collector/kubernetes.yaml`
removes it and its state.
