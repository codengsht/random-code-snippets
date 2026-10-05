With handlers keyed by `rule_id`, every new policy needs new Lambda code, and the table only holds settings. So you're right: the policies wouldn't really be managed centrally. That changes my recommendation. Organize the code by resource type instead of by policy. The Lambda becomes a generic engine with a fixed vocabulary, and each item in the table is a complete policy:

| In the table (the policy) | In code (the engine) |
|---|---|
| The resource type | How to list each resource type and turn each resource into a record |
| Conditions and exclusions | The fields each type exposes, and the operators |
| The action and its parameters | How to run each action |
| Scope, mode and versions | Guardrails no policy can turn off |

Discovery loads and validates the policies, lists each resource type once, checks every policy against those records, and queues the matches. None of that code refers to a specific rule. Most of the code goes into four per-resource-type modules ("adapters") that make the AWS calls, which you'd need in any design. The part that evaluates policies is small.

Your ticket's example was already heading this way with its `field`/`operator` condition. The difference here is that fields come from a fixed list the code defines, not raw API paths like `configuration.createTime`.

Here's the Lambda RQL as a policy item, with nothing about this rule in the code:

```json
{
  "rule_id": "lambda-delete",
  "schema_version": 1,
  "policy_version": 1,
  "description": "Deletes Lambda functions whose created_timestamp tag is older than 30 days",
  "prisma_policy": "lambda-30-days-or-older",
  "mode": "DRY_RUN",
  "resource_type": "AWS::Lambda::Function",
  "scope": {
    "regions": ["us-east-1", "us-west-2"],
    "excluded_accounts": []
  },
  "conditions": [
    { "field": "tag:created_timestamp", "operator": "OLDER_THAN_DAYS", "value": 30 }
  ],
  "exclusions": [
    { "field": "name", "operator": "STARTS_WITH", "value": ["pcs-", "el-", "es-"] },
    {
      "field": "arn",
      "operator": "IN",
      "value": [
        "arn:aws:lambda:us-east-1:930207301447:function:security-gate-tagger-lambda-use1",
        "arn:aws:lambda:us-east-1:930207301447:function:security-gate-error-handler-lambda-use1"
      ]
    }
  ],
  "action": { "type": "DELETE" }
}
```

The fields each resource type exposes:

| `resource_type` | Fields |
|---|---|
| Every type | `arn`, `name`, `tag:<key>` |
| `AWS::EC2::Image` | `created_at` (from `CreationDate`) |
| `AWS::EC2::Volume` | `created_at` (from `CreateTime`), `state` |
| `AWS::EC2::Snapshot` | `created_at` (from `StartTime`), `used_by_ami`, `is_backup` |
| `AWS::Lambda::Function` | `last_modified` (Lambda has no creation time) |

- **Operators:** `OLDER_THAN_DAYS`, `EQUALS`, `NOT_EQUALS`, `EXISTS`, `NOT_EXISTS`, `STARTS_WITH`, `IN` and `NOT_IN`. `STARTS_WITH` and `IN` take a list.
- **Matching:** every condition must match, and any exclusion skips the resource. There's no nesting or OR. `IN` covers OR within one field; anything else becomes two rules.
- **Age:** `OLDER_THAN_DAYS` means strictly older, and it also works on tags that hold a timestamp. A missing or unparseable value never matches.
- **Actions:** `ADD_TAGS` and `REMOVE_TAGS` on every type, `DELETE` on volumes, snapshots and functions, and `DEREGISTER` on AMIs.
- **`name`** is the function name, the AMI name, or the `Name` tag on volumes and snapshots.
- **`is_backup`** gets its definition from the snapshot RQL.

All seven rules fit without any rule-specific code:

| `rule_id` | `resource_type` | `conditions` | `action` |
|---|---|---|---|
| `ami-remove-approval-tag` | `AWS::EC2::Image` | `created_at` older than 30, `tag:InfoSecApproved` exists | `REMOVE_TAGS` InfoSecApproved |
| `ami-deregister` | `AWS::EC2::Image` | `created_at` older than 60 | `DEREGISTER` |
| `ebs-volume-tag` | `AWS::EC2::Volume` | `state` = available, `created_at` older than 30, `tag:SSI_Nuke` doesn't exist | `ADD_TAGS` SSI_Nuke=True |
| `ebs-volume-delete` | `AWS::EC2::Volume` | `state` = available, `created_at` older than 37, `tag:SSI_Nuke` = True | `DELETE` |
| `ebs-snapshot-tag` | `AWS::EC2::Snapshot` | `used_by_ami` = false, `is_backup` = false, `created_at` older than 30, `tag:SSI_Nuke` doesn't exist | `ADD_TAGS` SSI_Nuke=Yes |
| `ebs-snapshot-delete` | `AWS::EC2::Snapshot` | `used_by_ami` = false, `is_backup` = false, `created_at` older than 37, `tag:SSI_Nuke` = Yes | `DELETE` |
| `lambda-delete` | `AWS::Lambda::Function` | `tag:created_timestamp` older than 30 | `DELETE` |

In JSON, a tag action looks like `{ "type": "ADD_TAGS", "tags": [{ "key": "SSI_Nuke", "value": "True" }] }`. Only the Lambda rule has exclusions so far, since the other six still need their RQL.

Guardrails stay in code. Discovery and the Worker both enforce them, whatever the item says:

- Deleting a volume requires `state = available` at delete time.
- Deleting a snapshot requires that it isn't used by an AMI and isn't a backup.
- Destructive actions need an age condition at or above a minimum set in code.
- Destructive actions are capped per account per run. Past the cap, the rule stops and alerts, so one bad edit can't empty an account.

Policies can still repeat a guardrail, like `state = available` in the volume rules, so each item reads as the full rule. Earlier I suggested removing the `required_state` field; this is the safe way to keep it. The item can state it, the code enforces it either way, and the validator rejects a policy that contradicts it.

Because the table can now define deletions, the contract needs enforcing:

- **One Go validation package, used in CI and when loading.** It rejects:
  - unknown fields, or fields the resource type doesn't have
  - operators that don't fit the field, like `OLDER_THAN_DAYS` on `state`
  - unsupported actions
  - policies that contradict a guardrail
- **No in-place action changes.** In my last schema, a table edit couldn't turn a tag rule into a delete rule. Now it could.
  - Have CI reject changes to `action` or `resource_type` on an existing rule. The plan JSON shows each item before and after.
  - CI can also require new rules to start in `DRY_RUN`, so turning tagging into deletion means adding a new rule that starts in dry-run.
- **Cross-rule checks.** A delete rule that requires a tag another rule adds must have the larger age.
- **Policy tests as data.** Give each policy sample records with expected results and run them in CI. Then compare `DRY_RUN` output with Prisma's before switching to `ENFORCE`.

Adding or changing a policy for a supported resource type is only a table change. A new resource type, field, operator or action needs code, and once it's added every policy can use it. Prisma worked the same way, since RQL could only query what its collectors had gathered.

[Cloud Custodian](https://cloudcustodian.io/docs/usecases/ebsgarbagecollect.html), an open-source policy engine for cloud resources, uses this same model of resources, filters and actions, and it covers these four resource types. Its `mark-for-op` action tags a resource for deletion on a future date, which gives you a real grace period between tagging and deleting. Your ticket requires the DynamoDB table, but Custodian's vocabulary is a good reference before you settle yours.

Want me to write the Go interfaces for the engine (resource adapter, evaluator, validator), or all seven items in this shape?
