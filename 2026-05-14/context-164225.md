# Terraform: Reject gp2 Volumes in EC2 Module

## Goal
Add a validation to a `volumes` variable in a Terraform EC2 module to reject any
volume entry whose `type` is `gp2`. Surface a clear error message indicating
that gp2 is being deprecated in favor of gp3.

## Context
- The variable lives in another project (an EC2 Terraform module), not this
  repo. The variable was shown as a screenshot.
- `var.volumes` is typed `list(map(any))`, where each map represents a volume
  with keys like `size`, `iops`, and `type`.
- Valid `type` values per the variable description: `io1`, `gp3`, `st1`, `sc1`,
  `standard`. `gp2` is not in that list and should be rejected explicitly.

## Approach
Use a Terraform `validation` block inside the `variable "volumes"` definition.

- `alltrue([...])` is true only when every iterated element is true, so a single
  gp2 entry fails the whole list.
- A `for` expression walks each volume map and reads its `type` key with
  `lookup(v, "type", "")` so a missing key does not false-positive.
- `lower(...)` makes the comparison case-insensitive (catches `GP2`, `Gp2`,
  etc.).
- `error_message` returns a user-friendly message pointing to gp3.

## Snippet
See `volumes-validation.tf` for the copyable HCL block.

## Caveats
- Requires Terraform `0.13+` for `validation` blocks.
- Terraform `1.9+` is only needed if you later cross-reference other variables
  inside the condition, not required for this check.
- This validation does not enforce that `type` is set or that it is one of the
  documented valid values. If that is desired, add a second validation block
  rather than expanding this one.
- Run `terraform validate` (or `terraform plan`) against a config that calls the
  module with a sample `volumes` input to confirm the message fires.
