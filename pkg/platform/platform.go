package platform

import (
	"context"
	"fmt"
	"k8s.io/utils/pointer"
	"os"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/cluster-network-operator/pkg/bootstrap"
	cnoclient "github.com/openshift/cluster-network-operator/pkg/client"
	"github.com/openshift/cluster-network-operator/pkg/names"
	hyperv1 "github.com/openshift/hypershift/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

var cloudProviderConfig = types.NamespacedName{
	Namespace: "openshift-config-managed",
	Name:      "kube-cloud-config",
}

// isNetworkNodeIdentityEnabled determines if network node identity should be enabled.
// It checks the `enabled` key in the network-node-identity/openshift-network-operator configmap.
// If the configmap doesn't exist, it returns true (the feature is enabled by default).
func isNetworkNodeIdentityEnabled(client cnoclient.Client) (bool, error) {
	nodeIdentity := &corev1.ConfigMap{}
	nodeIdentityLookup := types.NamespacedName{Name: "network-node-identity", Namespace: names.APPLIED_NAMESPACE}
	if err := client.ClientFor("").CRClient().Get(context.TODO(), nodeIdentityLookup, nodeIdentity); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("unable to bootstrap OVN, unable to retrieve cluster config: %s", err)
	}
	enabled, ok := nodeIdentity.Data["enabled"]
	if ok {
		return enabled == "true", nil
	}
	klog.Warningf("key `enabled` not found in the network-node-identity configmap, defaulting to enabled")
	return true, nil
}

func InfraStatus(client cnoclient.Client) (*bootstrap.InfraStatus, error) {
	infraConfig := &configv1.Infrastructure{}
	if err := client.Default().CRClient().Get(context.TODO(), types.NamespacedName{Name: "cluster"}, infraConfig); err != nil {
		return nil, fmt.Errorf("failed to get infrastructure 'cluster': %v", err)
	}

	res := &bootstrap.InfraStatus{
		PlatformType:           infraConfig.Status.PlatformStatus.Type,
		PlatformStatus:         infraConfig.Status.PlatformStatus,
		ControlPlaneTopology:   infraConfig.Status.ControlPlaneTopology,
		InfraName:              infraConfig.Status.InfrastructureName,
		InfrastructureTopology: infraConfig.Status.InfrastructureTopology,
		APIServers:             map[string]bootstrap.APIServer{},
	}

	proxy := &configv1.Proxy{}
	if err := client.Default().CRClient().Get(context.TODO(), types.NamespacedName{Name: "cluster"}, proxy); err != nil {
		return nil, fmt.Errorf("failed to get proxy 'cluster': %w", err)
	}
	res.Proxy = proxy.Status

	// Extract apiserver URLs from the kubeconfig(s) passed to the CNO
	for name, c := range client.Clients() {
		h, p := c.HostPort()
		res.APIServers[name] = bootstrap.APIServer{
			Host: h,
			Port: p,
		}
	}

	// default-local defines how the CNO connects to the APIServer. So, just copy from Default
	res.APIServers[bootstrap.APIServerDefaultLocal] = res.APIServers[bootstrap.APIServerDefault]

	// Allow overriding the "default" apiserver via the environment var APISERVER_OVERRIDE_HOST / _PORT
	// This is used by Hypershift, since the cno connects to a "local" ServiceIP, but rendered manifests
	// that run on a hosted cluster need to talk to the external URL
	if h := os.Getenv(names.EnvApiOverrideHost); h != "" {
		p := os.Getenv(names.EnvApiOverridePort)
		if p == "" {
			p = "443"
		}

		res.APIServers[bootstrap.APIServerDefault] = bootstrap.APIServer{
			Host: h,
			Port: p,
		}
	}

	if res.PlatformType == configv1.AWSPlatformType {
		res.PlatformRegion = infraConfig.Status.PlatformStatus.AWS.Region
	} else if res.PlatformType == configv1.GCPPlatformType {
		res.PlatformRegion = infraConfig.Status.PlatformStatus.GCP.Region
	}

	// AWS and OpenStack specify a CA bundle via a config map; retrieve it.
	if res.PlatformType == configv1.AWSPlatformType || res.PlatformType == configv1.OpenStackPlatformType {
		cm := &corev1.ConfigMap{}
		if err := client.Default().CRClient().Get(context.TODO(), cloudProviderConfig, cm); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("failed to retrieve ConfigMap %s: %w", cloudProviderConfig, err)
			}
		} else {
			res.KubeCloudConfig = cm.Data
		}
	}

	if hc := NewHyperShiftConfig(); hc.Enabled {
		hcp := &hyperv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{Name: hc.Name}}
		err := client.ClientFor(names.ManagementClusterName).CRClient().Get(context.TODO(), types.NamespacedName{Namespace: hc.Namespace, Name: hc.Name}, hcp)
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve HostedControlPlane %s: %v", types.NamespacedName{Namespace: hc.Namespace, Name: hc.Name}, err)
		}
		res.HostedControlPlane = hcp
		if res.HostedControlPlane.Spec.Networking.APIServer == nil {
			res.HostedControlPlane.Spec.Networking.APIServer = &hyperv1.APIServerNetworking{
				AdvertiseAddress: pointer.String("172.20.0.1"),
				Port:             pointer.Int32(6443),
			}
		}
		if res.HostedControlPlane.Spec.Networking.APIServer.AdvertiseAddress == nil {
			res.HostedControlPlane.Spec.Networking.APIServer.AdvertiseAddress = pointer.String("172.20.0.1")
		}
		if res.HostedControlPlane.Spec.Networking.APIServer.Port == nil {
			res.HostedControlPlane.Spec.Networking.APIServer.Port = pointer.Int32(6443)
		}
	}

	netIDEnabled, err := isNetworkNodeIdentityEnabled(client)
	if err != nil {
		return nil, fmt.Errorf("failed to determine if network node identity should be enabled: %w", err)
	}
	res.NetworkNodeIdentityEnabled = netIDEnabled

	return res, nil
}
