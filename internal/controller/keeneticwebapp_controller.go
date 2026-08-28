/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	keeneticv1alpha1 "github.com/Arbuzov/keenetic-operator/api/v1alpha1"
	"github.com/Arbuzov/keenetic-operator/internal/keenetic"
)

// webAppFinalizer — свой, а не общий с host record: объекты разных типов,
// и общая строка означала бы, что по логу «кто держит объект» не отличить
// незакрытую уборку публикации от незакрытой уборки DNS-записи.
const webAppFinalizer = "keenetic.whitediver.com/webapp-finalizer"

// WebAppManager — то подмножество *keenetic.Client, которое нужно реконсайлеру.
// Выделено в интерфейс, чтобы подменять роутер фейком в тестах.
type WebAppManager interface {
	EnsureProxy(ctx context.Context, p keenetic.Proxy) error
	DeleteProxy(ctx context.Context, name string) error
}

// KeeneticWebAppReconciler приводит публикации на роутере в соответствие с CR.
type KeeneticWebAppReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Keenetic WebAppManager
}

//+kubebuilder:rbac:groups=keenetic.whitediver.com,resources=keeneticwebapps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=keenetic.whitediver.com,resources=keeneticwebapps/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=keenetic.whitediver.com,resources=keeneticwebapps/finalizers,verbs=update

func (r *KeeneticWebAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var app keeneticv1alpha1.KeeneticWebApp
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// --- удаление: finalizer снимает публикацию с роутера до исчезновения объекта ---
	if !app.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&app, webAppFinalizer) {
			// Удаляем по имени записи, а не по домену: роутер ключует
			// `ip http proxy` именно именем.
			if err := r.Keenetic.DeleteProxy(ctx, app.Spec.Name); err != nil {
				l.Error(err, "не удалось убрать ip http proxy с роутера")
				return ctrl.Result{}, err // реквью, finalizer держим
			}
			controllerutil.RemoveFinalizer(&app, webAppFinalizer)
			if err := r.Update(ctx, &app); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// --- ставим finalizer ДО создания внешнего состояния ---
	if !controllerutil.ContainsFinalizer(&app, webAppFinalizer) {
		controllerutil.AddFinalizer(&app, webAppFinalizer)
		if err := r.Update(ctx, &app); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil // событие update реквьюит нас заново
	}

	// --- желаемое состояние: публикация присутствует (идемпотентно) ---
	if err := r.Keenetic.EnsureProxy(ctx, proxyFromSpec(app.Spec)); err != nil {
		r.setCondition(&app, "Ready", metav1.ConditionFalse, "ApplyFailed", err.Error())
		app.Status.Applied = false
		if serr := r.Status().Update(ctx, &app); serr != nil {
			l.Error(serr, "не удалось записать статус ApplyFailed")
		}
		return ctrl.Result{}, err
	}

	app.Status.Applied = true
	app.Status.ObservedGeneration = app.Generation
	r.setCondition(&app, "Ready", metav1.ConditionTrue, "Applied", "ip http proxy присутствует на роутере")
	if err := r.Status().Update(ctx, &app); err != nil {
		return ctrl.Result{}, err
	}

	// периодически переутверждаем — дрейф (кто-то снёс запись руками) сам залечится
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// proxyFromSpec переводит spec в то, что понимает клиент роутера.
//
// Пустые scheme/level дозаполняются здесь, хотя у CRD на них есть
// +kubebuilder:default. Дефолт схемы срабатывает только при записи через
// API-сервер с этой версией CRD: объект, созданный до её обновления, приезжает
// с пустыми полями и без этого улетал бы в роутер как `upstream  1.2.3.4 80`.
func proxyFromSpec(s keeneticv1alpha1.KeeneticWebAppSpec) keenetic.Proxy {
	scheme := s.UpstreamScheme
	if scheme == "" {
		scheme = "http"
	}
	level := s.SecurityLevel
	if level == "" {
		level = "public"
	}
	return keenetic.Proxy{
		Name:          s.Name,
		Domain:        s.Domain,
		Scheme:        scheme,
		Address:       s.UpstreamAddress,
		Port:          s.UpstreamPort,
		SecurityLevel: level,
		Auth:          s.Auth,
	}
}

func (r *KeeneticWebAppReconciler) setCondition(app *keeneticv1alpha1.KeeneticWebApp,
	t string, s metav1.ConditionStatus, reason, msg string) {
	apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: app.Generation,
	})
}

func (r *KeeneticWebAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&keeneticv1alpha1.KeeneticWebApp{}).
		Complete(r)
}
