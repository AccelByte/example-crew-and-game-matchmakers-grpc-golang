// Copyright (c) 2022 AccelByte Inc. All Rights Reserved.
// This is licensed software from AccelByte Inc, for limitations
// and restrictions contact your company contract manager.

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/AccelByte/accelbyte-go-sdk/services-api/pkg/factory"
	"github.com/AccelByte/accelbyte-go-sdk/services-api/pkg/service/iam"
	"github.com/AccelByte/accelbyte-go-sdk/services-api/pkg/utils/auth/validator"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	matchfunctiongrpc "matchmaking-function-grpc-plugin-server-go/pkg/pb"

	sdkAuth "github.com/AccelByte/accelbyte-go-sdk/services-api/pkg/utils/auth"
	prometheusGrpc "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/contrib/propagators/b3"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/zipkin"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.12.0"

	"matchmaking-function-grpc-plugin-server-go/pkg/server"

	"google.golang.org/grpc"
)

const (
	environment = "production"
	id          = 1
)

var (
	service  = os.Getenv("OTEL_SERVICE_NAME")
	gamePort = flag.Int("gamePort", 6565, "The grpc game server port")
	res      = resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceNameKey.String(service),
		attribute.String("environment", environment),
		attribute.Int64("ID", id),
	)
)

func initProvider(ctx context.Context, grpcServer *grpc.Server) (*sdktrace.TracerProvider, error) {
	// Create Zipkin Exporter and install it as a global tracer.
	//
	// For demoing purposes, always sample. In a production application, you should
	// configure the sampler to a trace.ParentBased(trace.TraceIDRatioBased) set at the desired
	// ratio.
	exporter, err := zipkin.New(os.Getenv("OTEL_EXPORTER_ZIPKIN_ENDPOINT"))
	if err != nil {
		logrus.Fatalf("failed to call zipkin exporter. %s", err.Error())
	}

	res = resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceNameKey.String(os.Getenv("OTEL_SERVICE_NAME")),
		attribute.String("environment", environment),
		attribute.Int64("ID", id),
	)

	// Register the trace exporter with a TracerProvider, using a batch
	// span processor to aggregate spans before export.
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(time.Second*1)),
	)

	// Shutdown will flush any remaining spans and shut down the exporter.
	return tracerProvider, nil
}

func main() {
	go func() {
		runtime.SetBlockProfileRate(1)
		runtime.SetMutexProfileFraction(10)
		_ = http.ListenAndServe(":6060", nil)
	}()
	logrus.Printf("pprof served at :6060")

	logrus.Infof("starting app server.")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	unaryServerInterceptors := []grpc.UnaryServerInterceptor{
		otelgrpc.UnaryServerInterceptor(),
		prometheusGrpc.UnaryServerInterceptor,
	}
	streamServerInterceptors := []grpc.StreamServerInterceptor{
		otelgrpc.StreamServerInterceptor(),
		prometheusGrpc.StreamServerInterceptor,
	}

	if strings.ToLower(server.GetEnv("PLUGIN_GRPC_SERVER_AUTH_ENABLED", "false")) == "true" {
		refreshInterval := server.GetEnvInt("REFRESH_INTERVAL", 600)
		configRepo := sdkAuth.DefaultConfigRepositoryImpl()
		tokenRepo := sdkAuth.DefaultTokenRepositoryImpl()
		authService := iam.OAuth20Service{
			Client:           factory.NewIamClient(configRepo),
			ConfigRepository: configRepo,
			TokenRepository:  tokenRepo,
		}
		server.Validator = validator.NewTokenValidator(authService, time.Duration(refreshInterval)*time.Second)
		server.Validator.Initialize()

		unaryServerInterceptors = append(unaryServerInterceptors, server.UnaryAuthServerIntercept)
		streamServerInterceptors = append(streamServerInterceptors, server.StreamAuthServerIntercept)
		logrus.Infof("added auth interceptors")
	}

	// Create gRPC Server

	gameServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(unaryServerInterceptors...),
		grpc.ChainStreamInterceptor(streamServerInterceptors...),
	)

	// prometheus metrics
	prometheusGrpc.Register(gameServer)
	prometheusGrpc.EnableHandlingTimeHistogram()

	// Create non-global registry.
	registry := prometheus.NewRegistry()

	// Add go runtime metrics and process collectors.
	registry.MustRegister(
		prometheusGrpc.DefaultServerMetrics,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	go func() {
		http.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
		log.Fatal(http.ListenAndServe(":8080", nil))
	}()
	logrus.Printf("prometheus metrics served at :8080/metrics")

	logrus.Infof("listening to grpc port for game: %d", *gamePort)
	gameLis, err := net.Listen("tcp", fmt.Sprintf(":%d", *gamePort))
	if err != nil {
		logrus.Fatalf("failed to listen: %v", err)
		return
	}

	//create game matchmaker
	gameMM := server.NewGameMatchmaker()
	matchfunctiongrpc.RegisterMatchFunctionServer(gameServer, &server.MatchFunctionServer{
		UnimplementedMatchFunctionServer: matchfunctiongrpc.UnimplementedMatchFunctionServer{},
		MM:                               gameMM,
	})

	logrus.Infof("adding the grpc reflection.")

	// Enable gRPC Reflection
	reflection.Register(gameServer)
	logrus.Infof("gRPC reflection enabled")

	// Enable gRPC Health Check
	grpc_health_v1.RegisterHealthServer(gameServer, health.NewServer())
	logrus.Printf("gRPC server listening at %v", gameLis.Addr())

	logrus.Infof("listening...")
	go func() {
		if err = gameServer.Serve(gameLis); err != nil {
			logrus.Fatalf("failed to serve: %v", err)
			return
		}
	}()

	//TODO look at this for tracing
	logrus.Infof("starting init provider.")
	gameTraceProvider, err := initProvider(ctx, gameServer)
	if err != nil {
		logrus.Fatalf("failed to initializing the provider. %s", err.Error())

		return
	}

	// Register our TracerProvider as the global so any imported
	// instrumentation in the future will default to using it.
	otel.SetTracerProvider(gameTraceProvider)
	// Register the B3 propagator globally.
	p := b3.New()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		p,
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// Cleanly shutdown and flush telemetry when the application exits.
	defer func(ctx context.Context) {
		if err := gameTraceProvider.Shutdown(ctx); err != nil {
			logrus.Fatal(err)
		}
	}(ctx)

	flag.Parse()

	ctx, _ = signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	<-ctx.Done()
	fmt.Println("Goodbye...")
}
