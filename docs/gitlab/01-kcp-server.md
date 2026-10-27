# 01 - The kcp server

End state: a host running the `socialweb-computer` gateway, one tenant cluster at Ready, nodes sized
for the job, and a runner credential in hand. Nothing here needs GitLab.

Read [README.md](README.md#reachability-the-single-operational-fact) first: the tenant apiserver binds
loopback on this host, so GitLab has to run here.

## 1. Run the gateway

The gateway is the `socialweb-computer` binary under its own lingering account (`socialweb` on the
production host). Install it with `scripts/deploy.sh`:

```bash
BINARY=<path to socialweb-computer> SHA256=<sha256> SSH_KEY=~/.ssh/id_ed25519_no_password \
  GATEWAY_ENV_FILE=~/.tmp/deploy/gateway.env GATEWAY_SPEC_FILE=~/.tmp/deploy/gateway-spec.yaml \
  scripts/deploy.sh
```

`deploy.sh` runs `socialweb-computer install gateway` the first time and `upgrade` after, then waits
until the gateway answers `/api/config`. Check it as the account:

```bash
systemctl --user status socialweb-computer-gateway.service
curl -fsS http://127.0.0.1:8788/api/config
```

## 2. The tenant cluster

The cluster exists before its first node does. That is the point of the design: a cluster is a control
plane, and nodes are rented when a Pod needs one.

```bash
kubectl --kubeconfig <admin> get clusters
kubectl --kubeconfig <admin> get cluster <name> -o jsonpath='{.status.url}'
```

`<admin>` is the kcp admin kubeconfig, named in the `adminKubeconfig` field of the gateway's
`gateway.json`. That URL is `https://127.0.0.1:<leased port>`, and the port is per cluster. It is what
goes in the runner's `host`, and what the runner's certificate is signed against. The admin kubeconfig
never leaves the provider's hands and is not what a runner gets.

## 3. Size the nodes

A node boots at 2 vCPU and 2048 MiB. That fits a probe, not a toolchain. These numbers live in the
guest image the bidder boots, recorded in its manifest as `vcpu` and `memMiB`, so changing either
builds a new image.

### The root filesystem fills before the disk

A job's image and its build output go in the node's ROOT filesystem, where containerd stores layers.
Too small and the image pull dies:

```
failed to pull and unpack image ... no space left on device
```

That is the ROOT filesystem filling, and it arrives as a Pod that never starts. Size the image's root
filesystem for the toolchain and its layers together. A large file staged INTO the image does not help:
`mkfs.ext4 -d` refuses a file of about 2 GiB or more with `Ext2 file too big`. Anything that large is
pulled by the guest at run time instead, see
[03](03-jobs.md#what-the-guest-does-and-does-not-pull).

### The state directory has to be on real disk

Node disks and root filesystems are large files. Where `/tmp` is a tmpfs that is RAM, point the state
directory at the host's real disk:

```bash
TMPDIR=/var/tmp/kcp
```

A state directory that cannot hold a node's disks fails as a full filesystem partway through a boot,
not as a configuration error.

### Check the host before booting anything

```bash
socialweb-computer check-host -role gateway -state-dir <state dir>
```

It prints one line per check with its fix and exits 1 when any check fails (`docs/host-contract.md`).

## 4. Mint the runner's credential

A runner authenticates to the tenant apiserver with a CLIENT CERTIFICATE signed by that cluster's
ClientCA. The credential is valid only against that cluster.

Do not use the superuser. `system:admin` in `system:masters` is cluster-admin on the tenant cluster
and makes every RBAC rule below decorative.

```bash
SOCIALWEB_TENANTCREDENTIAL_STATE_DIR=/var/lib/socialweb \
SOCIALWEB_TENANTCREDENTIAL_CLUSTER=ci \
SOCIALWEB_TENANTCREDENTIAL_SERVER="$(kubectl --kubeconfig <admin> get cluster ci -o jsonpath='{.status.url}')" \
SOCIALWEB_TENANTCREDENTIAL_COMMON_NAME=gitlab-runner \
SOCIALWEB_TENANTCREDENTIAL_ORGANIZATIONS=gitlab-runner \
SOCIALWEB_TENANTCREDENTIAL_OUT_DIR=/etc/gitlab-runner/tenant-ci \
SOCIALWEB_TENANTCREDENTIAL_VALID_FOR=720h \
  socialweb-computer internal tenantcredential
```

It writes three files at mode 0600:

| File | What |
| --- | --- |
| `ca.crt` | the cluster's serving CA, what the apiserver presents at that URL |
| `tls.crt` | the runner's client cert; CommonName is the username, Organizations are the groups |
| `tls.key` | the private key |

`VALID_FOR` is optional and defaults to the mint's own validity. The credential has no revocation path,
so its lifetime is how long a leaked copy is useful. To rotate, move the old files aside first: the
tool refuses to overwrite.

A scoped identity can only come from the ClientCA on disk. The OpenBao-backed path's PKI role pins the
organization to `system:masters`, so a refused `COMMON_NAME` or `ORGANIZATIONS` is that distinction
and not a typo. In Go the scoped entry point is `clusterprovider.MintTenantCredentialForClusterAs`,
which signs directly and so dodges the vault.

## 5. Permit that group in the tenant cluster

Apply against the TENANT cluster. The namespace first, because the runner does not create it.

```bash
kubectl --kubeconfig <tenant-admin> apply -f - <<'YAML'
apiVersion: v1
kind: Namespace
metadata:
  name: gitlab-runner
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: gitlab-runner
rules:
  - apiGroups: [""]
    resources: ["pods", "pods/exec", "pods/attach", "pods/log"]
    verbs: ["create", "delete", "get", "list", "watch", "patch", "update"]
  - apiGroups: [""]
    resources: ["secrets", "configmaps", "services", "serviceaccounts", "events", "persistentvolumeclaims"]
    verbs: ["create", "delete", "get", "list", "watch", "update"]
  - apiGroups: ["apps"]
    resources: ["deployments"]
    verbs: ["create", "delete", "get", "list", "update"]
  - apiGroups: ["policy"]
    resources: ["poddisruptionbudgets"]
    verbs: ["create", "delete", "get", "list", "update"]
  - apiGroups: ["scheduling.k8s.io"]
    resources: ["priorityclasses"]
    verbs: ["get", "create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: gitlab-runner
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: gitlab-runner
subjects:
  - kind: Group
    name: gitlab-runner
    apiGroup: rbac.authorization.k8s.io
YAML
```

`Group` is the `ORGANIZATIONS` value from step 4. Change one, change the other. When they disagree,
every job fails with a 403 that reads as a broken runner.

`priorityclasses` is cluster-scoped, so a namespaced Role cannot grant it. `apps/deployments` and
`scheduling.k8s.io/priorityclasses` are only needed by the pre-warmed pause pods in
[03](03-jobs.md#pause-pods-and-why-they-fight-scale-to-zero).

Check both halves before blaming the runner:

```bash
openssl x509 -in /etc/gitlab-runner/tenant-ci/tls.crt -noout -subject
# subject=O = gitlab-runner, CN = gitlab-runner

kubectl --kubeconfig <tenant-admin> auth can-i create pods \
  --namespace gitlab-runner --as=gitlab-runner --as-group=gitlab-runner
# yes
```

## Hand-back

What [02](02-gitlab-runner.md) needs from this document:

- the apiserver URL, `https://127.0.0.1:<leased port>`
- `/etc/gitlab-runner/tenant-ci/{ca.crt,tls.crt,tls.key}`
- the group name, `gitlab-runner`
- the namespace, `gitlab-runner`

## GitLab on its own VM does not work

A GitLab on a separate host cannot reach the tenant apiserver, because the apiserver binds loopback on
the kcp host. The runner reports connection timeouts, and no runner configuration fixes it.

Making it work means the kcp host publishing an authenticated front for the apiserver. That is a
deliberate trust-boundary change, not a config step: the gateway refuses to bind anything but loopback
on purpose, and the entrypoint publishes exactly one port whose only upstream is the gateway. Decide
it deliberately.
