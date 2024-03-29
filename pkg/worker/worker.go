package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
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
	RestClientGetterFactory func(cluster, namespace string) (genericclioptions.RESTClientGetter, error)
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
	registry.RegisterWorkflow(w.HelmCanaryConstraint)
	registry.RegisterWorkflow(w.ReconcilePlacement)
	registry.RegisterActivity(w.DeterminePlacement)
}

type ReconcileRequest struct {
	Name, Namespace string
}

func (w *Worker) Reconcile(ctx workflow.Context, req ReconcileRequest) error {
	logger := workflow.GetLogger(ctx)
	activityContext := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		ScheduleToStartTimeout: 1 * time.Minute,
		ScheduleToCloseTimeout: 1 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 1,
		},
	})

	for true {
		workflow.Sleep(ctx, 30*time.Second)
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
		collectedFutures := 0
		var reconcileErrs []error
		for _, f := range environmentFutures {
			selector.AddFuture(f, func(f workflow.Future) {
				collectedFutures += 1
				if err := f.Get(ctx, nil); err != nil {
					reconcileErrs = append(reconcileErrs, err)
				}
			})
		}

		for collectedFutures < len(environmentFutures) {
			selector.Select(ctx)
		}

		if len(reconcileErrs) > 0 {
			for _, err := range reconcileErrs {
				logger.Error("failed reconciliation", "error", err)
			}
		}
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

	placementActivityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 1,
		},
	})

	parentNamespace := applicationConfig.Namespace
	activityExec := workflow.ExecuteActivity(placementActivityCtx, w.DeterminePlacement, parentNamespace, environment.Placement)

	var coordinates []PlacementCoordinates
	if err := activityExec.Get(ctx, &coordinates); err != nil {
		return err
	}

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

	var reconcileFutures []workflow.Future
	for _, placement := range coordinates {
		childWfOptions := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			ParentClosePolicy: enums.PARENT_CLOSE_POLICY_TERMINATE,
		})
		reconcileRequest := ReconcilePlacementRequest{
			Name:            applicationConfig.Name,
			Coordinates:     placement,
			TargetArtifacts: filteredArtifacts,
			Constraints:     environment.Constraints,
			EnvironmentName: environment.Name,
		}

		reconcileFutures = append(reconcileFutures, workflow.ExecuteChildWorkflow(childWfOptions, w.ReconcilePlacement, reconcileRequest))
	}

	selector := workflow.NewSelector(ctx)
	collectedFutures := 0
	var reconcileErrs []error
	for _, f := range reconcileFutures {
		selector.AddFuture(f, func(f workflow.Future) {
			collectedFutures += 1
			if err := f.Get(ctx, nil); err != nil {
				reconcileErrs = append(reconcileErrs, err)
			}
		})
	}

	for collectedFutures < len(reconcileFutures) {
		selector.Select(ctx)
	}

	if len(reconcileErrs) > 0 {
		for _, e := range reconcileErrs {
			logger.Info("could not reconcile placement", "error", e)
		}
		return fmt.Errorf("failed to reconcile environment %s", environment.Name)
	}

	updateReq := UpdateDeployedVersionsRequest{
		Name:              applicationConfig.Name,
		Namespace:         applicationConfig.Namespace,
		Environment:       environment.Name,
		DeployedArtifacts: filteredArtifacts,
	}

	if err := workflow.ExecuteActivity(activityContext, w.UpdateDeployedVersions, updateReq).Get(ctx, nil); err != nil {
		logger.Error("failed to update deployed versions", "error", err)
	}

	return nil
}

type ReconcilePlacementRequest struct {
	Name            string
	ConfigNamespace string
	Coordinates     PlacementCoordinates
	TargetArtifacts []v1alpha1.Artifact
	Constraints     []v1alpha1.Constraint
	EnvironmentName string
}

func (w *Worker) ReconcilePlacement(ctx workflow.Context, req ReconcilePlacementRequest) error {
	logger := workflow.GetLogger(ctx)
	activityContext := workflow.WithActivityOptions(ctx, defaultActivityOptions)

	ddr := DoDiffRequest{
		Name:            req.Name,
		Coordinates:     req.Coordinates,
		TargetArtifacts: req.TargetArtifacts,
	}

	var isDiff bool
	if err := workflow.ExecuteActivity(activityContext, w.DoDiff, ddr).Get(ctx, &isDiff); err != nil {
		logger.Error(
			"failed to determine diff, trying again later",
			"error", err)
		return err
	}

	if !isDiff {
		logger.Info("no diff detected, will check again later")
		return nil
	}

	doCheckReq := DoCheckRequest{
		Name:            req.Name,
		Coordinates:     req.Coordinates,
		Constraints:     req.Constraints,
		TargetArtifacts: req.TargetArtifacts,
	}
	childWorkflowCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		ParentClosePolicy: enums.PARENT_CLOSE_POLICY_TERMINATE,
	})
	var checked bool
	if err := workflow.ExecuteChildWorkflow(childWorkflowCtx, w.DoCheck, doCheckReq).Get(ctx, &checked); err != nil {
		logger.Error("unable to check for promotion, trying again later", "error", err)
		return err
	}

	if !checked {
		logger.Info("some checks are not ready, cannot promote artifact.")
		return nil
	}

	dpr := DoPushRequest{
		Name:            req.Name,
		Coordinates:     req.Coordinates,
		TargetArtifacts: req.TargetArtifacts,
	}
	var success bool
	if err := workflow.ExecuteActivity(activityContext, w.DoPush, dpr).Get(ctx, &success); err != nil {
		logger.Error("failed to reconcile environment, trying again later", "error", err)
		return err
	}
	if success {
		logger.Info("updated environment")
	}

	return nil
}

type DoDiffRequest struct {
	Name            string
	Coordinates     PlacementCoordinates
	TargetArtifacts []v1alpha1.Artifact
}

func (w *Worker) DoDiff(ctx context.Context, req DoDiffRequest) (bool, error) {
	rcg, _ := w.RestClientGetterFactory(req.Coordinates.Cluster, req.Coordinates.Namespace)
	hr, err := NewHelmReconciler(ctx, rcg, req.Name, req.Coordinates.Namespace, req.TargetArtifacts)
	if err != nil {
		return false, err
	}
	return hr.Diff(ctx)
}

type DoCheckRequest struct {
	Name            string
	Coordinates     PlacementCoordinates
	Constraints     []v1alpha1.Constraint
	TargetArtifacts []v1alpha1.Artifact
}

func (w *Worker) DoCheck(ctx workflow.Context, req DoCheckRequest) (bool, error) {
	logger := workflow.GetLogger(ctx)
	if len(req.Constraints) == 0 {
		logger.Info("environment has no constraints, proceeding.")
		return true, nil
	}

	var (
		statefulFutures  []futureProvider
		statelessFutures []futureProvider
	)
	for _, c := range req.Constraints {
		switch c.Kind {
		case "DependsOn":
			statelessFutures = append(statelessFutures, w.newDependsOnFutureProvider(req, *c.DependsOn, req.Coordinates))
		case "HelmCanary":
			statefulFutures = append(statefulFutures, w.newHelmCanaryFutureProvider(req, *c.HelmCanary, req.Coordinates))
		}
	}

	// stateless constraints have to execute before stateful ones can run
	canContinue, err := executeConstraints(ctx, statelessFutures)
	if err != nil {
		return false, err
	}
	if !canContinue {
		return false, nil
	}

	canContinue, err = executeConstraints(ctx, statefulFutures)
	if err != nil {
		return false, err
	}

	return canContinue, nil
}

func (w *Worker) newDependsOnFutureProvider(req DoCheckRequest, dependsOn v1alpha1.DependsOnConstraint, coordinates PlacementCoordinates) futureProvider {
	return func(ctx workflow.Context) workflow.Future {
		activityContext := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			ScheduleToCloseTimeout: 1 * time.Minute,
			RetryPolicy: &temporal.RetryPolicy{
				MaximumAttempts: 1,
			},
		})
		req := DependsOnConstraintRequest{
			Name:            req.Name,
			Namespace:       coordinates.Namespace,
			TargetArtifacts: req.TargetArtifacts,
			EnvironmentName: dependsOn.EnvironmentName,
			Artifacts:       dependsOn.Artifacts,
		}
		return workflow.ExecuteActivity(activityContext, w.DependsOnConstraint, req)
	}
}

func (w *Worker) newHelmCanaryFutureProvider(req DoCheckRequest, canaryConfig v1alpha1.HelmCanaryConstraint, coordinates PlacementCoordinates) futureProvider {
	return func(ctx workflow.Context) workflow.Future {
		childContext := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			ParentClosePolicy: enums.PARENT_CLOSE_POLICY_TERMINATE,
		})
		req := HelmCanaryRequest{
			Name:             req.Name,
			Coordinates:      coordinates,
			TargetArtifacts:  req.TargetArtifacts,
			CanaryConstraint: canaryConfig,
		}
		return workflow.ExecuteChildWorkflow(childContext, w.HelmCanaryConstraint, req)
	}

}

type futureProvider func(ctx workflow.Context) workflow.Future

func executeConstraints(ctx workflow.Context, providers []futureProvider) (bool, error) {
	var checkErr error
	collectedFutures := 0
	canContinue := true

	if len(providers) == 0 {
		return true, nil
	}

	selector := workflow.NewSelector(ctx)
	for _, provider := range providers {
		future := provider(ctx)
		selector.AddFuture(future, func(f workflow.Future) {
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

	if collectedFutures < len(providers) {
		selector.Select(ctx)
	}

	if checkErr != nil {
		return false, nil
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

type HelmCanaryRequest struct {
	Name             string
	Coordinates      PlacementCoordinates
	TargetArtifacts  []v1alpha1.Artifact
	CanaryConstraint v1alpha1.HelmCanaryConstraint
}

func (w *Worker) HelmCanaryConstraint(ctx workflow.Context, req HelmCanaryRequest) (bool, error) {
	artifacts := append([]v1alpha1.Artifact{
		{Kind: "HelmValues", Values: req.CanaryConstraint.Values},
	}, req.TargetArtifacts...)

	name := fmt.Sprintf("%s-canary", req.Name)
	canaryDuration, err := time.ParseDuration(req.CanaryConstraint.TTL)
	if err != nil {
		return false, err
	}

	activityContext := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 2,
		},
	})

	pushReq := DoPushRequest{
		Name:            name,
		Coordinates:     req.Coordinates,
		TargetArtifacts: artifacts,
	}

	if err := workflow.ExecuteActivity(activityContext, w.DoPush, pushReq).Get(ctx, nil); err != nil {
		return false, fmt.Errorf("could not push canary %w", err)
	}

	// wait for some amount of time to simulate running canary validation
	workflow.Sleep(ctx, canaryDuration)

	return true, nil
}

type DoPushRequest struct {
	Name            string
	Coordinates     PlacementCoordinates
	TargetArtifacts []v1alpha1.Artifact
}

func (w *Worker) DoPush(ctx context.Context, req DoPushRequest) error {
	rcg, _ := w.RestClientGetterFactory(req.Coordinates.Cluster, req.Coordinates.Namespace)
	hr, err := NewHelmReconciler(ctx, rcg, req.Name, req.Coordinates.Namespace, req.TargetArtifacts)
	if err != nil {
		return err
	}
	return hr.Push(ctx)
}

type UpdateDeployedVersionsRequest struct {
	Name, Namespace, Environment string
	DeployedArtifacts            []v1alpha1.Artifact
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
	for _, a := range req.DeployedArtifacts {
		deployedArtifacts[req.Environment][a.Name] = a
	}

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

type PlacementCoordinates struct {
	Cluster   string
	Namespace string
}

type PlacementData struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

func (w *Worker) DeterminePlacement(parentNamespace string, placement v1alpha1.Placement) ([]PlacementCoordinates, error) {
	if static := placement.StaticPlacement; static != nil {
		return []PlacementCoordinates{
			{Cluster: static.Cluster, Namespace: static.Namespace},
		}, nil
	}

	f, err := os.Open("clusters.json")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var data []PlacementData
	if err := json.NewDecoder(f).Decode(&data); err != nil {
		return nil, err
	}

	placementRequirements := labels.Requirements{}
	for k, v := range placement.DynamicPlacement.Selector {
		labelReq, _ := labels.NewRequirement(k, selection.Equals, []string{v})
		placementRequirements = append(placementRequirements, *labelReq)
	}
	selector := labels.NewSelector().Add(placementRequirements...)
	var coordinates []PlacementCoordinates
	for _, cluster := range data {
		if selector.Matches(labels.Set(cluster.Labels)) {
			coordinates = append(coordinates, PlacementCoordinates{
				Cluster:   cluster.Name,
				Namespace: parentNamespace,
			})
		}
	}

	return coordinates, nil

}
