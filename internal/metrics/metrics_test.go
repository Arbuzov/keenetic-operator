/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package metrics

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestObserveRouterOp pins the label contract the alerts are written against:
// a nil error must land in result="success", a non-nil one in result="error",
// and either way the duration histogram must get the observation. The pointer
// indirection is the subtle part — defer evaluates its arguments immediately,
// so taking the error by value here would always record "success".
func TestObserveRouterOp(t *testing.T) {
	RouterOperations.Reset()
	RouterOperationDuration.Reset()

	var ok error
	ObserveRouterOp(OpCount, time.Now(), &ok)

	boom := errors.New("router unreachable")
	ObserveRouterOp(OpCount, time.Now(), &boom)

	if got := testutil.ToFloat64(RouterOperations.WithLabelValues(OpCount, "success")); got != 1 {
		t.Errorf("operations{result=success} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(RouterOperations.WithLabelValues(OpCount, "error")); got != 1 {
		t.Errorf("operations{result=error} = %v, want 1", got)
	}
	if got := testutil.CollectAndCount(RouterOperationDuration); got != 1 {
		t.Errorf("duration series = %d, want 1 (one per operation label)", got)
	}
}

// A nil pointer must not panic: it is the "no error slot" case, not a failure.
func TestObserveRouterOpNilError(t *testing.T) {
	RouterOperations.Reset()
	RouterOperationDuration.Reset()

	ObserveRouterOp(OpHas, time.Now(), nil)

	if got := testutil.ToFloat64(RouterOperations.WithLabelValues(OpHas, "success")); got != 1 {
		t.Errorf("operations{result=success} = %v, want 1", got)
	}
}
