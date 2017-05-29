package cluster

import (
	"fmt"

	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/api/resource"

	"github.com/zalando-incubator/postgres-operator/pkg/spec"
	"github.com/zalando-incubator/postgres-operator/pkg/util"
	"github.com/zalando-incubator/postgres-operator/pkg/util/constants"
	"github.com/zalando-incubator/postgres-operator/pkg/util/volumes"
	"github.com/zalando-incubator/postgres-operator/pkg/util/filesystems"
)

func (c *Cluster) listPersistentVolumeClaims() ([]v1.PersistentVolumeClaim, error) {
	ns := c.Metadata.Namespace
	listOptions := v1.ListOptions{
		LabelSelector: c.labelsSet().String(),
	}

	pvcs, err := c.KubeClient.PersistentVolumeClaims(ns).List(listOptions)
	if err != nil {
		return nil, fmt.Errorf("could not list of PersistentVolumeClaims: %v", err)
	}
	return pvcs.Items, nil
}

func (c *Cluster) deletePersistenVolumeClaims() error {
	c.logger.Debugln("Deleting PVCs")
	ns := c.Metadata.Namespace
	pvcs, err := c.listPersistentVolumeClaims()
	if err != nil {
		return err
	}
	for _, pvc := range pvcs {
		c.logger.Debugf("Deleting PVC '%s'", util.NameFromMeta(pvc.ObjectMeta))
		if err := c.KubeClient.PersistentVolumeClaims(ns).Delete(pvc.Name, c.deleteOptions); err != nil {
			c.logger.Warningf("could not delete PersistentVolumeClaim: %v", err)
		}
	}
	if len(pvcs) > 0 {
		c.logger.Debugln("PVCs have been deleted")
	} else {
		c.logger.Debugln("No PVCs to delete")
	}

	return nil
}

// ListEC2VolumeIDs returns all EBS volume IDs belong to this cluster
func (c *Cluster) listPersistentVolumes() ([]*v1.PersistentVolume, error) {
	result := make([]*v1.PersistentVolume, 0)

	pvcs, err := c.listPersistentVolumeClaims()
	if err != nil {
		return nil, fmt.Errorf("could not list cluster's PersistentVolumeClaims: %v", err)
	}
	for _, pvc := range pvcs {
		if pvc.Annotations[constants.VolumeClaimStorageProvisionerAnnotation] != constants.EBSProvisioner {
			continue
		}
		pv, err := c.KubeClient.PersistentVolumes().Get(pvc.Spec.VolumeName)
		if err != nil {
			return nil, fmt.Errorf("could not get PersistentVolume: %v", err)
		}
		if pv.Annotations[constants.VolumeStorateProvisionerAnnotation] != constants.EBSProvisioner {
			return nil, fmt.Errorf("mismatched PersistentVolimeClaim and PersistentVolume provisioner annotations for the volume %s", pv.Name)
		}
		result = append(result, pv)
	}

	return result, nil
}

// resizeVolumes resize persistent volumes compatible with the given resizer interface
func (c *Cluster) resizeVolumes(newVolume spec.Volume, resizers []volumes.VolumeResizer) error {
	totalCompatible := 0
	newQuantity, err := resource.ParseQuantity(newVolume.Size)
	if err != nil {
		return fmt.Errorf("could not parse volume size: %v", err)
	}
	pvs, newSize, err := c.listVolumesWitManifestSize(newVolume)
	if err != nil {
		return fmt.Errorf("could not list persistent volumes: %v", err)
	}
	for _, pv := range pvs {
		for _, resizer := range resizers {
			if !resizer.VolumeBelongsToProvider(pv) {
				continue
			}
			totalCompatible += 1
			if !resizer.IsConnectedToProvider() {
				err := resizer.ConnectToProvider()
				if err != nil {
					return fmt.Errorf("could not connect to the volume provider: %v", err)
				}
				defer resizer.DisconnectFromProvider()
			}
			volumeSize := quantityToGigabyte(pv.Spec.Capacity[v1.ResourceStorage])
			if volumeSize > newSize {
				return fmt.Errorf("cannot shrink persistent volume")
			}
			if volumeSize != newSize {
				awsVolumeId, err := resizer.GetProviderVolumeID(pv)
				if err != nil {
					return err
				}
				c.logger.Debugf("updating persistent volume %s to %d", pv.Name, newSize)
				if err := resizer.ResizeVolume(awsVolumeId, newSize); err != nil {
					return fmt.Errorf("could not resize EBS volume %s: %v", awsVolumeId, err)
				}
				c.logger.Debugf("resizing the filesystem on the volume %s", pv.Name)
				podName := getPodNameFromPersistentVolume(pv)
				if err := c.resizePostgresFilesystem(podName, []filesystems.FilesystemResizer{&filesystems.Ext234Resize{}}); err != nil {
					return fmt.Errorf("could not resize the filesystem on pod '%s': %v", podName, err)
				}
				c.logger.Debugf("filesystem resize successfull on volume %s", pv.Name)
				pv.Spec.Capacity[v1.ResourceStorage] = newQuantity
				c.logger.Debugf("updating persistent volume definition for volume %s", pv.Name)
				if _, err := c.KubeClient.PersistentVolumes().Update(pv); err != nil {
					return fmt.Errorf("could not update persistent volume: %s", err)
				}
				c.logger.Debugf("successfully updated persistent volume %s", pv.Name)
			}
		}
	}
	if len(pvs) > 0 && totalCompatible == 0 {
		return fmt.Errorf("could not resize EBS volumes: persistent volumes are not compatible with existing resizing providers")
	}
	return nil
}

func (c *Cluster) VolumesNeedResizing(newVolume spec.Volume) (bool, error){
	volumes, manifestSize, err := c.listVolumesWitManifestSize(newVolume)
	if err != nil {
		return false, err
	}
	for _, pv := range(volumes) {
		currentSize := quantityToGigabyte(pv.Spec.Capacity[v1.ResourceStorage])
		if currentSize != manifestSize {
			return true, nil
		}
	}
	return false, nil
}

func (c *Cluster) listVolumesWitManifestSize(newVolume spec.Volume) ([]*v1.PersistentVolume, int64, error) {
	newSize, err := resource.ParseQuantity(newVolume.Size)
	if err != nil {
		return nil, 0, fmt.Errorf("could not parse volume size from the manifest: %v", err)
	}
	manifestSize := quantityToGigabyte(newSize)
	volumes, err := c.listPersistentVolumes()
	if err != nil {
		return nil, 0, fmt.Errorf("could not list persistent volumes: %v", err)
	}
	return volumes, manifestSize, nil
}

func getPodNameFromPersistentVolume(pv *v1.PersistentVolume) *spec.NamespacedName {
	namespace := pv.Spec.ClaimRef.Namespace
	name := pv.Spec.ClaimRef.Name[len(constants.DataVolumeName)+1:]
	return &spec.NamespacedName{namespace, name}
}

func quantityToGigabyte(q resource.Quantity) int64 {
	return q.ScaledValue(0) / (1 * constants.Gigabyte)
}