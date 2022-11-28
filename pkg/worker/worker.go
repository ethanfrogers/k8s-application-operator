package worker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"

	"github.com/ethanfrogers/k8s-application-operator/api/v1alpha1"
	"github.com/ethanfrogers/k8s-application-operator/pkg/apis/application"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Worker struct {
	ApplicationsClient *application.Clientset
	K8sClient          *kubernetes.Clientset
	Client             client.Client
}

func (w *Worker) Register(registry worker.Worker) {
	registry.RegisterWorkflow(w.Reconcile)
	registry.RegisterActivity(w.GetApplicationConfig)
	registry.RegisterWorkflow(w.ManageEnvironment)
	registry.RegisterActivity(w.InstallApplication)
	registry.RegisterActivity(w.EnsureInstallation)
}

type ReconcileRequest struct {
	Name, Namespace string
}

func (w *Worker) Reconcile(ctx workflow.Context, req ReconcileRequest) error {
	activityContext := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		ScheduleToStartTimeout: 1 * time.Minute,
		ScheduleToCloseTimeout: 1 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 1,
		},
	})
	var applicationConfig v1alpha1.Application
	key := types.NamespacedName{Namespace: req.Namespace, Name: req.Name}.String()
	err := workflow.ExecuteActivity(activityContext, w.GetApplicationConfig, key).Get(ctx, &applicationConfig)
	if err != nil {
		return err
	}

	environmentSignalNames := map[string]string{}
	var environmentFutures []workflow.Future
	for _, env := range applicationConfig.Spec.Environments {
		signalName := fmt.Sprintf("modify-%s", env.Name)
		environmentSignalNames[env.Name] = signalName
		childOptions := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			WorkflowID:        fmt.Sprintf("manage-%s-%s", key, env.Name),
			ParentClosePolicy: enums.PARENT_CLOSE_POLICY_TERMINATE,
		})
		envReq := EnvironmentManagerRequest{
			Name:            applicationConfig.ObjectMeta.Name,
			SignalName:      signalName,
			ParentNamespace: applicationConfig.ObjectMeta.Namespace,
		}
		manageWorkflow := workflow.ExecuteChildWorkflow(childOptions, w.EnvironmentManager, envReq)
		environmentFutures = append(environmentFutures, manageWorkflow)
	}

	return nil
}

func (w *Worker) GetApplicationConfig(ctx context.Context, key string) (*v1alpha1.Application, error) {
	parts := strings.Split(key, "/")
	namespacedName := types.NamespacedName{
		Namespace: parts[0],
		Name:      parts[1],
	}
	ac := &v1alpha1.Application{}
	if err := w.Client.Get(ctx, namespacedName, ac); err != nil {
		return nil, fmt.Errorf("could not fetch application config: %w", err)
	}
	return ac, nil
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

type EnvironmentManagerRequest struct {
	Name            string
	ParentNamespace string
	SignalName      string
}

type ModifyEnvironmentRequest struct {
	Artifacts []v1alpha1.Artifact
	Placement *v1alpha1.Placement
}

func (w *Worker) EnvironmentManager(ctx workflow.Context, req EnvironmentManagerRequest) error {
	logger := workflow.GetLogger(ctx)
	signalChan := workflow.GetSignalChannel(ctx, req.SignalName)
	selector := workflow.NewSelector(ctx)
	selector.AddReceive(signalChan, func(c workflow.ReceiveChannel, more bool) {
		var modifyRequest ModifyEnvironmentRequest
		c.Receive(ctx, &modifyRequest)
		namespace := determinePlacement(req.ParentNamespace, modifyRequest.Placement)
		installCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			ScheduleToCloseTimeout: 10 * time.Minute,
			RetryPolicy: &temporal.RetryPolicy{
				MaximumAttempts: 1,
			},
		})
		installRequest := InstallApplicationRequest{
			Name:      req.Name,
			Namespace: namespace,
			Artifacts: modifyRequest.Artifacts,
		}
		installExec := workflow.ExecuteActivity(ctx, w.InstallApplication, installRequest)
		var installResp InstallApplicationResponse
		if err := installExec.Get(installCtx, &installResp); err != nil {
			logger.Error("failed to install environment", "error", err)
		}
	})

	for true {
		selector.Select(ctx)
	}
	return nil
}

func determinePlacement(parentNamespace string, placement *v1alpha1.Placement) string {
	namespace := parentNamespace
	if placement != nil && placement.StaticPlacement != nil {
		namespace = placement.StaticPlacement.Namespace
	}
	return namespace
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
			ScheduleToCloseTimeout: 1 * time.Minute,
			RetryPolicy: &temporal.RetryPolicy{
				MaximumAttempts: 1,
			},
		})
		ensureReq := EnsureInstallationRequest{Name: name, Namespace: namespace}
		var deployed bool
		if err := workflow.ExecuteActivity(activityCtx, w.EnsureInstallation, ensureReq).Get(ctx, &deployed); err != nil {
			logger.Error("unable to ensure environment, trying again", "error", err)
			continue
		}
		if !deployed {
			installCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
				ScheduleToCloseTimeout: 10 * time.Minute,
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
			if err := workflow.ExecuteActivity(installCtx, w.InstallApplication, installReq).Get(ctx, &installResp); err != nil {
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
	ownerRequirement, err := labels.NewRequirement("owner", selection.Equals, []string{"helm"})
	if err != nil {
		return false, err
	}
	nameRequirement, err := labels.NewRequirement("name", selection.Equals, []string{req.Name})
	if err != nil {
		return false, err
	}

	listOptions := &client.ListOptions{
		LabelSelector: labels.NewSelector().Add(*ownerRequirement, *nameRequirement),
		Namespace:     req.Namespace,
	}
	var configMaps v1.ConfigMapList
	if err := w.Client.List(ctx, &configMaps, listOptions); err != nil {
		return false, err
	}
	if len(configMaps.Items) == 0 {
		return false, nil
	}

	return true, nil
}
