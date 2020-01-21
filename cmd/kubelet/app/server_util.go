package app

import (
	"context"
	"fmt"

	
	"k8s.io/klog"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/apimachinery/pkg/types"

)

// getNodeName returns the node name according to the cloud provider
// if cloud provider is specified. Otherwise, returns the hostname of the node.
func getNodeName(cloud cloudprovider.Interface, hostname string) (types.NodeName, error) {
	if cloud == nil {
		return types.NodeName(hostname), nil
	}

	instances, ok := cloud.Instances()
	if !ok {
		return "", fmt.Errorf("failed to get instances from cloud provider")
	}

	nodeName, err := instances.CurrentNodeName(context.TODO(), hostname)
	if err != nil {
		return "", fmt.Errorf("error fetching current node name from cloud provider: %v", err)
	}

	klog.V(2).Infof("cloud provider determined current node name to be %s", nodeName)

	return nodeName, nil
}
