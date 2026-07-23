// Datadog metric tag helpers for the AWS Health event forwarder Lambda.
// Design rules:
//   - alertStatusMetricTags: stable per-event identity ONLY. The value (1=open,
//     0=closed) must land on the same series across lifecycle updates, so never
//     add status_code / communication_id / anything update-specific here.
//   - receivedMetricTags: delivery-count dimensions. status_code is useful, but
//     drop unbounded IDs (event_arn, communication_id) to control cardinality/cost.
//   - durationMetricTags: low-cardinality only; distribution aggregation still
//     yields accurate avg/p99/max without per-event identity.

// alertStatusMetricTags returns the stable identity dimensions for an event.
func alertStatusMetricTags(event events.CloudWatchEvent, detail HealthEventDetail) []string {
	return []string{
		"receiving_account:" + event.AccountID,
		"event_arn:" + eventArnTagValue(detail.EventArn),
		"aws_service:" + detail.Service,
		"affected_region:" + detail.EventRegion,
		"event_type_code:" + detail.EventTypeCode,
		"event_scope_code:" + detail.EventScopeCode,
	}
}

// receivedMetricTags returns dimensions for the delivery-count metric.
func receivedMetricTags(event events.CloudWatchEvent, detail HealthEventDetail) []string {
	return []string{
		"receiving_account:" + event.AccountID,
		"aws_service:" + detail.Service,
		"affected_region:" + detail.EventRegion,
		"event_type_code:" + detail.EventTypeCode,
		"event_type_category:" + detail.EventTypeCategory,
		"status_code:" + detail.StatusCode,
	}
}

// durationMetricTags returns low-cardinality dimensions for the duration distribution.
func durationMetricTags(event events.CloudWatchEvent, detail HealthEventDetail) []string {
	return []string{
		"receiving_account:" + event.AccountID,
		"aws_service:" + detail.Service,
		"affected_region:" + detail.EventRegion,
		"event_type_code:" + detail.EventTypeCode,
	}
}
