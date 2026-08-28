/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	keeneticv1alpha1 "github.com/Arbuzov/keenetic-operator/api/v1alpha1"
)

// publishingReconciler — реконсайлер с включённой публикацией, но без клиента:
// webAppSpec и publishes к API-серверу не ходят, так что для них этого хватает.
func publishingReconciler() *IngressReconciler {
	return &IngressReconciler{
		DefaultUpstreamAddress: "192.168.99.44",
		DefaultUpstreamPort:    80,
		DefaultUpstreamScheme:  "http",
		DefaultSecurityLevel:   "public",
		PublishByDefault:       true,
		KeenDNSZone:            "example.keenetic.link",
	}
}

func ingressWith(annotations map[string]string) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default", Annotations: annotations},
	}
}

func TestWebAppSpecUsesDefaultsWithoutAnnotations(t *testing.T) {
	r := publishingReconciler()

	got, ok := r.webAppSpec(context.Background(), ingressWith(nil), "notes.example.keenetic.link", false)
	if !ok {
		t.Fatal("webAppSpec() = false, want a spec")
	}

	want := keeneticv1alpha1.KeeneticWebAppSpec{
		// Имя записи — первая метка хоста: так же выбирает сам роутер.
		Name:   "notes",
		Domain: "notes.example.keenetic.link",
		// Хост лежит прямо в зоне KeenDNS — публиковать его можно только ndns.
		NDNS:            true,
		UpstreamAddress: "192.168.99.44",
		UpstreamPort:    80,
		UpstreamScheme:  "http",
		SecurityLevel:   "public",
	}
	if got != want {
		t.Errorf("webAppSpec() = %#v, want %#v", got, want)
	}
}

func TestWebAppSpecHonoursAnnotations(t *testing.T) {
	r := publishingReconciler()
	ing := ingressWith(map[string]string{
		AnnProxyName:      "my-notes",
		AnnUpstream:       "192.168.99.50",
		AnnUpstreamPort:   "8443",
		AnnUpstreamScheme: "https",
		AnnSecurityLevel:  "private",
		AnnAuth:           "true",
	})

	got, ok := r.webAppSpec(context.Background(), ing, "notes.example.keenetic.link", false)
	if !ok {
		t.Fatal("webAppSpec() = false, want a spec")
	}

	want := keeneticv1alpha1.KeeneticWebAppSpec{
		Name:            "my-notes",
		Domain:          "notes.example.keenetic.link",
		NDNS:            true,
		UpstreamAddress: "192.168.99.50",
		UpstreamPort:    8443,
		UpstreamScheme:  "https",
		SecurityLevel:   "private",
		Auth:            true,
	}
	if got != want {
		t.Errorf("webAppSpec() = %#v, want %#v", got, want)
	}
}

// Без upstream публиковать некуда. Это не ошибка реконсайла: оператор без
// DEFAULT_UPSTREAM_IP просто ведёт DNS-записи, как до появления публикаций.
func TestWebAppSpecSkipsWithoutAnUpstream(t *testing.T) {
	r := publishingReconciler()
	r.DefaultUpstreamAddress = ""

	if _, ok := r.webAppSpec(context.Background(), ingressWith(nil), "notes.example.keenetic.link", false); ok {
		t.Error("webAppSpec() = true, want false without an upstream")
	}
}

// Негодные значения отсеиваются ДО создания CR: иначе объект не прошёл бы
// валидацию CRD-схемы и реконсайл крутился бы на ошибке создания.
func TestWebAppSpecRejectsBadValues(t *testing.T) {
	tests := map[string]map[string]string{
		"порт не число":           {AnnUpstreamPort: "http"},
		"порт вне диапазона":      {AnnUpstreamPort: "70000"},
		"схема не http/https":     {AnnUpstreamScheme: "ftp"},
		"неизвестный уровень":     {AnnSecurityLevel: "open"},
		"auth не булево":          {AnnAuth: "maybe"},
		"имя записи с пробелом":   {AnnProxyName: "my notes"},
		"имя записи слишком длин": {AnnProxyName: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"upstream не IPv4":        {AnnUpstream: "ingress.example.com"},
		"upstream с мусором":      {AnnUpstream: "192.168.99.44; reboot"},
	}

	for name, ann := range tests {
		t.Run(name, func(t *testing.T) {
			r := publishingReconciler()
			if _, ok := r.webAppSpec(context.Background(), ingressWith(ann), "notes.example.keenetic.link", false); ok {
				t.Errorf("webAppSpec(%v) = true, want false", ann)
			}
		})
	}
}

// Хост, чья первая метка не годится в имя записи роутера, пропускается, а не
// роняет реконсайл.
func TestWebAppSpecSkipsAHostWithAnUnusableLabel(t *testing.T) {
	r := publishingReconciler()

	long := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.example.keenetic.link" // 33 символа в метке
	if _, ok := r.webAppSpec(context.Background(), ingressWith(nil), long, false); ok {
		t.Error("webAppSpec() = true, want false for an over-long label")
	}
}

func TestPublishes(t *testing.T) {
	tests := []struct {
		name             string
		annotation       string
		set              bool
		publishByDefault bool
		want             bool
	}{
		{name: "нет аннотации, дефолт публикует", set: false, publishByDefault: true, want: true},
		{name: "нет аннотации, дефолт не публикует", set: false, publishByDefault: false, want: false},
		{name: "явное true", annotation: "true", set: true, publishByDefault: false, want: true},
		{name: "явное false", annotation: "false", set: true, publishByDefault: true, want: false},
		{
			// Аннотацию ставят, чтобы управлять публикацией. Откат к
			// publish-by-default на опечатке молча выставил бы сервис в интернет.
			name:       "опечатка не публикует, а не откатывается к дефолту",
			annotation: "ture", set: true, publishByDefault: true, want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := publishingReconciler()
			r.PublishByDefault = tt.publishByDefault
			ann := map[string]string{}
			if tt.set {
				ann[AnnPublish] = tt.annotation
			}
			if got := r.publishes(context.Background(), ingressWith(ann)); got != tt.want {
				t.Errorf("publishes() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Хост вне зоны KeenDNS роутера ndns'ом публиковать нельзя: роутер подставил бы
// СВОЮ зону и опубликовал не тот адрес.
func TestWebAppSpecUsesStaticDomainOutsideTheKeenDNSZone(t *testing.T) {
	r := publishingReconciler()

	got, ok := r.webAppSpec(context.Background(), ingressWith(nil), "notes.example.com", false)
	if !ok {
		t.Fatal("webAppSpec() = false, want a spec")
	}
	if got.NDNS {
		t.Errorf("NDNS = true for a host outside %q", r.KeenDNSZone)
	}
}

// Глубже одной метки ndns тоже не годится: роутер собирает имя как
// <запись>.<зона>, то есть ровно одну метку сверху.
func TestWebAppSpecDoesNotUseNDNSDeeperThanOneLabel(t *testing.T) {
	r := publishingReconciler()

	got, ok := r.webAppSpec(context.Background(), ingressWith(nil), "a.b.example.keenetic.link", false)
	if !ok {
		t.Fatal("webAppSpec() = false, want a spec")
	}
	if got.NDNS {
		t.Error("NDNS = true for a host two labels below the zone")
	}
}

// Хост с TLS обязан идти по https: ingress-nginx редиректит :80 на https, имя
// резолвится обратно в роутер, и получается петля редиректов, а не отказ.
func TestWebAppSpecSwitchesToHTTPSWhenTheHostRequiresTLS(t *testing.T) {
	r := publishingReconciler()

	got, ok := r.webAppSpec(context.Background(), ingressWith(nil), "dev.example.keenetic.link", true)
	if !ok {
		t.Fatal("webAppSpec() = false, want a spec")
	}
	if got.UpstreamScheme != "https" || got.UpstreamPort != 443 {
		t.Errorf("upstream = %s/%d, want https/443", got.UpstreamScheme, got.UpstreamPort)
	}
}

// Явный порт важнее вывода из TLS: его задали руками, значит знают, что делают.
func TestWebAppSpecKeepsAnAnnotatedPortUnderTLS(t *testing.T) {
	r := publishingReconciler()
	ing := ingressWith(map[string]string{AnnUpstreamPort: "8443"})

	got, ok := r.webAppSpec(context.Background(), ing, "dev.example.keenetic.link", true)
	if !ok {
		t.Fatal("webAppSpec() = false, want a spec")
	}
	if got.UpstreamScheme != "https" || got.UpstreamPort != 8443 {
		t.Errorf("upstream = %s/%d, want https/8443", got.UpstreamScheme, got.UpstreamPort)
	}
}

func TestIngressRequiresTLS(t *testing.T) {
	host := "dev.example.keenetic.link"

	withTLS := ingressWith(nil)
	withTLS.Spec.TLS = []networkingv1.IngressTLS{{Hosts: []string{host}}}
	if !ingressRequiresTLS(withTLS, host) {
		t.Error("spec.tls covering the host: got false, want true")
	}

	// Секция без списка хостов покрывает весь Ingress.
	blanket := ingressWith(nil)
	blanket.Spec.TLS = []networkingv1.IngressTLS{{SecretName: "x"}}
	if !ingressRequiresTLS(blanket, host) {
		t.Error("spec.tls without hosts: got false, want true")
	}

	otherHost := ingressWith(nil)
	otherHost.Spec.TLS = []networkingv1.IngressTLS{{Hosts: []string{"other.example.keenetic.link"}}}
	if ingressRequiresTLS(otherHost, host) {
		t.Error("spec.tls for another host: got true, want false")
	}

	// Аннотация перебивает: у books/notes/photos тут стоит ssl-redirect=false
	// при наличии TLS-секции, и редиректа там нет.
	off := ingressWith(map[string]string{"nginx.ingress.kubernetes.io/ssl-redirect": "false"})
	off.Spec.TLS = []networkingv1.IngressTLS{{Hosts: []string{host}}}
	if ingressRequiresTLS(off, host) {
		t.Error("ssl-redirect=false with TLS: got true, want false")
	}

	if ingressRequiresTLS(ingressWith(nil), host) {
		t.Error("no TLS and no annotation: got true, want false")
	}
}

var _ = Describe("Ingress controller publishing web apps", func() {
	It("creates an owned KeeneticWebApp per host and removes it when publishing is turned off", func() {
		pathType := networkingv1.PathTypePrefix
		ing := &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: "notes", Namespace: "default"},
			Spec: networkingv1.IngressSpec{
				Rules: []networkingv1.IngressRule{{
					Host: "notes.example.keenetic.link",
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{{
								Path:     "/",
								PathType: &pathType,
								Backend: networkingv1.IngressBackend{
									Service: &networkingv1.IngressServiceBackend{
										Name: "notes",
										Port: networkingv1.ServiceBackendPort{Number: 80},
									},
								},
							}},
						},
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, ing)).To(Succeed())

		// Адрес нужен не публикации, а DNS-записи: без него KeeneticHostRecord
		// не создаётся (spec.address обязателен), и проверить в конце, что
		// запись пережила отключение публикации, было бы не на чем.
		ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "192.168.99.1"}}
		Expect(k8sClient.Status().Update(ctx, ing)).To(Succeed())

		appKey := types.NamespacedName{Name: "notes.example.keenetic.link", Namespace: "default"}
		Eventually(func() error {
			var app keeneticv1alpha1.KeeneticWebApp
			return k8sClient.Get(ctx, appKey, &app)
		}).Should(Succeed())

		var app keeneticv1alpha1.KeeneticWebApp
		Expect(k8sClient.Get(ctx, appKey, &app)).To(Succeed())
		Expect(app.Spec.Name).To(Equal("notes"))
		Expect(app.Spec.Domain).To(Equal("notes.example.keenetic.link"))
		Expect(app.Spec.UpstreamAddress).To(Equal("192.168.99.44"))
		Expect(app.Spec.UpstreamPort).To(Equal(int32(80)))
		Expect(app.Spec.SecurityLevel).To(Equal("public"))
		owned, err := controllerutil.HasOwnerReference(app.OwnerReferences, ing, k8sClient.Scheme())
		Expect(err).NotTo(HaveOccurred())
		Expect(owned).To(BeTrue())

		// Публикацию выключили. Уборка наша, а не GC (под envtest он не
		// работает), и она обязана сработать: иначе роутер продолжал бы пускать
		// снаружи на приложение, которое сняли с публикации.
		ing.Annotations = map[string]string{AnnPublish: "false"}
		Expect(k8sClient.Update(ctx, ing)).To(Succeed())

		Eventually(func() bool {
			var got keeneticv1alpha1.KeeneticWebApp
			err := k8sClient.Get(ctx, appKey, &got)
			return apierrors.IsNotFound(err)
		}).Should(BeTrue())

		// DNS-запись при этом остаётся: хост по-прежнему должен резолвиться
		// внутри сети, публикация наружу — отдельное решение. Снести её заодно
		// значило бы уронить сервис в LAN при попытке всего лишь убрать его
		// из интернета.
		var rec keeneticv1alpha1.KeeneticHostRecord
		Expect(k8sClient.Get(ctx, appKey, &rec)).To(Succeed())
		Expect(rec.Spec.Address).To(Equal("192.168.99.1"))
	})
})
