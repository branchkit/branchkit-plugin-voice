package main

import (
	"encoding/json"
	"testing"
)

func micRecord(t *testing.T) map[string]any {
	t.Helper()
	var r map[string]any
	if err := json.Unmarshal([]byte(`{"stages":[{"stage":"_platform.mic"},{"stage":"_platform.stt","params":{"model":"m"}}]}`), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func micParams(r map[string]any) map[string]any {
	p, _ := r["stages"].([]any)[0].(map[string]any)["params"].(map[string]any)
	return p
}

func TestSetMicStageDevice(t *testing.T) {
	r := micRecord(t)
	if !setMicStageDevice(r, "USB-1", "Desk Mic") {
		t.Fatal("choosing a device should change the record")
	}
	p := micParams(r)
	if p["device"] != "USB-1" || p["device-name"] != "Desk Mic" {
		t.Fatalf("mic params = %v", p)
	}
	if other := r["stages"].([]any)[1].(map[string]any)["params"].(map[string]any); len(other) != 1 {
		t.Fatalf("a non-mic stage was touched: %v", other)
	}
	if setMicStageDevice(r, "USB-1", "Desk Mic") {
		t.Fatal("the same choice again should be no change, so no write")
	}
	if !setMicStageDevice(r, "", "") {
		t.Fatal("going back to the default should change the record")
	}
	if _, ok := r["stages"].([]any)[0].(map[string]any)["params"]; ok {
		t.Fatal("the default should leave the mic stage with no params")
	}
}
