package main

import (
	"encoding/json"
	"testing"
)

func TestPrometheusMetricsCanBeEncodedAsJSON(t *testing.T) {
	metrics := parsePrometheusMetrics([]byte("# HELP requests total\nrequests_total 42\ntemperature{room=\"lab\"} 21.5\n"))
	body, err := json.Marshal(metrics)
	if err != nil {
		t.Fatal(err)
	}

	var decoded map[string]string
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("encoded metrics are not JSON: %v", err)
	}
	if decoded["requests_total"] != "42" {
		t.Fatalf("requests_total = %q, want 42", decoded["requests_total"])
	}
}
