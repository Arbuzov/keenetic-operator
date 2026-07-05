/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	keeneticv1alpha1 "github.com/Arbuzov/keenetic-operator/api/v1alpha1"
)

// IngressReconciler превращает хосты Ingress в дочерние KeeneticHostRecord.
type IngressReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// DefaultAddress — адрес, когда у Ingress нет LB-IP в status
	// (один общий nginx LB). Берётся из env DEFAULT_INGRESS_IP.
	DefaultAddress string
}

//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
//+kubebuilder:rbac:groups=keenetic.whitediver.com,resources=keenetichostrecords,verbs=get;list;watch;create;update;patch;delete

func (r *IngressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var ing networkingv1.Ingress
	if err := r.Get(ctx, req.NamespacedName, &ing); err != nil {
		// Ingress удалён: его дочерние записи соберёт GC по OwnerReference.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	addr := r.addressFor(&ing)
	if addr == "" {
		l.Info("для ingress пока нет адреса, ждём обновления status", "ingress", req.NamespacedName)
		return ctrl.Result{}, nil // вернёмся, когда обновится status.loadBalancer
	}

	// желаемые записи = по одной на уникальный хост Ingress
	desired := map[string]string{} // имя объекта -> hostname
	for _, rule := range ing.Spec.Rules {
		if rule.Host == "" {
			continue
		}
		host := strings.ToLower(rule.Host)
		desired[host] = host // имя CR == hostname (валидный DNS subdomain, точки разрешены)
	}

	// создаём/обновляем дочерние записи
	for name, host := range desired {
		rec := &keeneticv1alpha1.KeeneticHostRecord{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ing.Namespace},
		}
		op, err := controllerutil.CreateOrUpdate(ctx, r.Client, rec, func() error {
			rec.Spec.Hostname = host
			rec.Spec.Address = addr
			// OwnerReference -> удаление Ingress каскадно снесёт CR
			return controllerutil.SetControllerReference(&ing, rec, r.Scheme)
		})
		if err != nil {
			return ctrl.Result{}, err
		}
		if op != controllerutil.OperationResultNone {
			l.Info("сверили host record", "name", name, "op", op)
		}
	}

	// чистим наши записи, которые больше не нужны (хост убрали из Ingress)
	var owned keeneticv1alpha1.KeeneticHostRecordList
	if err := r.List(ctx, &owned, client.InNamespace(ing.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	for i := range owned.Items {
		rec := &owned.Items[i]
		if !metav1.IsControlledBy(rec, &ing) {
			continue
		}
		if _, keep := desired[rec.Name]; !keep {
			if err := r.Delete(ctx, rec); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	}

	return ctrl.Result{}, nil
}

func (r *IngressReconciler) addressFor(ing *networkingv1.Ingress) string {
	for _, lb := range ing.Status.LoadBalancer.Ingress {
		if lb.IP != "" {
			return lb.IP
		}
	}
	return r.DefaultAddress
}

func (r *IngressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1.Ingress{}).
		Owns(&keeneticv1alpha1.KeeneticHostRecord{}). // реагируем и на изменения дочерних CR
		Complete(r)
}
