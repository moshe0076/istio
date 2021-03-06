// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v1alpha3

import (
	"github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/core"

	"github.com/envoyproxy/go-control-plane/envoy/api/v2/auth"
	v2_cluster "github.com/envoyproxy/go-control-plane/envoy/api/v2/cluster"
	"github.com/gogo/protobuf/types"

	"time"

	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/plugins/authn"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pkg/log"
)

const (
	// DefaultLbType set to round robin
	DefaultLbType = networking.LoadBalancerSettings_ROUND_ROBIN
	// ManagementClusterHostname indicates the hostname used for building inbound clusters for management ports
	ManagementClusterHostname = "mgmtCluster"

	// CDSv2 validation requires ConnectTimeout to be > 0s. This is applied if no explicit policy is set.
	defaultClusterConnectTimeout = 5 * time.Second

	// Name used for the xds cluster.
	xdsName = "xds-grpc"
)

// TODO: Need to do inheritance of DestRules based on domain suffix match

// BuildClusters returns the list of clusters for the given proxy. This is the CDS output
// For outbound: Cluster for each service/subset hostname or cidr with SNI set to service hostname
// Cluster type based on resolution
// For inbound (sidecar only): Cluster for each inbound endpoint port and for each service port
func BuildClusters(env model.Environment, proxy model.Proxy) []*v2.Cluster {
	clusters := make([]*v2.Cluster, 0)

	services, err := env.Services()
	if err != nil {
		log.Errorf("Failed for retrieve services: %v", err)
		return nil
	}

	clusters = append(clusters, buildOutboundClusters(env, services)...)
	for _, c := range clusters {
		// Envoy requires a non-zero connect timeout
		if c.ConnectTimeout == 0 {
			c.ConnectTimeout = defaultClusterConnectTimeout
		}
	}
	if proxy.Type == model.Sidecar {
		instances, err := env.GetProxyServiceInstances(proxy)
		if err != nil {
			log.Errorf("failed to get service proxy service instances: %v", err)
			return nil
		}

		managementPorts := env.ManagementPorts(proxy.IPAddress)
		clusters = append(clusters, buildInboundClusters(env, instances, managementPorts)...)

		// TODO: Bug? why only for sidecars?
		// append cluster for JwksUri (for Jwt authentication) if necessary.
		clusters = append(clusters, authn.BuildJwksURIClustersForProxyInstances(
			env.Mesh, env.IstioConfigStore, instances)...)
	}

	return clusters // TODO: normalize/dedup/order
}

func buildOutboundClusters(env model.Environment, services []*model.Service) []*v2.Cluster {
	clusters := make([]*v2.Cluster, 0)
	for _, service := range services {
		config := env.DestinationRule(service.Hostname, "")
		for _, port := range service.Ports {
			hosts := buildClusterHosts(env, service, port)

			// create default cluster
			clusterName := model.BuildSubsetKey(model.TrafficDirectionOutbound, "", service.Hostname, port)
			defaultCluster := buildDefaultCluster(env, clusterName, convertResolution(service.Resolution), hosts)
			updateEds(env, defaultCluster, service.Hostname)
			setUpstreamProtocol(defaultCluster, port)
			clusters = append(clusters, defaultCluster)

			if config != nil {
				destinationRule := config.Spec.(*networking.DestinationRule)
				applyTrafficPolicy(defaultCluster, destinationRule.TrafficPolicy)

				for _, subset := range destinationRule.Subsets {
					subsetClusterName := model.BuildSubsetKey(model.TrafficDirectionOutbound, subset.Name, service.Hostname, port)
					subsetCluster := buildDefaultCluster(env, subsetClusterName, convertResolution(service.Resolution), hosts)
					updateEds(env, subsetCluster, service.Hostname)
					setUpstreamProtocol(subsetCluster, port)
					applyTrafficPolicy(subsetCluster, destinationRule.TrafficPolicy)
					applyTrafficPolicy(subsetCluster, subset.TrafficPolicy)
					clusters = append(clusters, subsetCluster)
				}
			}
		}
	}

	return clusters
}

func updateEds(env model.Environment, cluster *v2.Cluster, serviceName string) {
	if cluster.Type != v2.Cluster_EDS {
		return
	}
	refresh := time.Duration(env.Mesh.RdsRefreshDelay.Seconds) * time.Second
	if refresh == 0 {
		// envoy crashes if 0. Will go away once we move to v2
		refresh = 5 * time.Second
	}
	cluster.EdsClusterConfig = &v2.Cluster_EdsClusterConfig{
		ServiceName: cluster.Name,
		EdsConfig: &core.ConfigSource{
			ConfigSourceSpecifier: &core.ConfigSource_ApiConfigSource{
				ApiConfigSource: &core.ApiConfigSource{
					ApiType:      core.ApiConfigSource_GRPC,
					ClusterNames: []string{xdsName},
					RefreshDelay: &refresh,
				},
			},
		},
	}
}

func buildClusterHosts(env model.Environment, service *model.Service, port *model.Port) []*core.Address {
	if service.Resolution != model.DNSLB {
		return nil
	}

	// FIXME port name not required if only one port
	instances, err := env.Instances(service.Hostname, []string{port.Name}, nil)
	if err != nil {
		log.Errorf("failed to retrieve instances for %s: %v", service.Hostname, err)
		return nil
	}

	hosts := make([]*core.Address, 0)
	for _, instance := range instances {
		host := util.BuildAddress(instance.Endpoint.Address, uint32(instance.Endpoint.Port))
		hosts = append(hosts, &host)
	}

	return hosts
}

func buildInboundClusters(env model.Environment, instances []*model.ServiceInstance, managementPorts []*model.Port) []*v2.Cluster {
	clusters := make([]*v2.Cluster, 0)
	for _, instance := range instances {
		// This cluster name is mainly for stats.
		clusterName := model.BuildSubsetKey(model.TrafficDirectionInbound, "", instance.Service.Hostname, instance.Endpoint.ServicePort)
		address := util.BuildAddress("127.0.0.1", uint32(instance.Endpoint.Port))
		localCluster := buildDefaultCluster(env, clusterName, v2.Cluster_STATIC, []*core.Address{&address})
		setUpstreamProtocol(localCluster, instance.Endpoint.ServicePort)
		clusters = append(clusters, localCluster)
	}

	// Add a passthrough cluster for traffic to management ports (health check ports)
	for _, port := range managementPorts {
		clusterName := model.BuildSubsetKey(model.TrafficDirectionInbound, "", ManagementClusterHostname, port)
		address := util.BuildAddress("127.0.0.1", uint32(port.Port))
		mgmtCluster := buildDefaultCluster(env, clusterName, v2.Cluster_STATIC, []*core.Address{&address})
		setUpstreamProtocol(mgmtCluster, port)
		clusters = append(clusters, mgmtCluster)
	}
	return clusters
}

func convertResolution(resolution model.Resolution) v2.Cluster_DiscoveryType {
	switch resolution {
	case model.ClientSideLB:
		return v2.Cluster_EDS
	case model.DNSLB:
		return v2.Cluster_STRICT_DNS
	case model.Passthrough:
		return v2.Cluster_ORIGINAL_DST
	default:
		return v2.Cluster_EDS
	}
}

func applyTrafficPolicy(cluster *v2.Cluster, policy *networking.TrafficPolicy) {
	if policy == nil {
		return
	}
	applyConnectionPool(cluster, policy.ConnectionPool)
	applyOutlierDetection(cluster, policy.OutlierDetection)
	applyLoadBalancer(cluster, policy.LoadBalancer)
	applyUpstreamTLSSettings(cluster, policy.Tls)
}

// FIXME: there isn't a way to distinguish between unset values and zero values
func applyConnectionPool(cluster *v2.Cluster, settings *networking.ConnectionPoolSettings) {
	if settings == nil {
		return
	}

	threshold := &v2_cluster.CircuitBreakers_Thresholds{}

	if settings.Http != nil {
		if settings.Http.Http2MaxRequests > 0 {
			// Envoy only applies MaxRequests in HTTP/2 clusters
			threshold.MaxRequests = &types.UInt32Value{Value: uint32(settings.Http.Http2MaxRequests)}
		}
		if settings.Http.Http1MaxPendingRequests > 0 {
			// Envoy only applies MaxPendingRequests in HTTP/1.1 clusters
			threshold.MaxPendingRequests = &types.UInt32Value{Value: uint32(settings.Http.Http1MaxPendingRequests)}
		}

		if settings.Http.MaxRequestsPerConnection > 0 {
			cluster.MaxRequestsPerConnection = &types.UInt32Value{Value: uint32(settings.Http.MaxRequestsPerConnection)}
		}

		// FIXME: zero is a valid value if explicitly set, otherwise we want to use the default value of 3
		if settings.Http.MaxRetries > 0 {
			threshold.MaxRetries = &types.UInt32Value{Value: uint32(settings.Http.MaxRetries)}
		}
	}

	if settings.Tcp != nil {
		if settings.Tcp.ConnectTimeout != nil {
			cluster.ConnectTimeout = util.ConvertGogoDurationToDuration(settings.Tcp.ConnectTimeout)
		}

		if settings.Tcp.MaxConnections > 0 {
			threshold.MaxConnections = &types.UInt32Value{Value: uint32(settings.Tcp.MaxConnections)}
		}
	}

	cluster.CircuitBreakers = &v2_cluster.CircuitBreakers{
		Thresholds: []*v2_cluster.CircuitBreakers_Thresholds{threshold},
	}
}

// FIXME: there isn't a way to distinguish between unset values and zero values
func applyOutlierDetection(cluster *v2.Cluster, outlier *networking.OutlierDetection) {
	if outlier == nil || outlier.Http == nil {
		return
	}

	out := &v2_cluster.OutlierDetection{}
	if outlier.Http.BaseEjectionTime != nil {
		out.BaseEjectionTime = outlier.Http.BaseEjectionTime
	}
	if outlier.Http.ConsecutiveErrors > 0 {
		out.Consecutive_5Xx = &types.UInt32Value{Value: uint32(outlier.Http.ConsecutiveErrors)}
	}
	if outlier.Http.Interval != nil {
		out.Interval = outlier.Http.Interval
	}
	if outlier.Http.MaxEjectionPercent > 0 {
		out.MaxEjectionPercent = &types.UInt32Value{Value: uint32(outlier.Http.MaxEjectionPercent)}
	}
	cluster.OutlierDetection = out
}

func applyLoadBalancer(cluster *v2.Cluster, lb *networking.LoadBalancerSettings) {
	if lb == nil {
		return
	}
	// TODO: RING_HASH and MAGLEV
	switch lb.GetSimple() {
	case networking.LoadBalancerSettings_LEAST_CONN:
		cluster.LbPolicy = v2.Cluster_LEAST_REQUEST
	case networking.LoadBalancerSettings_RANDOM:
		cluster.LbPolicy = v2.Cluster_RANDOM
	case networking.LoadBalancerSettings_ROUND_ROBIN:
		cluster.LbPolicy = v2.Cluster_ROUND_ROBIN
	case networking.LoadBalancerSettings_PASSTHROUGH:
		cluster.LbPolicy = v2.Cluster_ORIGINAL_DST_LB
		cluster.Type = v2.Cluster_ORIGINAL_DST
	}

	// DO not do if else here. since lb.GetSimple returns a enum value (not pointer).
}

func applyUpstreamTLSSettings(cluster *v2.Cluster, tls *networking.TLSSettings) {
	if tls == nil {
		return
	}

	switch tls.Mode {
	case networking.TLSSettings_DISABLE:
		// TODO: Need to make sure that authN does not override this setting
	case networking.TLSSettings_SIMPLE:
		cluster.TlsContext = &auth.UpstreamTlsContext{
			CommonTlsContext: &auth.CommonTlsContext{
				ValidationContext: &auth.CertificateValidationContext{
					TrustedCa: &core.DataSource{
						Specifier: &core.DataSource_Filename{
							Filename: tls.CaCertificates,
						},
					},
					VerifySubjectAltName: tls.SubjectAltNames,
				},
			},
			Sni: tls.Sni,
		}
	case networking.TLSSettings_MUTUAL:
		cluster.TlsContext = &auth.UpstreamTlsContext{
			CommonTlsContext: &auth.CommonTlsContext{
				TlsCertificates: []*auth.TlsCertificate{
					{
						CertificateChain: &core.DataSource{
							Specifier: &core.DataSource_Filename{
								Filename: tls.ClientCertificate,
							},
						},
						PrivateKey: &core.DataSource{
							Specifier: &core.DataSource_Filename{
								Filename: tls.PrivateKey,
							},
						},
					},
				},
				ValidationContext: &auth.CertificateValidationContext{
					TrustedCa: &core.DataSource{
						Specifier: &core.DataSource_Filename{
							Filename: tls.CaCertificates,
						},
					},
					VerifySubjectAltName: tls.SubjectAltNames,
				},
			},
			Sni: tls.Sni,
		}
	}
}

func setUpstreamProtocol(cluster *v2.Cluster, port *model.Port) {
	if port.Protocol.IsHTTP() {
		if port.Protocol == model.ProtocolHTTP2 || port.Protocol == model.ProtocolGRPC {
			cluster.Http2ProtocolOptions = &core.Http2ProtocolOptions{}
		}
	}
}

func buildDefaultCluster(env model.Environment, name string, discoveryType v2.Cluster_DiscoveryType,
	hosts []*core.Address) *v2.Cluster {
	cluster := &v2.Cluster{
		Name:  name,
		Type:  discoveryType,
		Hosts: hosts,
	}
	defaultTrafficPolicy := buildDefaultTrafficPolicy(env, discoveryType)
	applyTrafficPolicy(cluster, defaultTrafficPolicy)
	return cluster
}

func buildDefaultTrafficPolicy(env model.Environment, discoveryType v2.Cluster_DiscoveryType) *networking.TrafficPolicy {
	lbPolicy := DefaultLbType
	if discoveryType == v2.Cluster_ORIGINAL_DST {
		lbPolicy = networking.LoadBalancerSettings_PASSTHROUGH
	}

	return &networking.TrafficPolicy{
		LoadBalancer: &networking.LoadBalancerSettings{
			LbPolicy: &networking.LoadBalancerSettings_Simple{
				Simple: lbPolicy,
			},
		},
		ConnectionPool: &networking.ConnectionPoolSettings{
			Tcp: &networking.ConnectionPoolSettings_TCPSettings{
				ConnectTimeout: &types.Duration{
					Seconds: env.Mesh.ConnectTimeout.Seconds,
					Nanos:   env.Mesh.ConnectTimeout.Nanos,
				},
			},
		},
	}
}
