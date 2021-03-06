/*
Copyright 2019 The Rook Authors. All rights reserved.

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

package machinedisruption

import (
	"context"
	"fmt"
	"time"

	"github.com/coreos/pkg/capnslog"
	healthchecking "github.com/openshift/machine-api-operator/pkg/apis/healthchecking/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	cephClient "github.com/rook/rook/pkg/daemon/ceph/client"
	cephCluster "github.com/rook/rook/pkg/operator/ceph/cluster"
	"github.com/rook/rook/pkg/operator/ceph/disruption/controllerconfig"
	"github.com/rook/rook/pkg/operator/ceph/disruption/machinelabel"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	controllerName                  = "machinedisruption-controller"
	MDBCephClusterNamespaceLabelKey = "rook.io/cephClusterNamespace"
	MDBCephClusterNameLabelKey      = "rook.io/cephClusterName"
)

var logger = capnslog.NewPackageLogger("github.com/rook/rook", controllerName)

// MachineDisruptionReconciler reconciles MachineDisruption
type MachineDisruptionReconciler struct {
	scheme  *runtime.Scheme
	client  client.Client
	context *controllerconfig.Context
}

// Reconcile is the implementation of reconcile function for MachineDisruptionReconciler
// which ensures that the machineDisruptionBudget for the rook ceph cluster is in correct state
func (r *MachineDisruptionReconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	logger.Debugf("reconciling %s", request.NamespacedName)

	// Fetching the cephCluster
	cephClusterInstance := &cephv1.CephCluster{}
	err := r.client.Get(context.TODO(), request.NamespacedName, cephClusterInstance)
	if errors.IsNotFound(err) {
		logger.Infof("cephCluster instance not found for %s", request.NamespacedName)
		return reconcile.Result{}, nil
	} else if err != nil {
		logger.Errorf("could not fetch cephCluster %s: %+v", request.Name, err)
		return reconcile.Result{}, err
	}

	// skipping the reconcile since the feature is switched off
	if !cephClusterInstance.Spec.DisruptionManagement.ManageMachineDisruptionBudgets {
		logger.Debugf("Skipping reconcile for cephCluster %s as manageMachineDisruption is turned off", request.NamespacedName)
		return reconcile.Result{}, nil
	}

	mdb := &healthchecking.MachineDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      generateMDBInstanceName(request.Name, request.Namespace),
			Namespace: cephClusterInstance.Spec.DisruptionManagement.MachineDisruptionBudgetNamespace,
		},
	}

	err = r.client.Get(context.TODO(), types.NamespacedName{Name: mdb.GetName(), Namespace: mdb.GetNamespace()}, mdb)
	if errors.IsNotFound(err) {
		// If the MDB is not found creating the MDB for the cephCluster
		maxUnavailable := int32(0)
		// Generating the MDB instance for the cephCluster
		newMDB := &healthchecking.MachineDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      generateMDBInstanceName(request.Name, request.Namespace),
				Namespace: cephClusterInstance.Spec.DisruptionManagement.MachineDisruptionBudgetNamespace,
				Labels: map[string]string{
					MDBCephClusterNamespaceLabelKey: request.Namespace,
					MDBCephClusterNameLabelKey:      request.Name,
				},
				OwnerReferences: []metav1.OwnerReference{cephCluster.ClusterOwnerRef(cephClusterInstance.GetName(), string(cephClusterInstance.GetUID()))},
			},
			Spec: healthchecking.MachineDisruptionBudgetSpec{
				MaxUnavailable: &maxUnavailable,
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						machinelabel.MachineFencingLabelKey:          request.Name,
						machinelabel.MachineFencingNamespaceLabelKey: request.Namespace,
					},
				},
			},
		}
		err = r.client.Create(context.TODO(), newMDB)
		if err != nil {
			logger.Errorf("failed to create mdb %+v", err)
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	} else if err != nil {
		logger.Errorf("%+v", err)
		return reconcile.Result{}, err
	}
	if mdb.Spec.MaxUnavailable == nil {
		maxUnavailable := int32(0)
		mdb.Spec.MaxUnavailable = &maxUnavailable
	}
	// Check if the cluster is clean or not
	_, isClean, err := cephClient.IsClusterClean(r.context.ClusterdContext, request.Name)
	if err != nil {
		logger.Errorf("failed to get cephCluster status %+v", err)
		maxUnavailable := int32(0)
		mdb.Spec.MaxUnavailable = &maxUnavailable
		updateErr := r.client.Update(context.TODO(), mdb)
		if err != nil {
			logger.Errorf("failed to update mdb %+v", err)
			return reconcile.Result{}, updateErr
		}
		return reconcile.Result{}, err
	}
	if isClean && *mdb.Spec.MaxUnavailable != 1 {
		maxUnavailable := int32(1)
		mdb.Spec.MaxUnavailable = &maxUnavailable
		err = r.client.Update(context.TODO(), mdb)
		if err != nil {
			logger.Errorf("failed to update mdb %+v", err)
			return reconcile.Result{}, err
		}
	} else if !isClean && *mdb.Spec.MaxUnavailable != 0 {
		maxUnavailable := int32(0)
		mdb.Spec.MaxUnavailable = &maxUnavailable
		err = r.client.Update(context.TODO(), mdb)
		if err != nil {
			logger.Errorf("failed to update mdb %+v", err)
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{Requeue: true, RequeueAfter: time.Minute}, nil
}

func generateMDBInstanceName(name, namespace string) string {
	return fmt.Sprintf("%s-%s", name, namespace)
}
