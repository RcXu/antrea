// Copyright 2022 Antrea Authors
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

package options

import (
	"fmt"
	"net"
	"time"

	"gopkg.in/yaml.v2"

	flowaggregatorconfig "antrea.io/antrea/pkg/config/flowaggregator"
	"antrea.io/antrea/pkg/util/flowexport"
)

type Options struct {
	// The configuration object
	Config *flowaggregatorconfig.FlowAggregatorConfig
	// Expiration timeout for active flow records in the flow aggregator
	ActiveFlowRecordTimeout time.Duration
	// Expiration timeout for inactive flow records in the flow aggregator
	InactiveFlowRecordTimeout time.Duration
	// Transport protocol over which the aggregator collects IPFIX records from all Agents
	AggregatorTransportProtocol flowaggregatorconfig.AggregatorTransportProtocol
	// IPFIX flow collector address
	ExternalFlowCollectorAddr string
	// IPFIX flow collector transport protocol
	ExternalFlowCollectorProto string
	// clickHouseCommitInterval flow records batch commit interval to clickhouse in the flow aggregator
	ClickHouseCommitInterval time.Duration
}

func LoadConfig(configBytes []byte) (*Options, error) {
	var opt Options
	if err := yaml.UnmarshalStrict(configBytes, &opt.Config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal FlowAggregator config from ConfigMap: %v", err)
	}
	flowaggregatorconfig.SetConfigDefaults(opt.Config)
	// validate all the required options.
	if opt.Config.FlowCollector.Enable && opt.Config.FlowCollector.Address == "" {
		return nil, fmt.Errorf("external flow collector enabled without providing address")
	}
	if !opt.Config.FlowCollector.Enable && !opt.Config.ClickHouse.Enable {
		return nil, fmt.Errorf("external flow collector or ClickHouse should be configured")
	}
	// Validate common parameters
	var err error
	opt.ActiveFlowRecordTimeout, err = time.ParseDuration(opt.Config.ActiveFlowRecordTimeout)
	if err != nil {
		return nil, err
	}
	opt.InactiveFlowRecordTimeout, err = time.ParseDuration(opt.Config.InactiveFlowRecordTimeout)
	if err != nil {
		return nil, err
	}
	opt.AggregatorTransportProtocol, err = flowexport.ParseTransportProtocol(opt.Config.AggregatorTransportProtocol)
	if err != nil {
		return nil, err
	}
	// Validate flow collector specific parameters
	if opt.Config.FlowCollector.Enable && len(opt.Config.FlowCollector.Address) > 0 {
		host, port, proto, err := flowexport.ParseFlowCollectorAddr(
			opt.Config.FlowCollector.Address, flowaggregatorconfig.DefaultExternalFlowCollectorPort,
			flowaggregatorconfig.DefaultExternalFlowCollectorTransport)
		if err != nil {
			return nil, err
		}
		opt.ExternalFlowCollectorAddr = net.JoinHostPort(host, port)
		opt.ExternalFlowCollectorProto = proto

		if opt.Config.FlowCollector.RecordFormat != "IPFIX" && opt.Config.FlowCollector.RecordFormat != "JSON" {
			return nil, fmt.Errorf("record format %s is not supported", opt.Config.FlowCollector.RecordFormat)
		}
	}
	// Validate clickhouse specific parameters
	if opt.Config.ClickHouse.Enable {
		opt.ClickHouseCommitInterval, err = time.ParseDuration(opt.Config.ClickHouse.CommitInterval)
		if err != nil {
			return nil, err
		}
		if opt.ClickHouseCommitInterval < flowaggregatorconfig.MinClickHouseCommitInterval {
			return nil, fmt.Errorf("commitInterval %s is too small: shortest supported interval is %s",
				opt.Config.ClickHouse.CommitInterval, flowaggregatorconfig.MinClickHouseCommitInterval)
		}
	}
	return &opt, nil
}
