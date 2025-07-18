/*
Copyright 2019 The Kubernetes Authors.

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

package e2e

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"
	"golang.org/x/crypto/ssh"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kubernetes/test/e2e/framework"
	fnodes "k8s.io/kubernetes/test/e2e/framework/node"
	fpod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	fpv "k8s.io/kubernetes/test/e2e/framework/pv"
	admissionapi "k8s.io/pod-security-admission/api"

	cnsoperatorv1alpha1 "sigs.k8s.io/vsphere-csi-driver/v3/pkg/apis/cnsoperator"
	cnsregistervolumev1alpha1 "sigs.k8s.io/vsphere-csi-driver/v3/pkg/apis/cnsoperator/cnsregistervolume/v1alpha1"
	k8s "sigs.k8s.io/vsphere-csi-driver/v3/pkg/kubernetes"
)

var _ = ginkgo.Describe("Basic Static Provisioning", func() {

	f := framework.NewDefaultFramework("e2e-csistaticprovision")
	f.NamespacePodSecurityEnforceLevel = admissionapi.LevelPrivileged
	framework.TestContext.DeleteNamespace = true
	var (
		client                     clientset.Interface
		namespace                  string
		fcdID                      string
		pv                         *v1.PersistentVolume
		pvc                        *v1.PersistentVolumeClaim
		defaultDatacenter          *object.Datacenter
		defaultDatastore           *object.Datastore
		mgmtDatastore              *object.Datastore
		nonsharedDatastore         *object.Datastore
		deleteFCDRequired          bool
		pandoraSyncWaitTime        int
		err                        error
		datastoreURL               string
		storagePolicyName          string
		isVsanHealthServiceStopped bool
		isSPSserviceStopped        bool
		ctx                        context.Context
		nonSharedDatastoreURL      string
		fullSyncWaitTime           int
		isQuotaValidationSupported bool
	)

	ginkgo.BeforeEach(func() {
		bootstrap()
		client = f.ClientSet
		namespace = getNamespaceToRunTests(f)
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		defer cancel()
		nodeList, err := fnodes.GetReadySchedulableNodes(ctx, f.ClientSet)
		framework.ExpectNoError(err, "Unable to find ready and schedulable Node")
		storagePolicyName = GetAndExpectStringEnvVar(envStoragePolicyNameForSharedDatastores)
		if !(len(nodeList.Items) > 0) {
			framework.Failf("Unable to find ready and schedulable Node")
		}

		if os.Getenv(envPandoraSyncWaitTime) != "" {
			pandoraSyncWaitTime, err = strconv.Atoi(os.Getenv(envPandoraSyncWaitTime))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		} else {
			pandoraSyncWaitTime = defaultPandoraSyncWaitTime
		}
		deleteFCDRequired = false
		isVsanHealthServiceStopped = false
		isSPSserviceStopped = false
		var datacenters []string
		datastoreURL = GetAndExpectStringEnvVar(envSharedDatastoreURL)
		nonSharedDatastoreURL = GetAndExpectStringEnvVar(envNonSharedStorageClassDatastoreURL)

		finder := find.NewFinder(e2eVSphere.Client.Client, false)
		cfg, err := getConfig()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		dcList := strings.Split(cfg.Global.Datacenters, ",")
		for _, dc := range dcList {
			dcName := strings.TrimSpace(dc)
			if dcName != "" {
				datacenters = append(datacenters, dcName)
			}
		}
		for _, dc := range datacenters {
			defaultDatacenter, err = finder.Datacenter(ctx, dc)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			finder.SetDatacenter(defaultDatacenter)
			defaultDatastore, err = getDatastoreByURL(ctx, datastoreURL, defaultDatacenter)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			nonsharedDatastore, err = getDatastoreByURL(ctx, nonSharedDatastoreURL, defaultDatacenter)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		if guestCluster {
			// Get a config to talk to the apiserver
			restConfig := getRestConfigClient()
			_, svNamespace := getSvcClientAndNamespace()
			setStoragePolicyQuota(ctx, restConfig, storagePolicyName, svNamespace, rqLimit)
		}

		if os.Getenv(envFullSyncWaitTime) != "" {
			fullSyncWaitTime, err := strconv.Atoi(os.Getenv(envFullSyncWaitTime))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			if fullSyncWaitTime <= 0 || fullSyncWaitTime > defaultFullSyncWaitTime {
				framework.Failf("The FullSync Wait time %v is not set correctly", fullSyncWaitTime)
			}
		} else {
			fullSyncWaitTime = defaultFullSyncWaitTime
		}

		if supervisorCluster || stretchedSVC {
			//if isQuotaValidationSupported is true then quotaValidation is considered in tests
			vcVersion = getVCversion(ctx, vcAddress)
			isQuotaValidationSupported = isVersionGreaterOrEqual(vcVersion, quotaSupportedVCVersion)
		}

	})

	ginkgo.AfterEach(func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ginkgo.By("Performing test cleanup")
		if deleteFCDRequired {
			ginkgo.By(fmt.Sprintf("Deleting FCD: %s", fcdID))

			err := e2eVSphere.deleteFCD(ctx, fcdID, defaultDatastore.Reference())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		if pvc != nil {
			framework.ExpectNoError(fpv.DeletePersistentVolumeClaim(ctx, client, pvc.Name, namespace),
				"Failed to delete PVC", pvc.Name)
		}

		if pv != nil {
			framework.ExpectNoError(fpv.WaitForPersistentVolumeDeleted(ctx, client, pv.Name, poll, pollTimeoutShort))
			framework.ExpectNoError(e2eVSphere.waitForCNSVolumeToBeDeleted(pv.Spec.CSI.VolumeHandle))
		}

		if isVsanHealthServiceStopped {
			ginkgo.By(fmt.Sprintln("Starting vsan-health on the vCenter host"))
			err = invokeVCenterServiceControl(ctx, startOperation, vsanhealthServiceName, vcAddress)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow vsan-health to come up again", vsanHealthServiceWaitTime))
			time.Sleep(time.Duration(vsanHealthServiceWaitTime) * time.Second)
		}

		if isSPSserviceStopped {
			ginkgo.By(fmt.Sprintln("Starting sps on the vCenter host"))
			err = invokeVCenterServiceControl(ctx, startOperation, spsServiceName, vcAddress)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow sps to come up again", vsanHealthServiceWaitTime))
			time.Sleep(time.Duration(vsanHealthServiceWaitTime) * time.Second)
		}

		if guestCluster {
			svcClient, svNamespace := getSvcClientAndNamespace()
			setResourceQuota(svcClient, svNamespace, rqLimit)
			dumpSvcNsEventsOnTestFailure(svcClient, svNamespace)
		}
		if supervisorCluster {
			dumpSvcNsEventsOnTestFailure(client, namespace)
		}
	})

	staticProvisioningPreSetUpUtilForVMDKTests := func(ctx context.Context) (*restclient.Config,
		*storagev1.StorageClass, string) {
		namespace = getNamespaceToRunTests(f)

		// Get a config to talk to the apiserver
		restConfig := getRestConfigClient()

		ginkgo.By("Get storage Policy")
		framework.Logf("storagePolicyName: %s", vsanDefaultStoragePolicyName)
		profileID := e2eVSphere.GetSpbmPolicyID(vsanDefaultStoragePolicyName)
		framework.Logf("Profile ID :%s", profileID)
		scParameters := make(map[string]string)
		scParameters["storagePolicyID"] = profileID
		err = client.StorageV1().StorageClasses().Delete(ctx, vsanDefaultStorageClassInSVC, metav1.DeleteOptions{})
		if !apierrors.IsNotFound(err) {
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}

		storageclass, err := createStorageClass(client, scParameters, nil, "", "", false, vsanDefaultStorageClassInSVC)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		framework.Logf("storageclass name :%s", storageclass.GetName())
		storageclass, err = client.StorageV1().StorageClasses().Get(ctx, storageclass.GetName(), metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		framework.Logf("storageclass name :%s", storageclass.GetName())

		ginkgo.By("create resource quota")
		setStoragePolicyQuota(ctx, restConfig, storageclass.GetName(), namespace, rqLimit)

		return restConfig, storageclass, profileID
	}

	testCleanUpUtil := func(ctx context.Context, restClientConfig *restclient.Config,
		cnsRegistervolume *cnsregistervolumev1alpha1.CnsRegisterVolume, namespace string,
		pvcName string, pvName string) {
		if guestCluster {
			client, _ = getSvcClientAndNamespace()
		}
		ginkgo.By("Deleting the PV Claim")
		framework.ExpectNoError(fpv.DeletePersistentVolumeClaim(ctx, client, pvcName, namespace),
			"Failed to delete PVC", pvcName)
		pvc = nil

		ginkgo.By("Verify PV should be deleted automatically")
		framework.ExpectNoError(fpv.WaitForPersistentVolumeDeleted(ctx, client, pvName, poll,
			supervisorClusterOperationsTimeout))
		pv = nil

		if cnsRegistervolume != nil {
			ginkgo.By("Verify CRD should be deleted automatically")
			framework.ExpectNoError(waitForCNSRegisterVolumeToGetDeleted(ctx, restClientConfig,
				namespace, cnsRegistervolume, poll, supervisorClusterOperationsTimeout))
		}

	}

	// This test verifies the static provisioning workflow.
	//
	// Test Steps:
	// 1. Create FCD and wait for fcd to allow syncing with pandora.
	// 2. Create PV Spec with volumeID set to FCDID created in Step-1, and
	//    PersistentVolumeReclaimPolicy is set to Delete.
	// 3. Create PVC with the storage request set to PV's storage capacity.
	// 4. Wait for PV and PVC to bound.
	// 5. Create a POD.
	// 6. Verify volume is attached to the node and volume is accessible in the pod.
	// 7. Verify container volume metadata is present in CNS cache.
	// 8. Delete POD.
	// 9. Verify volume is detached from the node.
	// 10. Delete PVC.
	// 11. Verify PV is deleted automatically.
	ginkgo.It("[csi-block-vanilla] [csi-block-vanilla-parallelized] Verify basic static provisioning "+
		"workflow", ginkgo.Label(p0, block, vanilla, core, vc70), func() {
		var err error

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ginkgo.By("Creating FCD Disk")
		fcdID, err := e2eVSphere.createFCD(ctx, "BasicStaticFCD", diskSizeInMb, defaultDatastore.Reference())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		deleteFCDRequired = true

		ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow newly created FCD:%s to sync with pandora",
			pandoraSyncWaitTime, fcdID))
		time.Sleep(time.Duration(pandoraSyncWaitTime) * time.Second)

		// Creating label for PV.
		// PVC will use this label as Selector to find PV.
		staticPVLabels := make(map[string]string)
		staticPVLabels["fcd-id"] = fcdID

		ginkgo.By("Creating the PV")
		pv = getPersistentVolumeSpec(fcdID, v1.PersistentVolumeReclaimDelete, staticPVLabels, ext4FSType)
		pv, err = client.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{})
		if err != nil {
			return
		}
		err = e2eVSphere.waitForCNSVolumeToBeCreated(pv.Spec.CSI.VolumeHandle)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Creating the PVC")
		pvc = getPersistentVolumeClaimSpec(namespace, staticPVLabels, pv.Name)
		pvc, err = client.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Wait for PV and PVC to Bind.
		framework.ExpectNoError(fpv.WaitOnPVandPVC(ctx, client, f.Timeouts, namespace, pv, pvc))

		// Set deleteFCDRequired to false.
		// After PV, PVC is in the bind state, Deleting PVC should delete
		// container volume. So no need to delete FCD directly using vSphere
		// API call.
		deleteFCDRequired = false

		ginkgo.By("Verifying CNS entry is present in cache")
		_, err = e2eVSphere.queryCNSVolumeWithResult(pv.Spec.CSI.VolumeHandle)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Creating the Pod")
		var pvclaims []*v1.PersistentVolumeClaim
		pvclaims = append(pvclaims, pvc)
		pod, err := createPod(ctx, client, namespace, nil, pvclaims, false, "")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By(fmt.Sprintf("Verify volume: %s is attached to the node: %s", pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName))
		vmUUID := getNodeUUID(ctx, client, pod.Spec.NodeName)
		isDiskAttached, err := e2eVSphere.isVolumeAttachedToVM(client, pv.Spec.CSI.VolumeHandle, vmUUID)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(isDiskAttached).To(gomega.BeTrue(), "Volume is not attached")

		ginkgo.By("Verify the volume is accessible and available to the pod by creating an empty file")
		filepath := filepath.Join("/mnt/volume1", "/emptyFile.txt")
		createEmptyFilesOnVSphereVolume(namespace, pod.Name, []string{filepath})

		ginkgo.By("Verify container volume metadata is present in CNS cache")
		ginkgo.By(fmt.Sprintf("Invoking QueryCNSVolume with VolumeID: %s", pv.Spec.CSI.VolumeHandle))
		_, err = e2eVSphere.queryCNSVolumeWithResult(pv.Spec.CSI.VolumeHandle)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		labels := []types.KeyValue{{Key: "fcd-id", Value: fcdID}}
		ginkgo.By("Verify container volume metadata is matching the one in CNS cache")
		err = verifyVolumeMetadataInCNS(&e2eVSphere, pv.Spec.CSI.VolumeHandle,
			pvc.Name, pv.ObjectMeta.Name, pod.Name, labels...)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Deleting the Pod")
		framework.ExpectNoError(fpod.DeletePodWithWait(ctx, client, pod), "Failed to delete pod", pod.Name)

		ginkgo.By(fmt.Sprintf("Verify volume is detached from the node: %s", pod.Spec.NodeName))
		isDiskDetached, err := e2eVSphere.waitForVolumeDetachedFromNode(client, pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(isDiskDetached).To(gomega.BeTrue(), "Volume is not detached from the node")

		ginkgo.By("Deleting the PV Claim")
		framework.ExpectNoError(fpv.DeletePersistentVolumeClaim(ctx, client, pvc.Name, namespace),
			"Failed to delete PVC", pvc.Name)
		pvc = nil

		ginkgo.By("Verify PV should be deleted automatically")
		framework.ExpectNoError(fpv.WaitForPersistentVolumeDeleted(ctx, client, pv.Name, poll, pollTimeout))
		pv = nil
	})

	// This test verifies the static provisioning workflow with XFS filesystem.
	//
	// Test Steps:
	// 1. Create FCD and wait for fcd to allow syncing with pandora.
	// 2. Create PV Spec with volumeID set to FCDID created in Step-1, and
	//    PersistentVolumeReclaimPolicy is set to Delete, also mention fstype as xfs.
	// 3. Create PVC with the storage request set to PV's storage capacity.
	// 4. Wait for PV and PVC to bound.
	// 5. Create a POD.
	// 6. Verify volume is attached to the node and volume is accessible in the pod.
	// 7. Verify filesystem used inside pod to mount volume is xfs.
	// 7. Verify container volume metadata is present in CNS cache.
	// 8. Delete POD.
	// 9. Verify volume is detached from the node.
	// 10. Delete PVC.
	// 11. Verify PV is deleted automatically.
	ginkgo.It("[csi-block-vanilla] [csi-block-vanilla-parallelized] Verify basic static provisioning workflow "+
		"with XFS filesystem", ginkgo.Label(p1, block, vanilla, core, vc70), func() {
		var err error

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ginkgo.By("Creating FCD Disk")
		fcdID, err := e2eVSphere.createFCD(ctx, "BasicStaticFCD", diskSizeInMb, defaultDatastore.Reference())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		deleteFCDRequired = true

		ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow newly created FCD:%s to sync with pandora",
			pandoraSyncWaitTime, fcdID))
		time.Sleep(time.Duration(pandoraSyncWaitTime) * time.Second)

		// Creating label for PV.
		// PVC will use this label as Selector to find PV.
		staticPVLabels := make(map[string]string)
		staticPVLabels["fcd-id"] = fcdID

		ginkgo.By("Creating the PV with fstype as xfs")
		pv = getPersistentVolumeSpec(fcdID, v1.PersistentVolumeReclaimDelete, staticPVLabels, xfsFSType)
		pv, err = client.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{})
		if err != nil {
			return
		}
		err = e2eVSphere.waitForCNSVolumeToBeCreated(pv.Spec.CSI.VolumeHandle)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Creating the PVC")
		pvc = getPersistentVolumeClaimSpec(namespace, staticPVLabels, pv.Name)
		pvc, err = client.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Wait for PV and PVC to Bind.
		framework.ExpectNoError(fpv.WaitOnPVandPVC(ctx, client, f.Timeouts, namespace, pv, pvc))

		// Set deleteFCDRequired to false.
		// After PV, PVC is in the bind state, Deleting PVC should delete
		// container volume. So no need to delete FCD directly using vSphere
		// API call.
		deleteFCDRequired = false

		ginkgo.By("Verifying CNS entry is present in cache")
		_, err = e2eVSphere.queryCNSVolumeWithResult(pv.Spec.CSI.VolumeHandle)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Creating the Pod")
		var pvclaims []*v1.PersistentVolumeClaim
		pvclaims = append(pvclaims, pvc)
		pod, err := createPod(ctx, client, namespace, nil, pvclaims, false, execCommand)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By(fmt.Sprintf("Verify volume: %s is attached to the node: %s", pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName))
		vmUUID := getNodeUUID(ctx, client, pod.Spec.NodeName)
		isDiskAttached, err := e2eVSphere.isVolumeAttachedToVM(client, pv.Spec.CSI.VolumeHandle, vmUUID)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(isDiskAttached).To(gomega.BeTrue(), "Volume is not attached")

		// Verify that fstype used to mount volume inside pod is xfs.
		ginkgo.By("Verify that filesystem type used to mount volume is xfs as expected")
		_, err = e2eoutput.LookForStringInPodExec(namespace, pod.Name, []string{"/bin/cat", "/mnt/volume1/fstype"},
			xfsFSType, time.Minute)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Verify that write and read is successful at mountpath.
		ginkgo.By(fmt.Sprintf("Creating file file.txt at mountpath inside pod: %v", pod.Name))
		data := "This file file.txt is written by pod " + pod.Name
		filePath := "/mnt/volume1/file.txt"
		writeDataOnFileFromPod(namespace, pod.Name, filePath, data)

		ginkgo.By("Verify that data can be successfully read from file.txt")
		output := readFileFromPod(namespace, pod.Name, filePath)
		gomega.Expect(output == data+"\n").To(gomega.BeTrue(), "Pod is not able to read file.txt")

		ginkgo.By("Verify container volume metadata is present in CNS cache")
		ginkgo.By(fmt.Sprintf("Invoking QueryCNSVolume with VolumeID: %s", pv.Spec.CSI.VolumeHandle))
		_, err = e2eVSphere.queryCNSVolumeWithResult(pv.Spec.CSI.VolumeHandle)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		labels := []types.KeyValue{{Key: "fcd-id", Value: fcdID}}
		ginkgo.By("Verify container volume metadata is matching the one in CNS cache")
		err = verifyVolumeMetadataInCNS(&e2eVSphere, pv.Spec.CSI.VolumeHandle,
			pvc.Name, pv.ObjectMeta.Name, pod.Name, labels...)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Deleting the Pod")
		framework.ExpectNoError(fpod.DeletePodWithWait(ctx, client, pod), "Failed to delete pod", pod.Name)

		ginkgo.By(fmt.Sprintf("Verify volume is detached from the node: %s", pod.Spec.NodeName))
		isDiskDetached, err := e2eVSphere.waitForVolumeDetachedFromNode(client, pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(isDiskDetached).To(gomega.BeTrue(), "Volume is not detached from the node")

		ginkgo.By("Deleting the PVC")
		framework.ExpectNoError(fpv.DeletePersistentVolumeClaim(ctx, client, pvc.Name, namespace),
			"Failed to delete PVC", pvc.Name)
		pvc = nil

		ginkgo.By("Verify PV should be deleted automatically")
		framework.ExpectNoError(fpv.WaitForPersistentVolumeDeleted(ctx, client, pv.Name, poll, pollTimeout))
		pv = nil
	})

	// This test verifies the static provisioning workflow by creating the PV
	// by same name twice.
	//
	// Test Steps:
	// 1. Create FCD and wait for fcd to allow syncing with pandora.
	// 2. Create PV1 Spec with volumeID set to FCDID created in Step-1, and
	//    PersistentVolumeReclaimPolicy is set to Retain.
	// 3. Wait for the volume entry to be created in CNS.
	// 4. Delete PV1.
	// 5. Wait for PV1 to be deleted, and also entry is deleted from CNS.
	// 6. Create a PV2 by the same name as PV1.
	// 7. Wait for the volume entry to be created in CNS.
	// 8. Delete PV2.
	// 9. Wait for PV2 to be deleted, and also entry is deleted from CNS.
	ginkgo.It("[csi-block-vanilla] [csi-block-vanilla-parallelized] Verify static provisioning workflow using "+
		"same PV name twice", ginkgo.Label(p2, block, vanilla, core, vc70), func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ginkgo.By("Creating FCD Disk")
		fcdID, err = e2eVSphere.createFCD(ctx, "BasicStaticFCD", diskSizeInMb, defaultDatastore.Reference())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		deleteFCDRequired = true

		ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow newly created FCD:%s to sync with pandora",
			pandoraSyncWaitTime, fcdID))
		time.Sleep(time.Duration(pandoraSyncWaitTime) * time.Second)

		// Creating label for PV.
		// PVC will use this label as Selector to find PV.
		staticPVLabels := make(map[string]string)
		staticPVLabels["fcd-id"] = fcdID

		ginkgo.By("Create PV Spec")
		pvSpec := getPersistentVolumeSpec(fcdID, v1.PersistentVolumeReclaimRetain, staticPVLabels, ext4FSType)

		curtime := time.Now().Unix()
		randomValue := rand.Int()
		val := strconv.FormatInt(int64(randomValue), 10)
		val = string(val[1:3])
		curtimestring := strconv.FormatInt(curtime, 10)
		pvSpec.Name = "static-pv-" + curtimestring + val

		ginkgo.By("Creating the PV-1")
		pv, err = client.CoreV1().PersistentVolumes().Create(ctx, pvSpec, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = e2eVSphere.waitForCNSVolumeToBeCreated(pv.Spec.CSI.VolumeHandle)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Deleting PV-1")
		err = client.CoreV1().PersistentVolumes().Delete(ctx, pvSpec.Name, *metav1.NewDeleteOptions(0))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = e2eVSphere.waitForCNSVolumeToBeDeleted(pv.Spec.CSI.VolumeHandle)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Create the PV-2")
		pv2, err := client.CoreV1().PersistentVolumes().Create(ctx, pvSpec, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = e2eVSphere.waitForCNSVolumeToBeCreated(pv2.Spec.CSI.VolumeHandle)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Deleting PV-2")
		err = client.CoreV1().PersistentVolumes().Delete(ctx, pvSpec.Name, *metav1.NewDeleteOptions(0))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = e2eVSphere.waitForCNSVolumeToBeDeleted(pv2.Spec.CSI.VolumeHandle)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pv = nil
	})

	// This test verifies the static provisioning workflow in guest cluster.
	//
	// Test Steps:
	// 1. Create a PVC using the existing SC in GC.
	// 2. Wait for PVC to be Bound in GC.
	// 3. Verifying if the mapping PVC is bound in SC using the volume handler.
	// 4. Verify volume is created on CNS and check the spbm health.
	// 5. Change the reclaim policy of the PV from delete to retain in GC.
	// 6. Delete PVC in GC.
	// 7. Delete PV in GC.
	// 8. Verifying if PVC and PV still persists in SV cluster.
	// 9. Create a PV with reclaim policy=delete in GC using the bound PVC in
	//    SV cluster as the volume id.
	// 10. Create a PVC in GC using the above PV.
	// 11. Verify the PVC is bound in GC.
	// 12. Delete the PVC in GC.
	// 13. Verifying if PVC and PV also deleted in the SV cluster.
	// 14. Verify volume is deleted on CNS.
	ginkgo.It("[csi-guest] Static provisioning workflow in guest "+
		"cluster", ginkgo.Label(p1, block, tkg, vc70), func() {
		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		storagePolicyNameForSharedDatastores := GetAndExpectStringEnvVar(envStoragePolicyNameForSharedDatastores)
		scParameters := make(map[string]string)
		scParameters[svStorageClassName] = storagePolicyNameForSharedDatastores
		storageclass, pvclaim, err := createPVCAndStorageClass(ctx, client, namespace, nil,
			scParameters, "", nil, "", false, "")

		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		defer func() {
			err := client.StorageV1().StorageClasses().Delete(ctx, storageclass.Name, *metav1.NewDeleteOptions(0))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()
		defer func() {
			err := fpv.DeletePersistentVolumeClaim(ctx, client, pvclaim.Name, namespace)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()

		ginkgo.By("Waiting for claim to be in bound phase")
		ginkgo.By("Expect claim to pass provisioning volume as shared datastore")
		err = fpv.WaitForPersistentVolumeClaimPhase(ctx, v1.ClaimBound, client,
			pvclaim.Namespace, pvclaim.Name, framework.Poll, framework.ClaimProvisionTimeout)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		pv := getPvFromClaim(client, pvclaim.Namespace, pvclaim.Name)
		volumeID := pv.Spec.CSI.VolumeHandle
		// svcPVCName refers to PVC Name in the supervisor cluster.
		svcPVCName := volumeID
		volumeID = getVolumeIDFromSupervisorCluster(svcPVCName)
		gomega.Expect(volumeID).NotTo(gomega.BeEmpty())

		ginkgo.By(fmt.Sprintf("Invoking QueryCNSVolumeWithResult with VolumeID: %s", volumeID))
		queryResult, err := e2eVSphere.queryCNSVolumeWithResult(volumeID)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(queryResult.Volumes).ShouldNot(gomega.BeEmpty())

		pv.Spec.PersistentVolumeReclaimPolicy = v1.PersistentVolumeReclaimRetain
		pv, err = client.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Delete the PVC")
		err = fpv.DeletePersistentVolumeClaim(ctx, client, pvclaim.Name, namespace)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By(fmt.Sprintf("Delete the PV %s", pv.Name))
		err = client.CoreV1().PersistentVolumes().Delete(ctx, pv.Name, *metav1.NewDeleteOptions(0))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Verifying if volume still exists in the Supervisor Cluster")
		// svcPVCName refers to PVC Name in the supervisor cluster.
		volumeID = getVolumeIDFromSupervisorCluster(svcPVCName)
		gomega.Expect(volumeID).NotTo(gomega.BeEmpty())

		// Creating label for PV.
		// PVC will use this label as Selector to find PV.
		staticPVLabels := make(map[string]string)
		staticPVLabels["fcd-id"] = volumeID

		ginkgo.By("Creating the PV")
		pv = getPersistentVolumeSpec(svcPVCName, v1.PersistentVolumeReclaimDelete, staticPVLabels, ext4FSType)
		pv, err = client.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Creating the PVC")
		pvc = getPersistentVolumeClaimSpec(namespace, staticPVLabels, pv.Name)
		pvc, err = client.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Wait for PV and PVC to Bind.
		framework.ExpectNoError(fpv.WaitOnPVandPVC(ctx, client, f.Timeouts, namespace, pv, pvc))

		ginkgo.By("Deleting the PV Claim")
		framework.ExpectNoError(fpv.DeletePersistentVolumeClaim(ctx, client, pvc.Name, namespace),
			"Failed to delete PVC", pvc.Name)
		pvc = nil

		ginkgo.By("Verify PV should be deleted automatically")
		framework.ExpectNoError(fpv.WaitForPersistentVolumeDeleted(ctx, client, pv.Name, poll, pollTimeoutShort))
		pv = nil

		ginkgo.By("Verify volume is deleted in Supervisor Cluster")
		volumeExists := verifyVolumeExistInSupervisorCluster(svcPVCName)
		gomega.Expect(volumeExists).To(gomega.BeFalse())
	})

	// This test verifies the static provisioning workflow II in guest cluster.
	//
	// Test Steps:
	// 1. Create a PVC using the existing SC in SV Cluster.
	// 2. Wait for PVC to be Bound in SV cluster.
	// 3. Create a PV with reclaim policy=delete in GC using the bound PVC in
	//    SV cluster as the volume id.
	// 4. Create a PVC in GC using the above PV.
	// 5. Verify the PVC is bound in GC.
	// 6. Delete the PVC in GC.
	// 7. Verifying if PVC and PV also deleted in the SV cluster.
	// 8. Verify volume is deleted on CNS.
	ginkgo.It("[csi-guest] Static provisioning workflow II in guest "+
		"cluster", ginkgo.Label(p1, block, tkg, vc70), func() {
		var err error

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		storagePolicyNameForSharedDatastores := GetAndExpectStringEnvVar(envStoragePolicyNameForSharedDatastores)

		// Create supvervisor cluster client.
		var svcClient clientset.Interface
		if k8senv := GetAndExpectStringEnvVar("SUPERVISOR_CLUSTER_KUBE_CONFIG"); k8senv != "" {
			svcClient, err = createKubernetesClientFromConfig(k8senv)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		svNamespace := GetAndExpectStringEnvVar(envSupervisorClusterNamespace)

		ginkgo.By("Creating PVC in supervisor cluster")
		// Get storageclass from the supervisor cluster.
		storageclass, err := svcClient.StorageV1().StorageClasses().Get(ctx,
			storagePolicyNameForSharedDatastores, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pvclaim, err := createPVC(ctx, svcClient, svNamespace, nil, "", storageclass, "")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		defer func() {
			err := fpv.DeletePersistentVolumeClaim(ctx, svcClient, pvclaim.Name, svNamespace)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()

		ginkgo.By("Waiting for claim to be in bound phase")
		ginkgo.By("Expect claim to pass provisioning volume as shared datastore")
		err = fpv.WaitForPersistentVolumeClaimPhase(ctx, v1.ClaimBound, svcClient,
			pvclaim.Namespace, pvclaim.Name, framework.Poll, framework.ClaimProvisionTimeout)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		pv := getPvFromClaim(svcClient, pvclaim.Namespace, pvclaim.Name)
		volumeID := pv.Spec.CSI.VolumeHandle

		// Creating label for PV.
		// PVC will use this label as Selector to find PV.
		staticPVLabels := make(map[string]string)
		staticPVLabels["fcd-id"] = volumeID

		svcPVCName := volumeID
		ginkgo.By("Creating the PV in guest cluster")
		pv = getPersistentVolumeSpec(svcPVCName, v1.PersistentVolumeReclaimDelete, staticPVLabels, ext4FSType)
		pv, err = client.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Creating the PVC in guest cluster")
		pvc = getPersistentVolumeClaimSpec(namespace, staticPVLabels, pv.Name)
		pvc, err = client.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Wait for PV and PVC to Bind.
		framework.ExpectNoError(fpv.WaitOnPVandPVC(ctx, client, f.Timeouts, namespace, pv, pvc))

		ginkgo.By("Deleting the PV Claim")
		framework.ExpectNoError(fpv.DeletePersistentVolumeClaim(ctx, client, pvc.Name, namespace),
			"Failed to delete PVC", pvc.Name)
		pvc = nil

		ginkgo.By("Verify PV should be deleted automatically")
		framework.ExpectNoError(fpv.WaitForPersistentVolumeDeleted(ctx, client, pv.Name, poll, pollTimeoutShort))
		pv = nil

		ginkgo.By("Verify volume is deleted in Supervisor Cluster")
		volumeExists := verifyVolumeExistInSupervisorCluster(svcPVCName)
		gomega.Expect(volumeExists).To(gomega.BeFalse())

	})

	// This test verifies the static provisioning workflow on supervisor cluster.
	//
	// Test Steps:
	// 1. Create CNS volume note the volumeID.
	// 2. Create Resource quota.
	// 3. create CNS register volume with above created VolumeID.
	// 4. verify created PV, PVC and check the bidirectional reference.
	// 5. Create Pod , with above created PVC.
	// 6. Verify volume is attached to the node and volume is accessible in the pod.
	// 7. Delete POD.
	// 8. Delete PVC.
	// 9. Verify PV is deleted automatically.
	// 10. Verify Volume id deleted automatically.
	// 11. Verify CRD deleted automatically.
	ginkgo.It("[csi-supervisor] Verify static provisioning workflow on SVC - import "+
		"CNS volume", ginkgo.Label(p0, block, wcp, vc70), func() {
		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		curtime := time.Now().Unix()
		curtimestring := strconv.FormatInt(curtime, 10)
		pvcName := "cns-pvc-" + curtimestring
		framework.Logf("pvc name :%s", pvcName)

		restConfig, storageclass, profileID := staticProvisioningPreSetUpUtil(ctx, f, client, storagePolicyName)
		framework.Logf("Storage class : %s", storageclass.Name)

		ginkgo.By("Creating FCD (CNS Volume)")
		fcdID, err := e2eVSphere.createFCDwithValidProfileID(ctx,
			"staticfcd"+curtimestring, profileID, diskSizeInMb, defaultDatastore.Reference())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		deleteFCDRequired = false
		ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow newly created FCD:%s to sync with pandora",
			pandoraSyncWaitTime, fcdID))
		time.Sleep(time.Duration(pandoraSyncWaitTime) * time.Second)

		ginkgo.By("Create CNS register volume with above created FCD ")
		cnsRegisterVolume := getCNSRegisterVolumeSpec(ctx, namespace, fcdID, "", pvcName, v1.ReadWriteOnce)
		err = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		framework.ExpectNoError(waitForCNSRegisterVolumeToGetCreated(ctx, restConfig,
			namespace, cnsRegisterVolume, poll, supervisorClusterOperationsTimeout))
		cnsRegisterVolumeName := cnsRegisterVolume.GetName()
		framework.Logf("CNS register volume name : %s", cnsRegisterVolumeName)

		ginkgo.By(" verify created PV, PVC and check the bidirectional reference")
		pvc, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pv := getPvFromClaim(client, namespace, pvcName)
		verifyBidirectionalReferenceOfPVandPVC(ctx, client, pvc, pv, fcdID)

		ginkgo.By("Creating pod")
		pod, err := createPod(ctx, client, namespace, nil, []*v1.PersistentVolumeClaim{pvc}, false, "")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		podName := pod.GetName()
		framework.Logf("podName : %s", podName)

		ginkgo.By(fmt.Sprintf("Verify volume: %s is attached to the node: %s",
			pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName))
		var vmUUID string
		var exists bool
		annotations := pod.Annotations
		vmUUID, exists = annotations[vmUUIDLabel]
		gomega.Expect(exists).To(gomega.BeTrue(), fmt.Sprintf("Pod doesn't have %s annotation", vmUUIDLabel))
		_, err = e2eVSphere.getVMByUUID(ctx, vmUUID)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Deleting the pod")
		err = fpod.DeletePodWithWait(ctx, client, pod)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By(fmt.Sprintf("Verify volume: %s is detached from PodVM with vmUUID: %s",
			pv.Spec.CSI.VolumeHandle, vmUUID))
		_, err = e2eVSphere.getVMByUUIDWithWait(ctx, vmUUID, supervisorClusterOperationsTimeout)
		gomega.Expect(err).To(gomega.HaveOccurred(),
			fmt.Sprintf("PodVM with vmUUID: %s still exists. So volume: %s is not detached from the PodVM",
				vmUUID, pv.Spec.CSI.VolumeHandle))

		defer func() {
			testCleanUpUtil(ctx, restConfig, cnsRegisterVolume, namespace, pvc.Name, pv.Name)
		}()

	})

	// This test verifies the static provisioning workflow on supervisor cluster.
	//
	// Test Steps:
	// 1. Create FCD with valid storage policy.
	// 2. Create Resource quota.
	// 3. Create CNS register volume with above created FCD.
	// 4. verify PV, PVC got created , check the bidirectional reference.
	// 5. Create Pod , with above created PVC.
	// 6. Verify volume is attached to the node and volume is accessible in the pod.
	// 7. Delete POD.
	// 8. Delete PVC.
	// 9. Verify PV is deleted automatically.
	// 10. Verify Volume id deleted automatically.
	// 11. Verify CRD deleted automatically.
	ginkgo.It("[csi-supervisor] [stretched-svc] Verify static provisioning workflow on SVC import "+
		"FCD", ginkgo.Label(p0, block, wcp, vc70), func() {
		var err error
		var totalQuotaUsedBefore, storagePolicyQuotaBefore, storagePolicyUsageBefore *resource.Quantity
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		curtime := time.Now().Unix()
		curtimeinstring := strconv.FormatInt(curtime, 10)
		pvcName := "cns-pvc-" + curtimeinstring
		framework.Logf("pvc name :%s", pvcName)
		namespace = getNamespaceToRunTests(f)

		restConfig, storageclass, profileID := staticProvisioningPreSetUpUtil(ctx, f, client, storagePolicyName)

		if isQuotaValidationSupported {
			totalQuotaUsedBefore, _, storagePolicyQuotaBefore, _, storagePolicyUsageBefore, _ =
				getStoragePolicyUsedAndReservedQuotaDetails(ctx, restConfig,
					storageclass.Name, namespace, pvcUsage, volExtensionName, false)

		}

		ginkgo.By("Creating FCD Disk")
		fcdID, err := e2eVSphere.createFCDwithValidProfileID(ctx,
			"staticfcd"+curtimeinstring, profileID, diskSizeInMb, defaultDatastore.Reference())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		framework.Logf("FCD ID: %s", fcdID)
		deleteFCDRequired = false

		ginkgo.By("Create CNS register volume with above created FCD")
		cnsRegisterVolume := getCNSRegisterVolumeSpec(ctx, namespace, fcdID, "", pvcName, v1.ReadWriteOnce)
		err = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		framework.Logf("waiting for some time for FCD to register in CNS and for cnsRegisterVolume to get create")
		framework.ExpectNoError(waitForCNSRegisterVolumeToGetCreated(ctx,
			restConfig, namespace, cnsRegisterVolume, poll, pollTimeout))
		cnsRegisterVolumeName := cnsRegisterVolume.GetName()
		framework.Logf("CNS register volume name : %s", cnsRegisterVolumeName)

		ginkgo.By("verify created PV, PVC and check the bidirectional reference")
		pvc, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pv := getPvFromClaim(client, namespace, pvcName)
		verifyBidirectionalReferenceOfPVandPVC(ctx, client, pvc, pv, fcdID)

		ginkgo.By("Creating pod")
		pod, err := createPod(ctx, client, namespace, nil, []*v1.PersistentVolumeClaim{pvc}, false, "")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		podName := pod.GetName()
		framework.Logf("podName: %s", podName)

		ginkgo.By(fmt.Sprintf("Verify volume: %s is attached to the node: %s",
			pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName))
		var vmUUID string
		var exists bool
		annotations := pod.Annotations
		vmUUID, exists = annotations[vmUUIDLabel]
		gomega.Expect(exists).To(gomega.BeTrue(), fmt.Sprintf("Pod doesn't have %s annotation", vmUUIDLabel))
		_, err = e2eVSphere.getVMByUUID(ctx, vmUUID)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		diskSizeInMbstr := convertInt64ToStrMbFormat(diskSizeInMb)
		if isQuotaValidationSupported {
			sp_quota_pvc_status, sp_usage_pvc_status := validateQuotaUsageAfterResourceCreation(ctx, restConfig,
				storageclass.Name, namespace, pvcUsage, volExtensionName,
				[]string{diskSizeInMbstr}, totalQuotaUsedBefore, storagePolicyQuotaBefore,
				storagePolicyUsageBefore, false)
			gomega.Expect(sp_quota_pvc_status && sp_usage_pvc_status).NotTo(gomega.BeFalse())

		}

		ginkgo.By("Deleting the pod")
		err = fpod.DeletePodWithWait(ctx, client, pod)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By(fmt.Sprintf("Verify volume: %s is detached from PodVM with vmUUID: %s",
			pv.Spec.CSI.VolumeHandle, vmUUID))
		_, err = e2eVSphere.getVMByUUIDWithWait(ctx, vmUUID, supervisorClusterOperationsTimeout)
		gomega.Expect(err).To(gomega.HaveOccurred(),
			fmt.Sprintf("PodVM with vmUUID: %s still exists. So volume: %s is not detached from the PodVM",
				vmUUID, pv.Spec.CSI.VolumeHandle))

		defer func() {
			testCleanUpUtil(ctx, restConfig, cnsRegisterVolume, namespace, pvc.Name, pv.Name)

			//Validates PVC quota in both StoragePolicyQuota and StoragePolicyUsage CR
			_, _, storagePolicyQuota_afterCleanUp, _, storagePolicyUsage_AfterCleanup, _ :=
				getStoragePolicyUsedAndReservedQuotaDetails(ctx, restConfig,
					storageclass.Name, namespace, pvcUsage, volExtensionName, false)

			expectEqual(storagePolicyQuota_afterCleanUp, storagePolicyQuotaBefore,
				"Before and After values of storagePolicy-Quota CR should match")
			expectEqual(storagePolicyUsage_AfterCleanup, storagePolicyUsageBefore,
				"Before and After values of storagePolicy-Usage CR should match")

		}()

	})

	// This test verifies the static provisioning workflow on supervisor cluster
	// when there is no resource quota available.
	//
	// Test Steps:
	// 1. Create FCD with valid storage policy.
	// 2. Delete the existing resource quota.
	// 3. create CNS register volume with above created FCD.
	// 4. Since there is no resource quota available , CRD will be in pending state.
	// 5. Verify  PVC creation fails.
	// 6. Increase Resource quota.
	// 7. verify PVC, PV got created , check the bidirectional reference.
	// 8. Create Pod , with above created PVC.
	// 9. Verify volume is attached to the node and volume is accessible in the pod.
	// 10. Delete POD.
	// 11. Delete PVC.
	// 12. Verify PV is deleted automatically.
	// 13. Verify Volume id deleted automatically.
	// 14. Verify CRD deleted automatically.
	ginkgo.It("[csi-supervisor] Verify static provisioning workflow on svc - when there is no "+
		"resourcequota available", ginkgo.Label(p1, block, wcp, vc70, vc80), func() {
		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var totalQuotaUsedBefore, storagePolicyQuotaBefore, storagePolicyUsageBefore *resource.Quantity

		curtime := time.Now().Unix()
		curtimeinstring := strconv.FormatInt(curtime, 10)
		pvcName := "cns-pvc-" + curtimeinstring

		restConfig, _, profileID := staticProvisioningPreSetUpUtil(ctx, f, client, storagePolicyName)

		if isQuotaValidationSupported {
			totalQuotaUsedBefore, _, storagePolicyQuotaBefore, _, storagePolicyUsageBefore, _ =
				getStoragePolicyUsedAndReservedQuotaDetails(ctx, restConfig,
					storagePolicyName, namespace, pvcUsage, volExtensionName, false)
		}

		ginkgo.By("Create FCD with valid storage policy.")
		fcdID, err := e2eVSphere.createFCDwithValidProfileID(ctx,
			"staticfcd"+curtimeinstring, profileID, diskSizeInMb, defaultDatastore.Reference())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		deleteFCDRequired = false

		ginkgo.By("Remove existing storage  quota")
		removeStoragePolicyQuota(ctx, restConfig, storagePolicyName, namespace)

		ginkgo.By("Import above created FCD")
		cnsRegisterVolume := getCNSRegisterVolumeSpec(ctx, namespace, fcdID, "", pvcName, v1.ReadWriteOnce)
		_ = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)

		ginkgo.By("Since there is no resource quota available, Verify  PVC creation fails")
		_, err = client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
		gomega.Expect(err).To(gomega.HaveOccurred())

		ginkgo.By("Create resource quota")
		setStoragePolicyQuota(ctx, restConfig, storagePolicyName, namespace, rqLimit)
		framework.Logf("Wait till the PVC creation succeeds after increasing resource quota")
		framework.ExpectNoError(waitForCNSRegisterVolumeToGetCreated(ctx,
			restConfig, namespace, cnsRegisterVolume, poll, pollTimeout))

		cnsRegisterVolumeName := cnsRegisterVolume.GetName()
		framework.Logf("CNS register volume name : %s", cnsRegisterVolumeName)

		ginkgo.By(" verify created PV, PVC and check the bidirectional reference")
		pvc, err = client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pv := getPvFromClaim(client, namespace, pvcName)
		verifyBidirectionalReferenceOfPVandPVC(ctx, client, pvc, pv, fcdID)

		ginkgo.By("Creating pod")
		pod, err := createPod(ctx, client, namespace, nil, []*v1.PersistentVolumeClaim{pvc}, false, "")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		podName := pod.GetName()
		framework.Logf("podName: %s", podName)

		ginkgo.By(fmt.Sprintf("Verify volume: %s is attached to the node: %s",
			pv.Spec.CSI.VolumeHandle, pod.Spec.NodeName))
		var vmUUID string
		var exists bool
		annotations := pod.Annotations
		vmUUID, exists = annotations[vmUUIDLabel]
		gomega.Expect(exists).To(gomega.BeTrue(), fmt.Sprintf("Pod doesn't have %s annotation", vmUUIDLabel))
		_, err = e2eVSphere.getVMByUUID(ctx, vmUUID)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		diskSizeInMbstr := convertInt64ToStrMbFormat(diskSizeInMb)
		if isQuotaValidationSupported {
			sp_quota_pvc_status, sp_usage_pvc_status := validateQuotaUsageAfterResourceCreation(ctx, restConfig,
				storagePolicyName, namespace, pvcUsage, volExtensionName,
				[]string{diskSizeInMbstr}, totalQuotaUsedBefore, storagePolicyQuotaBefore,
				storagePolicyUsageBefore, false)
			gomega.Expect(sp_quota_pvc_status && sp_usage_pvc_status).NotTo(gomega.BeFalse())

		}

		ginkgo.By("Deleting the pod")
		err = fpod.DeletePodWithWait(ctx, client, pod)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.By("Wait for 3 minutes for the pod to get terminated successfully")
		time.Sleep(supervisorClusterOperationsTimeout)
		ginkgo.By(fmt.Sprintf("Verify volume: %s is detached from PodVM with vmUUID: %s",
			pv.Spec.CSI.VolumeHandle, vmUUID))
		_, err = e2eVSphere.getVMByUUIDWithWait(ctx, vmUUID, supervisorClusterOperationsTimeout)
		gomega.Expect(err).To(gomega.HaveOccurred(),
			fmt.Sprintf("PodVM with vmUUID: %s still exists. So volume: %s is not detached from the PodVM",
				vmUUID, pv.Spec.CSI.VolumeHandle))
		defer func() {
			testCleanUpUtil(ctx, restConfig, cnsRegisterVolume, namespace, pvc.Name, pv.Name)
			if isQuotaValidationSupported {
				//Validates PVC quota in both StoragePolicyQuota and StoragePolicyUsage CR
				_, _, storagePolicyQuota_afterCleanUp, _, storagePolicyUsage_AfterCleanup, _ :=
					getStoragePolicyUsedAndReservedQuotaDetails(ctx, restConfig,
						storagePolicyName, namespace, pvcUsage, volExtensionName, false)

				expectEqual(storagePolicyQuota_afterCleanUp, storagePolicyQuotaBefore,
					"Before and After values of storagePolicy-Quota CR should match")
				expectEqual(storagePolicyUsage_AfterCleanup, storagePolicyUsageBefore,
					"Before and After values of storagePolicy-Usage CR should match")
			}
		}()
	})

	// This test verifies the static provisioning workflow on supervisor cluster
	// - AccessMode is ReadWriteMany / ReadOnlyMany.
	//
	// Test Steps:
	// 1. Create FCD with valid storage policy.
	// 2. Create Resource quota.
	// 3. Create CNS register volume with above created FCD, AccessMode as "ReadWriteMany".
	// 4. verify  the error message.
	// 5. Create CNS register volume with above created FCD, AccessMode as "ReadOnlyMany".
	// 6. verify  the error message.
	// 7. Delete Resource quota.
	ginkgo.It("[csi-supervisor] Verify static provisioning when AccessMode is ReadWriteMany or "+
		"ReadOnlyMany", ginkgo.Label(p1, block, wcp, vc70), func() {
		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		curtime := time.Now().Unix()
		curtimeinstring := strconv.FormatInt(curtime, 10)
		pvcName := "cns-pvc-" + curtimeinstring
		framework.Logf("pvc name :%s", pvcName)

		restConfig, _, _ := staticProvisioningPreSetUpUtil(ctx, f, client, storagePolicyName)

		ginkgo.By("Create CNS register volume with above created FCD , AccessMode is set to ReadWriteMany ")
		cnsRegisterVolume := getCNSRegisterVolumeSpec(ctx, namespace, fcdID, "", pvcName, v1.ReadWriteMany)
		err = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = waitForCNSRegisterVolumeToGetCreated(ctx,
			restConfig, namespace, cnsRegisterVolume, poll, supervisorClusterOperationsTimeout)
		gomega.Expect(err).To(gomega.HaveOccurred())

		ginkgo.By("Verify error message when AccessMode is set to ReadWriteMany")
		cnsRegisterVolume = getCNSRegistervolume(ctx, restConfig, cnsRegisterVolume)
		actualErrorMsg := cnsRegisterVolume.Status.Error
		expectedErrorMsg := "AccessMode: ReadWriteMany is not supported"
		gomega.Expect(strings.Contains(actualErrorMsg, expectedErrorMsg), gomega.BeTrue())

		ginkgo.By("Create CNS register volume with above created FCD ,AccessMode is set to ReadOnlyMany")
		cnsRegisterVolume = getCNSRegisterVolumeSpec(ctx, namespace, fcdID, "", pvcName, v1.ReadOnlyMany)
		err = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = waitForCNSRegisterVolumeToGetCreated(ctx,
			restConfig, namespace, cnsRegisterVolume, poll, supervisorClusterOperationsTimeout)
		gomega.Expect(err).To(gomega.HaveOccurred())

		ginkgo.By("Verify error message when AccessMode is set to ReadOnlyMany")
		cnsRegisterVolume = getCNSRegistervolume(ctx, restConfig, cnsRegisterVolume)
		actualErrorMsg = cnsRegisterVolume.Status.Error
		expectedErrorMsg = "AccessMode: ReadOnlyMany is not supported"
		gomega.Expect(strings.Contains(actualErrorMsg, expectedErrorMsg), gomega.BeTrue())

	})

	// This test verifies the static provisioning workflow on supervisor
	// cluster, When duplicate FCD is used.
	//
	// Test Steps:
	// 1. Create FCD with valid storage policy.
	// 2. Create Resource quota.
	// 3. Create CNS register volume with above created FCD.
	// 4. verify PV, PVC got created.
	// 5. Use the above created FCD and create CNS register volume.
	// 6. Verify the error message.
	// 7. Edit CRD with new FCDID.
	// 8. Verify newly created  PV , PVC and verify the bidirectional reference.
	// 7. Delete PVC.
	// 8. Verify PV is deleted automatically.
	// 9. Verify Volume id deleted automatically.
	// 10. Verify CRD deleted automatically.
	ginkgo.It("[csi-supervisor] Verify static provisioning workflow - when "+
		"DuplicateFCD is used", ginkgo.Label(p2, block, wcp, vc70), func() {

		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		curtime := time.Now().Unix()
		curtimeinstring := strconv.FormatInt(curtime, 10)
		pvcName := "cns-pvc-" + curtimeinstring
		framework.Logf("pvc name :%s", pvcName)

		restConfig, _, profileID := staticProvisioningPreSetUpUtil(ctx, f, client, storagePolicyName)

		ginkgo.By("Creating Resource quota")
		setStoragePolicyQuota(ctx, restConfig, storagePolicyName, namespace, rqLimit)

		ginkgo.By("Create FCD")
		fcdID1, err := e2eVSphere.createFCDwithValidProfileID(ctx,
			"staticfcd1"+curtimeinstring, profileID, diskSizeInMinMb, defaultDatastore.Reference())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		fcdID2, err := e2eVSphere.createFCDwithValidProfileID(ctx,
			"staticfcd2"+curtimeinstring, profileID, diskSizeInMinMb, defaultDatastore.Reference())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		deleteFCDRequired = false
		ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow newly created FCD:%s to sync with pandora",
			pandoraSyncWaitTime, fcdID))
		time.Sleep(time.Duration(pandoraSyncWaitTime) * time.Second)

		ginkgo.By("Create CNS register volume with above created FCD ")
		cnsRegisterVolume := getCNSRegisterVolumeSpec(ctx, namespace, fcdID1, "", pvcName, v1.ReadWriteOnce)
		err = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		framework.ExpectNoError(waitForCNSRegisterVolumeToGetCreated(ctx,
			restConfig, namespace, cnsRegisterVolume, poll, supervisorClusterOperationsTimeout))
		cnsRegisterVolumeName := cnsRegisterVolume.GetName()
		framework.Logf("CNS register volume name : %s", cnsRegisterVolumeName)

		ginkgo.By("verify created PV, PVC")
		pvc1, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pv1 := getPvFromClaim(client, namespace, pvcName)

		ginkgo.By("Create CnsregisteVolume with already used FCD")
		pvcName2 := pvcName + "duplicatefcd"
		cnsRegisterVolume = getCNSRegisterVolumeSpec(ctx, namespace, fcdID1, "", pvcName2, v1.ReadWriteOnce)
		err = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = waitForCNSRegisterVolumeToGetCreated(ctx,
			restConfig, namespace, cnsRegisterVolume, poll, supervisorClusterOperationsTimeout)
		gomega.Expect(err).To(gomega.HaveOccurred())

		ginkgo.By("Verify the error message, when already used FCD is used")
		cnsRegisterVolume = getCNSRegistervolume(ctx, restConfig, cnsRegisterVolume)
		actualErrorMsg := cnsRegisterVolume.Status.Error
		expectedErrorMsg := "Duplicate Request"
		gomega.Expect(strings.Contains(actualErrorMsg, expectedErrorMsg), gomega.BeTrue())

		ginkgo.By("Update CRD with new FCD ID")
		cnsRegisterVolume.Spec.VolumeID = fcdID2
		cnsRegisterVolume = updateCNSRegistervolume(ctx, restConfig, cnsRegisterVolume)
		cnsRegisterVolume = getCNSRegistervolume(ctx, restConfig, cnsRegisterVolume)
		framework.Logf("PVC name after updating the FCDID  :%s", cnsRegisterVolume.Spec.PvcName)

		ginkgo.By("Wait for some time for the updated CRD to create PV , PVC")
		framework.ExpectNoError(waitForCNSRegisterVolumeToGetCreated(ctx,
			restConfig, namespace, cnsRegisterVolume, poll, pollTimeout))

		ginkgo.By("verify newly created PV, PVC")
		pvc2, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName2, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pv2 := getPvFromClaim(client, namespace, pvcName2)
		verifyBidirectionalReferenceOfPVandPVC(ctx, client, pvc2, pv2, fcdID2)

		defer func() {
			ginkgo.By("Deleting the PV Claim")
			framework.ExpectNoError(fpv.DeletePersistentVolumeClaim(ctx, client, pvc2.Name, namespace),
				"Failed to delete PVC", pvc2.Name)

			ginkgo.By("Verify PV should be deleted automatically")
			framework.ExpectNoError(fpv.WaitForPersistentVolumeDeleted(ctx, client,
				pv2.Name, poll, supervisorClusterOperationsTimeout))

			testCleanUpUtil(ctx, restConfig, nil, namespace, pvc1.Name, pv1.Name)
		}()

	})

	// This test verifies the static provisioning workflow on supervisor
	// cluster - when duplicate PVC name is used.
	//
	// Test Steps:
	// 1. Create FCD with valid storage policy.
	// 2. Create Resource quota.
	// 3. Create CNS register volume with above created FCD.
	// 4. verify PV, PVC got created.
	// 5. Create CNS register volume with new FCD, but already created PVC name.
	// 6. Verify the error message, due to duplicate PVC name.
	// 7. Edit CRD with new PVC name.
	// 8. Verify newly cretaed PV, PVC and bidirectional references.
	// 9. Delete PVC.
	// 10. Verify PV is deleted automatically.
	// 11. Verify Volume id deleted automatically.
	// 12. Verify CRD deleted automatically.
	ginkgo.It("[csi-supervisor] Verify static provisioning workflow - when "+
		"DuplicatePVC name is used", ginkgo.Label(p2, block, wcp, vc70), func() {

		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		curtime := time.Now().Unix()
		curtimeinstring := strconv.FormatInt(curtime, 10)
		pvcName := "cns-pvc-" + curtimeinstring
		framework.Logf("pvc name :%s", pvcName)

		restConfig, _, profileID := staticProvisioningPreSetUpUtil(ctx, f, client, storagePolicyName)

		ginkgo.By("Create FCD")
		fcdID1, err := e2eVSphere.createFCDwithValidProfileID(ctx,
			"staticfcd1"+curtimeinstring, profileID, diskSizeInMinMb, defaultDatastore.Reference())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		framework.Logf("FCDID1: %s", fcdID1)
		fcdID2, err := e2eVSphere.createFCDwithValidProfileID(ctx,
			"staticfcd2"+curtimeinstring, profileID, diskSizeInMinMb, defaultDatastore.Reference())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		framework.Logf("FCDID2: %s", fcdID2)
		ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow newly created FCD:%s to sync with pandora",
			pandoraSyncWaitTime, fcdID))
		time.Sleep(time.Duration(pandoraSyncWaitTime) * time.Second)
		deleteFCDRequired = false

		ginkgo.By("Create CNS register volume with above created FCD ")
		cnsRegisterVolume := getCNSRegisterVolumeSpec(ctx, namespace, fcdID1, "", pvcName, v1.ReadWriteOnce)
		err = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		framework.ExpectNoError(waitForCNSRegisterVolumeToGetCreated(ctx,
			restConfig, namespace, cnsRegisterVolume, poll, supervisorClusterOperationsTimeout))

		cnsRegisterVolumeName := cnsRegisterVolume.GetName()
		framework.Logf("CNS register volume name : %s", cnsRegisterVolumeName)

		ginkgo.By("verify created PV, PVC")
		pvc1, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pv1 := getPvFromClaim(client, namespace, pvcName)

		ginkgo.By("Create CnsregisteVolume with already created PVC")
		cnsRegisterVolume = getCNSRegisterVolumeSpec(ctx, namespace, fcdID2, "", pvcName, v1.ReadWriteOnce)
		err = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		time.Sleep(time.Duration(40) * time.Second)

		ginkgo.By("Verify Error message when duplicate PVC is used")
		cnsRegisterVolume = getCNSRegistervolume(ctx, restConfig, cnsRegisterVolume)
		actualErrorMsg := cnsRegisterVolume.Status.Error
		expectedErrorMsg := "Another PVC: " + pvcName + " already exists in namespace: " +
			namespace + " which is Bound to a different PV"
		gomega.Expect(strings.Contains(actualErrorMsg, expectedErrorMsg), gomega.BeTrue())

		updatedpvcName := "unique-pvc" + curtimeinstring
		cnsRegisterVolume.Spec.PvcName = updatedpvcName
		cnsRegisterVolume = updateCNSRegistervolume(ctx, restConfig, cnsRegisterVolume)
		cnsRegisterVolume = getCNSRegistervolume(ctx, restConfig, cnsRegisterVolume)
		framework.Logf("Updated pvc name  :%s", cnsRegisterVolume.Spec.PvcName)

		ginkgo.By("Wait for some time for the updated CRD to create PV , PVC")
		framework.ExpectNoError(waitForCNSRegisterVolumeToGetCreated(ctx,
			restConfig, namespace, cnsRegisterVolume, poll, pollTimeout))

		ginkgo.By("verify created PV, PVC")
		pvc2, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, updatedpvcName, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pv2 := getPvFromClaim(client, namespace, updatedpvcName)
		verifyBidirectionalReferenceOfPVandPVC(ctx, client, pvc2, pv2, fcdID2)

		defer func() {
			ginkgo.By("Deleting the PV Claim")
			framework.ExpectNoError(fpv.DeletePersistentVolumeClaim(ctx, client, pvc2.Name, namespace),
				"Failed to delete PVC ", pvc2.Name)

			ginkgo.By("Verify PV should be deleted automatically")
			framework.ExpectNoError(fpv.WaitForPersistentVolumeDeleted(ctx, client, pv2.Name,
				poll, supervisorClusterOperationsTimeout))

			testCleanUpUtil(ctx, restConfig, nil, namespace, pvc1.Name, pv1.Name)
		}()

	})

	// This test verifies the static provisioning workflow on supervisor
	// cluster - When vsanhealthService is down.
	//
	// Test Steps:
	// 1. Create FCD with valid non shared storage policy.
	// 2. Create Resource quota.
	// 3. Stop VsanHealthService.
	// 4. Create CNS register volume with above created FCD.
	// 5. Verify the error message , since VsanHealthService is down.
	// 6. Start VsanHealthService.
	// 7. CRD should be successful, PV, PVC should get created.
	// 8. Delete PVC.
	// 9. PV and CRD gets auto deleted.
	// 10. Delete Resource quota.
	ginkgo.It("[csi-supervisor] Verifies static provisioning workflow on supervisor cluster - "+
		"When vsanhealthService is down", ginkgo.Label(p2, block, wcp, negative, vc70), func() {

		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		curtime := time.Now().Unix()
		curtimeinstring := strconv.FormatInt(curtime, 10)
		pvcName := "cns-pvc-" + curtimeinstring
		framework.Logf("pvc name :%s", pvcName)

		restConfig, _, profileID := staticProvisioningPreSetUpUtil(ctx, f, client, storagePolicyName)

		ginkgo.By("Creating Resource quota")
		setStoragePolicyQuota(ctx, restConfig, storagePolicyName, namespace, rqLimit)

		ginkgo.By("Create FCD")
		fcdID, err := e2eVSphere.createFCDwithValidProfileID(ctx,
			"staticfcd"+curtimeinstring, profileID, diskSizeInMinMb, defaultDatastore.Reference())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By(fmt.Sprintln("Stopping vsan-health on the vCenter host"))
		isVsanHealthServiceStopped = true
		err = invokeVCenterServiceControl(ctx, stopOperation, vsanhealthServiceName, vcAddress)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow vsan-health to completely shutdown",
			vsanHealthServiceWaitTime))
		time.Sleep(time.Duration(vsanHealthServiceWaitTime) * time.Second)

		ginkgo.By("Create CNS register volume with above created FCD")
		cnsRegisterVolume := getCNSRegisterVolumeSpec(ctx, namespace, fcdID, "", pvcName, v1.ReadWriteOnce)
		err = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		time.Sleep(time.Duration(20) * time.Second)
		cnsRegisterVolumeName := cnsRegisterVolume.GetName()
		framework.Logf("CNS register volume name : %s", cnsRegisterVolumeName)

		ginkgo.By("Verify the error message, when vsanhealth is down")
		cnsRegisterVolume = getCNSRegistervolume(ctx, restConfig, cnsRegisterVolume)
		actualErrorMsg := cnsRegisterVolume.Status.Error
		expectedErrorMsg := "failed to create CNS volume"
		gomega.Expect(strings.Contains(actualErrorMsg, expectedErrorMsg), gomega.BeTrue())

		ginkgo.By(fmt.Sprintln("Starting vsan-health on the vCenter host"))
		err = invokeVCenterServiceControl(ctx, startOperation, vsanhealthServiceName, vcAddress)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow vsan-health to come up again", vsanHealthServiceWaitTime))
		time.Sleep(time.Duration(vsanHealthServiceWaitTime) * time.Second)
		isVsanHealthServiceStopped = false

		ginkgo.By("Wait for some time for the CRD to create PV , PVC")
		framework.ExpectNoError(waitForCNSRegisterVolumeToGetCreated(ctx,
			restConfig, namespace, cnsRegisterVolume, poll, pollTimeout))

		ginkgo.By("verify created PV, PVC")
		pvc, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pv := getPvFromClaim(client, namespace, pvcName)
		verifyBidirectionalReferenceOfPVandPVC(ctx, client, pvc, pv, fcdID)

		defer func() {
			testCleanUpUtil(ctx, restConfig, nil, namespace, pvc.Name, pv.Name)
		}()

	})

	// This test verifies the static provisioning workflow on supervisor
	// cluster - When SPSService is down.
	//
	// Test Steps:
	// 1. Create FCD with valid non shared storage policy.
	// 2. Create Resource quota.
	// 3. Stop SPSService.
	// 4. Create CNS register volume with above created FCD.
	// 5. Verify the error message , since SPSService is down.
	// 6. Start SPSService.
	// 7. CRD should be successful, PV, PVC should get created.
	// 8. Delete PVC.
	// 9. PV and CRD gets auto deleted.
	// 10. Delete Resource quota.
	ginkgo.It("[csi-supervisor] Verifies static provisioning workflow on SVC - When "+
		"SPS service is down", ginkgo.Label(p2, block, wcp, negative, vc70), func() {

		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		curtime := time.Now().Unix()
		curtimeinstring := strconv.FormatInt(curtime, 10)
		pvcName := "cns-pvc-" + curtimeinstring
		framework.Logf("pvc name :%s", pvcName)

		restConfig, _, profileID := staticProvisioningPreSetUpUtil(ctx, f, client, storagePolicyName)

		ginkgo.By("Creating Resource quota")
		setStoragePolicyQuota(ctx, restConfig, storagePolicyName, namespace, rqLimit)

		ginkgo.By("Create FCD")
		fcdID, err := e2eVSphere.createFCDwithValidProfileID(ctx,
			"staticfcd"+curtimeinstring, profileID, diskSizeInMinMb, defaultDatastore.Reference())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By(fmt.Sprintln("Stopping sps on the vCenter host"))
		isSPSserviceStopped = true
		err = invokeVCenterServiceControl(ctx, stopOperation, spsServiceName, vcAddress)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow sps to completely shutdown", vsanHealthServiceWaitTime))
		time.Sleep(time.Duration(vsanHealthServiceWaitTime) * time.Second)

		ginkgo.By("Create CNS register volume with above created FCD ")
		cnsRegisterVolume := getCNSRegisterVolumeSpec(ctx, namespace, fcdID, "", pvcName, v1.ReadWriteOnce)
		err = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		time.Sleep(time.Duration(20) * time.Second)
		cnsRegisterVolumeName := cnsRegisterVolume.GetName()
		framework.Logf("CNS register volume name : %s", cnsRegisterVolumeName)

		ginkgo.By("Verify the error message, when SPSService is down, CRD should not be successful")
		cnsRegisterVolume = getCNSRegistervolume(ctx, restConfig, cnsRegisterVolume)
		actualErrorMsg := cnsRegisterVolume.Status.Error
		expectedErrorMsg := "failed to create CNS volume"
		gomega.Expect(strings.Contains(actualErrorMsg, expectedErrorMsg), gomega.BeTrue())

		ginkgo.By(fmt.Sprintln("Starting sps on the vCenter host"))
		err = invokeVCenterServiceControl(ctx, startOperation, spsServiceName, vcAddress)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow sps to come up again", vsanHealthServiceWaitTime))
		time.Sleep(time.Duration(vsanHealthServiceWaitTime) * time.Second)
		isSPSserviceStopped = false

		ginkgo.By("Wait for some time for the updated CRD to create PV , PVC")
		framework.ExpectNoError(waitForCNSRegisterVolumeToGetCreated(ctx,
			restConfig, namespace, cnsRegisterVolume, poll, pollTimeout))

		ginkgo.By("verify created PV, PVC")
		pvc, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pv := getPvFromClaim(client, namespace, pvcName)
		verifyBidirectionalReferenceOfPVandPVC(ctx, client, pvc, pv, fcdID)

		defer func() {
			testCleanUpUtil(ctx, restConfig, nil, namespace, pvc.Name, pv.Name)
		}()
	})

	// This test verifies the static provisioning workflow on supervisour
	// cluster - on non shared datastore.
	//
	// Test Steps:
	// 1. Create FCD with valid non shared storage policy.
	// 2. Create Resource quota.
	// 3. Create CNS register volume with above created FCD.
	// 4. Verify the error message.
	// 5. Delete Resource quota.
	ginkgo.It("[csi-supervisor] Verify static provisioning workflow SVC - On "+
		"non shared datastore", ginkgo.Label(p2, block, wcp, vc70), func() {
		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		curtime := time.Now().Unix()
		curtimeinstring := strconv.FormatInt(curtime, 10)
		pvcName := "cns-pvc-" + curtimeinstring
		framework.Logf("pvc name :%s", pvcName)
		namespace = getNamespaceToRunTests(f)

		// Get a config to talk to the apiserver.
		restConfig := getRestConfigClient()

		ginkgo.By("Get the Policy ID and assign policy to storage class")
		nonsharedDatastoreName := GetAndExpectStringEnvVar(envStoragePolicyNameForNonSharedDatastores)
		profileID := e2eVSphere.GetSpbmPolicyID(nonsharedDatastoreName)
		framework.Logf("non shared datastore , profileID : %s", profileID)
		scParameters := make(map[string]string)
		scParameters["storagePolicyID"] = profileID

		ginkgo.By("Creating Resource quota")
		setStoragePolicyQuota(ctx, restConfig, nonsharedDatastoreName, namespace, rqLimit)

		ginkgo.By("Create FCD")
		fcdID, err := e2eVSphere.createFCDwithValidProfileID(ctx,
			"nonsharedfcd"+curtimeinstring, profileID, diskSizeInMb, nonsharedDatastore.Reference())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		deleteFCDRequired = false
		ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow newly created FCD:%s to sync with pandora",
			pandoraSyncWaitTime, fcdID))
		time.Sleep(time.Duration(pandoraSyncWaitTime) * time.Second)

		ginkgo.By("Create CNS register volume with above created FCD")
		cnsRegisterVolume := getCNSRegisterVolumeSpec(ctx, namespace, fcdID, "", pvcName, v1.ReadWriteOnce)
		err = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		time.Sleep(time.Duration(20) * time.Second)
		cnsRegisterVolumeName := cnsRegisterVolume.GetName()
		framework.Logf("CNS register volume name : %s", cnsRegisterVolumeName)

		ginkgo.By("Verify the error message, when non-shared data store is used to created CRD")
		cnsRegisterVolume = getCNSRegistervolume(ctx, restConfig, cnsRegisterVolume)
		actualErrorMsg := cnsRegisterVolume.Status.Error
		expectedErrorMsg := "Volume in the spec is not accessible to all nodes in the cluster"
		gomega.Expect(strings.Contains(actualErrorMsg, expectedErrorMsg), gomega.BeTrue())

	})

	// This test verifies the static provisioning on SVC, when FCD with
	// no storage policy.
	//
	// Test Steps:
	// 1. Create FCD with out storage policy.
	// 2. Create Resource quota.
	// 3. Create CNS register volume with above created FCD.
	// 4. Verify the error message.
	ginkgo.It("[csi-supervisor] Verify creating static provisioning workflow when FCD "+
		"with no storage policy", ginkgo.Label(p2, block, wcp, negative, vc70), func() {
		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		curtime := time.Now().Unix()
		curtimeinstring := strconv.FormatInt(curtime, 10)
		pvcName := "cns-pvc-" + curtimeinstring
		framework.Logf("pvc name :%s", pvcName)
		namespace = getNamespaceToRunTests(f)

		restConfig, _, _ := staticProvisioningPreSetUpUtil(ctx, f, client, storagePolicyName)

		ginkgo.By("Create FCD")
		fcdID, err := e2eVSphere.createFCD(ctx, "staticfcd"+curtimeinstring, diskSizeInMinMb, defaultDatastore.Reference())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		deleteFCDRequired = false
		ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow newly created FCD:%s to sync with pandora",
			pandoraSyncWaitTime, fcdID))
		time.Sleep(time.Duration(pandoraSyncWaitTime) * time.Second)

		ginkgo.By("Create CNS register volume with above created FCD")
		cnsRegisterVolume := getCNSRegisterVolumeSpec(ctx, namespace, fcdID, "", pvcName, v1.ReadWriteOnce)
		err = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		time.Sleep(time.Duration(40) * time.Second)
		cnsRegisterVolumeName := cnsRegisterVolume.GetName()
		framework.Logf("CNS register volume name : %s", cnsRegisterVolumeName)

		// In Case of datastore type VSAN and VVOL, FCD is created with default
		// storage policy even when no policy is specified.
		// Hence this check is necessary to validate the error message.
		ginkgo.By("Check the type of datastore, to validate the expected message")
		var expectedErrorMsg string
		dataStoreType, err := defaultDatastore.Type(ctx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		framework.Logf("Datastore type: %s", dataStoreType)
		if dataStoreType == "vsan" || dataStoreType == "VVOL" {
			expectedErrorMsg = "Failed to find K8S Storageclass mapping"
		} else {
			expectedErrorMsg = "Volume in the spec doesn't have storage policy associated with it"
		}

		ginkgo.By("Verify the error message, when FCD without storage policy is used")
		cnsRegisterVolume = getCNSRegistervolume(ctx, restConfig, cnsRegisterVolume)
		actualErrorMsg := cnsRegisterVolume.Status.Error
		framework.Logf("Error message :%s", actualErrorMsg)

		gomega.Expect(strings.Contains(actualErrorMsg, expectedErrorMsg), gomega.BeTrue())
	})

	// This test verifies the static provisioning workflow on SVC, when tried to
	// import volume with a storage policy that doesn't belong to the namespace.
	//
	// Test Steps:
	// 1. Create a Namespace.
	// 2. Create a storage policy.
	// 3. Create FCD with the above created storage policy.
	// 4. Import the volume created in step 3 to namespace created in step 1.
	ginkgo.It("[csi-supervisor] static provisioning workflow - "+
		"when tried to import volume with a storage policy that "+
		"doesn't belong to the namespace", ginkgo.Label(p2, block, wcp, negative, vc70), func() {

		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		curtime := time.Now().Unix()
		curtimeinstring := strconv.FormatInt(curtime, 10)
		pvcName := "cns-pvc-" + curtimeinstring
		framework.Logf("pvc name :%s", pvcName)

		restConfig := getRestConfigClient()

		storagePolicyName2 := GetAndExpectStringEnvVar(envStoragePolicyNameForSharedDatastores2)
		ginkgo.By("Get storage Policy")
		ginkgo.By(fmt.Sprintf("storagePolicyName: %s", storagePolicyName2))
		profileID := e2eVSphere.GetSpbmPolicyID(storagePolicyName2)
		framework.Logf("Profile ID :%s", profileID)
		scParameters := make(map[string]string)
		scParameters["storagePolicyID"] = profileID

		ginkgo.By("Create FCD")
		fcdID, err := e2eVSphere.createFCDwithValidProfileID(ctx,
			"staticfcd"+curtimeinstring, profileID, diskSizeInMinMb, defaultDatastore.Reference())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		defer func() {
			err := e2eVSphere.deleteFCD(ctx, fcdID, defaultDatastore.Reference())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}()

		ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow newly created FCD:%s to sync with pandora",
			pandoraSyncWaitTime, fcdID))
		time.Sleep(time.Duration(pandoraSyncWaitTime) * time.Second)

		ginkgo.By("Create CNS register volume with above created FCD")
		framework.Logf("Auto created namespace :%s", namespace)
		cnsRegisterVolume := getCNSRegisterVolumeSpec(ctx, namespace, fcdID, "", pvcName, v1.ReadWriteOnce)
		err = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = waitForCNSRegisterVolumeToGetCreated(ctx,
			restConfig, namespace, cnsRegisterVolume, poll, pollTimeoutShort)
		gomega.Expect(err).To(gomega.HaveOccurred())

		cnsRegisterVolumeName := cnsRegisterVolume.GetName()
		framework.Logf(" CNS register volume name : %s", cnsRegisterVolumeName)

		ginkgo.By("Verify the error message, when FCD without storage policy is used")
		cnsRegisterVolume = getCNSRegistervolume(ctx, restConfig, cnsRegisterVolume)
		actualErrorMsg := cnsRegisterVolume.Status.Error
		framework.Logf("Error message :%s", actualErrorMsg)
		expectedErrorMsg := fmt.Sprintf(
			"Failed to find K8S Storageclass mapping storagepolicyId: %s and assigned to namespace: %s",
			profileID, namespace)
		framework.Logf("Error message :%s", expectedErrorMsg)
		gomega.Expect(strings.Contains(actualErrorMsg, expectedErrorMsg), gomega.BeTrue())
		pvc = nil

		defer func() {
			pvName := "static-pv-" + fcdID
			framework.Logf("Deleting PersistentVolume %s", pvName)
			framework.ExpectNoError(fpv.DeletePersistentVolume(ctx, client, pvName))
			pv, err = client.CoreV1().PersistentVolumes().Get(context.TODO(), pvName, metav1.GetOptions{})
			if !apierrors.IsNotFound(err) {
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}
			if pv != nil {
				framework.ExpectNoError(fpv.DeletePersistentVolume(ctx, client, pvName))
			}
			pv = nil
		}()

	})

	// This test verifies the static provisioning workflow in guest cluster.
	//
	// Test Steps:
	// 1. Create FCD with valid storage policy on gc-svc.
	// 2. Create Resource quota.
	// 3. Create CNS register volume with above created FCD on SVC.
	// 4. verify CNS register volume creation fails

	ginkgo.It("[vmc] Create CNS register volume on management datastore", ginkgo.Label(deprecated), func() {
		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mgmtDatastoreURL := os.Getenv("MANAGEMENT_DATASTORE_URL")
		if mgmtDatastoreURL == "" {
			ginkgo.Skip("Env MANAGEMENT_DATASTORE_URL is missing")
		}

		curtime := time.Now().Unix()
		randomValue := rand.Int()
		val := strconv.FormatInt(int64(randomValue), 10)
		val = string(val[1:3])
		curtimestring := strconv.FormatInt(curtime, 10)
		svpvcName := "cns-pvc-" + curtimestring + val
		framework.Logf("pvc name :%s", svpvcName)
		namespace = getNamespaceToRunTests(f)

		_, _, profileID := staticProvisioningPreSetUpUtil(ctx, f, client, storagePolicyName)

		// Get supvervisor cluster client.
		_, svNamespace := getSvcClientAndNamespace()

		// Get restConfig.
		var restConfig *restclient.Config
		if k8senv := GetAndExpectStringEnvVar("SUPERVISOR_CLUSTER_KUBE_CONFIG"); k8senv != "" {
			restConfig, err = clientcmd.BuildConfigFromFlags("", k8senv)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}

		mgmtDatastore, err = getDatastoreByURL(ctx, mgmtDatastoreURL, defaultDatacenter)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Creating FCD (CNS Volume)")
		fcdID, err := e2eVSphere.createFCDwithValidProfileID(ctx,
			"staticfcd"+curtimestring, profileID, diskSizeInMb, mgmtDatastore.Reference())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		deleteFCDRequired = true
		ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow newly created FCD:%s to sync with pandora",
			pandoraSyncWaitTime, fcdID))
		time.Sleep(time.Duration(pandoraSyncWaitTime) * time.Second)

		ginkgo.By("Create CNS register volume with above created FCD")
		cnsRegisterVolume := getCNSRegisterVolumeSpec(ctx, svNamespace, fcdID, "", svpvcName, v1.ReadWriteOnce)
		err = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = waitForCNSRegisterVolumeToGetCreated(ctx,
			restConfig, namespace, cnsRegisterVolume, poll, supervisorClusterOperationsTimeout)
		gomega.Expect(err).To(gomega.HaveOccurred())

		cnsRegisterVolumeName := cnsRegisterVolume.GetName()
		framework.Logf("CNS register volume name : %s", cnsRegisterVolumeName)

	})

	// This test verifies the static provisioning workflow in guest cluster.
	//
	// Test Steps:
	// 1. Create FCD with valid storage policy on gc-svc.
	// 2. Create Resource quota.
	// 3. Create CNS register volume with above created FCD on SVC.
	// 4. verify PV, PVC got created , check the bidirectional reference on svc.
	// 5. On GC create a PV by pointing volume handle got created by static
	//    provisioning on gc-svc (in step 4).
	// 6. On GC create a PVC pointing to above created PV.
	// 7. Wait for PV , PVC to get bound.
	// 8. Create POD, verify the status.
	// 9. Delete all the above created PV, PVC and resource quota.
	ginkgo.It("[csi-guest] static volume provisioning on guest "+
		"cluster", ginkgo.Label(p0, block, tkg, vc70), func() {
		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		curtime := time.Now().Unix()
		randomValue := rand.Int()
		val := strconv.FormatInt(int64(randomValue), 10)
		val = string(val[1:3])
		curtimestring := strconv.FormatInt(curtime, 10)
		svpvcName := "cns-pvc-" + curtimestring + val
		framework.Logf("pvc name :%s", svpvcName)
		namespace = getNamespaceToRunTests(f)

		_, storageclass, profileID := staticProvisioningPreSetUpUtil(ctx, f, client, storagePolicyName)

		// Get supvervisor cluster client.
		svcClient, svNamespace := getSvcClientAndNamespace()

		// Get restConfig.
		var restConfig *restclient.Config
		if k8senv := GetAndExpectStringEnvVar("SUPERVISOR_CLUSTER_KUBE_CONFIG"); k8senv != "" {
			restConfig, err = clientcmd.BuildConfigFromFlags("", k8senv)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}

		ginkgo.By("Creating FCD (CNS Volume)")
		fcdID, err := e2eVSphere.createFCDwithValidProfileID(ctx,
			"staticfcd"+curtimestring, profileID, diskSizeInMb, defaultDatastore.Reference())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		deleteFCDRequired = false
		ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow newly created FCD:%s to sync with pandora",
			pandoraSyncWaitTime, fcdID))
		time.Sleep(time.Duration(pandoraSyncWaitTime) * time.Second)

		ginkgo.By("Create CNS register volume with above created FCD")
		cnsRegisterVolume := getCNSRegisterVolumeSpec(ctx, svNamespace, fcdID, "", svpvcName, v1.ReadWriteOnce)
		err = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		framework.ExpectNoError(waitForCNSRegisterVolumeToGetCreated(ctx,
			restConfig, namespace, cnsRegisterVolume, poll, supervisorClusterOperationsTimeout))
		cnsRegisterVolumeName := cnsRegisterVolume.GetName()
		framework.Logf("CNS register volume name : %s", cnsRegisterVolumeName)

		ginkgo.By("verify created PV, PVC and check the bidirectional reference")
		svcPVC, err := svcClient.CoreV1().PersistentVolumeClaims(svNamespace).Get(ctx, svpvcName, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		svcPV := getPvFromClaim(svcClient, svNamespace, svpvcName)
		verifyBidirectionalReferenceOfPVandPVC(ctx, svcClient, svcPVC, svcPV, fcdID)
		// TODO: add volume health check after PVC creation.

		volumeHandle := svcPVC.GetName()
		framework.Logf("Volume Handle :%s", volumeHandle)

		ginkgo.By("Creating PV in guest cluster")
		gcPV := getPersistentVolumeSpecWithStorageclass(volumeHandle,
			v1.PersistentVolumeReclaimRetain, storageclass.Name, nil, diskSize)
		gcPV, err = client.CoreV1().PersistentVolumes().Create(ctx, gcPV, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		gcPVName := gcPV.GetName()
		time.Sleep(time.Duration(10) * time.Second)
		framework.Logf("PV name in GC : %s", gcPVName)

		ginkgo.By("Creating PVC in guest cluster")
		gcPVC := getPVCSpecWithPVandStorageClass(svpvcName, "default", nil, gcPVName, storageclass.Name, diskSize)
		gcPVC, err = client.CoreV1().PersistentVolumeClaims("default").Create(ctx, gcPVC, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Waiting for claim to be in bound phase")
		err = fpv.WaitForPersistentVolumeClaimPhase(ctx, v1.ClaimBound, client,
			"default", gcPVC.Name, framework.Poll, framework.ClaimProvisionTimeout)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		framework.Logf("PVC name in GC : %s", gcPVC.GetName())

		ginkgo.By("Creating pod")
		pod, err := createPod(ctx, client, "default", nil, []*v1.PersistentVolumeClaim{gcPVC}, false, "")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		podName := pod.GetName()
		framework.Logf("podName: %s", podName)

		ginkgo.By("Deleting the pod")
		err = fpod.DeletePodWithWait(ctx, client, pod)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Verify volume is detached from the node")
		isDiskDetached, err := e2eVSphere.waitForVolumeDetachedFromNode(client,
			gcPV.Spec.CSI.VolumeHandle, pod.Spec.NodeName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(isDiskDetached).To(gomega.BeTrue(), fmt.Sprintf("Volume %q is not detached from the node %q",
			gcPV.Spec.CSI.VolumeHandle, pod.Spec.NodeName))

		ginkgo.By("Deleting the PV Claim")
		framework.ExpectNoError(fpv.DeletePersistentVolumeClaim(ctx, client, gcPVC.Name, "default"),
			"Failed to delete PVC ", gcPVC.Name)

		ginkgo.By("Verify PV should be released not deleted")
		framework.Logf("Waiting for PV to move to released state")
		// TODO: replace sleep with polling mechanism.
		time.Sleep(time.Duration(100) * time.Second)
		gcPV, err = client.CoreV1().PersistentVolumes().Get(ctx, gcPVName, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gcPVStatus := gcPV.Status.Phase
		if gcPVStatus != "Released" {
			framework.Logf("gcPVStatus: %s", gcPVStatus)
			gomega.Expect(gcPVStatus).NotTo(gomega.HaveOccurred())
		}

		ginkgo.By("Verify volume is not deleted in Supervisor Cluster")
		volumeExists := verifyVolumeExistInSupervisorCluster(svcPVC.GetName())
		gomega.Expect(volumeExists).NotTo(gomega.BeFalse())

		defer func() {
			testCleanUpUtil(ctx, restConfig, nil, svNamespace, svcPVC.Name, svcPV.Name)
		}()

	})

	// Perform dynamic and static volume provisioning together and verify the
	// PVC creation, Create Pod and then delete namespace.
	// Make sure all PV, PVC, POd's and CNS register volume got deleted.
	//
	// Test Steps:
	// 1. Create CNS volume (FCD).
	// 2. Create Resource quota.
	// 3. Create CNS register volume with above created FCD.
	// 4. Create pvc through dynamic volume provisioning.
	// 5. verify PV, PVC got created through static volume provisioning.
	// 6. Create Pod with the PVC created in step 4 and 5.
	// 7. Delete Namespace.
	// 8. Verify that PV's got deleted (This ensures that all PVC, CNS register
	//    volumes and POD's are deleted).
	ginkgo.It("[csi-supervisor] Perform static and dynamic provisioning together, "+
		"Create Pod and delete Namespace", ginkgo.Label(p1, block, wcp, vc70), func() {
		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		namespaceToDelete := GetAndExpectStringEnvVar(envSupervisorClusterNamespaceToDelete)
		framework.Logf("Namespace To delete :%s", namespaceToDelete)

		curtime := time.Now().Unix()
		curtimestring := strconv.FormatInt(curtime, 10)
		randomValue := rand.Int()
		val := strconv.FormatInt(int64(randomValue), 10)
		val = string(val[1:3])
		pvcName := "cns-pvc-" + curtimestring + val
		framework.Logf("pvc name :%s", pvcName)

		// Get a config to talk to the apiserver.
		restConfig, storageclass, profileID := staticProvisioningPreSetUpUtil(ctx, f, client, storagePolicyName)

		ginkgo.By("create resource quota")
		setStoragePolicyQuota(ctx, restConfig, storageclass.Name, namespace, rqLimit)

		ginkgo.By("Creating FCD (CNS Volume)")
		fcdID, err := e2eVSphere.createFCDwithValidProfileID(ctx,
			"staticfcd"+curtimestring+val, profileID, diskSizeInMb, defaultDatastore.Reference())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		deleteFCDRequired = false
		ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow newly created FCD:%s to sync with pandora",
			pandoraSyncWaitTime, fcdID))
		time.Sleep(time.Duration(pandoraSyncWaitTime) * time.Second)

		ginkgo.By("Perform dynamic provisioning and create PVC")
		pvc1, err := createPVC(ctx, client, namespaceToDelete, nil, "", storageclass, "")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		framework.Logf("Dynamically created PVC :%s", pvc1.Name)

		ginkgo.By("Dynamic volume provisioning - Waiting for claim to be in bound phase")
		err = fpv.WaitForPersistentVolumeClaimPhase(ctx, v1.ClaimBound, client,
			pvc1.Namespace, pvc1.Name, framework.Poll, framework.ClaimProvisionTimeout)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pv1 := getPvFromClaim(client, namespaceToDelete, pvc1.Name)

		ginkgo.By("Create CNS register volume with above created FCD")
		cnsRegisterVolume := getCNSRegisterVolumeSpec(ctx, namespaceToDelete, fcdID, "", pvcName, v1.ReadWriteOnce)
		err = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By("Wait for CNS register volume to get created")
		framework.ExpectNoError(waitForCNSRegisterVolumeToGetCreated(ctx,
			restConfig, namespaceToDelete, cnsRegisterVolume, poll, supervisorClusterOperationsTimeout))
		cnsRegisterVolumeName := cnsRegisterVolume.GetName()
		framework.Logf("CNS register volume name : %s", cnsRegisterVolumeName)

		ginkgo.By("verify created PV, PVC and check the bidirectional referance")
		pvc2, err := client.CoreV1().PersistentVolumeClaims(namespaceToDelete).Get(ctx, pvcName, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		framework.Logf("Statically created PVC :%s", pvc2.Name)
		pv2 := getPvFromClaim(client, namespaceToDelete, pvcName)
		verifyBidirectionalReferenceOfPVandPVC(ctx, client, pvc2, pv2, fcdID)

		ginkgo.By("Creating pod")
		pod1, err := createPod(ctx, client, namespaceToDelete, nil, []*v1.PersistentVolumeClaim{pvc1}, false, "")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		podName1 := pod1.GetName()
		framework.Logf("First podName: %s", podName1)
		pod2, err := createPod(ctx, client, namespaceToDelete, nil, []*v1.PersistentVolumeClaim{pvc2}, false, "")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		podName2 := pod2.GetName()
		framework.Logf("Second podName: %s", podName2)

		ginkgo.By("Delete Namspace")
		err = client.CoreV1().Namespaces().Delete(ctx, namespaceToDelete, metav1.DeleteOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		framework.ExpectNoError(waitForNamespaceToGetDeleted(ctx,
			client, namespaceToDelete, poll, supervisorClusterOperationsTimeout))

		ginkgo.By("Verify PV got deleted")
		framework.ExpectNoError(fpv.WaitForPersistentVolumeDeleted(ctx, client,
			pv1.Name, poll, supervisorClusterOperationsTimeout))
		framework.ExpectNoError(e2eVSphere.waitForCNSVolumeToBeDeleted(pv1.Spec.CSI.VolumeHandle))

		framework.ExpectNoError(fpv.WaitForPersistentVolumeDeleted(ctx, client,
			pv2.Name, poll, supervisorClusterOperationsTimeout))
		framework.ExpectNoError(e2eVSphere.waitForCNSVolumeToBeDeleted(pv2.Spec.CSI.VolumeHandle))

	})

	// Verify static provisioning - import VMDK.
	// Presetup: Create VMDK with valid storage policy.
	//
	// Test Steps:
	// 1. Create VMDK with valid storage policy.
	// 2. Create Resource quota.
	// 3. Create CNS register volume with above created VMDK.
	// 4. verify PV, PVC got created , check the bidirectional reference.
	ginkgo.It("[csi-supervisor] Verify static provisioning - import "+
		"VMDK", ginkgo.Label(p1, block, wcp, vc70), func() {
		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		dataStoreType, err := defaultDatastore.Type(ctx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		framework.Logf("Datastore type: %s", dataStoreType)

		if dataStoreType != "vsan" {
			ginkgo.Skip("Skipping static provisioning - import VMDK test Since the testbed dont have vSAN datastore - " +
				"Because for this test uses vSAN default datastore policy ")
		} else {
			curtime := time.Now().Unix()
			randomValue := rand.Int()
			val := strconv.FormatInt(int64(randomValue), 10)
			val = string(val[1:3])
			curtimestring := strconv.FormatInt(curtime, 10)
			pvcName := "cns-pvc-" + curtimestring + val
			framework.Logf("pvc name :%s", pvcName)
			namespace = getNamespaceToRunTests(f)

			restConfig, storageclass, _ := staticProvisioningPreSetUpUtilForVMDKTests(ctx)

			defer func() {
				framework.Logf("Delete storage class")
				err = client.StorageV1().StorageClasses().Delete(ctx, storageclass.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}()

			vmdk := GetAndExpectStringEnvVar(envVmdkDiskURL)
			framework.Logf("VMDK path : %s", vmdk)
			ginkgo.By("Create CNS register volume with VMDK")
			cnsRegisterVolume := getCNSRegisterVolumeSpec(ctx, namespace, "", vmdk, pvcName, v1.ReadWriteOnce)
			err = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			framework.ExpectNoError(waitForCNSRegisterVolumeToGetCreated(ctx,
				restConfig, namespace, cnsRegisterVolume, poll, supervisorClusterOperationsTimeout))
			cnsRegisterVolumeName := cnsRegisterVolume.GetName()
			framework.Logf("CNS register volume name : %s", cnsRegisterVolumeName)

			ginkgo.By("verify created PV, PVC and check the bidirectional reference")
			pvc, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			pv := getPvFromClaim(client, namespace, pvcName)
			pvName := pvc.Spec.VolumeName
			// pvName will be like static-pv-<volumeID> This volumeID Should be same as in PV volumeHandle
			volumeID := strings.ReplaceAll(pvName, "static-pv-", "")
			verifyBidirectionalReferenceOfPVandPVC(ctx, client, pvc, pv, volumeID)

			// TODO: need to add code to delete VMDK hard disk and to create POD.

			defer func() {
				ginkgo.By("Deleting the PV Claim")
				framework.ExpectNoError(fpv.DeletePersistentVolumeClaim(ctx, client, pvcName, namespace),
					"Failed to delete PVC", pvcName)
				pvc = nil

				ginkgo.By("PV will be in released state , hence delete PV explicitly")
				framework.ExpectNoError(fpv.DeletePersistentVolume(ctx, client, pv.GetName()))
				pv = nil

				ginkgo.By("Verify CRD should be deleted automatically")
				framework.ExpectNoError(waitForCNSRegisterVolumeToGetDeleted(ctx,
					restConfig, namespace, cnsRegisterVolume, poll, supervisorClusterOperationsTimeout))

			}()
		}

	})

	// Specify VolumeID and DiskURL together and verify the error message.
	// Presetup: Create VMDK with valid storage policy.
	//
	// Test Steps:
	// 1. Create FCD.
	// 2. Create Resource quota.
	// 3. Create CNS register volume with above created VMDK and FCDID.
	// 4. Verify the error message "VolumeID and DiskURLPath cannot be specified
	//    together".
	ginkgo.It("[csi-supervisor] Specify VolumeID and DiskURL together and "+
		"verify the error message", ginkgo.Label(p2, block, wcp, negative, vc70), func() {

		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		dataStoreType, err := defaultDatastore.Type(ctx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		framework.Logf("Datastore type: %s", dataStoreType)
		if dataStoreType != "vsan" {
			ginkgo.Skip("Skipping 'Specify VolumeID and DiskURL together and verify the error message' " +
				"test since the testbed does not have vSAN datastore")
		} else {
			curtime := time.Now().Unix()
			randomValue := rand.Int()
			val := strconv.FormatInt(int64(randomValue), 10)
			val = string(val[1:3])
			curtimestring := strconv.FormatInt(curtime, 10)
			pvcName := "cns-pvc-" + curtimestring + val
			framework.Logf("pvc name :%s", pvcName)
			namespace = getNamespaceToRunTests(f)

			restConfig, storageclass, profileID := staticProvisioningPreSetUpUtilForVMDKTests(ctx)

			defer func() {
				framework.Logf("Delete storage class")
				err = client.StorageV1().StorageClasses().Delete(ctx, storageclass.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}()

			ginkgo.By("Creating FCD Disk")
			fcdID, err := e2eVSphere.createFCDwithValidProfileID(ctx,
				"staticfcd"+curtimestring+val, profileID, diskSizeInMb, defaultDatastore.Reference())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			framework.Logf("fcdID  : %s", fcdID)
			deleteFCDRequired = false

			vmdk := GetAndExpectStringEnvVar(envVmdkDiskURL)
			framework.Logf("VMDK path : %s", vmdk)
			ginkgo.By("Create CNS register volume with VMDK")
			cnsRegisterVolume := getCNSRegisterVolumeSpec(ctx, namespace, fcdID, vmdk, pvcName, v1.ReadWriteOnce)
			err = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = waitForCNSRegisterVolumeToGetCreated(ctx,
				restConfig, namespace, cnsRegisterVolume, poll, supervisorClusterOperationsTimeout)
			gomega.Expect(err).To(gomega.HaveOccurred())
			cnsRegisterVolumeName := cnsRegisterVolume.GetName()
			framework.Logf("CNS register volume name : %s", cnsRegisterVolumeName)

			ginkgo.By("Verify the error message")
			cnsRegisterVolume = getCNSRegistervolume(ctx, restConfig, cnsRegisterVolume)
			actualErrorMsg := cnsRegisterVolume.Status.Error
			framework.Logf("Error message :%s", actualErrorMsg)
			expectedErrorMsg := "VolumeID and DiskURLPath cannot be specified together"
			gomega.Expect(strings.Contains(actualErrorMsg, expectedErrorMsg), gomega.BeTrue())

		}
	})

	/*
		Full sync to deregister/delete volume
		STEPS:
		1.Create FCD disk.
		2.Creating Static PV with FCD ID and PVC from it.
		3.Put vsan health service down.
		4.Delete the PVC and PV.
		5.Bring up vsan health service down.
		6.Allow FullSync to Deregister FCD.
		7.Verify Volume is deleted.
		8.Delete FCD.
	*/
	ginkgo.It("[csi-block-vanilla] [csi-supervisor] Full sync to deregister/delete "+
		"volume", ginkgo.Label(p0, block, wcp, vanilla, core, vc70), func() {
		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var fcdID string
		curr_time := time.Now().Unix()
		curTimeString := strconv.FormatInt(curr_time, 10)
		pvcName := "cns-pvc-" + curTimeString
		framework.Logf("pvc name :%s", pvcName)
		var restConfig *restclient.Config

		if vanillaCluster {
			ginkgo.By("Creating FCD Disk")
			fcdID, err = e2eVSphere.createFCD(ctx, "BasicStaticFCD", diskSizeInMb, defaultDatastore.Reference())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		} else if supervisorCluster {
			restConfig = getRestConfigClient()
			storagePolicyName := GetAndExpectStringEnvVar(envStoragePolicyNameForSharedDatastores)
			profileID := e2eVSphere.GetSpbmPolicyID(storagePolicyName)
			framework.Logf("Profile ID :%s", profileID)
			ginkgo.By("Creating FCD (CNS Volume)")
			fcdID, err = e2eVSphere.createFCDwithValidProfileID(ctx,
				"staticfcd"+curTimeString, profileID, diskSizeInMb, defaultDatastore.Reference())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

		}

		ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow newly created FCD:%s to sync with pandora",
			pandoraSyncWaitTime, fcdID))
		time.Sleep(time.Duration(pandoraSyncWaitTime) * time.Second)

		defer func() {
			if !supervisorCluster {
				ginkgo.By("Deleting FCD")
				err := e2eVSphere.deleteFCD(ctx, fcdID, defaultDatastore.Reference())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}
		}()

		if vanillaCluster {
			// Creating label for PV.
			// PVC will use this label as Selector to find PV.
			staticPVLabels := make(map[string]string)
			staticPVLabels["fcd-id"] = fcdID

			ginkgo.By("Creating the PV")
			pv = getPersistentVolumeSpec(fcdID, v1.PersistentVolumeReclaimRetain, staticPVLabels, ext4FSType)
			pv, err = client.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{})
			if err != nil {
				return
			}
			err = e2eVSphere.waitForCNSVolumeToBeCreated(pv.Spec.CSI.VolumeHandle)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("Creating the PVC")
			pvc = getPersistentVolumeClaimSpec(namespace, staticPVLabels, pv.Name)
			pvc, err = client.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Wait for PV and PVC to Bind.
			framework.ExpectNoError(fpv.WaitOnPVandPVC(ctx, client, f.Timeouts, namespace, pv, pvc))

		} else if supervisorCluster {
			ginkgo.By("Create CNS register volume with above created FCD ")
			cnsRegisterVolume := getCNSRegisterVolumeSpec(ctx, namespace, fcdID, "", pvcName, v1.ReadWriteOnce)
			err = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			framework.ExpectNoError(waitForCNSRegisterVolumeToGetCreated(ctx, restConfig,
				namespace, cnsRegisterVolume, poll, supervisorClusterOperationsTimeout))
			cnsRegisterVolumeName := cnsRegisterVolume.GetName()
			framework.Logf("CNS register volume name : %s", cnsRegisterVolumeName)

			ginkgo.By(" verify created PV, PVC and check the bidirectional reference")
			pvc, err = client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			pv = getPvFromClaim(client, namespace, pvcName)
			verifyBidirectionalReferenceOfPVandPVC(ctx, client, pvc, pv, fcdID)
		}

		ginkgo.By("Verifying CNS entry is present in cache")
		_, err = e2eVSphere.queryCNSVolumeWithResult(pv.Spec.CSI.VolumeHandle)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		ginkgo.By(fmt.Sprintln("Stopping vsan-health on the vCenter host"))
		isVsanHealthServiceStopped = true
		err = invokeVCenterServiceControl(ctx, stopOperation, vsanhealthServiceName, vcAddress)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = waitVCenterServiceToBeInState(ctx, vsanhealthServiceName, vcAddress, svcStoppedMessage)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		defer func() {
			if isVsanHealthServiceStopped {
				ginkgo.By(fmt.Sprintf("Starting %v on the vCenter host", vsanhealthServiceName))
				err = invokeVCenterServiceControl(ctx, startOperation, vsanhealthServiceName, vcAddress)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = waitVCenterServiceToBeInState(ctx, vsanhealthServiceName, vcAddress, svcRunningMessage)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				isVsanHealthServiceStopped = false
			}
		}()

		framework.Logf("Deleting PersistentVolumeClaim %s", pvc.Name)
		err = fpv.DeletePersistentVolumeClaim(ctx, client, pvc.Name, namespace)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, err = client.CoreV1().PersistentVolumeClaims(namespace).Get(context.TODO(), pvc.Name, metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}

		framework.Logf("Deleting PersistentVolume %s", pv.Name)
		err = fpv.DeletePersistentVolume(ctx, client, pv.Name)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, err = client.CoreV1().PersistentVolumes().Get(context.TODO(), pv.Name, metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}

		ginkgo.By(fmt.Sprintf("Starting %v on the vCenter host", vsanhealthServiceName))
		err = invokeVCenterServiceControl(ctx, startOperation, vsanhealthServiceName, vcAddress)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = waitVCenterServiceToBeInState(ctx, vsanhealthServiceName, vcAddress, svcRunningMessage)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		isVsanHealthServiceStopped = false

		if vanillaCluster {
			ginkgo.By("Trigger 2 full syncs on demand")
			restConfig := getRestConfigClient()
			cnsOperatorClient, err := k8s.NewClientForGroup(ctx, restConfig, cnsoperatorv1alpha1.GroupName)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			enableFullSyncTriggerFss(ctx, client, csiSystemNamespace, fullSyncFss)
			triggerFullSync(ctx, cnsOperatorClient)
		} else if supervisorCluster {
			ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow 2 full sync cycles to finish", fullSyncWaitTime))
			time.Sleep(time.Duration(fullSyncWaitTime*2) * time.Second)
		}

		ginkgo.By("Wait for CNS Volume to be deleted")
		err = e2eVSphere.waitForCNSVolumeToBeDeleted(pv.Spec.CSI.VolumeHandle)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

	})

	/*
		VMDK is deleted from datastore but CNS volume is still present
		STEPS:
		1.Create FCD disk.
		2.Creating Static PV with FCD ID and PVC from it.
		3.Delete the vmdk file associated this above FCD.
		4.Delete the PVC and PV.
		5.Wait for volume to be deleted from K8s.
		6.Wait for Volume to be deleted on CNS
	*/
	ginkgo.It("[csi-block-vanilla] [csi-supervisor] VMDK is deleted from datastore "+
		"but CNS volume is still present", ginkgo.Label(p1, block, wcp, vanilla, core, vc70), func() {
		var err error
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var fcdID string
		curr_time := time.Now().Unix()
		curTimeString := strconv.FormatInt(curr_time, 10)
		pvcName := "cns-pvc-" + curTimeString
		framework.Logf("pvc name :%s", pvcName)
		var restConfig *restclient.Config
		var sshClientConfig *ssh.ClientConfig
		var masterIP, vmdk string

		dataStoreType, err := defaultDatastore.Type(ctx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		if dataStoreType != "vsan" {
			ginkgo.Skip("Skipping static provisioning - import VMDK test Since the testbed dont have vSAN datastore - " +
				"Because for this test uses vSAN default datastore policy ")
		}

		if vanillaCluster {
			nimbusGeneratedK8sVmPwd := GetAndExpectStringEnvVar(nimbusK8sVmPwd)
			sshClientConfig = &ssh.ClientConfig{
				User: rootUser,
				Auth: []ssh.AuthMethod{
					ssh.Password(nimbusGeneratedK8sVmPwd),
				},
				HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			}
			ginkgo.By("Creating FCD Disk")
			fcdID, err = e2eVSphere.createFCD(ctx, "BasicStaticFCD", diskSizeInMb, defaultDatastore.Reference())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		} else if supervisorCluster {
			svcMasterPwd := GetAndExpectStringEnvVar(svcMasterPassword)
			sshClientConfig = &ssh.ClientConfig{
				User: rootUser,
				Auth: []ssh.AuthMethod{
					ssh.Password(svcMasterPwd),
				},
				HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			}
			restConfig = getRestConfigClient()
			storagePolicyName := GetAndExpectStringEnvVar(envStoragePolicyNameForSharedDatastores)
			profileID := e2eVSphere.GetSpbmPolicyID(storagePolicyName)
			framework.Logf("Profile ID :%s", profileID)
			ginkgo.By("Creating FCD (CNS Volume)")
			fcdID, err = e2eVSphere.createFCDwithValidProfileID(ctx,
				"staticfcd"+curTimeString, profileID, diskSizeInMb, defaultDatastore.Reference())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

		}

		ginkgo.By(fmt.Sprintf("Sleeping for %v seconds to allow newly created FCD:%s to sync with pandora",
			pandoraSyncWaitTime, fcdID))
		time.Sleep(time.Duration(pandoraSyncWaitTime) * time.Second)

		defer func() {
			if deleteFCDRequired {
				ginkgo.By("Deleting FCD")
				err := e2eVSphere.deleteFCD(ctx, fcdID, defaultDatastore.Reference())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}
		}()

		if vanillaCluster {
			// Creating label for PV.
			// PVC will use this label as Selector to find PV.
			staticPVLabels := make(map[string]string)
			staticPVLabels["fcd-id"] = fcdID

			ginkgo.By("Creating the PV")
			pv = getPersistentVolumeSpec(fcdID, v1.PersistentVolumeReclaimRetain, staticPVLabels, ext4FSType)
			pv, err = client.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{})
			if err != nil {
				return
			}
			err = e2eVSphere.waitForCNSVolumeToBeCreated(pv.Spec.CSI.VolumeHandle)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("Creating the PVC")
			pvc = getPersistentVolumeClaimSpec(namespace, staticPVLabels, pv.Name)
			pvc, err = client.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Wait for PV and PVC to Bind.
			framework.ExpectNoError(fpv.WaitOnPVandPVC(ctx, client, f.Timeouts, namespace, pv, pvc))

		} else if supervisorCluster {
			vmdk = GetAndExpectStringEnvVar(envVmdkDiskURL)
			framework.Logf("VMDK path : %s", vmdk)
			ginkgo.By("Create CNS register volume with VMDK")
			cnsRegisterVolume := getCNSRegisterVolumeSpec(ctx, namespace, "", vmdk, pvcName, v1.ReadWriteOnce)
			err = createCNSRegisterVolume(ctx, restConfig, cnsRegisterVolume)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			framework.ExpectNoError(waitForCNSRegisterVolumeToGetCreated(ctx, restConfig,
				namespace, cnsRegisterVolume, poll, supervisorClusterOperationsTimeout))
			cnsRegisterVolumeName := cnsRegisterVolume.GetName()
			framework.Logf("CNS register volume name : %s", cnsRegisterVolumeName)

			ginkgo.By(" verify created PV, PVC and check the bidirectional reference")
			pvc, err = client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			pv = getPvFromClaim(client, namespace, pvcName)
			verifyBidirectionalReferenceOfPVandPVC(ctx, client, pvc, pv, fcdID)
		}

		ginkgo.By("Verifying CNS entry is present in cache")
		_, err = e2eVSphere.queryCNSVolumeWithResult(pv.Spec.CSI.VolumeHandle)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		if vanillaCluster {
			k8sMasterIPs := getK8sMasterIPs(ctx, client)
			masterIP = k8sMasterIPs[0]
		} else if supervisorCluster {
			masterIP = GetAndExpectStringEnvVar(svcMasterIP)
		}

		datastoreName, _, err := e2eVSphere.fetchDatastoreNameFromDatastoreUrl(ctx, fcdID)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		framework.Logf("Get vmdk path from volume handle")
		if vanillaCluster {
			vmdk = getVmdkPathFromVolumeHandle(sshClientConfig, masterIP, datastoreName, pv.Spec.CSI.VolumeHandle)
		}
		esxHost := GetAndExpectStringEnvVar(envEsxHostIP)
		ginkgo.By("Delete the vmdk file associasted with the above FCD")
		err = deleteVmdk(ctx, esxHost, vmdk)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		framework.Logf("Deleting PersistentVolumeClaim %s", pvc.Name)
		err = fpv.DeletePersistentVolumeClaim(ctx, client, pvc.Name, namespace)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, err = client.CoreV1().PersistentVolumeClaims(namespace).Get(context.TODO(), pvc.Name, metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}

		framework.Logf("Deleting PersistentVolume %s", pv.Name)
		err = fpv.DeletePersistentVolume(ctx, client, pv.Name)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, err = client.CoreV1().PersistentVolumes().Get(context.TODO(), pv.Name, metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}

		ginkgo.By("Wait for CNS Volume to be deleted")
		err = e2eVSphere.waitForCNSVolumeToBeDeleted(pv.Spec.CSI.VolumeHandle)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

	})
})
