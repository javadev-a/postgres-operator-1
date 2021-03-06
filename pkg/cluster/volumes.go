package cluster

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/pkg/api/v1"

	"github.com/zalando-incubator/postgres-operator/pkg/spec"
	"github.com/zalando-incubator/postgres-operator/pkg/util"
	"github.com/zalando-incubator/postgres-operator/pkg/util/constants"
	"github.com/zalando-incubator/postgres-operator/pkg/util/filesystems"
	"github.com/zalando-incubator/postgres-operator/pkg/util/volumes"
)

func (c *Cluster) listPersistentVolumeClaims() ([]v1.PersistentVolumeClaim, error) {
	ns := c.Namespace
	listOptions := metav1.ListOptions{
		LabelSelector: c.labelsSet().String(),
	}

	pvcs, err := c.KubeClient.PersistentVolumeClaims(ns).List(listOptions)
	if err != nil {
		return nil, fmt.Errorf("could not list of PersistentVolumeClaims: %v", err)
	}
	return pvcs.Items, nil
}

func (c *Cluster) deletePersistenVolumeClaims() error {
	c.logger.Debugln("deleting PVCs")
	pvcs, err := c.listPersistentVolumeClaims()
	if err != nil {
		return err
	}
	for _, pvc := range pvcs {
		c.logger.Debugf("deleting PVC %q", util.NameFromMeta(pvc.ObjectMeta))
		if err := c.KubeClient.PersistentVolumeClaims(pvc.Namespace).Delete(pvc.Name, c.deleteOptions); err != nil {
			c.logger.Warningf("could not delete PersistentVolumeClaim: %v", err)
		}
	}
	if len(pvcs) > 0 {
		c.logger.Debugln("PVCs have been deleted")
	} else {
		c.logger.Debugln("no PVCs to delete")
	}

	return nil
}

func (c *Cluster) listPersistentVolumes() ([]*v1.PersistentVolume, error) {
	result := make([]*v1.PersistentVolume, 0)

	pvcs, err := c.listPersistentVolumeClaims()
	if err != nil {
		return nil, fmt.Errorf("could not list cluster's PersistentVolumeClaims: %v", err)
	}
	lastPodIndex := *c.Statefulset.Spec.Replicas - 1
	for _, pvc := range pvcs {
		lastDash := strings.LastIndex(pvc.Name, "-")
		if lastDash > 0 && lastDash < len(pvc.Name)-1 {
			pvcNumber, err := strconv.Atoi(pvc.Name[lastDash+1:])
			if err != nil {
				return nil, fmt.Errorf("could not convert last part of the persistent volume claim name %q to a number", pvc.Name)
			}
			if int32(pvcNumber) > lastPodIndex {
				c.logger.Debugf("skipping persistent volume %q corresponding to a non-running pods", pvc.Name)
				continue
			}
		}
		pv, err := c.KubeClient.PersistentVolumes().Get(pvc.Spec.VolumeName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("could not get PersistentVolume: %v", err)
		}
		result = append(result, pv)
	}

	return result, nil
}

// resizeVolumes resize persistent volumes compatible with the given resizer interface
func (c *Cluster) resizeVolumes(newVolume spec.Volume, resizers []volumes.VolumeResizer) error {
	c.setProcessName("resizing volumes")

	totalCompatible := 0
	newQuantity, err := resource.ParseQuantity(newVolume.Size)
	if err != nil {
		return fmt.Errorf("could not parse volume size: %v", err)
	}
	pvs, newSize, err := c.listVolumesWithManifestSize(newVolume)
	if err != nil {
		return fmt.Errorf("could not list persistent volumes: %v", err)
	}
	for _, pv := range pvs {
		volumeSize := quantityToGigabyte(pv.Spec.Capacity[v1.ResourceStorage])
		if volumeSize > newSize {
			return fmt.Errorf("cannot shrink persistent volume")
		}
		if volumeSize == newSize {
			continue
		}
		for _, resizer := range resizers {
			if !resizer.VolumeBelongsToProvider(pv) {
				continue
			}
			totalCompatible++
			if !resizer.IsConnectedToProvider() {
				err := resizer.ConnectToProvider()
				if err != nil {
					return fmt.Errorf("could not connect to the volume provider: %v", err)
				}
				defer func() {
					if err := resizer.DisconnectFromProvider(); err != nil {
						c.logger.Errorf("%v", err)
					}
				}()
			}
			awsVolumeID, err := resizer.GetProviderVolumeID(pv)
			if err != nil {
				return err
			}
			c.logger.Debugf("updating persistent volume %q to %d", pv.Name, newSize)
			if err := resizer.ResizeVolume(awsVolumeID, newSize); err != nil {
				return fmt.Errorf("could not resize EBS volume %q: %v", awsVolumeID, err)
			}
			c.logger.Debugf("resizing the filesystem on the volume %q", pv.Name)
			podName := getPodNameFromPersistentVolume(pv)
			if err := c.resizePostgresFilesystem(podName, []filesystems.FilesystemResizer{&filesystems.Ext234Resize{}}); err != nil {
				return fmt.Errorf("could not resize the filesystem on pod %q: %v", podName, err)
			}
			c.logger.Debugf("filesystem resize successful on volume %q", pv.Name)
			pv.Spec.Capacity[v1.ResourceStorage] = newQuantity
			c.logger.Debugf("updating persistent volume definition for volume %q", pv.Name)
			if _, err := c.KubeClient.PersistentVolumes().Update(pv); err != nil {
				return fmt.Errorf("could not update persistent volume: %q", err)
			}
			c.logger.Debugf("successfully updated persistent volume %q", pv.Name)
		}
	}
	if len(pvs) > 0 && totalCompatible == 0 {
		return fmt.Errorf("could not resize EBS volumes: persistent volumes are not compatible with existing resizing providers")
	}
	return nil
}

func (c *Cluster) volumesNeedResizing(newVolume spec.Volume) (bool, error) {
	vols, manifestSize, err := c.listVolumesWithManifestSize(newVolume)
	if err != nil {
		return false, err
	}
	for _, pv := range vols {
		currentSize := quantityToGigabyte(pv.Spec.Capacity[v1.ResourceStorage])
		if currentSize != manifestSize {
			return true, nil
		}
	}
	return false, nil
}

func (c *Cluster) listVolumesWithManifestSize(newVolume spec.Volume) ([]*v1.PersistentVolume, int64, error) {
	newSize, err := resource.ParseQuantity(newVolume.Size)
	if err != nil {
		return nil, 0, fmt.Errorf("could not parse volume size from the manifest: %v", err)
	}
	manifestSize := quantityToGigabyte(newSize)
	vols, err := c.listPersistentVolumes()
	if err != nil {
		return nil, 0, fmt.Errorf("could not list persistent volumes: %v", err)
	}
	return vols, manifestSize, nil
}

// getPodNameFromPersistentVolume returns a pod name that it extracts from the volume claim ref.
func getPodNameFromPersistentVolume(pv *v1.PersistentVolume) *spec.NamespacedName {
	namespace := pv.Spec.ClaimRef.Namespace
	name := pv.Spec.ClaimRef.Name[len(constants.DataVolumeName)+1:]
	return &spec.NamespacedName{Namespace: namespace, Name: name}
}

func quantityToGigabyte(q resource.Quantity) int64 {
	return q.ScaledValue(0) / (1 * constants.Gigabyte)
}
