/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	keeneticv1alpha1 "github.com/Arbuzov/keenetic-operator/api/v1alpha1"
	"github.com/Arbuzov/keenetic-operator/internal/keenetic"
	"github.com/Arbuzov/keenetic-operator/internal/metrics"
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
	CountProxies(ctx context.Context) (int, error)
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
			// `ip http proxy` именно именем. И только если это имя больше
			// никому не нужно — см. releaseName.
			//
			// Имя берём из status, а не из spec: там записано то, что реально
			// уехало на роутер. Пусто оно бывает и когда применить не успели, и
			// когда применили, но не смогли записать status — во втором случае
			// запись на роутере есть, поэтому откатываемся на spec.name, иначе
			// она осталась бы там навсегда.
			applied := app.Status.AppliedName
			if applied == "" {
				applied = app.Spec.Name
			}
			if err := r.releaseName(ctx, &app, applied); err != nil {
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

	// --- чужое имя не занимаем ---
	// Роутер ключует запись именем, а имя по умолчанию — первая метка хоста.
	// Значит notes.a.example.com и notes.b.example.com метят в одну и ту же
	// запись `notes`, и без этой проверки два объекта переписывали бы её друг
	// за другом вечно — каждый оборот с `system configuration save`, то есть с
	// записью во флеш. Победителя не выбираем, как и при расхождении адресов:
	// разводится это заданием keenetic.whitediver.com/proxy-name, а не гаданием.
	rival, err := r.nameClaimedElsewhere(ctx, &app)
	if err != nil {
		return ctrl.Result{}, err
	}
	if rival != "" {
		metrics.WebAppsConflict.Inc()
		r.setCondition(&app, "Ready", metav1.ConditionFalse, "NameConflict",
			fmt.Sprintf("имя записи %q уже занято публикацией %s под другим доменом", app.Spec.Name, rival))
		app.Status.Applied = false
		if err := r.Status().Update(ctx, &app); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
	}

	// --- переименование: прежняя запись остаётся на роутере, если её не убрать ---
	// Роутер не подскажет, какая из его записей была нашей, поэтому прежнее имя
	// мы знаем только из status. Не убрать его — значит бросить работающую
	// публикацию под старым именем навсегда.
	if prev := app.Status.AppliedName; prev != "" && prev != app.Spec.Name {
		if err := r.releaseName(ctx, &app, prev); err != nil {
			l.Error(err, "не удалось убрать прежнюю запись после переименования", "previous", prev)
			return ctrl.Result{}, err
		}
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
	app.Status.AppliedName = app.Spec.Name
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
		Name: s.Name,
		// Роутер ждёт в `domain static` зону, а не FQDN, и сам склеивает
		// `<name>.<zone>`. Полный хост там превращается в
		// notes.notes.example.keenetic.link.
		Zone:          keenetic.ZoneOf(s.Domain),
		NDNS:          s.NDNS,
		Scheme:        scheme,
		Address:       s.UpstreamAddress,
		Port:          s.UpstreamPort,
		SecurityLevel: level,
		Auth:          s.Auth,
		// Всегда включаем — так выглядят записи, заведённые через веб-интерфейс
		// роутера. PreserveHost из них обязателен: без него upstream получает
		// `Host: <ip>`, ingress-контроллер не находит правило и отдаёт 404.
		SSLRedirect:     true,
		XRealIP:         true,
		PreserveHost:    true,
		PreserveReferer: true,
		PreserveOrigin:  true,
	}
}

// releaseName снимает запись с роутера — но только если это имя больше никто
// не заявляет.
//
// Проверка не намеренная перестраховка, а необходимость: объектов на одно имя
// бывает несколько законно. Один и тот же хост живёт в нескольких namespace
// (в этом кластере dev.* обслуживают семь Ingress'ов из разных namespace), и
// каждый заводит свой KeeneticWebApp с одним и тем же spec.name. Снести запись
// по уходу первого из них значит выключить публикацию у остальных шести —
// они переутвердят её в течение пяти минут, но это пять минут, которых можно
// не платить.
func (r *KeeneticWebAppReconciler) releaseName(ctx context.Context,
	app *keeneticv1alpha1.KeeneticWebApp, name string) error {
	if name == "" {
		// Применить запись не успели — снимать нечего.
		return nil
	}
	others, err := r.claimants(ctx, name, app.UID)
	if err != nil {
		return err
	}
	if len(others) > 0 {
		log.FromContext(ctx).Info("запись на роутере оставляем: имя заявляют другие публикации",
			"name", name, "claimants", others)
		return nil
	}
	if err := r.Keenetic.DeleteProxy(ctx, name); err != nil {
		return err
	}
	// Освежаем гейдж по той же причине, что и у host record: после удаления
	// последней публикации реконсайлить больше некого, и значение замерло бы
	// навсегда. Best-effort — падать на метрике при удалении нельзя.
	if n, cErr := r.Keenetic.CountProxies(ctx); cErr == nil {
		metrics.RouterWebApps.Set(float64(n))
	} else {
		log.FromContext(ctx).V(1).Info("не удалось освежить keenetic_router_web_apps после удаления", "err", cErr)
	}
	return nil
}

// nameClaimedElsewhere — заявляет ли то же имя записи другая публикация, но под
// ДРУГИМ доменом. Совпадение домена — это законное совладение (один хост в
// нескольких namespace), расхождение — коллизия имён.
func (r *KeeneticWebAppReconciler) nameClaimedElsewhere(ctx context.Context,
	app *keeneticv1alpha1.KeeneticWebApp) (string, error) {
	var list keeneticv1alpha1.KeeneticWebAppList
	if err := r.List(ctx, &list); err != nil {
		return "", err
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.UID == app.UID || !other.DeletionTimestamp.IsZero() {
			continue
		}
		if other.Spec.Name == app.Spec.Name && !strings.EqualFold(other.Spec.Domain, app.Spec.Domain) {
			return other.Namespace + "/" + other.Name, nil
		}
	}
	return "", nil
}

// claimants — живые публикации (кроме self), заявляющие это имя записи.
func (r *KeeneticWebAppReconciler) claimants(ctx context.Context,
	name string, self types.UID) ([]string, error) {
	var list keeneticv1alpha1.KeeneticWebAppList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	var out []string
	for i := range list.Items {
		other := &list.Items[i]
		if other.UID == self || !other.DeletionTimestamp.IsZero() {
			continue
		}
		if other.Spec.Name == name {
			out = append(out, other.Namespace+"/"+other.Name)
		}
	}
	return out, nil
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
