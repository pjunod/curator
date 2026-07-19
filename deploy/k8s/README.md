# deploy/k8s/

Worked manifests for the **floating single instance** pattern from
[docs/deployment.md](../../docs/deployment.md): exactly one Monarr pod that
kube reschedules freely across nodes, with the SQLite database following it —
so a node failure means a sub-minute reschedule, not data loss and not a
pinned workload.

| File | Pattern | Needs |
|---|---|---|
| `monarr.yaml` | `/data` on replicated **block** storage; the volume reattaches wherever the pod lands | Longhorn / Ceph RBD / DRBD storage class |
| `monarr-litestream.yaml` | `/data` on node-local scratch, continuously replicated to S3; an init container restores on reschedule — **no node-local dependency at all** | any S3 endpoint (minio, garage, B2, S3) |

Both keep `replicas: 1` + `strategy: Recreate` — the automation loops are a
single-writer design (like upstream Sonarr/Radarr), so the goal is a fast-
moving single instance, not multiple concurrent ones.

Shared rules:

- **Never** put `/data` on NFS or GlusterFS — shared-file locking corrupts
  WAL SQLite. The media **pool is fine** on a shared filesystem; mount it
  however every node can see it.
- Edit before applying: the image reference, the storage class (or S3
  endpoint + credentials), and the pool volume.
- In the Litestream variant, S3 *is* the durable copy — Monarr's own
  `backup.run` snapshots land on the scratch volume and vanish with it,
  which is fine, but point `restore` drills at the S3 replica.

Restore drill (litestream variant), before you need it:

```sh
kubectl exec deploy/monarr -c litestream -- \
  litestream restore -config /etc/litestream.yml -o /tmp/check.db /data/monarr.db
```
