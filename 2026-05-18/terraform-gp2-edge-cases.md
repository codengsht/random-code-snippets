# Catching gp2 in lists, maps, and tfvars interpolation

Pure regex can't safely parse nested HCL — these cases need either looser regex
(more false positives) or post-processing.

## 1. Lists: `default = ["gp2"]`

Same-line list literals:

```yaml
- find: '(\[[^\]\n]*?)"gp2"([^\]\n]*?\])'
  replace: '$1"gp3"$2'
  regexEnabled: true
  searchPerLine: true
```

Handles `["gp2"]`, `["gp2", "gp3"]`, `["other", "gp2"]`. If `gp2` appears twice
in the same brackets, only the first match per pass is caught — run twice if
that's a real case.

For lists split across lines, turn off per-line scanning:

```yaml
- find: '(\[[^\]]*?)"gp2"([^\]]*?\])'
  replace: '$1"gp3"$2'
  regexEnabled: true
  searchPerLine: false
```

## 2. Maps: `default = { type = "gp2" }`

Already covered by the existing `\btype\s*=\s*"gp2"` pattern when on a single
line — the inner `type = "gp2"` matches regardless of being inside braces.

For multi-line map blocks, the same per-attribute patterns still work line-by-line:

```hcl
default = {
  type = "gp2"   # ← existing pattern matches this line
  size = 100
}
```

## 3. `.tfvars` interpolation

Can't be solved with the resource-attribute approach — the variable name in
`.tfvars` is whatever the user chose. But `.tfvars` files are simple `name = value`
pairs. So in `.tfvars` files specifically, **any** variable assigned to `"gp2"`
is almost certainly an EBS volume type.

Add a separate scan rule scoped to tfvars:

```yaml
- find: '^(\s*[A-Za-z_][\w-]*\s*=\s*)"gp2"'
  replace: '$1"gp3"'
  regexEnabled: true
  searchPerLine: true
  fileInclude:
    - ".tfvars"
```

If the tool doesn't support per-rule `fileInclude`, split into a second config
file/job that only scans `.tfvars`.

JSON variant:

```yaml
- find: '("[A-Za-z_][\w-]*"\s*:\s*)"gp2"'
  replace: '$1"gp3"'
  regexEnabled: true
  searchPerLine: true
  fileInclude:
    - ".tfvars.json"
```

## 4. Final flag-and-review pass

After bulk replace, surface anything that slipped through:

```bash
grep -rn '"gp2"' \
  --include='*.tf' \
  --include='*.tfvars' \
  --include='*.tfvars.json' \
  --include='*.hcl' \
  .
```

Anything still showing up is either: a legitimately-needed gp2 reference, a
pattern that wasn't covered, or inside a comment/string literal.

## When regex stops being the right tool

For nested structures, dynamic blocks, or `for_each` constructs:

- **HCL-aware**: small Python script using `python-hcl2` parses the AST and lets
  you mutate by attribute path.
- **Terraform-native**: run `terraform plan` with the change applied, then diff.
  Or let `tflint` / `checkov` flag remaining gp2 usage as a policy rule.

For most repos, layered regex + final grep covers 95%+ without the engineering
overhead. Pull out a parser only for hundreds of edge cases or a recurring
migration pattern.
