package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"runtime"
	"time"

	"github.com/go-pg/pg"
	"github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/logrus"
	"github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	"github.com/grpc-ecosystem/go-grpc-middleware/tags"
	"github.com/grpc-ecosystem/go-grpc-middleware/tracing/opentracing"
	"github.com/grpc-ecosystem/go-grpc-prometheus"
	grpc_runtime "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"github.com/uber/jaeger-client-go/config"
	"github.com/uber/jaeger-client-go/rpcmetrics"
	prometheus_metrics "github.com/uber/jaeger-lib/metrics/prometheus"
	"github.com/urfave/cli"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	todoV1 "github.com/nizsheanez/monorepo/todo/client"
	todo "github.com/nizsheanez/monorepo/todo/client/v2"
)

func main() {
	app := cli.NewApp()
	app.Name = path.Base(os.Args[0])
	app.Usage = "Todo app"
	app.Version = "0.0.1"
	app.Flags = commonFlags
	app.Action = start

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

// Panic handler prints the stack trace when recovering from a panic.
var panicHandler = grpc_recovery.RecoveryHandlerFunc(func(p interface{}) error {
	buf := make([]byte, 1<<16)
	runtime.Stack(buf, true)
	log.Errorf("panic recovered: %+v", string(buf))
	return status.Errorf(codes.Internal, "%s", p)
})

func start(c *cli.Context) {
	lis, err := net.Listen("tcp", c.String("bind-grpc"))
	if err != nil {
		log.Fatalf("Failed to listen: %v", c.String("bind-grpc"))
	}

	// Logrus
	logger := log.NewEntry(log.New())
	grpc_logrus.ReplaceGrpcLogger(logger)
	log.SetLevel(log.InfoLevel)

	// Prometheus monitoring
	metrics := prometheus_metrics.New()

	// Jaeger tracing
	cfg := config.Configuration{
		Sampler: &config.SamplerConfig{
			Type:  "const",
			Param: c.Float64("jaeger-sampler"),
		},
		Reporter: &config.ReporterConfig{
			LocalAgentHostPort: c.String("jaeger-host") + ":" + c.String("jaeger-port"),
		},
	}
	tracer, closer, err := cfg.New(
		"todo",
		config.Logger(jaegerLoggerAdapter{logger}),
		config.Observer(rpcmetrics.NewObserver(metrics.Namespace("todo", nil), rpcmetrics.DefaultNameNormalizer)),
	)
	if err != nil {
		logger.Fatalf("Cannot initialize Jaeger Tracer %s", err)
	}
	defer closer.Close()

	// Set GRPC Interceptors
	server := grpc.NewServer(
		grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(
			grpc_ctxtags.StreamServerInterceptor(grpc_ctxtags.WithFieldExtractor(grpc_ctxtags.CodeGenRequestFieldExtractor)),
			grpc_opentracing.StreamServerInterceptor(grpc_opentracing.WithTracer(tracer)),
			grpc_prometheus.StreamServerInterceptor,
			grpc_logrus.StreamServerInterceptor(logger),
			grpc_recovery.StreamServerInterceptor(grpc_recovery.WithRecoveryHandler(panicHandler)),
		)),
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
			grpc_ctxtags.UnaryServerInterceptor(grpc_ctxtags.WithFieldExtractor(grpc_ctxtags.CodeGenRequestFieldExtractor)),
			grpc_opentracing.UnaryServerInterceptor(grpc_opentracing.WithTracer(tracer)),
			grpc_prometheus.UnaryServerInterceptor,
			grpc_logrus.UnaryServerInterceptor(logger),
			grpc_recovery.UnaryServerInterceptor(grpc_recovery.WithRecoveryHandler(panicHandler)),
		)),
	)

	// Connect to PostgresQL
	db := pg.Connect(&pg.Options{
		User:                  c.String("db-user"),
		Password:              c.String("db-password"),
		Database:              c.String("db-name"),
		Addr:                  c.String("db-host") + ":" + c.String("db-port"),
		RetryStatementTimeout: true,
		MaxRetries:            4,
		MinRetryBackoff:       250 * time.Millisecond,
	})

	// Create Table from Todo struct generated by gRPC
	db.CreateTable(&todo.Todo{}, nil)

	// Register Todo service, prometheus and HTTP service handler
	//api.RegisterTodoServiceServer(server, &todo.Service{DB: db})
	grpc_prometheus.Register(server)

	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		http.ListenAndServe(c.String("bind-prometheus-http"), mux)
	}()

	log.Println("Starting Todo service..")
	go server.Serve(lis)

	conn, err := grpc.Dial(c.String("bind-grpc"), grpc.WithInsecure())
	if err != nil {
		panic("Couldn't contact grpc server")
	}

	mux := grpc_runtime.NewServeMux()
	err = api.RegisterTodoServiceHandler(context.Background(), mux, conn)
	if err != nil {
		panic("Cannot serve http api")
	}
	http.ListenAndServe(c.String("bind-http"), mux)
}

type jaegerLoggerAdapter struct {
	logger *log.Entry
}

func (l jaegerLoggerAdapter) Error(msg string) {
	l.logger.Error(msg)
}

func (l jaegerLoggerAdapter) Infof(msg string, args ...interface{}) {
	l.logger.Info(fmt.Sprintf(msg, args...))
}
