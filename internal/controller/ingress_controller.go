/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"net/netip"
	"slices"
	"strconv"
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
	"github.com/Arbuzov/keenetic-operator/internal/keenetic"
	"github.com/Arbuzov/keenetic-operator/internal/metrics"
)

// Аннотации на Ingress, которыми настраивается публикация хоста наружу
// (`ip http proxy` на роутере). Всё, кроме publish, имеет разумный дефолт из
// env, так что типичному Ingress'у аннотации не нужны вовсе.
const (
	// AnnPublish — "false" отключает публикацию этого Ingress'а.
	AnnPublish = "keenetic.whitediver.com/publish"
	// AnnUpstream — куда роутер проксирует. Это адрес ingress-контроллера, а НЕ
	// тот адрес, в который резолвится имя: имя обязано резолвиться в роутер,
	// иначе прокси минуется и TLS-сертификат KeenDNS не применяется.
	AnnUpstream       = "keenetic.whitediver.com/upstream"
	AnnUpstreamPort   = "keenetic.whitediver.com/upstream-port"
	AnnUpstreamScheme = "keenetic.whitediver.com/upstream-scheme"
	AnnSecurityLevel  = "keenetic.whitediver.com/security-level"
	AnnAuth           = "keenetic.whitediver.com/auth"
	// AnnProxyName — имя записи в конфиге роутера. По умолчанию первая метка
	// хоста (notes для notes.example.keenetic.link).
	AnnProxyName = "keenetic.whitediver.com/proxy-name"
)

// IngressReconciler превращает хосты Ingress в дочерние KeeneticHostRecord
// (статический DNS) и KeeneticWebApp (публикация наружу).
type IngressReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// DefaultAddress — адрес, когда у Ingress нет LB-IP в status
	// (один общий nginx LB). Берётся из env DEFAULT_INGRESS_IP.
	DefaultAddress string

	// DefaultUpstream* — куда публикуемые хосты проксируются по умолчанию.
	// Пустой DefaultUpstreamAddress означает «публиковать нечем»: без него
	// оператор ведёт только DNS-записи, как и до появления публикаций.
	DefaultUpstreamAddress string
	DefaultUpstreamPort    int32
	DefaultUpstreamScheme  string
	// DefaultSecurityLevel — public | private для хостов без аннотации.
	DefaultSecurityLevel string
	// PublishByDefault — публиковать ли Ingress, на котором нет AnnPublish.
	PublishByDefault bool
	// KeenDNSZone — зона KeenDNS роутера, например example.keenetic.link.
	// Хост внутри неё публикуется через `domain ndns` — единственный способ
	// заявить имя в KeenDNS. Пусто -> публикуем через `domain static <зона>`,
	// что верно для собственных доменов, но для keenetic.link даст запись,
	// которая имя не заявляет.
	KeenDNSZone string
}

//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
//+kubebuilder:rbac:groups=keenetic.whitediver.com,resources=keenetichostrecords,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=keenetic.whitediver.com,resources=keeneticwebapps,verbs=get;list;watch;create;update;patch;delete

func (r *IngressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var ing networkingv1.Ingress
	if err := r.Get(ctx, req.NamespacedName, &ing); err != nil {
		// Ingress удалён: его дочерние записи соберёт GC по OwnerReference.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Раннего выхода по «у этого Ingress ещё нет адреса» здесь нет намеренно.
	// Адрес записи берётся не отсюда, а из согласия всех Ingress'ов хоста (см.
	// ниже), поэтому безадресный Ingress всё равно обязан оформить владение:
	// иначе сосед, у которого адрес есть, уйдёт, унесёт запись как последнюю
	// ссылку — и хост, который мы всё ещё заявляем, исчезнет с роутера.

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
		// Ровно один адрес — согласие, пишем. Ноль значит, что ни один из
		// Ingress'ов хоста ещё не получил адрес: ждём, это не ничья ошибка.
		// Больше одного — разошлись, и лечится это НЕ выбором победителя: с
		// MatchEveryOwner перезапись spec будит всех владельцев, те переписывают
		// обратно — не «последний победил», а незатухающий цикл, каждый оборот
		// которого доходит до роутера как `ip host` + `system configuration
		// save`, то есть запись во флеш. Одно имя не может резолвиться в два
		// адреса; чинится это в Ingress'ах, а не здесь.
		settled := len(agreed) == 1
		if len(agreed) > 1 {
			metrics.HostRecordsAddressConflict.Inc()
		}

		rec := &keeneticv1alpha1.KeeneticHostRecord{
			ObjectMeta: metav1.ObjectMeta{Name: host, Namespace: ing.Namespace},
		}
		if !settled {
			// Владение и выбор адреса — разные вещи. Даже без согласованного
			// адреса мы обязаны числиться владельцем существующей записи: иначе
			// уход другого Ingress'а снесёт её как «последнюю ссылку» вместе с
			// нужной нам записью на роутере, и delete-событие нас даже не
			// разбудит. А вот создать запись нельзя — spec.address обязателен
			// в CRD.
			reason := "адреса ещё нет ни у одного Ingress этого хоста"
			if len(agreed) > 1 {
				reason = "Ingress'ы заявляют разные адреса"
			}
			err := r.Get(ctx, client.ObjectKeyFromObject(rec), rec)
			if apierrors.IsNotFound(err) {
				l.Info("хост пропущен, записи ещё нет",
					"host", host, "reason", reason, "addresses", agreed)
				deferred = true
				continue
			}
			if err != nil {
				return ctrl.Result{}, err
			}
			l.Info("адрес не трогаем, владение оформляем",
				"host", host, "reason", reason, "addresses", agreed)
			deferred = true
		}

		op, err := controllerutil.CreateOrUpdate(ctx, r.Client, rec, func() error {
			rec.Spec.Hostname = host
			if settled {
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

	// --- публикации наружу (`ip http proxy`) ---
	// Отдельным проходом, а не внутри цикла выше: DNS-запись и публикация —
	// разные объекты роутера с разными условиями применимости. Хост, который
	// не публикуется, обязан всё равно получить запись `ip host`, иначе он
	// перестанет резолвиться внутри сети.
	webDeferred, err := r.reconcileWebApps(ctx, &ing, desired)
	if err != nil {
		return ctrl.Result{}, err
	}

	if deferred || webDeferred {
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

// reconcileWebApps приводит публикации хостов этого Ingress'а в соответствие
// с его аннотациями. Возвращает true, если что-то отложено до разрешения
// конфликта между Ingress'ами.
func (r *IngressReconciler) reconcileWebApps(ctx context.Context,
	ing *networkingv1.Ingress, hosts map[string]struct{}) (bool, error) {
	l := log.FromContext(ctx)

	tlsByHost, err := r.hostsRequiringTLS(ctx)
	if err != nil {
		return false, err
	}

	// Чего хочет именно этот Ingress. Пустая карта — законное состояние:
	// publish=false, или publish включён, но публиковать некуда.
	mine := map[string]struct{}{}
	if r.publishes(ctx, ing) {
		for host := range hosts {
			if _, ok := r.webAppSpec(ctx, ing, host, tlsByHost[host]); ok {
				mine[host] = struct{}{}
			}
		}
	}

	// Чего хотят по этим хостам все Ingress'ы namespace. Как и с адресами,
	// нужно, чтобы совладельцы одного хоста не переписывали spec друг за другом.
	specsByHost, err := r.webAppSpecsByHost(ctx, ing.Namespace, tlsByHost)
	if err != nil {
		return false, err
	}

	var deferred bool
	for host := range mine {
		agreed := specsByHost[host]
		app := &keeneticv1alpha1.KeeneticWebApp{
			ObjectMeta: metav1.ObjectMeta{Name: host, Namespace: ing.Namespace},
		}

		if len(agreed) != 1 {
			// Разошлись. Победителя не выбираем по той же причине, что и с
			// адресами: с MatchEveryOwner перезапись spec будит всех владельцев,
			// и цикл переписываний упирался бы в роутер, то есть во флеш.
			// Опаснее, чем с адресом: пока конфликт не разрешён, публикация
			// живёт с прежними настройками — например, остаётся public после
			// того, как её в Ingress понизили до private.
			metrics.WebAppsConflict.Inc()
			deferred = true

			// Владение всё равно оформляем: иначе уход другого Ingress'а унесёт
			// публикацию как последнюю ссылку вместе с хостом, который мы
			// по-прежнему заявляем.
			err := r.Get(ctx, client.ObjectKeyFromObject(app), app)
			if apierrors.IsNotFound(err) {
				l.Info("публикация пропущена, записи ещё нет",
					"host", host, "reason", "Ingress'ы заявляют разные настройки")
				continue
			}
			if err != nil {
				return false, err
			}
			l.Info("настройки публикации не трогаем, владение оформляем", "host", host)
		}

		op, err := controllerutil.CreateOrUpdate(ctx, r.Client, app, func() error {
			if len(agreed) == 1 {
				app.Spec = agreed[0]
			}
			return controllerutil.SetOwnerReference(ing, app, r.Scheme)
		})
		if err != nil {
			return false, err
		}
		if op != controllerutil.OperationResultNone {
			l.Info("сверили публикацию", "name", host, "op", op)
		}
	}

	// снимаем свою ссылку с публикаций, которые этот Ingress больше не хочет
	var owned keeneticv1alpha1.KeeneticWebAppList
	if err := r.List(ctx, &owned, client.InNamespace(ing.Namespace)); err != nil {
		return false, err
	}
	for i := range owned.Items {
		app := &owned.Items[i]
		if _, keep := mine[app.Name]; keep {
			continue
		}
		ours, err := controllerutil.HasOwnerReference(app.OwnerReferences, ing, r.Scheme)
		if err != nil {
			return false, err
		}
		if !ours {
			continue
		}
		if err := controllerutil.RemoveOwnerReference(ing, app, r.Scheme); err != nil {
			return false, err
		}
		if len(app.OwnerReferences) > 0 {
			// публикация всё ещё нужна другим Ingress'ам — только снимаем свою
			// ссылку. NotFound глотаем: объект мог уйти между List и Update.
			if err := r.Update(ctx, app); err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
			continue
		}
		// ушёл последний владелец. Удаляем сами, не дожидаясь GC: под envtest
		// он не работает вовсе, а в кластере иначе остался бы зазор, в котором
		// роутер продолжает пускать трафик снаружи на снятое приложение.
		if err := r.Delete(ctx, app); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}

	return deferred, nil
}

// webAppSpecsByHost собирает по каждому хосту namespace множество различных
// желаемых публикаций. Один вариант — согласие, больше одного — конфликт.
func (r *IngressReconciler) webAppSpecsByHost(ctx context.Context, namespace string,
	tlsByHost map[string]bool) (map[string][]keeneticv1alpha1.KeeneticWebAppSpec, error) {
	var list networkingv1.IngressList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	byHost := map[string][]keeneticv1alpha1.KeeneticWebAppSpec{}
	for i := range list.Items {
		ing := &list.Items[i]
		if !r.publishes(ctx, ing) {
			continue
		}
		for _, rule := range ing.Spec.Rules {
			if rule.Host == "" {
				continue
			}
			host := strings.ToLower(rule.Host)
			spec, ok := r.webAppSpec(ctx, ing, host, tlsByHost[host])
			if !ok {
				continue
			}
			if !slices.Contains(byHost[host], spec) {
				byHost[host] = append(byHost[host], spec)
			}
		}
	}
	return byHost, nil
}

// publishes — просит ли этот Ingress публиковать свои хосты наружу.
//
// Невнятное значение аннотации трактуется как «не публиковать», а не как
// дефолт: аннотацию ставят, чтобы управлять публикацией, и опечатка в ней при
// publish-by-default молча выставила бы сервис в интернет. Кто хочет дефолт —
// не пишет аннотацию вовсе.
func (r *IngressReconciler) publishes(ctx context.Context, ing *networkingv1.Ingress) bool {
	v, ok := ing.Annotations[AnnPublish]
	if !ok || v == "" {
		return r.PublishByDefault
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		log.FromContext(ctx).Info("не разобрал аннотацию публикации, хосты не публикуем",
			"ingress", ing.Name, "annotation", AnnPublish, "value", v)
		return false
	}
	return b
}

// webAppSpec — желаемая публикация одного хоста. false означает «этот хост
// публиковать нечем или нечему»: не ошибка реконсайла, а причина пропустить.
func (r *IngressReconciler) webAppSpec(ctx context.Context, ing *networkingv1.Ingress,
	host string, tlsRequired bool) (keeneticv1alpha1.KeeneticWebAppSpec, bool) {
	l := log.FromContext(ctx)
	ann := ing.Annotations

	name := ann[AnnProxyName]
	if name == "" {
		// Первая метка FQDN: так же, как имя записи выбирает сам роутер в
		// режиме `domain ndns`, и так же выглядит в его веб-интерфейсе.
		name, _, _ = strings.Cut(host, ".")
	}
	if !keenetic.ValidProxyName(name) {
		l.Info("хост не публикуем: имя записи не годится для роутера",
			"host", host, "name", name, "hint", AnnProxyName)
		return keeneticv1alpha1.KeeneticWebAppSpec{}, false
	}

	address := ann[AnnUpstream]
	if address == "" {
		address = r.DefaultUpstreamAddress
	}
	if address == "" {
		// Не ошибка: без DEFAULT_UPSTREAM_IP оператор просто работает как
		// раньше — ведёт DNS-записи и ничего не публикует.
		l.V(1).Info("хост не публикуем: не задан upstream", "host", host, "hint", AnnUpstream)
		return keeneticv1alpha1.KeeneticWebAppSpec{}, false
	}
	if addr, err := netip.ParseAddr(address); err != nil || !addr.Is4() {
		// Проверяем здесь, а не полагаемся на схему CRD: с кривым адресом
		// объект не пройдёт валидацию при создании, и реконсайл будет крутиться
		// на ошибке вместо того, чтобы один раз сказать, что не так. Остальные
		// поля ниже отсеиваются ровно так же.
		l.Info("хост не публикуем: upstream не IPv4-адрес",
			"host", host, "value", address, "hint", AnnUpstream)
		return keeneticv1alpha1.KeeneticWebAppSpec{}, false
	}

	port := r.DefaultUpstreamPort
	portSet := false
	if v := ann[AnnUpstreamPort]; v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 || n > 65535 {
			l.Info("хост не публикуем: некорректный порт upstream",
				"host", host, "annotation", AnnUpstreamPort, "value", v)
			return keeneticv1alpha1.KeeneticWebAppSpec{}, false
		}
		port, portSet = int32(n), true
	}

	scheme := ann[AnnUpstreamScheme]
	if scheme == "" && tlsRequired {
		// Хост, у которого хоть один Ingress несёт TLS, ingress-nginx по
		// умолчанию редиректит с :80 на https. Отправить туда роутер по http
		// значит получить 308 на имя, которое резолвится обратно в роутер, —
		// петля редиректов, а не отказ. Именно так лёг dev.* со всем, что на
		// нём висит.
		scheme = "https"
		if !portSet {
			port = 443
		}
	}
	if scheme == "" {
		scheme = r.DefaultUpstreamScheme
	}
	if scheme == "" {
		scheme = "http"
	}
	if port == 0 {
		port = 80
	}
	if scheme != "http" && scheme != "https" {
		l.Info("хост не публикуем: некорректная схема upstream",
			"host", host, "annotation", AnnUpstreamScheme, "value", scheme)
		return keeneticv1alpha1.KeeneticWebAppSpec{}, false
	}

	level := ann[AnnSecurityLevel]
	if level == "" {
		level = r.DefaultSecurityLevel
	}
	if level == "" {
		level = "public"
	}
	if level != "public" && level != "private" {
		l.Info("хост не публикуем: некорректный уровень доступа",
			"host", host, "annotation", AnnSecurityLevel, "value", level)
		return keeneticv1alpha1.KeeneticWebAppSpec{}, false
	}

	auth := false
	if v := ann[AnnAuth]; v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			l.Info("хост не публикуем: некорректное значение auth",
				"host", host, "annotation", AnnAuth, "value", v)
			return keeneticv1alpha1.KeeneticWebAppSpec{}, false
		}
		auth = b
	}

	return keeneticv1alpha1.KeeneticWebAppSpec{
		Name:   name,
		Domain: host,
		// Внутри зоны KeenDNS публиковать можно только через `domain ndns` —
		// иначе запись есть, а имя не заявлено, и роутер отвечает на него
		// собственным веб-интерфейсом.
		NDNS:            r.inKeenDNSZone(host),
		UpstreamAddress: address,
		UpstreamPort:    port,
		UpstreamScheme:  scheme,
		SecurityLevel:   level,
		Auth:            auth,
	}, true
}

// inKeenDNSZone — лежит ли хост непосредственно в зоне KeenDNS роутера.
// Именно непосредственно: `domain ndns` собирает имя как `<имя записи>.<зона>`,
// то есть ровно одну метку сверху, и для a.b.<зона> дало бы не тот хост.
func (r *IngressReconciler) inKeenDNSZone(host string) bool {
	if r.KeenDNSZone == "" {
		return false
	}
	return strings.EqualFold(keenetic.ZoneOf(host), r.KeenDNSZone)
}

// hostsRequiringTLS — хосты, к которым upstream обязан идти по https.
//
// Считается по ВСЕМ Ingress'ам кластера, а не по одному и не по namespace.
// Хост живёт в нескольких namespace сразу (dev.* обслуживают семь Ingress'ов),
// и TLS может нести только часть из них: у dev.* это argo-cd, grafana,
// octoprint и prometheus, а шесть остальных идут без. Достаточно одного —
// ingress-nginx редиректит весь хост, — а решение, посчитанное по своему
// namespace, у разных объектов вышло бы разным, и они переписывали бы одну
// запись роутера друг за другом.
func (r *IngressReconciler) hostsRequiringTLS(ctx context.Context) (map[string]bool, error) {
	var list networkingv1.IngressList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for i := range list.Items {
		ing := &list.Items[i]
		for _, rule := range ing.Spec.Rules {
			if rule.Host == "" {
				continue
			}
			host := strings.ToLower(rule.Host)
			out[host] = out[host] || ingressRequiresTLS(ing, host)
		}
	}
	return out, nil
}

// ingressRequiresTLS — отвечает ли ingress-nginx по этому хосту редиректом на
// https. Секции tls достаточно: ssl-redirect у ingress-nginx включён по
// умолчанию именно для хостов с TLS. Явная аннотация перебивает в обе стороны.
func ingressRequiresTLS(ing *networkingv1.Ingress, host string) bool {
	for _, ann := range []string{
		"nginx.ingress.kubernetes.io/force-ssl-redirect",
		"nginx.ingress.kubernetes.io/ssl-redirect",
	} {
		if v, ok := ing.Annotations[ann]; ok {
			b, err := strconv.ParseBool(v)
			if err == nil {
				return b
			}
		}
	}
	for _, t := range ing.Spec.TLS {
		if len(t.Hosts) == 0 {
			// Секция без списка хостов покрывает весь Ingress.
			return true
		}
		for _, h := range t.Hosts {
			if strings.EqualFold(h, host) {
				return true
			}
		}
	}
	return false
}

func (r *IngressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1.Ingress{}).
		// MatchEveryOwner обязателен: без него Owns будит только
		// controller-владельца, а их у нас больше нет — только обычные.
		Owns(&keeneticv1alpha1.KeeneticHostRecord{}, builder.MatchEveryOwner).
		Owns(&keeneticv1alpha1.KeeneticWebApp{}, builder.MatchEveryOwner).
		Complete(r)
}
