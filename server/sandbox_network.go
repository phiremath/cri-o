package server

import (
	"context"
	"fmt"
	"math"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	cnicurrent "github.com/containernetworking/cni/pkg/types/current"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	"github.com/cri-o/cri-o/internal/log"
	"github.com/cri-o/cri-o/server/metrics"
	"github.com/cri-o/ocicni/pkg/ocicni"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/api/resource"
)

// networkStart sets up the sandbox's network and returns the pod IP on success
// or an error
func (s *Server) networkStart(ctx context.Context, sb *sandbox.Sandbox) (podIPs []string, result cnitypes.Result, retErr error) {
	overallStart := time.Now()
	// Give a network Start call a full 5 minutes, independent of the context of the request.
	// This is to prevent the CNI plugin from taking an unbounded amount of time,
	// but to still allow a long-running sandbox creation to be cached and reused,
	// rather than failing and recreating it.
	const startTimeout = 5 * time.Minute
	startCtx, startCancel := context.WithTimeout(context.Background(), startTimeout)
	defer startCancel()

	if sb.HostNetwork() {
		return nil, nil, nil
	}

	podNetwork, err := s.newPodNetwork(sb)
	if err != nil {
		return nil, nil, err
	}

	// Ensure network resources are cleaned up if the plugin succeeded
	// but an error happened between plugin success and the end of networkStart()
	defer func() {
		if retErr != nil {
			log.Infof(ctx, "NetworkStart: stopping network for sandbox %s", sb.ID())
			// use a new context to prevent an expired context from preventing a stop
			if err2 := s.networkStop(context.Background(), sb); err2 != nil {
				log.Errorf(ctx, "Error stopping network on cleanup: %v", err2)
			}
		}
	}()

	podSetUpStart := time.Now()
	_, err = s.config.CNIPlugin().SetUpPodWithContext(startCtx, podNetwork)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create pod network sandbox %s(%s): %v", sb.Name(), sb.ID(), err)
	}
	// metric about the CNI network setup operation
	metrics.Instance().MetricOperationsLatencySet("network_setup_pod", podSetUpStart)

	podNetworkStatus, err := s.config.CNIPlugin().GetPodNetworkStatusWithContext(startCtx, podNetwork)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get network status for pod sandbox %s(%s): %v", sb.Name(), sb.ID(), err)
	}

	// only one cnitypes.Result is returned since newPodNetwork sets Networks list empty
	result = podNetworkStatus[0].Result
	log.Debugf(ctx, "CNI setup result: %v", result)

	network, err := cnicurrent.GetResult(result)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get network JSON for pod sandbox %s(%s): %v", sb.Name(), sb.ID(), err)
	}

	// iterate over each IP and add the portmap if needed
	for _, podIPConfig := range network.IPs {
		ip := podIPConfig.Address.IP
		podIPs = append(podIPs, ip.String())
		log.Infof(ctx, "Skipped use of hostport manager add...")
	}
	log.Debugf(ctx, "Found POD IPs: %v", podIPs)

	// metric about the whole network setup operation
	metrics.Instance().MetricOperationsLatencySet("network_setup_overall", overallStart)
	return podIPs, result, err
}

// getSandboxIP retrieves the IP address for the sandbox
func (s *Server) getSandboxIPs(sb *sandbox.Sandbox) ([]string, error) {
	if sb.HostNetwork() {
		return nil, nil
	}

	podNetwork, err := s.newPodNetwork(sb)
	if err != nil {
		return nil, err
	}
	podNetworkStatus, err := s.config.CNIPlugin().GetPodNetworkStatus(podNetwork)
	if err != nil {
		return nil, fmt.Errorf("failed to get network status for pod sandbox %s(%s): %v", sb.Name(), sb.ID(), err)
	}

	res, err := cnicurrent.GetResult(podNetworkStatus[0].Result)
	if err != nil {
		return nil, fmt.Errorf("failed to get network JSON for pod sandbox %s(%s): %v", sb.Name(), sb.ID(), err)
	}

	podIPs := make([]string, 0, len(res.IPs))
	for _, podIPConfig := range res.IPs {
		podIPs = append(podIPs, podIPConfig.Address.IP.String())
	}

	return podIPs, nil
}

// networkStop cleans up and removes a pod's network.  It is best-effort and
// must call the network plugin even if the network namespace is already gone
func (s *Server) networkStop(ctx context.Context, sb *sandbox.Sandbox) error {
	if sb.HostNetwork() || sb.NetworkStopped() {
		return nil
	}
	// give a network stop call 1 minutes, half of a StopPod request timeout limit
	stopCtx, stopCancel := context.WithTimeout(ctx, 1*time.Minute)
	defer stopCancel()

	log.Infof(ctx, "Skipped use of hostport manager remove...")

	podNetwork, err := s.newPodNetwork(sb)
	if err != nil {
		return err
	}
	if err := s.config.CNIPlugin().TearDownPodWithContext(stopCtx, podNetwork); err != nil {
		return errors.Wrapf(err, "failed to destroy network for pod sandbox %s(%s)", sb.Name(), sb.ID())
	}

	return sb.SetNetworkStopped(true)
}

func (s *Server) newPodNetwork(sb *sandbox.Sandbox) (ocicni.PodNetwork, error) {
	var egress, ingress int64

	if val, ok := sb.Annotations()["kubernetes.io/egress-bandwidth"]; ok {
		egressQ, err := resource.ParseQuantity(val)
		if err != nil {
			return ocicni.PodNetwork{}, fmt.Errorf("failed to parse egress bandwidth: %v", err)
		} else if iegress, isok := egressQ.AsInt64(); isok {
			egress = iegress
		}
	}
	if val, ok := sb.Annotations()["kubernetes.io/ingress-bandwidth"]; ok {
		ingressQ, err := resource.ParseQuantity(val)
		if err != nil {
			return ocicni.PodNetwork{}, fmt.Errorf("failed to parse ingress bandwidth: %v", err)
		} else if iingress, isok := ingressQ.AsInt64(); isok {
			ingress = iingress
		}
	}

	var bwConfig *ocicni.BandwidthConfig

	if ingress > 0 || egress > 0 {
		bwConfig = &ocicni.BandwidthConfig{}
		if ingress > 0 {
			bwConfig.IngressRate = uint64(ingress)
			bwConfig.IngressBurst = math.MaxUint32*8 - 1 // 4GB burst limit
		}
		if egress > 0 {
			bwConfig.EgressRate = uint64(egress)
			bwConfig.EgressBurst = math.MaxUint32*8 - 1 // 4GB burst limit
		}
	}

	// Pass along sandbox port mapping info.
	var portMappings []ocicni.PortMapping //nolint
	sbPortMappings := sb.PortMappings()
	for _, pm := range sbPortMappings {
		if pm.ContainerPort == 0 {
			continue
		}
		portMap := ocicni.PortMapping{
			HostPort:      pm.HostPort,
			ContainerPort: pm.ContainerPort,
			Protocol:      string(pm.Protocol),
		}
		portMappings = append(portMappings, portMap)
	}

	network := s.config.CNIPlugin().GetDefaultNetworkName()
	return ocicni.PodNetwork{
		Name:      sb.KubeName(),
		Namespace: sb.Namespace(),
		UID:       sb.Metadata().UID,
		Networks:  []ocicni.NetAttachment{},
		ID:        sb.ID(),
		NetNS:     sb.NetNsPath(),
		RuntimeConfig: map[string]ocicni.RuntimeConfig{
			network: {Bandwidth: bwConfig, PortMappings: portMappings},
		},
	}, nil
}
