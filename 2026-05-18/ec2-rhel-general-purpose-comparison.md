# EC2 General-Purpose Instance Comparison for RHEL (2 vCPU, 4/8 GiB)

Comparison of t3, m5, m5a, m5ad, m5d at the 2 vCPU tier for RHEL workloads.

## Sizing availability at 2 vCPU

- **4 GiB memory**: only `t3.medium`. The m5/m5a/m5d/m5ad families start at `.large` which is 2 vCPU / 8 GiB.
- **8 GiB memory**: `t3.large`, `m5.large`, `m5a.large`, `m5d.large`, `m5ad.large`.

If you need 2 vCPU / 4 GiB, t3.medium is the only choice in this set.

## Family-level differences

| Family | Processor | Burstable? | Local storage | Notes |
|---|---|---|---|---|
| **t3** | Intel Xeon (Skylake/Cascade Lake) | Yes (CPU credits, unlimited mode by default) | EBS only | Cheapest. For workloads with variable CPU. Throttles below baseline if credits exhausted (standard mode) or charges extra (unlimited mode). |
| **m5** | Intel Xeon Platinum 8175M/8259CL (Cascade Lake) | No, fixed performance | EBS only | General-purpose baseline. Consistent performance. |
| **m5a** | AMD EPYC 7000 (1st gen) | No | EBS only | ~10% cheaper than m5. Slightly lower clock speed. Good when you don't need Intel-specific instructions. |
| **m5d** | Intel Xeon Platinum (same as m5) | No | 1 × 75 GB NVMe SSD | m5 + local instance-store NVMe. Use for low-latency scratch/cache/temp storage. Data lost on stop/terminate. |
| **m5ad** | AMD EPYC 7000 | No | 1 × 75 GB NVMe SSD | m5a + local NVMe. Cheapest of the m5 variants with local SSD. |

Common across the m5 family: up to 10 Gbps network, EBS-optimized by default, Nitro system, supports ENA, encryption at rest for instance store on the `d` variants.

## Indicative on-demand pricing (us-east-1, RHEL)

RHEL adds a license uplift on top of the Linux base rate. For instances with ≤4 vCPUs, the RHEL uplift is roughly $0.06/hr flat. Treat these as a ballpark and verify on the AWS pricing page.

| Instance | vCPU | Memory | Local SSD | Approx RHEL $/hr (us-east-1) |
|---|---|---|---|---|
| t3.medium | 2 | 4 GiB | — | ~$0.10 |
| t3.large | 2 | 8 GiB | — | ~$0.14 |
| m5a.large | 2 | 8 GiB | — | ~$0.15 |
| m5.large | 2 | 8 GiB | — | ~$0.16 |
| m5ad.large | 2 | 8 GiB | 1×75 NVMe | ~$0.16 |
| m5d.large | 2 | 8 GiB | 1×75 NVMe | ~$0.17 |

Cheapest to most expensive: **t3 < m5a < m5 ≈ m5ad < m5d**.

## Picking one for RHEL

- **Dev/test, bursty CPU, low steady utilization** → `t3.medium` or `t3.large`. Cheapest, but watch credits under sustained load.
- **Steady general-purpose, cost-sensitive** → `m5a.large`. AMD, ~10% off m5.
- **Steady, want Intel** (AVX-512, specific ISV support) → `m5.large`.
- **Need fast local scratch/cache** (database temp, build artifacts, shuffle space) → `m5ad.large` (AMD) or `m5d.large` (Intel).

## RHEL licensing note

If bringing your own Red Hat subscriptions (RHEL BYOS / Cloud Access), the license uplift goes away and m5a savings become proportionally more meaningful. With AWS Marketplace RHEL the ~$0.06/hr uplift is the same regardless of family, compressing the relative price gaps.
