/*
Copyright 2024.

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

package controller

import (
	"context"
	"fmt"
	"os"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cachev1alpha1 "github.com/egarciam/memcached-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const memcachedFinalizer = "github.egarciam.com/finalizer"

// Definitions to manage status conditions
const (
	// typeAvailableMemcached represents the status of the Deployment reconciliation
	typeAvailableMemcached = "Available"
	// typeDegradedMemcached represents the status used when the custom resource is deleted and the finalizer operations are must to occur.
	typeDegradedMemcached = "Degraded"
)

// MemcachedReconciler reconciles a Memcached object
type MemcachedReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// The following markers are used to generate the rules permissions (RBAC) on config/rbac using controller-gen
// when the command <make manifests> is executed.
// To know more about markers see: https://book.kubebuilder.io/reference/markers.html

//+kubebuilder:rbac:groups=cache.egarciam.github.com,resources=memcacheds,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cache.egarciam.github.com,resources=memcacheds/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cache.egarciam.github.com,resources=memcacheds/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Memcached object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.16.3/pkg/reconcile
func (r *MemcachedReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// TODO(user): your logic here
	//Fetch the memcached instance
	memcached := &cachev1alpha1.Memcached{}
	err := r.Get(ctx, req.NamespacedName, memcached)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If the CR is not found then, it usually means that it was deleted or not created
			// In this way, we will stop the reconciliation
			log.Info("memcached resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the api
		log.Error(err, "Failed to get memcached")
		return ctrl.Result{}, err
	}

	//Let's just set tje status as unknown where there's no status available
	if memcached.Status.Conditions == nil || len(memcached.Status.Conditions) == 0 {
		meta.SetStatusCondition(&memcached.Status.Conditions, metav1.Condition{Type: typeAvailableMemcached, Status: metav1.ConditionUnknown, Reason: "Reconciling",
			Message: "Starting reconciliation"})
		if err = r.Status().Update(ctx, memcached); err != nil {
			log.Error(err, "Failed to update memcached status")
			return ctrl.Result{}, err
		}

		//Let's refetch
		if err := r.Get(ctx, req.NamespacedName, memcached); err != nil {
			log.Error(err, "Failed to re-fetch memcached")
			return ctrl.Result{}, err
		}
	}

	// Let's add a finalizer- Then we can define some operations that can ocurr before the CR is deleted
	if !controllerutil.ContainsFinalizer(memcached, memcachedFinalizer) {
		log.Info("Adding finalizer for Memcached")
		if ok := controllerutil.AddFinalizer(memcached, memcachedFinalizer); !ok {
			log.Error(err, "failed to add finalizer to CR")
			return ctrl.Result{Requeue: true}, nil
		}

		if err = r.Update(ctx, memcached); err != nil {
			log.Error(err, "Failed to update CR to add finalizer!!")
			return ctrl.Result{}, err
		}
	}

	//Check if the memcached instance is marked to be deleted, which is indicated by
	//the deletion timestamp being set
	isMemcachedMarkedToBeDeleted := memcached.GetDeletionTimestamp() != nil
	if isMemcachedMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(memcached, memcachedFinalizer) {
			log.Info("Performing finalizer operations for memcached before delete CR")

			// Let's add here an status "Downgrade" to define that this resource begins its process to be terminated
			meta.SetStatusCondition(&memcached.Spec.Conditions, metav1.Condition{
				Type:    typeDegradedMemcached,
				Status:  metav1.ConditionUnknown,
				Reason:  "Finalizing",
				Message: fmt.Sprintf("Perfoming finalizer operations for the custom resource: %s ", memcached.Name),
			})
			if err := r.Status().Update(ctx, memcached); err != nil {
				log.Error(err, "Failed to update memcached status")
				return ctrl.Result{}, err
			}

			// Peerfomr all operations required before remove the finalizer and allow
			// kubernetes API to remove the CR
			r.doFinalizerOperationsForMemcached(memcached)

			// TODO(user): If you add operations to the doFinalizerOperationsForMemcached method
			// then you need to ensure that all worked fine before deleting and updating the Downgrade status
			// otherwise, you should requeue here.

			// Re-fetch the memcached Custom Resource before update the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raise the issue "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation

			if err := r.Get(ctx, req.NamespacedName, memcached); err != nil {
				log.Error(err, "Failed to re-fetch memcached")
				return ctrl.Result{}, err
			}

			meta.SetStatusCondition(&memcached.Status.Conditions, metav1.Condition{
				Type:    typeDegradedMemcached,
				Status:  metav1.ConditionTrue,
				Reason:  "Finalizing",
				Message: fmt.Sprintf("Finalizer opertaions for custom resource %s name were sucessfully accomplished", memcached.Name),
			})

			if err := r.Status().Update(ctx, memcached); err != nil {
				log.Error(err, "Failed to update memcached status")
				return ctrl.Result{}, err
			}

			log.Info("Removing finalizer for Memcached after succesfully perfomr the operations")
			if ok := controllerutil.RemoveFinalizer(memcached, memcachedFinalizer); !ok {
				log.Error(err, "Failed to remove finalizer for ythe memcached")
				return ctrl.Result{Requeue: true}, nil
			}

			if err := r.Update(ctx, memcached) err != nil {
				log.Error(eerr, "Failed to remove finalizer for memcached")
				return ctrl.Result{}, err			}
		}
	}

	return ctrl.Result{, nil}

}

// finalizeMemcached will perform the required operations before delete the CR.
func (r *MemcachedReconciler) doFinalizerOperationsForMemcached(cr *cachev1alpha1.Memcached) {
	// TODO(user): Add the cleanup steps that the operator
	// needs to do before the CR can be deleted. Examples
	// of finalizers include performing backups and deleting
	// resources that are not owned by this CR, like a PVC.

	// Note: It is not recommended to use finalizers with the purpose of delete resources which are
	// created and managed in the reconciliation. These ones, such as the Deployment created on this reconcile,
	// are defined as depended of the custom resource. See that we use the method ctrl.SetControllerReference.
	// to set the ownerRef which means that the Deployment will be deleted by the Kubernetes API.
	// More info: https://kubernetes.io/docs/tasks/administer-cluster/use-cascading-deletion/

	// The following implementation will raise an event
	r.Recorder.Event(cr, "Warning", "Deleting",
		fmt.Sprintf("Custom Resource %s is being deleted from the namespace %s",
			cr.Name,
			cr.Namespace))
}

// labelsForMemcached returns the labels for selecting the resources
// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
func labelsForMemcached(name string) map[string]string {
	var imageTag string
	image, err := imageForMemcached()
	if err == nil {
		imageTag = strings.Split(image, ":")[1]
	}
	return map[string]string{"app.kubernetes.io/name": "Memcached",
		"app.kubernetes.io/instance":   name,
		"app.kubernetes.io/version":    imageTag,
		"app.kubernetes.io/part-of":    "memcached-operator",
		"app.kubernetes.io/created-by": "controller-manager",
	}
}

// imageForMemcached gets the Operand image which is managed by this controller
// from the MEMCACHED_IMAGE environment variable defined in the config/manager/manager.yaml
func imageForMemcached() (string, error) {
	var imageEnvVar = "MEMCACHED_IMAGE"
	image, found := os.LookupEnv(imageEnvVar)
	if !found {
		return "", fmt.Errorf("Unable to find %s environment variable with the image", imageEnvVar)
	}
	return image, nil
}

// SetupWithManager sets up the controller with the Manager.
// Note that the Deployment will be also watched in order to ensure its
// desirable state on the cluster
func (r *MemcachedReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cachev1alpha1.Memcached{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}
