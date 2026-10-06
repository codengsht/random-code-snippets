# TTL cleanup policies.
#
# Every file in policies/ is one policy, authored in DynamoDB's attribute-value
# JSON, and written to the table verbatim. There is deliberately no type-mapping
# layer here: a mapping has to be edited for every new field, and a field it
# does not know about is dropped silently, which would make a delete policy more
# permissive than its file says.
#
# Requires Terraform 1.4+ for terraform_data.

locals {
  policy_dir   = "${path.module}/policies"
  policy_files = fileset(local.policy_dir, "*.json")

  # Keyed by file name. Used by the file-naming check below.
  policy_by_file = {
    for f in local.policy_files : f => jsondecode(file("${local.policy_dir}/${f}"))
  }

  # Keyed by rule_id, so renaming a file is a no-op and two files sharing a
  # rule_id fail the plan with "Two different items produced the key".
  policy_docs = {
    for f, p in local.policy_by_file : p.rule_id.S => p
  }
}

locals {
  # Attribute names a policy file is allowed to contain. Adding a field to the
  # schema means adding it here. policy_hash is absent on purpose: Terraform
  # computes it, so a file must not carry it.
  allowed_attributes = [
    "rule_id",
    "schema_version",
    "description",
    "mode",
    "resource_type",
    "scope",
    "conditions",
    "exclusions",
    "action",
  ]

  supported_schema_versions = ["1"]
  valid_modes               = ["DISABLED", "DRY_RUN", "ENFORCE"]
  valid_actions             = ["ADD_TAGS", "REMOVE_TAGS", "DELETE", "DEREGISTER"]

  # Destructive actions need an age condition at or above this many days,
  # whatever a file asks for.
  destructive_actions      = ["DELETE", "DEREGISTER"]
  min_destructive_age_days = 7

  # Offender lists, computed once so the error messages can name them.
  # try() on the typed path doubles as a type-code check: a value written with
  # the wrong type code misses the path and falls through to the safe default.
  unknown_attributes = flatten([
    for id, p in local.policy_docs : [
      for k in keys(p) : "${id}.${k}" if !contains(local.allowed_attributes, k)
    ]
  ])

  misnamed_files = [
    for f, p in local.policy_by_file : f
    if trimsuffix(f, ".json") != try(p.rule_id.S, "")
  ]

  invalid_schema_versions = [
    for id, p in local.policy_docs : id
    if !contains(local.supported_schema_versions, try(p.schema_version.N, ""))
  ]

  invalid_modes = [
    for id, p in local.policy_docs : id
    if !contains(local.valid_modes, try(p.mode.S, ""))
  ]

  invalid_actions = [
    for id, p in local.policy_docs : id
    if !contains(local.valid_actions, try(p.action.M.type.S, ""))
  ]

  underage_destructive = [
    for id, p in local.policy_docs : id
    if contains(local.destructive_actions, try(p.action.M.type.S, ""))
    && tonumber(try(p.conditions.M.older_than_days.N, "0")) < local.min_destructive_age_days
  ]
}

# Plan-time guardrails. The Go loader revalidates every policy when it reads
# them; these exist to fail the plan instead of failing at runtime.
resource "terraform_data" "policy_validation" {
  input = sort(keys(local.policy_docs))

  lifecycle {
    precondition {
      condition     = length(local.policy_docs) > 0
      error_message = "No policy files found in ${local.policy_dir}."
    }

    precondition {
      condition     = length(local.unknown_attributes) == 0
      error_message = "Unknown attributes: ${join(", ", local.unknown_attributes)}. Add them to local.allowed_attributes if they are intentional."
    }

    precondition {
      condition     = length(local.misnamed_files) == 0
      error_message = "Each policy file must be named <rule_id>.json. Mismatched: ${join(", ", local.misnamed_files)}."
    }

    precondition {
      condition     = length(local.invalid_schema_versions) == 0
      error_message = "Unsupported schema_version in: ${join(", ", local.invalid_schema_versions)}. Supported: ${join(", ", local.supported_schema_versions)}."
    }

    precondition {
      condition     = length(local.invalid_modes) == 0
      error_message = "mode must be one of ${join(", ", local.valid_modes)}, written as {\"S\": \"...\"}. Offenders: ${join(", ", local.invalid_modes)}."
    }

    precondition {
      condition     = length(local.invalid_actions) == 0
      error_message = "action.type must be one of ${join(", ", local.valid_actions)}. Offenders: ${join(", ", local.invalid_actions)}."
    }

    precondition {
      condition     = length(local.underage_destructive) == 0
      error_message = "${join(", ", local.underage_destructive)}: a ${join("/", local.destructive_actions)} policy needs conditions.older_than_days of at least ${local.min_destructive_age_days}."
    }
  }
}

resource "aws_dynamodb_table_item" "policy" {
  for_each = local.policy_docs

  table_name = module.dynamodb.dynamodb_table_name
  hash_key   = "rule_id"

  # The file's attributes verbatim, plus a content hash Terraform computes.
  # Decoding and re-encoding normalises whitespace and key order, so
  # reformatting a file does not change the hash. The hash covers the policy
  # content only, never itself.
  item = jsonencode(merge(
    each.value,
    { policy_hash = { S = sha256(jsonencode(each.value)) } }
  ))

  depends_on = [terraform_data.policy_validation]
}

output "ttl_policies" {
  description = "Policy rule_id to content hash, for cross-checking against action logs."
  value = {
    for id, p in local.policy_docs : id => {
      mode          = try(p.mode.S, null)
      resource_type = try(p.resource_type.S, null)
      policy_hash   = sha256(jsonencode(p))
    }
  }
}
