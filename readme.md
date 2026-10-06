# TTL policies DynamoDB table

Terraform for the `ttl_policies` DynamoDB table and the TTL cleanup policies stored in it. The policies decide which AWS resources get tagged, untagged, deregistered or deleted, and when.

## Purpose

The table is the single, central source of truth for TTL cleanup policies.

- Each policy is one item, managed from the central IS PCE account.
- The cleanup Lambdas in every LOB account read their policies from this table. They have no code for individual policies, so changing a threshold, tag, exclusion, scope or mode is a table change, not a Lambda deployment.
- Policies cover AMIs, EBS volumes, EBS snapshots and Lambda functions in us-east-1, us-west-1 and us-west-2.
- The table itself lives in one region, us-east-1. Every region's Lambdas read that one table. See [Single table vs global table](#single-table-vs-global-table).
- It replaces the TTL policies that ran in Prisma Cloud.

How the table is used:

1. An EventBridge schedule in each LOB account and region starts the Discovery Lambda.
2. Discovery scans the table in us-east-1 with a strongly consistent read, validates each policy, and evaluates the ones that apply to its account and region.
3. Matches go to an SQS work queue, with a DLQ for failed messages.
4. The Worker Lambda re-reads the policy with a strongly consistent `GetItem`, rechecks the resource, and then acts.

## Key decisions

- **One item per policy, keyed by `rule_id`.** There's one record per policy and no per-account overrides, so `rule_id` alone identifies an item. No sort key and no generic `PK`/`SK`.
- **The item holds the whole policy.** Resource type, conditions, exclusions, scope, mode and action all live in the table. The Lambdas only know a fixed set of fields per resource type. A new policy built from existing fields needs only a table change. A new kind of check needs a code change.
- **Named fields, not a query language.** Checks are named fields like `older_than_days` and `name_prefixes`, not field/operator/value expressions. Every policy has the same shape and leaves out the fields it doesn't use.
- **Same results, not the same conditions.** Policies have to produce the same outcomes as the TTL automation they replace. Checks that this design makes unnecessary are left out.
- **Native DynamoDB types, authored directly.** Each policy is one file under `policies/`, written in DynamoDB's attribute-value JSON, and Terraform writes it to the table verbatim. Nested data stays maps and lists rather than a JSON string in one attribute. There is deliberately no type-mapping layer in Terraform: a mapping has to be edited for every new field, and a field it doesn't know about is dropped silently, which would make a delete policy more permissive than its file says. See [How policies are stored](#how-policies-are-stored).
- **Terraform is the only writer.** A policy can delete resources in every account, so every change goes through PR review. The table's resource-based policy blocks item writes from anyone else.
- **Safety checks live in code.** Table edits can't turn them off. See [Safety checks](#safety-checks).
- **One table in one region.** The table is in us-east-1 only, and the us-west Lambdas read it cross-region. That keeps it to one table, one key and one policy to maintain, and a daily cleanup job tolerates a missed run. See [Single table vs global table](#single-table-vs-global-table).
- **Streams on from day one.** Nothing reads the stream today. It's enabled with `NEW_AND_OLD_IMAGES` because replicas require it and the view type can't be changed once replicas exist, so this keeps the global table option open at no cost.
- **Customer-managed encryption key.** The module encrypts the table at rest with the key passed as `kms_key_arn`. The key isn't stored in the table. It stays in KMS in the PCE account, and only roles its key policy allows can read the table's data. One key in us-east-1 covers readers in every region.
- **Lambda age comes from the `created_timestamp` tag.** The Lambda API has no creation time. The enforced Lambda module adds this tag to every function, and functions created outside the module never match.

## Table schema

These are settings on the table itself, not data stored in it. The only data in the table is the policy items described in [Item format](#item-format).

| Setting | Value |
|---|---|
| Account | The central PCE account |
| Table name | `ttl_policies` |
| Partition key | `rule_id` (String) |
| Sort key | None |
| Capacity mode | On-demand |
| Region | us-east-1 only. No replicas |
| Encryption | One customer-managed KMS key in us-east-1, in the PCE account |
| Streams | Enabled with `NEW_AND_OLD_IMAGES`. Nothing reads it; it's on so replicas stay possible |
| Resource-based policy | One, on the table. See [Access](#access) |
| Deletion protection | Recommended |

- Only key attributes go in `attributes`. DynamoDB rejects definitions for attributes that no key uses. Every other field is free-form, so the validator enforces the item format.
- The partition key can't change after the table is created. Global secondary indexes can be added later, but none are needed: Discovery reads every policy in one scan.
- One table removes replication lag, but it doesn't make reads current on its own: DynamoDB reads are eventually consistent by default. Discovery and the Worker both pass `ConsistentRead=true`, so a read that starts after an apply completes sees the new policy. Strongly consistent reads cost twice the read units, which is nothing on eight small items.

```hcl
module "dynamodb" {
  source              = "repo.usaa.com/usaa-terraform__usaa/terraform-aws-dynamodb/aws"
  version             = "11.0.0"
  dynamodb_table_name = "ttl_policies"
  hash_key            = "rule_id"
  tags                = data.usaatags_resource_tags.tags.tags
  kms_key_arn         = module.kms_key.key_arn # us-east-1 key

  attributes = [
    { name = "rule_id", type = "S" }
  ]

  # Nothing reads the stream today. It's on so replicas stay possible later,
  # since the view type can't be changed once replicas exist.
  stream_enabled   = true
  stream_view_type = "NEW_AND_OLD_IMAGES"

  ci_job_id              = var.ci_job_id
  datasensitivityclasscd = "" # see https://go/SDMMatrix
}
```

Confirm the module supports the stream and resource policy settings, and check its input names. See [Open items](#open-items).

## Single table vs global table

The table is in us-east-1 only, so the Lambdas in us-west-1 and us-west-2 read it across regions. The alternative is a global table with a replica in each region.

Only one thing differs between the two: whether a Discovery read stays inside its own region. The items, schema, validation, Worker and safety checks are identical either way.

| | Single table (current) | Global table |
|---|---|---|
| Tables | 1 | 3 replicas |
| KMS keys | 1, in us-east-1 | 1 per region. Multi-Region keys share key material but not key policies, so it's 3 policies either way |
| Resource-based policies | 1 | 1 per replica |
| Read path from us-west | Cross-region, so a region-local gateway endpoint won't serve it | In-region |
| Regional outage | us-east-1 down pauses cleanup everywhere | Each region keeps running |
| Replication failure | None | 20 hours without replication or key access permanently converts a replica to a standalone table |
| Policy changes | A strongly consistent read sees the update once the apply completes | Replication lag on top of that, and a strongly consistent read only reflects its own replica |
| Cost | Baseline | A couple of extra KMS keys. Replicated writes and cross-region transfer round to nothing at this size |

Why single region for now:

- This is a daily batch job reading eight small items. A missed run costs less than three key policies drifting apart.
- One key policy covers readers in every region. `kms:ViaService` names the table's region, not the caller's, so every LOB role goes through `dynamodb.us-east-1.amazonaws.com`.
- There's no replication to break and no 20-hour conversion hazard.

What has to be true for it to work: the us-west Lambdas need a network path to `dynamodb.us-east-1.amazonaws.com`. Gateway VPC endpoints are region-local and don't work from a peered VPC in another region or through a transit gateway, so if the LOB VPCs reach DynamoDB that way, a cross-region read fails. The options are an interface endpoint in a us-east-1 VPC reached over peering, NAT or internet egress, or running Discovery outside the VPC. It only calls public AWS APIs (EC2, Lambda, DynamoDB, SQS), so VPC attachment may not be needed. This is an [open item](#open-items).

### Switching to a global table later

Replicas can be added to a table that already has data, and DynamoDB backfills them. Some AWS docs still carry an "empty table" requirement inherited from the legacy global tables API, so verify on a throwaway table first.

Two things have to be right from the start, because they can't be changed later: streams with `NEW_AND_OLD_IMAGES`, already enabled, and `rule_id` as the partition key. Everything else is additive: a KMS key and key policy per new region, a resource policy per replica, aliased Terraform providers, and the replication service-linked role exempted from the write deny.

The Lambdas read the table's region from a `TABLE_REGION` environment variable that defaults to `AWS_REGION`. Today it's set to `us-east-1` everywhere. With a global table you drop it and each Lambda reads its local replica, so the switch is a config change, not a code change.

## Item format

| Field | Type | Meaning |
|---|---|---|
| `rule_id` | String | Partition key. A stable name like `lambda-delete`. Keep thresholds out of it, since they can change |
| `schema_version` | Number | Version of the item format. Discovery skips items with a version it doesn't support |
| `policy_hash` | String | Added by Terraform, never written in a file. `sha256` of the policy content, logged with every action |
| `description` | String | For people. Code ignores it |
| `mode` | String | `DISABLED`, `DRY_RUN` or `ENFORCE`. Any other value means don't act |
| `resource_type` | String | `AWS::EC2::Image`, `AWS::EC2::Volume`, `AWS::EC2::Snapshot` or `AWS::Lambda::Function` |
| `scope` | Map | Where the policy runs |
| `conditions` | Map | What a resource must meet. All of them must match |
| `exclusions` | Map | What skips a resource. Any match skips it |
| `action` | Map | What happens to a matching resource |

Every field is required in a policy file except `exclusions`. `policy_hash` is the one attribute a file must not contain, since Terraform computes it.

The tables below describe the logical shape. In a file each value carries its DynamoDB type code, so `older_than_days` is written `{ "N": "30" }` and a list of regions is `{ "L": [{ "S": "us-east-1" }] }`. Numbers go in quotes, and account IDs are `S` even though they look numeric.

### `scope`

| Field | Meaning |
|---|---|
| `regions` | Regions the policy runs in |
| `accounts` | `["ALL"]` or a list of account IDs. An empty list is rejected, not read as "all" |
| `excluded_accounts` | Accounts the policy never runs in |

### `conditions`

| Field | Meaning |
|---|---|
| `older_than_days` | Required. The resource must be strictly older than this many days, in UTC. Age counts from the resource's creation time (AMI `CreationDate`, volume `CreateTime`, snapshot `StartTime`) or from the tag in `age_from_tag` |
| `age_from_tag` | Lambda only, since functions have no creation time. A function without the tag doesn't match |
| `state` | Volumes only. `available` means unattached |
| `tags` | Tags the resource must have, with these values |

### `exclusions`

| Field | Meaning |
|---|---|
| `tag_keys` | Skip if any of these tags exists |
| `tags` | Skip if any of these tags has this value |
| `name_prefixes` | Skip if the name starts with one of these. The name is the AMI or function name, or the `Name` tag on volumes and snapshots |
| `resources` | Skip these resources, listed by ID or ARN |

### `action`

| `type` | Parameters | Resource types |
|---|---|---|
| `ADD_TAGS` | `tags` (map) | All |
| `REMOVE_TAGS` | `tag_keys` (list) | All |
| `DELETE` | None | Volumes, snapshots and Lambda functions |
| `DEREGISTER` | None | AMIs |

### How policies are evaluated

- A resource matches when it meets every condition and no exclusion.
- Tag keys and values are case-sensitive strings, so `"True"` doesn't match `"true"`.
- If Discovery can't fetch data a policy needs, it skips the resource and reports it. Missing data never counts as "no tag".
- Actions that are already done are skipped, like adding a tag that's already set or removing one that's already gone. A tag with a different value is overwritten.
- Every policy is evaluated against the data collected at the start of the run, so a resource is never tagged and deleted in the same run.
- AMI policies only see AMIs the account owns. Amazon, Marketplace and shared AMIs never appear.

## Sample policy: Lambda

`policies/lambda-delete.json`, exactly as it exists in the repo:

```json
{
  "rule_id":        { "S": "lambda-delete" },
  "schema_version": { "N": "1" },
  "description":    { "S": "Deletes Lambda functions whose created_timestamp tag is older than 30 days" },
  "mode":           { "S": "DRY_RUN" },
  "resource_type":  { "S": "AWS::Lambda::Function" },

  "scope": {
    "M": {
      "regions": {
        "L": [
          { "S": "us-east-1" },
          { "S": "us-west-1" },
          { "S": "us-west-2" }
        ]
      },
      "accounts": {
        "L": [
          { "S": "ALL" }
        ]
      },
      "excluded_accounts": {
        "L": []
      }
    }
  },

  "conditions": {
    "M": {
      "older_than_days": { "N": "30" },
      "age_from_tag":    { "S": "created_timestamp" }
    }
  },

  "exclusions": {
    "M": {
      "name_prefixes": {
        "L": [
          { "S": "pcs-" },
          { "S": "el-" },
          { "S": "es-" }
        ]
      },
      "resources": {
        "L": [
          { "S": "arn:aws:lambda:us-east-1:930207301447:function:security-gate-tagger-lambda-use1" },
          { "S": "arn:aws:lambda:us-east-1:930207301447:function:security-gate-error-handler-lambda-use1" }
        ]
      }
    }
  },

  "action": {
    "M": {
      "type": { "S": "DELETE" }
    }
  }
}
```

This policy deletes Lambda functions whose `created_timestamp` tag is more than 30 days old. It skips functions whose names start with `pcs-`, `el-` or `es-`, and the two security-gate functions. `excluded_accounts` is empty until the account list is confirmed; the policy is in `DRY_RUN`, so nothing is deleted meanwhile.

- The enforced Lambda module adds `created_timestamp` to every function. Functions without it were created outside the module, for example by AWS services, and never match.
- Discovery parses the tag in the format the module writes. A value in any other format is skipped and reported.
- Discovery lists functions with the Resource Groups Tagging API, filtered on `created_timestamp`, so every function that can match comes back with its tags.

## Initial policies

Every policy starts in `DRY_RUN`. Terraform is the source of truth, and this table is a summary.

| `rule_id` | Resource type | Conditions | Exclusions | Action |
|---|---|---|---|---|
| `ami-remove-approval-tag` | AMI | Older than 30 days | Tags `forensic-evidence`, `WaiveredAMI`, `cotsAppID`. Names starting with `pcs-`, `el-`, `es-`. Excluded AMI IDs | Remove tag `InfoSecApproved` |
| `ami-deregister` | AMI | Older than 60 days | Tags `forensic-evidence`, `InfoSecApproved`, `WaiveredAMI`. Excluded AMI IDs | Deregister |
| `ami-dev-ttl` | AMI | Older than 30 days | Same as `ami-remove-approval-tag` | TBD |
| `ebs-volume-tag` | EBS volume | Older than 30 days, `state` is `available` | None | Add tag `SSI_Nuke=True` |
| `ebs-volume-delete` | EBS volume | Older than 37 days, `state` is `available`, tag `SSI_Nuke=True` | None | Delete |
| `ebs-snapshot-tag` | EBS snapshot | Older than 30 days | Tag `builder=packer-build`. Tag `aws:backup:source-resource` | Add tag `SSI_Nuke=Yes` |
| `ebs-snapshot-delete` | EBS snapshot | Older than 37 days, tag `SSI_Nuke=Yes` | Same as `ebs-snapshot-tag` | Delete |
| `lambda-delete` | Lambda function | `created_timestamp` tag older than 30 days | Names starting with `pcs-`, `el-`, `es-`. The two security-gate function ARNs | Delete |

Both volume policies also have account 172607267291 in `scope.excluded_accounts`.

Behavior to know about:

- **Tag and delete policies work in pairs.** Each delete policy requires the tag its tag policy adds: `SSI_Nuke=True` on volumes and `SSI_Nuke=Yes` on snapshots. Existing resources already carry these values, so changing a value means those resources are never deleted.
- **The 7-day gap isn't guaranteed.** Age counts from creation, so a resource first seen when it's already past 37 days is tagged on one run and deleted on the next.
- **Cleanup tags aren't removed.** A tagged volume that gets attached keeps `SSI_Nuke`. If it's detached later, it's deleted on the next run.
- **AMI approval spans two policies.** `ami-remove-approval-tag` removes `InfoSecApproved` after 30 days, and `ami-deregister` skips AMIs that still have it. AMIs with `pcs-`, `el-` or `es-` names or a `cotsAppID` tag keep the tag, so they're protected from deregistration only while they have it.
- **Packer-built and AWS Backup snapshots are never tagged or deleted.**

## Safety checks

The Lambdas enforce these no matter what a policy says:

- Volumes are deleted only if they're still unattached (`available`) at delete time.
- Snapshots are deleted only if no AMI in the account and region uses them. The check includes disabled AMIs. If it can't complete, no snapshots are deleted in that region for the run.
- `DELETE` and `DEREGISTER` policies need an `older_than_days` at or above a minimum set in code.
- Each policy has an action cap per account, region and run. Discovery counts matches before queueing anything. A policy over its cap queues nothing and raises an alert.
- Before every action the Worker re-reads the policy with a strongly consistent `GetItem` (`ConsistentRead=true`) and acts only if it's still in `ENFORCE` with the same `policy_hash` as the queued message. Otherwise it drops the message, and the next run finds the resource again.
- If that policy read fails, the Worker takes no action, leaves the message to retry, and alerts once the retries are exhausted. A failed or stale policy read is never treated as approval.
- Every action is logged with `rule_id`, `policy_hash`, a `run_id` and the Lambda build version. Discovery also logs each policy's full content once per run, so a hash in an action log can be matched to the policy that produced it.

## How policies are stored

```
policies/
  lambda-delete.json      one file per policy, named <rule_id>.json
ttl_policies.tf           reads the directory, validates, writes the items
```

`ttl_policies.tf` walks `policies/` with `fileset`, decodes each file, and writes one `aws_dynamodb_table_item` per policy with `for_each`. The `item` argument takes DynamoDB's attribute-value JSON, which is the format the files are already in, so the file content goes through untouched. The only thing Terraform adds is `policy_hash`, through a generic `merge` rather than a per-field mapping:

```hcl
item = jsonencode(merge(
  each.value,
  { policy_hash = { S = sha256(jsonencode(each.value)) } }
))
```

Decoding and re-encoding normalises whitespace and key order, so reformatting a file doesn't change its hash.

`for_each` is keyed on `rule_id` rather than the file name, so renaming a file doesn't destroy and recreate the item.

### Trade-offs of this layout

- **No shared lists.** Each file is self-contained, so exclusions used by several policies (the excluded AMI IDs, the `pcs-`/`el-`/`es-` prefixes) are repeated per file instead of defined once in a local. Each file reads as a complete policy, but updating a shared list touches several files. Injecting them from a local would mean a path-specific merge, which is the mapping layer this layout avoids.
- **Noisier files.** `{ "N": "30" }` reads worse than `30`. The plan-time checks below exist partly to make up for it.

## Validation

Two layers, and both have to pass before an action happens.

### Terraform, at plan time

`ttl_policies.tf` puts preconditions on a `terraform_data` resource, so a bad file fails the plan instead of reaching the table. The checks reject:

- an attribute not in `local.allowed_attributes`, which catches a typo or a renamed field
- a file not named `<rule_id>.json`
- two files with the same `rule_id`, which fails as a duplicate map key
- an unsupported `schema_version`
- a `mode` outside `DISABLED`, `DRY_RUN`, `ENFORCE`
- an `action.type` outside `ADD_TAGS`, `REMOVE_TAGS`, `DELETE`, `DEREGISTER`
- a `DELETE` or `DEREGISTER` policy whose `older_than_days` is below the floor in `local.min_destructive_age_days`
- an empty `policies/` directory

The checks read typed paths such as `p.conditions.M.older_than_days.N`, so a wrong type code misses the path, falls through to the safe default, and fails the check. Adding a field to the schema means adding its name to `local.allowed_attributes`.

### Go, when policies load

Discovery and the Worker revalidate every policy they read, skip invalid ones and alert. This layer catches what Terraform can't see:

- fields nested inside `conditions`, `exclusions` and `action` that the allow-list doesn't cover
- a field or action its `resource_type` doesn't support, like `state` on an AMI policy
- an empty `scope.accounts`
- a tag and delete pair that disagree: the delete policy must require the same tag value its tag policy adds, and use a larger age

`attributevalue.UnmarshalMap` silently ignores attributes with no matching struct field, so the loader decodes through `encoding/json` with `DisallowUnknownFields` to turn a typo into an error rather than a zero value.

### Plan review

Two rules the tooling can't enforce, so they belong in PR review:

- `resource_type` and `action.type` are immutable on an existing policy. Add a new policy instead.
- A new policy starts in `DRY_RUN`.

## Access

### Writes

Only the Terraform pipeline role writes items. This statement goes in the table's resource-based policy, next to the read grants, and denies item writes to everyone else:

```json
{
  "Sid": "OnlyPipelineWritesItems",
  "Effect": "Deny",
  "Principal": "*",
  "Action": [
    "dynamodb:PutItem",
    "dynamodb:UpdateItem",
    "dynamodb:DeleteItem",
    "dynamodb:BatchWriteItem",
    "dynamodb:PartiQLInsert",
    "dynamodb:PartiQLUpdate",
    "dynamodb:PartiQLDelete"
  ],
  "Resource": "arn:aws:dynamodb:us-east-1:<central-account-id>:table/ttl_policies",
  "Condition": {
    "ArnNotLike": {
      "aws:PrincipalArn": "arn:aws:iam::<central-account-id>:role/<terraform-pipeline-role>"
    }
  }
}
```

- Optionally, add a break-glass role to the exception so a policy can be stopped without waiting for a PR. If it changes an item, make the same change in Terraform, or the next apply reverts it.
- If replicas are added later, exempt the DynamoDB replication service-linked role (`AWSServiceRoleForDynamoDBReplication`) too. A replica that can't replicate for more than 20 hours is permanently converted to a standalone table.

### Reads

The Discovery and Worker roles in every LOB account need access in three places. All three are in us-east-1, whatever region the caller runs in:

- the table's resource-based policy: `dynamodb:Scan` for Discovery and `dynamodb:GetItem` for the Worker
- the us-east-1 KMS key policy: `kms:Decrypt` through DynamoDB, with `kms:ViaService` set to `dynamodb.us-east-1.amazonaws.com`
- the role's own IAM policy, for the same actions

The KMS permission is needed because DynamoDB uses the key on the caller's behalf when it reads the table. The key stays in the PCE account, and the LOB accounts never call KMS themselves. `kms:ViaService` names the table's region, not the caller's, so one statement covers the Lambdas in all three regions.

Use `aws:PrincipalOrgID` and a role-name pattern on `aws:PrincipalArn` instead of listing accounts.

The Lambdas build the table ARN from `TABLE_REGION` and the central account ID. Cross-account calls have to use the table ARN, not the table name.

## Changing a policy

1. Edit the file under `policies/`. Nothing else to update: `policy_hash` is recomputed on its own.
2. Open a PR. `terraform plan` runs the checks in [Validation](#validation).
3. Review the plan. Each changed item shows its old and new values.
4. Merge and apply. Discovery uses the new policy on its next run, and the Worker drops queued work carrying the old hash.

Adding a policy is the same, plus creating `policies/<rule_id>.json`. A new field in the schema also needs its name in `local.allowed_attributes` and handling in Go.

**Rolling out a policy:** start in `DRY_RUN` and compare what the policy would change with what the current TTL automation changes over the same days. Any difference should be deliberate. Then switch policies to `ENFORCE` one at a time.

**Stopping a policy:** set `mode` to `DISABLED`. Workers read the policy with a strongly consistent `GetItem` before acting, so any read that starts after the apply completes sees `DISABLED` and drops the message, including work already queued. Actions a Worker already authorized on an earlier read, or that are in flight at the AWS API, may still complete. Treat `DISABLED` as "no new actions", not an instant stop.

## Querying the table

```bash
# Read one policy
aws dynamodb get-item \
  --region us-east-1 \
  --table-name ttl_policies \
  --key '{"rule_id": {"S": "lambda-delete"}}'

# List policies that are still in DRY_RUN
aws dynamodb scan \
  --region us-east-1 \
  --table-name ttl_policies \
  --filter-expression "#m = :mode" \
  --projection-expression "rule_id, #m, policy_hash" \
  --expression-attribute-names '{"#m": "mode"}' \
  --expression-attribute-values '{":mode": {"S": "DRY_RUN"}}'
```

- The table only exists in us-east-1, so every call needs `--region us-east-1` no matter where you run it.
- `mode`, `scope`, `action`, `state` and `type` are DynamoDB reserved words, so use `#` placeholders for them in expressions.
- Items come back in DynamoDB's typed format, like `{"S": "DELETE"}` and `{"N": "30"}`.
- From another account, pass the table ARN as `--table-name`.

## Open items

- **`ami-dev-ttl`:** its action, and which accounts it covers. If that includes account 244263515617, add it to `excluded_accounts`.
- **Exclusion values:** each policy's account exclusions and the excluded AMI IDs, from `policies_aws_ops.tf`.
- **Cross-region read path:** confirm the Discovery and Worker Lambdas in us-west-1 and us-west-2 can reach `dynamodb.us-east-1.amazonaws.com`. A region-local gateway endpoint won't serve it. If there's no path and the Lambdas have to stay VPC-attached, a global table is the simpler fix. See [Single table vs global table](#single-table-vs-global-table).
- **us-west-1:** confirm it's in scope. It needs its own cleanup stack, but no extra table or KMS key.
- **`created_timestamp`:** confirm the format the Lambda module writes, and that the value stays at creation time. With Terraform's `timestamp()`, the module needs `ignore_changes` on the tag or a `time_static` resource. Otherwise every apply resets the age.
- **Module support:** confirm version 11.0.0 supports streams and a resource-based policy, and check its input names.
- **Security-gate functions:** the ARN exclusions only cover us-east-1. Confirm whether these functions also exist in other regions.
- **Limits:** the minimum age for `DELETE` and `DEREGISTER`, and each policy's action cap.
- **`datasensitivityclasscd`:** choose the value for the module.
- **Encryption key:** confirm a customer-managed key is required for this table. The data classification may decide it. If it isn't required, the default AWS owned key also works across accounts and needs no key policy or KMS grants at all. AWS managed keys (`aws/dynamodb`) don't work across accounts.
- **Grace period:** decide whether to guarantee 7 days between tagging and deletion. That needs a tag recording when the cleanup tag was added.
- **Remaining seven policies:** only `lambda-delete` exists so far. The other seven in [Initial policies](#initial-policies) still need their files.
- **Shared exclusion lists:** the AMI policies repeat the same protections and excluded AMI IDs. Decide whether repeating them per file is acceptable or whether they should be injected from a local. See [Trade-offs of this layout](#trade-offs-of-this-layout).
