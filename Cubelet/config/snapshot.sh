#!/bin/sh
#64Mi+128Mi
cube-runtime   snapshot --path /usr/local/services/cubetoolbox/cube-snapshot/1C238M --resource '{"cpu": 1, "memory": 238}' --disk '[{"path": "/data/cube-shim/disks/512Mi-ext4/disk0.raw", "fs_type":"ext4", "size":536870912}]' --force
#64Mi+256Mi
cube-runtime   snapshot --path /usr/local/services/cubetoolbox/cube-snapshot/1C368M --resource '{"cpu": 1, "memory": 368}' --disk '[{"path": "/data/cube-shim/disks/512Mi-ext4/disk0.raw", "fs_type":"ext4", "size":536870912}]' --force
#64Mi+384Mi
cube-runtime   snapshot --path /usr/local/services/cubetoolbox/cube-snapshot/1C499M --resource '{"cpu": 1, "memory": 499}' --disk '[{"path": "/data/cube-shim/disks/512Mi-ext4/disk0.raw", "fs_type":"ext4", "size":536870912}]' --force
#64Mi+512Mi
cube-runtime   snapshot --path /usr/local/services/cubetoolbox/cube-snapshot/1C629M --resource '{"cpu": 1, "memory": 629}' --disk '[{"path": "/data/cube-shim/disks/512Mi-ext4/disk0.raw", "fs_type":"ext4", "size":536870912}]' --force
#64Mi+768Mi
cube-runtime   snapshot --path /usr/local/services/cubetoolbox/cube-snapshot/1C890M --resource '{"cpu": 1, "memory": 890}' --disk '[{"path": "/data/cube-shim/disks/512Mi-ext4/disk0.raw", "fs_type":"ext4", "size":536870912}]' --force
#64Mi+1024Mi
cube-runtime   snapshot --path /usr/local/services/cubetoolbox/cube-snapshot/1C1150M --resource '{"cpu": 1, "memory": 1150}' --disk '[{"path": "/data/cube-shim/disks/512Mi-ext4/disk0.raw", "fs_type":"ext4", "size":536870912}]' --force
#64Mi+2048Mi
cube-runtime   snapshot --path /usr/local/services/cubetoolbox/cube-snapshot/2C2192M --resource '{"cpu": 2, "memory": 2192}' --disk '[{"path": "/data/cube-shim/disks/512Mi-ext4/disk0.raw", "fs_type":"ext4", "size":536870912}]' --force


touch /run/cube-shim/snapshot
