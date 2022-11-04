# volume-nfs-provisioner
Dynamically provisioned NFS exports for Kubernetes block volumes

## Overview
This project aims to provide "per-volume" nfs export for block volumes.

NFS failover is handled by a Kubernetes Pod.

## Diagrams
Data Plane:
```
+-------+       +-------+        +-------+
| nginx |       | nginx |        | nginx |
|  pod1 |       |  pod2 |        |  pod3 |
+---+---+       +---+---+        +---+---+
    ^               ^                ^
    |               |                |
    |          +----+-----+          |
    +----------+  NFS PVC +----------+
               +----^-----+
                    |
                +---+----+
                | NFS PV |
                +---^----+
                    |
             +------+-------+
             | (cluster ip) |
             |              |
             |    NFS POD   |
             +------^-------+
                    |
               +----+-----+
               | DATA PVC |
               +----^-----+
                    |
               +----+----+
               | DATA PV |
               +---------+
```

Control Plane (dynamic provisioning):
```
                                              +--------+
                         +---------------+    | Block  |
                    +--->+ ReadWriteOnce +--->+ Volume +-------------------------------+
                    |    +---------------+    +--------+                               |
                    |                                                                  |
                    |                                                                  v
+-----+    +--------+                                                                +-+--+
| PVC +--->+ Access |                                                                | PV |
+-----+    |  Mode? |                                                                +-+--+
           +--------+                                                                  ^
                    |                                                                  |
                    |                         +--------+    +--------+    +--------+   |
                    |    +---------------+    | Block  |    | NFS    |    | NFS    |   |
                    +--->+ ReadWriteMany +--->+ Volume +--->+ Export +--->+ Volume +---+
                         +---------------+    +--------+    +--------+    +--------+
```
