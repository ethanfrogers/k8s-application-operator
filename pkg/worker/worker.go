package worker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"k8s.io/cli-runtime/pkg/genericclioptions"

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
	ApplicationsClient      *application.Clientset
	K8sClient               *kubernetes.Clientset
	Client                  client.Client
	RestClientGetterFactory func(namespace string) (genericclioptions.RESTClientGetter, error)
}

func (w *Worker) Register(registry worker.Worker) {
	registry.RegisterWorkflow(w.Reconcile)
	registry.RegisterActivity(w.GetApplicationConfig)
	registry.RegisterActivity(w.DoDiff)
	registry.RegisterWorkflow(w.DoCheck)
	registry.RegisterActivity(w.DoPush)
	registry.RegisterWorkflow(w.EnvironmentReconciler)
	registry.RegisterActivity(w.UpdateDeployedVersions)
	registry.RegisterActivity(w.DependsOnConstraint)
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
		envReq := EnvironmentReconcilerRequest{
			SpecName:          applicationConfig.Name,
			SpecNamespace:     applicationConfig.Namespace,
			TargetEnvironment: env.Name,
		}
		manageWorkflow := workflow.ExecuteChildWorkflow(childOptions, w.EnvironmentReconciler, envReq)
		environmentFutures = append(environmentFutures, manageWorkflow)
	}

	selector := workflow.NewSelector(ctx)
	for _, f := range environmentFutures {
		selector.AddFuture(f, func(f workflow.Future) {
			if err := f.Get(ctx, nil); err != nil {
				workflow.GetLogger(ctx).Error("reconciler workflow failed", "error", err)
			}
		})
	}

	for true {
		selector.Select(ctx)
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

func findFirstArtifact(artifacts []v1alpha1.Artifact, kind string) (*v1alpha1.Artifact, error) {
	for _, a := range artifacts {
		if a.Kind == kind {
			return &a, nil
		}
	}

	return nil, fmt.Errorf("artifact of kind %s not found", kind)
}

func findAllArtifacts(artifacts []v1alpha1.Artifact, kind string) ([]v1alpha1.Artifact, error) {
	var a []v1alpha1.Artifact
	for _, artifact := range artifacts {
		if artifact.Kind == kind {
			a = append(a, artifact)
		}
	}
	if len(a) == 0 {
		return nil, fmt.Errorf("no artifacts of type %s found", kind)
	}

	return a, nil
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

type EnvironmentReconcilerRequest struct {
	SpecName, SpecNamespace string
	TargetEnvironment       string
}

var defaultActivityOptions = workflow.ActivityOptions{
	ScheduleToStartTimeout: 1 * time.Minute,
	ScheduleToCloseTimeout: 1 * time.Minute,
	RetryPolicy: &temporal.RetryPolicy{
		MaximumAttempts: 1,
	},
}

func (w *Worker) EnvironmentReconciler(ctx workflow.Context, req EnvironmentReconcilerRequest) error {
	logger := workflow.GetLogger(ctx)
	for true {
		workflow.Sleep(ctx, 1*time.Minute)
		activityContext := workflow.WithActivityOptions(ctx, defaultActivityOptions)
		var applicationConfig v1alpha1.Application
		key := types.NamespacedName{Namespace: req.SpecNamespace, Name: req.SpecName}.String()
		err := workflow.ExecuteActivity(activityContext, w.GetApplicationConfig, key).Get(ctx, &applicationConfig)
		if err != nil {
			return err
		}

		var environment *v1alpha1.Environment
		for _, env := range applicationConfig.Spec.Environments {
			if env.Name == req.TargetEnvironment {
				environment = &env
				break
			}
		}

		if environment == nil {
			return fmt.Errorf("environment with %s name is not specified", req.TargetEnvironment)
		}
		logger.Info("beginning reconciliation for environment", "environment", environment.Name)
		targetNamespace := determinePlacement(applicationConfig.ObjectMeta.Namespace, environment.Placement)
		chartArtifact, _ := findFirstArtifact(applicationConfig.Spec.Artifacts, "HelmChart")

		artifactsByName := map[string]v1alpha1.Artifact{}
		for _, a := range applicationConfig.Spec.Artifacts {
			artifactsByName[a.Name] = a
		}

		var filteredArtifacts []v1alpha1.Artifact
		for _, required := range environment.RequiredArtifacts {
			if a, ok := artifactsByName[required]; ok {
				filteredArtifacts = append(filteredArtifacts, a)
			}
		}

		ddr := DoDiffRequest{
			Name:            applicationConfig.Name,
			Namespace:       targetNamespace,
			TargetArtifacts: filteredArtifacts,
		}

		var isDiff bool
		if err := workflow.ExecuteActivity(activityContext, w.DoDiff, ddr).Get(ctx, &isDiff); err != nil {
			logger.Error(
				"failed to determine diff, trying again later",
				"error", err)
			continue
		}

		if !isDiff {
			logger.Info("no diff detected, will check again later")
			continue
		}

		doCheckReq := DoCheckRequest{
			Name:            applicationConfig.Name,
			Namespace:       applicationConfig.Namespace,
			Constraints:     environment.Constraints,
			TargetArtifacts: filteredArtifacts,
		}
		childWorkflowCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			ParentClosePolicy: enums.PARENT_CLOSE_POLICY_TERMINATE,
		})
		var checked bool
		if err := workflow.ExecuteChildWorkflow(childWorkflowCtx, w.DoCheck, doCheckReq).Get(ctx, &checked); err != nil {
			logger.Error("unable to check for promotion, trying again later", "error", err)
			continue
		}

		if !checked {
			logger.Info("some checks are not ready, cannot promote artifact.")
			continue
		}

		dpr := DoPushRequest{
			Name:            applicationConfig.Name,
			Namespace:       targetNamespace,
			TargetArtifacts: filteredArtifacts,
		}
		var success bool
		if err := workflow.ExecuteActivity(activityContext, w.DoPush, dpr).Get(ctx, &success); err != nil {
			logger.Error("failed to reconcile environment, trying again later", "error", err)
			continue
		}
		if success {
			logger.Info("updated environment")
		}

		updateReq := UpdateDeployedVersionsRequest{
			Name:             applicationConfig.Name,
			Namespace:        applicationConfig.Namespace,
			Environment:      req.TargetEnvironment,
			DeployedArtifact: *chartArtifact,
		}

		if err := workflow.ExecuteActivity(activityContext, w.UpdateDeployedVersions, updateReq); err != nil {
			logger.Error("failed to update deployed versions", "error", err)
		}
	}

	return nil
}

type DoDiffRequest struct {
	Name            string
	Namespace       string
	TargetArtifacts []v1alpha1.Artifact
}

func (w *Worker) DoDiff(ctx context.Context, req DoDiffRequest) (bool, error) {
	rcg, _ := w.RestClientGetterFactory(req.Namespace)
	hr, err := NewHelmReconciler(ctx, rcg, req.Name, req.Namespace, req.TargetArtifacts)
	if err != nil {
		return false, err
	}
	return hr.Diff(ctx)
}

type DoCheckRequest struct {
	Name, Namespace string
	Constraints     []v1alpha1.Constraint
	TargetArtifacts []v1alpha1.Artifact
}

func (w *Worker) DoCheck(ctx workflow.Context, req DoCheckRequest) (bool, error) {
	var futures []workflow.Future
	if len(req.Constraints) == 0 {
		return true, nil
	}

	for _, c := range req.Constraints {
		switch c.Kind {
		case "DependsOn":
			activityContext := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
				ScheduleToCloseTimeout: 1 * time.Minute,
				RetryPolicy: &temporal.RetryPolicy{
					MaximumAttempts: 1,
				},
			})
			req := DependsOnConstraintRequest{
				Name:            req.Name,
				Namespace:       req.Namespace,
				TargetArtifacts: req.TargetArtifacts,
				EnvironmentName: c.DependsOn.EnvironmentName,
				Artifacts:       c.DependsOn.Artifacts,
			}
			f := workflow.ExecuteActivity(activityContext, w.DependsOnConstraint, req)
			futures = append(futures, f)
		}
	}

	var checkErr error
	collectedFutures := 0
	canContinue := true
	selector := workflow.NewSelector(ctx)
	for _, f := range futures {
		selector.AddFuture(f, func(f workflow.Future) {
			collectedFutures += 1
			var canProceed bool
			if err := f.Get(ctx, &canProceed); err != nil {
				checkErr = err
			}
			if canContinue == true && !canProceed {
				canContinue = false
			}
		})
	}

	for collectedFutures < len(futures) && checkErr == nil {
		selector.Select(ctx)
	}

	if checkErr != nil {
		return false, checkErr
	}

	return canContinue, nil
}

type DependsOnConstraintRequest struct {
	Name, Namespace string
	TargetArtifacts []v1alpha1.Artifact
	EnvironmentName string
	Artifacts       []string
}

func (w *Worker) DependsOnConstraint(ctx context.Context, req DependsOnConstraintRequest) (bool, error) {
	var applicationConfig v1alpha1.Application
	namespacedName := types.NamespacedName{
		Name:      req.Name,
		Namespace: req.Namespace,
	}
	if err := w.Client.Get(ctx, namespacedName, &applicationConfig); err != nil {
		return false, fmt.Errorf("failed to get application config: %w", err)
	}

	deployedArtifacts := applicationConfig.Status.DeployedArtifacts
	if deployedArtifacts == nil {
		return false, nil
	}

	envArtifacts, ok := deployedArtifacts[req.EnvironmentName]
	if !ok {
		return false, nil
	}

	artifactsByName := map[string]v1alpha1.Artifact{}
	for _, a := range applicationConfig.Spec.Artifacts {
		artifactsByName[a.Name] = a
	}

	// for each dependent artifact, check the deployed
	// versions of those artifacts
	for _, a := range req.Artifacts {
		deployedArtifact, ok := envArtifacts[a]
		if !ok {
			return false, nil
		}

		targetArtifact, ok := artifactsByName[a]
		if !ok {
			return false, nil
		}

		if deployedArtifact.Version != targetArtifact.Version {
			return false, nil
		}

	}
	return true, nil

}

type DoPushRequest struct {
	Name            string
	Namespace       string
	TargetArtifacts []v1alpha1.Artifact
}

func (w *Worker) DoPush(ctx context.Context, req DoPushRequest) error {
	rcg, _ := w.RestClientGetterFactory(req.Namespace)
	hr, err := NewHelmReconciler(ctx, rcg, req.Name, req.Namespace, req.TargetArtifacts)
	if err != nil {
		return err
	}
	return hr.Push(ctx)
}

type UpdateDeployedVersionsRequest struct {
	Name, Namespace, Environment string
	DeployedArtifact             v1alpha1.Artifact
}

func (w *Worker) UpdateDeployedVersions(ctx context.Context, req UpdateDeployedVersionsRequest) error {
	var applicationConfig v1alpha1.Application
	namespacedName := types.NamespacedName{
		Name:      req.Name,
		Namespace: req.Namespace,
	}
	if err := w.Client.Get(ctx, namespacedName, &applicationConfig); err != nil {
		return err
	}

	deployedArtifacts := applicationConfig.Status.DeployedArtifacts
	if deployedArtifacts == nil {
		deployedArtifacts = map[string]map[string]v1alpha1.Artifact{}
	}
	if _, ok := deployedArtifacts[req.Environment]; !ok {
		deployedArtifacts[req.Environment] = map[string]v1alpha1.Artifact{}
	}
	deployedArtifacts[req.Environment][req.DeployedArtifact.Name] = req.DeployedArtifact

	applicationConfig.Status.DeployedArtifacts = deployedArtifacts

	if err := w.Client.Status().Update(ctx, &applicationConfig); err != nil {
		return err
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
