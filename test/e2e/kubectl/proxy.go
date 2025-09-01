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
	"sync/atomic"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
)

var _ = SIGDescribe("Kubectl proxy", func() {
	f := framework.NewDefaultFramework("proxy")

	f.It("should use proxy when configured in kubeconfig", func(ctx context.Context) {
		// Load current config.
		currentConfig, err := framework.LoadConfig()
		framework.ExpectNoError(err, "Failed to load current cluster config")

		apiServerURL, err := url.Parse(currentConfig.Host)
		framework.ExpectNoError(err, "Failed to parse API server URL")

		// Create a reverse proxy that forwards to the real API server
		var proxyRequestCount int64
		reverseProxy := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				// Increment request counter
				atomic.AddInt64(&proxyRequestCount, 1)

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

		ginkgo.By("Starting kubectl proxy with proxy-url configured kubeconfig")

		// Reset proxy tracking
		atomic.StoreInt64(&proxyRequestCount, 0)

		// Use TestKubeconfig to start kubectl proxy with the proxy-enabled kubeconfig
		tk := e2ekubectl.NewTestKubeconfig("", "", kubeconfigPath, "", framework.TestContext.KubectlPath, f.Namespace.Name)
		cmd := tk.KubectlCmd("proxy", "-p", "0", "--disable-filter")

		// Start kubectl proxy in background and let it run briefly to make initial API calls
		stdout, _, err := framework.StartCmdAndStreamOutput(cmd)
		framework.ExpectNoError(err, "Failed to start kubectl proxy")
		defer framework.TryKill(cmd)

		// Read initial output to ensure proxy started
		buf := make([]byte, 128)
		n, err := stdout.Read(buf)
		if err != nil {
			framework.Failf("Failed to read kubectl proxy startup output: %v", err)
		}

		framework.Logf("kubectl proxy started: %s", string(buf[:n]))

		// Give kubectl proxy time to make its initial API server requests
		time.Sleep(2 * time.Second)

		// The key validation: verify that our test proxy server received requests from kubectl proxy
		// This proves that kubectl proxy is using the proxy-url configuration
		requestCount := atomic.LoadInt64(&proxyRequestCount)
		framework.Logf("Test proxy received %d requests from kubectl proxy", requestCount)

		// This is the core test - kubectl proxy should have made requests through our proxy
		gomega.Expect(requestCount).To(gomega.BeNumerically(">", 0),
			"Expected kubectl proxy to make requests through configured proxy-url - this validates the proxy-url fix")

		framework.Logf("SUCCESS: kubectl proxy used proxy-url configuration from kubeconfig")
	})
})
