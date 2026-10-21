# llm-deployment-operator

A Kubernetes operator for platform teams that fits Llama and Qwen models onto NVIDIA MIG slices
and rolls them out on KServe with weighted canaries.

[![CI](https://github.com/nitishsjsucs/llm-deployment-operator/actions/workflows/ci.yaml/badge.svg)](https://github.com/nitishsjsucs/llm-deployment-operator/actions/workflows/ci.yaml)
[![Go 1.24](https://img.shields.io/badge/go-1.24-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

```mermaid
flowchart LR
    subgraph IN["You apply"]
        MD["ModelDeployment"]
        TQ["TenantQuota"]
    end
    subgraph OP["llm-deployment-operator"]
        PL["Planner<br/>checkpoint + MIG profile"]
        RO["Rollout<br/>state machine"]
        QC["Quota check"]
        AP["Drift-aware apply"]
    end
    subgraph CH["Child objects"]
        RCT["ResourceClaimTemplate"]
        LIS["LLMInferenceService"]
        HR["HTTPRoute"]
    end
    subgraph CL["Cluster"]
        DRA["NVIDIA DRA driver"]
        KS["KServe"]
        MIG["MIG slices<br/>A100 / H100"]
        POD["vLLM pods"]
    end
    MD --> PL --> RO --> QC --> AP
    TQ --> QC
    AP --> RCT & LIS & HR
    RCT --> DRA --> MIG
    LIS --> KS --> POD
    HR -->|"weighted split"| POD
    POD --- MIG
```

> [!IMPORTANT]
> Unit- and fake-client-tested only (51 tests). Nothing has run on a live cluster with KServe, the
> NVIDIA DRA driver, a Gateway or vLLM. [What was and wasn't tested](docs/testing.md)

## Highlights

- **Memory-aware planner** — estimates weights + KV cache + overhead; picks the highest-precision checkpoint that fits.
- **Smallest MIG slice** — e.g. Llama-3.1-8B AWQ on `1g.20gb`, four replicas per A100-80GB.
- **DRA claims** — one `ResourceClaimTemplate` per revision pins the MIG profile and GPU model.
- **Canary rollouts** — warm-up at 0%, then 10 → 50 → 100% via `HTTPRoute`; auto promote or rollback.
- **Drift correction** — reverts out-of-band edits to owned objects and counts each one.
- **Per-tenant quota** — `TenantQuota` caps MIG slices per profile and total MIG memory, canaries included.

## Quickstart

No cluster needed: run the tests and the allocation calculator.

```sh
git clone https://github.com/nitishsjsucs/llm-deployment-operator.git
cd llm-deployment-operator
make test                                   # controller-gen, go fmt, go vet, go test ./...
go run ./cmd/packsim                        # sample fleet: whole GPUs vs MIG packing
go run ./cmd/packsim -explain llama-3.1-8b  # memory estimate per checkpoint
```

Deploying needs Kubernetes 1.34+, KServe, the NVIDIA DRA driver and Gateway API: see [docs/installing.md](docs/installing.md).

## How it works

### One reconcile pass

```mermaid
sequenceDiagram
    participant W as Work queue
    participant R as Reconciler
    participant P as Planner
    participant S as Rollout
    participant Q as Quota
    participant K as API server
    W->>R: ModelDeployment, child or TenantQuota event
    R->>P: plan(spec)
    alt nothing fits
        P-->>R: error with every estimate
        R->>K: status: Planned=False, phase Failed
    else plan found
        P-->>R: checkpoint, MIG profile, estimate
        R->>K: save ControllerRevision (revision hash)
        R->>K: read Ready of stable + canary
        R->>S: Next(state, policy, observation)
        S-->>R: next state, RequeueAfter
        opt allocation grows
            R->>Q: others + proposed vs TenantQuotas
            Q-->>R: OK, or hold current allocation
        end
        R->>K: apply claim template, LLMInferenceService, HTTPRoute
        Note over R,K: out-of-band edits are restored (DriftCorrected)
        R->>K: prune old revisions, update status
        R-->>W: RequeueAfter
    end
```

Rollout state lives in the `ModelDeployment` status, so every pass starts from what is stored.
`RequeueAfter` wakes the reconciler for the next timed step.

### Picking a checkpoint and MIG profile

```mermaid
flowchart LR
    A["Estimate each checkpoint<br/>weights + KV cache<br/>+ 1.5 GiB overhead"] --> B["Order checkpoints<br/>1. more weight bits<br/>2. smaller estimate<br/>3. AWQ before GPTQ<br/>4. order in spec"]
    B --> C["Profiles ≤ maxMemoryGiBPerReplica<br/>least memory first,<br/>then fewest compute slices"]
    C --> D{"estimate ≤ 90%<br/>of profile memory?"}
    D -- yes --> E["Plan: checkpoint,<br/>profile, replicas per GPU"]
    D -- no --> F{"Larger<br/>profile left?"}
    F -- yes --> D
    F -- no --> G{"Next<br/>checkpoint left?"}
    G -- "yes, from the<br/>smallest profile" --> D
    G -- no --> H["Planned=False, phase Failed<br/>running revisions untouched"]
```

For the sample, fp16 (24.46 GiB) fits no profile under the 20 GiB cap, so AWQ (14.83 GiB) lands on `1g.20gb`.
AWQ and GPTQ 4-bit tie on memory; AWQ-first is a fixed convention, not a quality claim. [Memory math](docs/design.md#memory-estimate)

### Canary rollout

```mermaid
stateDiagram-v2
    direction LR
    [*] --> Stable: first revision
    Stable --> Warming: spec changes
    state Canary {
        direction LR
        Warming: Warming (0%)
        S10: 10%
        S50: 50%
        S100: 100%
        Warming --> S10: Ready
        S10 --> S50: Ready for stepInterval
        S50 --> S100: Ready for stepInterval
    }
    S100 --> Stable: promote after stepInterval
    Canary --> Stable: spec reverted (abort)
    Canary --> RolledBack: not Ready by progressDeadline
    RolledBack --> Warming: spec changes again
```

The canary runs `ceil(replicas × weight / 100)` replicas next to the full stable set (10% of 4 → 1),
and drops to 0% the moment it stops being Ready. `strategy: Recreate` swaps revisions at once. [Rollout details](docs/rollouts.md)

### Drift correction

```mermaid
flowchart LR
    A["Rendered object"] --> B{"Live object<br/>exists?"}
    B -- no --> C["Create"]
    B -- yes --> D{"Every rendered field<br/>matches live?"}
    D -- yes --> E["Leave alone"]
    D -- no --> F{"Live spec-hash ==<br/>rendered spec-hash?"}
    F -- "yes: someone else edited it" --> G["Restore spec<br/>DriftCorrected event<br/>counter + 1"]
    F -- "no: the spec changed" --> H["Update"]
```

Only rendered fields are compared, so fields that KServe or webhooks add are left alone.
Claim templates are immutable, so they are deleted and recreated instead. [Drift strategy](docs/design.md#drift-correction)

## Results

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/llama-8b-memory-dark.svg">
  <img alt="Llama-3.1-8B per-replica memory estimate at max-model-len 8192 and 8 sequences: fp16 24.46 GiB and gptq-int8 18.11 GiB exceed the 17.55 GiB usable on 1g.20gb and need 3g.40gb; awq-int4 and gptq-int4 at 14.83 GiB fit 1g.20gb" src="docs/images/llama-8b-memory-light.svg" width="720">
</picture>

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/packsim-gpus-dark.svg">
  <img alt="The 20-replica sample fleet needs 20 A100-80GB GPUs with one replica per whole GPU and 8 GPUs when packed onto MIG instances" src="docs/images/packsim-gpus-light.svg" width="720">
</picture>

| Allocation | GPUs | Replicas | Estimates / GPU memory | MIG memory / GPU memory | Compute slices used |
|---|---|---|---|---|---|
| One replica per whole GPU | 20 | 20 | 26.6% | – | – |
| MIG-packed (first-fit decreasing) | 8 | 20 | 66.5% | 100.0% | 44 / 56 |

> Static allocation calculation by `cmd/packsim` for a made-up five-workload fleet, from the planner's
> estimates. No model was loaded; GPU utilization, throughput and latency were not measured. [Full output](docs/packsim.md)

## Example

```yaml
# config/samples/serving_v1alpha1_modeldeployment_llama.yaml (HF_TOKEN env omitted)
apiVersion: serving.llmops.dev/v1alpha1
kind: ModelDeployment
metadata: {name: llama-chat, namespace: team-a}
spec:
  tenant: team-a
  model:
    family: llama
    name: meta-llama/Llama-3.1-8B-Instruct
    catalogRef: llama-3.1-8b           # or spec.model.architecture
  variants:
    - {quantization: fp16, uri: hf://meta-llama/Llama-3.1-8B-Instruct}
    - {quantization: awq, uri: hf://hugging-quants/Meta-Llama-3.1-8B-Instruct-AWQ-INT4}
    - {quantization: gptq, bits: 4, uri: hf://hugging-quants/Meta-Llama-3.1-8B-Instruct-GPTQ-INT4}
  gpuBudget:
    gpuType: A100-80GB                 # A100-80GB | A100-40GB | H100-80GB
    maxMemoryGiBPerReplica: 20         # rules out 3g.40gb and larger
    maxModelLen: 8192
    maxConcurrentSequences: 8
  replicas: 4
  runtime:
    image: vllm/vllm-openai:v0.10.1
    extraArgs: ["--enable-prefix-caching"]
  rollout: {strategy: Canary, steps: [10, 50, 100], stepInterval: 5m, progressDeadline: 30m}
```

The reconciler's `Planned` condition for this sample:

```text
awq-int4 on 1g.20gb: estimated 14.83 GiB (weights 5.33, KV cache 8.00, overhead 1.50) of 17.55 GiB usable; 4 replicas per A100-80GB
```

The objects it renders are in [docs/design.md](docs/design.md#rendered-objects).

## Project layout

| Path | What |
|---|---|
| `api/v1alpha1` | `ModelDeployment` and `TenantQuota` types (group `serving.llmops.dev`) |
| `internal/planner`, `internal/packing` | Memory estimate, checkpoint + profile choice, MIG tables, first-fit-decreasing packing |
| `internal/rollout` | Canary / Recreate state machine (pure) |
| `internal/quota` | Quota arithmetic (pure) |
| `internal/render` | Builds the `ResourceClaimTemplate`, `LLMInferenceService` and `HTTPRoute` |
| `internal/controller` | Reconcilers, drift-aware apply, metrics, field indexes |
| `cmd/manager`, `cmd/packsim` | Operator entrypoint; static fleet allocation calculator |
| `config/`, `charts/` | Kustomize manifests, samples, Helm chart |

## Limitations

- One MIG instance per replica: no tensor parallelism, so fp16 70B cannot be planned.
- Static MIG partitioning only; the operator never creates or reshapes MIG instances.
- The memory estimate assumes a 16-bit KV cache and a fixed 1.5 GiB overhead; it was never checked against vLLM.
- Quota is checked in the reconciler, not an admission webhook, so status lag can let a brief overcommit through.
- Canary health is the `LLMInferenceService` `Ready` condition only; no error-rate or latency analysis.
- `/metrics` is plain HTTP with no authentication.

All known gaps: [docs/limitations.md](docs/limitations.md).

## Docs

| Page | What's in it |
|---|---|
| [design.md](docs/design.md) | Memory math, MIG tables and packing, DRA selectors, KServe and HTTPRoute shape, drift, quota |
| [rollouts.md](docs/rollouts.md) | Canary and Recreate transitions, timing, replica math, events |
| [api.md](docs/api.md) | Spec fields and defaults, status, conditions, events |
| [installing.md](docs/installing.md) | Prerequisites, image build, kustomize and Helm, manager flags |
| [packsim.md](docs/packsim.md) | The fleet calculator: inputs, full output, MIG layout, chart regeneration |
| [testing.md](docs/testing.md) | What was and wasn't tested, how to run the checks |
| [limitations.md](docs/limitations.md) | Every known gap |

## License

Apache License 2.0. See [LICENSE](LICENSE).
