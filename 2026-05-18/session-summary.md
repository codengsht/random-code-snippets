# Session Summary — 2026-05-18

## Topics covered

### 1. EC2 instance type comparison for RHEL
Compared t3, m5, m5a, m5d, m5ad at 2 vCPU tier for RHEL workloads. See
`ec2-rhel-general-purpose-comparison.md`.

Key takeaway: **t3 < m5a < m5 ≈ m5ad < m5d** (cheapest to most expensive).
2 vCPU / 4 GiB only available as `t3.medium`.

### 2. Terraform gp2 → gp3 regex review
Reviewed a scanList config for migrating EBS volume types. See
`terraform-gp2-to-gp3-regex-review.md` and `terraform-gp2-edge-cases.md`.

Main gap: original config only matched `type = "gp2"` (rare attribute) — was
missing `volume_type` (common, used in `aws_instance`, `aws_launch_template`,
`aws_autoscaling_group`, etc.) and `storage_type` (RDS).

### 3. regex-scanner Go CLI
Built a single-file Go CLI using `github.com/dlclark/regexp2 v1.11.5`. Iterated
through three output modes:

- **Default**: `file:line:col [pattern] match` lines (pipeline-friendly)
- **`-table`**: ASCII table per match + pattern hit summary
- **`-json`**: NDJSON
- **`-html FILE`**: composes with any of the above; writes self-contained
  HTML report with stat cards, summary table, per-file groupings, and a
  client-side filter

See `regex-scanner.go`, `regex-scanner-go.mod`, `regex-scanner-readme.md`.

## Notable design decisions

- regexp2's `MatchTimeout` set per-pattern (2s default) to mitigate catastrophic
  backtracking — the price of having lookarounds/backrefs vs RE2.
- Worker pool for concurrent file scanning, NumCPU default.
- HTML report uses `html/template` (auto-escaping) and atomic temp-file rename.
- Self-contained HTML — no external CSS/JS/fonts — so it works offline and as
  an attachment.
