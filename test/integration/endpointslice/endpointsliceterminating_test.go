/*
Copyright 2021 The Kubernetes Authors.

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

package endpointslice

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/pkg/controller/endpointslice"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/test/integration/framework"
	"k8s.io/kubernetes/test/utils/ktesting"
	"k8s.io/utils/ptr"
)

// TestEndpointSliceTerminating tests that terminating endpoints are included with the
// correct conditions set for ready, serving and terminating.
func TestEndpointSliceTerminating(t *testing.T) {
	testcases := []struct {
		name              string
		podStatus         corev1.PodStatus
		expectedEndpoints []discovery.Endpoint
	}{
		{
			name: "ready terminating pods",
			podStatus: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{
					{
						Type:   corev1.PodReady,
						Status: corev1.ConditionTrue,
					},
				},
				PodIP: "10.0.0.1",
				PodIPs: []corev1.PodIP{
					{
						IP: "10.0.0.1",
					},
				},
			},
			expectedEndpoints: []discovery.Endpoint{
				{
					Addresses: []string{"10.0.0.1"},
					Conditions: discovery.EndpointConditions{
						Ready:       ptr.To(false),
						Serving:     ptr.To(true),
						Terminating: ptr.To(true),
					},
				},
			},
		},
		{
			name: "not ready terminating pods",
			podStatus: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{
					{
						Type:   corev1.PodReady,
						Status: corev1.ConditionFalse,
					},
				},
				PodIP: "10.0.0.1",
				PodIPs: []corev1.PodIP{
					{
						IP: "10.0.0.1",
					},
				},
			},
			expectedEndpoints: []discovery.Endpoint{
				{
					Addresses: []string{"10.0.0.1"},
					Conditions: discovery.EndpointConditions{
						Ready:       ptr.To(false),
						Serving:     ptr.To(false),
						Terminating: ptr.To(true),
					},
				},
			},
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			// Disable ServiceAccount admission plugin as we don't have serviceaccount controller running.
			server := kubeapiservertesting.StartTestServerOrDie(t, nil, framework.DefaultTestServerFlags(), framework.SharedEtcd())
			defer server.TearDownFn()

			client, err := clientset.NewForConfig(server.ClientConfig)
			if err != nil {
				t.Fatalf("Error creating clientset: %v", err)
			}

			resyncPeriod := 12 * time.Hour
			informers := informers.NewSharedInformerFactory(client, resyncPeriod)

			tCtx := ktesting.Init(t)
			epsController := endpointslice.NewController(
				tCtx,
				informers.Core().V1().Pods(),
				informers.Core().V1().Services(),
				informers.Core().V1().Nodes(),
				informers.Discovery().V1().EndpointSlices(),
				int32(100),
				client,
				1*time.Second)

			// Start informer and controllers
			informers.Start(tCtx.Done())
			go epsController.Run(tCtx, 1)

			// Create namespace
			ns := framework.CreateNamespaceOrDie(client, "test-endpoints-terminating", t)
			defer framework.DeleteNamespaceOrDie(client, ns, t)

			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "fake-node",
				},
			}

			_, err = client.CoreV1().Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Failed to create test node: %v", err)
			}

			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: ns.Name,
					Labels: map[string]string{
						"foo": "bar",
					},
				},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{
						"foo": "bar",
					},
					Ports: []corev1.ServicePort{
						{Name: "port-443", Port: 443, Protocol: "TCP", TargetPort: intstr.FromInt32(443)},
					},
				},
			}

			_, err = client.CoreV1().Services(ns.Name).Create(context.TODO(), svc, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Failed to create test Service: %v", err)
			}

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod",
					Labels: map[string]string{
						"foo": "bar",
					},
				},
				Spec: corev1.PodSpec{
					NodeName: "fake-node",
					Containers: []corev1.Container{
						{
							Name:  "fakename",
							Image: "fakeimage",
							Ports: []corev1.ContainerPort{
								{
									Name:          "port-443",
									ContainerPort: 443,
								},
							},
						},
					},
				},
			}

			pod, err = client.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Failed to create test ready pod: %v", err)
			}

			pod.Status = testcase.podStatus
			_, err = client.CoreV1().Pods(ns.Name).UpdateStatus(context.TODO(), pod, metav1.UpdateOptions{})
			if err != nil {
				t.Fatalf("Failed to update status for test ready pod: %v", err)
			}

			// first check that endpoints are included, test should always have 1 initial endpoint
			err = wait.PollImmediate(1*time.Second, 10*time.Second, func() (bool, error) {
				esList, err := client.DiscoveryV1().EndpointSlices(ns.Name).List(context.TODO(), metav1.ListOptions{
					LabelSelector: discovery.LabelServiceName + "=" + svc.Name,
				})

				if err != nil {
					return false, err
				}

				if len(esList.Items) == 0 {
					return false, nil
				}

				numEndpoints := 0
				for _, slice := range esList.Items {
					numEndpoints += len(slice.Endpoints)
				}

				if numEndpoints > 0 {
					return true, nil
				}

				return false, nil
			})
			if err != nil {
				t.Errorf("Error waiting for endpoint slices: %v", err)
			}

			// Delete pod and check endpoints slice conditions
			err = client.CoreV1().Pods(ns.Name).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})
			if err != nil {
				t.Fatalf("Failed to delete pod in terminating state: %v", err)
			}

			// Validate that terminating the endpoint will result in the expected endpoints in EndpointSlice.
			// Use a stricter timeout value here since we should try to catch regressions in the time it takes to remove terminated endpoints.
			var endpoints []discovery.Endpoint
			err = wait.PollImmediate(1*time.Second, 10*time.Second, func() (bool, error) {
				esList, err := client.DiscoveryV1().EndpointSlices(ns.Name).List(context.TODO(), metav1.ListOptions{
					LabelSelector: discovery.LabelServiceName + "=" + svc.Name,
				})

				if err != nil {
					return false, err
				}

				if len(esList.Items) == 0 {
					return false, nil
				}

				endpoints = esList.Items[0].Endpoints
				if len(endpoints) == 0 && len(testcase.expectedEndpoints) == 0 {
					return true, nil
				}

				if len(endpoints) != len(testcase.expectedEndpoints) {
					return false, nil
				}

				if !reflect.DeepEqual(endpoints[0].Addresses, testcase.expectedEndpoints[0].Addresses) {
					return false, nil
				}

				if !reflect.DeepEqual(endpoints[0].Conditions, testcase.expectedEndpoints[0].Conditions) {
					return false, nil
				}

				return true, nil
			})
			if err != nil {
				t.Logf("actual endpoints: %v", endpoints)
				t.Logf("expected endpoints: %v", testcase.expectedEndpoints)
				t.Errorf("unexpected endpoints: %v", err)
			}
		})
	}
}

func TestEndpointSliceDisruptionTargetTerminating(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DisruptionTargetSignalsEndpointTerminating, true)

	server := kubeapiservertesting.StartTestServerOrDie(t, nil, framework.DefaultTestServerFlags(), framework.SharedEtcd())
	defer server.TearDownFn()

	client, err := clientset.NewForConfig(server.ClientConfig)
	if err != nil {
		t.Fatalf("Error creating clientset: %v", err)
	}

	resyncPeriod := 12 * time.Hour
	informers := informers.NewSharedInformerFactory(client, resyncPeriod)

	tCtx := ktesting.Init(t)
	epsController := endpointslice.NewController(
		tCtx,
		informers.Core().V1().Pods(),
		informers.Core().V1().Services(),
		informers.Core().V1().Nodes(),
		informers.Discovery().V1().EndpointSlices(),
		int32(100),
		client,
		1*time.Second)

	informers.Start(tCtx.Done())

	var wg sync.WaitGroup
	defer wg.Done()
	wg.Go(func() {
		epsController.Run(tCtx, 1)
	})

	ns := framework.CreateNamespaceOrDie(client, "test-disruption-target", t)
	defer framework.DeleteNamespaceOrDie(client, ns, t)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "fake-node"},
	}
	_, err = client.CoreV1().Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create node: %v", err)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service",
			Namespace: ns.Name,
			Labels:    map[string]string{"foo": "bar"},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"foo": "bar"},
			Ports:    []corev1.ServicePort{{Name: "port-443", Port: 443, Protocol: "TCP", TargetPort: intstr.FromInt32(443)}},
		},
	}
	_, err = client.CoreV1().Services(ns.Name).Create(context.TODO(), svc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create Service: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-pod",
			Labels: map[string]string{"foo": "bar"},
		},
		Spec: corev1.PodSpec{
			NodeName: "fake-node",
			Containers: []corev1.Container{{
				Name:  "fakename",
				Image: "fakeimage",
				Ports: []corev1.ContainerPort{{Name: "port-443", ContainerPort: 443}},
			}},
		},
	}
	pod, err = client.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create pod: %v", err)
	}

	pod.Status = corev1.PodStatus{
		Phase: corev1.PodRunning,
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		},
		PodIP:  "10.0.0.1",
		PodIPs: []corev1.PodIP{{IP: "10.0.0.1"}},
	}
	_, err = client.CoreV1().Pods(ns.Name).UpdateStatus(context.TODO(), pod, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Failed to update pod status: %v", err)
	}

	// Wait for the pod to appear as Ready in EndpointSlice.
	err = wait.PollUntilContextTimeout(tCtx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		esList, err := client.DiscoveryV1().EndpointSlices(ns.Name).List(ctx, metav1.ListOptions{
			LabelSelector: discovery.LabelServiceName + "=" + svc.Name,
		})
		if err != nil {
			return false, err
		}
		for _, slice := range esList.Items {
			for _, ep := range slice.Endpoints {
				if ptr.Deref(ep.Conditions.Ready, false) {
					return true, nil
				}
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("Timed out waiting for ready endpoint: %v", err)
	}

	// Add DisruptionTarget condition (simulates kubelet eviction/shutdown).
	pod, err = client.CoreV1().Pods(ns.Name).Get(context.TODO(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get pod: %v", err)
	}
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:   corev1.DisruptionTarget,
		Status: corev1.ConditionTrue,
		Reason: "TerminationByKubelet",
	})
	_, err = client.CoreV1().Pods(ns.Name).UpdateStatus(context.TODO(), pod, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Failed to add DisruptionTarget condition: %v", err)
	}

	// Wait for EndpointSlice to show Terminating=true, Ready=false, Serving=true.
	var endpoints []discovery.Endpoint
	err = wait.PollUntilContextTimeout(tCtx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		esList, err := client.DiscoveryV1().EndpointSlices(ns.Name).List(ctx, metav1.ListOptions{
			LabelSelector: discovery.LabelServiceName + "=" + svc.Name,
		})
		if err != nil {
			return false, err
		}
		if len(esList.Items) == 0 {
			return false, nil
		}
		endpoints = esList.Items[0].Endpoints
		if len(endpoints) != 1 {
			return false, nil
		}
		ep := endpoints[0]
		if ptr.Deref(ep.Conditions.Terminating, false) &&
			!ptr.Deref(ep.Conditions.Ready, true) &&
			ptr.Deref(ep.Conditions.Serving, false) {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		t.Logf("actual endpoints: %v", endpoints)
		t.Errorf("Expected endpoint with Terminating=true, Ready=false, Serving=true: %v", err)
	}
}
