/*
 *
 * Copyright 2021 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package xdsresource

import (
	"fmt"
	"net"
	"strconv"

	v3corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	v3endpointpb "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	v3typepb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/golang/protobuf/proto"
	"google.golang.org/grpc/internal/grpclog"
	"google.golang.org/grpc/internal/pretty"
	"google.golang.org/grpc/xds/internal"
	"google.golang.org/protobuf/types/known/anypb"
)

// UnmarshalEndpoints processes resources received in an EDS response,
// validates them, and transforms them into a native struct which contains only
// fields we are interested in.
func UnmarshalEndpoints(opts *UnmarshalOptions) (map[string]EndpointsUpdateErrTuple, UpdateMetadata, error) {
	update := make(map[string]EndpointsUpdateErrTuple)
	md, err := processAllResources(opts, update)
	return update, md, err
}

func unmarshalEndpointsResource(r *anypb.Any, logger *grpclog.PrefixLogger) (string, EndpointsUpdate, error) {
	r, err := unwrapResource(r)
	if err != nil {
		return "", EndpointsUpdate{}, fmt.Errorf("failed to unwrap resource: %v", err)
	}

	if !IsEndpointsResource(r.GetTypeUrl()) {
		return "", EndpointsUpdate{}, fmt.Errorf("unexpected resource type: %q ", r.GetTypeUrl())
	}

	cla := &v3endpointpb.ClusterLoadAssignment{}
	if err := proto.Unmarshal(r.GetValue(), cla); err != nil {
		return "", EndpointsUpdate{}, fmt.Errorf("failed to unmarshal resource: %v", err)
	}
	logger.Infof("Resource with name: %v, type: %T, contains: %v", cla.GetClusterName(), cla, pretty.ToJSON(cla))

	u, err := parseEDSRespProto(cla, logger)
	if err != nil {
		return cla.GetClusterName(), EndpointsUpdate{}, err
	}
	u.Raw = r
	return cla.GetClusterName(), u, nil
}

func parseAddress(socketAddress *v3corepb.SocketAddress) string {
	return net.JoinHostPort(socketAddress.GetAddress(), strconv.Itoa(int(socketAddress.GetPortValue())))
}

func parseDropPolicy(dropPolicy *v3endpointpb.ClusterLoadAssignment_Policy_DropOverload) OverloadDropConfig {
	percentage := dropPolicy.GetDropPercentage()
	var (
		numerator   = percentage.GetNumerator()
		denominator uint32
	)
	switch percentage.GetDenominator() {
	case v3typepb.FractionalPercent_HUNDRED:
		denominator = 100
	case v3typepb.FractionalPercent_TEN_THOUSAND:
		denominator = 10000
	case v3typepb.FractionalPercent_MILLION:
		denominator = 1000000
	}
	return OverloadDropConfig{
		Category:    dropPolicy.GetCategory(),
		Numerator:   numerator,
		Denominator: denominator,
	}
}

func parseEndpoints(lbEndpoints []*v3endpointpb.LbEndpoint) ([]Endpoint, error) {
	endpoints := make([]Endpoint, 0, len(lbEndpoints))
	for _, lbEndpoint := range lbEndpoints {
		weight := lbEndpoint.GetLoadBalancingWeight().GetValue()
		if weight == 0 {
			return nil, fmt.Errorf("EDS response contains an endpoint with zero weight: %+v", lbEndpoint)
		}
		endpoints = append(endpoints, Endpoint{
			HealthStatus: EndpointHealthStatus(lbEndpoint.GetHealthStatus()),
			Address:      parseAddress(lbEndpoint.GetEndpoint().GetAddress().GetSocketAddress()),
			Weight:       weight,
		})
	}
	return endpoints, nil
}

func parseEDSRespProto(m *v3endpointpb.ClusterLoadAssignment, logger *grpclog.PrefixLogger) (EndpointsUpdate, error) {
	ret := EndpointsUpdate{}
	for _, dropPolicy := range m.GetPolicy().GetDropOverloads() {
		ret.Drops = append(ret.Drops, parseDropPolicy(dropPolicy))
	}
	priorities := make(map[uint32]map[string]bool)
	for _, locality := range m.Endpoints {
		l := locality.GetLocality()
		if l == nil {
			return EndpointsUpdate{}, fmt.Errorf("EDS response contains a locality without ID, locality: %+v", locality)
		}
		weight := locality.GetLoadBalancingWeight().GetValue()
		if weight == 0 {
			logger.Warningf("Ignoring locality %s with weight 0", pretty.ToJSON(l))
			continue
		}
		lid := internal.LocalityID{
			Region:  l.Region,
			Zone:    l.Zone,
			SubZone: l.SubZone,
		}
		priority := locality.GetPriority()
		localitiesWithPriority := priorities[priority]
		if localitiesWithPriority == nil {
			localitiesWithPriority = make(map[string]bool)
			priorities[priority] = localitiesWithPriority
		}
		lidStr, _ := lid.ToString()
		if localitiesWithPriority[lidStr] {
			return EndpointsUpdate{}, fmt.Errorf("duplicate locality %s with the same priority %v", lidStr, priority)
		}
		localitiesWithPriority[lidStr] = true
		endpoints, err := parseEndpoints(locality.GetLbEndpoints())
		if err != nil {
			return EndpointsUpdate{}, err
		}
		ret.Localities = append(ret.Localities, Locality{
			ID:        lid,
			Endpoints: endpoints,
			Weight:    locality.GetLoadBalancingWeight().GetValue(),
			Priority:  priority,
		})
	}
	for i := 0; i < len(priorities); i++ {
		if _, ok := priorities[uint32(i)]; !ok {
			return EndpointsUpdate{}, fmt.Errorf("priority %v missing (with different priorities %v received)", i, priorities)
		}
	}
	return ret, nil
}
