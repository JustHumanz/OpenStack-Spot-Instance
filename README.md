# OpenStack spot controller prototype

A Go controller for OpenStack 2025.2 that proposes moving operator-designated spot VMs from heavily loaded computes to less loaded computes. Nova and Placement remain responsible for the final scheduling and resource claims. This is a prototype, not a production-ready spot market.

## Scope and safeguards

- Dry-run is off by default. Always pass `-dry-run` to preview proposals without changing OpenStack.
- Nova server listing is filtered by the exact server tag `spot_instance:true`. Migration also requires an operator-controlled allowlist of **flavor IDs**. Tags can be changed by server owners, so do not rely on the tag alone for authorization. Use separate spot flavors, even when their sizing matches ordinary flavors.
- Placement inventory uses `(total - reserved) * allocation_ratio - usage`. The controller also checks minimum, maximum, and step size for each requested resource. Inventory and usage are snapshots; Nova makes the authoritative, concurrent destination claim.
- The controller only considers active instances with a single source-provider allocation containing shared `VCPU`, `MEMORY_MB`, and optionally `DISK_GB`. Pinned CPUs, multi-provider allocations, and other resources are skipped. Nova still checks availability zones, affinity, NUMA, traits, devices, storage, and migration compatibility.
- Live migration uses Nova microversion 2.68 and a scheduler-validated host; it never uses force. An accepted request is asynchronous. While running, the controller waits until the corresponding migration reaches a terminal state. If it cannot confirm the result, it stops making migration requests.
- Only one proposal or migration is initiated per cycle. The default 30-minute per-VM cooldown and in-flight tracking are **in memory**, and are lost on restart. Run one replica only; there is no leader election or cross-process locking. Check Nova migrations before restarting or enabling live mode. A request failure can leave the controller waiting indefinitely; investigate Nova before restarting it.
- After trying all possible live migrations, the controller can shelve and offload one eligible spot VM if no measured destination is below the utilization threshold and meets the minimum improvement. Placement capacity on a host above the threshold does not make it an eligible migration destination. If a capacity-fitting host has missing metrics, the controller will not shelve because its eligibility is unknown. Shelving stops the VM and interrupts its workload; it is not transparent preemption. The VM remains available to unshelve later, subject to Nova policy and capacity. This behavior is controlled by `-shelve-when-no-destination` (default `true`) and is suppressed by `-dry-run`.
- This does not create, sell, price, or delete instances. Normal Nova create requests are not intercepted, queued, or guaranteed priority. Building a reclaim API, persistent coordination, user notifications, and billing is future work.

## Requirements

- Go 1.24 or newer to build; OpenStack 2025.2 with working KVM live migration and accessible Ceph storage.
- An OpenStack identity authorized by your deployment policy to list servers across projects, list hypervisors and migrations, and read Placement inventory/usage/allocations. Live mode requires permission to request live migration; shelve fallback additionally requires permission to shelve and shelve-offload servers. Use a dedicated account with the smallest practical policy grants.
- An OpenStack `clouds.yaml` profile and `OS_CLOUD` name. Do not commit credentials.
- Prometheus or Kubernetes metrics-server if real CPU utilization is required. Placement mode uses allocated vCPU capacity instead of measured usage.

## Run locally

```sh
go test ./...
go build -o spot-controller .
cp config.example.yaml config.yaml
./spot-controller -config config.yaml -once
```

Edit `config.yaml` first: set `spot.flavors` to your allowed flavor UUIDs and tune `metrics`, `spot`, and `controller` settings. The example uses `controller.dry_run: true`; with that setting, a one-shot run only logs a proposal. Set it to `false` and omit `-once` to enable actions and let the controller monitor completion. The only controller CLI options are `-config` (default `config.yaml`) and `-once`.

Create an OpenStack client configuration file, for example `~/.config/openstack/clouds.yaml`:

```yaml
clouds:
	openstack:
		auth:
			auth_url: https://identity.example.com/v3
			username: spot-controller
			password: REPLACE_WITH_SECRET
			project_name: service
			user_domain_name: Default
			project_domain_name: Default
		identity_api_version: 3
```

Keep this file out of source control and restrict its permissions. Select the profile and run the controller:

```sh
export OS_CLOUD=openstack
go run . -config config.yaml -once
```

Gophercloud searches for `clouds.yaml` in the current directory, `${XDG_CONFIG_HOME:-$HOME/.config}/openstack/`, and `/etc/openstack/`. Set `OS_CLIENT_CONFIG_FILE` to a specific `clouds.yaml` path to use that location instead. If that option is set, the same directory is also used to look for `secure.yaml`.

In the current implementation, region and endpoint interface are selected separately from the cloud profile: set `OS_REGION_NAME` and `OS_INTERFACE` (`public`, `internal`, or `admin`) when needed. The controller does not currently use the region/interface returned by `clouds.yaml`.

The command above uses the `controller.dry_run` value from YAML. To enable actions after reviewing proposals, set it to `false` and omit `-once` so the process can monitor completion. When enabled, it prefers live migration; if no measured destination is below the threshold and meets the minimum improvement, it may shelve and offload the VM. Shelving powers off the guest and interrupts its workload. Disable that fallback with `spot.shelve_when_no_destination: false`. A one-shot run exits after one cycle, so use it only for dry-run previews.

Add the Nova server tag to an existing spot server with:

```sh
openstack --os-compute-api-version 2.26 server set --tag spot_instance:true SERVER_ID
```

This is a server tag, not a `spot_instance=true` metadata property. The server's flavor ID must also appear in `spot.flavors` in `config.yaml`.

## Monitoring

Select the metrics source with `metrics.driver` in `config.yaml`:

- `placement` (default): percentage of overcommitted VCPU inventory allocated. This is not actual CPU load.
- `prometheus`: set `metrics.prometheus_url`. The default PromQL is `metrics.prometheus_query`; it requires node-exporter data and a unique `instance` label. The query must return an instant vector of CPU percentages. Prometheus evaluates instant queries at request time, so sample timestamps alone do not prove the underlying exporter is fresh. Configure the query and monitoring to exclude stale/down exporters.
- `metrics-server`: uses the Kubernetes Metrics API node CPU usage divided by node CPU capacity. It uses in-cluster credentials, or `metrics.kubeconfig` when running outside the cluster. It does not assess memory pressure or VM-specific usage.

The Prometheus URL may be the base URL, the root `/query` browser URL, or the root `/api/v1/query` API URL. For a Prometheus server behind a path prefix, provide the base URL including that prefix. Startup logs show the selected driver and each compute host's matched metric and load.

`metrics.host_map` maps Nova compute-service host names to Prometheus `instance` labels or Kubernetes node names. For example:

```yaml
metrics:
	host_map:
		compute-1: compute-1.example.com:9100
		compute-2: compute-2.example.com:9100
```

Without a mapping, the Nova host name is used exactly. For Prometheus, if that does not match, the controller also tries to match Nova's hypervisor `host_ip` to a unique Prometheus `instance` IP address. This fallback is skipped when hosts or metric labels share the same IP, or when an explicit mapping exists. Missing, invalid, ambiguous, or stale samples are skipped. Ensure each mapping identifies a different host. Prometheus must be reachable from the controller; authenticated Prometheus endpoints or custom CA configuration are not currently supported.

For IP-based Prometheus `instance` labels, map each Nova compute host to its exact label, including the port. The controller reports the expected label when a host has no matching sample. An overloaded host must also have an active server with the required spot tag and allowed flavor, and a suitable destination with enough Placement capacity and the minimum load improvement.

Set `spot.threshold` (default 80 percent), `spot.min_improvement` (20 percentage points), `controller.interval` (1 minute), `controller.cooldown` (30 minutes), and `controller.max_metrics_age` (3 minutes) in YAML. A source at or above the threshold is overloaded; a migration destination must be below the threshold and at least the minimum improvement less busy than the source. If no such measured target exists, the controller may shelve a VM even when Placement shows capacity on hosts that are above threshold. It will not shelve if a capacity-fitting host has missing metrics. Utilization thresholds do not represent guaranteed guest performance. Memory and disk telemetry are not considered when ranking.

## Kubernetes deployment

Build and push the image using the included Dockerfile. Configure the image and namespace in `deploy/kubernetes.yaml`; controller settings are in the `spot-controller-config` ConfigMap's `config.yaml` data. The example grants read access to Kubernetes nodes and node metrics; omit those RBAC resources when not using metrics-server. Startup logs the resolved flavor allowlist so a missing or stale config is visible immediately.

Mount a `clouds.yaml` Kubernetes Secret at `/etc/openstack/clouds.yaml` in the chosen namespace and set `OS_CLOUD=openstack` on the container. `clouds.Parse()` searches `/etc/openstack` by default. For example, create the Secret from a protected file with `kubectl create secret generic spot-controller-openstack --namespace openstack --from-file=clouds.yaml=/secure/path/clouds.yaml`, then add this volume and mount to the Deployment:

```yaml
spec:
  template:
    spec:
      containers:
        - name: controller
          image: hub.humanz.moe/library/nova-spot-instance:dev
          imagePullPolicy: Always
          args:
            - -config=/etc/spot-controller/config.yaml
          env:
            - name: OS_INTERFACE
              value: internal
            - name: OS_CLOUD
              value: openstack
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            runAsNonRoot: true
            capabilities:
              drop: ["ALL"]
          volumeMounts:
          - name: controller-config
            mountPath: "/etc/spot-controller/"
            readOnly: true
          - name: os-cloud-config
            mountPath: "/etc/openstack/"
            readOnly: true              
          resources:
            requests:
              cpu: 50m
              memory: 96Mi
            limits:
              cpu: 500m
              memory: 256Mi
      volumes:
      - name: controller-config
        configMap:
          name: spot-controller-config
      - name: os-cloud-config
        secret:
          secretName: humans-cloud-config
```

The checked-in Deployment mounts `spot-controller-config` at `/etc/spot-controller/config.yaml` and the OpenStack cloud secret at `/etc/openstack/`. Edit the ConfigMap's YAML data for your spot flavor IDs, metrics source, thresholds, and dry-run setting; `-once` remains available for a one-cycle preview. `OS_CLOUD` selects the cloud profile, while `OS_REGION_NAME` and `OS_INTERFACE` remain environment variables and may be added to the pod when needed. For internal certificate authorities, supply a trusted CA bundle in the container and reference it in the cloud configuration. Apply the manifest after reviewing it, and keep `controller.dry_run: true` until you are ready to enable actions.

This example intentionally uses one replica and the `Recreate` deployment strategy. It is not a high-availability deployment; in-memory state is not durable and another controller instance must not run concurrently.