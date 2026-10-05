The feedback is good, and I'd adopt almost all of it. Points 1 to 4 change the contract, so settle them before you freeze it. Point 5 is a real gap, but you can close it by enforcing the cap in Discovery, with no shared counter. Your RQL screenshots also show a few things neither review covered, including an eighth policy.

My take on each point:

- **1. Missing data vs. failed retrieval: agree, and this one matters most.** Each condition and exclusion should evaluate to true, false or unknown. A false condition or a true exclusion means no match. Otherwise, any unknown defers the resource and reports why, even in `ENFORCE`.
  - You can also drop the negative operators. Every negative check in these RQLs can be written as an exclusion with a positive operator. For example, `tags[*].key does not contain "WaiveredAMI"` becomes an exclusion on `tag:WaiveredAMI` with `EXISTS`.
  - That leaves v1 with `OLDER_THAN_DAYS`, `EQUALS`, `EXISTS`, `STARTS_WITH` and `IN`. A missing value is false for all of them.
  - Most unknowns will come from Lambda. EC2's Describe calls return tags with each resource, but Lambda tags need a separate call.
  - If listing the resources stops partway, it's still safe to evaluate what came back and report the run as incomplete. If a separate lookup stops partway (like the AMI check below), its fact becomes unknown for every resource.

- **2. No hidden decisions in derived fields: agree.** The RQL defines both terms. A backup is a snapshot with an `aws:backup:source-resource` tag, and an AMI snapshot is one tagged `builder=packer-build`. Both become exclusions in the policy, and `is_backup` goes away.
  - Keep `used_by_ami`, but as a safety check in code for snapshot deletes, not a policy field. [EC2 only refuses to delete an AMI's root snapshot](https://docs.aws.amazon.com/ebs/latest/userguide/ebs-deleting-snapshot.html). So the current rules can delete a non-Packer AMI's other snapshots, which breaks the AMI.
  - For a complete list, call DescribeImages with `IncludeDisabled=true`. [Disabled AMIs are hidden by default](https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_DescribeImages.html) but still use their snapshots. If that call fails, skip every snapshot delete in that region for the run.
  - This check blocks deletes Prisma would try, so report those as blocked. They'll show up when you compare dry-run results with Prisma.
  - The Packer exclusion also means Packer snapshots are never cleaned up, even after `ami-deregister` removes their AMI. Using `used_by_ami` instead would fix that, but it changes behavior, so leave it until after the migration matches Prisma.

- **3. Matching Prisma: agree.** Make it a rule to translate each RQL check one-to-one. The Worker skips actions that would change nothing, and behavior changes come later as separate, reviewed changes. A `DRY_RUN` match list then lines up with Prisma's alerts, so checking the migration becomes a diff.
  - I made one more mistake of this kind: the 30-day AMI RQL never checks that `InfoSecApproved` exists. Drop that condition along with the `NOT_EXISTS` on `SSI_Nuke`.
  - If a volume already has `SSI_Nuke` with a different value, EC2's CreateTags overwrites it. So if Prisma's remediation is a plain `create-tags`, matching Prisma means overwriting `SSI_Nuke=False`.

- **4. Approved age sources: agree.** Code defines which age field each destructive action may use:
  - `created_at` for AMI deregistration and for volume and snapshot deletion
  - `tag:created_timestamp` for Lambda deletion
  
  The validator rejects any other field, or an age below that action's minimum. Compare in UTC. A malformed value is unknown, and a future one counts as not older and gets reported. I couldn't find whether Prisma's `_DateTime.ageInDays` counts whole or partial days, so check resources near the threshold in the dry-run comparison.

- **5. Action cap: the gap is real, but enforce it in Discovery.** Discovery already sees every match for one account and region in a run. Have it count each rule's matches before sending anything to the queue. If a rule is over the cap, it sends none of them and raises an alert.
  - Retries and duplicate SQS messages only repeat the same resources. The actions are also idempotent: tagging again changes nothing, and deleting something already gone returns a not-found error that the Worker treats as done.
  - That caps distinct resources per rule, account, region and run, without a shared counter. Setting Discovery's reserved concurrency to 1 stops two runs from overlapping.
  - You'd only need the reservation design for a cap that spans regions or runs. Keep `run_id` for logging either way.

- **6. How rules interact: agree.** Evaluate every rule against the data collected at the start of the run, as Prisma did, so no resource is tagged and deleted in the same run. Record every rule that matched each resource.
  - The validator can find related rules on its own: any rule whose action adds or removes a tag that another rule checks.
  - The two AMI rules depend on each other differently. `ami-remove-approval-tag` removes `InfoSecApproved`, and `ami-deregister` skips AMIs that still have it. But `ami-deregister` lacks the name-prefix and `cotsAppID` exclusions, so those AMIs are protected only while they keep `InfoSecApproved`. One that never had it is deregistered after 60 days. Keep that to match Prisma, and document it.

- **7. Controlling who writes: agree.** I'd allow tag value changes, but only together with the paired rule in the same change. Changing a value also leaves behind resources already tagged with the old one. Have the Worker act only if the rule still has the message's `policy_version` and is still in `ENFORCE`. A policy edit then never applies to work already in the queue.

I'd take the rest as written, including us-west-1 if it's in scope. That region needs its own replica, CMK and Discovery stack. On the Cloud Custodian lifecycle: Prisma has no step that removes the cleanup tag either. So a tagged volume that gets attached and later detached is deleted on the next run. The schema can already express a rule that removes the tag from attached volumes, but that's another change for after the migration.

The RQL adds a few things the schema needs:

- **An eighth policy, `ami-30-days-or-older-dev-ttl`.** It's the 30-day AMI rule plus `ownerId` not equal to 244263515617. Its action isn't shown, and it isn't one of the ticket's seven global policies. It's probably one of the account-specific policies listed below the ticket's table, so `scope` needs a list of accounts to include, not just exclude.
- **AMI owner fields, `owner_id` and `owner_alias`.**
  - `imageOwnerAlias` [can only be `amazon`, `aws-backup-vault` or `aws-marketplace`](https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_Image.html). So both owner checks only matter if the AMI list includes AMIs owned by other accounts.
  - Match whatever AMIs Prisma collected. If the list has only the account's own AMIs, the 244263515617 check just excludes that account.
- **An `id` field,** since `${excluded_amis}` is a list of AMI IDs.
- **The ESG account is 172607267291.** Both volume rules skip it using `volumeArn does not contain`. Put it in `scope.excluded_accounts` instead; that's the one check I'd translate by meaning rather than word for word.
- **Account exclusions from the Prisma alert rules.** "AWS group minus exclusions" and the `excluded_amis` values are in `policies_aws_ops.tf`, not in the RQL.

Schema changes:

- `scope.accounts` is `["ALL"]` or a list of account IDs. An empty list is rejected, not treated as "all".
- The field list adds `id`, `owner_id` and `owner_alias`. It drops `is_backup` and `last_modified`, and `used_by_ami` moves into code. `id` is the AMI, volume or snapshot ID, or the function name.
- Tag values are strings compared exactly, so `"true"` doesn't match `"True"`. Only the first `tag:` is a prefix, so `tag:aws:backup:source-resource` reads the key `aws:backup:source-resource`.

Two sets of exclusions repeat across rules. Define each once as a Terraform local and copy it in full into each item, so every item is still a complete policy:

- **AMI protections:** `tag:forensic-evidence` exists, `tag:WaiveredAMI` exists, `owner_alias` exists, `id` in `excluded_amis`
- **Snapshot protections:** `tag:builder` = packer-build, `tag:aws:backup:source-resource` exists

| `rule_id` | Conditions | Exclusions | Action |
|---|---|---|---|
| `ami-remove-approval-tag` | `created_at` older than 30 | AMI protections, `tag:cotsAppID` exists, `name` starts with pcs-, el- or es- | `REMOVE_TAGS` InfoSecApproved |
| `ami-deregister` | `created_at` older than 60 | AMI protections, `tag:InfoSecApproved` exists | `DEREGISTER` |
| `ami-dev-ttl` | `created_at` older than 30 | Same as `ami-remove-approval-tag`, plus `owner_id` = 244263515617 | Not in the screenshot |
| `ebs-volume-tag` | `state` = available, `created_at` older than 30 | None | `ADD_TAGS` SSI_Nuke=True |
| `ebs-volume-delete` | `state` = available, `created_at` older than 37, `tag:SSI_Nuke` = True | None | `DELETE` |
| `ebs-snapshot-tag` | `created_at` older than 30 | Snapshot protections | `ADD_TAGS` SSI_Nuke=Yes |
| `ebs-snapshot-delete` | `created_at` older than 37, `tag:SSI_Nuke` = Yes | Snapshot protections | `DELETE` |
| `lambda-delete` | `tag:created_timestamp` exists and is older than 30 | `name` starts with pcs-, el- or es-, `arn` in the two security-gate ARNs | `DELETE` |

Both volume rules also have 172607267291 in `scope.excluded_accounts`.

Here's `ami-deregister` as a complete item:

```json
{
  "rule_id": "ami-deregister",
  "schema_version": 1,
  "policy_version": 1,
  "description": "Deregisters AMIs older than 60 days unless approved, waived, forensic evidence or excluded",
  "prisma_policy": "ami-60-days-or-older",
  "mode": "DRY_RUN",
  "resource_type": "AWS::EC2::Image",
  "scope": {
    "regions": ["us-east-1", "us-west-1", "us-west-2"],
    "accounts": ["ALL"],
    "excluded_accounts": ["<from the alert rule in policies_aws_ops.tf>"]
  },
  "conditions": [
    { "field": "created_at", "operator": "OLDER_THAN_DAYS", "value": 60 }
  ],
  "exclusions": [
    { "field": "tag:forensic-evidence", "operator": "EXISTS" },
    { "field": "tag:WaiveredAMI", "operator": "EXISTS" },
    { "field": "owner_alias", "operator": "EXISTS" },
    { "field": "id", "operator": "IN", "value": ["<excluded_amis from policies_aws_ops.tf>"] },
    { "field": "tag:InfoSecApproved", "operator": "EXISTS" }
  ],
  "action": { "type": "DEREGISTER" }
}
```



------------------------------------------------------------------------------------------------------------



The tag is already part of the policy. `lambda-delete` reads each function's age from `created_timestamp` through `age_from_tag`. Last time I only dropped Prisma's separate check that the tag exists, because reading the age from the tag already requires it. The tag itself comes from your module, so nothing about it is Prisma-specific.

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
    "excluded_accounts": ["<from policies_aws_ops.tf>"]
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

Since the module always adds the tag, a few things get simpler:

- **Nothing else has to keep tagging.** That answers my earlier question about whether the security-gate tagger has to keep running after Prisma is retired. It doesn't, at least not for this rule.
- **The format is known.** Discovery only has to parse the format the module writes, plus any format older module versions used. Terraform's `timestamp()`, for example, produces RFC 3339 in UTC (`2026-09-28T14:03:22Z`). A value in any other format was set outside the module, so Discovery skips that function and reports it.
- **Listing functions is easier.** Discovery can use the Resource Groups Tagging API, filtered on `created_timestamp`. One paginated listing returns every function that can match, with its tags. There's no separate tag lookup per function, so no lookup can fail partway.

Functions without the tag still never match. With modules enforced, those are functions created outside the module, like ones that AWS services or account baseline tooling create. Leaving them alone is the right default, and it's what Prisma does today.

One thing to check is how the module sets the value. If it uses `timestamp()`, it also needs `ignore_changes` on that tag, or a `time_static` resource instead. Otherwise every apply rewrites the tag, and the age counts from the last deploy, not from creation. Prisma reads the same tag, so the behavior carries over either way. It only changes what "older than 30 days" actually measures.
