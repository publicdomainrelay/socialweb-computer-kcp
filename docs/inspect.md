# Inspecting an application kcp

The long form of the README inspection section: the helpers, every state path, the embedded k3s and the ssh access. Short path in [the README](../README.md#inspect-the-kcp-of-an-application).

A production application has its own kcp; the applications of a market network share the network's kcp, one workspace each (`root:<app>`). A kcp admin kubeconfig's server URL ends in `/clusters/<workspace>`; swap the workspace to walk the kcp. Define two helpers:

```bash
kc() { local k=$1 ws=$2; shift 2; local b; b=$(kubectl --kubeconfig $k config view -o jsonpath='{.clusters[0].cluster.server}' | sed 's#/clusters/.*##'); kubectl --kubeconfig $k --server "$b/clusters/$ws" "$@"; }
tree() { local k=$1 ws=${2:-root}; echo "$ws"; for c in $(kc $k $ws get workspaces -o name 2>/dev/null | sed 's#.*/##'); do tree $k "$ws:$c"; done; }
```

Find every kcp and every tenant cluster kubeconfig under a state root:

```bash
STATE=/var/tmp/swcit/<run>                                 # integration runs; a dev instance defaults to $HOME/.local/state/socialweb-computer
find $STATE /var/lib/socialweb-computer -name admin.kubeconfig -path '*kcp/admin.kubeconfig'   # every kcp front kubeconfig
find $STATE /var/lib/socialweb-computer -path '*server/cred/admin.kubeconfig'                   # every tenant k3s kubeconfig
for K in $(find $STATE -path '*kcp/admin.kubeconfig' -not -path '*slot-*' -not -path '*shared*'); do echo "== $K"; tree $K; done
kc $K root api-resources --api-group=socialweb.computer    # the kinds this layer serves
```

- `dev env` prints the network's kcp kubeconfig and ports.
- A child application of a market network has no kcp of its own: walk `root:<app>` of the network's kcp. Its state (component state dirs, its OpenBao) is under the network's state dir, in `apps/`; the run directories of every DenoPod of the network are under `<network state>/denokcp/runs/`, and each names its pod in `service.txt`.
- A production bidder's kcp is under `/var/lib/socialweb-computer/swc-bidder/handles/<hash>/kcp`, and a production gateway's kcp is under its state dir (default `/var/lib/socialweb-computer/socialweb/kcp`). A claim lives in the cluster's workspace: `kc $K root:<acct>:<cluster> get nodeclaims`.
- A production host has no `kubectl`; use the gateway's embedded k3s: `K3S=$(find /var/lib/socialweb-computer/socialweb/runtime -name k3s -type f | head -1); $K3S kubectl ...`.

DenoPod logs live under the run directory (`runs/`), one directory per pod, with `stdout.txt` and `stderr.txt` (gone on restart). The `socialweb-computer` process logs to `journalctl --user -u <unit>`, or use `dev logs core`.

SSH to a production host: `ssh -i ~/.ssh/id_ed25519_no_password -p 1997 root@socialweb.computer`.

## License
Unlicense (public domain).

## See also

- [ARCHITECTURE](ARCHITECTURE.md)
