# Terraform gp2 → gp3 Regex Review

Review of a scanList config for migrating EBS volume types from gp2 to gp3 across Terraform files.

## Original config

```yaml
scanList:
  # HCL: type = "gp2"  - module calls, resource blocks, .tfvars, .auto.tfvars
  - find: '(type\s*=\s*)"gp2"'
    replace: '$1"gp3"'
    regexEnabled: true
    searchPerLine: true

  # HCL variable default: default = "gp2"  - variables.tf
  - find: '(default\s*=\s*)"gp2"'
    replace: '$1"gp3"'
    regexEnabled: true
    searchPerLine: true

  # JSON: "type": "gp2"  - .tfvars.json, .auto.tfvars.json
  - find: '("type"\s*:\s*)"gp2"'
    replace: '$1"gp3"'
    regexEnabled: true
    searchPerLine: true

fileInclude:
  - ".tf"
  - ".tfvars"
  - ".tfvars.json"

fileExclude:
  - ".terraform.lock.hcl"
  - ".terraform/"
  - ".md"
  - "CHANGELOG"

scanHiddenDirectories: false
```

## What's covered

- `type = "gp2"` in HCL — matches `aws_ebs_volume.type`
- `default = "gp2"` — variable defaults
- `"type": "gp2"` in JSON

## What's missing (the important part)

`type` is the **less common** attribute name for EBS volume type. The common ones are:

| Attribute | Resources where it appears |
|---|---|
| `volume_type` | `aws_instance` (root_block_device, ebs_block_device), `aws_launch_template`, `aws_launch_configuration`, `aws_autoscaling_group`, `aws_spot_fleet_request` |
| `storage_type` | `aws_db_instance`, `aws_rds_cluster_instance` |
| `type` | `aws_ebs_volume` only |

## Suggested additional patterns

```yaml
# HCL: volume_type = "gp2"
- find: '(\bvolume_type\s*=\s*)"gp2"'
  replace: '$1"gp3"'
  regexEnabled: true
  searchPerLine: true

# HCL: storage_type = "gp2"  (RDS)
- find: '(\bstorage_type\s*=\s*)"gp2"'
  replace: '$1"gp3"'
  regexEnabled: true
  searchPerLine: true

# JSON: "volume_type": "gp2"
- find: '("volume_type"\s*:\s*)"gp2"'
  replace: '$1"gp3"'
  regexEnabled: true
  searchPerLine: true

# JSON: "storage_type": "gp2"
- find: '("storage_type"\s*:\s*)"gp2"'
  replace: '$1"gp3"'
  regexEnabled: true
  searchPerLine: true
```

## Tighten existing patterns with word boundaries

```yaml
- find: '(\btype\s*=\s*)"gp2"'
- find: '(\bdefault\s*=\s*)"gp2"'
```

Without `\b`, something like `mytype = "gp2"` would also be rewritten.

## False-positive flags

- `default = "gp2"` matches anything with that literal — variable defaults, locals, etc. Right-hand side is pinned to `"gp2"` so blast radius is small.
- Patterns rewrite `gp2` inside commented lines (`# type = "gp2"`).

## fileExclude review

- `.md` and `CHANGELOG` are redundant given fileInclude only lists `.tf`, `.tfvars`, `.tfvars.json`. Harmless noise.
- Consider adding: `.terragrunt-cache/`, `examples/` (if you don't want demo code rewritten), `vendor/` or generated dirs.

## What regex won't catch

- gp2 inside list/map defaults: `default = ["gp2"]`, `default = { type = "gp2" }`
- gp2 via interpolation: `var.volume_type` where the value is `gp2` in `.tfvars`
- gp2 in module input blocks where the argument name isn't `type`/`default`/`volume_type`/`storage_type`

After the bulk replace, run a final sweep:

```bash
grep -rn '"gp2"' --include='*.tf' --include='*.tfvars' --include='*.tfvars.json' .
```

## gp2 → gp3 migration note

gp3 has independent IOPS and throughput knobs. If the original gp2 volume was >334 GiB it had >1000 IOPS baseline; gp3 starts at 3000 IOPS / 125 MB/s baseline, which is usually fine. Worth a glance for any volume tuned beyond defaults.
