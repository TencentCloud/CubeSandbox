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
  description = "Primary availability zone for managed services (subnet, MySQL, Redis, TKE control plane). Example: ap-guangzhou-3"
  default     = ""
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
  description = "Jumpserver instance type, e.g. S5.MEDIUM4, S5.LARGE8"
  default     = "S5.MEDIUM4"
}

variable "compute_instance_type" {
  description = "Preferred compute-node instance type (fallback default when compute_instance_types is shorter than compute_node_count). Actual purchased types are recorded in compute_instance_types."
  default     = "S5.2XLARGE16"
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
  description = "Number of CVM PVM compute nodes (create.sh creates 1 by default; override with TENCENTCLOUD_COMPUTE_NODE_COUNT)"
  type        = number
  default     = 1

  validation {
    condition     = var.compute_node_count >= 0 && floor(var.compute_node_count) == var.compute_node_count
    error_message = "compute_node_count must be a non-negative integer."
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
  description = "Initial node count of the TKE node pool (create.sh defaults to 2 via TENCENTCLOUD_TKE_NODE_COUNT)"
  type        = number
  default     = 2

  validation {
    condition     = var.tke_node_count >= 1 && floor(var.tke_node_count) == var.tke_node_count
    error_message = "tke_node_count must be an integer >= 1."
  }
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

variable "image_tag" {
  description = "Shared image tag for the Cube components (cube-master/cube-api/cube-proxy/cube-webui); must match the TAG built/pushed by build_images.sh"
  type        = string
  default     = "latest"
}

variable "image_registry" {
  description = "Registry domain for the Cube component images; leave empty to use the TCR instance created by this run (create.sh pushes the images to that TCR)"
  type        = string
  default     = ""
}

variable "image_namespace" {
  description = "Namespace for the Cube component images; leave empty to use the TCR namespace created by this run"
  type        = string
  default     = ""
}
