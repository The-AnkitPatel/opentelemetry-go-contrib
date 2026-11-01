// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package otelhttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type testCtxKey struct{}

func copyRouteRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), testCtxKey{}, "copied")))
	})
}

func TestRouteMiddleware(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Wrap mux in RouteMiddleware, then inside request-copying middleware, then inside otelhttp.NewHandler
	handler := NewHandler(
		copyRouteRequest(RouteMiddleware(mux)),
		"server",
		WithTracerProvider(tp),
		WithMeterProvider(mp),
		WithRouteTracking(),
	)

	req := httptest.NewRequest(http.MethodGet, "/items/42", http.NoBody)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	// Verify span name and route attribute
	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "GET /items/{id}", spans[0].Name())
	assert.Contains(t, spans[0].Attributes(), attribute.String("http.route", "/items/{id}"))

	// Verify metric attribute
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	require.NotEmpty(t, rm.ScopeMetrics)

	foundRouteMetric := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "http.server.request.duration" {
				data, ok := m.Data.(metricdata.Histogram[float64])
				require.True(t, ok)
				for _, dp := range data.DataPoints {
					if v, ok := dp.Attributes.Value("http.route"); ok {
						assert.Equal(t, "/items/{id}", v.AsString())
						foundRouteMetric = true
					}
				}
			}
		}
	}
	assert.True(t, foundRouteMetric, "metric http.server.request.duration should have http.route attribute")
}

func TestRouteMiddleware_DisabledByDefault(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// RouteTracking is NOT enabled, so RouteMiddleware is a pass-through
	handler := NewHandler(
		copyRouteRequest(RouteMiddleware(mux)),
		"server",
		WithTracerProvider(tp),
	)

	req := httptest.NewRequest(http.MethodGet, "/items/42", http.NoBody)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	// Without route tracking and with copying middleware, route is not visible to outer handler
	assert.Equal(t, "GET", spans[0].Name())
	assert.NotContains(t, spans[0].Attributes(), attribute.String("http.route", "/items/{id}"))
}

func TestRouteMiddleware_NestedMux(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	innerMux := http.NewServeMux()
	innerMux.HandleFunc("GET /api/v1/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	outerMux := http.NewServeMux()
	outerMux.Handle("/api/v1/", RouteMiddleware(innerMux))

	handler := NewHandler(
		copyRouteRequest(RouteMiddleware(outerMux)),
		"server",
		WithTracerProvider(tp),
		WithRouteTracking(),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/99", http.NoBody)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	// Innermost matched route wins
	assert.Equal(t, "GET /api/v1/users/{id}", spans[0].Name())
	assert.Contains(t, spans[0].Attributes(), attribute.String("http.route", "/api/v1/users/{id}"))
}

func TestRouteMiddleware_PanicPreservation(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /panic/{id}", func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})

	handler := NewHandler(
		copyRouteRequest(RouteMiddleware(mux)),
		"server",
		WithTracerProvider(tp),
		WithRouteTracking(),
	)

	req := httptest.NewRequest(http.MethodGet, "/panic/123", http.NoBody)
	rec := httptest.NewRecorder()

	assert.Panics(t, func() {
		handler.ServeHTTP(rec, req)
	})

	// Even during panic unwinding, defer in RouteMiddleware should have set route on span
	spans := sr.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "GET /panic/{id}", spans[0].Name())
	assert.Contains(t, spans[0].Attributes(), attribute.String("http.route", "/panic/{id}"))
}

func TestRouteMiddleware_Unmatched(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {})

	handler := NewHandler(
		copyRouteRequest(RouteMiddleware(mux)),
		"server",
		WithTracerProvider(tp),
		WithRouteTracking(),
	)

	req := httptest.NewRequest(http.MethodGet, "/nonexistent", http.NoBody)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	spans := sr.Ended()
	require.Len(t, spans, 1)
	// Unmatched 404 should retain default name without route attribute
	assert.Equal(t, "GET", spans[0].Name())
	assert.NotContains(t, spans[0].Attributes(), attribute.String("http.route", "/nonexistent"))
}

func TestRouteMiddleware_ConcurrencyRace(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := NewHandler(
		copyRouteRequest(RouteMiddleware(mux)),
		"server",
		WithTracerProvider(tp),
		WithRouteTracking(),
	)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/items/1", http.NoBody)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusOK, rec.Code)
		}(i)
	}
	wg.Wait()
}
