# Installing

> [!WARNING]
> None of the prerequisites below were available while this project was developed, so these steps
> have not been run end to end. See [testing.md](testing.md).

## Prerequisites

| Component | Requirement | Why |
|---|---|---|
| Kubernetes | 1.34 or later, `resource.k8s.io/v1` served | DRA `ResourceClaimTemplate` (the Helm chart sets `kubeVersion: >=1.34.0-0`) |
| NVIDIA DRA driver for GPUs | MIG support on, `mig.nvidia.com` DeviceClass, MIG instances already created | the claim selects an existing MIG device by profile and product name |
| KServe | `LLMInferenceService` CRD and controller (the llm-d based serving stack) | runs vLLM, creates the InferencePool and endpoint picker |
| Gateway API | `HTTPRoute` CRDs, Inference Extension (`InferencePool`) CRDs, a Gateway implementation and a Gateway | default `kserve/kserve-ingress-gateway`; override per deployment with `spec.routing.gateway` |

The manager watches `LLMInferenceService`, `HTTPRoute` and `ResourceClaimTemplate`, so those APIs
must be served before it starts.

## Build and deploy

No image is published. Build and push one, then deploy with kustomize or Helm:

```sh
make docker-build docker-push IMG=<registry>/llm-deployment-operator:<tag>

# kustomize: CRDs, RBAC, Deployment and metrics Service in llm-deployment-operator-system
make deploy IMG=<registry>/llm-deployment-operator:<tag>

# or Helm: CRDs come from the chart's crds/ directory
helm install llm-deployment-operator charts/llm-deployment-operator \
  --namespace llm-deployment-operator-system --create-namespace \
  --set image.repository=<registry>/llm-deployment-operator --set image.tag=<tag>
```

## Try the samples

The samples live in namespaces `team-a` and `team-b`, and the llama sample reads `HF_TOKEN` from
Secret `hf-token` (key `token`) in `team-a`. Create those first:

```sh
kubectl create namespace team-a && kubectl create namespace team-b
kubectl -n team-a create secret generic hf-token --from-literal=token=<hugging-face-token>
kubectl apply -k config/samples
kubectl get mdep -A && kubectl get tq
```

Clients then call `/<namespace>/<deployment>/v1/chat/completions` on the Gateway, for example
`/team-a/llama-chat/v1/chat/completions`.

## Make targets

| Target | What it does |
|---|---|
| `make install` / `make uninstall` | apply / delete the CRDs (`config/crd`) |
| `make deploy` / `make undeploy` | kustomize build of `config/default` piped to `kubectl` |
| `make run` | run the manager against the current kubeconfig |
| `make docker-build` / `make docker-push` | build / push `IMG` (default `controller:latest`) |

## Manager flags

| Flag | Default | Meaning |
|---|---|---|
| `--metrics-bind-address` | `:8080` | Prometheus metrics (plain HTTP); `0` disables |
| `--health-probe-bind-address` | `:8081` | `/healthz` and `/readyz` |
| `--leader-elect` | `false` (kustomize and Helm turn it on) | only one active manager |
| `--inference-pool-api-group` | `inference.networking.k8s.io` | use `inference.networking.x-k8s.io` for KServe versions on the older InferencePool API |
| `--zap-*` | zap defaults | logging (e.g. `--zap-log-level=debug`) |

## Helm values

| Value | Default |
|---|---|
| `image.repository`, `image.tag`, `image.pullPolicy` | `ghcr.io/nitishsjsucs/llm-deployment-operator`, chart `appVersion`, `IfNotPresent` (no image is published there) |
| `replicaCount` | `1` |
| `leaderElection` | `true` |
| `inferencePoolAPIGroup` | `inference.networking.k8s.io` |
| `metrics.port`, `metrics.service.enabled` | `8080`, `true` |
| `probes.port` | `8081` |
| `extraArgs` | `[]`, e.g. `["--zap-log-level=debug"]` |
| `resources` | requests `10m` / `64Mi`, limits `500m` / `256Mi` |

## KServe configuration to check

- Remove `nvidia.com/gpu` limits from your default `LLMInferenceServiceConfig`: the pod gets its GPU from the DRA claim.
- If KServe serves vLLM over TLS, pass `--ssl-certfile` / `--ssl-keyfile` through `spec.runtime.extraArgs`.
- If your driver names MIG attributes differently from `profile` / `productName`, change `MIGSelectors` in `internal/render/render.go`.
