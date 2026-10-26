# ---------------------------------------------------------------
# query_outputs.tf — read-only outputs consumed by create.sh's interactive
# selectors (select_zone / select_instance_type via terraform_plan_json) to
# present an ONLINE, region-aware list of availability zones and CVM instance
# types. They simply re-expose the data sources already declared in main.tf so
# that `terraform plan -json` carries them under
# .planned_values.outputs._zones / ._instance_types, which create.sh parses.
#
# These outputs depend ONLY on data sources (no managed resources), so the
# metadata plan that create.sh runs to read them does not need the TKE cluster
# or the kubernetes provider (create.sh runs that plan with create_tke=false and
# deploy_tke_addons=false).
#
# IMPORTANT — availability varies by region AND by availability zone: the set of
# zones differs per region, and within a region a given instance family/spec may
# be sold out or simply not offered in some zones. These lists reflect what the
# Tencent Cloud API reports for the CURRENTLY configured region; the final choice
# is still validated at apply time (create.sh retries with zone/instance-type
# fallback if the picked combination is unavailable).
# ---------------------------------------------------------------

# CVM availability zones offered in the configured region. Filtered to the zones
# the API reports AVAILABLE (local.available_zones) so the interactive menu and the
# non-interactive "first zone" default never offer a retired zone (e.g.
# ap-guangzhou-1) that would fail the apply with InvalidZone.MismatchRegion.
output "_zones" {
  description = "Creatable CVM availability zones in the configured region (online; the set of zones varies per region)."
  value = [
    for z in local.available_zones : {
      name        = z.name
      description = try(z.description, z.name)
    }
  ]
}

# Candidate CVM instance types in the configured region. These are the curated
# specs declared in main.tf (exclude_sold_out = true); create.sh further filters
# them to the recommended config (CPU >= 4, RAM >= 8, S3+ series).
output "_instance_types" {
  description = "Candidate CVM instance types in the configured region (online; per-zone availability still varies)."
  value = [
    for t in concat(
      data.tencentcloud_instance_types.spec_8c16g.instance_types,
      data.tencentcloud_instance_types.spec_4c8g.instance_types,
      data.tencentcloud_instance_types.spec_2c4g.instance_types,
      ) : {
      type   = t.instance_type
      cpu    = try(t.cpu_core_count, 0)
      memory = try(t.memory_size, 0)
    }
  ]
}
