package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go.temporal.io/sdk/activity"
	"reflect"

	"helm.sh/helm/v3/pkg/chart/loader"

	"github.com/ethanfrogers/k8s-application-operator/api/v1alpha1"

	"helm.sh/helm/v3/pkg/storage/driver"

	"k8s.io/cli-runtime/pkg/genericclioptions"

	"helm.sh/helm/v3/pkg/action"
)

type HelmReconciler struct {
	ReleaseName     string
	Config          *action.Configuration
	TargetArtifacts []v1alpha1.Artifact
	TargetNamespace string
}

func NewHelmReconciler(ctx context.Context, client genericclioptions.RESTClientGetter, name, namespace string, targetArtifacts []v1alpha1.Artifact) (*HelmReconciler, error) {
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
		TargetArtifacts: targetArtifacts,
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

	chartArtifact, err := findFirstArtifact(hr.TargetArtifacts, "HelmChart")
	if err != nil {
		return false, fmt.Errorf("no artifacts of type HelmChart found")
	}

	if lastDeployedRelease.Chart.Metadata.Version != chartArtifact.Version {
		return true, nil
	}

	valuesArtifacts, err := findAllArtifacts(hr.TargetArtifacts, "HelmValues")
	if err != nil {
		return false, err
	}

	mergedValues, err := getMergedValues(valuesArtifacts)
	if !reflect.DeepEqual(mergedValues, lastDeployedRelease.Config) {
		return true, nil
	}

	return false, nil
}

func getMergedValues(artifacts []v1alpha1.Artifact) (map[string]interface{}, error) {
	base := map[string]interface{}{}
	for _, a := range artifacts {
		j, err := a.Values.MarshalJSON()
		if err != nil {
			return nil, err
		}
		var unmarshaled map[string]interface{}
		if err := json.NewDecoder(bytes.NewReader(j)).Decode(&unmarshaled); err != nil {
			return nil, err
		}
		base = mergeMaps(unmarshaled, base)
	}
	return base, nil
}

func mergeMaps(a, b map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(a))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		if v, ok := v.(map[string]interface{}); ok {
			if bv, ok := out[k]; ok {
				if bv, ok := bv.(map[string]interface{}); ok {
					out[k] = mergeMaps(bv, v)
					continue
				}
			}
		}
		out[k] = v
	}
	return out
}

func (hr *HelmReconciler) Push(ctx context.Context) error {
	isUpgrade := true
	_, err := hr.Config.Releases.Deployed(hr.ReleaseName)
	if errors.Is(err, driver.ErrNoDeployedReleases) {
		isUpgrade = false
	}

	chartArtifact, err := findFirstArtifact(hr.TargetArtifacts, "HelmChart")
	if err != nil {
		return err
	}

	chartPath, cleanup, err := downloadChartArtifact(ctx, chartArtifact.Repository, chartArtifact.Version)
	if err != nil {
		return fmt.Errorf("failed to download chart: %w", err)
	}
	defer cleanup()

	valuesArtifacts, err := findAllArtifacts(hr.TargetArtifacts, "HelmValues")
	if err != nil {
		return err
	}

	mergedValues, err := getMergedValues(valuesArtifacts)
	if err != nil {
		return err
	}

	if isUpgrade {
		return hr.doUpgrade(ctx, chartPath, mergedValues)
	}

	return hr.doInstall(ctx, chartPath, mergedValues)
}

func (hr *HelmReconciler) doInstall(ctx context.Context, chartPath string, values map[string]interface{}) error {
	installAction := action.NewInstall(hr.Config)
	installAction.Namespace = hr.TargetNamespace
	installAction.ReleaseName = hr.ReleaseName
	chart, err := loader.Load(chartPath)
	if err != nil {
		return err
	}
	_, err = installAction.Run(chart, values)
	return err
}

func (hr *HelmReconciler) doUpgrade(ctx context.Context, chartPath string, values map[string]interface{}) error {
	upgradeAction := action.NewUpgrade(hr.Config)
	upgradeAction.Namespace = hr.TargetNamespace
	chart, err := loader.Load(chartPath)
	if err != nil {
		return err
	}

	_, err = upgradeAction.Run(hr.ReleaseName, chart, values)
	return err
}
