// Copyright 2021 Antrea Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package connections

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/component-base/metrics/legacyregistry"

	"antrea.io/antrea/pkg/agent/flowexporter"
	interfacestoretest "antrea.io/antrea/pkg/agent/interfacestore/testing"
	"antrea.io/antrea/pkg/agent/metrics"
	proxytest "antrea.io/antrea/pkg/agent/proxy/testing"
	k8sproxy "antrea.io/antrea/third_party/proxy"
)

func TestDenyConnectionStore_AddOrUpdateConn(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	metrics.InitializeConnectionMetrics()
	// Create flow for testing adding and updating of same connection.
	refTime := time.Now()
	tuple := flowexporter.Tuple{SourceAddress: net.IP{1, 2, 3, 4}, DestinationAddress: net.IP{4, 3, 2, 1}, Protocol: 6, SourcePort: 65280, DestinationPort: 255}
	servicePortName := k8sproxy.ServicePortName{
		NamespacedName: types.NamespacedName{
			Namespace: "serviceNS1",
			Name:      "service1",
		},
		Port:     "255",
		Protocol: v1.ProtocolTCP,
	}
	// flow for testing adding and updating
	testFlow := flowexporter.Connection{
		StopTime:                  refTime.Add(-(time.Second * 20)),
		StartTime:                 refTime.Add(-(time.Second * 20)),
		FlowKey:                   tuple,
		DestinationServiceAddress: tuple.DestinationAddress,
		DestinationServicePort:    tuple.DestinationPort,
		OriginalBytes:             uint64(60),
		OriginalPackets:           uint64(1),
		IsActive:                  true,
	}
	mockIfaceStore := interfacestoretest.NewMockInterfaceStore(ctrl)
	mockProxier := proxytest.NewMockProxier(ctrl)
	protocol, _ := lookupServiceProtocol(tuple.Protocol)
	serviceStr := fmt.Sprintf("%s:%d/%s", tuple.DestinationAddress.String(), tuple.DestinationPort, protocol)
	mockProxier.EXPECT().GetServiceByIP(serviceStr).Return(servicePortName, true)
	mockIfaceStore.EXPECT().GetInterfaceByIP(tuple.SourceAddress.String()).Return(nil, false)
	mockIfaceStore.EXPECT().GetInterfaceByIP(tuple.DestinationAddress.String()).Return(nil, false)

	denyConnStore := NewDenyConnectionStore(mockIfaceStore, mockProxier, testFlowExporterOptions)

	denyConnStore.AddOrUpdateConn(&testFlow, refTime.Add(-(time.Second * 20)), uint64(60))
	expConn := testFlow
	expConn.DestinationServicePortName = servicePortName.String()
	actualConn, ok := denyConnStore.GetConnByKey(flowexporter.NewConnectionKey(&testFlow))
	assert.Equal(t, ok, true, "deny connection should be there in deny connection store")
	assert.Equal(t, expConn, *actualConn, "deny connections should be equal")
	assert.Equal(t, 1, denyConnStore.connectionStore.expirePriorityQueue.Len(), "Length of the expire priority queue should be 1")
	checkDenyConnectionMetrics(t, len(denyConnStore.connections))

	denyConnStore.AddOrUpdateConn(&testFlow, refTime.Add(-(time.Second * 10)), uint64(60))
	expConn.OriginalBytes = uint64(120)
	expConn.OriginalPackets = uint64(2)
	expConn.StopTime = refTime.Add(-(time.Second * 10))
	actualConn, ok = denyConnStore.GetConnByKey(flowexporter.NewConnectionKey(&testFlow))
	assert.Equal(t, ok, true, "deny connection should be there in deny connection store")
	assert.Equal(t, expConn, *actualConn, "deny connections should be equal")
	assert.True(t, actualConn.IsActive)
	assert.Equal(t, 1, denyConnStore.connectionStore.expirePriorityQueue.Len())
	checkDenyConnectionMetrics(t, len(denyConnStore.connections))
}

func checkDenyConnectionMetrics(t *testing.T, numConns int) {
	expectedDenyConnectionCount := `
	# HELP antrea_agent_denied_connection_count [ALPHA] Number of denied connections detected by Flow Exporter deny connections tracking. This metric gets updated when a flow is rejected/dropped by network policy.
	# TYPE antrea_agent_denied_connection_count gauge
	`
	expectedDenyConnectionCount = expectedDenyConnectionCount + fmt.Sprintf("antrea_agent_denied_connection_count %d\n", numConns)
	err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(expectedDenyConnectionCount), "antrea_agent_denied_connection_count")
	assert.NoError(t, err)
}
