package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ethanfrogers/k8s-application-operator/pkg/apis/application"
	"github.com/ethanfrogers/k8s-application-operator/pkg/worker"
	"go.temporal.io/sdk/client"
	temporal "go.temporal.io/sdk/worker"
	"go.uber.org/zap"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
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

	c, err := client.Dial(opts)
	if err != nil {
		panic(err)
	}
	defer c.Close()

	if *kubeconfig == "" {
		pth := filepath.Join(os.Getenv("HOME"), ".kube", "config")
		kubeconfig = &pth
		logger.Sugar().Infof("kubeconfig is empty, using default %s", *kubeconfig)
	}
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err)
	}
	applicationsClient, err := application.NewForConfig(config)
	if err != nil {
		panic(err)
	}
	k8sClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	w := &worker.Worker{
		ApplicationsClient: applicationsClient,
		K8sClient:          k8sClient,
	}

	tworker := temporal.New(c, *workflowTaskQueue, temporal.Options{})
	w.Register(tworker)

	if err := tworker.Run(temporal.InterruptCh()); err != nil {
		fmt.Printf("worker failed to start: %s", err.Error())
		os.Exit(1)
	}
}
