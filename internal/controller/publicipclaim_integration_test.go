/*
Copyright 2025.

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
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkv1alpha1 "serialx.net/cilium-dhcp-wanip-operator/api/v1alpha1"
)

func TestPublicIPClaimReconcilerMockSSHIntegration(t *testing.T) {
	g := gomega.NewWithT(t)

	ctx := context.Background()
	scheme := runtime.NewScheme()
	g.Expect(corev1.AddToScheme(scheme)).To(gomega.Succeed())
	g.Expect(networkv1alpha1.AddToScheme(scheme)).To(gomega.Succeed())

	sshKey, pubKey, err := generateClientKeyPair()
	g.Expect(err).NotTo(gomega.HaveOccurred())

	responses := []*mockExecResponse{
		{
			ExpectContains: "/usr/local/bin/alloc_public_ip.sh",
			Output: `interface ready
203.0.113.77
`,
			ExitStatus: 0,
		},
		{
			ExpectContains: "kill $(cat \"$PID_FILE\")",
			Output:         "cleanup done\n",
			ExitStatus:     0,
		},
	}

	sshServer, err := newMockSSHServer(pubKey, responses)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(func() {
		g.Expect(sshServer.Close()).To(gomega.Succeed())
	})

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "router-key",
			Namespace: "kube-system",
		},
		Data: map[string][]byte{
			"id_rsa": sshKey,
		},
	}

	pool := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumLoadBalancerIPPool",
		"metadata": map[string]interface{}{
			"name": "internet-pool",
		},
		"spec": map[string]interface{}{
			"blocks": []interface{}{},
		},
	}}

	dynClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), pool)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&networkv1alpha1.PublicIPClaim{}).
		WithObjects(secret).
		Build()

	reconciler := &PublicIPClaimReconciler{
		Client:  c,
		Dynamic: dynClient,
		Scheme:  scheme,
	}

	claim := &networkv1alpha1.PublicIPClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "claim-allocation",
			Namespace: "default",
		},
		Spec: networkv1alpha1.PublicIPClaimSpec{
			PoolName: "internet-pool",
			Router: networkv1alpha1.RouterSpec{
				Host:         sshServer.Host(),
				Port:         sshServer.Port(),
				User:         "udmadmin",
				SSHSecretRef: "router-key",
				Command:      "/usr/local/bin/alloc_public_ip.sh",
				WanParent:    "eth9",
			},
		},
	}

	g.Expect(c.Create(ctx, claim)).To(gomega.Succeed())

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}}

	res, err := reconciler.Reconcile(ctx, req)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(res.RequeueAfter).To(gomega.BeNumerically(">", 0))

	res, err = reconciler.Reconcile(ctx, req)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(res.RequeueAfter).To(gomega.Equal(time.Duration(0)))

	updated := &networkv1alpha1.PublicIPClaim{}
	g.Expect(c.Get(ctx, req.NamespacedName, updated)).To(gomega.Succeed())

	g.Expect(updated.Status.Phase).To(gomega.Equal(networkv1alpha1.ClaimPhaseReady))
	g.Expect(updated.Status.AssignedIP).To(gomega.Equal("203.0.113.77"))
	g.Expect(updated.Status.WanInterface).To(gomega.HavePrefix("wan-"))
	g.Expect(updated.Status.MacAddress).To(gomega.MatchRegexp(`^02:[0-9a-f]{2}(:[0-9a-f]{2}){4}$`))

	poolObj, err := dynClient.Resource(schema.GroupVersionResource{
		Group:    "cilium.io",
		Version:  "v2",
		Resource: "ciliumloadbalancerippools",
	}).Get(ctx, "internet-pool", metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	blocks, found, err := unstructured.NestedSlice(poolObj.Object, "spec", "blocks")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(found).To(gomega.BeTrue())
	g.Expect(blocks).To(gomega.HaveLen(1))
	g.Expect(blocks[0].(map[string]interface{})["cidr"]).To(gomega.Equal("203.0.113.77/32"))

	commands := sshServer.Commands()
	g.Expect(commands).To(gomega.HaveLen(1))
	g.Expect(commands[0]).To(gomega.ContainSubstring(fmt.Sprintf("WAN_PARENT=\"%s\"", claim.Spec.Router.WanParent)))
	g.Expect(commands[0]).To(gomega.ContainSubstring(fmt.Sprintf("WAN_IF=\"%s\"", updated.Status.WanInterface)))
	g.Expect(commands[0]).To(gomega.ContainSubstring(fmt.Sprintf("WAN_MAC=\"%s\"", updated.Status.MacAddress)))

	g.Expect(updated.Finalizers).To(gomega.ContainElement(finalizerName))

	now := metav1.NewTime(time.Now())
	deleted := updated.DeepCopy()
	deleted.SetDeletionTimestamp(&now)
	deleted.Status = updated.Status

	deleteClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&networkv1alpha1.PublicIPClaim{}).
		WithObjects(secret.DeepCopy(), deleted).
		Build()

	// Create new reconciler for deletion test to avoid mutating the original
	deleteReconciler := &PublicIPClaimReconciler{
		Client:  deleteClient,
		Dynamic: dynClient,
		Scheme:  scheme,
	}

	res, err = deleteReconciler.Reconcile(ctx, req)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(res.RequeueAfter).To(gomega.Equal(time.Duration(0)))

	commands = sshServer.Commands()
	g.Expect(commands).To(gomega.HaveLen(2))
	g.Expect(commands[1]).To(gomega.ContainSubstring("kill $(cat \"$PID_FILE\")"))

	poolObj, err = dynClient.Resource(schema.GroupVersionResource{
		Group:    "cilium.io",
		Version:  "v2",
		Resource: "ciliumloadbalancerippools",
	}).Get(ctx, "internet-pool", metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	blocks, found, err = unstructured.NestedSlice(poolObj.Object, "spec", "blocks")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(found).To(gomega.BeTrue())
	g.Expect(blocks).To(gomega.BeEmpty())
}

// mockExecResponse defines a canned response for SSH command execution.
// ExpectContains is an optional substring to match in the command.
// If provided and the command doesn't match, an error response is returned.
type mockExecResponse struct {
	ExpectContains string
	Output         string
	ExitStatus     uint32
}

// mockSSHServer implements a minimal SSH server for testing PublicIPClaim reconciliation.
//
// How it works:
// 1. Accepts SSH connections and authenticates using public key auth
// 2. Accepts session channels and "exec" requests
// 3. Matches incoming commands against pre-configured responses (FIFO order)
// 4. Records all executed commands for later verification
// 5. Uses sync.WaitGroup to ensure clean shutdown (all goroutines complete)
//
// Thread safety:
// - mu protects commands and responses slices (accessed by multiple connections)
// - wg tracks all active goroutines for proper cleanup on Close()
type mockSSHServer struct {
	listener  net.Listener
	host      string
	port      int
	config    *ssh.ServerConfig
	mu        sync.Mutex
	commands  []string
	responses []*mockExecResponse
	wg        sync.WaitGroup
}

func newMockSSHServer(authorized ssh.PublicKey, responses []*mockExecResponse) (*mockSSHServer, error) {
	hostKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate host key: %w", err)
	}

	signer, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		return nil, fmt.Errorf("host key signer: %w", err)
	}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), authorized.Marshal()) {
				return nil, nil
			}
			return nil, fmt.Errorf("unauthorized public key")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	addr := ln.Addr().(*net.TCPAddr)

	server := &mockSSHServer{
		listener:  ln,
		host:      addr.IP.String(),
		port:      addr.Port,
		config:    cfg,
		responses: responses,
	}

	server.wg.Add(1)
	go func() {
		defer server.wg.Done()
		server.serve()
	}()

	return server, nil
}

func (s *mockSSHServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}

		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer func() { _ = c.Close() }()
			s.handleConn(c)
		}(conn)
	}
}

func (s *mockSSHServer) handleConn(conn net.Conn) {
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, s.config)
	if err != nil {
		return
	}
	defer func() { _ = sshConn.Close() }()

	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}

		channel, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}

		s.wg.Add(1)
		go func(ch ssh.Channel, in <-chan *ssh.Request) {
			defer s.wg.Done()
			defer func() { _ = ch.Close() }()
			for req := range in {
				switch req.Type {
				case "env", "pty-req":
					if req.WantReply {
						_ = req.Reply(true, nil)
					}
				case "exec":
					if req.WantReply {
						_ = req.Reply(true, nil)
					}
					var payload struct {
						Command string
					}
					if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
						_, _ = ch.Stderr().Write([]byte(err.Error()))
						return
					}
					output, status := s.recordAndRespond(payload.Command)
					if output != "" {
						_, _ = ch.Write([]byte(output))
					}
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&struct {
						ExitStatus uint32
					}{status}))
					return
				default:
					if req.WantReply {
						_ = req.Reply(false, nil)
					}
				}
			}
		}(channel, requests)
	}
}

func (s *mockSSHServer) recordAndRespond(cmd string) (string, uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.commands = append(s.commands, cmd)
	if len(s.responses) == 0 {
		return "", 0
	}

	resp := s.responses[0]
	s.responses = s.responses[1:]
	if resp.ExpectContains != "" && !strings.Contains(cmd, resp.ExpectContains) {
		return fmt.Sprintf("unexpected command: %s\n", cmd), 1
	}
	return resp.Output, resp.ExitStatus
}

func (s *mockSSHServer) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, len(s.commands))
	copy(out, s.commands)
	return out
}

func (s *mockSSHServer) Host() string {
	return s.host
}

func (s *mockSSHServer) Port() int {
	return s.port
}

func (s *mockSSHServer) Close() error {
	err := s.listener.Close()
	s.wg.Wait()
	return err
}

func generateClientKeyPair() ([]byte, ssh.PublicKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generate rsa key: %w", err)
	}

	privDER := x509.MarshalPKCS1PrivateKey(key)
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: privDER}
	pemBytes := pem.EncodeToMemory(block)

	pub, err := ssh.NewPublicKey(&key.PublicKey)
	if err != nil {
		return nil, nil, fmt.Errorf("public key: %w", err)
	}

	return pemBytes, pub, nil
}
