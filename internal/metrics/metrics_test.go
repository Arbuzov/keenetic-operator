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

// observedCall mirrors the production call shape exactly: the defer is
// registered up front and err is assigned afterwards.
func observedCall(fail bool) (err error) {
	defer ObserveRouterOp(OpEnsure, time.Now(), &err)
	if fail {
		err = errors.New("router unreachable")
	}
	return err
}

// Calling ObserveRouterOp directly proves the labels; this proves the pattern
// the doc comment prescribes. Taking the error by value instead of by pointer
// still compiles and still passes the direct test — but every call would be
// recorded as a success, because defer snapshots its arguments at the point it
// is registered, which is before err is ever assigned.
func TestObserveRouterOpThroughDeferredNamedReturn(t *testing.T) {
	RouterOperations.Reset()
	RouterOperationDuration.Reset()

	if err := observedCall(false); err != nil {
		t.Fatalf("observedCall(false) = %v, want nil", err)
	}
	if err := observedCall(true); err == nil {
		t.Fatal("observedCall(true) = nil, want an error")
	}

	if got := testutil.ToFloat64(RouterOperations.WithLabelValues(OpEnsure, "success")); got != 1 {
		t.Errorf("operations{result=success} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(RouterOperations.WithLabelValues(OpEnsure, "error")); got != 1 {
		t.Errorf("operations{result=error} = %v, want 1 — a by-value argument would report success here", got)
	}
}
