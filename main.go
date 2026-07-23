// Package main is the entry point for the aws-health-event-forwarder AWS Lambda
// function. It receives AWS Health events (forwarded via EventBridge) and
// emits corresponding metrics to Datadog.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	ddlambda "github.com/DataDog/dd-trace-go/contrib/aws/datadog-lambda-go/v2"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

// healthEventTimeFormat is the RFC2822-style format AWS Health uses for time fields.
const healthEventTimeFormat = "Mon, 2 Jan 2006 15:04:05 GMT"

// statusCodeClosed is the AWS Health event statusCode value indicating a
// resolved/closed issue.
const statusCodeClosed = "closed"

// HealthEventDetail maps the fields from an AWS Health event detail payload.
type HealthEventDetail struct {
	EventArn          string `json:"eventArn"`
	Service           string `json:"service"`
	EventTypeCode     string `json:"eventTypeCode"`
	EventTypeCategory string `json:"eventTypeCategory"`
	EventScopeCode    string `json:"eventScopeCode"`
	StatusCode        string `json:"statusCode"`
	StartTime         string `json:"startTime"`
	EndTime           string `json:"endTime"`
	EventRegion       string `json:"eventRegion"`
	CommunicationID   string `json:"communicationId"`
	EventDescription  []struct {
		Language          string `json:"language"`
		LatestDescription string `json:"latestDescription"`
	} `json:"eventDescription"`
}

// eventArnTagValue trims an AWS Health eventArn down to the event-type-code
// and unique event_id portion (everything after the second "/"), keeping the
// tag value well under Datadog's 200-character tag limit. For example:
//
//	arn:aws:health:af-south-1::event/EC2/AWS_EC2_OPERATIONAL_ISSUE/AWS_EC2_OPERATIONAL_ISSUE_7f35c8ae-af1f-54e6-a526-d0179ed6d68f
//
// becomes:
//
//	AWS_EC2_OPERATIONAL_ISSUE/AWS_EC2_OPERATIONAL_ISSUE_7f35c8ae-af1f-54e6-a526-d0179ed6d68f
func eventArnTagValue(eventArn string) string {
	parts := strings.SplitN(eventArn, "/", 3)
	if len(parts) < 3 {
		return eventArn
	}
	return parts[2]
}

// alertStatusMetricTags returns the stable identity dimensions for an AWS
// Health event. These values must remain identical across lifecycle updates so
// that an open value of 1 and a closed value of 0 are written to the same
// Datadog metric series.
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

// receivedMetricTags returns the dimensions for the delivery-count metric.
// status_code is included so open vs. resolved deliveries can be distinguished,
// while event_arn and communication_id are intentionally excluded: they are
// unique per event/update and would explode metric cardinality (and cost)
// without adding value to an aggregated count.
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

// durationMetricTags returns low-cardinality dimensions for the outage-duration
// distribution. status_code is omitted (it is always "closed" here), and
// event_arn / communication_id are omitted to avoid unbounded cardinality;
// distribution aggregation still yields accurate avg/p99/max without per-event
// identity.
func durationMetricTags(event events.CloudWatchEvent, detail HealthEventDetail) []string {
	return []string{
		"receiving_account:" + event.AccountID,
		"aws_service:" + detail.Service,
		"affected_region:" + detail.EventRegion,
		"event_type_code:" + detail.EventTypeCode,
	}
}

func handleRequest(ctx context.Context, event events.CloudWatchEvent) error {
	log.Printf("Received AWS Health event: source=%s detail-type=%s region=%s",
		event.Source, event.DetailType, event.Region)

	var detail HealthEventDetail
	if err := json.Unmarshal(event.Detail, &detail); err != nil {
		return fmt.Errorf("failed to unmarshal health event detail: %w", err)
	}

	statusTags := alertStatusMetricTags(event, detail)

	// Send one distribution sample for every event delivery received.
	ddlambda.Metric("aws.health.issue.received", 1, receivedMetricTags(event, detail)...)

	// Emit the last observed alert status: 1 = active issue, 0 = resolved. Stable
	// identity tags ensure open and closed updates target the same metric series.
	alertStatus := 1.0
	if detail.StatusCode == statusCodeClosed {
		alertStatus = 0.0
	}
	ddlambda.Metric("aws.health.issue.alert_status", alertStatus, statusTags...)
	log.Printf("Alert status: %.0f (statusCode=%s)", alertStatus, detail.StatusCode)

	// On resolved events, compute and emit outage duration in seconds.
	if detail.StatusCode == statusCodeClosed && detail.StartTime != "" && detail.EndTime != "" {
		startTime, err := time.Parse(healthEventTimeFormat, detail.StartTime)
		if err != nil {
			log.Printf("Warning: could not parse startTime %q: %v", detail.StartTime, err)
		}

		endTime, err := time.Parse(healthEventTimeFormat, detail.EndTime)
		if err != nil {
			log.Printf("Warning: could not parse endTime %q: %v", detail.EndTime, err)
		}

		if !startTime.IsZero() && !endTime.IsZero() {
			durationSeconds := endTime.Sub(startTime).Seconds()
			// Distribution preserves all samples within a rollup window, enabling
			// accurate sum/avg/p99/count queries even when multiple events close
			// simultaneously — unlike a gauge which keeps only the last value.
			ddlambda.Metric("aws.health.issue.duration_seconds", durationSeconds, durationMetricTags(event, detail)...)
			log.Printf("Outage duration: %.0f seconds (%.2f hours)", durationSeconds, durationSeconds/3600)
		}
	}

	log.Printf("Forwarded health event to Datadog: service=%s type=%s status=%s",
		detail.Service, detail.EventTypeCode, detail.StatusCode)

	return nil
}

func main() {
	lambda.Start(ddlambda.WrapFunction(handleRequest, nil))
}
