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
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	networkv1alpha1 "serialx.net/cilium-dhcp-wanip-operator/api/v1alpha1"
)

var _ = Describe("PublicIPClaim Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		publicipclaim := &networkv1alpha1.PublicIPClaim{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind PublicIPClaim")
			err := k8sClient.Get(ctx, typeNamespacedName, publicipclaim)
			if err != nil && errors.IsNotFound(err) {
				resource := &networkv1alpha1.PublicIPClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					// TODO(user): Specify other spec details if needed.
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &networkv1alpha1.PublicIPClaim{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance PublicIPClaim")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &PublicIPClaimReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// First reconcile adds the finalizer and requeues
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile will perform validation
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})

			// The reconcile should handle the resource without crashing,
			// but since we're not providing required fields (poolName, router config),
			// it should set the status to Failed with validation error
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("is required"))

			// Verify the claim was updated with Failed status
			err = k8sClient.Get(ctx, typeNamespacedName, publicipclaim)
			Expect(err).NotTo(HaveOccurred())
			Expect(publicipclaim.Status.Phase).To(Equal(networkv1alpha1.ClaimPhaseFailed))
			Expect(publicipclaim.Status.Message).To(ContainSubstring("is required"))
		})
	})
})
