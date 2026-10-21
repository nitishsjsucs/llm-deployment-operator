# API reference

Group `serving.llmops.dev`, version `v1alpha1`. Source: [`api/v1alpha1`](../api/v1alpha1).
Defaults are applied by the API server through the CRD schema, and again in memory by the reconciler
for objects stored before a default existed.

## ModelDeployment (namespaced, short name `mdep`)

| Field | Default | Validation / notes |
|---|---|---|
| `spec.tenant` | – | required, 1–63 characters; matched against `TenantQuota.spec.tenant` |
| `spec.model.family` | – | `llama` or `qwen` |
| `spec.model.name` | – | served model name (`--served-model-name`, KServe `spec.model.name`) |
| `spec.model.catalogRef` | – | one of `llama-3.1-8b`, `llama-3.1-70b`, `qwen2.5-7b`, `qwen2.5-14b`, `qwen2.5-72b` |
| `spec.model.architecture` | – | inline dimensions from `config.json`: `parametersMillions`, `layers`, `kvHeads`, `headDim`, `hiddenSize`, `vocabSize`, `tiedEmbeddings`, `maxContextLength`; exactly one of `catalogRef` / `architecture` |
| `spec.variants[]` | – | 1–8 entries: `quantization` (`fp16`, `awq`, `gptq`), `uri` (`hf://`, `s3://`, `gs://`, `pvc://`, `oci://`, `http(s)://`), `bits` (fp16 → 16, awq → 4, gptq → 4 or 8; defaults 16 / 4 / 4) |
| `spec.gpuBudget.gpuType` | – | `A100-80GB`, `A100-40GB` or `H100-80GB` |
| `spec.gpuBudget.maxMemoryGiBPerReplica` | unset (any profile) | ≥ 1; caps the memory of the MIG profile a replica may claim |
| `spec.gpuBudget.maxModelLen` | `8192` | ≥ 256; `--max-model-len`; must not exceed the model's context |
| `spec.gpuBudget.maxConcurrentSequences` | `8` | ≥ 1; `--max-num-seqs`; the KV cache is sized for all of them at `maxModelLen` |
| `spec.replicas` | `1` | ≥ 0; one MIG instance each |
| `spec.runtime.image` | KServe's default | vLLM image |
| `spec.runtime.extraArgs` | – | appended to the generated vLLM arguments |
| `spec.runtime.env` | – | added to the vLLM container (e.g. `HF_TOKEN` from a Secret) |
| `spec.rollout.strategy` | `Canary` | `Canary` or `Recreate` |
| `spec.rollout.steps` | `[10, 50, 100]` | 1–10 items, each 1–100, strictly increasing, last must be 100 |
| `spec.rollout.stepInterval` | `5m` | time Ready at a step before moving on |
| `spec.rollout.progressDeadline` | `30m` | time a step may wait for Ready before rollback |
| `spec.routing.gateway` | `kserve/kserve-ingress-gateway` | `name`, `namespace` of the Gateway |
| `spec.routing.hostnames` | all of the Gateway's | up to 16 |

### Status

| Field | Meaning |
|---|---|
| `phase` | `Pending`, `Progressing` (canary in flight), `Ready` (stable revision Ready), `Failed` (no plan) |
| `selectedVariant`, `migProfile`, `memoryEstimate` (MiB), `replicasPerGPU` | plan for the current spec (the canary's during a rollout) |
| `stableRevision`, `canaryRevision`, `canaryWeight`, `canaryStep`, `canaryStepStartTime`, `rolledBackRevision` | rollout state ([rollouts.md](rollouts.md#persisted-state)) |
| `allocatedSlices[]` | MIG instances held per revision (`revision`, `profile`, `count`); counted against quota |
| `observedGeneration` | generation the status reflects |

### Conditions

| Type | True reason | False reason(s) |
|---|---|---|
| `Planned` | `VariantSelected` (message: estimate breakdown) | `PlanFailed` (message: every checkpoint's estimate) |
| `Ready` | `StableReady` | `NotDeployed`, `StableNotReady` |
| `Progressing` | `CanaryInProgress` | `Stable`, `RolledBack` |
| `QuotaExceeded` | `QuotaExceeded` (message: which limit, by how much) | `WithinQuota` |
| `RolledBack` | `ProgressDeadlineExceeded` | removed when no revision is held back |

### Events

| Type | Reasons |
|---|---|
| Normal | `Deployed`, `Recreated`, `CanaryStarted`, `CanaryAdvanced`, `CanaryPromoted`, `CanaryAborted` |
| Warning | `RolledBack`, `QuotaExceeded`, `PlanFailed`, `DriftCorrected` |

`kubectl get mdep` columns: Tenant, Variant, MIG, Replicas, Canary, Phase, Age.

## TenantQuota (cluster-scoped, short name `tq`)

| Field | Validation / notes |
|---|---|
| `spec.tenant` | required, 1–63 characters |
| `spec.migSlices[]` | `profile` (pattern `^[1-7]g\.[0-9]+gb$`), `max` ≥ 0; one entry per profile |
| `spec.maxGPUMemoryGiB` | ≥ 0; nominal MIG memory across all of the tenant's instances |
| `status.usedSlices[]`, `status.usedGPUMemoryGiB`, `status.deployments` | current usage, kept by the TenantQuota reconciler |

`kubectl get tq` columns: Tenant, Used GiB, Max GiB, Deployments, Age.

## Metrics

| Metric | Labels | Meaning |
|---|---|---|
| `llmdeploy_drift_corrections_total` | `kind` = `LLMInferenceService`, `HTTPRoute`, `ResourceClaimTemplate` | child objects modified out of band and restored |

controller-runtime's standard metrics are served on the same endpoint (`--metrics-bind-address`, default `:8080`).
