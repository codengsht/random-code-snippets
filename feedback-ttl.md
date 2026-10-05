**This design meets your central-management goal.** A new policy using existing fields, operators, and actions can be added without changing Lambda code. I would keep the overall schema and the resource-based adapters.

`resource_type` now has a clear purpose: selecting the adapter and validating the available fields and actions. `rule_id` remains an identifier.

Before freezing the contract, I would resolve the following points.

1. **Define missing data and failed retrieval separately.**

   This is the most important evaluator detail.

   | Resource fact | Meaning | Recommended behavior |
   |---|---|---|
   | Present | Retrieved successfully and has a value | Evaluate normally |
   | Absent | Retrieved successfully; field or tag does not exist | Only explicit absence checks match |
   | Unavailable | API failed, permissions were denied, or collection was incomplete | Defer destructive action and report why |

   `NOT_EXISTS` must never match because tag retrieval failed. Likewise, an unavailable exclusion must not silently evaluate to false and allow deletion.

   I would make `NOT_EQUALS` and `NOT_IN` require a present value. Use `NOT_EXISTS` explicitly when absence is intended. Also distinguish `"True"` as a tag string from `true` as a boolean.

2. **Do not hide policy decisions inside derived fields.**

   This sentence needs tightening:

   > `is_backup` gets its definition from the snapshot RQL.

   If `is_backup` means “matches these particular tag exclusions,” those decisions have moved back into Go. Preserve those tag checks directly in the centrally managed policy:

   ```json
   {
     "exclusions": [
       {
         "field": "tag:builder",
         "operator": "EQUALS",
         "value": "packer-build"
       },
       {
         "field": "tag:aws:backup:source-resource",
         "operator": "EXISTS"
       }
     ]
   }
   ```

   A derived fact such as `used_by_ami` is useful when it represents an actual relationship retrieved from AWS. Define its data source, completeness requirements, and failure behavior. Failing to enumerate AMIs cannot produce `used_by_ami=false`.

   Your `tag:<key>` notation is workable; parse only the initial `tag:` prefix so keys containing colons remain intact.

3. **The vocabulary does not yet demonstrate complete migration parity.**

   The other six RQL queries are already in your earlier [IMG_4911.jpg](/Users/frknio/Downloads/IMG_4911.jpg) and [IMG_4912.jpg](/Users/frknio/Downloads/IMG_4912.jpg). They reveal several gaps:

   - AMI rules test `imageOwnerAlias` absence, but your field catalog does not expose that fact.
   - AMI deregistration excludes approval, forensic, and waiver tags, plus explicit AMI IDs.
   - The original volume and snapshot tagging queries do not require `SSI_Nuke` to be absent. Adding `NOT_EXISTS` changes behavior when the tag already exists with a different value.
   - Actual AMI-reference checks are stronger than the screenshot’s Packer-tag exclusion.

   Add the missing facts and translate the predicates explicitly. AWS documents the AMI ownership metadata in its [Image API structure](https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_Image.html).

   If the new tag-absence check merely avoids unnecessary writes, the executor can recognize that the desired tag already exists. What to do with a *different* existing value is a business decision that should remain explicit.

4. **Validate the age source, not only the minimum number.**

   Requiring any `OLDER_THAN_DAYS` condition above a minimum is insufficient. A volume-delete policy could satisfy that requirement using an arbitrary old tag instead of the volume’s creation time.

   Define approved age bases for each destructive action/resource combination. For example, volume creation time and the explicitly approved Lambda `created_timestamp` convention.

   Document timestamp formats, UTC comparison, strict boundaries, and treatment of future or malformed values. A mutable creation tag proves the age claimed by that tag; it does not independently establish the function’s actual creation date.

5. **Make the action cap a distributed execution guarantee.**

   A local counter inside a Worker does not enforce a per-account limit across concurrent invocations, retries, three regions, and overlapping runs.

   Define a stable `run_id`, the exact budget scope, and an atomic reservation mechanism before mutations. Coordinate reservations with idempotency so duplicate messages do not independently acquire permission for additional actions. Keep this operational state separate from the seven policy records.

   This matters because SQS-triggered Lambda processing can deliver duplicates. [AWS Lambda/SQS documentation](https://docs.aws.amazon.com/lambda/latest/dg/with-sqs.html). DynamoDB supports conditional atomic updates that can form part of that coordination. [Atomic counter examples](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/example_dynamodb_Scenario_AtomicCounterOperations_section.html).

6. **Keep the flat language, but define interactions between policies.**

   AND conditions, OR exclusions, and list membership are a reasonable first version. I would not add nesting before the actual policies require it.

   However, “anything else becomes two rules” can create overlapping matches or conflicting actions. Define duplicate-action handling and preserve which policies matched.

   Cross-rule checks should compare resource type, tag key/value, age source, and overlapping scope—not just age numbers. They still do not establish a grace period after tagging.

7. **Enforce publication rules at the writer boundary.**

   The shared validation package and policy fixtures are strong choices. Ensure the central publishing role is the controlled write path; CI checks cannot protect direct table edits that bypass it.

   I would make `resource_type` and `action.type` immutable for an existing rule. Decide separately whether action parameters are immutable: freezing the entire `action` object also prevents centrally changing tag parameters without creating a new rule.

   Log the engine build version alongside `policy_version`, because changing an adapter or operator can change policy behavior without changing the stored item.

Also restore `us-west-1`, and describe “list once” as one paginated discovery pass per account, region, and resource type, plus any required enrichment.

The Cloud Custodian reference is useful. Its example includes marking, unmarking when a volume becomes attached, and later evaluating the scheduled deletion date. If you adopt that model, carry over the complete lifecycle; the marking tag itself does not schedule execution. [Custodian’s EBS example](https://cloudcustodian.io/docs/usecases/ebsgarbagecollect.html).
