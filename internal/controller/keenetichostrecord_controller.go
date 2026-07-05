/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	keeneticv1alpha1 "github.com/Arbuzov/keenetic-operator/api/v1alpha1"
)

const hostRecordFinalizer = "keenetic.whitediver.com/finalizer"

// HostRecordManager — то подмножество *keenetic.Client, которое нужно реконсайлеру.
// Выделено в интерфейс, чтобы подменять роутер фейком в тестах.
type HostRecordManager interface {
	EnsureHost(ctx context.Context, host, ip string) error
	DeleteHost(ctx context.Context, host, ip string) error
	HasHost(ctx context.Context, host, ip string) (bool, error)
	CountHosts(ctx context.Context) (int, error)
}

// KeeneticHostRecordReconciler приводит роутер в соответствие с CR.
type KeeneticHostRecordReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Keenetic HostRecordManager
	// MaxHosts — страховка от лимита Keenetic в 64 записи `ip host`.
	MaxHosts int
}

//+kubebuilder:rbac:groups=keenetic.whitediver.com,resources=keenetichostrecords,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=keenetic.whitediver.com,resources=keenetichostrecords/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=keenetic.whitediver.com,resources=keenetichostrecords/finalizers,verbs=update

func (r *KeeneticHostRecordReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var rec keeneticv1alpha1.KeeneticHostRecord
	if err := r.Get(ctx, req.NamespacedName, &rec); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// --- удаление: finalizer снимает запись с роутера до того, как объект исчезнет ---
	if !rec.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&rec, hostRecordFinalizer) {
			if err := r.Keenetic.DeleteHost(ctx, rec.Spec.Hostname, rec.Spec.Address); err != nil {
				l.Error(err, "не удалось убрать ip host с роутера")
				return ctrl.Result{}, err // реквью, finalizer держим
			}
			controllerutil.RemoveFinalizer(&rec, hostRecordFinalizer)
			if err := r.Update(ctx, &rec); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// --- ставим finalizer ДО создания внешнего состояния ---
	if !controllerutil.ContainsFinalizer(&rec, hostRecordFinalizer) {
		controllerutil.AddFinalizer(&rec, hostRecordFinalizer)
		if err := r.Update(ctx, &rec); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil // событие update реквьюит нас заново
	}

	// --- гард на 64 записи ---
	count, err := r.Keenetic.CountHosts(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	exists, err := r.Keenetic.HasHost(ctx, rec.Spec.Hostname, rec.Spec.Address)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !exists && count >= r.MaxHosts {
		r.setCondition(&rec, "Ready", metav1.ConditionFalse, "LimitReached",
			fmt.Sprintf("на роутере уже %d/%d записей ip host", count, r.MaxHosts))
		rec.Status.Applied = false
		_ = r.Status().Update(ctx, &rec)
		return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
	}

	// --- желаемое состояние: запись присутствует (идемпотентно) ---
	if err := r.Keenetic.EnsureHost(ctx, rec.Spec.Hostname, rec.Spec.Address); err != nil {
		r.setCondition(&rec, "Ready", metav1.ConditionFalse, "ApplyFailed", err.Error())
		_ = r.Status().Update(ctx, &rec)
		return ctrl.Result{}, err
	}

	rec.Status.Applied = true
	rec.Status.ObservedGeneration = rec.Generation
	r.setCondition(&rec, "Ready", metav1.ConditionTrue, "Applied", "ip host присутствует на роутере")
	if err := r.Status().Update(ctx, &rec); err != nil {
		return ctrl.Result{}, err
	}

	// периодически переутверждаем — дрейф (кто-то снёс запись руками) сам залечится
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *KeeneticHostRecordReconciler) setCondition(rec *keeneticv1alpha1.KeeneticHostRecord,
	t string, s metav1.ConditionStatus, reason, msg string) {
	apimeta.SetStatusCondition(&rec.Status.Conditions, metav1.Condition{
		Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: rec.Generation,
	})
}

func (r *KeeneticHostRecordReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&keeneticv1alpha1.KeeneticHostRecord{}).
		Complete(r)
}
