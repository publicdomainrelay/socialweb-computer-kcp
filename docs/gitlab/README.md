# GitLab on a tenant cluster

This guide puts a GitLab runner on a tenant cluster: a virtual k3s cluster a `socialweb-computer`
gateway provisions, whose nodes are guest VMs rented from the compute market. You run the GitLab
server and the runner on the kcp host; jobs run as Pods on the tenant cluster.

Three steps, one document each:

| Step | Document | What it covers |
| --- | --- | --- |
| 1 | [The kcp server](01-kcp-server.md) | Install the gateway, create the tenant cluster, size the nodes, mint the runner's credential, grant its group |
| 2 | [The GitLab runner](02-gitlab-runner.md) | Install GitLab, register a runner, write its `config.toml`, start it |
| 3 | [Jobs](03-jobs.md) | Job images, pipelines, one job per node, scaling, the cross-node limit |

Step 1 leaves you holding a cluster URL, three credential files and a group name. Step 2 points a
runner at them. Step 3 is what happens when a job runs. [04](04-spa-cluster-runner.md) is the second
runner shape, for a cluster the user made through the gateway and is not on the operator's host.

## Reachability: the single operational fact

A tenant apiserver binds **loopback on the kcp host**. It is not on a network interface, and no
configuration changes that. Two routes reach it, and they are not interchangeable:

| Route | URL | Credential that works |
| --- | --- | --- |
| Direct, host-local | `https://127.0.0.1:<k3s-secure port>` | a client certificate signed by that cluster's ClientCA |
| Through the kcp mount | `https://<kcp-host>/clusters/root:<accountHash>:<cluster>:clusters` | a bearer token only |

The mount is a reverse proxy: kcp terminates TLS and proxies to the apiserver. Only a bearer
credential survives it; a client certificate does not. The mount is the right route for a human with
`kubectl` and a minted token. It is the wrong route for GitLab CI, which needs the Kubernetes exec and
attach paths (`pods/exec`) over an upgraded SPDY connection, and a reverse proxy does not carry them.

Run the GitLab Runner where `127.0.0.1:<port>` is reachable: on the kcp host.

The port is leased per cluster and is not a constant. Read it from the cluster's own status:

```bash
kubectl --kubeconfig <admin> get cluster <name> -o jsonpath='{.status.url}'
```

That value is the `host` you put in the runner config.

### Running GitLab on a separate host

A GitLab on another host cannot reach a loopback-bound apiserver, and no runner setting fixes that.
Making it work means the kcp host publishing an authenticated front for the apiserver, which widens a
trust boundary this project keeps at loopback on purpose: `internal/gatewaymain/config.go` refuses to
bind anything but loopback, and the host's caddy publishes only the gateway API and the kcp front
(`scripts/deploy.sh`). See [01](01-kcp-server.md#gitlab-on-its-own-vm-does-not-work).

## Cross-node traffic and DNS: the newest part

Each market node answers names on its own node-local CoreDNS, and pods reach pods on other nodes through
`socialweb-meshd`. Treat the cross-node path as the least proven part of a market cluster: read
[03](03-jobs.md#pods-on-different-nodes-and-dns-on-every-node) before scaling a cluster past one node
for anything that needs the network.

## Node count and the idle hold

The default ceiling is ten nodes, and `SOCIALWEB_CLUSTERPROVIDER_SCALE_PODS_PER_NODE` defaults to 8;
a ceiling below one is refused rather than clamped. Every node is a booted guest, so both values are a
cost knob. A cluster serving a runner should set `podsPerNode` from a measured job: with pod
anti-affinity, one job Pod occupies one node whatever the setting says.

The runner's `concurrent` may exceed one when the ceiling is above one. Do not leave it at 1: a job
left over from a cancelled pipeline holds the only slot.

Past the idle grace (`SOCIALWEB_CLUSTERPROVIDER_SCALE_HOLD`, default 2m) a cluster with nothing
running drops to zero nodes, and a job arriving after that pays the node boot again. GitLab's
pre-warmed pause pods ([03](03-jobs.md#pause-pods-and-why-they-fight-scale-to-zero)) exist to hide
that latency.
