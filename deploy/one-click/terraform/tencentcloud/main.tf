terraform {
  required_providers {
    tencentcloud = {
      source = "tencentcloudstack/tencentcloud"
      # Pin to the 1.x line so a future breaking major (2.0) is never pulled
      # in automatically. The lock file (.terraform.lock.hcl) is intentionally
      # gitignored and stripped from release bundles, so providers are
      # re-resolved within this range on each machine; tighten this constraint
      # here if you need bit-for-bit reproducible plugin versions.
      version = ">= 1.81.0, < 2.0.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.0"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.0"
    }
    local = {
      source  = "hashicorp/local"
      version = "~> 2.0"
    }
    null = {
      source  = "hashicorp/null"
      version = "~> 3.0"
    }
  }
}

# kubernetes provider: reads the kubeconfig from .kube/config
# Phase 1: deploy_tke_addons=false → all k8s resources have count=0, provider does not connect
# Phase 2: deploy_tke_addons=true → provider reads the existing kubeconfig
# On destroy: destroy.sh writes the kubeconfig first
provider "kubernetes" {
  config_path = "${path.module}/.kube/config"
}

resource "random_string" "deploy_suffix" {
  length  = 6
  special = false
  upper   = false
}

provider "tencentcloud" {
  region = var.region
}

########################
# Data sources
########################

# Availability zones
data "tencentcloud_availability_zones_by_product" "default" {
  product = "cvm"
}

# OS image
data "tencentcloud_images" "default" {
  image_type       = ["PUBLIC_IMAGE"]
  image_name_regex = var.image_name_regex
}

# Instance type lookup (used for interactive selection, covering common types)
data "tencentcloud_instance_types" "spec_8c16g" {
  filter {
    name   = "instance-family"
    values = ["S5", "S4", "S3", "S2", "SA2", "SA3", "SA4", "SA5", "C5", "C4", "C3", "M5", "M4", "M3"]
  }
  cpu_core_count   = 8
  memory_size      = 16
  exclude_sold_out = true
}

data "tencentcloud_instance_types" "spec_4c8g" {
  filter {
    name   = "instance-family"
    values = ["S5", "S4", "S3", "S2", "SA2", "SA3", "SA4", "SA5", "C5", "C4", "C3", "M5", "M4", "M3"]
  }
  cpu_core_count   = 4
  memory_size      = 8
  exclude_sold_out = true
}

data "tencentcloud_instance_types" "spec_2c4g" {
  filter {
    name   = "instance-family"
    values = ["S5", "S4", "S3", "S2", "SA2", "SA3", "SA4", "SA5", "C5", "C4", "C3", "M5", "M4", "M3"]
  }
  cpu_core_count   = 2
  memory_size      = 4
  exclude_sold_out = true
}

########################
# Local variables: prefer explicit configuration, otherwise auto-detect
########################

locals {
  # DescribeZones (via tencentcloud_availability_zones_by_product) still lists
  # legacy/retired zones the region keeps for backward compatibility — notably
  # ap-guangzhou-1 / ap-guangzhou-2 — with state != "AVAILABLE". They sort FIRST
  # by zone id, so a blind zones[0] / full-list selection lands on a dead zone and
  # any CVM/MySQL placed there fails the apply with InvalidZone.MismatchRegion.
  # Keep only zones the API reports AVAILABLE so the auto-selected primary/slave
  # zone and the online zone menu (query_outputs.tf "_zones") never pick one. Fall
  # back to the raw list only if the API ever reports zero AVAILABLE zones, so a
  # surprising response degrades to the old behavior instead of erroring on an
  # empty list.
  all_zones       = data.tencentcloud_availability_zones_by_product.default.zones
  _avail_zones    = [for z in local.all_zones : z if z.state == "AVAILABLE"]
  available_zones = length(local._avail_zones) > 0 ? local._avail_zones : local.all_zones

  primary_zone = var.availability_zone != "" ? var.availability_zone : local.available_zones[0].name

  # MySQL two-node multi-AZ deployments require first_slave_zone != availability_zone.
  # Pick the first AVAILABLE zone that is different from the explicit primary zone;
  # the old available_zones[1] shortcut breaks when primary itself is the second
  # available zone, e.g. primary=ap-guangzhou-6 and zones=[5,6,7].
  mysql_slave_candidates = [for z in local.available_zones : z.name if z.name != local.primary_zone]
  mysql_slave_zone       = length(local.mysql_slave_candidates) > 0 ? local.mysql_slave_candidates[0] : local.primary_zone

  jumpserver_zone = var.jumpserver_availability_zone != "" ? var.jumpserver_availability_zone : local.primary_zone
  compute_zone    = var.compute_availability_zone != "" ? var.compute_availability_zone : local.primary_zone
  tke_worker_zone = var.tke_worker_availability_zone != "" ? var.tke_worker_availability_zone : local.primary_zone

  tke_worker_type = var.tke_worker_instance_type

  compute_zones = [
    for i in range(var.compute_node_count) :
    length(var.compute_availability_zones) > i && var.compute_availability_zones[i] != "" ?
    var.compute_availability_zones[i] : local.compute_zone
  ]

  compute_types = [
    for i in range(var.compute_node_count) :
    length(var.compute_instance_types) > i && var.compute_instance_types[i] != "" ?
    var.compute_instance_types[i] : var.compute_instance_type
  ]

  # Extra subnets for CVM roles placed outside the primary zone (same VPC).
  extra_cvm_zones = toset(compact(concat(
    [
      local.jumpserver_zone != local.primary_zone ? local.jumpserver_zone : "",
      local.tke_worker_zone != local.primary_zone ? local.tke_worker_zone : "",
    ],
    [for z in local.compute_zones : z != local.primary_zone ? z : ""]
  )))

  # Assign each extra CVM zone a stable, non-overlapping /24. Offsetting the
  # third octet by 10 guarantees these can never collide with the primary
  # subnet's fixed 10.0.1.0/24, and avoids the previous fragile scheme that
  # derived the octet from the zone-name suffix (which produced 10.0.1.0/24 —
  # a hard collision — whenever a non-primary role landed in a zone ending -1).
  extra_cvm_zone_list = sort(tolist(local.extra_cvm_zones))
  cvm_subnet_cidrs = {
    for idx, z in local.extra_cvm_zone_list : z => cidrsubnet("10.0.0.0/16", 8, idx + 10)
  }

  # Multi-AZ MySQL requires at least two AVAILABLE availability zones; in single-AZ
  # regions it must fall back to a single-zone deployment or the apply fails.
  mysql_multi_az = length(local.available_zones) > 1

  jumpserver_subnet_id = local.jumpserver_zone == local.primary_zone ? tencentcloud_subnet.cluster.id : tencentcloud_subnet.cvm[local.jumpserver_zone].id
  tke_worker_subnet_id = local.tke_worker_zone == local.primary_zone ? tencentcloud_subnet.cluster.id : tencentcloud_subnet.cvm[local.tke_worker_zone].id

  tke_node_pool_subnet_ids = distinct(compact([
    tencentcloud_subnet.cluster.id,
    local.tke_worker_zone != local.primary_zone ? tencentcloud_subnet.cvm[local.tke_worker_zone].id : "",
  ]))
}

########################
# SSH key: automatically reads the local ./.ssh/id_rsa.pub
########################

resource "tencentcloud_key_pair" "cluster" {
  key_name   = "cs_cluster_${random_string.deploy_suffix.result}"
  public_key = file(pathexpand(var.ssh_public_key_path))
}

########################
# Network
########################

resource "tencentcloud_vpc" "cluster" {
  name       = var.vpc_name
  cidr_block = "10.0.0.0/16"
}

resource "tencentcloud_subnet" "cluster" {
  vpc_id            = tencentcloud_vpc.cluster.id
  name              = "cubesandbox-cluster-subnet"
  cidr_block        = "10.0.1.0/24"
  availability_zone = local.primary_zone
}

# Per-zone subnets for CVM roles that land outside the primary zone (same VPC).
resource "tencentcloud_subnet" "cvm" {
  for_each = local.cvm_subnet_cidrs

  vpc_id            = tencentcloud_vpc.cluster.id
  name              = "cubesandbox-cvm-${replace(each.key, "-", "")}"
  cidr_block        = each.value
  availability_zone = each.key
}

########################
# NAT gateway — the whole VPC reaches the public network through this gateway
########################

resource "tencentcloud_eip" "nat" {
  name                       = "cubesandbox-nat-eip"
  internet_charge_type       = "TRAFFIC_POSTPAID_BY_HOUR"
  internet_max_bandwidth_out = 200
}

resource "tencentcloud_nat_gateway" "cluster" {
  name             = "cubesandbox-nat"
  vpc_id           = tencentcloud_vpc.cluster.id
  bandwidth        = 200
  max_concurrent   = 1000000
  assigned_eip_set = [tencentcloud_eip.nat.public_ip]
}

# Route: 0.0.0.0/0 → NAT gateway
resource "tencentcloud_route_table_entry" "nat" {
  route_table_id         = tencentcloud_vpc.cluster.default_route_table_id
  destination_cidr_block = "0.0.0.0/0"
  next_type              = "NAT"
  next_hub               = tencentcloud_nat_gateway.cluster.id
}

########################
# Security groups (per-role, least privilege)
#
# A single shared security group used to front every role (jumpserver, compute
# nodes, TKE workers and the CLBs), which meant e.g. a compute node inherited the
# public 443/80/3000 ingress it never needs. The group is now split per role so
# each only opens what it actually requires, and compromising one role no longer
# grants the inbound surface of the others.
########################

# --- 1. Jumpserver: public SSH on 443 + VPC internal ---
resource "tencentcloud_security_group" "jumpserver" {
  name        = "cubesandbox-sg-jumpserver"
  description = "CubeSandbox jumpserver: public SSH (443) + VPC internal"
}

resource "tencentcloud_security_group_rule_set" "jumpserver" {
  security_group_id = tencentcloud_security_group.jumpserver.id

  ingress {
    action      = "ACCEPT"
    cidr_block  = "0.0.0.0/0"
    protocol    = "TCP"
    port        = "443"
    description = "Allow jump-server SSH (cloud-init moves sshd to 443)"
  }

  ingress {
    action      = "ACCEPT"
    cidr_block  = "10.0.0.0/16"
    protocol    = "ALL"
    port        = "ALL"
    description = "Allow VPC internal traffic"
  }

  egress {
    action      = "ACCEPT"
    cidr_block  = "0.0.0.0/0"
    protocol    = "ALL"
    port        = "ALL"
    description = "Allow all outbound"
  }
}

# --- 2. Compute nodes: TKE pod CIDR + VPC internal only (no public ingress) ---
resource "tencentcloud_security_group" "compute" {
  name        = "cubesandbox-sg-compute"
  description = "CubeSandbox compute nodes: TKE pod CIDR + VPC internal only"
}

resource "tencentcloud_security_group_rule_set" "compute" {
  security_group_id = tencentcloud_security_group.compute.id

  # cube-proxy runs as a TKE pod (GlobalRouter, no hostNetwork), so its traffic to
  # a compute node arrives sourced from the pod CIDR. It reaches each sandbox on a
  # dynamic host port (20000-29999) on the compute node's private VPC IP, so the
  # full range is opened with ALL. Bound to var.tke_cluster_cidr (not a literal) so
  # the rule keeps matching if the TKE pod network is reconfigured.
  ingress {
    action      = "ACCEPT"
    cidr_block  = var.tke_cluster_cidr
    protocol    = "ALL"
    port        = "ALL"
    description = "Allow TKE cube-proxy (pod CIDR) -> compute node all ports"
  }

  ingress {
    action      = "ACCEPT"
    cidr_block  = "10.0.0.0/16"
    protocol    = "ALL"
    port        = "ALL"
    description = "Allow VPC internal traffic (jumpserver management, cube-master scheduling)"
  }

  egress {
    action      = "ACCEPT"
    cidr_block  = "0.0.0.0/0"
    protocol    = "ALL"
    port        = "ALL"
    description = "Allow all outbound"
  }
}

# --- 3. TKE workers (pod hosts): pod-to-pod + VPC internal only ---
resource "tencentcloud_security_group" "tke_pod" {
  name        = "cubesandbox-sg-tke-pod"
  description = "CubeSandbox TKE workers: pod-to-pod + VPC internal"
}

resource "tencentcloud_security_group_rule_set" "tke_pod" {
  security_group_id = tencentcloud_security_group.tke_pod.id

  # Pod-to-pod traffic within the cluster arrives sourced from the TKE pod CIDR
  # (GlobalRouter). Workers carry no public IP, so no public ingress is needed.
  ingress {
    action      = "ACCEPT"
    cidr_block  = var.tke_cluster_cidr
    protocol    = "ALL"
    port        = "ALL"
    description = "Allow pod-to-pod communication (TKE pod CIDR)"
  }

  # VPC internal covers CLB health checks (pass-to-target reaches pods from a VPC
  # address), jumpserver management, and the cube-master CFS NFS mount.
  ingress {
    action      = "ACCEPT"
    cidr_block  = "10.0.0.0/16"
    protocol    = "ALL"
    port        = "ALL"
    description = "Allow VPC internal traffic (CLB health checks, jumpserver, CFS NFS)"
  }

  egress {
    action      = "ACCEPT"
    cidr_block  = "0.0.0.0/0"
    protocol    = "ALL"
    port        = "ALL"
    description = "Allow all outbound"
  }
}

# --- 4. CLB (load balancers): only the public-facing service ports ---
resource "tencentcloud_security_group" "clb" {
  name        = "cubesandbox-sg-clb"
  description = "CubeSandbox CLB: public-facing ports for cube services"
}

resource "tencentcloud_security_group_rule_set" "clb" {
  security_group_id = tencentcloud_security_group.clb.id

  # cube-proxy (80/443) + cube-webui (80) + cube-api (3000) front-end ingress.
  # When enable_public_network is true the CLBs get public VIPs, so open these
  # to the internet; when false (default) the CLBs are VPC-internal only, so
  # scope the ingress to the VPC CIDR. Keep this in sync with the per-Service
  # internal-subnetid annotations in tke-addons.tf.
  ingress {
    action      = "ACCEPT"
    cidr_block  = var.enable_public_network ? "0.0.0.0/0" : "10.0.0.0/16"
    protocol    = "TCP"
    port        = "80"
    description = "Allow CLB HTTP (cube-proxy + cube-webui)"
  }

  ingress {
    action      = "ACCEPT"
    cidr_block  = var.enable_public_network ? "0.0.0.0/0" : "10.0.0.0/16"
    protocol    = "TCP"
    port        = "443"
    description = "Allow CLB HTTPS (cube-proxy)"
  }

  ingress {
    action      = "ACCEPT"
    cidr_block  = var.enable_public_network ? "0.0.0.0/0" : "10.0.0.0/16"
    protocol    = "TCP"
    port        = "3000"
    description = "Allow cube-api CLB (jumpserver public access)"
  }

  # cube-master is exposed through an INTERNAL (VPC-only) CLB, so 8089 never needs
  # to be reachable from the public internet. Scope it to the VPC CIDR.
  ingress {
    action      = "ACCEPT"
    cidr_block  = "10.0.0.0/16"
    protocol    = "TCP"
    port        = "8089"
    description = "Allow cube-master CLB (VPC-internal only)"
  }

  egress {
    action      = "ACCEPT"
    cidr_block  = "0.0.0.0/0"
    protocol    = "ALL"
    port        = "ALL"
    description = "Allow CLB -> backend (pod/node)"
  }
}

########################
# Cloud File Storage (CFS) — shared persistent storage for cube-master
#   The cube-master Deployment runs multiple replicas that must share the same
#   template / snapshot / runtime state under /data/CubeMaster/storage, so the
#   backing volume has to be ReadWriteMany. A CBS disk (ReadWriteOnce) cannot
#   attach to multiple pods/nodes at once, so we use a CFS NFS share instead.
#
#   storage_type = "SD" is CFS "General Standard" — an elastic,
#   pay-as-you-go NFS file system that needs no pre-provisioned capacity
#   (`capacity` is required only for the Turbo series, so it is left unset here).
#   No fixed quota is set on the Kubernetes side either: the cube-master pod
#   mounts this share as a plain in-tree NFS volume (no PVC), so the share simply
#   grows with usage.
########################

resource "tencentcloud_cfs_access_group" "cubemaster_data" {
  count       = var.use_cfs ? 1 : 0
  name        = "cubesandbox-cubemaster-${random_string.deploy_suffix.result}"
  description = "Allow the CubeSandbox VPC to mount the cube-master CFS share"
}

# The NFS mount is performed by the TKE worker node (kubelet), whose client
# address is its VPC private IP — so authorize the whole VPC CIDR. no_root_squash
# keeps root so cube-master (running as root) owns the files it writes.
resource "tencentcloud_cfs_access_rule" "cubemaster_data" {
  count           = var.use_cfs ? 1 : 0
  access_group_id = tencentcloud_cfs_access_group.cubemaster_data[0].id
  auth_client_ip  = "10.0.0.0/16"
  priority        = 1
  rw_permission   = "RW"
  user_permission = "no_root_squash"
}

resource "tencentcloud_cfs_file_system" "cubemaster_data" {
  count             = var.use_cfs ? 1 : 0
  name              = "cubesandbox-cubemaster-data"
  availability_zone = local.primary_zone
  access_group_id   = tencentcloud_cfs_access_group.cubemaster_data[0].id
  protocol          = "NFS"
  storage_type      = "SD"
  vpc_id            = tencentcloud_vpc.cluster.id
  subnet_id         = tencentcloud_subnet.cluster.id
}

########################
# Cloud Database MySQL (optional)
########################

resource "tencentcloud_mysql_instance" "mysql" {
  instance_name  = "cubesandbox-mysql"
  engine_version = "8.0"
  engine_type    = "InnoDB"
  device_type    = "UNIVERSAL" # general purpose
  charge_type    = "POSTPAID"  # pay-as-you-go

  root_password    = var.mysql_root_password
  mem_size         = 4000
  volume_size      = 200
  internet_service = 0 # do not enable public network

  # High availability: across availability zones when the region has >= 2 zones,
  # otherwise a single-AZ deployment (multi-AZ would fail in single-zone regions).
  availability_zone = local.primary_zone
  first_slave_zone  = local.mysql_slave_zone
  slave_deploy_mode = local.mysql_multi_az ? 1 : 0 # 1 = multi-AZ, 0 = single-AZ
  slave_sync_mode   = 1                            # semi-sync

  vpc_id        = tencentcloud_vpc.cluster.id
  subnet_id     = tencentcloud_subnet.cluster.id
  intranet_port = 3306

  parameters = {
    character_set_server = "utf8mb4"
  }

  tags = {
    CubeSandboxRole = "mysql"
  }
}

# MySQL application account (CubeSandbox)
resource "tencentcloud_mysql_account" "cube" {
  mysql_id = tencentcloud_mysql_instance.mysql.id
  name     = var.cube_user
  password = var.cube_password
  host     = "%"
}

# Database-level privileges (after mysql_init_db creates var.cube_db — granting on a
# missing database races with the local-exec CREATE DATABASE and can fail apply).
resource "tencentcloud_mysql_privilege" "cube" {
  depends_on = [null_resource.mysql_init_db]

  mysql_id     = tencentcloud_mysql_instance.mysql.id
  account_name = tencentcloud_mysql_account.cube.name
  account_host = tencentcloud_mysql_account.cube.host
  global       = ["SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "DROP", "INDEX", "ALTER", "CREATE TEMPORARY TABLES", "LOCK TABLES", "CREATE VIEW", "SHOW VIEW", "CREATE ROUTINE", "ALTER ROUTINE", "EXECUTE"]
  database {
    database_name = var.cube_db
    privileges    = ["SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "DROP", "INDEX", "ALTER", "CREATE TEMPORARY TABLES", "LOCK TABLES", "CREATE VIEW", "SHOW VIEW", "CREATE ROUTINE", "ALTER ROUTINE", "EXECUTE"]
  }
}

# Create the database (via the jumpserver)
resource "null_resource" "mysql_init_db" {
  depends_on = [tencentcloud_mysql_account.cube, tencentcloud_instance.jumpserver]

  # Re-run the init when the target database name changes OR the MySQL instance
  # is replaced (new id / intranet IP). Without the id/ip triggers a replaced
  # instance would keep the old trigger value and never get the application
  # database created on it (cube-master would then fail to connect). var.cube_db
  # is validated to a bare SQL identifier, so it is safe to inline below.
  triggers = {
    cube_db  = var.cube_db
    mysql_id = tencentcloud_mysql_instance.mysql.id
    mysql_ip = tencentcloud_mysql_instance.mysql.intranet_ip
  }

  provisioner "local-exec" {
    # Pass the root password through the process ENVIRONMENT instead of
    # interpolating it into the command string, so it lands in neither the local
    # `local-exec` argv nor terraform's rendered plan command, and a password
    # containing a single quote cannot break the shell quoting. It is then piped
    # over stdin to the jump-server (read back by $(cat), exported as MYSQL_PWD)
    # so it never appears in the remote argv / `ps` output either (CWE-214).
    environment = {
      MYSQL_ROOT_PW = var.mysql_root_password
    }
    # The jumpserver runs cloud-init on first boot (switching SSH to port 443),
    # which can take a few minutes. Retry the SSH+mysql command until the
    # jumpserver is reachable instead of failing immediately with
    # "kex_exchange_identification: Connection closed by remote host".
    command = <<-EOT
      for i in $(seq 1 30); do
        if printf '%s' "$MYSQL_ROOT_PW" | ssh -i ${pathexpand(var.ssh_private_key_path)} \
          -p 443 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
          -o ConnectTimeout=10 -o BatchMode=yes \
          root@${tencentcloud_instance.jumpserver.public_ip} \
          "MYSQL_PWD=\"\$(cat)\" mysql -h'${tencentcloud_mysql_instance.mysql.intranet_ip}' -P3306 -uroot -e 'CREATE DATABASE IF NOT EXISTS ${var.cube_db} CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci' 2>&1"; then
          echo "${var.cube_db} database ready"
          exit 0
        fi
        echo "jumpserver SSH/MySQL not ready yet, retry $i/30..."
        sleep 10
      done
      echo "ERROR: could not create the ${var.cube_db} database via the jumpserver after retries" >&2
      exit 1
    EOT
  }
}

########################
# Cloud Database Redis
########################

resource "tencentcloud_redis_instance" "redis" {

  name              = "cubesandbox-redis"
  availability_zone = local.primary_zone
  type_id           = 9 # Redis 7.0 standard architecture (master/replica)
  mem_size          = var.redis_mem_size
  vpc_id            = tencentcloud_vpc.cluster.id
  subnet_id         = tencentcloud_subnet.cluster.id
  charge_type       = "POSTPAID"
  port              = 6379
  password          = var.redis_password

  tags = {
    CubeSandboxRole = "redis"
  }
}

########################
# TKE Kubernetes cluster (optional)
########################

resource "tencentcloud_kubernetes_cluster" "tke" {
  count                      = var.create_tke ? 1 : 0
  cluster_name               = var.tke_cluster_name
  cluster_version            = var.tke_cluster_version
  cluster_cidr               = var.tke_cluster_cidr
  service_cidr               = var.tke_service_cidr
  vpc_id                     = tencentcloud_vpc.cluster.id
  cluster_deploy_type        = "MANAGED_CLUSTER"
  cluster_level              = "L5"
  cluster_internet           = false # kube-apiserver: NO public-network access
  cluster_intranet           = true  # kube-apiserver: intranet (VPC-internal) access only
  cluster_intranet_subnet_id = tencentcloud_subnet.cluster.id
  cluster_max_pod_num        = 256
  cluster_max_service_num    = 4096 # matches the number of IPs in service_cidr /20
  network_type               = "GR"
  container_runtime          = "containerd"
  deletion_protection        = false

  # Enabling kube-apiserver access (intranet here) requires at least 1 worker
  # node. Count is driven by var.tke_node_count (TENCENTCLOUD_TKE_NODE_COUNT).
  worker_config {
    count                = var.tke_node_count
    availability_zone    = local.tke_worker_zone
    instance_type        = local.tke_worker_type
    instance_charge_type = "POSTPAID_BY_HOUR"
    system_disk_type     = "CLOUD_BSSD"
    system_disk_size     = 50
    subnet_id            = local.tke_worker_subnet_id
    public_ip_assigned   = false
    security_group_ids   = [tencentcloud_security_group.tke_pod.id]
    key_ids              = [tencentcloud_key_pair.cluster.id]
  }

  tags = {
    CubeSandboxRole = "tke"
  }
}

resource "tencentcloud_kubernetes_node_pool" "tke" {
  # Disabled: the managed TKE cluster already creates the requested initial workers via worker_config.count.
  # Keep this resource in config with count=0 so existing outputs/destroy logic remain safe.
  count = 0

  name       = "${var.tke_cluster_name}-pool"
  cluster_id = tencentcloud_kubernetes_cluster.tke[0].id
  vpc_id     = tencentcloud_vpc.cluster.id
  subnet_ids = local.tke_node_pool_subnet_ids

  max_size            = var.tke_node_count + 2
  min_size            = 1
  desired_capacity    = var.tke_node_count
  deletion_protection = false

  auto_scaling_config {
    instance_type              = local.tke_worker_type
    instance_charge_type       = "POSTPAID_BY_HOUR"
    system_disk_type           = "CLOUD_BSSD"
    system_disk_size           = 50
    orderly_security_group_ids = [tencentcloud_security_group.tke_pod.id]
    public_ip_assigned         = false
    key_ids                    = [tencentcloud_key_pair.cluster.id]
  }

  tags = {
    CubeSandboxRole = "tke"
  }
}

########################
# Tencent Container Registry (TCR)
########################

resource "tencentcloud_tcr_instance" "cluster" {
  count                = var.use_tcr ? 1 : 0
  name                 = "cubesandbox-${random_string.deploy_suffix.result}"
  instance_type        = "basic"
  registry_charge_type = 1
  # Delete the auto-created COS backend bucket together with the instance so a
  # `terraform destroy` leaves no residual storage. NOTE: delete_bucket is a
  # delete-time flag read from state, so it only takes effect for instances
  # created (or re-applied) after this is set; destroy.sh additionally deletes
  # the bucket via `tccli ... DeleteInstance --DeleteBucket true` to cover
  # already-deployed instances whose state predates this argument.
  delete_bucket = true
  tags = {
    CubeSandboxRole = "tcr"
  }
}

resource "tencentcloud_tcr_namespace" "cluster" {
  count       = var.use_tcr ? 1 : 0
  instance_id = tencentcloud_tcr_instance.cluster[0].id
  name        = "cubesandbox-cluster"
  is_public   = true
}

# VPC private-network access
resource "tencentcloud_tcr_vpc_attachment" "cluster" {
  count                    = var.use_tcr ? 1 : 0
  instance_id              = tencentcloud_tcr_instance.cluster[0].id
  vpc_id                   = tencentcloud_vpc.cluster.id
  subnet_id                = tencentcloud_subnet.cluster.id
  enable_public_domain_dns = true
  enable_vpc_domain_dns    = true
}

# Long-lived access token (used for docker login)
resource "tencentcloud_tcr_token" "cluster" {
  count       = var.use_tcr ? 1 : 0
  instance_id = tencentcloud_tcr_instance.cluster[0].id
  description = "CubeSandbox deploy token"
}

# Write the TCR token onto the jumpserver for docker login
resource "null_resource" "tcr_token_deploy" {
  count      = var.use_tcr ? 1 : 0
  depends_on = [tencentcloud_instance.jumpserver]

  # Re-push the token when it is rotated (new token id) or the jumpserver is
  # replaced (new id) — otherwise the regenerated token would never reach
  # /root/.tcr_token and the later docker login / image build would fail with a
  # confusing auth error. Mirrors null_resource.mysql_init_db's trigger guard.
  triggers = {
    tcr_token_id  = tencentcloud_tcr_token.cluster[0].id
    jumpserver_id = tencentcloud_instance.jumpserver.id
  }

  provisioner "local-exec" {
    # Pass the token through the process ENVIRONMENT (not interpolated into the
    # command string) so it stays out of the local `local-exec` argv / terraform's
    # rendered plan command, then pipe it over stdin to the jump-server (read back
    # by $(cat)) so it never appears in the remote argv / `ps` output (CWE-214).
    environment = {
      TCR_TOKEN = tencentcloud_tcr_token.cluster[0].token
    }
    # The jumpserver runs cloud-init on first boot (switching SSH to port 443),
    # which can take a few minutes. Retry until the token lands instead of
    # silently ignoring failures: a missing /root/.tcr_token makes the later
    # docker login / image build fail with a confusing, unrelated error.
    command = <<-EOT
      for i in $(seq 1 30); do
        if printf '%s' "$TCR_TOKEN" | \
          ssh -i ${pathexpand(var.ssh_private_key_path)} \
            -p 443 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
            -o ConnectTimeout=10 -o BatchMode=yes \
            root@${tencentcloud_instance.jumpserver.public_ip} \
            "cat > /root/.tcr_token && chmod 600 /root/.tcr_token"; then
          echo "TCR token written to the jumpserver"
          exit 0
        fi
        echo "jumpserver SSH not ready yet, retry $i/30..."
        sleep 10
      done
      echo "ERROR: could not write the TCR token to the jumpserver after retries" >&2
      exit 1
    EOT
  }
}

########################
# CVM — jumpserver (SSH entry point, port 443)
########################

resource "tencentcloud_instance" "jumpserver" {
  instance_name     = "cubesandbox-jumpserver"
  availability_zone = local.jumpserver_zone
  image_id          = data.tencentcloud_images.default.images.0.image_id
  instance_type     = var.jumpserver_instance_type

  vpc_id    = tencentcloud_vpc.cluster.id
  subnet_id = local.jumpserver_subnet_id

  instance_charge_type = "POSTPAID_BY_HOUR"

  system_disk_type = "CLOUD_BSSD"
  system_disk_size = 50

  # Public network (the jumpserver needs a public IP as the entry point)
  allocate_public_ip         = true
  internet_max_bandwidth_out = 200

  key_ids = [tencentcloud_key_pair.cluster.id]

  orderly_security_groups = [tencentcloud_security_group.jumpserver.id]

  # Change SSH port 22 → 443 + install base tools
  user_data = base64encode(<<-EOF
    #!/bin/bash
    set -e
    # Change the SSH port
    if command -v semanage &>/dev/null; then
      semanage port -a -t ssh_port_t -p tcp 443 2>/dev/null || true
    fi
    sed -i 's/^#Port 22/Port 443/' /etc/ssh/sshd_config
    sed -i 's/^Port 22/Port 443/' /etc/ssh/sshd_config
    if ! grep -q '^Port 443' /etc/ssh/sshd_config; then
      echo 'Port 443' >> /etc/ssh/sshd_config
    fi
    systemctl restart sshd
    # Install base tools + docker
    dnf install -y mysql nc redis jq curl python3-pip kubernetes-client git docker 2>&1 || true
    # buildx CLI plugin: build_images.sh forces BuildKit (the component Dockerfiles
    # use COPY --chmod) and the bare `docker` package omits buildx. build_images.sh
    # self-heals this too, but install it up front so the build host is ready.
    dnf install -y docker-buildx-plugin 2>&1 || dnf install -y moby-buildx 2>&1 || true
    systemctl enable --now docker 2>&1 || true
    pip3 install -q cubesandbox -i https://mirrors.tencent.com/pypi/simple/ 2>&1 || true
  EOF
  )
  timeouts {
    create = "10m"
  }
}

########################
# Compute node CVM (optional)
########################

resource "tencentcloud_instance" "compute" {
  count = var.compute_node_count

  instance_name     = "cubesandbox-compute"
  availability_zone = local.compute_zones[count.index]
  image_id          = data.tencentcloud_images.default.images.0.image_id
  instance_type     = local.compute_types[count.index]

  vpc_id = tencentcloud_vpc.cluster.id
  subnet_id = (
    local.compute_zones[count.index] == local.primary_zone ?
    tencentcloud_subnet.cluster.id :
    tencentcloud_subnet.cvm[local.compute_zones[count.index]].id
  )

  instance_charge_type = "POSTPAID_BY_HOUR"

  system_disk_type = "CLOUD_BSSD"
  system_disk_size = 50

  # Dedicated CBS data disk per compute node, formatted as XFS and mounted at
  # /data/cubelet by the user_data script below. Sized via compute_data_disk_size.
  data_disks {
    data_disk_type       = "CLOUD_BSSD"
    data_disk_size       = var.compute_data_disk_size
    delete_with_instance = true
  }

  # Format the first data disk as XFS and mount it at /data/cubelet on first
  # boot. The disk is matched by serial/size rather than a fixed /dev/vdX name
  # so it stays correct even if device ordering shifts. Idempotent: re-running
  # (e.g. on reboot) is a no-op once the disk is formatted and in fstab.
  user_data = base64encode(<<-EOF
    #!/bin/bash
    set -euo pipefail

    MOUNT_POINT=/data/cubelet

    # Find the unformatted, unmounted data disk (excludes the system disk, which
    # already carries a partition table / filesystem).
    # Covers virtio (vdX), SCSI/SATA (sdX), and NVMe (nvmeXnY) device naming.
    DATA_DISK=""
    for i in $(seq 1 30); do
      for dev in /dev/vd? /dev/sd? /dev/nvme?n? /dev/nvme??n?; do
        [ -b "$dev" ] || continue
        # Skip disks that already have partitions or a filesystem.
        # NVMe partitions are named nvmeXnYpZ (with a 'p' separator), while
        # sd/vd partitions are sdX1, vdX1 (digit appended directly).
        devbase="$(basename "$dev")"
        if lsblk -no NAME "$dev" | grep -qE "^$devbase""p?[0-9]"; then
          continue
        fi
        if blkid "$dev" >/dev/null 2>&1; then
          continue
        fi
        DATA_DISK="$dev"
        break
      done
      [ -n "$DATA_DISK" ] && break
      sleep 5
    done

    if [ -z "$DATA_DISK" ]; then
      echo "============================================================" >&2
      echo "WARNING: cubelet data disk not found after 30 retries (150s)." >&2
      echo "         /data/cubelet will fall back to the system disk." >&2
      echo "         Sandbox images and runtime data may exhaust the 50GB" >&2
      echo "         system disk and cause 'no space left on device' later." >&2
      echo "         Check CBS data disk provisioning in the TencentCloud console." >&2
      echo "============================================================" >&2
      exit 0
    fi

    mkfs.xfs -f "$DATA_DISK"
    mkdir -p "$MOUNT_POINT"

    # Wait for udev to settle so blkid can read the fresh UUID reliably.
    udevadm settle --timeout=10 2>/dev/null || true

    DISK_UUID=""
    for _retry in 1 2 3 4 5; do
      DISK_UUID="$(blkid -s UUID -o value "$DATA_DISK" 2>/dev/null)" && [ -n "$DISK_UUID" ] && break
      sleep 1
    done

    if [ -z "$DISK_UUID" ]; then
      echo "ERROR: failed to obtain UUID for $DATA_DISK after retries; falling back to device path" >&2
      FSTAB_SRC="$DATA_DISK"
    else
      FSTAB_SRC="UUID=$DISK_UUID"
    fi

    if ! grep -q "$MOUNT_POINT" /etc/fstab; then
      echo "$FSTAB_SRC $MOUNT_POINT xfs defaults,noatime,nofail 0 2" >> /etc/fstab
    fi
    mount "$MOUNT_POINT"
  EOF
  )

  # Private network only (accessed through the jumpserver)
  allocate_public_ip = false

  key_ids = [tencentcloud_key_pair.cluster.id]

  orderly_security_groups = [tencentcloud_security_group.compute.id]
}

########################
# Outputs
########################

output "security_group_ids" {
  description = "Per-role security group ids (jumpserver / compute / tke_pod / clb)"
  value = {
    jumpserver = tencentcloud_security_group.jumpserver.id
    compute    = tencentcloud_security_group.compute.id
    tke_pod    = tencentcloud_security_group.tke_pod.id
    clb        = tencentcloud_security_group.clb.id
  }
}

output "subnet_id" {
  value = tencentcloud_subnet.cluster.id
}

# Jumpserver
output "jumpserver_public_ip" {
  value = tencentcloud_instance.jumpserver.public_ip
}

output "jumpserver_private_ip" {
  value = tencentcloud_instance.jumpserver.private_ip
}

output "jumpserver_ssh_command" {
  value = "ssh -i ${pathexpand(var.ssh_private_key_path)} -p 443 -o StrictHostKeyChecking=no root@${tencentcloud_instance.jumpserver.public_ip}"
}

# TCR outputs
output "tcr_id" {
  value = var.use_tcr ? tencentcloud_tcr_instance.cluster[0].id : ""
}

output "tcr_registry_name" {
  value = var.use_tcr ? "${tencentcloud_tcr_instance.cluster[0].name}.tencentcloudcr.com" : ""
}
output "tcr_registry_url" {
  value = var.use_tcr ? "${tencentcloud_tcr_instance.cluster[0].name}.tencentcloudcr.com/${tencentcloud_tcr_namespace.cluster[0].name}" : ""
}

output "tcr_namespace" {
  value = var.use_tcr ? tencentcloud_tcr_namespace.cluster[0].name : ""
}

output "tcr_token_user" {
  value     = var.use_tcr ? tencentcloud_tcr_token.cluster[0].user_name : ""
  sensitive = true
}

# MySQL outputs
output "mysql_instance_id" {
  value = tencentcloud_mysql_instance.mysql.id
}

output "mysql_intranet_ip" {
  value = tencentcloud_mysql_instance.mysql.intranet_ip
}

output "mysql_intranet_port" {
  value = tencentcloud_mysql_instance.mysql.intranet_port
}

# Redis outputs
output "redis_instance_id" {
  value = tencentcloud_redis_instance.redis.id
}

output "redis_intranet_ip" {
  value = tencentcloud_redis_instance.redis.ip
}

output "redis_intranet_port" {
  value = tencentcloud_redis_instance.redis.port
}

# TKE outputs
output "tke_cluster_id" {
  value = length(tencentcloud_kubernetes_cluster.tke) > 0 ? tencentcloud_kubernetes_cluster.tke[0].id : ""
}

# Intranet (VPC-internal) kube-apiserver address. Public access is disabled, so
# this endpoint is only reachable from inside the VPC (e.g. the jumpserver).
output "tke_cluster_endpoint" {
  value = length(tencentcloud_kubernetes_cluster.tke) > 0 ? tencentcloud_kubernetes_cluster.tke[0].pgw_endpoint : ""
}

# Intranet kubeconfig (matches the intranet-only apiserver access above).
output "tke_kube_config" {
  value     = length(tencentcloud_kubernetes_cluster.tke) > 0 ? tencentcloud_kubernetes_cluster.tke[0].kube_config_intranet : ""
  sensitive = true
}

output "tke_node_pool_id" {
  value = length(tencentcloud_kubernetes_node_pool.tke) > 0 ? tencentcloud_kubernetes_node_pool.tke[0].id : ""
}

# Compute node outputs
output "compute_instance_ids" {
  value = tencentcloud_instance.compute[*].id
}

output "compute_instance_types" {
  description = "Actually purchased instance type per compute node (same order as compute_private_ips)"
  value       = tencentcloud_instance.compute[*].instance_type
}

output "compute_availability_zones" {
  description = "Actually used availability zone per compute node"
  value       = tencentcloud_instance.compute[*].availability_zone
}

output "compute_private_ips" {
  value = tencentcloud_instance.compute[*].private_ip
}

output "config_summary" {
  description = "Resolved deployment configuration (zones, instance types)"
  value = {
    availability_zone            = local.primary_zone
    jumpserver_availability_zone = local.jumpserver_zone
    compute_availability_zone    = local.compute_zone
    tke_worker_availability_zone = local.tke_worker_zone
    jumpserver_instance_type     = var.jumpserver_instance_type
    compute_instance_type        = var.compute_instance_type
    tke_worker_instance_type     = local.tke_worker_type
    compute_instance_types       = local.compute_types
    compute_availability_zones   = local.compute_zones
  }
}
