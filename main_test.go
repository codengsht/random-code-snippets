package main

import (
	"reflect"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func TestAlertStatusMetricTagsAreStableAcrossLifecycleUpdates(t *testing.T) {
	event := events.CloudWatchEvent{AccountID: "123456789012"}
	open := HealthEventDetail{
		EventArn:          "event-id-abc",
		Service:           "EC2",
		EventTypeCode:     "AWS_EC2_OPERATIONAL_ISSUE",
		EventTypeCategory: "issue",
		EventScopeCode:    "ACCOUNT_SPECIFIC",
		StatusCode:        "open",
		EventRegion:       "us-east-1",
		CommunicationID:   "communication-open",
	}
	closed := open
	closed.StatusCode = "closed"
	closed.CommunicationID = "communication-closed"

	openTags := alertStatusMetricTags(event, open)
	closedTags := alertStatusMetricTags(event, closed)
	if !reflect.DeepEqual(openTags, closedTags) {
		t.Fatalf("alert status tags changed across lifecycle updates:\nopen:   %v\nclosed: %v", openTags, closedTags)
	}

	want := []string{
		"receiving_account:123456789012",
		"event_arn:event-id-abc",
		"aws_service:EC2",
		"affected_region:us-east-1",
		"event_type_code:AWS_EC2_OPERATIONAL_ISSUE",
		"event_scope_code:ACCOUNT_SPECIFIC",
	}
	if !reflect.DeepEqual(openTags, want) {
		t.Fatalf("unexpected alert status tags:\ngot:  %v\nwant: %v", openTags, want)
	}
}

func TestDeliveryMetricTagsIncludeUpdateContext(t *testing.T) {
	event := events.CloudWatchEvent{AccountID: "123456789012"}
	detail := HealthEventDetail{
		EventArn:          "event-id-abc",
		Service:           "EC2",
		EventTypeCode:     "AWS_EC2_OPERATIONAL_ISSUE",
		EventTypeCategory: "issue",
		EventScopeCode:    "ACCOUNT_SPECIFIC",
		StatusCode:        "closed",
		EventRegion:       "us-east-1",
		CommunicationID:   "communication-closed",
	}

	want := []string{
		"receiving_account:123456789012",
		"event_arn:event-id-abc",
		"aws_service:EC2",
		"affected_region:us-east-1",
		"event_type_code:AWS_EC2_OPERATIONAL_ISSUE",
		"event_scope_code:ACCOUNT_SPECIFIC",
		"event_type_category:issue",
		"status_code:closed",
		"communication_id:communication-closed",
	}
	if got := deliveryMetricTags(event, detail); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected delivery tags:\ngot:  %v\nwant: %v", got, want)
	}
}
