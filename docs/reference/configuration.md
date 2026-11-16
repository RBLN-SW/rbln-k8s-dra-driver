# DRA Driver settings

Use the operator's `draKubeletPlugin` settings for an operator deployment, or this
repository's [chart values](../../deployments/helm/k8s-dra-driver-npu/values.yaml)
for a standalone deployment. These settings describe the current source.

## Change logging

Merge this into your operator values and apply the
[operator upgrade procedure](https://docs.rbln.ai/latest/software/system_management/kubernetes/upgrade_npu_operator.html#upgrading-with-helm):

```yaml
draKubeletPlugin:
  logging:
    level: debug
    format: json
```

For the [standalone chart](../guides/deploy-standalone.md), use top-level
`logging.level` and `logging.format`; these configure both the kubelet plugin
and the optional webhook. Read logs from the updated Pods and restore `info`
after diagnosis.

| Environment variable | Default | Accepted values |
| --- | --- | --- |
| `RBLN_DRA_DRIVER_LOG_LEVEL` | `info` | `error`, `warning`/`warn`, `info`, `debug`, `trace` |
| `RBLN_DRA_DRIVER_LOG_FORMAT` | `json` | `json`, `text` |

Invalid logging values fall back to the corresponding default and emit a warning.
`trace` includes request/response payloads and is intended for targeted debugging.
The current operator's logging schema accepts levels through `debug`, so `trace`
is available only through a deployment that supports the binary's full settings.

## Follow a claim in logs

Records use `ts`, `level`, and `msg`, with `err` on failures and `caller` at debug
or trace. Claim operations include `claimNamespace`, `claimName`, and `claimUID`;
request records also carry `requestID` and `method`. Webhook records instead use
`requestUID`, `resource`, `namespace`, and `name`.

The numeric `v` field distinguishes Kubernetes library verbosity within debug
and trace. The plugin bridges Kubernetes, gRPC, and RSD library logging into its
structured stream. Use the claim identifiers to correlate preparation failures
with the workload, as shown in [allocation diagnosis](../guides/troubleshoot-allocation.md).

## Kubelet plugin options

Flags take precedence over environment variables. The operator or standalone
chart supplies some defaults; inspect the rendered Pod before changing paths.

| Flag | Environment variable | Binary default / purpose |
| --- | --- | --- |
| `--node-name` | `NODE_NAME` | Required |
| `--cdi-root` | `CDI_ROOT` | `/etc/cdi` |
| `--kubelet-registrar-directory-path` | `KUBELET_REGISTRAR_DIRECTORY_PATH` | `/var/lib/kubelet/plugins_registry` |
| `--kubelet-plugins-directory-path` | `KUBELET_PLUGINS_DIRECTORY_PATH` | `/var/lib/kubelet/plugins` |
| `--healthcheck-port` | `HEALTHCHECK_PORT` | `-1`: disabled; charts configure `51515`; `0` selects a random port |
| `--device-scan-interval` | `DEVICE_SCAN_INTERVAL` | `5s`; non-positive disables periodic scans |

The scan covers native and VFIO devices. `--vfio-rescan-interval` and
`VFIO_RESCAN_INTERVAL` remain aliases; prefer the new names. At the environment
level, `DEVICE_SCAN_INTERVAL` takes precedence over the legacy name.

For all binary options, run `npu-kubelet-plugin --help` from the deployed release.
[Back to the guides](../../README.md#guides)
