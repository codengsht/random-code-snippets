# gp2 vs gp3 EBS volumes

Comparison and rationale for migrating from gp2 to gp3.

## Side-by-side

| Property | gp2 | gp3 |
|---|---|---|
| Volume size | 1 GiB – 16 TiB | 1 GiB – 16 TiB |
| Baseline IOPS | 3 IOPS per GiB (min 100, max 16,000) | 3,000 IOPS — flat, regardless of size |
| Burst IOPS | Up to 3,000 via burst credits (volumes <1,000 GiB) | N/A — baseline already 3,000 |
| Max IOPS | 16,000 (only at ≥5,334 GiB) | 16,000 |
| Baseline throughput | Scales with size, up to 250 MB/s | 125 MB/s — flat |
| Max throughput | 250 MB/s (at ≥334 GiB) | 1,000 MB/s |
| IOPS / throughput tunable independently of size? | No | Yes |
| Extra IOPS cost | Not available | $0.005 per IOPS-month above 3,000 |
| Extra throughput cost | Not available | $0.040 per MB/s-month above 125 |
| Storage price (us-east-1) | $0.10 / GB-month | $0.08 / GB-month |
| Latency | Single-digit ms | Single-digit ms (similar) |
| Multi-attach | No | No (use io1/io2 if needed) |
| Snapshot pricing | Same | Same |

## Where gp3 shines

**Smaller volumes get more performance for free.** A 50 GiB gp2 volume gets
150 baseline IOPS (with bursting up to 3,000). Same gp3 volume gets 3,000 IOPS
baseline, period. Boot volumes and small data volumes stop being bottlenecks.

**Decoupled tuning.** Need 6,000 IOPS on a 100 GiB volume? On gp2 you'd over-provision
storage to ~2,000 GiB to get there. On gp3 you pay for 100 GiB plus 3,000 extra IOPS.
Same for throughput — push to 1,000 MB/s without sizing storage up.

**~20% cheaper per GB at the storage tier.** Below the threshold where you're
buying extra IOPS/throughput, every gp3 volume is just cheaper than its gp2
equivalent.

## When gp3 might cost more

Mostly when a gp2 volume was huge specifically to get IOPS or throughput it
didn't otherwise need:

- A 5,334 GiB gp2 volume hits the 16,000 IOPS ceiling at gp2 storage price.
  Recreating that on gp3 means 5,334 GiB of storage + 13,000 extra IOPS — can
  come out roughly even.
- A volume with sustained throughput close to 250 MB/s on gp2: gp3's 125 MB/s
  baseline means buying extra throughput. Usually still cheaper, worth checking.

In practice, the vast majority of gp2 volumes (boot disks, app data under a few
hundred GiB) get cheaper *and* faster on gp3.

## Why switch from gp2 to gp3

1. **Cost.** gp3 storage is roughly 20% cheaper per GB. For a fleet running on
   gp2 boot volumes and small-to-medium data volumes, this is a line-item
   reduction with no architectural work — the migration is an in-place modify,
   no downtime.

2. **Better baseline for small/medium volumes.** Anything under ~1 TiB on gp2
   relied on burst credits to reach 3,000 IOPS. gp3 makes that the baseline.
   Workloads that occasionally got throttled when burst credits ran out (CI
   runners, batch jobs, dev environments) stop hitting that ceiling.

3. **Independent IOPS and throughput knobs.** No more oversizing storage to buy
   performance. If a database needs 8,000 IOPS, you provision 8,000 IOPS — not
   "however many GiB it takes to get 8,000 IOPS." Cleaner sizing, more
   predictable cost.

## Migration mechanics

Online modify, no downtime, no remount:

```bash
aws ec2 modify-volume --volume-id vol-... --volume-type gp3
# or in Terraform: change volume_type = "gp2" to volume_type = "gp3"
```

Constraint to plan around: 6-hour cooldown per volume between modifications.
Roll out in waves rather than batching with retries.

## Trade-off worth knowing

gp3's default 125 MB/s throughput is half what a >334 GiB gp2 volume got
automatically. For volumes sized large specifically for throughput (some
database log volumes, video processing scratch space), set throughput
explicitly in the gp3 config rather than relying on the default:

```hcl
resource "aws_ebs_volume" "logs" {
  type       = "gp3"
  size       = 500
  iops       = 3000
  throughput = 250   # match the gp2 max baseline
}
```
