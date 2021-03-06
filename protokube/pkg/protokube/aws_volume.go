/*
Copyright 2016 The Kubernetes Authors.

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

package protokube

import (
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/golang/glog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The tag name we use to differentiate multiple logically independent clusters running in the same region
const TagNameKubernetesCluster = "KubernetesCluster"

// The tag name we use for specifying that something is in the master role
const TagNameRoleMaster = "k8s.io/role/master"

const TagNameEtcdClusterPrefix = "k8s.io/etcd/"

const TagNameMasterId = "k8s.io/master/id"

//const DefaultAttachDevice = "/dev/xvdb"

var devices = []string{"/dev/xvdu", "/dev/xvdv", "/dev/xvdx", "/dev/xvdx", "/dev/xvdy", "/dev/xvdz"}

type AWSVolumes struct {
	ec2      *ec2.EC2
	metadata *ec2metadata.EC2Metadata

	zone       string
	clusterTag string
	instanceId string
	internalIP net.IP

	mutex     sync.Mutex
	deviceMap map[string]string
}

var _ Volumes = &AWSVolumes{}

func NewAWSVolumes() (*AWSVolumes, error) {
	a := &AWSVolumes{
		deviceMap: make(map[string]string),
	}

	s := session.New()
	s.Handlers.Send.PushFront(func(r *request.Request) {
		// Log requests
		glog.V(4).Infof("AWS API Request: %s/%s", r.ClientInfo.ServiceName, r.Operation.Name)
	})

	config := aws.NewConfig()
	a.metadata = ec2metadata.New(s, config)

	region, err := a.metadata.Region()
	if err != nil {
		return nil, fmt.Errorf("error querying ec2 metadata service (for az/region): %v", err)
	}

	a.zone, err = a.metadata.GetMetadata("placement/availability-zone")
	if err != nil {
		return nil, fmt.Errorf("error querying ec2 metadata service (for az): %v", err)
	}

	a.instanceId, err = a.metadata.GetMetadata("instance-id")
	if err != nil {
		return nil, fmt.Errorf("error querying ec2 metadata service (for instance-id): %v", err)
	}

	a.ec2 = ec2.New(s, config.WithRegion(region))

	err = a.discoverTags()
	if err != nil {
		return nil, err
	}

	return a, nil
}

func (a *AWSVolumes) ClusterID() string {
	return a.clusterTag
}

func (a *AWSVolumes) InternalIP() net.IP {
	return a.internalIP
}

func (a *AWSVolumes) discoverTags() error {
	instance, err := a.describeInstance()
	if err != nil {
		return err
	}

	tagMap := make(map[string]string)
	for _, tag := range instance.Tags {
		tagMap[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}

	clusterID := tagMap[TagNameKubernetesCluster]
	if clusterID == "" {
		return fmt.Errorf("Cluster tag %q not found on this instance (%q)", TagNameKubernetesCluster, a.instanceId)
	}

	a.clusterTag = clusterID

	a.internalIP = net.ParseIP(aws.StringValue(instance.PrivateIpAddress))
	if a.internalIP == nil {
		return fmt.Errorf("Internal IP not found on this instance (%q)", a.instanceId)
	}

	return nil
}

func (a *AWSVolumes) describeInstance() (*ec2.Instance, error) {
	request := &ec2.DescribeInstancesInput{}
	request.InstanceIds = []*string{&a.instanceId}

	var instances []*ec2.Instance
	err := a.ec2.DescribeInstancesPages(request, func(p *ec2.DescribeInstancesOutput, lastPage bool) (shouldContinue bool) {
		for _, r := range p.Reservations {
			instances = append(instances, r.Instances...)
		}
		return true
	})

	if err != nil {
		return nil, fmt.Errorf("error querying for EC2 instance %q: %v", a.instanceId, err)
	}

	if len(instances) != 1 {
		return nil, fmt.Errorf("unexpected number of instances found with id %q: %d", a.instanceId, len(instances))
	}

	return instances[0], nil
}

func newEc2Filter(name string, value string) *ec2.Filter {
	filter := &ec2.Filter{
		Name: aws.String(name),
		Values: []*string{
			aws.String(value),
		},
	}
	return filter
}

func (a *AWSVolumes) findVolumes(request *ec2.DescribeVolumesInput) ([]*Volume, error) {
	var volumes []*Volume
	err := a.ec2.DescribeVolumesPages(request, func(p *ec2.DescribeVolumesOutput, lastPage bool) (shouldContinue bool) {
		for _, v := range p.Volumes {
			volumeID := aws.StringValue(v.VolumeId)
			vol := &Volume{
				ID: volumeID,
				Info: VolumeInfo{
					Description: volumeID,
				},
			}
			state := aws.StringValue(v.State)

			vol.Status = state

			for _, attachment := range v.Attachments {
				vol.AttachedTo = aws.StringValue(attachment.InstanceId)
				if aws.StringValue(attachment.InstanceId) == a.instanceId {
					vol.LocalDevice = aws.StringValue(attachment.Device)
				}
			}

			skipVolume := false

			for _, tag := range v.Tags {
				k := aws.StringValue(tag.Key)
				v := aws.StringValue(tag.Value)

				switch k {
				case TagNameKubernetesCluster, TagNameRoleMaster, "Name":
				// Ignore
				case TagNameMasterId:
					id, err := strconv.Atoi(v)
					if err != nil {
						glog.Warningf("error parsing master-id tag on volume %q %s=%s; skipping volume", volumeID, k, v)
						skipVolume = true
					} else {
						vol.Info.MasterID = id
					}
				default:
					if strings.HasPrefix(k, TagNameEtcdClusterPrefix) {
						etcdClusterName := k[len(TagNameEtcdClusterPrefix):]
						spec, err := ParseEtcdClusterSpec(etcdClusterName, v)
						if err != nil {
							// Fail safe
							glog.Warningf("error parsing etcd cluster tag %q on volume %q; skipping volume: %v", v, volumeID, err)
							skipVolume = true
						}
						vol.Info.EtcdClusters = append(vol.Info.EtcdClusters, spec)
					} else {
						glog.Warningf("unknown tag on volume %q: %s=%s", volumeID, k, v)
					}
				}
			}

			if !skipVolume {
				volumes = append(volumes, vol)
			}
		}
		return true
	})

	if err != nil {
		return nil, fmt.Errorf("error querying for EC2 volumes: %v", err)
	}
	return volumes, nil
}

//func (a *AWSVolumes) FindMountedVolumes() ([]*Volume, error) {
//	request := &ec2.DescribeVolumesInput{}
//	request.Filters = []*ec2.Filter{
//		newEc2Filter("tag:"+TagNameKubernetesCluster, a.clusterTag),
//		newEc2Filter("tag-key", TagNameRoleMaster),
//		newEc2Filter("attachment.instance-id", a.instanceId),
//	}
//
//	return a.findVolumes(request)
//}
//
//func (a *AWSVolumes) FindMountableVolumes() ([]*Volume, error) {
//	request := &ec2.DescribeVolumesInput{}
//	request.Filters = []*ec2.Filter{
//		newEc2Filter("tag:"+TagNameKubernetesCluster, a.clusterTag),
//		newEc2Filter("tag-key", TagNameRoleMaster),
//		newEc2Filter("availability-zone", a.zone),
//	}
//
//	return a.findVolumes(request)
//}

func (a *AWSVolumes) FindVolumes() ([]*Volume, error) {
	request := &ec2.DescribeVolumesInput{}
	request.Filters = []*ec2.Filter{
		newEc2Filter("tag:"+TagNameKubernetesCluster, a.clusterTag),
		newEc2Filter("tag-key", TagNameRoleMaster),
		newEc2Filter("availability-zone", a.zone),
	}

	return a.findVolumes(request)
}

// assignDevice picks a hopefully unused device and reserves it for the volume attachment
func (a *AWSVolumes) assignDevice(volumeID string) (string, error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	// TODO: Check for actual devices in use (like cloudprovider does)
	for _, d := range devices {
		if a.deviceMap[d] == "" {
			a.deviceMap[d] = volumeID
			return d, nil
		}
	}
	return "", fmt.Errorf("All devices in use")
}

// releaseDevice releases the volume mapping lock; used when an attach was known to fail
func (a *AWSVolumes) releaseDevice(d string, volumeID string) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if a.deviceMap[d] != volumeID {
		glog.Fatalf("deviceMap logic error: %q -> %q, not %q", d, a.deviceMap[d], volumeID)
	}
	a.deviceMap[d] = ""
}

// AttachVolume attaches the specified volume to this instance, returning the mountpoint & nil if successful
func (a *AWSVolumes) AttachVolume(volume *Volume) error {
	volumeID := volume.ID

	device := volume.LocalDevice
	if device == "" {
		d, err := a.assignDevice(volumeID)
		if err != nil {
			return err
		}
		device = d

		request := &ec2.AttachVolumeInput{
			Device:     aws.String(device),
			InstanceId: aws.String(a.instanceId),
			VolumeId:   aws.String(volumeID),
		}

		attachResponse, err := a.ec2.AttachVolume(request)
		if err != nil {
			return fmt.Errorf("Error attaching EBS volume %q: %v", volumeID, err)
		}

		glog.V(2).Infof("AttachVolume request returned %v", attachResponse)
	}

	// Wait (forever) for volume to attach or reach a failure-to-attach condition
	for {
		request := &ec2.DescribeVolumesInput{
			VolumeIds: []*string{&volumeID},
		}

		volumes, err := a.findVolumes(request)
		if err != nil {
			return fmt.Errorf("Error describing EBS volume %q: %v", volumeID, err)
		}

		if len(volumes) == 0 {
			return fmt.Errorf("EBS volume %q disappeared during attach", volumeID)
		}
		if len(volumes) != 1 {
			return fmt.Errorf("Multiple volumes found with id %q", volumeID)
		}

		v := volumes[0]
		if v.AttachedTo != "" {
			if v.AttachedTo == a.instanceId {
				volume.LocalDevice = device
				return nil
			} else {
				a.releaseDevice(device, volumeID)

				return fmt.Errorf("Unable to attach volume %q, was attached to %q", volumeID, v.AttachedTo)
			}
		}

		switch v.Status {
		case "attaching":
			glog.V(2).Infof("Waiting for volume %q to be attached (currently %q)", volumeID, v.Status)
		// continue looping

		default:
			return fmt.Errorf("Observed unexpected volume state %q", v.Status)
		}

		time.Sleep(10 * time.Second)
	}
}
