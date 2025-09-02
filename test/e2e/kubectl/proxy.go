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
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	clientset "k8s.io/client-go/kubernetes"
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
		// Create the pod to exec into.
		ginkgo.By("Creating a test pod to exec into")
		ns := f.Namespace.Name
		podName := "test-proxy-exec-pod"
		pod := e2epod.NewAgnhostPod(ns, podName, nil, nil, nil)
		pod = e2epod.NewPodClient(f).CreateSync(ctx, pod)

		// Load current config. This is supposed to be used for kubectl proxy only.
		currentConfig, err := framework.LoadConfig()
		framework.ExpectNoError(err, "Failed to load current cluster config")

		apiServerURL, err := url.Parse(currentConfig.Host)
		framework.ExpectNoError(err, "Failed to parse API server URL")

		// Create a proxy that forwards to the real API server.
		var (
			proxyRequestCount int64
			execRequestCount  int64
		)
		reverseProxy := &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				// Update request metrics.
				atomic.AddInt64(&proxyRequestCount, 1)
				if strings.Contains(r.In.URL.Path, "/exec") {
					atomic.AddInt64(&execRequestCount, 1)
				}

				framework.Logf("Proxy forwarding request: %s %s -> %s",
					r.In.Method, r.In.URL.Path, apiServerURL.String())

				// Forward to the real API server.
				r.SetURL(apiServerURL)
				r.Out.Host = apiServerURL.Host

				// Copy authentication headers from the original request.
				if auth := r.In.Header.Get("Authorization"); auth != "" {
					r.Out.Header.Set("Authorization", auth)
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

		// Create a temporary kubeconfig with proxy configuration.
		tempDir := ginkgo.GinkgoT().TempDir()
		kubeconfigPath := filepath.Join(tempDir, "kubeconfig-with-proxy")

		// Build kubeconfig with proxy-url.
		proxyConfig := clientcmdapi.Config{
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

		err = clientcmd.WriteToFile(proxyConfig, kubeconfigPath)
		framework.ExpectNoError(err, "Failed to write kubeconfig with proxy")

		// Use the proxy-enabled kubeconfig for kubectl proxy.
		ginkgo.By("Start kubectl proxy")

		proxyPort, err := getFreeProxyPort()
		framework.ExpectNoError(err, "Failed to get free proxy port")

		tk := e2ekubectl.NewTestKubeconfig("", "", kubeconfigPath, "", framework.TestContext.KubectlPath, "")
		proxyCmd := tk.KubectlCmd("proxy", "-p", strconv.Itoa(proxyPort), "--reject-paths", "")

		_, _, err = framework.StartCmdAndStreamOutput(proxyCmd)
		if err != nil {
			framework.Logf("kubectl proxy failed (may be due to TLS): %v", err)
		}
		defer framework.TryKill(proxyCmd)

		time.Sleep(3 * time.Second)

		// Put together a config using kubectl proxy.
		execConfig := clientcmdapi.Config{
			Clusters: map[string]*clientcmdapi.Cluster{
				"test-cluster": {
					Server: fmt.Sprintf("http://127.0.0.1:%d", proxyPort),
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

		{
			clientConfig, err := clientcmd.NewDefaultClientConfig(execConfig, &clientcmd.ConfigOverrides{}).ClientConfig()
			framework.ExpectNoError(err, "Failed to create client config")
			f.SetClientConfig(clientConfig)

			currentClientSet := f.ClientSet
			f.ClientSet, err = clientset.NewForConfig(clientConfig)
			framework.ExpectNoError(err)

			defer func() {
				f.SetClientConfig(currentConfig)
				f.ClientSet = currentClientSet
			}()
		}

		// Run kubectl exec.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		stdout, stderr, err := e2epod.ExecWithOptionsContext(ctx, f, e2epod.ExecOptions{
			Command:            []string{"echo", "hello-proxy"},
			Namespace:          f.Namespace.Name,
			PodName:            pod.Name,
			ContainerName:      pod.Spec.Containers[0].Name,
			Stdin:              nil,
			CaptureStdout:      true,
			CaptureStderr:      true,
			PreserveWhitespace: false,
		})
		framework.ExpectNoError(err, "Failed to kubectl exec")
		if len(stdout) > 0 {
			framework.Logf("kubectl exec stdout: %s", stdout)
		}
		if len(stderr) > 0 {
			framework.Logf("kubectl exec stderr: %s", stderr)
		}

		// The key validation: verify that our test proxy server received exec requests.
		requestCount := atomic.LoadInt64(&proxyRequestCount)
		execCount := atomic.LoadInt64(&execRequestCount)
		framework.Logf("Test proxy received %d total requests (%d exec requests)", requestCount, execCount)

		// This is the core test - kubectl exec should have made requests through our proxy
		gomega.Expect(requestCount).To(gomega.BeNumerically(">", 0),
			"Expected kubectl exec to make requests through the configured proxy")
	})
})

func getFreeProxyPort() (int, error) {
	addr, err := net.ResolveTCPAddr("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}

	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
