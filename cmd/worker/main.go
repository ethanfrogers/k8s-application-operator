package main

import (
	"fmt"
	flag "github.com/spf13/pflag"
	"os"
	"path/filepath"

	"k8s.io/utils/pointer"

	"k8s.io/cli-runtime/pkg/genericclioptions"

	"k8s.io/apimachinery/pkg/runtime"

	deploymentsv1alpha1 "github.com/ethanfrogers/k8s-application-operator/api/v1alpha1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ethanfrogers/k8s-application-operator/pkg/worker"
	tclient "go.temporal.io/sdk/client"
	temporal "go.temporal.io/sdk/worker"
	"go.uber.org/zap"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	contextMap map[string]string
)

func main() {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(deploymentsv1alpha1.AddToScheme(scheme))

	workflowTaskQueue := flag.String("workflow-task-queue", "application-reconciler", "")
	temporalHost := flag.String("temporal-host", "localhost:7233", "")
	kubeconfig := flag.String("kubeconfig", "", "")
	flag.StringToStringVarP(&contextMap, "map-cluster-ctx", "m", map[string]string{}, "")
	flag.Parse()

	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	opts := tclient.Options{
		HostPort: *temporalHost,
	}

	c, err := tclient.Dial(opts)
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
	crclient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}

	for k, v := range contextMap {
		fmt.Printf("cluster %s mapped to context %s\n", k, v)
	}

	w := &worker.Worker{
		Client: crclient,
		RestClientGetterFactory: func(cluster, namespace string) (genericclioptions.RESTClientGetter, error) {
			clusterContext, ok := contextMap[cluster]
			if !ok {
				return nil, fmt.Errorf("no cluster found for %s", cluster)
			}
			return &genericclioptions.ConfigFlags{
				KubeConfig: kubeconfig,
				Namespace:  &namespace,
				Context:    pointer.String(clusterContext),
			}, nil
		},
	}

	tworker := temporal.New(c, *workflowTaskQueue, temporal.Options{})
	w.Register(tworker)

	if err := tworker.Run(temporal.InterruptCh()); err != nil {
		fmt.Printf("worker failed to start: %s", err.Error())
		os.Exit(1)
	}
}
