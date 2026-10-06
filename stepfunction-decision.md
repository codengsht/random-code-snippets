Step Functions fits Discovery well, and I'd switch. Discovery's job is calling AWS APIs, filtering the results and handing off matches, and that's what SDK integrations and JSONata are built for. Two conditions:

- **Move the Worker too.** If it stays a Go Lambda, you still own a runtime, dependencies and a certification, so most of the overhead you want to remove stays.
- **Evaluate a whole page of resources in one JSONata expression,** not one Map iteration per resource. The cost and hard limits below depend on it.

"No code" really means "no runtime." The evaluator, caps and safety checks still exist as JSONata and Choice states in the state machine. They still delete resources in every account, so they need the same review and tests. Before you commit, ask AppSec whether they certify a state machine definition as IaC or as application code. That decides how much certification work actually goes away.

What you gain:

- **No runtime to maintain.** There's no Go toolchain, dependency CVEs, build artifacts or code signing, and no forced runtime upgrades. For example, [`provided.al2`](https://docs.aws.amazon.com/lambda/latest/dg/lambda-runtimes.html), a common Go runtime, was deprecated on July 31, 2026, and `provided.al2023` follows on June 30, 2029.
- **It deploys like the table.** The state machine is a Terraform resource, so it goes through the same IaC pipeline and scanning. Versions and aliases give you rollback, and a customer-managed KMS key can encrypt the definition and execution history.
- **Retries and failure handling are configuration.** Every API call gets its own Retry and Catch. A failed Describe call can go straight to "data unavailable" and skip destructive actions for that resource type, which the README already requires.
- **A built-in audit trail.** Standard workflows keep each state's input and output for 90 days, so you can see exactly what a run fetched and matched. That helps during the DRY_RUN comparison.
  - The execution ARN works as the `run_id`.
  - If the schedule starts the state machine through an alias, each execution records which version ran.
- **It fits the policy design.** There's no per-policy code, so one JSONata evaluator covers every policy, and each resource type is one paginated SDK call. New policies still need only a table change.
- **Testable before deploying.** The [TestState API](https://docs.aws.amazon.com/step-functions/latest/dg/test-state-isolation.html) runs a single state or a whole workflow against mocked service responses, including Map and Parallel. It calls the Step Functions API, so CI needs AWS credentials or LocalStack. Step Functions Local is no longer supported.

What it costs you:

- **JSONata is still code, and fewer engineers know it than Go.** Whoever reviews deletion logic has to read it comfortably, and it has traps:
  - An expression that resolves to a missing field [fails the state](https://docs.aws.amazon.com/step-functions/latest/dg/transforming-data.html) instead of returning null.
  - A filter that matches one item returns that item, not a one-item list.
- **Hard limits shape the design.** From the [service quotas](https://docs.aws.amazon.com/step-functions/latest/dg/service-quotas.html):
  - Each API response and state output must stay under 256 KiB. Otherwise the state fails with `States.DataLimitExceeded`, which `States.ALL` doesn't catch. Set `MaxResults` on every Describe call so each page fits.
  - A Standard execution fails at 25,000 history events. Evaluating per page stays far below that, but a Map iteration per resource can hit it in large accounts.
  - A JSONata expression fails if it runs longer than 1 second or uses too much memory.
  - Variables are capped at 256 KiB each (about 10,000 snapshot IDs) and 10 MiB per execution.
- **Coarser error handling.** In Go, a bad record is skipped and the loop continues. In JSONata, one bad value fails the whole page, like a `created_timestamp` that `$toMillis` can't parse. That's the downside of evaluating per page, so every expression has to check its inputs first.
- **Validation needs a new home.** The README's "same Go validator in CI and at load time" no longer applies.
  - Make validation one JSONata state, and run that state in CI through TestState against the planned items. CI and runtime then share one implementation.
  - Checks that compare old and new plan values become policy checks on the plan, using OPA, Sentinel or a script. That covers immutable fields, version bumps and tag and delete pairs. They only run in CI, so nothing extra gets deployed.
- **DynamoDB returns typed JSON.** Scan and GetItem return values like `{"N": "30"}`, so the state machine needs a small recursive JSONata function to convert items to plain JSON.
- **SDK integrations can lag.** [New API parameters](https://docs.aws.amazon.com/step-functions/latest/dg/supported-services-awssdk.html) aren't available right away. Confirm each one you rely on works, like `IncludeDisabled` on DescribeImages for the snapshot safety check.
- **Cost scales with state transitions.** Standard workflows cost [$0.000025 per transition](https://aws.amazon.com/step-functions/pricing/) in us-east-1, including retries, with 4,000 free a month. With 100 accounts in 3 regions running daily, for example:
  - Evaluating per page at roughly 150 transitions a run comes to about 1.35 million transitions a month, or around $34.
  - A Map iteration per resource, at about 3 transitions each for 5,000 resources, is 15,000 transitions a run, or around $3,400 a month.
  - The same job as a 512 MB Lambda running 60 seconds a run costs about $4.50 a month.
- **Network perimeter exceptions.** Step Functions runs outside your VPC, so Discovery no longer needs subnets or VPC endpoints. But its calls come from AWS's network under the state machine's role.
  - If your SCPs, RCPs, the table's resource policy or the KMS key policies require `aws:SourceVpc` or `aws:SourceVpce`, those calls get denied.
  - AWS's [data perimeter guidance](https://aws.amazon.com/blogs/security/establishing-a-data-perimeter-on-aws-allow-access-to-company-data-only-from-expected-networks/) handles this by exempting service roles through a tag, combined with an identity perimeter.
- **No reserved concurrency.** Add a first state that ends the run if another execution is already running. Otherwise overlapping runs could act on up to twice the per-run cap.

Use Standard workflows, not Express. Standard runs exactly once, keeps 90 days of history and has no 5-minute limit. Express is cheaper at high volume, but scheduled starts can run twice, history only goes to CloudWatch Logs, and every run stops at 5 minutes.

The Worker converts the same way. It re-reads the policy, checks `mode` and `policy_version`, rechecks the resource, acts and logs, all of which are SDK calls and Choice states. The simplest setup drops SQS and the Worker Lambda. Discovery passes each policy's matches to a Distributed Map, and each action runs as a child execution with its own history.

- `MaxConcurrency` limits the API call rate, and Catch plus an alert replaces the DLQ. If you'd rather keep the queue, EventBridge Pipes can start a state machine from SQS.
- Failed items can be redriven for 14 days, and the next run finds them again anyway.
- Test EC2 error handling early. EC2's API doesn't define separate error types, so codes like `InvalidVolume.NotFound` may only appear in the error's Cause, not its name. Treating "already deleted" as done depends on telling those apart.

What doesn't change:

- The table and policies stay as they are.
- Step Functions [can't make SDK calls to another region](https://docs.aws.amazon.com/step-functions/latest/dg/tutorial-access-cross-acct-resources.html). You still deploy one state machine per region, each reading its local replica, so the global table decision holds.
- EventBridge Scheduler starts the state machine directly through its templated StartExecution target, as your notes say.

Step Functions also makes a hub model possible. Its `Credentials` field lets one state machine per region in the central account assume a role in each LOB account, so each account only needs an IAM role. The trade-off is a single central role that can delete resources in every account. I'd stay per-account unless the per-account rollout becomes the main cost.

The pricing tool in the cloud-architect power couldn't authenticate, so the prices above come from AWS's public pricing pages. Want me to update the README for this decision, or sketch the state machine for one resource type?
