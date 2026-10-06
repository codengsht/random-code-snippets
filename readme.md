# TTL policies DynamoDB table

Terraform for the `ttl_policies` DynamoDB table and the TTL cleanup policies stored in it. The policies decide which AWS resources get tagged, untagged, deregistered or deleted, and when.

## Purpose

The table is the single, central source of truth for TTL cleanup policies.

- Each policy is one item, managed from the central IS PCE account.
- The cleanup Lambdas in every LOB account read their policies from this table. They have no code for individual policies, so changing a threshold, tag, exclusion, scope or mode is a table change, not a Lambda deployment.
- Policies cover AMIs, EBS volumes, EBS snapshots and Lambda functions in us-east-1, us-west-1 and us-west-2.
- It replaces the TTL policies that ran in Prisma Cloud.

How the table is used:

1. An EventBridge schedule in each LOB account and region starts the Discovery Lambda.
2. Discovery scans the table's replica in its own region, validates each policy, and evaluates the ones that apply to its account and region.
3. Matches go to an SQS work queue, with a DLQ for failed messages.
4. The Worker Lambda re-reads the policy with `GetItem`, rechecks the resource, and then acts.

## Key decisions

- **One item per policy, keyed by `rule_id`.** There's one record per policy and no per-account overrides, so `rule_id` alone identifies an item. No sort key and no generic `PK`/`SK`.
- **The item holds the whole policy.** Resource type, conditions, exclusions, scope, mode and action all live in the table. The Lambdas only know a fixed set of fields per resource type. A new policy built from existing fields needs only a table change. A new kind of check needs a code change.
- **Named fields, not a query language.** Checks are named fields like `older_than_days` and `name_prefixes`, not field/operator/value expressions. Every policy has the same shape and leaves out the fields it doesn't use.
- **Same results, not the same conditions.** Policies have to produce the same outcomes as the TTL automation they replace. Checks that this design makes unnecessary are left out.
- **Native DynamoDB types.** Nested data is stored as maps and lists, never as a JSON string in one attribute, so every field stays readable and filterable.
- **Terraform is the only writer.** A policy can delete resources in every account, so every change goes through PR review. Each replica's resource-based policy blocks item writes from anyone else.
- **Safety checks live in code.** Table edits can't turn them off. See [Safety checks](#safety-checks).
- **Global table with regional KMS keys.** Each region reads its own replica, so cleanup in one region doesn't depend on another. The table uses customer-managed keys, since AWS managed keys don't support cross-account access. KMS keys are regional, so each replica has its own key.
- **Lambda age comes from the `created_timestamp` tag.** The Lambda API has no creation time. The enforced Lambda module adds this tag to every function, and functions created outside the module never match.

## Table schema

| Setting | Value |
|---|---|
| Table name | `ttl_policies` |
| Partition key | `rule_id` (String) |
| Sort key | None |
| Capacity mode | On-demand |
| Regions | us-east-1, where Terraform writes, with replicas in us-west-1 and us-west-2 |
| Encryption | Customer-managed KMS key in each region |
| Streams | Enabled with `NEW_AND_OLD_IMAGES`, which replicas require |
| Resource-based policy | One on each replica. See [Access](#access) |
| Deletion protection | Recommended |

- Only key attributes go in `attributes`. DynamoDB rejects definitions for attributes that no key uses. Every other field is free-form, so the validator enforces the item format.
- The partition key can't change after the table is created. Global secondary indexes can be added later, but none are needed: Discovery reads every policy in one scan.
- Replication is eventually consistent, which is fine for a scheduled job.

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

  ci_job_id              = var.ci_job_id
  datasensitivityclasscd = "" # see https://go/SDMMatrix
}
```

Confirm the module supports the replica, stream and resource policy settings above. See [Open items](#open-items).

## Item format

| Field | Type | Meaning |
|---|---|---|
| `rule_id` | String | Partition key. A stable name like `lambda-delete`. Keep thresholds out of it, since they can change |
| `schema_version` | Number | Version of the item format. Discovery skips items with a version it doesn't support |
| `policy_version` | Number | Bumped on every change and logged with every action |
| `description` | String | For people. Code ignores it |
| `mode` | String | `DISABLED`, `DRY_RUN` or `ENFORCE`. Any other value means don't act |
| `resource_type` | String | `AWS::EC2::Image`, `AWS::EC2::Volume`, `AWS::EC2::Snapshot` or `AWS::Lambda::Function` |
| `scope` | Map | Where the policy runs |
| `conditions` | Map | What a resource must meet. All of them must match |
| `exclusions` | Map | What skips a resource. Any match skips it |
| `action` | Map | What happens to a matching resource |

Every field is required except `exclusions`.

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

```json
{
  "rule_id": "lambda-delete",
  "schema_version": 1,
  "policy_version": 1,
  "description": "Deletes Lambda functions whose created_timestamp tag is older than 30 days",
  "mode": "DRY_RUN",
  "resource_type": "AWS::Lambda::Function",
  "scope": {
    "regions": ["us-east-1", "us-west-1", "us-west-2"],
    "accounts": ["ALL"],
    "excluded_accounts": ["<account IDs to skip>"]
  },
  "conditions": {
    "older_than_days": 30,
    "age_from_tag": "created_timestamp"
  },
  "exclusions": {
    "name_prefixes": ["pcs-", "el-", "es-"],
    "resources": [
      "arn:aws:lambda:us-east-1:930207301447:function:security-gate-tagger-lambda-use1",
      "arn:aws:lambda:us-east-1:930207301447:function:security-gate-error-handler-lambda-use1"
    ]
  },
  "action": { "type": "DELETE" }
}
```

This policy deletes Lambda functions whose `created_timestamp` tag is more than 30 days old. It skips functions whose names start with `pcs-`, `el-` or `es-`, and the two security-gate functions.

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
- The Worker acts only if the policy is still in `ENFORCE` with the same `policy_version` as the queued message. Otherwise it drops the message, and the next run finds the resource again.
- Every action is logged with `rule_id`, `policy_version`, a `run_id` and the Lambda build version.

## Validation

The same Go validator runs in CI against the Terraform plan, and in Discovery when it loads policies. Discovery skips an invalid policy and raises an alert. A policy is invalid if:

- it has an unknown field, a missing required field, or a field with the wrong type
- its `schema_version` isn't supported
- it uses a field or action its resource type doesn't support, like `state` on an AMI policy
- it's a `DELETE` or `DEREGISTER` policy with `older_than_days` below the minimum
- its `scope.accounts` is empty

CI also rejects a change that:

- changes `resource_type` or `action.type` on an existing policy. Add a new policy instead.
- adds a policy that doesn't start in `DRY_RUN`
- changes a policy without bumping `policy_version`
- breaks a tag and delete pair. The delete policy must require the same tag value its tag policy adds, and use a larger age.

## Access

### Writes

Only the Terraform pipeline role writes items. This statement goes in each replica's resource-based policy, next to the read grants, and denies item writes to everyone else:

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
  "Resource": "arn:aws:dynamodb:<region>:<central-account-id>:table/ttl_policies",
  "Condition": {
    "ArnNotLike": {
      "aws:PrincipalArn": [
        "arn:aws:iam::<central-account-id>:role/<terraform-pipeline-role>",
        "arn:aws:iam::<central-account-id>:role/aws-service-role/replication.dynamodb.amazonaws.com/AWSServiceRoleForDynamoDBReplication"
      ]
    }
  }
}
```

- Keep the DynamoDB replication service-linked role in the exception. If a replica can't replicate for more than 20 hours, it's permanently converted to a standalone table. The same happens if DynamoDB loses access to the replica's KMS key for more than 20 hours.
- Optionally, add a break-glass role to the exception so a policy can be stopped without waiting for a PR. If it changes an item, make the same change in Terraform, or the next apply reverts it.

### Reads

The Discovery and Worker roles in every LOB account need access in each region, in three places:

- the replica's resource-based policy: `dynamodb:Scan` for Discovery and `dynamodb:GetItem` for the Worker
- the KMS key policy in that region: `kms:Decrypt` through DynamoDB (`kms:ViaService` set to `dynamodb.<region>.amazonaws.com`)
- the role's own IAM policy, for the same actions

Use `aws:PrincipalOrgID` and a role-name pattern on `aws:PrincipalArn` instead of listing accounts. Multi-Region KMS keys share key material but not key policies, so every region's key policy needs these grants.

The Lambdas build the table ARN from `AWS_REGION` and the central account ID, so each region reads its own replica. Cross-account calls have to use the table ARN, not the table name.

## Changing a policy

Policies are defined in Terraform as a map with one entry per policy, and written with `aws_dynamodb_table_item` using `for_each`. Terraform converts each entry into DynamoDB's typed format. Lists that several policies share, like the excluded AMI IDs and name prefixes, are defined once as locals.

1. Edit the policy and bump its `policy_version`.
2. Open a PR. CI runs the validator against the plan.
3. Review the plan. Each changed item shows its old and new values.
4. Merge and apply. Discovery uses the new version on its next run, and the Worker drops queued work from the old version.

**Rolling out a policy:** start in `DRY_RUN` and compare what the policy would change with what the current TTL automation changes over the same days. Any difference should be deliberate. Then switch policies to `ENFORCE` one at a time.

**Stopping a policy:** set `mode` to `DISABLED`. The Worker checks the mode before every action, so this also stops work already in the queue.

## Querying the table

```bash
# Read one policy
aws dynamodb get-item \
  --table-name ttl_policies \
  --key '{"rule_id": {"S": "lambda-delete"}}'

# List policies that are still in DRY_RUN
aws dynamodb scan \
  --table-name ttl_policies \
  --filter-expression "#m = :mode" \
  --projection-expression "rule_id, #m, policy_version" \
  --expression-attribute-names '{"#m": "mode"}' \
  --expression-attribute-values '{":mode": {"S": "DRY_RUN"}}'
```

- `mode`, `scope`, `action`, `state` and `type` are DynamoDB reserved words, so use `#` placeholders for them in expressions.
- Items come back in DynamoDB's typed format, like `{"S": "DELETE"}` and `{"N": "30"}`.
- From another account, pass the table ARN as `--table-name`.

## Open items

- **`ami-dev-ttl`:** its action, and which accounts it covers. If that includes account 244263515617, add it to `excluded_accounts`.
- **Exclusion values:** each policy's account exclusions and the excluded AMI IDs, from `policies_aws_ops.tf`.
- **us-west-1:** confirm it's in scope. It needs its own replica, KMS key and cleanup stack.
- **`created_timestamp`:** confirm the format the Lambda module writes, and that the value stays at creation time. With Terraform's `timestamp()`, the module needs `ignore_changes` on the tag or a `time_static` resource. Otherwise every apply resets the age.
- **Module support:** confirm version 11.0.0 supports streams, replicas with their own KMS keys, and a resource-based policy on each replica. If it doesn't, an `aws_dynamodb_table_replica` outside the module conflicts with it unless the module ignores changes to `replica`.
- **Security-gate functions:** the ARN exclusions only cover us-east-1. Confirm whether these functions also exist in other regions.
- **Limits:** the minimum age for `DELETE` and `DEREGISTER`, and each policy's action cap.
- **`datasensitivityclasscd`:** choose the value for the module.
- **Grace period:** decide whether to guarantee 7 days between tagging and deletion. That needs a tag recording when the cleanup tag was added.
