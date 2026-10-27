# 04 - A runner for a cluster made through the gateway

Audience: an operator running GitLab CI jobs on a tenant cluster the user made through the gateway,
where the cluster is not on the operator's host.

Outcome: a GitLab runner on the GitLab host, holding nothing but a kubeconfig the user fetched with
`getKubeconfig` at https://k8s.socialweb.computer, placing job Pods into that user's tenant cluster
through the kcp mount.

This is the second runner shape. [02](02-gitlab-runner.md) runs on the kcp host and reaches the
tenant apiserver on its loopback port with a client certificate, because that is where the apiserver
binds. 04 is for the case where the cluster is not on the operator's host at all: the user made it
through the gateway, the nodes are rented from the compute market, and the only way in is the URL
`getKubeconfig` returned.

The gateway is API-only; there is no SPA. [README: Use the hosted
gateway](../../README.md#use-the-hosted-gateway) has the `createCluster` and `getKubeconfig` calls.

Read [01](01-kcp-server.md) for the kcp host and the tenant cluster, [02](02-gitlab-runner.md) for
the loopback runner shape, and [03](03-jobs.md) for the job side.

## The topology this is written for

| | |
| --- | --- |
| Gateway API | `https://k8s.socialweb.computer` |
| kcp front | `https://kcp.socialweb.computer` |
| cluster in a kubeconfig | `https://kcp.socialweb.computer/clusters/root:<accountHash>:<cluster>:clusters` |

`<accountHash>` is the first 8 characters of the base36 of `sha256(did)`
(`internal/kcpclient/names.go`, `AccountHash`). The mount path is
`root:<accountHash>:<cluster>:clusters` (`MountPath`).

The kubeconfig's user is a client certificate and its key, made by the cluster's own OpenBao PKI for
the user's DID and valid for the lifetime the user chose in `getKubeconfig` (at most 24 hours, or no
expiry when the user asks for one):

```yaml
users:
  - name: <cluster>
    user:
      client-certificate-data: <PEM>
      client-key-data: <PEM>
```

Plain `kubectl` and client-go use it with nothing installed. The client certificate front at
`kcp.socialweb.computer` admits the certificate while its `ClusterClientCertificate` exists and is not
revoked, and the certificate is cluster-admin in that tenant cluster.

## Where the runner runs

On the GitLab host (`ssh gitlab`).

- It is off the kcp host, which is the point. Nothing about this shape needs the operator's host,
  because nothing needs `127.0.0.1:<leased port>`: the kubeconfig carries the URL and the
  certificate.
- GitLab is there, so the runner's HTTPS to GitLab is direct instead of a network hop.
- A runner is already installed and registered there (see [the GitLab host](#the-gitlab-host)).
- The cluster is a public HTTPS name, so the runner host is the only host that has to reach it.

## Prerequisites

- The GitLab host resolves and reaches `kcp.socialweb.computer` (the cluster).
- A user account, a cluster made through the gateway, and the kubeconfig from `getKubeconfig`. It
  needs no helper on the runner host.
- Outbound HTTPS from the GitLab host to the internet.
- A GitLab project to run jobs in, and a runner registered to it.
- The user is cluster-admin in their own cluster, so there is no RBAC for the operator to grant. See
  [what the user still applies](#what-the-user-still-applies).

## The kubeconfig

`getKubeconfig` returns one kubeconfig file. Put it on the runner host where only the
runner's user can read it:

```bash
sudo install -d -m 0700 /etc/gitlab-runner/tenant-ci
sudo install -m 0600 <cluster>.kubeconfig.yaml /etc/gitlab-runner/tenant-ci/kubeconfig.yaml
```

Check it before pointing a runner at it:

```bash
export KUBECONFIG=/etc/gitlab-runner/tenant-ci/kubeconfig.yaml
kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}'; echo
kubectl auth can-i create pods --namespace gitlab-runner
kubectl auth can-i create pods/exec --namespace gitlab-runner
kubectl get nodes
```

- The server is the kcp mount URL, not an address.
- Both `can-i` calls answer `yes`, and they answer `yes` for everything else too.
- `get nodes` is the resting state: empty. A cluster with nothing in it has no nodes, and a node
  appears only when a Pod cannot be placed. `No resources found` is the correct answer here, not a
  broken cluster.

### The certificate expires, and that is the rotation

The certificate has the lifetime the user chose in `getKubeconfig`, up to 24 hours (the gateway refuses
anything under 60 seconds or over 24 hours), or no expiry when the user asks for a kubeconfig that
never expires. Past the lifetime the client certificate front answers 401 and every job fails at
authentication. Rotation is downloading a new kubeconfig and overwriting the file. Nothing else in the
runner config changes. A revoked certificate is refused at the front within a second.

A long-lived runner wants a kubeconfig with no expiry, and the user revokes it when the runner goes.

### KUBECONFIG is the process environment, not a config.toml key

The Kubernetes executor has no `kubeconfig` setting. It loads one through client-go, and the only
lever is the environment of the `gitlab-runner` process itself. See
[how the executor finds the cluster](#how-the-executor-finds-the-cluster).

Do not reach for `environment = [...]` in `[[runners]]`. That key injects into the build
environment, which is the job container, which is already inside the cluster
(`common/config.go`: "Custom environment variables injected to build environment"). It does not
reach the runner.

## What the user still applies

Two objects, and one of them is optional.

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: gitlab-runner
```

The runner does not create it. `setupBuildNamespace` returns immediately unless
`namespace_per_job = true` (`executors/kubernetes/kubernetes.go`), so with the default the configured
namespace has to exist or every job fails with `namespaces "gitlab-runner" not found`.

Only if the user turns on pause pods, the PriorityClass they name, because `priorityclasses` is
cluster-scoped:

```yaml
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: gitlab-pause
value: -10
globalDefault: false
description: "Pause pods, below job pods, so a job preempts one."
```

### The credential is cluster-admin

The credential this shape uses is the client certificate of the kubeconfig `getKubeconfig` returns.

| Shape from 01 | This shape |
| --- | --- |
| a client certificate from the ClientCA, CommonName and Organizations chosen by the operator | a client certificate from the cluster's OpenBao PKI, CommonName the user's DID, group `socialweb.computer:cluster-owners` |
| the identity, and therefore every RBAC rule, under the operator's control | cluster-admin by construction; the owner group is bound to `cluster-admin` |
| `kubectl auth can-i ... --as=gitlab-runner --as-group=gitlab-runner` to check it | `kubectl auth can-i '*' '*'` answers `yes` |

Two consequences. The whole class of 403s in 02's failure table cannot happen. And a mistake that
RBAC would have caught now succeeds: a job that deletes nodes, rewrites the ClusterRole bindings, or
reads every Secret in the tenant cluster is allowed to. There is no scoped variant of this runner
shape, and the operator does not hold the key that would make one.

## The file

The same file 02 describes (`~/.gitlab-runner/config.toml` on the GitLab host, or
`/etc/gitlab-runner/config.toml` under a service). What differs is the connection block.

```toml
concurrent = 2
check_interval = 3

[[runners]]
  name = "kcp-tenant-kubernetes"
  url = "https://gitlab.dorper-matrix.ts.net:8443/"
  token = "<<runner authentication token>>"
  executor = "kubernetes"
  run_untagged = false
  tag_list = ["kcp-tenant"]

  # Runs AFTER the clone, before the job's script. A STRING, not an array.
  pre_build_script = "if ! command -v pnpm >/dev/null 2>&1 && command -v npm >/dev/null 2>&1; then npm install -g pnpm@10.11.0; fi; true"

  # Kilobytes. Default is 4096, which is 4 MiB and is not enough for a Go test log.
  output_limit = 51200

  [runners.kubernetes]
    # No host, no cert_file, no key_file, no ca_file. The kubeconfig carries all of those,
    # and setting host would make the executor ignore the kubeconfig.
    namespace = "gitlab-runner"
    image_pull_policy = "if-not-present"

    cpu_request = "100m"
    memory_request = "128Mi"
    cpu_limit = "10"
    memory_limit = "20Gi"

    helper_cpu_request = "100m"
    helper_memory_request = "128Mi"
    helper_cpu_limit = "1"
    helper_memory_limit = "1Gi"

    # Seconds for the Pod to reach Running: a rented node boots, then the image pulls.
    poll_timeout = 3600
    cleanup_grace_period_seconds = 30

    [runners.kubernetes.affinity]
      [runners.kubernetes.affinity.pod_anti_affinity]
        [[runners.kubernetes.affinity.pod_anti_affinity.required_during_scheduling_ignored_during_execution]]
          topology_key = "kubernetes.io/hostname"
          [runners.kubernetes.affinity.pod_anti_affinity.required_during_scheduling_ignored_during_execution.label_selector]
            [[runners.kubernetes.affinity.pod_anti_affinity.required_during_scheduling_ignored_during_execution.label_selector.match_expressions]]
              key = "project.runner.gitlab.com/id"
              operator = "Exists"
```

### Every setting, and what each one means

| Key | Value here | Why |
| --- | --- | --- |
| `concurrent` | `2` | Jobs in flight at once. Every concurrent job is a rented node, so this is a cost knob. Set it to the number of nodes the user will pay for. `1` still wedges on a stale job from a cancelled pipeline. |
| `check_interval` | `3` | How often the runner polls GitLab. |
| `run_untagged` / `tag_list` | `false` / `["kcp-tenant"]` | The GitLab host also carries an `instance` runner that is online and `run_untagged`. An untagged job is eligible for it and may never reach this one. Tag this runner and give its projects the tag. |
| `pre_build_script` | pnpm guard | Runs after the clone, before the job's script. A string, not an array. The place the pnpm/corepack guard lives. |
| `output_limit` | `51200` | Kilobytes of trace. 4096 is the runner's own cap, not GitLab's, and 4 MiB is too small for a Go test log. |
| `host` | not set | Empty selects the kubeconfig path. Any non-empty `host` makes the executor take the out-of-cluster path and ignore the kubeconfig completely. |
| `cert_file` / `key_file` / `ca_file` | not set | The kubeconfig carries the client certificate and the CA. |
| `namespace` | `gitlab-runner` | Where job Pods go. Must exist. See [what the user still applies](#what-the-user-still-applies). |
| `image_pull_policy` | `if-not-present` | The node image seeds some images; a rented node's seed list is the image's, not the operator's. |
| `cpu_request` / `memory_request` | `100m` / `128Mi` | The scheduler places on these. A request that fits no rented node size is a Pod that waits forever and a market bill for a node that never gets used. |
| `cpu_limit` / `memory_limit` | `10` / `20Gi` | Limits are enforcement, not placement. |
| `helper_*_request` / `helper_*_limit` | see file | A limit with no request is the request. |
| `poll_timeout` | `3600` | Seconds for the Pod to reach Running. The first Pod waits for the market to win an RFP, boot a VM, join it, and only then pull the image. Minutes, not seconds. |
| anti-affinity | `required` | One job is one rented node, which is the cost model. |
| `[runners.kubernetes.autoscaler]` | not set | Pause pods. See [pause pods](#pause-pods). |

## The GitLab host

- GitLab `19.2.7-ce.0` in a container, `external_url https://gitlab.dorper-matrix.ts.net:8443`,
  ports 80 and 22 published on the host. `gitlab.dorper-matrix.ts.net` is a Tailscale Funnel: the
  name resolves in public DNS and `:8443` answers from off the tailnet.
- `gitlab-runner` `19.2.1` at `/home/johnandersen777/bin/gitlab-runner`, installed from the GitLab
  package repository, config at `~/.gitlab-runner/config.toml` (mode 0600). There is no
  `/etc/gitlab-runner/` on this host; if the runner is moved to a service, create it.
- No kubectl. The recipe needs it for verification only; the runner does not.
- Three runners are registered:

| id | description | type | state |
| --- | --- | --- | --- |
| 1 | `socialwebcomputer` | instance_type | online, `executor = "instance"` with the Fleeting plugin, tag `socialwebcomputer`, `run_untagged = false` |
| 2 | `kcp-tenant-kubernetes` | project_type, project 5 | offline |
| 3 | `kcp-tenant-kubernetes-streamplace` | project_type, project 1 | offline |

Runners 2 and 3 are already registered and already the right shape. They are offline because nothing
is running them: `config.toml` holds runner 1 and nothing else. Runner 1 is a different mechanism: it
rents VMs through the market via `request-vm-ssh` and runs jobs over SSH. It is not this runner, and
the two must not both be eligible for the same job.

## Register

Only if runners 2 and 3 are not the ones being used. This is operator text; do not run it from a
script.

Create the runner in the project's **Settings > CI/CD > Runners**, tag it `kcp-tenant`, clear
"Run untagged jobs", and copy the authentication token. Then, on the GitLab host:

```bash
gitlab-runner register \
  --non-interactive \
  --config ~/.gitlab-runner/config.toml \
  --url https://gitlab.dorper-matrix.ts.net:8443/ \
  --token <runner authentication token> \
  --executor kubernetes \
  --tag-list kcp-tenant \
  --run-untagged=false
```

Then replace the generated `[runners.kubernetes]` block with [the file](#the-file) above. `register`
writes the token and the executor and nothing else useful.

Registering runners 2 and 3 is the same command with a different token against `johnandersen777/kcp-ci-probe`
(id 5) and `streamplace/streamplace` (id 1). Their tokens are already in GitLab and are not in any config
file on this host, so a re-registration issues new ones.

## Run it

`KUBECONFIG` is the runner process's, so it goes in its environment. Under systemd:

```bash
sudo systemctl edit gitlab-runner
```

```ini
[Service]
Environment=KUBECONFIG=/etc/gitlab-runner/tenant-ci/kubeconfig.yaml
```

Or, for a foreground run under the operator's own shell:

```bash
export KUBECONFIG=/etc/gitlab-runner/tenant-ci/kubeconfig.yaml
gitlab-runner run --config ~/.gitlab-runner/config.toml --working-directory /var/lib/gitlab-runner
```

Runner 1 takes only jobs tagged `socialwebcomputer`, so it does not compete for untagged jobs; give
this runner's jobs the `kcp-tenant` tag all the same.

## Verify

Three readings, in order. The first two are free; the third costs a node boot.

**1. The runner decoded its config.** Watch the runner's own output, not GitLab's view of it:

```
FATAL: Service run failed  error=decoding configuration file: toml: line 9
```

Any `FATAL` at startup means the process is gone, and GitLab will still show the runner online.

**2. The kubeconfig answers outside the runner.**

```bash
export KUBECONFIG=/etc/gitlab-runner/tenant-ci/kubeconfig.yaml
kubectl get nodes                 # No resources found
kubectl get ns gitlab-runner      # the namespace the user applied
```

At rest `get nodes` is empty and stays empty: this cluster has no always-on node. Then run the
smallest pipeline that proves the integration,
[03's probe](03-jobs.md#the-smallest-pipeline-that-proves-the-integration), with the tag:

```yaml
prove-placement:
  stage: prove
  tags: [kcp-tenant]
  image: busybox:1.36
  script:
    - echo "running on $(hostname)"
```

**3. A job brings a node up, and the cluster goes back to zero.**

```bash
kubectl get pods -n gitlab-runner -o wide -w
kubectl get nodes -w
```

A working run reads:

```
the job pod exists, Pending, cluster at zero nodes
the scale decision read Provision: intent and no node
a node joins on demand and becomes Ready
the job pod is placed on it and runs
the job finishes success
the job pod is gone
the cluster is back at zero nodes; kubectl get nodes is empty again
```

The gap between "pod Pending" and "node Ready" is a market round trip plus a VM boot, in minutes.
That is the design, not a stall.

## Failure modes

In the order this shape meets them. 02's table still applies for everything about the file itself;
these are the ones this shape adds or sharpens.

| Symptom | Cause |
| --- | --- |
| `invalid configuration: no configuration has been provided` at runner startup | `KUBECONFIG` is not in the runner process's environment. `[[runners]] environment = [...]` does not reach it. |
| `the server has asked for the client to provide credentials` or `Unauthorized` | The certificate expired or was revoked, or the account no longer owns the cluster. Download a new kubeconfig. |
| The runner talks to the wrong cluster, or to a cluster on its own host | `host` is still set somewhere in `[runners.kubernetes]`, or the process has `KUBERNETES_SERVICE_HOST` set because the runner is itself running inside a cluster. With `host` empty the executor tries in-cluster config first and only then the kubeconfig. |
| Job hangs at "Preparing the environment" | The SPDY upgrade to `pods/exec` did not survive the kcp mount. See [the exec path](#the-exec-path-through-the-kcp-mount). |
| `namespaces "gitlab-runner" not found` | The namespace was never applied. The runner does not create it. |
| Job stays `pending`, no node appears | Nothing rented a node, or the market lost the RFP. Compare `kubectl get pods -A` with `kubectl get nodes`; a Pending Pod the scale decision never sees is a provider that is not running. |
| Pod `Pending` for minutes and then Running | Normal. A rented node has to boot. `poll_timeout` has to cover it. |
| Pod `Pending` forever on a node that exists | Requests do not fit the node size the market rented. See 02's trap about a limit being a request. |
| `ImagePullBackOff` after a node boot | The job's image does not exist, or the rented node cannot reach its registry. 03's check costs one curl and saves a node boot. |
| Job fails at the clone with `Could not resolve host` | The node's own CoreDNS is not answering on `169.254.20.10`. `kubectl -n kube-system get pods -l app.kubernetes.io/name=socialweb-node-dns -o wide` shows whether the node runs one; none means the node lacks the `socialweb.computer/node-local-dns=true` label (it joined with an older user_data) or the DaemonSet was never applied (the provider's add-on pass logs `applied the node network add-ons`). See [03](03-jobs.md#pods-on-different-nodes-and-dns-on-every-node). |
| Job fails at the clone with `unable to access '...'` where the DNS did resolve | `CI_REPOSITORY_URL` names a GitLab the rented node cannot reach. A rented node's egress is the provider's, so `gitlab.dorper-matrix.ts.net` is reachable only because it is a public Funnel. Either publish GitLab on a name the public internet resolves, or give the job `GIT_STRATEGY: none`. |
| The cluster never returns to zero | Something is Pending that is not a job Pod. Pause pods are the usual answer; see below. |
| A job's first request is `Forbidden`, ever | It should not be. The certificate is cluster-admin. If this appears, the owner group binding changed and the runner is no longer the thing that broke. |

## Pause pods

[03's pause pods](03-jobs.md#pause-pods-and-why-they-fight-scale-to-zero) apply unchanged, and cost
more here, because a node is rented rather than booted on the operator's own hardware. In 19.2.1 the
shape is:

```toml
    [runners.kubernetes.autoscaler]
      max_pause_pods = 1
      pause_pod_image = "registry.k8s.io/pause:3.10"
      pause_pod_priority_class_name = "gitlab-pause"

      [[runners.kubernetes.autoscaler.policy]]
        idle_count = 0
        idle_time = "1m"
```

A pause pod is a Pending Pod that is not a k3s addon, so the scale decision counts it as intent and
rents a node to place it. The cluster then never returns to zero while one exists. `idle_count = 0`
outside working hours is the way out, and it is a cron on the runner, not a cluster setting.

Do not set a 24x7 `idle_count > 0` on a cluster whose nodes are billed.

## The exec path through the kcp mount

The kubernetes executor does not merely create Pods. It attaches to the build container with an HTTP
upgrade, `POST /api/v1/namespaces/<ns>/pods/<pod>/exec` with `Connection: Upgrade` and
`Upgrade: SPDY/3.1` (`executors/kubernetes/exec.go`). In this shape the request goes runner to
`kcp.socialweb.computer`, through Caddy, into kcp, and out to the tenant apiserver.

The kcp mount is a reverse proxy, and [README.md](README.md#reachability-the-single-operational-fact)
records that a client certificate does not survive it. The credential side is answered by the client
certificate front, which Caddy sends requests with a client certificate to. The upgrade side is
answered by the kcp vhost. `scripts/deploy.sh`
writes that vhost with `flush_interval -1` and a `transport http` block carrying `versions 1.1`, so
Caddy speaks HTTP/1.1 to kcp and the SPDY upgrade survives. Without `versions 1.1` the upgrade dies
at Caddy with `502 Bad Gateway`.

If the upgrade does not survive, the symptom is a job that hangs at "Preparing the environment"
while its Pod sits `Running`: the Pod was created, and the attach never completes.

### How the executor finds the cluster

To check the executor's cluster selection in the source rather than take it on trust:
`executors/kubernetes/util.go`, gitlab-runner v19.2.1:

```go
func getKubeClientConfig(config *common.KubernetesConfig, overwrites *overwrites) (*restclient.Config, error) {
	if config.Host != "" {
		kubeConfig, err = getOutClusterClientConfig(config)
	} else {
		kubeConfig, err = guessClientConfig(config)
	}
	...
}
```

Three facts follow:

- `host = ""` selects the kubeconfig path. Any non-empty `host` takes the other branch and the
  kubeconfig is never read.
- `clientcmd.NewDefaultClientConfigLoadingRules()` is client-go's standard loader: `$KUBECONFIG`,
  then `~/.kube/config`. That is why `KUBECONFIG` is a process environment variable and not a config
  key.
- `InClusterConfig()` is tried before the kubeconfig and wins if it succeeds. On the GitLab host it
  fails, because there is no `KUBERNETES_SERVICE_HOST`. Inside a Pod it would not.
