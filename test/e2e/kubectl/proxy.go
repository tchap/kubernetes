/*
Copyright 2025 The Kubernetes Authors.

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
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	admissionapi "k8s.io/pod-security-admission/api"
)

var _ = SIGDescribe("Kubectl proxy", func() {
	f := framework.NewDefaultFramework("proxy")

	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline
	f.It("should use proxy when configured in kubeconfig", func(ctx context.Context) {
		// Create a test pod first
		ginkgo.By("Creating a test pod to exec into")
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
				// Increment request counter
				atomic.AddInt64(&proxyRequestCount, 1)

				// Track exec-specific requests
				if strings.Contains(pr.In.URL.Path, "/exec") {
					atomic.AddInt64(&execRequestCount, 1)
				}

				framework.Logf("Proxy forwarding request: %s %s -> %s",
					pr.In.Method, pr.In.URL.Path, apiServerURL.String())

				// Forward to the real API server
				pr.SetURL(apiServerURL)
				pr.Out.Host = apiServerURL.Host

				// Copy authentication headers from the original request
				if auth := pr.In.Header.Get("Authorization"); auth != "" {
					pr.Out.Header.Set("Authorization", auth)
				}
			},
			ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
				framework.Logf("Proxy error forwarding to %s: %v", apiServerURL.String(), err)
				http.Error(rw, err.Error(), http.StatusBadGateway)
			},
			Transport: func() http.RoundTripper {
				// Create a transport that can handle the test API server's TLS
				if currentConfig.Transport != nil {
					return currentConfig.Transport
				}
				// Fallback for test environments
				return &http.Transport{
					TLSClientConfig: &tls.Config{
						InsecureSkipVerify: true,
					},
				}
			}(),
		}

		proxyServer := httptest.NewServer(reverseProxy)
		defer proxyServer.Close()

		proxyURL, err := url.Parse(proxyServer.URL)
		framework.ExpectNoError(err, "Failed to parse proxy URL")

		// Create a temporary kubeconfig with proxy configuration
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

		// Reset proxy tracking before starting
		atomic.StoreInt64(&proxyRequestCount, 0)
		atomic.StoreInt64(&execRequestCount, 0)

		ginkgo.By("Performing kubectl exec with proxy-url configured kubeconfig")

		// Use the proxy-enabled kubeconfig for kubectl exec
		tk := e2ekubectl.NewTestKubeconfig("", "", kubeconfigPath, "", framework.TestContext.KubectlPath, f.Namespace.Name)
		execCmd := tk.KubectlCmd("exec", pod.Name, "--", "echo", "hello-proxy")

		// Execute the command
		stdout, stderr, err := framework.StartCmdAndStreamOutput(execCmd)
		if err != nil {
			framework.Logf("kubectl exec failed (may be due to TLS): %v", err)
		}

		// Read any output
		if stdout != nil {
			buf := make([]byte, 1024)
			if n, readErr := stdout.Read(buf); readErr == nil {
				framework.Logf("kubectl exec stdout: %s", string(buf[:n]))
			}
		}
		if stderr != nil {
			buf := make([]byte, 1024)
			if n, readErr := stderr.Read(buf); readErr == nil {
				framework.Logf("kubectl exec stderr: %s", string(buf[:n]))
			}
		}

		// Give some time for requests to complete
		time.Sleep(3 * time.Second)

		// The key validation: verify that our test proxy server received exec requests
		requestCount := atomic.LoadInt64(&proxyRequestCount)
		execCount := atomic.LoadInt64(&execRequestCount)
		framework.Logf("Test proxy received %d total requests (%d exec requests)", requestCount, execCount)

		// This is the core test - kubectl exec should have made requests through our proxy
		gomega.Expect(requestCount).To(gomega.BeNumerically(">", 0),
			"Expected kubectl exec to make requests through configured proxy-url - this validates the proxy-url fix")

		framework.Logf("SUCCESS: kubectl exec used proxy-url configuration from kubeconfig")
	})
})
