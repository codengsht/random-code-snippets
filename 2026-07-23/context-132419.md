# Session Context — 2026-07-23

## Goal
Understand how to send Datadog metrics from Go, whether metrics are aggregated,
and redesign the metric tags for an AWS Health event forwarder Lambda.

## Approach
- Reviewed the existing Lambda (`main.go`) which uses
  `ddlambda.Metric(...)` from `datadog-lambda-go`.
- Explained the three ways to send Datadog metrics from Go (Lambda extension,
  DogStatsD, HTTP API) and how each metric type aggregates.
- Evaluated a proposed tag redesign and flagged two blocking problems, then
  agreed on a final tag set per metric.
- Implemented the tag changes and updated tests; `go build` + `go test` pass.

## Outcome
Split the single `deliveryMetricTags` helper into two purpose-built helpers:
- `receivedMetricTags` — delivery-count dimensions, drops unbounded IDs.
- `durationMetricTags` — low-cardinality dimensions for the duration distribution.
`alertStatusMetricTags` left unchanged (stable per-event identity).

## Caveats
- `ddlambda.Metric()` only submits **distribution** metrics — you cannot send a
  true GAUGE from Lambda with this library. Query `alert_status` with `max`/`min`.
- Never tag `alert_status` with `status_code`: it splits the open (1) and closed
  (0) values onto different series and breaks the 1→0 transition.
- Datadog bills on unique tag combinations; `event_arn` and `communication_id`
  are effectively unbounded, so keep them off aggregated count/duration metrics.
