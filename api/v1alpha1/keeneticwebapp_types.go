/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KeeneticWebAppSpec — желаемая запись `ip http proxy` на роутере: публикация
// внутреннего веб-приложения наружу через KeenDNS.
//
// Это НЕ то же самое, что KeeneticHostRecord. `ip host` — статический DNS для
// LAN: имя резолвится в сам роутер. `ip http proxy` — обратный прокси перед
// приложением: роутер терминирует TLS по сертификату KeenDNS и ходит на
// upstream. Без первой записи имя не резолвится внутри сети; без второй роутер
// не знает, куда проксировать запрос, и отдаёт собственный веб-интерфейс.
type KeeneticWebAppSpec struct {
	// Name — идентификатор записи в конфиге роутера (`ip http proxy <name>`).
	// Роутер держит имя отдельно от домена, и удаление идёт именно по имени, а
	// не по FQDN. Обычно это первая метка домена: notes для
	// notes.example.keenetic.link.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`
	Name string `json:"name"`

	// Domain — FQDN, под которым приложение видно снаружи (host из Ingress).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`
	Domain string `json:"domain"`

	// UpstreamAddress — IPv4 внутреннего сервера, куда роутер проксирует.
	// Намеренно отдельно от KeeneticHostRecord.spec.address: там адрес самого
	// роутера (имя должно резолвиться в него, иначе прокси минуется), а здесь —
	// адрес ingress-контроллера за роутером. Свести их в одно поле нельзя.
	// +kubebuilder:validation:Pattern=`^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$`
	UpstreamAddress string `json:"upstreamAddress"`

	// UpstreamPort — TCP-порт upstream'а.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	UpstreamPort int32 `json:"upstreamPort"`

	// UpstreamScheme — по какому протоколу роутер ходит на upstream.
	// +kubebuilder:validation:Enum=http;https
	// +kubebuilder:default=http
	// +optional
	UpstreamScheme string `json:"upstreamScheme,omitempty"`

	// SecurityLevel — public: «свободный доступ», имя открыто из интернета.
	// private: снаружи пускает только после входа в веб-интерфейс роутера.
	// +kubebuilder:validation:Enum=public;private
	// +kubebuilder:default=public
	// +optional
	SecurityLevel string `json:"securityLevel,omitempty"`

	// Auth — требовать ли авторизацию роутера перед приложением
	// (`auth` / `no auth` в конфиге).
	// +optional
	Auth bool `json:"auth,omitempty"`
}

// KeeneticWebAppStatus — наблюдаемое состояние.
type KeeneticWebAppStatus struct {
	// Applied — присутствует ли запись на роутере прямо сейчас.
	Applied bool `json:"applied,omitempty"`

	// AppliedName — под каким именем запись реально лежит на роутере.
	// Нужен ровно для одного: роутер ключует `ip http proxy` именем, а по
	// самому роутеру не узнать, какая из его записей была нашей. Без этого
	// поля смена spec.name создаёт запись под новым именем и навсегда бросает
	// старую — finalizer потом удалит только текущее имя. То же по сути, что и
	// уборка прежнего адреса у KeeneticHostRecord, только там прежнее значение
	// видно в конфиге роутера, а здесь — нет.
	// +optional
	AppliedName string `json:"appliedName,omitempty"`

	// ObservedGeneration — поколение spec, на котором последний раз сошлись.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions — стандартный k8s-паттерн (тип Ready и т.п.).
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Domain",type=string,JSONPath=`.spec.domain`
//+kubebuilder:printcolumn:name="Upstream",type=string,JSONPath=`.spec.upstreamAddress`
//+kubebuilder:printcolumn:name="Port",type=integer,JSONPath=`.spec.upstreamPort`
//+kubebuilder:printcolumn:name="Level",type=string,JSONPath=`.spec.securityLevel`
//+kubebuilder:printcolumn:name="Applied",type=boolean,JSONPath=`.status.applied`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KeeneticWebApp — одно опубликованное веб-приложение на Keenetic.
type KeeneticWebApp struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KeeneticWebAppSpec   `json:"spec,omitempty"`
	Status KeeneticWebAppStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// KeeneticWebAppList — список.
type KeeneticWebAppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KeeneticWebApp `json:"items"`
}
