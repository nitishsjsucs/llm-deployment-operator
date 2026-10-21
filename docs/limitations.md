# Limitations and known gaps

The biggest one first: nothing here has run on a real cluster. See [testing.md](testing.md).

## Planning

| Gap | Effect |
|---|---|
| One MIG instance per replica | No tensor parallelism across instances or GPUs, so models that do not fit a full GPU (for example fp16 70B) cannot be planned |
| 16-bit KV cache assumed | `--kv-cache-dtype fp8` passed through `extraArgs` is not taken into account, so the estimate is too high for it |
| Fixed quantization group size of 128 | Checkpoints with other group sizes are estimated slightly off |
| Fixed 1.5 GiB runtime overhead | An assumption, never compared with real vLLM memory use |
| Three GPU types, compute-only profiles | `A100-80GB`, `A100-40GB`, `H100-80GB`; the `+me` media profiles are not modelled |

## MIG and DRA

| Gap | Effect |
|---|---|
| Static MIG partitioning only | The operator never creates or reconfigures MIG instances; they must exist on the nodes |
| DRA attribute names unverified | `profile` and `productName` come from driver documentation; a different driver version may need `MIGSelectors` changed |
| `H100` product pattern | The H100-80GB selector matches any product name containing `H100`, not only the 80 GB model |

## Rollouts

| Gap | Effect |
|---|---|
| Health = `Ready` condition only | No metric-based canary analysis (error rate, latency) |
| Rolled-back revision is held | It is retried only after the spec changes to a different revision |
| Weight and replicas move together | Advancing a step raises the canary's replicas and its traffic weight in the same reconcile |
| Canary needs headroom | Up to `replicas` extra MIG instances at the 100% step |
| Recreate has no overlap | The old revision is removed in the same pass the new one is created, so traffic has no ready backend until the new revision is Ready |

## Quota

| Gap | Effect |
|---|---|
| Enforced by the reconciler, not an admission webhook | With the default of one concurrent reconcile per controller checks are sequential, but status lag between deployments of the same tenant can still let a short-lived overcommit through |
| Counts nominal memory | `maxGPUMemoryGiB` uses the size in the profile name (20 GiB for `1g.20gb`), not usable memory |

## Operations

| Gap | Effect |
|---|---|
| Plain HTTP `/metrics` | No authentication or TLS on the metrics endpoint |
| No published image | Build and push your own (`make docker-build docker-push`) |
| Whole-`spec` updates | When an owned field is corrected, fields other writers added to `spec` are dropped until they re-add them |
| No admission webhook | Validation relies on CRD schema and CEL rules; plan errors surface only in status |
