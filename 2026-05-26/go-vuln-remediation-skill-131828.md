---
trigger: manual
description: Guides the agent through remediating vulnerable Go module dependencies from a GitLab Gemnasium report.
---

# Go Vulnerability Remediation

Remediate vulnerable Go dependencies identified in a GitLab Gemnasium dependency scanning report. This skill assumes a single Go module at the workspace root.


---

## 1. Preflight

### 1a. Locate the report

Find `gl-dependency-scanning-report.json` at the workspace root. If not found, ask the user for the path. If still unavailable, halt.

### 1b. Discover module root

The `go.mod` file will be at the workspace root. Confirm it exists. If it does not, halt and inform the user.

### 1c. Check working tree cleanliness

Run `git status --short -- go.mod go.sum` in the module root.

- If **both files are clean**: record this — safe rollback via `git restore --source=HEAD` is available.
- If **either file is dirty**: halt and inform the user that remediation requires a clean `go.mod` and `go.sum`. Do not proceed. Do not offer to continue with risk of data loss.

### 1d. Record environment

Capture `go env GOVERSION` and `go version`. Note any `GOPRIVATE`, `GONOSUMCHECK`, or `GONOSUMDB` settings that may affect module resolution.

---

## 2. Parse and Group Findings

Parse the report JSON. Extract from each `vulnerabilities[]` entry:

- `id` — CVE or scanner identifier
- `severity` — Critical / High / Medium / Low
- `location.dependency.package.name` — Go module path
- `location.dependency.version` — current version

### No-fix-available filter

Each vulnerability entry has a `solution` key. If its value is `"there is no solution available yet"` (or similar wording indicating no fix exists), skip that entry entirely — do not attempt remediation. Record it in the final report as skipped with reason "no fix available."

### Fixed version resolution

For entries that pass the no-fix filter, try these sources in order — do not browse external URLs unless the user approves:

1. `remediations[].fixes[].id` matched back to a `diff` or version string in `remediations[].summary`
2. `solution` field (often contains "Upgrade to X.Y.Z or later")
3. `details` or `links[]` fields if they contain structured version data
4. `identifiers[]` — look for advisory IDs that embed version info

If no fixed version can be determined from local report fields, mark the finding as **manual review required** and skip automated upgrade. Do not fetch external advisory URLs unless the user explicitly approves network access for this purpose.

### Deduplication

Group findings by module path. For each module:
- Collect all CVE IDs.
- Select the highest minimum fixed version using Go module version ordering. Use `go list -m -versions <module>` to resolve ordering when versions include pseudo-versions or pre-releases — do not rely on naive string/semver sorting.
- Remediate each module exactly once.

Skip entries missing required fields (module name, version, severity) with a warning.

If no actionable findings remain after grouping, inform the user and end.

---

## 3. Classify Dependency Path

For each grouped module:

1. Check `go.mod` require directives:
   - Listed without `// indirect` → direct.
   - Listed with `// indirect` → indirect (recorded in module graph but not directly imported).
   - Not listed at all → transitive (pulled by another dependency).

2. Run `go mod why -m <module>` to explain why the module is needed. If the output is inconclusive or does not show the full parent chain, fall back to `go mod graph` and trace the path from a direct dependency to the vulnerable module.

3. Present the dependency path to the user for indirect/transitive dependencies so they understand the impact.

4. If classification is ambiguous (e.g., the module appears in a `replace` directive or `go mod why` gives multiple paths), say so rather than forcing a label.

---

## 4. Approval

Apply the severity policy:

| Severity | Default action |
|----------|---------------|
| Critical / High | Auto-proceed — inform the user, no prompt needed. |
| Medium / Low | Present details (module, current version, fixed version, CVEs, dependency path) and wait for explicit approval. |

If the user declines, record the module as skipped and move on.

---

## 5. Upgrade (per module)

For each approved module, one at a time:

### 5a. Edge-case checks before upgrading

- If the module has a `replace` directive in `go.mod`, warn the user that automatic upgrade may not apply and ask how to proceed.
- If the module uses a major-version path (e.g., `/v2`), ensure the upgrade target matches the major version.
- If the fixed version is a retracted version, skip and report.
- If the module is `stdlib` or `toolchain`, inform the user that `go get` cannot fix it — a Go toolchain upgrade is needed.
- If the module is private and `GOPRIVATE`/network settings may block resolution, warn before attempting.

### 5b. First attempt — solution-suggested version

If the `solution` field from the report recommends a specific version (e.g., "Upgrade to 1.2.4"), attempt that version first:

```
go get <module>@<solution_version>
```

If the solution does not specify a concrete version, fall back to `@latest`:

```
go get <module>@latest
```

### 5c. Validate

```
go mod tidy
go test ./...
command -v govulncheck
```

Include `go build ./...` if the project has build-only packages with no test files.

Any non-zero exit code means the attempt failed.

### 5d. On failure — rollback

Since preflight confirmed `go.mod` and `go.sum` were clean (step 1c halts otherwise):
```
git restore --source=HEAD -- go.mod go.sum
```

### 5e. Second attempt — fallback

If the first attempt used the solution-suggested version, try `@latest`. If the first attempt was already `@latest`, try the minimum fixed version from the grouped findings:

```
go get <module>@<fallback_version>
```

Validate again (same as 5c). On failure, rollback (same as 5d), record the module as failed with the reason, and continue to the next module.

### 5f. On success

Record: module, previous version, new version, all associated CVE IDs.

---

## 6. Post-Remediation Validation

After all modules are processed:

1. Run `go mod tidy && go test ./...` on the combined final state.
2. If `govulncheck` is available (`which govulncheck`), run `govulncheck ./...` and report whether the remediated CVEs are resolved. If not available, note this in the report.
3. If final validation fails, do **not** commit or stage. Report the failure and leave the workspace as-is for the user to investigate.

---

## 7. Report Results

Present a summary covering every module from the grouped findings:

- Module path, previous version, new version (or "—"), status (upgraded / failed / skipped / manual review), associated CVEs.
- For failures, include the reason (build error, test failure, etc.).
- If `govulncheck` ran, note whether it confirmed remediation.

---

## 8. Commit (only when requested)

**Do not auto-commit unless the user explicitly asked for commits.**

If the user requested commits and there is at least one successful upgrade:

1. Stage only `go.mod` and `go.sum`.
2. Commit message format:
   ```
   fix(deps): remediate Go module vulnerabilities

   - <module>: <old> → <new> (<CVE-1>, <CVE-2>)
   - <module>: <old> → <new> (<CVE>)
   ```

If the user did **not** request commits:
- Leave changes in the working tree (staged or unstaged per user preference).
- Provide the commit message so the user can commit manually.

---

## Edge Cases Reference

| Situation | Handling |
|-----------|----------|
| `replace` directive on vulnerable module | Warn user; do not auto-upgrade without confirmation |
| `exclude` directive blocking fixed version | Report conflict; ask user |
| Retracted target version | Skip; report as manual review |
| Major version path (`/v2`, `/v3`) | Ensure upgrade stays on same major |
| Private module / network error | Report clearly; do not retry silently |
| Stdlib/toolchain vulnerability | Cannot fix with `go get`; advise toolchain upgrade |
| Pseudo-version in use | Use `go list -m -versions` for ordering; upgrade normally |
| Pre-existing dirty `go.mod`/`go.sum` | Halt in preflight; do not proceed |
