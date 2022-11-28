package worker

import (
	"context"
	"fmt"
	"github.com/ethanfrogers/k8s-application-operator/api/v1alpha1"
	"github.com/ethanfrogers/k8s-application-operator/pkg/apis/application"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"io"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

type Worker struct {
	ApplicationsClient *application.Clientset
	K8sClient          *kubernetes.Clientset
}

func (w *Worker) Register(registry worker.Worker) {
	registry.RegisterWorkflow(w.Reconcile)
	registry.RegisterActivity(w.GetApplicationConfig)
	registry.RegisterWorkflow(w.ManageEnvironment)
	registry.RegisterActivity(w.InstallApplication)
	registry.RegisterActivity(w.EnsureInstallation)
}

type ReconcileRequest struct {
	Key string
}

func (w *Worker) Reconcile(ctx workflow.Context, req ReconcileRequest) error {
	activityContext := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		ScheduleToStartTimeout: 1 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 1,
		},
	})
	var applicationConfig v1alpha1.Application
	err := workflow.ExecuteActivity(activityContext, w.GetApplicationConfig, req.Key).Get(ctx, &applicationConfig)
	if err != nil {
		return err
	}
	selector := workflow.NewSelector(ctx)
	for _, env := range applicationConfig.Spec.Environments {
		childOptions := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			WorkflowID:        fmt.Sprintf("manage-%s-%s", req.Key, env.Name),
			ParentClosePolicy: enums.PARENT_CLOSE_POLICY_TERMINATE,
		})
		envReq := ManageEnvironmentRequest{
			Name:      applicationConfig.ObjectMeta.Name,
			Namespace: applicationConfig.ObjectMeta.Namespace,
			Artifacts: applicationConfig.Spec.Artifacts,
			Placement: env.Placement,
		}
		manageWorkflow := workflow.ExecuteChildWorkflow(childOptions, w.ManageEnvironment, envReq)
		selector.AddFuture(manageWorkflow, func(f workflow.Future) {})
	}

	for true {
		selector.Select(ctx)
	}

	return nil
}

func (w *Worker) GetApplicationConfig(ctx context.Context, key string) (*v1alpha1.Application, error) {
	parts := strings.Split(key, "/")
	application, err := w.ApplicationsClient.V1alpha1().Applications(parts[0]).Get(ctx, parts[1], v12.GetOptions{})
	if err != nil {
		return nil, err
	}
	return application, nil
}

type InstallApplicationRequest struct {
	Name      string
	Namespace string
	Artifacts []v1alpha1.Artifact
}

type InstallApplicationResponse struct {
}

func (w *Worker) InstallApplication(ctx context.Context, req *InstallApplicationRequest) (*InstallApplicationResponse, error) {
	chartArtifact, err := findFirstArtifact(req.Artifacts, "HelmChart")
	if err != nil {
		return nil, err
	}
	chartPath, cleanup, err := downloadChartArtifact(ctx, chartArtifact.Repository, chartArtifact.Version)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	installName := req.Name
	installNamespace := req.Namespace

	args := []string{
		"install",
		installName,
		"-n", installNamespace,
		chartPath,
	}
	cmd := exec.Command("helm", args...)
	if err := cmd.Run(); err != nil {
		return nil, err
	}

	return &InstallApplicationResponse{}, nil

}

func findFirstArtifact(artifacts []v1alpha1.Artifact, kind string) (*v1alpha1.Artifact, error) {
	for _, a := range artifacts {
		if a.Kind == kind {
			return &a, nil
		}
	}

	return nil, fmt.Errorf("artifact of kind %s not found", kind)
}

func downloadChartArtifact(ctx context.Context, reference, version string) (string, func(), error) {
	url := fmt.Sprintf("%s-%s.tgz", reference, version)
	resp, err := http.Get(url)
	if err != nil {
		return "", nil, err
	}
	f, err := os.CreateTemp("", "")
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return "", nil, err
	}

	cleanup := func() {
		os.RemoveAll(f.Name())
	}
	return f.Name(), cleanup, nil
}

type ManageEnvironmentRequest struct {
	Name      string
	Namespace string
	Artifacts []v1alpha1.Artifact
	Placement *v1alpha1.Placement
}

func (w *Worker) ManageEnvironment(ctx workflow.Context, req ManageEnvironmentRequest) error {
	logger := workflow.GetLogger(ctx)
	name := req.Name
	namespace := req.Namespace
	if req.Placement != nil && req.Placement.StaticPlacement != nil {
		namespace = req.Placement.StaticPlacement.Namespace
	}
	var err error
	for err == nil {
		workflow.Sleep(ctx, 1*time.Minute)
		activityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			RetryPolicy: &temporal.RetryPolicy{
				MaximumAttempts: 1,
			},
		})
		ensureReq := EnsureInstallationRequest{Name: name, Namespace: namespace}
		var deployed bool
		if err := workflow.ExecuteActivity(activityCtx, w.EnsureInstallation, ensureReq).Get(ctx, deployed); err != nil {
			logger.Error("unable to ensure environment, trying again", "error", err)
			continue
		}
		if !deployed {
			installCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
				RetryPolicy: &temporal.RetryPolicy{
					MaximumAttempts: 1,
				},
			})
			installReq := InstallApplicationRequest{
				Name:      name,
				Namespace: namespace,
				Artifacts: req.Artifacts,
			}
			var installResp InstallApplicationResponse
			if err := workflow.ExecuteActivity(installCtx, installCtx, installReq).Get(ctx, &installResp); err != nil {
				logger.Error("failed to install application, will try again", "error", err)
			}
		}

	}
	return nil
}

type EnsureInstallationRequest struct {
	Name      string
	Namespace string
}

func (w *Worker) EnsureInstallation(ctx context.Context, req EnsureInstallationRequest) (bool, error) {
	listOptions := v12.ListOptions{
		LabelSelector: fmt.Sprintf("owner=helm,name=%s", req.Name),
	}
	configMaps, err := w.K8sClient.CoreV1().ConfigMaps(req.Namespace).List(ctx, listOptions)
	if err != nil {
		return false, err
	}
	if len(configMaps.Items) == 0 {
		return false, nil
	}
	return true, nil
}
