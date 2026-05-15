# Prisma Cloud RQL — find EC2 instances using gp2 EBS volumes

## Goal
Identify AWS EC2 instances backed by gp2 EBS volumes in Prisma Cloud, so they can be migrated to gp3 (typical cost/perf win).

## Approach
Use Prisma Cloud RQL on the Investigate page against the `aws-ec2-describe-volumes` config API, then optionally pivot back to `aws-ec2-describe-instances` via the volume IDs in `blockDeviceMappings`.

## Outcome
Three reusable RQL queries (see `prisma-gp2-volumes.rql`):

1. List every gp2 volume.
2. List gp2 volumes that are currently attached to an instance.
3. Pivot to the EC2 instances themselves that have any gp2 block device.

## Caveats / tips
- Switch result type to **Config** in the UI to export CSV with account, region, resource id, tags.
- Prisma ships a built-in policy "AWS EBS volumes are of not gp3 type" — consider enabling that instead of a custom policy if you just want alerting.
- For an ongoing control, save query #1 as a custom Config policy.
- Add a tag filter (e.g. `Environment = prod`) to prioritize migration targets.
- gp2 → gp3 migration can be done in place with `modify-volume`, no downtime, but watch IOPS/throughput defaults (gp3 starts at 3000 IOPS / 125 MB/s).
