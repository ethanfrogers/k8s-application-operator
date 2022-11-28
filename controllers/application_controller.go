/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"

	"github.com/ethanfrogers/k8s-application-operator/pkg/worker"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	deploymentsv1alpha1 "github.com/ethanfrogers/k8s-application-operator/api/v1alpha1"
	temporal "go.temporal.io/sdk/client"
)

// ApplicationReconciler reconciles a Application object
type ApplicationReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	TemporalClient temporal.Client
}

const applicationFinalizer = "deployments.datadoghq.com/application-finalizer"

//+kubebuilder:rbac:groups=deployments.datadoghq.com,resources=applications,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=deployments.datadoghq.com,resources=applications/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=deployments.datadoghq.com,resources=applications/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Application object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *ApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reqLogger := log.FromContext(ctx)

	var obj deploymentsv1alpha1.Application
	if err := r.Get(ctx, req.NamespacedName, &obj); err != nil {
		if errors.IsNotFound(err) {
			reqLogger.Info("resource not found, object must have been deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if obj.ObjectMeta.DeletionTimestamp != nil {
		reqLogger.Info("object deleted, cleaning up")
		if controllerutil.ContainsFinalizer(&obj, applicationFinalizer) {
			status := obj.Status
			if err := r.TemporalClient.TerminateWorkflow(ctx, status.ControllerWorkflowID, status.ControllerWorkflowRunID, "object deletion"); err != nil {
				reqLogger.Error(err, "unable to terminate workflow")
			}
			controllerutil.RemoveFinalizer(&obj, applicationFinalizer)
			if err := r.Update(ctx, &obj); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	if !controllerutil.ContainsFinalizer(&obj, applicationFinalizer) {
		// ensure finalizers are set
		controllerutil.AddFinalizer(&obj, applicationFinalizer)
		if err := r.Update(ctx, &obj); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	status := obj.Status
	if status.ControllerWorkflowID == "" && status.ControllerWorkflowRunID == "" {
		wfInfo, err := r.startReconcilerWorkflow(ctx, req.NamespacedName)
		if err != nil {
			return ctrl.Result{Requeue: true}, err
		}

		status := deploymentsv1alpha1.ApplicationStatus{
			ControllerWorkflowID:    wfInfo.controllerWorkflowID,
			ControllerWorkflowRunID: wfInfo.controllerWorkflowRunID,
		}
		updatedObject := obj.DeepCopy()
		updatedObject.Status = status
		if err := r.Status().Update(ctx, updatedObject); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&deploymentsv1alpha1.Application{}).
		Complete(r)
}

type controllerWorkflowInfo struct {
	controllerWorkflowID, controllerWorkflowRunID string
}

func (r *ApplicationReconciler) startReconcilerWorkflow(ctx context.Context, key types.NamespacedName) (*controllerWorkflowInfo, error) {
	opts := temporal.StartWorkflowOptions{
		ID:        fmt.Sprintf("reconcile-%s", key.String()),
		TaskQueue: "application-reconciler",
	}
	req := worker.ReconcileRequest{Name: key.Name, Namespace: key.Namespace}
	execution, err := r.TemporalClient.ExecuteWorkflow(ctx, opts, "Reconcile", req)
	if err != nil {
		return nil, err
	}
	return &controllerWorkflowInfo{
		controllerWorkflowID:    execution.GetID(),
		controllerWorkflowRunID: execution.GetRunID(),
	}, nil

}
