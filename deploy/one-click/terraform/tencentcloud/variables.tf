variable "vpc_name" {
  description = "VPC name (create.sh prompts for this; override with TENCENTCLOUD_VPC_NAME)"
  type        = string
  default     = "cubesandbox-terraform-vpc"
}

variable "region" {
  description = "Tencent Cloud region"
  default     = "ap-guangzhou"

  validation {
    condition     = length(trimspace(var.region)) > 0
    error_message = "region must not be empty (e.g. ap-guangzhou)."
  }
}

variable "availability_zone" {
  description = "Primary availability zone for managed services (subnet, MySQL, Redis, TKE control plane). Matches TENCENTCLOUD_AVAILABILITY_ZONE in env.example."
  default     = "ap-guangzhou-6"
}

variable "jumpserver_availability_zone" {
  description = "Jumpserver CVM zone; leave empty to use availability_zone"
  default     = ""
}

variable "compute_availability_zone" {
  description = "Compute node CVM zone; leave empty to use availability_zone"
  default     = ""
}

variable "tke_worker_availability_zone" {
  description = "TKE worker node zone; leave empty to use availability_zone"
  default     = ""
}

variable "image_name_regex" {
  description = "OS image name (regex match); defaults to OpenCloudOS Server 9"
  default     = "OpenCloudOS Server 9"
}

variable "jumpserver_instance_type" {
  description = "Jumpserver instance type, e.g. SA9.MEDIUM4, SA9.LARGE8"
  default     = "SA9.MEDIUM4"
}

variable "compute_instance_type" {
  description = "Preferred compute-node instance type (fallback default when compute_instance_types is shorter than compute_node_count). Actual purchased types are recorded in compute_instance_types."
  default     = "SA9.MEDIUM8"
}

variable "compute_instance_types" {
  description = "Per compute-node instance types; shorter lists are padded with compute_instance_type. Set by create.sh from actually purchased CVMs."
  type        = list(string)
  default     = []
}

variable "compute_availability_zones" {
  description = "Per compute-node availability zones; shorter lists use compute_availability_zone / availability_zone."
  type        = list(string)
  default     = []
}

variable "ssh_public_key_path" {
  description = "SSH public key path; defaults to the project directory ./.ssh/id_rsa.pub"
  default     = "./.ssh/id_rsa.pub"
}

variable "ssh_private_key_path" {
  description = "SSH private key path; defaults to the project directory ./.ssh/id_rsa"
  default     = "./.ssh/id_rsa"
}

variable "compute_node_count" {
  description = "Number of CVM PVM compute nodes (matches TENCENTCLOUD_COMPUTE_NODE_COUNT in env.example)"
  type        = number
  default     = 2

  validation {
    condition     = var.compute_node_count >= 0 && floor(var.compute_node_count) == var.compute_node_count
    error_message = "compute_node_count must be a non-negative integer."
  }
}

variable "compute_data_disk_size" {
  description = "Per compute-node CBS data disk size in GB; formatted as XFS and mounted at /data/cubelet (override with TENCENTCLOUD_COMPUTE_DATA_DISK_SIZE)"
  type        = number
  default     = 200

  validation {
    condition     = var.compute_data_disk_size >= 10 && floor(var.compute_data_disk_size) == var.compute_data_disk_size
    error_message = "compute_data_disk_size must be an integer >= 10 (GB)."
  }
}

# WARNING: these defaults are weak, well-known demo credentials kept only so a
# zero-config `create.sh` / `terraform apply` succeeds. Always override them for
# any non-throwaway deployment via TENCENTCLOUD_MYSQL_PASSWORD /
# TENCENTCLOUD_REDIS_PASSWORD (create.sh) or -var / TF_VAR_* (raw terraform).
variable "mysql_root_password" {
  description = "MySQL root password (override the insecure default for real deployments)"
  type        = string
  default     = "CubeSandbox123!"
  sensitive   = true
}

variable "redis_password" {
  description = "Redis password (override the insecure default for real deployments)"
  type        = string
  default     = "ceuhvu123"
  sensitive   = true
}

variable "cube_password" {
  description = "Password for the CubeSandbox MySQL application account 'cube' (override the insecure default for real deployments; create.sh wires TENCENTCLOUD_CUBE_PASSWORD into this)"
  type        = string
  default     = "cube_pass"
  sensitive   = true
}

# cube_db / cube_user are the single source of truth for the application
# database name and account. They flow into the MySQL account/privilege/init
# (main.tf), the cube-master conf Secret (tke-addons.tf) and create.sh's health
# checks, so a customized value stays consistent end to end instead of drifting
# against a hard-coded default. create.sh maps TENCENTCLOUD_CUBE_DB /
# TENCENTCLOUD_CUBE_USER onto these.
variable "cube_db" {
  description = "CubeSandbox application database name (create.sh wires TENCENTCLOUD_CUBE_DB into this)"
  type        = string
  default     = "cube_mvp"

  validation {
    # Used as a bare SQL identifier in CREATE DATABASE / GRANT, so restrict it to
    # safe identifier characters to avoid injection through the local-exec command.
    condition     = can(regex("^[A-Za-z0-9_]+$", var.cube_db))
    error_message = "cube_db must contain only letters, digits and underscores (e.g. cube_mvp)."
  }
}

variable "cube_user" {
  description = "CubeSandbox MySQL application account name (create.sh wires TENCENTCLOUD_CUBE_USER into this)"
  type        = string
  default     = "cube"

  validation {
    condition     = can(regex("^[A-Za-z0-9_]+$", var.cube_user))
    error_message = "cube_user must contain only letters, digits and underscores (e.g. cube)."
  }
}

variable "redis_mem_size" {
  description = "Redis memory size (MB)"
  type        = number
  default     = 1024
}

variable "create_tke" {
  # The TKE cluster is always created. create.sh also toggles this flag internally
  # to phase the apply: it is kept false for the base applies so the kubernetes
  # provider does not connect before the API Server exists, then flipped true for
  # the final cluster + addons step.
  description = "Whether to create the TKE Kubernetes cluster (always enabled; create.sh also uses it to phase the apply)"
  type        = bool
  default     = true
}

variable "tke_cluster_name" {
  description = "TKE cluster name"
  type        = string
  default     = "cubesandbox-terraform-tke"
}

variable "tke_cluster_version" {
  description = "TKE Kubernetes version"
  type        = string
  default     = "1.34.1"
}

variable "tke_node_count" {
  description = "TKE worker node count (worker_config.count). Set via TENCENTCLOUD_TKE_NODE_COUNT in create.sh."
  type        = number
  default     = 2

  validation {
    condition     = var.tke_node_count >= 1 && floor(var.tke_node_count) == var.tke_node_count
    error_message = "tke_node_count must be an integer >= 1 (TKE intranet apiserver requires at least one worker)."
  }
}

variable "tke_worker_instance_type" {
  description = "TKE worker node instance type (4C8G is sufficient for control-plane pods)"
  type        = string
  default     = "SA9.LARGE8"
}

variable "tke_cluster_cidr" {
  description = "TKE Pod network CIDR"
  type        = string
  default     = "10.200.0.0/16"

  validation {
    condition     = can(cidrhost(var.tke_cluster_cidr, 0))
    error_message = "tke_cluster_cidr must be a valid CIDR (e.g. 10.200.0.0/16)."
  }
}

variable "tke_service_cidr" {
  description = "TKE Service network CIDR (mask 17-27)"
  type        = string
  default     = "192.168.0.0/20"

  validation {
    condition     = can(cidrhost(var.tke_service_cidr, 0)) && can(regex("/(1[7-9]|2[0-7])$", var.tke_service_cidr))
    error_message = "tke_service_cidr must be a valid CIDR with a mask between /17 and /27 (e.g. 192.168.0.0/20)."
  }
}

variable "deploy_tke_addons" {
  description = "Whether to deploy the TKE Kubernetes resources (cube-master/api/proxy/webui)"
  type        = bool
  default     = true
}

# Network exposure mode for the three user-facing Services (cube-api /
# cube-proxy / cube-webui).
#
#   false (default): each Service is fronted by a VPC-INTERNAL CLB (private VIP
#     only, reachable from inside the VPC / via the jumpserver / VPN). The
#     cubesandbox-sg-clb ingress for 80 / 443 / 3000 is scoped to the VPC CIDR
#     instead of 0.0.0.0/0. This is the safe default: no public exposure.
#   true: each Service is fronted by a PUBLIC CLB (public VIP reachable from the
#     internet) and cubesandbox-sg-clb opens 80 / 443 / 3000 to 0.0.0.0/0. Opt
#     into this only when you genuinely need public access, and read the
#     "Hardening the Public-Facing Services" doc section first.
#
# cube-master is unaffected: it always uses a VPC-internal CLB regardless of
# this flag.
#
# IMPORTANT: Changing this value on an existing deployment will RECREATE the
# affected CLB Services (cube-api / cube-proxy / cube-webui). Public↔internal
# are fundamentally different CLB types, so Terraform destroys the old CLB and
# provisions a new one — the VIP address will change. Update any DNS records or
# client configurations that reference the old VIP after the apply completes.
variable "enable_public_network" {
  description = "Expose cube-api / cube-proxy / cube-webui through PUBLIC CLBs. false (default) = VPC-internal CLBs only (no public exposure); true = public CLBs reachable from the internet. cube-master always stays VPC-internal. WARNING: toggling this value recreates the CLB Services and changes the VIP addresses."
  type        = bool
  default     = false
}

variable "use_tcr" {
  description = "Create/use a private TCR and build/push component images. Default false uses public prebuilt images and skips TCR/PrivateDNS."
  type        = bool
  default     = false
}

variable "use_cfs" {
  description = "Create/use CFS shared storage for cube-master. Default false uses an emptyDir volume, intended for single-replica cube-master."
  type        = bool
  default     = false
}

variable "image_tag" {
  description = "Shared image tag for the Cube components when per-component image overrides are empty"
  type        = string
  default     = "cubesandbox002-20260630"
}

variable "image_registry" {
  description = "Registry domain for the Cube component images. Defaults to the public CubeSandbox image registry when use_tcr=false."
  type        = string
  default     = "cube-sandbox-image.tencentcloudcr.com"
}

variable "image_namespace" {
  description = "Namespace for the Cube component images. Defaults to public namespace demo when use_tcr=false."
  type        = string
  default     = "demo"
}

variable "cubemaster_image" {
  description = "Full cubemaster image override, optionally pinned by digest."
  type        = string
  default     = "cube-sandbox-image.tencentcloudcr.com/demo/cubemaster:v0.4.0-591b5b7-package-20260701"
}

variable "cubeapi_image" {
  description = "Full cube-api image override, optionally pinned by digest."
  type        = string
  default     = "cube-sandbox-image.tencentcloudcr.com/demo/cubeapi@sha256:bcff401e2f2cef8304b242f8ee3d81f08ca48303c4cbf323692c4c4d331d91ab"
}

variable "cubeproxy_image" {
  description = "Full cube-proxy image override, optionally pinned by digest."
  type        = string
  default     = "cube-sandbox-image.tencentcloudcr.com/demo/cubeproxy@sha256:dfe82ee66d29a9556197a5332481869be738e9598e29e315185cd5b27c5fc096"
}

variable "webui_image" {
  description = "Full webui image override, optionally pinned by digest."
  type        = string
  default     = "cube-sandbox-image.tencentcloudcr.com/demo/webui@sha256:87ec878d252154b1162afa541494b3298ae468caa9b4279d4f109c3f8d79c351"
}

# Per-component replica counts. All four default to 1 in env.example / variables.tf
# and are independently tunable via -var / TF_VAR_* / the TENCENTCLOUD_*_REPLICAS
# env knobs wired by create.sh.
#
# cubemaster_replicas is special: it is the single source of truth for BOTH the
# cube-master Deployment's spec.replicas AND the conf's
# default_headless_service_nodes_num, which MUST agree (cube-master apportions the
# global create/destroy concurrency per master as total/master_count and estimates
# global in-flight load as local*master_count — see HealthyMasterNodes() in
# pkg/localcache and pkg/scheduler). Driving both from this one variable keeps them
# from drifting.
variable "cubemaster_replicas" {
  description = "cube-master Deployment replica count (also feeds the conf's default_headless_service_nodes_num)"
  type        = number
  default     = 1

  validation {
    condition     = var.cubemaster_replicas >= 1 && floor(var.cubemaster_replicas) == var.cubemaster_replicas
    error_message = "cubemaster_replicas must be an integer >= 1."
  }
}

variable "cube_api_replicas" {
  description = "cube-api Deployment replica count"
  type        = number
  default     = 1

  validation {
    condition     = var.cube_api_replicas >= 1 && floor(var.cube_api_replicas) == var.cube_api_replicas
    error_message = "cube_api_replicas must be an integer >= 1."
  }
}

# cube-proxy defaults to a SINGLE replica (unlike the other components, which
# default to 2). This is deliberate: the auto-pause/auto-resume feature is only
# correct in single-replica mode.
#
# Each cube-proxy pod runs a co-resident cube-proxy-sidecar whose sweeper decides
# when a sandbox is idle based on the last-active timestamps it observes on its
# OWN cube-proxy. With >1 replica behind a round-robin / least-conn CLB, requests
# for a single sandbox are spread across replicas, so no individual sidecar sees
# the full activity stream. A replica that happened not to serve recent requests
# will believe the sandbox is idle and pause it out from under an actively-used
# session, producing a pause -> auto-resume churn loop.
#
# If you must scale cube-proxy to >1 replica for HA/throughput, the front-end
# load balancer MUST be configured to hash on the sandbox ID so that all traffic
# for a given sandbox is pinned to one replica (consistent session affinity by
# SandboxID). Without that, auto-pause/auto-resume will misfire.
variable "cube_proxy_replicas" {
  description = "cube-proxy Deployment replica count. Defaults to 1: auto-pause/auto-resume is only correct in single-replica mode. Setting >1 REQUIRES the front-end LB to hash on SandboxID (session affinity), otherwise auto-pause/auto-resume will misfire."
  type        = number
  default     = 1

  validation {
    condition     = var.cube_proxy_replicas >= 1 && floor(var.cube_proxy_replicas) == var.cube_proxy_replicas
    error_message = "cube_proxy_replicas must be an integer >= 1."
  }
}

variable "cube_webui_replicas" {
  description = "cube-webui Deployment replica count"
  type        = number
  default     = 1

  validation {
    condition     = var.cube_webui_replicas >= 1 && floor(var.cube_webui_replicas) == var.cube_webui_replicas
    error_message = "cube_webui_replicas must be an integer >= 1."
  }
}
