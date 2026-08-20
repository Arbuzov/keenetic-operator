/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

// Package metrics — доменные метрики оператора: только то, чего не видно
// ниоткуда, кроме как отсюда. Состояние роутера знает лишь тот, у кого есть
// SSH к нему, а состояние самих CR прекрасно снимается kube-state-metrics'ом
// из .status — дублировать его здесь незачем.
//
// Общие controller_runtime_* / workqueue_* / rest_client_* регистрирует сам
// controller-runtime; сюда они не входят.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Имена операций для лейбла `operation`. Одна операция == одна логическая
// работа с роутером, а не одна SSH-сессия: ensure это чтение running-config и,
// если записи нет, ещё и запись — две сессии под одним лейблом. Вложенные
// вызовы не измеряются повторно, так что один отказ даёт ровно один инкремент.
const (
	OpEnsure = "ensure"
	OpDelete = "delete"
	OpHas    = "has"
	OpCount  = "count"
)

var (
	// RouterHosts — сколько записей `ip host` сейчас на роутере. Обновляется
	// на каждом реконсайле; записи переутверждаются раз в 5 минут, так что
	// значение не застаивается, пока существует хоть один KeeneticHostRecord.
	RouterHosts = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "keenetic_router_hosts",
		Help: "Number of `ip host` entries currently present on the router.",
	})

	// RouterHostsLimit — потолок из KEENETIC_MAX_HOSTS. Вынесен в метрику,
	// чтобы алерт писался как отношение, а не с зашитой в запрос константой.
	RouterHostsLimit = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "keenetic_router_hosts_limit",
		Help: "Configured cap on `ip host` entries (KEENETIC_MAX_HOSTS).",
	})

	// RouterOperations — исходы обращений к роутеру. Единственный способ
	// увидеть, что SSH до роутера отвалился: реконсайлер возвращает такую
	// ошибку наверх, но по controller_runtime_reconcile_errors_total не
	// отличить недоступный роутер от конфликта записи в API-сервер.
	RouterOperations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "keenetic_router_operations_total",
		Help: "Router SSH operations by operation and outcome.",
	}, []string{"operation", "result"})

	// RouterOperationDuration — длительность логической операции, не одной
	// SSH-сессии: ensure укладывает в один замер и чтение, и запись. Дефолтные
	// бакеты client_golang (5ms..10s) для этого не годятся: заход на бытовой
	// роутер это сотни миллисекунд в лучшем случае и секунды на деградации, а
	// один только таймаут диала — 10s.
	RouterOperationDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "keenetic_router_operation_duration_seconds",
		Help:    "Duration of one logical router operation (ensure spans a read and a write).",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 8), // 0.1s .. 12.8s
	}, []string{"operation"})

	// HostRecordsLimitRejected — записи, которые не поехали на роутер из-за
	// упора в лимит. Отдельный счётчик нужен именно потому, что этот путь
	// возвращает nil-ошибку (и RequeueAfter): в reconcile_errors_total его
	// не видно вообще, то есть самый неприятный отказ — «новые хосты молча
	// не появляются» — иначе неотличим от штатной работы.
	HostRecordsLimitRejected = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "keenetic_host_records_limit_rejected_total",
		Help: "Reconciles that could not apply a record because the router is at its `ip host` cap.",
	})

	// HostRecordsAddressConflict — хосты, для которых Ingress'ы одного
	// namespace заявили разные адреса, так что оператор отказался выбирать.
	// Отказ тихий по построению (nil-ошибка и RequeueAfter), то есть в
	// reconcile_errors_total его нет — ровно тот же класс молчаливого отказа,
	// что и упор в лимит. Тикает на каждом отложенном хосте на каждом проходе,
	// поэтому rate() > 0 читается как «конфликт прямо сейчас».
	HostRecordsAddressConflict = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "keenetic_host_records_address_conflict_total",
		Help: "Hosts left unwritten because Ingresses sharing them report different addresses.",
	})
)

func init() {
	// Регистр controller-runtime, а не prometheus.DefaultRegisterer: менеджер
	// отдаёт на /metrics именно его.
	ctrlmetrics.Registry.MustRegister(
		RouterHosts,
		RouterHostsLimit,
		RouterOperations,
		RouterOperationDuration,
		HostRecordsLimitRejected,
		HostRecordsAddressConflict,
	)
}

// ObserveRouterOp записывает исход и длительность одной операции с роутером.
// Рассчитан на defer с именованным возвращаемым значением:
//
//	func (c *Client) EnsureHost(...) (err error) {
//	    defer metrics.ObserveRouterOp(metrics.OpEnsure, time.Now(), &err)
//
// errp именно указатель: defer вычисляет аргументы в момент объявления, так
// что по значению сюда всегда приезжал бы nil.
func ObserveRouterOp(op string, start time.Time, errp *error) {
	result := "success"
	if errp != nil && *errp != nil {
		result = "error"
	}
	RouterOperations.WithLabelValues(op, result).Inc()
	RouterOperationDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
}
