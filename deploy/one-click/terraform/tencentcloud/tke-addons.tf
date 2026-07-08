########################
# TKE Addons — Kubernetes resources (Terraform instead of kubectl)
#   Deployed only when create_tke=true
########################

locals {
  # Default mode uses tag-based public images and does not create/use TCR.
  # If use_tcr=true, create.sh builds/pushes the images into the per-deployment TCR.
  image_registry = var.use_tcr ? (
    var.image_registry != "" ? var.image_registry : "${tencentcloud_tcr_instance.cluster[0].name}.tencentcloudcr.com"
  ) : var.image_registry
  image_namespace = var.use_tcr ? (
    var.image_namespace != "" ? var.image_namespace : tencentcloud_tcr_namespace.cluster[0].name
  ) : var.image_namespace
  cube_master_image = var.cubemaster_image != "" ? var.cubemaster_image : "${local.image_registry}/${local.image_namespace}/cube-master:${var.image_tag}"
  cube_api_image    = var.cubeapi_image != "" ? var.cubeapi_image : "${local.image_registry}/${local.image_namespace}/cube-api:${var.image_tag}"
  cube_proxy_image  = var.cubeproxy_image != "" ? var.cubeproxy_image : "${local.image_registry}/${local.image_namespace}/cube-proxy:${var.image_tag}"
  cube_webui_image  = var.webui_image != "" ? var.webui_image : "${local.image_registry}/${local.image_namespace}/webui:${var.image_tag}"
  cube_lcm_image    = var.cube_lifecycle_manager_image != "" ? var.cube_lifecycle_manager_image : "${local.image_registry}/${local.image_namespace}/cube-lifecycle-manager:${var.image_tag}"
  cube_admin_token  = var.cube_admin_token != "" ? var.cube_admin_token : random_password.cube_admin_token[0].result

  # cube_db / cube_user are wired through Terraform (var.cube_db / var.cube_user)
  # so the MySQL account/database created in main.tf, the cube-master conf Secret
  # here, and create.sh's later health checks all agree on the same names. They
  # default to cube_mvp / cube; create.sh maps TENCENTCLOUD_CUBE_DB /
  # TENCENTCLOUD_CUBE_USER onto these variables.
  cube_db       = var.cube_db
  cube_user     = var.cube_user
  cube_password = var.cube_password

  # cube-master URL: in-cluster Service DNS (cube-api / cube-proxy reach
  # cube-master over the cluster network, so the internal CLB IP is not needed).
  cubemaster_url = "http://cubemaster.cubesandbox.svc.cluster.local:8089"

  # cube-master runs as an HA Deployment backed by the shared CFS store. The
  # replica count is the single source of truth for BOTH spec.replicas AND the
  # conf's default_headless_service_nodes_num, which MUST agree — see the
  # var.cubemaster_replicas docs in variables.tf for why under-reporting the
  # master count oversubscribes the compute nodes.
  cubemaster_replicas = var.cubemaster_replicas
  # Multi-node scheduling: pick randomly from the top scored compute nodes.
  # The multi-node guide recommends 3 as a small-cluster starting point; cap at
  # the actual compute-node count so the default 2-node POC uses 2.
  cubemaster_priority_select_num = max(1, min(var.compute_node_count, 3))

  # All files under the certificate directory
  cert_files = fileset("${path.module}/cubeproxy-certs", "*")

  # create.sh writes these files next to the Terraform module before applying
  # addons. Terraform evaluates locals even during early targeted network applies,
  # so every file() call must be guarded with fileexists().
  webui_nginx_conf = fileexists("${path.module}/webui-nginx.conf") ? file("${path.module}/webui-nginx.conf") : (
    fileexists("${path.module}/../../webui/nginx.conf") ? file("${path.module}/../../webui/nginx.conf") : <<-EOF
      worker_processes auto;
      events { worker_connections 1024; }
      http {
        server {
          listen 80;
          root /usr/share/nginx/html;
          index index.html;
          location ^~ /sandbox/ { proxy_pass __SANDBOX_PROXY_UPSTREAM__; }
          location /cubeapi/ { proxy_pass __WEB_UI_UPSTREAM__/cubeapi/; }
          location / { try_files $uri $uri/ /index.html; }
        }
      }
    EOF
  )
  cubeproxy_nginx_conf = replace(
    replace(
      replace(
        replace(
          replace(
            fileexists("${path.module}/cubeproxy-nginx.conf") ? file("${path.module}/cubeproxy-nginx.conf") : (
              fileexists("${path.module}/../../cubeproxy/nginx.conf.template") ? file("${path.module}/../../cubeproxy/nginx.conf.template") : <<-EOF
                user root;
                worker_processes auto;
                error_log /data/log/cube-proxy/error.log notice;
                daemon off;
                events { worker_connections 100000; }
                http {
                  include mime.types;
                  default_type application/octet-stream;
                  server {
                    listen __CUBE_PROXY_HTTP_PORT__;
                    server_name _;
                    location / { return 404; }
                  }
                  server {
                    listen __CUBE_PROXY_HTTPS_PORT__ ssl;
                    server_name _;
                    ssl_certificate /usr/local/openresty/nginx/certs/__CUBE_PROXY_SSL_CERT__;
                    ssl_certificate_key /usr/local/openresty/nginx/certs/__CUBE_PROXY_SSL_KEY__;
                    location / { return 404; }
                  }
                  server {
                    listen __CUBE_PROXY_ADMIN_LISTEN__:8082;
                    server_name _;
                    location / { return 404; }
                  }
                }
              EOF
            ),
            "__CUBE_PROXY_HTTP_PORT__",
            "8081"
          ),
          "__CUBE_PROXY_HTTPS_PORT__",
          "8080"
        ),
        "__CUBE_PROXY_SSL_CERT__",
        "cube.app+3.pem"
      ),
      "__CUBE_PROXY_SSL_KEY__",
      "cube.app+3-key.pem"
    ),
    "__CUBE_PROXY_ADMIN_LISTEN__:8082",
    "0.0.0.0:8082"
  )

  # Precondition for creating the TKE addons
  deploy_addons = var.create_tke && var.deploy_tke_addons
}

resource "random_password" "cube_admin_token" {
  count   = var.cube_admin_token == "" ? 1 : 0
  length  = 32
  special = false
}

# Write the kubeconfig to a local file (written as soon as TKE is created, independent of the addons).
# The apiserver is intranet-only, so use the intranet kubeconfig. create.sh then
# rewrites this local file to reach the endpoint through the jumpserver tunnel.
resource "local_file" "tke_kubeconfig" {
  count    = var.create_tke ? 1 : 0
  content  = tencentcloud_kubernetes_cluster.tke[0].kube_config_intranet
  filename = "${path.module}/.kube/config"

  # On CREATE this makes the kubeconfig file be (re)written LAST — after every
  # kubernetes_* addon — so it never clobbers the jumpserver-tunnel kubeconfig
  # that create.sh points the provider at while the addons are being applied.
  #
  # NOTE: depends_on couples "create after" with "destroy BEFORE", so a plain
  # `terraform destroy` would tear this down FIRST, deleting .kube/config and
  # breaking the provider mid-delete (Delete http://localhost ... connection
  # refused). destroy.sh therefore detaches this resource from state before the
  # addon/cluster teardown and removes the file itself at the very end (so the
  # kubeconfig outlives the cluster). See destroy.sh Phase 1.
  depends_on = [
    kubernetes_deployment.cube_webui,
    kubernetes_service.cube_webui,
    kubernetes_deployment.cube_lifecycle_manager,
    kubernetes_service.cube_lifecycle_manager,
    kubernetes_deployment.cube_proxy,
    kubernetes_service.cube_proxy,
    kubernetes_deployment.cube_api,
    kubernetes_service.cube_api,
    kubernetes_deployment.cubemaster,
    kubernetes_service.cubemaster,
  ]
}

# ---------------------------------------------------------------
# Namespace
# ---------------------------------------------------------------
resource "kubernetes_namespace" "cubesandbox" {
  count      = local.deploy_addons ? 1 : 0
  depends_on = [tencentcloud_kubernetes_cluster.tke]
  metadata {
    name = "cubesandbox"
  }
}

# ---------------------------------------------------------------
# CubeEgress MITM root CA
#
# `cubemastercli tpl create-from-image` defaults --with-cube-ca=true, so
# CubeMaster bakes this root cert into every template rootfs (read from
# the hardcoded path /etc/cube/ca/cube-root-ca.crt inside its container —
# see CubeMaster/pkg/templatecenter/cube_egress_ca_bake.go). Without the
# cert on disk the build fails with "CubeEgress root CA is missing".
#
# We generate the CA in Terraform (ECDSA P-256, 10 yr, matching the
# systemd path's cube-egress-prepare.sh) and store both halves in a
# Secret. BOTH halves are mounted into the cubemaster pod at
# /etc/cube/ca: the public cert is baked into template rootfs, and the
# private key is served to compute nodes over /cube/ca/cube-root-ca.key
# (CubeMaster/pkg/service/httpservice/cube/ca_download.go reads them from
# /etc/cube/ca). Each compute-node CubeEgress pulls this same CA via
# cube-egress-prepare.sh and signs leaf certs with the matching key, so
# templates baked on master are trusted by sandboxes whose traffic the
# compute-node CubeEgress MITMs. The cube-master CLB is VPC-internal and
# port 8089 is firewalled to the VPC/pod CIDR, so the unauthenticated key
# endpoint is reachable only from inside the cluster network. Keeping CA
# generation in Terraform state means a re-apply reuses the same CA (no
# needless template-rebake churn).
# ---------------------------------------------------------------
resource "tls_private_key" "cube_egress_ca" {
  count       = local.deploy_addons ? 1 : 0
  algorithm   = "ECDSA"
  ecdsa_curve = "P256"
}

resource "tls_self_signed_cert" "cube_egress_ca" {
  count           = local.deploy_addons ? 1 : 0
  private_key_pem = tls_private_key.cube_egress_ca[0].private_key_pem

  subject {
    common_name = "CubeSandbox Egress MITM CA"
  }

  validity_period_hours = 87600 # 10 years
  is_ca_certificate     = true
  set_subject_key_id    = true

  allowed_uses = [
    "cert_signing",
    "crl_signing",
  ]
}

resource "kubernetes_secret" "cube_egress_ca" {
  count = local.deploy_addons ? 1 : 0
  metadata {
    name      = "cube-egress-ca"
    namespace = kubernetes_namespace.cubesandbox[0].metadata[0].name
  }
  type = "Opaque"
  data = {
    "cube-root-ca.crt" = tls_self_signed_cert.cube_egress_ca[0].cert_pem
    "cube-root-ca.key" = tls_private_key.cube_egress_ca[0].private_key_pem
  }
}

# ---------------------------------------------------------------
# cube-master: Secret → Deployment → CLB Service (private network)
# ---------------------------------------------------------------

# cube-master configuration file. It embeds the MySQL and Redis credentials, so
# it is stored as a Secret (not a ConfigMap) and mounted as a file into the pod.
resource "kubernetes_secret" "cubemaster_conf" {
  count = local.deploy_addons ? 1 : 0
  type  = "Opaque"
  metadata {
    name      = "cubemaster-conf"
    namespace = kubernetes_namespace.cubesandbox[0].metadata[0].name
  }

  data = {
    "conf.yaml" = yamlencode({
      common = {
        http_port                          = 8089
        http_readtimeout                   = 120
        http_writetimeout                  = 360
        http_idletimeout                   = 360
        sync_meta_data_interval            = "30s"
        sync_metric_data_interval          = "1s"
        collect_metric_interval            = "1s"
        default_headless_service_nodes_num = local.cubemaster_replicas
        enable_check_com_net_id_param      = false
      }
      log = {
        module    = "cubemaster"
        path      = "/data/log/CubeMaster"
        file_size = 100
        file_num  = 10
        level     = "info"
      }
      cubelet_conf = {
        grpc = {
          grpc_port = 9999
        }
        common_timeout_insec       = 30
        create_image_timeout_insec = 300
        create_concurrent_limit    = 100
        destroy_concurent_limit    = 100
        enable_exposed_port        = true
        exposed_port_list          = ["80"]
        disable_redis_proxy_port   = true
      }
      auth = {
        enable = false
      }
      req_template_conf = {
        whitelist_req_tag = {
          WorkingDir  = true
          RLimit      = true
          DnsConfig   = true
          HostAliases = true
          Poststop    = true
          Prestop     = true
        }
        # The default egress policy lives under "cube_network_config": that is the
        # only key CubeMaster deserializes (CreateCubeSandboxReq.CubeNetworkConfig,
        # json:"cube_network_config"). The legacy "cubevs_context" key is silently
        # dropped, so the policy below would never apply. Two guards keep this in
        # sync: a Go regression test
        # (CubeMaster/pkg/service/httpservice/cube/cubebox_req_template_test.go)
        # deserializes the shipped configs' template and asserts the network config
        # is populated, and deploy/one-click/tests/test_package_layout.sh statically
        # checks THIS template uses cube_network_config (and not cubevs_context).
        cube_box_req_template = "{\"volumes\":[{\"name\":\"tmp\",\"volume_source\":{\"empty_dir\":{\"medium\":0}}}],\"containers\":[{\"name\":\"cubebox-default\",\"envs\":[{\"key\":\"TZ\",\"value\":\"Asia/Shanghai\"},{\"key\":\"TERM\",\"value\":\"xterm\"}],\"volume_mounts\":[{\"name\":\"tmp\",\"container_path\":\"/\"}],\"security_context\":{\"privileged\":true,\"readonly_rootfs\":false,\"no_new_privs\":false}}],\"network_type\":\"tap\",\"cube_network_config\":{\"allowInternetAccess\":true,\"denyOut\":[\"10.0.0.0/8\",\"100.64.0.0/10\",\"172.16.0.0/12\",\"192.168.0.0/16\"]}}"
      }
      ossdb_config = {
        addr                       = "${tencentcloud_mysql_instance.mysql.intranet_ip}:3306"
        user                       = local.cube_user
        pwd                        = local.cube_password
        db_name                    = local.cube_db
        conn_timeout               = 5
        read_timeout               = 5
        write_timeout              = 5
        max_idle_conns             = 5
        max_open_conns             = 20
        max_conn_life_time_seconds = 300
      }
      instance_db_config = {
        addr                       = "${tencentcloud_mysql_instance.mysql.intranet_ip}:3306"
        user                       = local.cube_user
        pwd                        = local.cube_password
        db_name                    = local.cube_db
        conn_timeout               = 5
        read_timeout               = 5
        write_timeout              = 5
        max_idle_conns             = 5
        max_open_conns             = 20
        max_conn_life_time_seconds = 300
      }
      # CubeMaster only consumes a single Redis pool (config.Config.RedisConf,
      # yaml:"redis"). There is no read/write split in the server-side config, so
      # do not emit redis_read / redis_write here — they would be dead config that
      # implies a capability the control plane ignores.
      redis = {
        nodes        = "${tencentcloud_redis_instance.redis.ip}:6379"
        password     = var.redis_password
        db_no        = 0
        max_idle     = 8
        max_active   = 32
        idle_timeout = 30
        max_retry    = 2
      }
      scheduler = {
        priority_select_num         = local.cubemaster_priority_select_num
        metric_update_timeout       = "300s"
        local_metric_update_timeout = "300s"
        filter = {
          enable_filters = ["cpu", "mem", "template_locality", "realtime_create_num"]
        }
        score = {
          enable_scorers = ["real_time_weighted_average"]
          resource_weights = {
            mvm_num          = 2
            local_create_num = 3
            cpu_usage        = 1
            quota_mem_usage  = 1
          }
          plugin_conf = {
            real_time_weighted_average = {
              weight = 1.0
              enable_weight_factors = [
                "mvm_num",
                "local_create_num",
                "cpu_usage",
                "quota_mem_usage",
              ]
              time_decay_seconds = 300
            }
          }
        }
      }
    })
  }

  depends_on = [tencentcloud_mysql_instance.mysql, tencentcloud_redis_instance.redis]
}

# cube-master Deployment
resource "kubernetes_deployment" "cubemaster" {
  count      = local.deploy_addons ? 1 : 0
  depends_on = [null_resource.mysql_init_db]

  metadata {
    name      = "cubemaster"
    namespace = kubernetes_namespace.cubesandbox[0].metadata[0].name
    labels    = { app = "cubemaster" }
  }
  spec {
    replicas = local.cubemaster_replicas
    selector {
      match_labels = { app = "cubemaster" }
    }
    template {
      metadata {
        labels = { app = "cubemaster" }
      }
      spec {
        container {
          name  = "cubemaster"
          image = local.cube_master_image
          env {
            name  = "CUBE_MASTER_CONFIG_PATH"
            value = "/etc/cubemaster/conf.yaml"
          }
          port {
            name           = "http"
            container_port = 8089
          }
          port {
            name           = "grpc"
            container_port = 9999
          }
          readiness_probe {
            http_get {
              path = "/notify/health"
              port = 8089
            }
            initial_delay_seconds = 10
            period_seconds        = 10
          }
          volume_mount {
            name       = "conf"
            mount_path = "/etc/cubemaster"
          }
          # Shared CFS (NFS, ReadWriteMany): all replicas read/write the same
          # template / snapshot / runtime state.
          volume_mount {
            name       = "data"
            mount_path = "/data/CubeMaster/storage"
          }
          # CubeEgress root CA, read by the --with-cube-ca template bake.
          volume_mount {
            name       = "cube-egress-ca"
            mount_path = "/etc/cube/ca"
            read_only  = true
          }
        }
        volume {
          name = "conf"
          secret {
            secret_name = kubernetes_secret.cubemaster_conf[0].metadata[0].name
          }
        }
        # Default no-CFS mode uses pod-local emptyDir storage, suitable for the
        # default single-replica cube-master. Set use_cfs=true when scaling
        # cube-master beyond one replica or when persistent shared storage is needed.
        dynamic "volume" {
          for_each = var.use_cfs ? [1] : []
          content {
            name = "data"
            nfs {
              server = tencentcloud_cfs_file_system.cubemaster_data[0].mount_ip
              path   = "/"
            }
          }
        }
        dynamic "volume" {
          for_each = var.use_cfs ? [] : [1]
          content {
            name = "data"
            empty_dir {}
          }
        }
        # Both the public cert and the private key are projected here:
        # cubemaster bakes the cert into template rootfs AND serves both
        # files to compute nodes via /cube/ca/<file> (ca_download.go), so a
        # compute-node CubeEgress signs leaf certs with the same CA the
        # templates trust. The key is exposed only inside the VPC (internal
        # CLB + SG-restricted 8089).
        volume {
          name = "cube-egress-ca"
          secret {
            secret_name = kubernetes_secret.cube_egress_ca[0].metadata[0].name
            items {
              key  = "cube-root-ca.crt"
              path = "cube-root-ca.crt"
            }
            items {
              key  = "cube-root-ca.key"
              path = "cube-root-ca.key"
            }
          }
        }
        dns_config {
          nameservers = ["183.60.83.19", "183.60.82.98"]
        }
      }
    }
  }
}

# cube-master private-network CLB Service
# NOTE: cubemaster always stays VPC-internal regardless of enable_public_network,
# so it does NOT use replace_triggered_by — its CLB type never changes.
resource "kubernetes_service" "cubemaster" {
  count = local.deploy_addons ? 1 : 0
  metadata {
    name      = "cubemaster"
    namespace = kubernetes_namespace.cubesandbox[0].metadata[0].name
    annotations = {
      "service.kubernetes.io/qcloud-loadbalancer-internal-subnetid" = tencentcloud_subnet.cluster.id
      "service.cloud.tencent.com/modification-protection"           = "false"
      "service.cloud.tencent.com/pass-to-target"                    = "true"
      "service.cloud.tencent.com/security-groups"                   = tencentcloud_security_group.clb.id
    }
  }
  lifecycle {
    # TKE controller-manager injects runtime annotations; ignore to avoid drift.
    ignore_changes = [
      metadata[0].annotations,
    ]
  }

  spec {
    type     = "LoadBalancer"
    selector = { app = "cubemaster" }
    port {
      name     = "http"
      port     = 8089
      protocol = "TCP"
    }
  }
}

# ---------------------------------------------------------------
# Network mode trigger — forces Service (CLB) recreation when
# enable_public_network flips. Public↔internal requires a new CLB
# instance (different type / different VIP), so recreation is the
# correct behaviour. Without this, the lifecycle ignore_changes on
# annotations would silently suppress the CLB type switch.
# ---------------------------------------------------------------
resource "null_resource" "network_mode_trigger" {
  triggers = {
    enable_public_network = tostring(var.enable_public_network)
  }
}

# ---------------------------------------------------------------
# cube-api: Deployment → CLB Service (public network)
# ---------------------------------------------------------------

resource "kubernetes_deployment" "cube_api" {
  count = local.deploy_addons ? 1 : 0
  metadata {
    name      = "cube-api"
    namespace = kubernetes_namespace.cubesandbox[0].metadata[0].name
    labels    = { app = "cube-api" }
  }
  spec {
    replicas = var.cube_api_replicas
    selector {
      match_labels = { app = "cube-api" }
    }
    template {
      metadata {
        labels = { app = "cube-api" }
      }
      spec {
        container {
          name  = "cube-api"
          image = local.cube_api_image
          args = [
            "--cubemaster-url",
            local.cubemaster_url,
            "--sandbox-domain",
            "cube.app",
          ]
          env {
            name  = "CUBE_MASTER_ADDR"
            value = local.cubemaster_url
          }
          port {
            container_port = 3000
          }
          readiness_probe {
            http_get {
              path = "/health"
              port = 3000
            }
            initial_delay_seconds = 5
            period_seconds        = 5
          }
        }
      }
    }
  }
}

resource "kubernetes_service" "cube_api" {
  count = local.deploy_addons ? 1 : 0
  metadata {
    name      = "cube-api"
    namespace = kubernetes_namespace.cubesandbox[0].metadata[0].name
    # When enable_public_network is false (default), pin the CLB to a
    # VPC-internal subnet so it only gets a private VIP. When true, omit the
    # internal-subnetid annotation so TKE provisions a public CLB.
    annotations = merge({
      "service.cloud.tencent.com/modification-protection" = "false"
      "service.cloud.tencent.com/pass-to-target"          = "true"
      "service.cloud.tencent.com/security-groups"         = tencentcloud_security_group.clb.id
      }, var.enable_public_network ? {
      "service.kubernetes.io/qcloud-loadbalancer-internet-charge-type" = "TRAFFIC_POSTPAID_BY_HOUR"
      } : {
      "service.kubernetes.io/qcloud-loadbalancer-internal-subnetid" = tencentcloud_subnet.cluster.id
    })
  }
  lifecycle {
    # TKE controller-manager injects runtime annotations (e.g. bindedip,
    # loadbalanceId) that would otherwise cause perpetual drift on every plan.
    ignore_changes = [
      metadata[0].annotations,
    ]
    # Force Service (and hence CLB) recreation when the network mode flips.
    # Public↔internal requires a brand-new CLB instance, so recreation is safe
    # and expected — the VIP will change.
    replace_triggered_by = [
      null_resource.network_mode_trigger,
    ]
  }

  spec {
    type     = "LoadBalancer"
    selector = { app = "cube-api" }
    port {
      port     = 3000
      protocol = "TCP"
    }
  }
}

# ---------------------------------------------------------------
# cube-lifecycle-manager: Secret → Deployment → ClusterIP Service
# ---------------------------------------------------------------

resource "kubernetes_secret" "cube_lifecycle_manager_conf" {
  count = local.deploy_addons ? 1 : 0
  type  = "Opaque"

  metadata {
    name      = "cube-lifecycle-manager-conf"
    namespace = kubernetes_namespace.cubesandbox[0].metadata[0].name
  }

  data = {
    "redis-password"   = var.redis_password
    "cube-admin-token" = local.cube_admin_token
  }
}

resource "kubernetes_deployment" "cube_lifecycle_manager" {
  count = local.deploy_addons ? 1 : 0

  metadata {
    name      = "cube-lifecycle-manager"
    namespace = kubernetes_namespace.cubesandbox[0].metadata[0].name
    labels    = { app = "cube-lifecycle-manager" }
  }

  spec {
    replicas = var.cube_lifecycle_manager_replicas

    selector {
      match_labels = { app = "cube-lifecycle-manager" }
    }

    template {
      metadata {
        labels = { app = "cube-lifecycle-manager" }
      }

      spec {
        container {
          name  = "cube-lifecycle-manager"
          image = local.cube_lcm_image

          port {
            name           = "http"
            container_port = 8083
            protocol       = "TCP"
          }

          env {
            name  = "CUBE_LCM_REDIS_ADDR"
            value = "${tencentcloud_redis_instance.redis.ip}:6379"
          }
          env {
            name = "CUBE_LCM_REDIS_PASSWORD"
            value_from {
              secret_key_ref {
                name = kubernetes_secret.cube_lifecycle_manager_conf[0].metadata[0].name
                key  = "redis-password"
              }
            }
          }
          env {
            name  = "CUBE_LCM_REDIS_DB"
            value = "0"
          }
          env {
            name  = "CUBE_LCM_CUBEMASTER_URL"
            value = local.cubemaster_url
          }
          env {
            name  = "CUBE_LCM_LISTEN_ADDR"
            value = "0.0.0.0:8083"
          }
          env {
            name  = "CUBE_LCM_DEFAULT_IDLE_TIMEOUT"
            value = var.cube_lifecycle_manager_default_idle_timeout
          }
          env {
            name  = "CUBE_LCM_HEARTBEAT_TTL"
            value = var.cube_lifecycle_manager_heartbeat_ttl
          }
          env {
            name  = "CUBE_LCM_DISCOVERY_REFRESH"
            value = var.cube_lifecycle_manager_discovery_refresh
          }
          env {
            name = "CUBE_LCM_ADMIN_TOKEN"
            value_from {
              secret_key_ref {
                name = kubernetes_secret.cube_lifecycle_manager_conf[0].metadata[0].name
                key  = "cube-admin-token"
              }
            }
          }

          liveness_probe {
            http_get {
              path = "/healthz"
              port = 8083
            }
            initial_delay_seconds = 10
            period_seconds        = 10
            timeout_seconds       = 3
            failure_threshold     = 6
          }

          readiness_probe {
            http_get {
              path = "/readyz"
              port = 8083
            }
            initial_delay_seconds = 5
            period_seconds        = 5
            timeout_seconds       = 3
            failure_threshold     = 3
          }
        }
      }
    }
  }

  depends_on = [
    kubernetes_service.cubemaster,
    tencentcloud_redis_instance.redis,
  ]
}

resource "kubernetes_service" "cube_lifecycle_manager" {
  count = local.deploy_addons ? 1 : 0

  metadata {
    name      = "cube-lifecycle-manager"
    namespace = kubernetes_namespace.cubesandbox[0].metadata[0].name
    labels    = { app = "cube-lifecycle-manager" }
  }

  spec {
    type     = "ClusterIP"
    selector = { app = "cube-lifecycle-manager" }

    port {
      name        = "http"
      port        = 8083
      target_port = 8083
      protocol    = "TCP"
    }
  }
}

# ---------------------------------------------------------------
# cube-proxy: Secrets → Deployment → CLB Service (public network)
# ---------------------------------------------------------------

# global.conf — embeds the Redis password, so store it as a Secret (not a
# ConfigMap) and mount it as a file into the cube-proxy pod.
resource "kubernetes_secret" "cubeproxy_global" {
  count = local.deploy_addons ? 1 : 0
  type  = "Opaque"
  metadata {
    name      = "cubeproxy-global"
    namespace = kubernetes_namespace.cubesandbox[0].metadata[0].name
  }
  data = {
    "global.conf" = <<-EOT
      set $redis_ip "${tencentcloud_redis_instance.redis.ip}";
      set $redis_port "6379";
      set $redis_pd "${var.redis_password}";
      set $redis_index 0;
      set $timeout_min 500;
      set $timeout_max 700;
      set $cube_proxy_host_ip "127.0.0.1";
      set $cube_sidecar_addr "cube-lifecycle-manager.cubesandbox.svc.cluster.local:8083";
      set $cube_admin_token "${local.cube_admin_token}";
    EOT
    # Same Redis password exposed as a discrete key so the cube-proxy container
    # can read it via secret_key_ref instead of a plaintext Deployment env value
    # (which would show up in `kubectl get deploy -o yaml`). Projected out of the
    # global.conf volume mount via that mount's explicit `items` below.
    "redis-password" = var.redis_password
  }
}

resource "kubernetes_config_map" "cubeproxy_nginx_conf" {
  count = local.deploy_addons ? 1 : 0

  metadata {
    name      = "cubeproxy-nginx-conf"
    namespace = kubernetes_namespace.cubesandbox[0].metadata[0].name
    labels    = { app = "cube-proxy" }
  }

  data = {
    "nginx.conf" = local.cubeproxy_nginx_conf
  }
}

# Certificate Secret (from the cubeproxy-certs/ directory). This holds the
# cube-proxy server cert AND its PRIVATE key (cube.app+3-key.pem), so it must be
# a Secret, not a ConfigMap — a ConfigMap is stored unencrypted in etcd and is
# readable by anyone with `get configmap`, which would leak the TLS private key.
resource "kubernetes_secret" "cubeproxy_certs" {
  count = local.deploy_addons && length(local.cert_files) > 0 ? 1 : 0
  type  = "Opaque"
  metadata {
    name      = "cubeproxy-certs"
    namespace = kubernetes_namespace.cubesandbox[0].metadata[0].name
  }
  data = {
    for f in local.cert_files : lower(replace(f, "/[^a-zA-Z0-9-]/", "-")) => file("${path.module}/cubeproxy-certs/${f}")
  }
}

# cube-proxy Deployment
resource "kubernetes_deployment" "cube_proxy" {
  count = local.deploy_addons ? 1 : 0

  depends_on = [
    kubernetes_service.cube_lifecycle_manager,
    tencentcloud_redis_instance.redis,
  ]

  metadata {
    name      = "cube-proxy"
    namespace = kubernetes_namespace.cubesandbox[0].metadata[0].name
    labels    = { app = "cube-proxy" }
  }
  spec {
    # PR #705 moves lifecycle coordination into cube-lifecycle-manager. Each
    # cube-proxy replica publishes its admin endpoint to Redis so CLM can
    # discover and push lifecycle state without static per-replica wiring.
    replicas = var.cube_proxy_replicas
    selector {
      match_labels = { app = "cube-proxy" }
    }
    template {
      metadata {
        labels = { app = "cube-proxy" }
      }
      spec {
        container {
          name  = "cube-proxy"
          image = local.cube_proxy_image
          port {
            name           = "proxy"
            container_port = 8080
            protocol       = "TCP"
          }
          port {
            name           = "http"
            container_port = 8081
            protocol       = "TCP"
          }
          port {
            name           = "http80"
            container_port = 80
            protocol       = "TCP"
          }
          port {
            name           = "https"
            container_port = 443
            protocol       = "TCP"
          }
          port {
            name           = "admin"
            container_port = 8082
            protocol       = "TCP"
          }
          env {
            name  = "CUBE_PROXY_REDIS_IP"
            value = tencentcloud_redis_instance.redis.ip
          }
          env {
            name  = "CUBE_PROXY_REDIS_PORT"
            value = "6379"
          }
          env {
            name = "CUBE_PROXY_REDIS_PASSWORD"
            value_from {
              secret_key_ref {
                name = kubernetes_secret.cubeproxy_global[0].metadata[0].name
                key  = "redis-password"
              }
            }
          }
          env {
            name  = "CUBE_PROXY_REGISTRY_ENABLE"
            value = "1"
          }
          env {
            name = "POD_NAME"
            value_from {
              field_ref {
                field_path = "metadata.name"
              }
            }
          }
          env {
            name = "POD_IP"
            value_from {
              field_ref {
                field_path = "status.podIP"
              }
            }
          }
          env {
            name  = "CUBE_PROXY_ID"
            value = "$(POD_NAME)"
          }
          env {
            name  = "CUBE_PROXY_ADMIN_URL"
            value = "http://$(POD_IP):8082"
          }
          env {
            name  = "CUBE_PROXY_RESUME_URL"
            value = "http://$(POD_IP):8082"
          }
          env {
            name  = "CUBE_PROXY_NODE_IP"
            value = "$(POD_IP)"
          }
          env {
            name  = "CUBE_PROXY_VERSION"
            value = var.image_tag
          }
          env {
            name  = "CUBE_PROXY_HEARTBEAT_INTERVAL_MS"
            value = tostring(var.cube_proxy_heartbeat_interval_ms)
          }
          env {
            name  = "CUBE_PROXY_REGISTRY_REDIS_HOST"
            value = tencentcloud_redis_instance.redis.ip
          }
          env {
            name  = "CUBE_PROXY_REGISTRY_REDIS_PORT"
            value = "6379"
          }
          env {
            name = "CUBE_PROXY_REGISTRY_REDIS_PASSWORD"
            value_from {
              secret_key_ref {
                name = kubernetes_secret.cubeproxy_global[0].metadata[0].name
                key  = "redis-password"
              }
            }
          }
          env {
            name  = "CUBE_PROXY_REGISTRY_REDIS_DB"
            value = "0"
          }

          # global.conf mount
          volume_mount {
            name       = "global-conf"
            mount_path = "/usr/local/openresty/nginx/conf/global"
            read_only  = true
          }

          volume_mount {
            name       = "nginx-conf"
            mount_path = "/usr/local/openresty/nginx/conf/nginx.conf"
            sub_path   = "nginx.conf"
            read_only  = true
          }

          # Certificate volume mounts (dynamic)
          dynamic "volume_mount" {
            for_each = local.cert_files
            content {
              name       = "cert-${lower(replace(volume_mount.value, "/[^a-zA-Z0-9-]/", "-"))}"
              mount_path = "/usr/local/openresty/nginx/certs/${volume_mount.value}"
              sub_path   = lower(replace(volume_mount.value, "/[^a-zA-Z0-9-]/", "-"))
              read_only  = true
            }
          }

          # --- Health probes ---
          # liveness: if nginx stops accepting connections on the dataplane port
          # (process hang / deadlock), kubelet restarts the container. A plain
          # crash/OOM is already covered by restartPolicy: Always; this catches
          # the "still alive but unresponsive" case.
          liveness_probe {
            tcp_socket {
              port = 8081
            }
            initial_delay_seconds = 5
            period_seconds        = 10
            timeout_seconds       = 3
            failure_threshold     = 3
            success_threshold     = 1
          }

          # readiness: only route traffic once nginx is accepting connections, so
          # the endpoint is removed from the Service during restart and the CLB
          # stops forwarding to this pod before it is ready (avoids 502s).
          readiness_probe {
            tcp_socket {
              port = 8081
            }
            initial_delay_seconds = 3
            period_seconds        = 5
            timeout_seconds       = 2
            failure_threshold     = 2
            success_threshold     = 1
          }
        }

        # global.conf volume (Secret: contains the Redis password). Project ONLY
        # global.conf so the discrete redis-password key in the same Secret is not
        # also surfaced as a file under conf/global/.
        volume {
          name = "global-conf"
          secret {
            secret_name = kubernetes_secret.cubeproxy_global[0].metadata[0].name
            items {
              key  = "global.conf"
              path = "global.conf"
            }
          }
        }

        volume {
          name = "nginx-conf"
          config_map {
            name = kubernetes_config_map.cubeproxy_nginx_conf[0].metadata[0].name
          }
        }

        # Certificate volume (dynamic). Sourced from the Secret above so the TLS
        # private key is never projected from a ConfigMap.
        dynamic "volume" {
          for_each = local.cert_files
          content {
            name = "cert-${lower(replace(volume.value, "/[^a-zA-Z0-9-]/", "-"))}"
            secret {
              secret_name = kubernetes_secret.cubeproxy_certs[0].metadata[0].name
              items {
                key  = lower(replace(volume.value, "/[^a-zA-Z0-9-]/", "-"))
                path = lower(replace(volume.value, "/[^a-zA-Z0-9-]/", "-"))
              }
            }
          }
        }
      }
    }
  }
}

# cube-proxy CLB Service (public network 80/443)
resource "kubernetes_service" "cube_proxy" {
  count = local.deploy_addons ? 1 : 0
  metadata {
    name      = "cube-proxy"
    namespace = kubernetes_namespace.cubesandbox[0].metadata[0].name
    # Public mode: a public CLB billed by traffic (internet-charge-type).
    # Internal mode (default): pin to a VPC-internal subnet for a private VIP.
    annotations = merge({
      "service.cloud.tencent.com/specify-protocol"        = "{\"80\":{\"protocol\":[\"TCP\"]},\"443\":{\"protocol\":[\"TCP\"]}}"
      "service.cloud.tencent.com/modification-protection" = "false"
      "service.cloud.tencent.com/pass-to-target"          = "true"
      "service.cloud.tencent.com/security-groups"         = tencentcloud_security_group.clb.id
      }, var.enable_public_network ? {
      "service.kubernetes.io/qcloud-loadbalancer-internet-charge-type" = "TRAFFIC_POSTPAID_BY_HOUR"
      } : {
      "service.kubernetes.io/qcloud-loadbalancer-internal-subnetid" = tencentcloud_subnet.cluster.id
    })
  }
  lifecycle {
    # TKE controller-manager injects runtime annotations (e.g. bindedip,
    # loadbalanceId) that would otherwise cause perpetual drift on every plan.
    ignore_changes = [
      metadata[0].annotations,
    ]
    # Force Service (and hence CLB) recreation when the network mode flips.
    # Public↔internal requires a brand-new CLB instance, so recreation is safe
    # and expected — the VIP will change.
    replace_triggered_by = [
      null_resource.network_mode_trigger,
    ]
  }

  spec {
    type     = "LoadBalancer"
    selector = { app = "cube-proxy" }
    port {
      name        = "tcp-80"
      port        = 80
      target_port = 8081
      protocol    = "TCP"
    }
    port {
      name        = "tcp-ssl-443"
      port        = 443
      target_port = 8080
      protocol    = "TCP"
    }
  }
}

# ---------------------------------------------------------------
# cube-webui: ConfigMap (nginx.conf) → Deployment → CLB Service (public network 80)
# ---------------------------------------------------------------

resource "kubernetes_config_map" "cube_webui_nginx_conf" {
  count = local.deploy_addons ? 1 : 0
  metadata {
    name      = "cube-webui-nginx-conf"
    namespace = kubernetes_namespace.cubesandbox[0].metadata[0].name
    labels    = { app = "cube-webui" }
  }
  data = {
    # webui/nginx.conf has two upstream placeholders that must both be replaced,
    # otherwise nginx rejects the leftover literal with "invalid URL prefix".
    # webui runs inside the cluster, so it reaches the backends over the internal
    # (VPC) network via their Service ClusterIPs instead of the public CLB IPs:
    #   __WEB_UI_UPSTREAM__        → cube-api   (the /cubeapi/ backend, port 3000)
    #   __SANDBOX_PROXY_UPSTREAM__ → cube-proxy (the /sandbox/ backend, port 80)
    "nginx.conf" = replace(
      replace(
        local.webui_nginx_conf,
        "__WEB_UI_UPSTREAM__",
        "http://${kubernetes_service.cube_api[0].spec[0].cluster_ip}:3000"
      ),
      "__SANDBOX_PROXY_UPSTREAM__",
      "http://${kubernetes_service.cube_proxy[0].spec[0].cluster_ip}"
    )
  }

  depends_on = [kubernetes_service.cube_api, kubernetes_service.cube_proxy]
}

resource "kubernetes_deployment" "cube_webui" {
  count = local.deploy_addons ? 1 : 0
  metadata {
    name      = "cube-webui"
    namespace = kubernetes_namespace.cubesandbox[0].metadata[0].name
    labels    = { app = "cube-webui" }
  }
  spec {
    replicas = var.cube_webui_replicas
    selector {
      match_labels = { app = "cube-webui" }
    }
    template {
      metadata {
        labels = { app = "cube-webui" }
      }
      spec {
        container {
          name              = "webui"
          image             = local.cube_webui_image
          image_pull_policy = "Always"
          port {
            name           = "http"
            container_port = 80
            protocol       = "TCP"
          }
          volume_mount {
            name       = "nginx-conf"
            mount_path = "/usr/local/openresty/nginx/conf/nginx.conf"
            sub_path   = "nginx.conf"
            read_only  = true
          }
          resources {
            requests = {
              cpu    = "100m"
              memory = "128Mi"
            }
            limits = {
              cpu    = "500m"
              memory = "256Mi"
            }
          }
          liveness_probe {
            http_get {
              path = "/"
              port = "http"
            }
            initial_delay_seconds = 10
            period_seconds        = 10
            timeout_seconds       = 3
            failure_threshold     = 6
          }
          readiness_probe {
            http_get {
              path = "/"
              port = "http"
            }
            initial_delay_seconds = 5
            period_seconds        = 5
            timeout_seconds       = 3
            failure_threshold     = 3
          }
        }
        volume {
          name = "nginx-conf"
          config_map {
            name = kubernetes_config_map.cube_webui_nginx_conf[0].metadata[0].name
          }
        }
      }
    }
  }
}

resource "kubernetes_service" "cube_webui" {
  count = local.deploy_addons ? 1 : 0
  metadata {
    name      = "cube-webui"
    namespace = kubernetes_namespace.cubesandbox[0].metadata[0].name
    # When enable_public_network is false (default), pin the CLB to a
    # VPC-internal subnet so it only gets a private VIP. When true, bill by
    # traffic (matching cube-api / cube-proxy) for cost predictability.
    annotations = merge({
      "service.cloud.tencent.com/modification-protection" = "false"
      "service.cloud.tencent.com/pass-to-target"          = "true"
      "service.cloud.tencent.com/security-groups"         = tencentcloud_security_group.clb.id
      }, var.enable_public_network ? {
      "service.kubernetes.io/qcloud-loadbalancer-internet-charge-type" = "TRAFFIC_POSTPAID_BY_HOUR"
      } : {
      "service.kubernetes.io/qcloud-loadbalancer-internal-subnetid" = tencentcloud_subnet.cluster.id
    })
  }
  lifecycle {
    # TKE controller-manager injects runtime annotations (e.g. bindedip,
    # loadbalanceId) that would otherwise cause perpetual drift on every plan.
    ignore_changes = [
      metadata[0].annotations,
    ]
    # Force Service (and hence CLB) recreation when the network mode flips.
    # Public↔internal requires a brand-new CLB instance, so recreation is safe
    # and expected — the VIP will change.
    replace_triggered_by = [
      null_resource.network_mode_trigger,
    ]
  }

  spec {
    type     = "LoadBalancer"
    selector = { app = "cube-webui" }
    port {
      name     = "http"
      port     = 80
      protocol = "TCP"
    }
  }
}

# ---------------------------------------------------------------
# Output the TKE CLB IPs
# ---------------------------------------------------------------
output "tke_cubemaster_clb_ip" {
  value = local.deploy_addons ? kubernetes_service.cubemaster[0].status[0].load_balancer[0].ingress[0].ip : ""
}

output "tke_cube_api_clb_ip" {
  value = local.deploy_addons ? kubernetes_service.cube_api[0].status[0].load_balancer[0].ingress[0].ip : ""
}

output "tke_cube_proxy_clb_ip" {
  value = local.deploy_addons ? kubernetes_service.cube_proxy[0].status[0].load_balancer[0].ingress[0].ip : ""
}

output "tke_cube_webui_clb_ip" {
  value = local.deploy_addons ? kubernetes_service.cube_webui[0].status[0].load_balancer[0].ingress[0].ip : ""
}
