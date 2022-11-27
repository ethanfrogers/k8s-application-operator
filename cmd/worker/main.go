package main

import (
	"flag"
	"fmt"
	"github.com/ethanfrogers/k8s-application-operator/pkg/worker"
	"go.temporal.io/sdk/client"
	temporal "go.temporal.io/sdk/worker"
	"go.uber.org/zap"
	"os"
	"path/filepath"
)

func main() {
	workflowTaskQueue := flag.String("workflow-task-queue", "kubernetes_worker", "")
	temporalHost := flag.String("temporal-host", "localhost:7233", "")
	kubeconfig := flag.String("kubeconfig", "", "")
	flag.Parse()

	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	opts := client.Options{
		HostPort: *temporalHost,
	}

	c, err := client.NewLazyClient(opts)
	if err != nil {
		panic(err)
	}
	defer c.Close()

	if *kubeconfig == "" {
		pth := filepath.Join(os.Getenv("HOME"), ".kube", "config")
		kubeconfig = &pth
		logger.Sugar().Infof("kubeconfig is empty, using default %s", *kubeconfig)
	}

	w := &worker.Worker{}

	if err := w.Run(temporal.InterruptCh()); err != nil {
		fmt.Printf("worker failed to start: %s", err.Error())
		os.Exit(1)
	}
}
