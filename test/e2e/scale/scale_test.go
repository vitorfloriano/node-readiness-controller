//go:build scale
// +build scale

/*
Copyright The Kubernetes Authors.

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

package scale

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var _ = Describe("Node Readiness Controller Scale Tests", func() {
	var (
		cmd        *exec.Cmd
		clientset  *kubernetes.Clientset
		kubeCtx    context.Context
		kubeCancel context.CancelFunc
	)

	BeforeEach(func() {
		kubeCtx, kubeCancel = context.WithCancel(context.Background())

		// Create clientset to communicate with API server
		config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		Expect(err).NotTo(HaveOccurred(), "Failed to build client-go config")
		clientset, err = kubernetes.NewForConfig(config)
		Expect(err).NotTo(HaveOccurred(), "Failed to create clientset")

		// Start the controller manager process
		By("starting the controller manager process")
		cmd = exec.Command(controllerBinaryPath,
			"--metrics-bind-address=:8080",
			"--health-probe-bind-address=:8081",
			"--leader-elect=false",
			"--enable-webhook=false",
		)
		cmd.Env = append(cmd.Environ(), "KUBECONFIG="+kubeconfigPath)

		// Start the process asynchronously
		err = cmd.Start()
		Expect(err).NotTo(HaveOccurred(), "Failed to start controller manager process")

		// Wait for the metrics endpoint to become ready
		By("waiting for the controller metrics port to bind")
		Eventually(func(g Gomega) {
			resp, err := http.Get("http://localhost:8080/metrics")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
		}, "10s", "500ms").Should(Succeed(), "Controller failed to start or bind port 8080")
	})

	AfterEach(func() {
		// Clean up the controller process
		if cmd != nil && cmd.Process != nil {
			By("stopping the controller manager process")
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}

		if kubeCancel != nil {
			kubeCancel()
		}

		// Clean up nodes scaled during the test
		By("cleaning up simulated nodes")
		cleanupCmd := exec.Command("kwokctl", "scale", "node", "kwok-node", "--replicas", "0", "--name", "nrr-scale-test")
		_ = cleanupCmd.Run()
	})

	It("should scale nodes and verify they are present", func() {
		By("creating a base node")
		baseNode := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "kwok-node",
				Labels: map[string]string{
					"type": "kwok",
				},
			},
		}
		_, err := clientset.CoreV1().Nodes().Create(kubeCtx, baseNode, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred(), "Failed to create base node")
		}

		By("scaling simulated nodes to 5")
		scaleCmd := exec.Command("kwokctl", "scale", "node", "kwok-node", "--replicas", "5", "--name", "nrr-scale-test")
		output, err := scaleCmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Failed to scale nodes: %s", string(output)))

		By("verifying nodes are created in Kubernetes")
		Eventually(func(g Gomega) {
			nodes, err := clientset.CoreV1().Nodes().List(kubeCtx, metav1.ListOptions{})
			g.Expect(err).NotTo(HaveOccurred())

			// Filter for simulated nodes
			count := 0
			for _, node := range nodes.Items {
				if node.Labels["type"] == "kwok" || node.Labels["kubernetes.io/hostname"] != "" {
					count++
				}
			}
			g.Expect(count).To(BeNumerically(">=", 5))
		}, "15s", "1s").Should(Succeed())
	})
})
