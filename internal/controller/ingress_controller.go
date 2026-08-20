/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"slices"
	"strings"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	keeneticv1alpha1 "github.com/Arbuzov/keenetic-operator/api/v1alpha1"
	"github.com/Arbuzov/keenetic-operator/internal/metrics"
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

	// желаемые записи = по одной на уникальный хост Ingress.
	// Имя CR == hostname (валидный DNS subdomain, точки разрешены).
	desired := map[string]struct{}{}
	for _, rule := range ing.Spec.Rules {
		if rule.Host == "" {
			continue
		}
		desired[strings.ToLower(rule.Host)] = struct{}{}
	}

	// Адреса, которые по каждому хосту заявляют ВСЕ Ingress'ы этого namespace.
	// Нужны, чтобы совладельцы одной записи не переписывали spec.address друг
	// за другом.
	addrsByHost, err := r.addressesByHost(ctx, ing.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	// создаём/обновляем записи
	var deferred bool
	for host := range desired {
		agreed := addrsByHost[host]
		// Разногласие по адресу лечится НЕ выбором победителя: с
		// MatchEveryOwner перезапись spec будит всех владельцев, те переписывают
		// обратно — это не «последний победил», а незатухающий цикл, и каждый
		// его оборот доходит до роутера как `ip host` + `system configuration
		// save`, то есть запись во флеш. Одно имя не может резолвиться в два
		// адреса; чинится это в Ingress'ах, а не здесь.
		conflict := len(agreed) != 1
		if conflict {
			metrics.HostRecordsAddressConflict.Inc()
		}

		rec := &keeneticv1alpha1.KeeneticHostRecord{
			ObjectMeta: metav1.ObjectMeta{Name: host, Namespace: ing.Namespace},
		}
		if conflict {
			// Владение и выбор адреса — разные вещи. Даже в конфликте мы обязаны
			// числиться владельцем существующей записи: иначе уход другого
			// Ingress'а снесёт её как «последнюю ссылку» вместе с нужной нам
			// записью на роутере, и delete-событие нас даже не разбудит.
			// А вот создать запись нельзя — spec.address обязателен в CRD.
			err := r.Get(ctx, client.ObjectKeyFromObject(rec), rec)
			if apierrors.IsNotFound(err) {
				l.Info("хост пропущен: Ingress'ы заявляют разные адреса, записи ещё нет",
					"host", host, "addresses", agreed)
				deferred = true
				continue
			}
			if err != nil {
				return ctrl.Result{}, err
			}
			l.Info("адрес не трогаем: Ingress'ы заявляют разные адреса",
				"host", host, "addresses", agreed)
			deferred = true
		}

		op, err := controllerutil.CreateOrUpdate(ctx, r.Client, rec, func() error {
			rec.Spec.Hostname = host
			if !conflict {
				rec.Spec.Address = agreed[0]
			}
			// Именно SetOwnerReference, а не SetControllerReference: несколько
			// Ingress'ов в одном namespace спокойно делят хост (у нас так живёт
			// весь mcp — четыре Ingress'а на dev.whitediver.keenetic.link).
			// Controller-ссылка бывает только одна, и остальные вечно падали бы
			// с AlreadyOwnedError. Обычных владельцев может быть много, а GC
			// удалит запись, когда уйдёт последний из них — это и есть нужная
			// семантика: запись живёт, пока её хочет хоть один Ingress.
			// Address у всех совладельцев один (адрес общего LB), так что
			// перезапись spec друг за другом не создаёт борьбы.
			return controllerutil.SetOwnerReference(&ing, rec, r.Scheme)
		})
		if err != nil {
			return ctrl.Result{}, err
		}
		if op != controllerutil.OperationResultNone {
			l.Info("сверили host record", "name", host, "op", op)
		}
	}

	// снимаем свою ссылку с записей, которые этот Ingress больше не хочет
	var owned keeneticv1alpha1.KeeneticHostRecordList
	if err := r.List(ctx, &owned, client.InNamespace(ing.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	for i := range owned.Items {
		rec := &owned.Items[i]
		if _, keep := desired[rec.Name]; keep {
			continue
		}
		ours, err := controllerutil.HasOwnerReference(rec.OwnerReferences, &ing, r.Scheme)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ours {
			continue
		}
		if err := controllerutil.RemoveOwnerReference(&ing, rec, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if len(rec.OwnerReferences) > 0 {
			// запись всё ещё нужна другим Ingress'ам — только снимаем свою ссылку.
			// NotFound глотаем: запись могла уйти между List и Update. Сейчас
			// такого окна нет (MaxConcurrentReconciles = 1), но если его поднимут,
			// глотание — единственное, что отделяет это место от падения.
			if err := r.Update(ctx, rec); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			continue
		}
		// ушёл последний владелец — запись больше никому не нужна.
		// Делаем это сами, не дожидаясь GC: под envtest он не работает вовсе,
		// а в кластере иначе остался бы зазор со стухшей записью на роутере.
		if err := r.Delete(ctx, rec); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	if deferred {
		// Пока конфликт не разрешён, разбудить нас некому: чужой Ingress мы не
		// watch'им, а записи, через которую прилетело бы событие, может и не
		// быть. Возвращаемся сами, иначе исправленный конфликт ждал бы ресинка.
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}
	return ctrl.Result{}, nil
}

// addressesByHost собирает по каждому хосту namespace множество адресов,
// которые заявляют обслуживающие его Ingress'ы. Один адрес — согласие, можно
// писать; больше одного — конфликт, писать нельзя (см. вызов).
func (r *IngressReconciler) addressesByHost(ctx context.Context, namespace string) (map[string][]string, error) {
	var list networkingv1.IngressList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	byHost := map[string][]string{}
	for i := range list.Items {
		addr := r.addressFor(&list.Items[i])
		if addr == "" {
			continue // ещё не получил адрес — не считаем это разногласием
		}
		for _, rule := range list.Items[i].Spec.Rules {
			if rule.Host == "" {
				continue
			}
			host := strings.ToLower(rule.Host)
			if !slices.Contains(byHost[host], addr) {
				byHost[host] = append(byHost[host], addr)
			}
		}
	}
	return byHost, nil
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
		// MatchEveryOwner обязателен: без него Owns будит только
		// controller-владельца, а их у нас больше нет — только обычные.
		Owns(&keeneticv1alpha1.KeeneticHostRecord{}, builder.MatchEveryOwner).
		Complete(r)
}
