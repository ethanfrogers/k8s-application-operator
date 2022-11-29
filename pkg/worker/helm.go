package worker

import (
	"context"
	"errors"
	"fmt"

	"go.temporal.io/sdk/activity"

	"helm.sh/helm/v3/pkg/chart/loader"

	"github.com/ethanfrogers/k8s-application-operator/api/v1alpha1"

	"helm.sh/helm/v3/pkg/storage/driver"

	"k8s.io/cli-runtime/pkg/genericclioptions"

	"helm.sh/helm/v3/pkg/action"
)

type HelmReconciler struct {
	ReleaseName     string
	Config          *action.Configuration
	TargetArtifact  v1alpha1.Artifact
	TargetNamespace string
}

func NewHelmReconciler(ctx context.Context, client genericclioptions.RESTClientGetter, name, namespace string, targetArtifact v1alpha1.Artifact) (*HelmReconciler, error) {
	logger := activity.GetLogger(ctx)
	cfg := &action.Configuration{}
	logFunc := action.DebugLog(func(format string, v ...interface{}) {
		logger.Info(fmt.Sprintf(format, v...))
	})
	if err := cfg.Init(client, namespace, "configmaps", logFunc); err != nil {
		return nil, err
	}
	return &HelmReconciler{
		Config:          cfg,
		TargetNamespace: namespace,
		ReleaseName:     name,
		TargetArtifact:  targetArtifact,
	}, nil
}

func (hr *HelmReconciler) Diff(ctx context.Context) (bool, error) {
	lastDeployedRelease, err := hr.Config.Releases.Deployed(hr.ReleaseName)
	if err != nil {
		if errors.Is(err, driver.ErrNoDeployedReleases) {
			return true, nil
		}
		return false, err
	}

	if lastDeployedRelease.Chart.Metadata.Version != hr.TargetArtifact.Version {
		return true, nil
	}

	return false, nil
}

func (hr *HelmReconciler) Push(ctx context.Context) error {
	isUpgrade := true
	_, err := hr.Config.Releases.Deployed(hr.ReleaseName)
	if errors.Is(err, driver.ErrNoDeployedReleases) {
		isUpgrade = false
	}

	chartPath, cleanup, err := downloadChartArtifact(ctx, hr.TargetArtifact.Repository, hr.TargetArtifact.Version)
	if err != nil {
		return fmt.Errorf("failed to download chart: %w", err)
	}
	defer cleanup()

	if isUpgrade {
		return hr.doUpgrade(ctx, chartPath)
	}

	return hr.doInstall(ctx, chartPath)
}

func (hr *HelmReconciler) doInstall(ctx context.Context, chartPath string) error {
	installAction := action.NewInstall(hr.Config)
	installAction.Namespace = hr.TargetNamespace
	installAction.ReleaseName = hr.ReleaseName
	chart, err := loader.Load(chartPath)
	if err != nil {
		return err
	}
	_, err = installAction.Run(chart, map[string]interface{}{})
	return err
}

func (hr *HelmReconciler) doUpgrade(ctx context.Context, chartPath string) error {
	upgradeAction := action.NewUpgrade(hr.Config)
	upgradeAction.Namespace = hr.TargetNamespace
	chart, err := loader.Load(chartPath)
	if err != nil {
		return err
	}

	_, err = upgradeAction.Run(hr.ReleaseName, chart, map[string]interface{}{})
	return err
}
