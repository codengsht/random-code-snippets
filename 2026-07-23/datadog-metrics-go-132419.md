# Sending Datadog Metrics from Go + Aggregation Notes

## Three ways to send metrics

### 1. Lambda — datadog-lambda-go (custom metrics)
```go
import ddlambda "github.com/DataDog/dd-trace-go/contrib/aws/datadog-lambda-go/v2"

ddlambda.Metric("aws.health.issue.received", 1, "tag1:value1", "tag2:value2")
```
Metrics are automatically submitted as **distributions** via the Datadog Lambda
extension. No standalone agent required. There is no gauge/count option here.

### 2. DogStatsD — when an agent is available (EC2/ECS/long-running services)
```go
import "github.com/DataDog/datadog-go/v5/statsd"

client, _ := statsd.New("127.0.0.1:8125")
defer client.Close()

client.Gauge("myapp.temperature", 22.5, []string{"room:kitchen"}, 1)
client.Count("myapp.requests", 1, []string{"endpoint:/health"}, 1)
client.Histogram("myapp.latency", 120, []string{"route:home"}, 1)
```

### 3. HTTP API — datadog-api-client-go (no agent, last resort)
Direct POST to Datadog's API; adds network latency and rate limits.

## Are metrics aggregated or sent individually?
Not one HTTP request per call. The client aggregates over a rollup window
(~10s) then sends in a batch. How it aggregates depends on the metric TYPE:

| Type          | Aggregation behavior                                            |
|---------------|----------------------------------------------------------------|
| Count         | Values summed within the rollup window (5+3 = 8)               |
| Gauge         | Only the last value is kept                                    |
| Histogram     | Converted locally to stats (avg, max, count, p95, ...)         |
| Distribution  | Raw samples preserved; aggregation happens server-side (Datadog)|

Distributions let you graph avg/sum/max/min/count and p50/p75/p95/p99 at query
time. This is why they suit `duration_seconds` and `received` from Lambda.
