/*
Copyright 2026 The Kubernetes Authors.

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

package kcm

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	kubecontrollermanagertesting "k8s.io/kubernetes/cmd/kube-controller-manager/app/testing"
	"k8s.io/kubernetes/test/integration/framework"
	"k8s.io/kubernetes/test/utils/ktesting"
	"k8s.io/kubernetes/test/utils/kubeconfig"
)

const (
	interval = 100 * time.Millisecond

	leaseWaitTimeout = 10 * time.Second
)

func TestRun(t *testing.T) {
	ctx := ktesting.Init(t)

	kas := kubeapiservertesting.StartTestServerOrDie(t, nil, framework.DefaultTestServerFlags(), framework.SharedEtcd())
	defer kas.TearDownFn()

	kubeConfigFile := createKubeConfigFileForRestConfig(t, kas.ClientConfig)

	kcm := kubecontrollermanagertesting.StartTestServerOrDie(t, ctx, []string{
		"--kubeconfig=" + kubeConfigFile,
		"--controllers=", // We don't need any controllers, actually.
		"--leader-elect-lease-duration=60s",
	})
	defer kcm.TearDownFn()

	// Wait until the lease is acquired.
	client := clientset.NewForConfigOrDie(kas.ClientConfig)
	if err := wait.PollUntilContextTimeout(ctx, interval, leaseWaitTimeout, true, func(ctx context.Context) (bool, error) {
		_, err := client.CoordinationV1().Leases("kube-system").Get(ctx, "kube-controller-manager", metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}); err != nil {
		t.Fatal("Leader election lock not acquired in time:", err)
	}

	// Stop KCM.
	kcm.TearDownFn()

	// Make sure the lease is released within certain time window.
	if err := wait.PollUntilContextTimeout(ctx, interval, leaseWaitTimeout, true, func(ctx context.Context) (bool, error) {
		// The lease is not actually deleted, but leaseDurationSeconds is set to 1.
		lease, err := client.CoordinationV1().Leases("kube-system").Get(ctx, "kube-controller-manager", metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return lease.Spec.LeaseDurationSeconds != nil && *lease.Spec.LeaseDurationSeconds == 1, nil
	}); err != nil {
		t.Fatal("Leader election lock not released in time:", err)
	}
}

func createKubeConfigFileForRestConfig(t *testing.T, restConfig *rest.Config) string {
	t.Helper()

	clientConfig := kubeconfig.CreateKubeConfig(restConfig)
	kubeConfigFile := filepath.Join(t.TempDir(), "kubeconfig.yaml")
	if err := clientcmd.WriteToFile(*clientConfig, kubeConfigFile); err != nil {
		t.Fatal(err)
	}

	return kubeConfigFile
}
