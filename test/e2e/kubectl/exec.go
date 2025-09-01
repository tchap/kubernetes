/*
Copyright 2024 The Kubernetes Authors.

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

// OWNER = sig/cli

package kubectl

import (
	"context"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	admissionapi "k8s.io/pod-security-admission/api"
)

func worker(f *framework.Framework, pod *v1.Pod, id int, jobs <-chan int, results chan<- error) {
	for j := range jobs {
		framework.Logf("Worker: %d Job: %d", id, j)
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			stdout, stderr, err := e2epod.ExecWithOptionsContext(ctx, f, e2epod.ExecOptions{
				Command:            []string{"date"},
				Namespace:          f.Namespace.Name,
				PodName:            pod.Name,
				ContainerName:      pod.Spec.Containers[0].Name,
				Stdin:              nil,
				CaptureStdout:      true,
				CaptureStderr:      true,
				PreserveWhitespace: false,
			})
			if err != nil {
				framework.Logf("Try: %d Error: %v stdout: %s stderr: %s", j, err, stdout, stderr)
			}
			results <- err
		}()
	}
}

var _ = SIGDescribe("Kubectl exec", func() {
	f := framework.NewDefaultFramework("exec")

	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline
	f.It("should be able to execute 1000 times in a container", func(ctx context.Context) {
		const size = 1000
		ns := f.Namespace.Name
		podName := "test-exec-pod"
		jobs := make(chan int, size)
		results := make(chan error, size)

		pod := e2epod.NewAgnhostPod(ns, podName, nil, nil, nil)
		pod = e2epod.NewPodClient(f).CreateSync(ctx, pod)

		// 10 workers for 1000 executions
		ginkgo.By("Starting workers to exec on pod")
		for w := 0; w < 10; w++ {
			framework.Logf("Starting worker %d", w)
			go worker(f, pod, w, jobs, results)
		}
		for i := 0; i < size; i++ {
			framework.Logf("Sending job %d", i)
			jobs <- i
		}
		ginkgo.By("All jobs processed")
		close(jobs)

		errors := []error{}
		for c := 0; c < size; c++ {
			framework.Logf("Getting results %d", c)
			err := <-results
			if err != nil {
				errors = append(errors, err)
			}
		}
		// Accept a 99% success rate to be able to handle infrastructure errors
		if len(errors) > (size*1)/100 {
			framework.Failf("Exec failed %d times with following errors : %v", len(errors), errors)
		}
	})

	f.It("should use proxy when configured in kubeconfig", func(ctx context.Context) {
		// Create a pod to exec into.
		ns := f.Namespace.Name
		podName := "test-proxy-exec-pod"
		pod := e2epod.NewAgnhostPod(ns, podName, nil, nil, nil)
		pod = e2epod.NewPodClient(f).CreateSync(ctx, pod)

		// Load current config.
		currentConfig, err := framework.LoadConfig()
		framework.ExpectNoError(err, "Failed to load current cluster config")

		apiServerURL, err := url.Parse(currentConfig.Host)
		framework.ExpectNoError(err, "Failed to parse API server URL")

		// Create a reverse proxy that forwards to the real API server
		var (
			proxyRequestCount int64
			execRequestCount  int64
		)
		reverseProxy := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				// Increment relevant counters.
				atomic.AddInt64(&proxyRequestCount, 1)
				if strings.Contains(pr.In.URL.Path, "/exec") {
					atomic.AddInt64(&execRequestCount, 1)
				}

				// Forward to the real API server.
				pr.SetURL(apiServerURL)
				pr.Out.Host = apiServerURL.Host
			},
			Transport: currentConfig.Transport,
		}

		proxyServer := httptest.NewServer(reverseProxy)
		defer proxyServer.Close()

		proxyURL, err := url.Parse(proxyServer.URL)
		framework.ExpectNoError(err, "Failed to parse proxy URL")

		// Create a temporary kubeconfig with proxy configuration.
		tempDir := ginkgo.GinkgoT().TempDir()
		kubeconfigPath := filepath.Join(tempDir, "kubeconfig-with-proxy")

		// Build kubeconfig with proxy-url
		config := &clientcmdapi.Config{
			Clusters: map[string]*clientcmdapi.Cluster{
				"test-cluster": {
					Server:                   currentConfig.Host,
					CertificateAuthorityData: currentConfig.CAData,
					ProxyURL:                 proxyURL.String(),
				},
			},
			Contexts: map[string]*clientcmdapi.Context{
				"test-context": {
					Cluster:  "test-cluster",
					AuthInfo: "test-user",
				},
			},
			AuthInfos: map[string]*clientcmdapi.AuthInfo{
				"test-user": {
					ClientCertificateData: currentConfig.CertData,
					ClientKeyData:         currentConfig.KeyData,
					Token:                 currentConfig.BearerToken,
				},
			},
			CurrentContext: "test-context",
		}

		err = clientcmd.WriteToFile(*config, kubeconfigPath)
		framework.ExpectNoError(err, "Failed to write kubeconfig with proxy")

		// Save original KUBECONFIG and restore it later
		originalKubeconfig := os.Getenv("KUBECONFIG")
		defer func() {
			if originalKubeconfig != "" {
				os.Setenv("KUBECONFIG", originalKubeconfig)
			} else {
				os.Unsetenv("KUBECONFIG")
			}
		}()

		ginkgo.By("Setting KUBECONFIG to use proxy configuration")
		os.Setenv("KUBECONFIG", kubeconfigPath)

		// Reset proxy tracking
		atomic.StoreInt64(&proxyRequestCount, 0)
		atomic.StoreInt64(&execRequestCount, 0)

		ginkgo.By("Executing command through proxy-configured kubectl")

		// Use kubectl exec with proxy configuration
		// This should go through our proxy server
		stdout, stderr, err := e2epod.ExecWithOptionsContext(ctx, f, e2epod.ExecOptions{
			Command:            []string{"echo", "hello-proxy"},
			Namespace:          ns,
			PodName:            pod.Name,
			ContainerName:      pod.Spec.Containers[0].Name,
			Stdin:              nil,
			CaptureStdout:      true,
			CaptureStderr:      true,
			PreserveWhitespace: false,
		})

		framework.Logf("Exec result - stdout: %s, stderr: %s, err: %v", stdout, stderr, err)
		framework.Logf("Proxy received %d requests (%d exec requests)",
			atomic.LoadInt64(&proxyRequestCount), atomic.LoadInt64(&execRequestCount))

		// The exec should succeed
		framework.ExpectNoError(err, "kubectl exec should succeed even with proxy")

		// Verify the command output
		gomega.Expect(strings.TrimSpace(stdout)).To(gomega.Equal("hello-proxy"))

		// Most importantly: verify that the proxy server was used
		gomega.Expect(int(atomic.LoadInt64(&proxyRequestCount))).To(gomega.BeNumerically(">", 0),
			"Expected proxy server to receive requests during kubectl exec")

		// Verify that exec-specific requests went through the proxy
		gomega.Expect(atomic.LoadInt64(&execRequestCount)).To(gomega.Equal(atomic.LoadInt64(&proxyRequestCount)),
			"Expected exec requests to match proxy requests")
	})
})
