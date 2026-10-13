# Go Lambdas vs Step Functions for the TTL cleanup runtime

## Purpose and scope
The spike asked one question: **can AWS Step Functions replace the Go Lambdas?** The motivation is vulnerability management and certification. A Go Lambda is a build artifact, so it carries CVE scanning, dependency patching, SBOM generation, runtime-deprecation migrations and re-certification, and it carries them across roughly 1,200 deployment targets (400+ accounts x 3 regions). A state machine is a JSON definition in Terraform, with no artifact to scan or patch.

The answer to the literal question is **yes, technically**. Every AWS API this system calls is reachable from a Step Functions `Task` state, so a zero-Lambda design is buildable. The spike's useful output is not that answer, it is the list of constraints that come with it and the workarounds each one needs.

In scope: the runtime only. The Discovery and Worker components, their orchestration, and how they read the policy table. Out of scope: the pipeline and infrastructure deployment workstream, and the policy table schema itself, which is settled in the README.

**One decision has already been taken and is treated here as a prerequisite, not an option:** the `ttl_policies` table becomes a DynamoDB global table with replicas in us-east-1, us-west-1 and us-west-2. See [Global table: a prerequisite for Option B](#global-table-a-prerequisite-for-option-b).

### How claims are labelled

Nothing in this document is asserted without a label.

| Label | Meaning |
|---|---|
| **Verified** | Read from AWS documentation or the AWS public price list API. URL cited inline |
| **Measured** | Computed locally during the spike. The method is stated so it can be re-run |
| **Estimated** | Derived from assumptions. The assumptions are stated |
| **Unverified** | Believed true, not confirmed. Needs a test before anything depends on it |

Statements drawn from AWS documentation are paraphrased rather than quoted, with the source page linked at the point of use and again in the [evidence table](#evidence-table). Where a figure matters, the figure is given as published.

## The options

### Option A: Go Lambdas (baseline)

What the README describes today.

```
EventBridge schedule (per account, per region)
  -> Discovery Lambda (Go, AWS SDK)
       reads ttl_policies, enumerates resources, evaluates policies
  -> SQS work queue (+ DLQ)
  -> Worker Lambda (Go)
       re-reads the policy, rechecks the resource, acts
```

Policy interpretation lives in Go. The `Policy` structs are the schema and the loader enforces it. Two deployable artifacts per target.

### Option B: Step Functions, zero Lambdas

One Standard state machine per account and region. All work done by AWS SDK service integrations. Policy evaluation in JSONata. No Go code, no build artifact, no SQS between stages unless deliberately reintroduced.

```
EventBridge schedule (per account, per region)
  -> Standard state machine
       Task  aws-sdk:dynamodb:scan        read applicable policies
       Task  aws-sdk:ec2:describeVolumes  enumerate, NextToken loop in ASL
       Map   per candidate: recheck -> Choice -> act -> audit write
```

### Hybrid sub-variant: Step Functions orchestrating Lambdas

A middle path: Step Functions for control flow, one or more Go Lambdas for the parts that are awkward in ASL (enumeration with pagination, the snapshot-in-use cross-check, schema validation).

The structural observation worth recording: **the vulnerability-management benefit is close to binary.** One Go Lambda anywhere in the path keeps the whole build, scan, sign, patch and deploy pipeline alive, and keeps the runtime-deprecation clock running. A hybrid therefore takes on Step Functions' payload and pagination constraints while retaining Lambda's maintenance surface. It can still be the right shape for other reasons. It removes Go from policy *evaluation* while keeping it for data *handling*, which is where the 256 KiB ceiling bites hardest. The point is only that it does not deliver the spike's primary motivation.

## Side-by-side summary

| Dimension | Option A: Go Lambdas | Option B: Step Functions, zero Lambdas |
|---|---|---|
| Patching surface | Go runtime, module dependencies, base image. Scanned and patched per release, across ~1,200 targets | None. No artifact. AWS patches the service |
| Certification surface | SBOM, CVE scan, signing, re-certification per release | State machine definition is Terraform. Reviewed like any other IaC |
| Runtime deprecation | AWS retires Go/provided runtimes on a schedule. Forced migrations | Not applicable |
| Deployment artifact | Two zip or image artifacts, versioned, per target | JSON definition inside the Terraform plan |
| Cross-region DynamoDB read | Works directly. `TABLE_REGION` env var, cross-region `Scan` | Not possible. Requires the global table ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/concepts-access-cross-acct-resources.html)) |
| Cross-account DynamoDB read | SDK + assumed role | Task `Credentials` field, same region only ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/concepts-access-cross-acct-resources.html)) |
| Pagination | SDK paginators. Free | Manual `NextToken` loop in ASL. Pages accumulate against 256 KiB |
| Payload ceiling | Lambda memory, configurable to 10 GB | 256 KiB per task, state or execution, hard ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/limits-overview.html)) |
| Work-item ceiling per run | None in practice | 25,000 history events per Standard execution, hard. Removed by Distributed Map ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/limits-overview.html)) |
| Concurrency control | SQS batch size + reserved concurrency | `Map` `MaxConcurrency` |
| Per-policy action caps | Counted in Go before queueing | Counted in JSONata, or a DynamoDB counter. More states, more transitions |
| Audit records | Structured logs plus an explicit write | Execution history is an audit trail for free, plus explicit writes. 90-day retention ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/limits-overview.html)) |
| Schema validation | Go loader with `DisallowUnknownFields`. Rejects unknown or mistyped fields at any depth | None at runtime. JSONata has no schema. Must move left into Terraform |
| Exclusion-list failure mode | Missing field is an error | Missing field is an empty sequence, so "nothing excluded". Fails open |
| Testing story | Go unit tests, Terratest running the real loader | `TestState` API per state, Terratest for the definition. No unit test for a whole workflow |
| Retry and partial failure | SQS redrive + DLQ, mature and well understood | `Retry`/`Catch` per state, redrive per execution. SQS-to-Step-Functions coupling needs care, see below |
| Idempotent replay | Built in Go. SQS is at-least-once, so the Worker has to be idempotent by construction | Standard is exactly-once at the workflow level and name-based duplicate starts return idempotently, but AWS scopes exactly-once as holding *unless Retry is specified in ASL* ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/concepts-standard-vs-express.html)). Retries are required by the acceptance criteria, so actions must still be idempotent by construction. Express does not manage idempotency at all |
| Local development | `go test`, run the handler locally | Deploy and run, or `TestState` one state at a time |
| Observability | CloudWatch Logs, metrics, X-Ray | Execution history, visual graph, CloudWatch Logs, metrics |
| Cost model | Per invocation + GB-second. Estimated inside the free tier at this volume | Per state transition. Scales with resource count and state count |
| Team skill requirement | Go, AWS SDK | ASL, JSONata, SDK integration parameter shapes |
| us-west-1 throttling | Lambda concurrency is the same everywhere | `StateTransition` is 800/sec in us-west-1 vs 5,000/sec in us-east-1 and us-west-2 ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/limits-overview.html)) |

## Global table: a prerequisite for Option B

Verified: cross-Region AWS SDK integration and cross-Region resource access are not available in Step Functions ([source](https://docs.aws.amazon.com/step-functions/latest/dg/concepts-access-cross-acct-resources.html)). Cross-*account* access in the same region is supported, through a `Task` state's `Credentials` field naming a target role, with that role's trust policy keyed on `sts:ExternalId` set to the state machine ARN.

Corroboration that there is no native workaround: the AWS CDK ships a `CallAwsServiceCrossRegion` construct, and its own documentation says the construct creates a Lambda function to make the call ([source](https://docs.aws.amazon.com/cdk/api/v2/docs/aws-cdk-lib.aws_stepfunctions_tasks.CallAwsServiceCrossRegion.html)). The official answer to "how do I call cross-region from Step Functions" is "use a Lambda", which is exactly what Option B is trying to avoid.

So without replicas, state machines in us-west-1 and us-west-2 could not read the us-east-1 table at all. With replicas in all three regions, every policy read is in-region and cross-account, which is supported. That is the whole reason the global table is a prerequisite.

The asymmetry belongs in the comparison: **the global table is mandatory for Option B and optional for Option A.** A Go Lambda reads us-east-1 from us-west-2 directly, subject only to the network path already tracked as an open item in the README.

What the global table changes, either way:

- **The kill switch gets weaker.** `ConsistentRead=true` against a replica is strongly consistent for that replica only. Setting a policy's `mode` to `DISABLED` becomes effective once replication reaches each region, not immediately. Replication is typically sub-second but carries no SLA. For a system that authorises deletion, that is a real change: today an apply plus a strongly consistent read is a hard stop. A region-local control would restore an immediate regional stop, for example an SSM parameter per region or a per-region control item written by the pipeline, checked before any destructive action. Whether to add one is listed in [Open questions](#open-questions-for-the-leads).
- **Operational additions.** One KMS key and key policy per region, one resource-based policy per replica, aliased Terraform providers, and the DynamoDB replication service-linked role (`AWSServiceRoleForDynamoDBReplication`) exempted from the item-write deny.
- **A new failure mode.** A replica that cannot replicate or cannot reach its key for more than 20 hours is permanently converted to a standalone table.
- **Streams are already correct.** Enabled with `NEW_AND_OLD_IMAGES`, which replicas require, and the view type cannot be changed once replicas exist. This prerequisite is already satisfied, as the README intended.

## Option A: Go Lambdas

### Pros

- **Mature and already designed.** The README specifies the item format, the loader, the safety checks and the access model against this architecture. No redesign.
- **The loader is a real schema gate.** `attributevalue.UnmarshalMap` silently skips attributes with no matching struct field, so the loader re-decodes through `encoding/json` with `DisallowUnknownFields`. A misspelled `exlusions` is an error, not an empty exclusion list. The same code runs in Terratest on every PR and at runtime on every item.
- **Pagination is free.** The AWS SDK for Go ships paginators. Enumerating every snapshot in a region is a loop, not a design problem.
- **No payload ceiling that matters.** Inventory lives in process memory, configurable up to 10 GB. The 256 KiB trap in Option B does not exist here.
- **Arbitrary logic is cheap.** The snapshot-in-use cross-check, date parsing for `created_timestamp` in whatever format the Lambda module writes, per-policy action caps, prefix matching: all ordinary code.
- **SQS semantics are well understood.** Visibility timeout, redrive policy, DLQ, replay. Idempotent replay is a matter of care, not of fighting the platform.
- **Testable offline.** `go test` covers policy evaluation without deploying anything.
- **Cost is effectively zero.** Estimated: at this volume both invocation count and GB-seconds sit well inside the Lambda free tier. See [Cost](#cost).

### Cons

- **The maintenance surface the spike exists to remove.** Two artifacts, built, scanned, signed, certified and deployed to ~1,200 targets. Every CVE in a transitive Go dependency is a release. Every runtime deprecation is a forced migration on AWS's schedule, not the team's.
- **Changing evaluation logic is a code change.** The table covers thresholds, exclusions, scope and mode, which is most day-to-day change. A genuinely new *kind* of check still needs Go, a deploy, and ordering care: the loader has to be deployed before a policy file uses a new field.
- **Two deploy artifacts to keep in step.** Discovery and Worker share the schema. A version skew between them across 1,200 targets is a real operational concern.
- **Opaque execution.** Correlating a run across Discovery logs, an SQS queue and Worker logs is log archaeology. There is no built-in picture of what one run did.

## Option B: Step Functions, zero Lambdas

### Pros

- **No artifact, so no patching, scanning or certification.** This is the point of the exercise. No SBOM, no CVE triage, no runtime-deprecation migrations, nothing to sign. The definition is reviewed as Terraform alongside the policies it reads.
- **Every required API is reachable.** Verified: SDK integrations cover over nine thousand API actions across more than two hundred services ([source](https://docs.aws.amazon.com/step-functions/latest/dg/connect-to-services.html)).
- **The execution history is an audit trail for free.** Input, output, error and timing per state, retained 90 days, reducible to 30 by support request ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/limits-overview.html)). The acceptance criteria want policy version, scope, resource ID, decision, reason, timestamp and action result; most of that is in the history already, without an explicit write.
- **Dry run is a `Choice` state.** `mode` gates whether the action task runs. No code path to get wrong.
- **Concurrency and caps are declarative.** `Map` `MaxConcurrency` bounds parallelism. Action caps are an item count checked before the `Map`.
- **Visual diff on review.** A policy-logic change shows up as a change to the graph in the plan.
- **One artifact class, not two.** Discovery and Worker can be one state machine, which removes the version-skew concern entirely.
- **Standard helps the idempotent-replay criterion, partially.** Verified: Standard is exactly-once, persists execution state between transitions, and returns an idempotent response if an execution is started with the name of one already running ([source](https://docs.aws.amazon.com/step-functions/latest/dg/concepts-standard-vs-express.html)). A daily schedule that fires twice cannot produce two concurrent runs if the execution name is derived from the date.

  **Read the guarantee precisely.** AWS scopes exactly-once as tasks and states never running more than once *unless Retry behaviour is specified in ASL* (same source). The acceptance criteria require retries, so Retry will be configured, and at that point a task can run more than once. A `DeleteVolume` that succeeds at the API but times out before the response arrives will be retried. So exactly-once removes duplicate *workflow* starts; it does not remove duplicate *action* attempts, and the action still has to be idempotent by construction. This is the same obligation Option A carries, not a reduction of it. The difference between the options here is narrower than the labels suggest.
- **`TestState` exists.** Individual states can be run in isolation against real inputs. Quota: bucket 50, refill 10/sec ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/limits-overview.html)).

### Cons

- **No cross-region reads.** Forces the global table, and with it the weaker kill switch. See above.
- **No auto-pagination.** Every `DescribeVolumes`, `DescribeSnapshots`, `DescribeImages` and `GetResources` needs an explicit `NextToken` loop in ASL, accumulating pages into a 256 KiB budget. Measured: EC2 paginates these three through `MaxResults`/`NextToken` with result keys `Volumes`, `Snapshots`, `Images` (read from the bundled aws-cli 2.35.4 botocore EC2 2016-11-15 `paginators-1.json`).
- **The 256 KiB ceiling is the hard constraint.** It is not a throughput limit, it is a correctness limit, and it fails before doing any work. See [Payload sizing](#payload-sizing-measured).
- **No schema.** JSONata resolves a missing path to an empty sequence. For a condition that fails safe. For an exclusion list it fails open. See [Validation](#validation-the-structural-difference).
- **Awkward at data handling.** The AWS docs on JSONata say, for the memory-limit error, to consider moving the transformation to a Lambda ([source](https://docs.aws.amazon.com/step-functions/latest/dg/transforming-data.html)). Option B's hardest states are exactly the kind the docs point at Lambda for.
- **No native SQS source.** Connecting SQS to a state machine needs EventBridge Pipes, and the Standard-target invocation semantics change the retry story. See the constraints table.
- **New skills.** ASL, JSONata 2.0.6, and the exact parameter shapes SDK integrations expect. Verified: Step Functions implements the JSONata 2.0.6 specification, supports all its built-in functions and operators except `$eval` (use `$parse`), and adds `$partition`, `$range`, `$hash`, `$random`, `$uuid`, `$parse` ([source](https://docs.aws.amazon.com/step-functions/latest/dg/transforming-data.html)).
- **Per-transition billing.** Small in absolute terms, but it scales with resource count and with how many states each resource passes through. See [Cost](#cost).

### SDK integration mapping

Every runtime need maps to an existing integration. The DynamoDB and Resource Groups Tagging entries were checked directly against the supported-services list; the EC2 and Lambda ones follow the same `aws-sdk:<service>:<apiAction>` form.

| Need | Task resource | Notes |
|---|---|---|
| Read applicable policies | `arn:aws:states:::aws-sdk:dynamodb:scan` | Or `getItem` per `rule_id`. In-region replica, cross-account via `Credentials` |
| Re-read policy before acting | `arn:aws:states:::aws-sdk:dynamodb:getItem` | `ConsistentRead=true`. Replica-local consistency only |
| Enumerate volumes | `aws-sdk:ec2:describeVolumes` | `NextToken` loop required |
| Delete volume | `aws-sdk:ec2:deleteVolume` | |
| Enumerate snapshots | `aws-sdk:ec2:describeSnapshots` | `OwnerIds: ["self"]`. `NextToken` loop required |
| Delete snapshot | `aws-sdk:ec2:deleteSnapshot` | |
| Enumerate AMIs | `aws-sdk:ec2:describeImages` | `Owners: ["self"]` mandatory, see below |
| Deregister AMI | `aws-sdk:ec2:deregisterImage` | |
| Enumerate Lambda functions | `aws-sdk:resourcegroupstaggingapi:getResources` | Verified: resource `arn:aws:states:::aws-sdk:resourcegroupstaggingapi:[apiAction]`, exception prefix `ResourceGroupsTaggingApi` ([source](https://docs.aws.amazon.com/step-functions/latest/dg/supported-services-awssdk.html)) |
| Delete Lambda function | `aws-sdk:lambda:deleteFunction` | |
| Add or remove tags | `aws-sdk:ec2:createTags` / `aws-sdk:ec2:deleteTags` | |
| Write an audit record | `aws-sdk:dynamodb:putItem` | Optional. Execution history covers much of it |

**`Owners: ["self"]` is not optional.** Measured from the botocore EC2 service model: if `Owners` is omitted from `DescribeImages`, results include every image the caller has launch permissions for, regardless of ownership, which means all Amazon-owned and Marketplace AMIs. `DescribeSnapshots` has the same class of risk through `OwnerIds`, which accepts account IDs, `self` and `amazon`. In Go this is a code-review item in one place. In ASL it is a parameter in a JSON definition, and an omission produces a page of AMIs nobody owns. The README's statement that AMI policies only see owned AMIs has to be enforced by that parameter either way.

### Payload sizing (measured)

**Method.** Representative objects serialised as minified UTF-8 JSON and byte-counted locally. Policy items follow the schema documented in the README. EC2 objects were constructed from the documented response fields in the botocore EC2 2016-11-15 service model, with 6 tags each. **These are constructed samples, not captured from the real accounts.** Tag count dominates object size, so a fleet with heavier tagging shifts every number up. A different but equally reasonable sample moves each figure by a percent or two, which changes none of the conclusions below. The measurement is worth re-running against real `describe-*` output once a representative account is available.

Limit: 256 KiB = 262,144 bytes, per task, state or execution, as a UTF-8 string. Identical for Standard and Express. Exceeding it raises `States.DataLimitExceeded` ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/limits-overview.html)).

Policy items are not a problem:

| Payload | Measured bytes | % of 256 KiB |
|---|---|---|
| `lambda-delete`, full documented shape | 876 | 0.33% |
| A fat AMI policy carrying 25 excluded AMI IDs | 1,515 | 0.58% |
| All 8 policies, i.e. a full `Scan` result | 7,888 | 3.01% |

Resource objects are where it gets tight:

| Object | Measured bytes | Fit in 256 KiB |
|---|---|---|
| EBS snapshot, full `DescribeSnapshots` item | 678 | 386 |
| EBS volume, full `DescribeVolumes` item | 643 | 407 |
| AMI, full `DescribeImages` item, 2 block device mappings | 1,265 | 207 |
| Snapshot projected to `SnapshotId` / `StartTime` / `Tags` | 120 | 2,184 |

Two concrete traps follow.

**`MaxResults` has to be capped well below the API maximum.** Measured, one page of full `DescribeVolumes` items plus a `NextToken`:

| `MaxResults` | Measured bytes | % of limit | Result |
|---|---|---|---|
| 100 | 65,452 | 25% | fine |
| 200 | 129,852 | 50% | fine |
| 300 | 194,252 | 74% | tight |
| 400 | 258,652 | 98.7% | one extra tag away from failing |
| 500 | 323,052 | 123% | `States.DataLimitExceeded` on the first page |

**The snapshot-in-use safety check is the hardest constraint.** The README requires that a snapshot is deleted only if no AMI in the account and region references it. That needs both inventories in state at the same time. Measured:

| Inventory | Full objects | Projected down |
|---|---|---|
| 500 snapshots + 200 AMIs | 592,726 bytes, 2.3x limit | 91,726 bytes, 0.35x |
| 1,000 snapshots + 300 AMIs | 1,058,826 bytes, 4.0x | not measured |
| 2,000 snapshots + 500 AMIs | 1,991,026 bytes, 7.6x | 320,026 bytes, 1.22x |

Projection buys roughly a 6x headroom improvement and still fails at 2,000 snapshots plus 500 AMIs. In Go this check is a map lookup over whatever the account holds. In Option B it is bounded by state size, and the bound depends on inventory nobody has measured yet.

### Execution history ceiling

Verified: 25,000 events per Standard execution, and the execution fails on reaching it. Express is unlimited ([source](https://docs.aws.amazon.com/step-functions/latest/dg/limits-overview.html)).

**Estimated.** AWS documents the event types but not a per-state event count, so the following is an estimate of ASL emission patterns for an inline `Map`. Confirm it by running one execution and calling `GetExecutionHistory`.

| Per-iteration shape | Estimated events/item | Estimated item ceiling |
|---|---|---|
| 1 task | ~8 | ~3,125 |
| 2 tasks (recheck + act) | ~14 | ~1,785 |
| 2 tasks + 1 choice | ~16 | ~1,562 |
| 3 tasks + 2 choices (adds an audit write) | ~24 | ~1,041 |

The shape the acceptance criteria imply, revalidate then decide then act then record, is the bottom row. So an inline `Map` tops out around a thousand resources per execution, estimated.

**Distributed Map removes this ceiling, and it requires Standard.** Verified: Express does not support Distributed Map ([source](https://docs.aws.amazon.com/step-functions/latest/dg/concepts-standard-vs-express.html)). Each iteration becomes its own child execution with its own 25,000-event budget, and the parent records only a handful of events for the entire `Map` state. The cost is child-execution sprawl and an audit trail that has to be reassembled across executions rather than read in one place. Relevant quotas, all verified from the same page: maximum 10,000 parallel child executions in a single Map Run; Standard children dispatched at up to 100 TPS and Express at up to 1,000 TPS; maximum 1,000 open Map Runs per account per region; 14-day redrivable period.

Inline `Map` keeps one execution history, which is simpler to audit and easier to explain to an auditor. Distributed `Map` scales. That is the trade, and it is a live question for the leads.

### Failure convergence

The position taken on scale is that partial progress is fine: if a run removes some resources and fails, later daily runs converge, and taking a few days to reach steady state is acceptable. That reasoning is sound for one of the two limits and not for the other. The distinction is not obvious and is worth stating plainly.

| | 25,000-event overflow | 256 KiB overflow |
|---|---|---|
| When it fails | Partway through acting | During enumeration, before any resource is identified |
| Work completed | Everything up to the failure point, and it persists | None |
| Next run | Has less to do. Run 1 handles 1,000 of 1,500, run 2 finishes 500 | Repeats the identical failure, deterministically |
| Shape | A draining backlog | A permanent stall |
| How it presents | Alarms, then stops alarming as it converges | A quiet alarm that never clears, and cleanup silently never happens |
| Mitigation | Distributed Map, or accept convergence | Low `MaxResults` plus immediate projection |

The 256 KiB mitigation is configuration, not architecture. It does not require a different design, but it does require getting two parameters right and keeping them right as tagging conventions change.

## Constraints specific to Option B, and their workarounds

| Constraint | Impact | Workaround | Residual risk |
|---|---|---|---|
| No cross-region SDK integration ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/concepts-access-cross-acct-resources.html)) | us-west state machines cannot read the us-east-1 table | Global table with three replicas. Already decided | `DISABLED` is no longer instantaneous in every region. Weaker kill switch. 20-hour replication failure permanently detaches a replica |
| 256 KiB per state ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/limits-overview.html)) | Enumeration fails outright above a few hundred full objects | `MaxResults` ≤ 200, project each page to the fields the policy needs before storing it | Does not converge on failure. A tagging-convention change can push an object past the per-page budget silently |
| Snapshot-in-use check needs two inventories in state | Measured 2.3x over the limit at 500 snapshots + 200 AMIs | Project both sides hard. Or page AMIs and check incrementally, which multiplies states and transitions. Or move this one check to a Lambda, which forfeits the zero-Lambda property | Still measured 1.22x over at 2,000 snapshots + 500 AMIs. Inventory is unmeasured, so the real headroom is unknown. The README's rule is that if the check cannot complete, no snapshots are deleted in that region for the run, and that has to be preserved |
| 25,000 history events per Standard execution ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/limits-overview.html)) | Estimated ~1,000-item ceiling for the per-item shape the acceptance criteria imply | Accept convergence across daily runs, or use Distributed Map | Distributed Map scatters the audit trail across child executions and adds Map Run quotas to manage |
| No auto-pagination | `NextToken` loop in ASL per enumeration | A reusable loop fragment: Task -> accumulate -> Choice on `NextToken` | More states means more transitions, more history events and more cost. The loop is hand-written per resource type |
| No schema for policy items | A misspelled exclusion field loads as "nothing excluded". Fails open on a delete policy | Move validation into Terraform: unknown-field rejection, type checks, per-`resource_type` field legality, and the tag/delete pair consistency rule. Pair with `TestState` and Terratest | Terraform validation is harder to express than Go struct tags and easier to leave incomplete. Partial mitigation only: a path returning nothing can raise `States.QueryEvaluationError` under "failure to return a result", but a filter over a tag key that simply does not match yields an empty sequence silently |
| JSONata 1-second evaluation timeout, per expression ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/transforming-data.html)) | An expression that filters a large array can time out | Keep each `{% %}` small. Filter inside `Map` per item rather than over the whole inventory in one expression | The budget is per expression evaluation, not per state machine or execution, so this is manageable, but it interacts with payload size. A big array plus a complex predicate hits either the timeout or the memory ceiling |
| JSONata memory ceiling, number not published ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/transforming-data.html)) | Cannot be designed against precisely. AWS names the error but publishes no figure | Same mitigation: small expressions over small inputs | Unquantifiable by design. Only testing at real inventory size gives confidence. The docs' own suggestion for this error is to move the transform to a Lambda |
| All JSONata failures raise `States.QueryEvaluationError` ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/transforming-data.html)) | Type error, type incompatibility, out-of-range, no result, memory, timeout and stack overflow are one error code | `Catch` it and route to an explicit failure state. `Retry` is available but only helps transient causes | Hard to distinguish a bad policy item from a transient blip without parsing the error message |
| No native SQS event source | Discovery-to-Worker decoupling needs EventBridge Pipes ([SQS source](https://docs.aws.amazon.com/eventbridge/latest/userguide/eb-pipes.html), [construct](https://docs.aws.amazon.com/solutions/latest/constructs/aws_sqs_pipes_stepfunctions.html)) | Either adopt Pipes, or drop SQS and do everything in one state machine with `Map` for fan-out | See the next row. Dropping SQS also drops the DLQ, so "failed work for investigation and replay" becomes "failed execution for redrive" |
| Pipes cannot invoke a Standard state machine synchronously | Verified: `REQUEST_RESPONSE` is the default invocation type but is not supported for Standard workflows, so a Standard target must use `FIRE_AND_FORGET`, i.e. `StartExecution` ([source](https://docs.aws.amazon.com/eventbridge/latest/pipes-reference/API_PipeTargetStateMachineParameters.html)). Verified separately: Pipes deletes SQS messages once it has successfully processed the batch ([source](https://docs.aws.amazon.com/eventbridge/latest/userguide/eb-pipes-sqs.html)) | Use Express as the Pipes target so it can be invoked synchronously, but see the Express row below for what that costs. Or handle failure inside the workflow and write to a failure table rather than relying on SQS redrive. Or drop SQS | **Unverified in combination.** The two facts above together imply the message is deleted when the execution *starts*, so a failure inside the workflow would not return it to the queue and the DLQ would never see it. That directly affects the retry, partial-failure and idempotent-replay acceptance criteria. Test this before relying on an SQS-based Option B design |
| `StateTransition` throttle is 800/sec outside us-east-1, us-west-2 and eu-west-1 ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/limits-overview.html)) | us-west-1 gets roughly 6x less headroom than the other two in-scope regions | Soft quota, increasable by request. Or bound `Map` concurrency per region | At current estimated volume this is nowhere near binding. It matters only if a backlog is ever processed at full concurrency |
| Standard execution history retained 90 days ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/limits-overview.html)) | If the history *is* the audit record, audit retention is 90 days | Write explicit audit records to DynamoDB or S3 for anything that must outlive 90 days | Adds a task per item, so more transitions and more history events. Partly cancels the "audit for free" advantage |
| Express gives up several Standard properties ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/concepts-standard-vs-express.html)) | Choosing Express for the synchronous Pipes invocation costs the free audit trail, Distributed Map, Activities, and the `.sync` and `.waitForTaskToken` patterns. It also caps at 5 minutes, does not persist execution state between transitions, and does not manage idempotency | Configure CloudWatch Logs for history. Build idempotency into the action itself: check the resource's current state before acting, which the final-revalidation criterion already requires | Asynchronous Express is at-least-once, so an action can run twice. Synchronous Express is at-most-once and does not restart on an exception, so an action can run zero times and report nothing. Both need deliberate handling on a delete path |
| State machine definition max 1 MB, request max 1 MB ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/limits-overview.html)) | A definition handling 4 resource types, 8 policies, pagination loops and audit writes gets large | Split per resource type, or per policy, into separate state machines | More state machines per account and region to deploy and monitor. Not close to binding at current scope, but worth watching |

## Concerns that apply to both options

These are not differentiators. They are listed so they are not misattributed to one option during the decision.

- **EC2 filters are inclusive only.** Measured from the botocore EC2 model: `DescribeVolumes` supports server-side filters including `attachment.attach-time`, `attachment.delete-on-termination`, `attachment.device`, `attachment.instance-id`, `attachment.status`, `availability-zone`, `availability-zone-id`, `create-time`, `encrypted`, `fast-restored`, `multi-attach-enabled`, `operator.managed`, `operator.principal`, `size`, `snapshot-id`, `status`, `tag-key`, `volume-id`, `volume-type`, plus the generic `tag:<key>` form. Values inside one filter are OR'd, separate filters are AND'd, and **there is no negation operator**. So policy *conditions* can often be pushed server-side (`status=available`, `tag:SSI_Nuke=True`), but policy *exclusions* never can. Packer-build snapshots, AWS Backup snapshots, `pcs-`/`el-`/`es-` prefixes and excluded AMI IDs are all client-side work. `create-time` matches by string and wildcard rather than range, so "older than N days" cannot be pushed server-side either. This affects both options identically, but it is important for sizing: the enumerated set is always larger than the candidate set, and it is the enumerated set that has to fit in 256 KiB under Option B.
- **AMI and snapshot inventory is unmeasured.** One development account was reported as having roughly 50 Lambda functions and 10 EBS volumes, under existing Prisma Cloud cleanup. That is a single sample and it does not cover the two resource types that drive Option B's sizing. AMIs and snapshots matter more because client-side exclusions reduce what gets *acted on* but not what gets *enumerated into state*, and because the snapshot-in-use check needs the full AMI inventory regardless of how few snapshots are candidates. To measure, per region:

  ```bash
  for r in us-east-1 us-west-1 us-west-2; do
    echo "$r snapshots: $(aws ec2 describe-snapshots --owner-ids self --region $r --query 'length(Snapshots)')"
    echo "$r images:    $(aws ec2 describe-images --owners self --region $r --query 'length(Images)')"
  done
  ```

  Run this across a representative spread of accounts, not one. Option B's feasibility at the hard edges depends on the answer.
- **Policy data correctness is outside both runtimes.** The DynamoDB item is the source of truth. If a threshold or an exclusion value is wrong in the table, both options do the wrong thing correctly. Neither architecture helps. What differs is only whether a *structurally malformed* item is caught, which is covered below.
- **The 7-day grace period is not guaranteed today.** The delete policies measure resource age at 37 days rather than waiting 7 days after the cleanup tag was applied. A resource first seen when it is already past 37 days is tagged on one run and deleted on the next. Carrying that behaviour forward is a product decision, and the README tracks it as an open item. Guaranteeing the gap needs a tag recording when the cleanup tag was added, in either option.
- **`created_timestamp` format is unconfirmed.** `lambda-delete` depends on parsing it. Both options need the exact format the enforced Lambda module writes, and confirmation that the value stays at creation time. Option B needs it expressed in JSONata date functions rather than Go time parsing, which is a tighter constraint on what formats are tolerable, but the underlying unknown is shared.
- **Excluded-account enforcement.** `scope.excluded_accounts` has to be honoured before any action, in either option. Under Option B that is a `Choice` state against the account ID, taken from the execution ARN in `$states.context` or baked into the per-account definition; under Option A a check in Go. Both need the account list confirmed, which the README tracks as open.
- **Global table kill-switch consistency.** Listed here as well as above because the decision has been taken: it now applies to whichever option is chosen, so it is no longer an Option B cost alone.

## Validation: the structural difference

Both options share the policy-data limit above. The narrower structural difference is worth recording precisely, because it is the one place where Option B is weaker in a way that is not just operational.

**Go today.** `attributevalue.UnmarshalMap` silently skips attributes with no matching struct field. The project's loader compensates by re-decoding through `encoding/json` with `DisallowUnknownFields`, so a misspelled field at any depth, `exlusions` for `exclusions`, is rejected as an error rather than loading as an empty value. That is a deliberate design choice in the README, made for exactly this reason.

**JSONata.** There is no schema. A path that resolves to nothing yields an empty sequence. The consequence depends on which field it is:

| Field | Empty sequence means | Direction of failure |
|---|---|---|
| A condition | "no match" | Fails safe. The resource is skipped |
| An exclusion list | "nothing is excluded" | **Fails open.** A protected resource becomes eligible for deletion |

Partial mitigation: a path returning nothing can raise `States.QueryEvaluationError` under the "failure to return a result" case ([verified](https://docs.aws.amazon.com/step-functions/latest/dg/transforming-data.html)). But a filter over a tag key that simply does not match yields an empty sequence silently, which is the common case. The mitigation does not cover the dangerous path.

**What Option B therefore requires.** The schema contract has to move left into Terraform, because there is no runtime loader to reject a malformed item. Terraform would need to absorb what the Go loader does today:

- reject fields the schema does not declare, at any depth
- type checking, including the DynamoDB type codes (a number written `{ "S": "30" }`)
- per-`resource_type` field legality, for example rejecting `state` on a Lambda policy
- the tag/delete pair consistency rule: a delete policy must require the same tag value its tag policy adds, and use a larger age
- the existing age floor and non-empty-folder checks, which Terraform already has

The README notes Terraform currently keeps only two plan checks and relies on Go for the rest. Under Option B all of it becomes Terraform's job. `TestState` plus Terratest is the replacement testing story: `TestState` exercises individual states against real inputs, Terratest validates every file in `policies/` against whatever validation replaces the loader.

This is sized work, not an objection. It is listed here so it is priced into Option B rather than discovered later.

## Cost

**This is an order-of-magnitude estimate, not a forecast.** The rates are verified. Everything else is an assumption, and the assumptions dominate the result. The team should redo this with its own figures once inventory is measured.

### Rates (verified)

Read from the AWS public price list API, `AmazonStates` offer, publication 2026-09-11, cross-checked against the [pricing page](https://aws.amazon.com/step-functions/pricing/):

| Item | Rate |
|---|---|
| Standard state transition, us-east-1 | $0.000025 |
| Standard state transition, us-west-2 | $0.000025 |
| Standard state transition, us-west-1 | $0.0000279 |
| Free tier | First 4,000 state transitions free. The usage type is `Global-StateTransition`, which indicates per account rather than per account and region |
| Express | $0.000001 per request plus $0.00001667 per GB-second for the first 3.6M GB-s |

Lambda, from the `AWSLambda` offer: free tier is 1,000,000 requests and 400,000 GB-seconds, both `Global-` usage types, so per account.

An AWS blog puts 1,000 runs of a sample Standard workflow at roughly $0.42 ([source](https://aws.amazon.com/blogs/compute/building-cost-effective-aws-step-functions-workflows/)), which is a useful sanity check on the order of magnitude.

### Formula

```
transitions_per_run        = fixed_overhead + per_candidate * candidates
transitions_per_account_mo = transitions_per_run * runs_per_month * regions
billable                   = max(0, transitions_per_account_mo - 4000)
monthly_cost               = billable * rate * accounts
```

### Assumptions (estimated)

- `fixed_overhead` = 50 transitions per run. Policy `Scan`, four enumerations with ~2 pages each and their loop states, plus eight policy branches.
- `per_candidate` = 4 transitions. Recheck, `Choice`, act, audit write.
- `runs_per_month` = 30. One run per account per region per day.
- `regions` = 3. `accounts` = 400 for full-estate steady state.
- Rate blended evenly across the three regions, $0.0000260 per transition.
- The free tier is assumed available. In accounts already using Step Functions for other work it is already consumed, which raises the figure.

### Result (estimated)

| Candidates per run | Transitions/run | Transitions/account/month | $/account/month | $/month at 400 accounts |
|---|---|---|---|---|
| 10 | 90 | 8,100 | 0.11 | 43 |
| 20 | 130 | 11,700 | 0.20 | 80 |
| 50 | 250 | 22,500 | 0.48 | 192 |
| 100 | 450 | 40,500 | 0.95 | 379 |
| 200 | 850 | 76,500 | 1.88 | 753 |
| 500 | 2,050 | 184,500 | 4.69 | 1,875 |

Initial footprint, assuming 10 approved development accounts: roughly $2/month at 20 candidates per run, roughly $19/month at 200.

### Option A at the same workload (estimated)

At 20 candidates per run: about 1,890 invocations per account per month and about 2,835 GB-seconds, assuming 512 MB and a 3-second average. At 200 candidates per run: about 18,090 invocations and 27,135 GB-seconds. Both are inside the 1,000,000-request and 400,000-GB-second free tier, so Lambda compute cost is effectively zero at this volume. The same free-tier caveat applies: accounts already running Lambdas have consumed some of it.

### Reading this

Shared costs, meaning SQS, DynamoDB reads, CloudWatch and KMS, are broadly the same either way and are left out. The cost difference between the options is in the low hundreds of dollars per month at full estate under these assumptions, against a maintenance burden measured in engineer time across ~1,200 deployment targets. Both sides of that comparison are estimates, and the second one is the harder to quantify. The figures are here to establish that cost is not the deciding factor at this scale, not to rank the options.

## Evidence table

| Claim | Status | Source or method |
|---|---|---|
| No cross-region SDK integration or resource access | Verified | [concepts-access-cross-acct-resources.html](https://docs.aws.amazon.com/step-functions/latest/dg/concepts-access-cross-acct-resources.html) |
| Cross-account works in-region via `Credentials` + `sts:ExternalId` | Verified | Same page |
| AWS's own cross-region workaround creates a Lambda | Verified | [CallAwsServiceCrossRegion](https://docs.aws.amazon.com/cdk/api/v2/docs/aws-cdk-lib.aws_stepfunctions_tasks.CallAwsServiceCrossRegion.html) |
| JSONata 2.0.6, `$eval` unavailable, `$parse` et al. added | Verified | [transforming-data.html](https://docs.aws.amazon.com/step-functions/latest/dg/transforming-data.html) |
| `Arguments`/`Output` replace the five JSONPath fields; `Choice` uses `Condition`; `$states` exposes `.input`/`.result`/`.errorOutput`/`.context` | Verified | Same page |
| 1-second expression timeout, per expression evaluation | Verified | Same page |
| Memory ceiling exists, no published figure, docs suggest moving large transforms to Lambda | Verified | Same page |
| All seven JSONata failure modes raise `States.QueryEvaluationError`, catchable and retryable | Verified | Same page |
| 256 KiB max input/output per task, state or execution; same for Standard and Express | Verified | [limits-overview.html](https://docs.aws.amazon.com/step-functions/latest/dg/limits-overview.html) |
| 25,000 history events per Standard execution, execution fails on reaching it; Express unlimited | Verified | Same page |
| 1 MB max definition size, 1 MB max request size | Verified | Same page |
| 1 year max execution (Standard), 5 minutes (Express) | Verified | Same page |
| 90-day history retention, reducible to 30 by support request; Express needs CloudWatch Logs | Verified | Same page |
| 14-day redrivable period | Verified | Same page |
| 10,000 parallel child executions per Map Run; 100 TPS Standard / 1,000 TPS Express dispatch; 1,000 open Map Runs per account per region | Verified | Same page |
| `StateTransition` 5,000/sec in us-east-1, us-west-2, eu-west-1; 800/sec elsewhere; soft quota | Verified | Same page |
| `TestState` quota: bucket 50, refill 10/sec | Verified | Same page |
| Standard is exactly-once; asynchronous Express at-least-once; synchronous Express at-most-once and does not restart on an exception | Verified | [concepts-standard-vs-express.html](https://docs.aws.amazon.com/step-functions/latest/dg/concepts-standard-vs-express.html) |
| Standard's exactly-once model holds for tasks and states only where Retry is not specified in ASL; with Retry configured a task can run more than once | Verified | Same page |
| Standard returns an idempotent response when an execution is started with the name of a running one; Express does not manage idempotency | Verified | Same page |
| Express does not support Distributed Map, Activities, or the `.sync` and `.waitForTaskToken` patterns | Verified | Same page |
| Express does not persist execution state between state transitions | Verified | Same page |
| Over 9,000 API actions across 200+ services | Verified | [connect-to-services.html](https://docs.aws.amazon.com/step-functions/latest/dg/connect-to-services.html) |
| `resourcegroupstaggingapi` integration available, prefix `ResourceGroupsTaggingApi` | Verified | [supported-services-awssdk.html](https://docs.aws.amazon.com/step-functions/latest/dg/supported-services-awssdk.html) |
| Standard state transition rates and the 4,000 free tier | Verified | AWS price list API, `AmazonStates`, publication 2026-09-11 |
| Lambda free tier 1M requests + 400,000 GB-s, per account | Verified | AWS price list API, `AWSLambda` |
| ~$0.42 per 1,000 runs of a sample Standard workflow | Verified | [AWS compute blog](https://aws.amazon.com/blogs/compute/building-cost-effective-aws-step-functions-workflows/) |
| Pipes cannot invoke Standard synchronously; Standard must use `FIRE_AND_FORGET` | Verified | [PipeTargetStateMachineParameters](https://docs.aws.amazon.com/eventbridge/latest/pipes-reference/API_PipeTargetStateMachineParameters.html) |
| Pipes deletes SQS messages after it successfully processes a batch | Verified | [eb-pipes-sqs.html](https://docs.aws.amazon.com/eventbridge/latest/userguide/eb-pipes-sqs.html) |
| Step Functions has no native SQS event source; Pipes is the connector | Verified | [eb-pipes.html](https://docs.aws.amazon.com/eventbridge/latest/userguide/eb-pipes.html), [SQS-Pipes-Step Functions construct](https://docs.aws.amazon.com/solutions/latest/constructs/aws_sqs_pipes_stepfunctions.html) |
| **Composite:** a Standard workflow failure under Pipes does not return the message to SQS, so the DLQ never sees it | **Unverified** | Follows from the two verified Pipes facts above. Not confirmed by test. Test before relying on it |
| `DescribeVolumes`/`DescribeSnapshots`/`DescribeImages` paginate via `MaxResults`/`NextToken`; result keys `Volumes`/`Snapshots`/`Images` | Measured | botocore `ec2/2016-11-15/paginators-1.json`, bundled with aws-cli 2.35.4 |
| `DescribeImages` without `Owners` returns all images the caller can launch, regardless of ownership; `DescribeSnapshots.OwnerIds` accepts account IDs, `self`, `amazon` | Measured | botocore `ec2/2016-11-15/service-2.json` parameter documentation |
| `DescribeVolumes` server-side filter list; inclusive only, no negation; `create-time` is string/wildcard | Measured | Same model file |
| Policy item sizes: 876 / 1,515 / 7,888 bytes | Measured | Minified UTF-8 JSON byte count of items built to the README schema |
| EC2 object sizes: 678 / 643 / 1,265 / 120 bytes | Measured | Same method, objects constructed from documented API fields with 6 tags each. Constructed samples, not captured from the real accounts |
| `MaxResults` page sizes: 25% / 50% / 74% / 98.7% / 123% of 256 KiB at 100/200/300/400/500 | Measured | Derived from the per-object measurement |
| Snapshot-in-use check 2.3x over at 500+200, 7.6x at 2,000+500; 1.22x at 2,000+500 projected | Measured | Same method |
| Step Functions SDK integrations do not auto-paginate | Unverified | Consistent with the integrations behaving as plain API calls and with the absence of any paginator configuration in ASL, but not stated as such in a doc page read during this spike. Confirm with one `TestState` call |
| ~8 / 14 / 16 / 24 history events per inline `Map` iteration, and the resulting ~3,125 / ~1,785 / ~1,562 / ~1,041 item ceilings | Estimated | Assumed ASL emission pattern. AWS documents event types but not per-state counts. Confirm by running one execution and calling `GetExecutionHistory` |
| Cost table | Estimated | Verified rates, assumed fixed overhead of 50 transitions per run and 4 per candidate, 30 runs/month, 3 regions |
| ~50 Lambda functions and ~10 EBS volumes in a development account | Reported | Single sample, reported by the team, under existing Prisma Cloud cleanup |
| AMI and snapshot counts per account and region | **Unmeasured** | Not yet collected. Commands given above. Option B's hard-edge feasibility depends on this |
| DynamoDB global table replication latency | Unverified | Typically sub-second, no SLA. Measure if the kill-switch delay matters |
| Terraform module 11.0.0 support for streams and resource-based policy | Unverified | Already an open item in the README. Unchanged by this comparison |

## Open questions for the leads

These are the decisions this document cannot make.

- **Does the exclusion fail-open behaviour in Option B get accepted, and on what terms?** On one side, moving the full schema contract into Terraform restores the gate but is real work and is easier to leave incomplete than Go struct tags. On the other, accepting it means a misspelled exclusion field in a delete policy makes that policy more permissive than its file reads, which is precisely the failure the README's storage design was built to prevent.
- **Is a hybrid acceptable, given the maintenance benefit is close to binary?** A hybrid solves the 256 KiB and pagination problems cleanly and removes Go from policy evaluation. It does not reduce the CVE, SBOM, signing or runtime-deprecation surface, because one Lambda keeps the whole pipeline. Whether that is a reasonable trade depends on how much of the certification burden is per-artifact versus per-release, which is a question for the certification owners rather than this document.
- **Standard or Express?** Standard gives a 90-day execution history that covers most of the audit criteria without extra writes, exactly-once workflow execution (subject to the Retry caveat above, so actions still need to be idempotent), automatic name-based idempotency, redrive and Distributed Map, but caps at 25,000 history events. Express has unlimited history events and can be invoked synchronously by Pipes, but caps at 5 minutes, does not support Distributed Map, Activities or the `.sync` and `.waitForTaskToken` patterns, has no visible history without CloudWatch Logs, and does not manage idempotency. Asynchronous Express is at-least-once so an action can run twice; synchronous Express is at-most-once and does not restart on an exception, so an action can run zero times. All verified on the [workflow type comparison](https://docs.aws.amazon.com/step-functions/latest/dg/concepts-standard-vs-express.html). Note the interaction: Express is the way to get synchronous Pipes invocation, and it is also the type that cannot use Distributed Map.
- **Inline `Map` or Distributed `Map`?** Inline keeps one execution history, which is simpler to audit and to explain, and caps at an estimated ~1,000 items per run. Distributed removes the cap and scatters the audit trail across child executions, adding Map Run quotas and reassembly work.
- **Is us-west-1 in scope?** It is already an open item in the README. Under Option B it gains two extra consequences: a higher per-transition rate ($0.0000279 vs $0.000025, verified) and a `StateTransition` quota of 800/sec against 5,000/sec in the other two regions (verified). Neither is binding at estimated volume, but both are reasons to settle the question before building three regional stacks.
- **Does the Pipes invocation behaviour get tested before an SQS-based Option B is relied on?** The composite claim is unverified and it bears directly on the retry, partial-failure, DLQ and idempotent-replay acceptance criteria. The alternative is to drop SQS entirely and keep everything in one state machine, which trades the DLQ for execution redrive. That is a different operational model for failed work, and the leads would need to decide whether it meets the dead-letter and replay criteria.
- **Does AMI and snapshot inventory get measured before the decision, or after?** Option B's feasibility at its hardest edge, the snapshot-in-use check, depends on numbers nobody has yet. The measurement is two CLI calls per account and region.
- **Where does the region-local kill switch live, if one is added?** The global table makes `DISABLED` eventually effective rather than immediately effective in every region. An SSM parameter per region or a per-region control item written by the pipeline would restore a hard regional stop, at the cost of a second control plane to keep consistent with the table.
