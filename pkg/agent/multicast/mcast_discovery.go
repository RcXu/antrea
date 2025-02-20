// Copyright 2021 Antrea Authors
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

package multicast

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"antrea.io/libOpenflow/openflow13"
	"antrea.io/libOpenflow/protocol"
	"antrea.io/libOpenflow/util"
	"antrea.io/ofnet/ofctrl"
	apitypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"antrea.io/antrea/pkg/agent/interfacestore"
	"antrea.io/antrea/pkg/agent/openflow"
	"antrea.io/antrea/pkg/agent/types"
	"antrea.io/antrea/pkg/apis/controlplane/v1beta2"
	"antrea.io/antrea/pkg/apis/crd/v1alpha1"
)

const (
	IGMPProtocolNumber = 2
)

var (
	// igmpMaxResponseTime is the maximum time allowed before sending a responding report which is used for the
	// "Max Resp Code" field in the IGMP query message. It is also the maximum time to wait for the IGMP report message
	// when checking the last group member.
	igmpMaxResponseTime = time.Second * 10
	// igmpQueryDstMac is the MAC address used in the dst MAC field in the IGMP query message
	igmpQueryDstMac, _ = net.ParseMAC("01:00:5e:00:00:01")
)

type IGMPSnooper struct {
	ofClient      openflow.Client
	ifaceStore    interfacestore.InterfaceStore
	eventCh       chan *mcastGroupEvent
	validator     types.McastNetworkPolicyController
	queryInterval time.Duration
	// igmpReportANPStats is a map that saves AntreaNetworkPolicyStats of IGMP report packets.
	// The map can be interpreted as
	// map[UID of the AntreaNetworkPolicy]map[name of AntreaNetworkPolicy rule]statistics of rule.
	igmpReportANPStats      map[apitypes.UID]map[string]*types.RuleMetric
	igmpReportANPStatsMutex sync.Mutex
	// Similar to igmpReportANPStats, it stores ACNP stats for IGMP reports.
	igmpReportACNPStats      map[apitypes.UID]map[string]*types.RuleMetric
	igmpReportACNPStatsMutex sync.Mutex
}

func (s *IGMPSnooper) HandlePacketIn(pktIn *ofctrl.PacketIn) error {
	matches := pktIn.GetMatches()
	// Get custom reasons in this packet-in.
	match := matches.GetMatchByName(openflow.CustomReasonField.GetNXFieldName())
	customReasons, err := getInfoInReg(match, openflow.CustomReasonField.GetRange().ToNXRange())
	if err != nil {
		klog.ErrorS(err, "Received error while unloading customReason from OVS reg", "regField", openflow.CustomReasonField.GetName())
		return err
	}
	if customReasons&openflow.CustomReasonIGMP == openflow.CustomReasonIGMP {
		return s.processPacketIn(pktIn)
	}
	return nil
}

func getInfoInReg(regMatch *ofctrl.MatchField, rng *openflow13.NXRange) (uint32, error) {
	regValue, ok := regMatch.GetValue().(*ofctrl.NXRegister)
	if !ok {
		return 0, errors.New("register value cannot be retrieved")
	}
	if rng != nil {
		return ofctrl.GetUint32ValueWithRange(regValue.Data, rng), nil
	}
	return regValue.Data, nil
}

func (s *IGMPSnooper) parseSrcInterface(pktIn *ofctrl.PacketIn) (*interfacestore.InterfaceConfig, error) {
	matches := pktIn.GetMatches()
	ofPortField := matches.GetMatchByName("OXM_OF_IN_PORT")
	if ofPortField == nil {
		return nil, errors.New("in_port field not found")
	}
	ofPort := ofPortField.GetValue().(uint32)
	ifaceConfig, found := s.ifaceStore.GetInterfaceByOFPort(ofPort)
	if !found {
		return nil, errors.New("target Pod not found")
	}
	return ifaceConfig, nil
}

func (s *IGMPSnooper) queryIGMP(group net.IP, versions []uint8) error {
	for _, version := range versions {
		igmp, err := generateIGMPQueryPacket(group, version, s.queryInterval)
		if err != nil {
			return err
		}
		// outPort sets the output port of the packetOut message. We expect the message to go through OVS pipeline
		// from table0. The final OpenFlow message will use a standard OpenFlow port number OFPP_TABLE = 0xfffffff9 corrected
		// by ofnet.
		outPort := uint32(0)
		if err := s.ofClient.SendIGMPQueryPacketOut(igmpQueryDstMac, types.McastAllHosts, outPort, igmp); err != nil {
			return err
		}
		klog.V(2).InfoS("Sent packetOut for IGMP query", "group", group.String(), "version", version, "outPort", outPort)
	}
	return nil
}

func (s *IGMPSnooper) validate(event *mcastGroupEvent, igmpType uint8, packetInData protocol.Ethernet) (bool, error) {
	if s.validator == nil {
		// Return true directly if there is no validator.
		return true, nil
	}
	if event.iface.Type != interfacestore.ContainerInterface {
		return true, fmt.Errorf("interface is not container")
	}

	ruleInfo, err := s.validator.GetIGMPNPRuleInfo(event.iface.PodName, event.iface.PodNamespace, event.group, igmpType)
	if err != nil {
		// It shall drop the packet if function Validate returns error
		klog.ErrorS(err, "Failed to validate multicast group event")
		return false, err
	}

	if ruleInfo != nil {
		klog.V(2).InfoS("Got NetworkPolicy action for IGMP report", "RuleAction", ruleInfo.RuleAction, "uuid", ruleInfo.UUID, "Name", ruleInfo.Name)
		s.addToIGMPReportNPStatsMap(*ruleInfo, uint64(packetInData.Len()))
		if ruleInfo.RuleAction == v1alpha1.RuleActionDrop {
			return false, nil
		}
	}

	return true, nil
}

func (s *IGMPSnooper) validatePacketAndNotify(event *mcastGroupEvent, igmpType uint8, packetInData protocol.Ethernet) {
	allow, err := s.validate(event, igmpType, packetInData)
	if err != nil {
		// Antrea Agent does not remove the Pod from the OpenFlow group bucket immediately when an error is returned,
		// but it will be removed when after timeout (Controller.mcastGroupTimeout)
		return
	}
	if !allow {
		// If any rule is desired to drop the traffic, Antrea Agent removes the Pod from
		// the OpenFlow group bucket directly
		event.eType = groupLeave
	}
	s.eventCh <- event
}

func (s *IGMPSnooper) addToIGMPReportNPStatsMap(item types.IGMPNPRuleInfo, packetLen uint64) {
	updateRuleStats := func(igmpReportStatsMap map[apitypes.UID]map[string]*types.RuleMetric, uuid apitypes.UID, name string) {
		if igmpReportStatsMap[uuid] == nil {
			igmpReportStatsMap[uuid] = make(map[string]*types.RuleMetric)
		}
		if igmpReportStatsMap[uuid][name] == nil {
			igmpReportStatsMap[uuid][name] = &types.RuleMetric{}
		}
		t := igmpReportStatsMap[uuid][name]
		t.Packets += 1
		t.Bytes += packetLen
	}
	ruleType := *item.NPType
	if ruleType == v1beta2.AntreaNetworkPolicy {
		s.igmpReportANPStatsMutex.Lock()
		updateRuleStats(s.igmpReportANPStats, item.UUID, item.Name)
		s.igmpReportANPStatsMutex.Unlock()
	} else if ruleType == v1beta2.AntreaClusterNetworkPolicy {
		s.igmpReportACNPStatsMutex.Lock()
		updateRuleStats(s.igmpReportACNPStats, item.UUID, item.Name)
		s.igmpReportACNPStatsMutex.Unlock()
	}
}

// WARNING: This func will reset the saved stats.
func (s *IGMPSnooper) collectStats() (igmpANPStats, igmpACNPStats map[apitypes.UID]map[string]*types.RuleMetric) {
	s.igmpReportANPStatsMutex.Lock()
	igmpANPStats = s.igmpReportANPStats
	s.igmpReportANPStats = make(map[apitypes.UID]map[string]*types.RuleMetric)
	s.igmpReportANPStatsMutex.Unlock()
	s.igmpReportACNPStatsMutex.Lock()
	igmpACNPStats = s.igmpReportACNPStats
	s.igmpReportACNPStats = make(map[apitypes.UID]map[string]*types.RuleMetric)
	s.igmpReportACNPStatsMutex.Unlock()
	return igmpANPStats, igmpACNPStats
}

func (s *IGMPSnooper) processPacketIn(pktIn *ofctrl.PacketIn) error {
	now := time.Now()
	iface, err := s.parseSrcInterface(pktIn)
	if err != nil {
		return err
	}
	klog.V(2).InfoS("Received PacketIn for IGMP packet", "in_port", iface.OFPort)
	podName := "unknown"
	if iface.Type == interfacestore.ContainerInterface {
		podName = iface.PodName
	}
	igmp, err := parseIGMPPacket(pktIn.Data)
	if err != nil {
		return err
	}
	igmpType := igmp.GetMessageType()
	switch igmpType {
	case protocol.IGMPv1Report:
		fallthrough
	case protocol.IGMPv2Report:
		mgroup := igmp.(*protocol.IGMPv1or2).GroupAddress
		klog.V(2).InfoS("Received IGMPv1or2 Report message", "group", mgroup.String(), "interface", iface.InterfaceName, "pod", podName)
		event := &mcastGroupEvent{
			group: mgroup,
			eType: groupJoin,
			time:  now,
			iface: iface,
		}
		s.validatePacketAndNotify(event, igmpType, pktIn.Data)
	case protocol.IGMPv3Report:
		msg := igmp.(*protocol.IGMPv3MembershipReport)
		for _, gr := range msg.GroupRecords {
			mgroup := gr.MulticastAddress
			klog.V(2).InfoS("Received IGMPv3 Report message", "group", mgroup.String(), "interface", iface.InterfaceName, "pod", podName, "recordType", gr.Type, "sourceCount", gr.NumberOfSources)
			evtType := groupJoin
			if (gr.Type == protocol.IGMPIsIn || gr.Type == protocol.IGMPToIn) && gr.NumberOfSources == 0 {
				evtType = groupLeave
			}
			event := &mcastGroupEvent{
				group: mgroup,
				eType: evtType,
				time:  now,
				iface: iface,
			}
			s.validatePacketAndNotify(event, igmpType, pktIn.Data)
		}
	case protocol.IGMPv2LeaveGroup:
		mgroup := igmp.(*protocol.IGMPv1or2).GroupAddress
		klog.V(2).InfoS("Received IGMPv2 Leave message", "group", mgroup.String(), "interface", iface.InterfaceName, "pod", podName)
		event := &mcastGroupEvent{
			group: mgroup,
			eType: groupLeave,
			time:  now,
			iface: iface,
		}
		s.eventCh <- event
	}
	return nil
}

func generateIGMPQueryPacket(group net.IP, version uint8, queryInterval time.Duration) (util.Message, error) {
	// The max response time field in IGMP protocol uses a value in units of 1/10 second.
	// See https://datatracker.ietf.org/doc/html/rfc2236 and https://datatracker.ietf.org/doc/html/rfc3376
	respTime := uint8(igmpMaxResponseTime.Seconds() * 10)
	switch version {
	case 1:
		return &protocol.IGMPv1or2{
			Type:            protocol.IGMPQuery,
			MaxResponseTime: 0,
			Checksum:        0,
			GroupAddress:    group,
		}, nil
	case 2:
		return &protocol.IGMPv1or2{
			Type:            protocol.IGMPQuery,
			MaxResponseTime: respTime,
			Checksum:        0,
			GroupAddress:    group,
		}, nil
	case 3:
		return &protocol.IGMPv3Query{
			Type:                     protocol.IGMPQuery,
			MaxResponseTime:          respTime,
			GroupAddress:             group,
			SuppressRouterProcessing: false,
			RobustnessValue:          0,
			IntervalTime:             uint8(queryInterval.Seconds()),
			NumberOfSources:          0,
		}, nil
	}
	return nil, fmt.Errorf("unsupported IGMP version %d", version)
}

func parseIGMPPacket(pkt protocol.Ethernet) (protocol.IGMPMessage, error) {
	if pkt.Ethertype != protocol.IPv4_MSG {
		return nil, errors.New("not IPv4 packet")
	}
	ipPacket, ok := pkt.Data.(*protocol.IPv4)
	if !ok {
		return nil, errors.New("failed to parse IPv4 packet")
	}
	if ipPacket.Protocol != IGMPProtocolNumber {
		return nil, errors.New("not IGMP packet")
	}
	data, _ := ipPacket.Data.MarshalBinary()
	igmpLength := ipPacket.Length - uint16(4*ipPacket.IHL)
	if igmpLength == 8 {
		igmp := new(protocol.IGMPv1or2)
		if err := igmp.UnmarshalBinary(data); err != nil {
			return nil, err
		}
		return igmp, nil
	}
	switch data[0] {
	case protocol.IGMPQuery:
		igmp := new(protocol.IGMPv3Query)
		if err := igmp.UnmarshalBinary(data); err != nil {
			return nil, err
		}
		return igmp, nil
	case protocol.IGMPv3Report:
		igmp := new(protocol.IGMPv3MembershipReport)
		if err := igmp.UnmarshalBinary(data); err != nil {
			return nil, err
		}
		return igmp, nil
	default:
		return nil, errors.New("unknown igmp packet")
	}
}

func newSnooper(ofClient openflow.Client, ifaceStore interfacestore.InterfaceStore, eventCh chan *mcastGroupEvent, queryInterval time.Duration, multicastValidator types.McastNetworkPolicyController) *IGMPSnooper {
	snooper := &IGMPSnooper{ofClient: ofClient, ifaceStore: ifaceStore, eventCh: eventCh, validator: multicastValidator, queryInterval: queryInterval}
	snooper.igmpReportACNPStats = make(map[apitypes.UID]map[string]*types.RuleMetric)
	snooper.igmpReportANPStats = make(map[apitypes.UID]map[string]*types.RuleMetric)
	ofClient.RegisterPacketInHandler(uint8(openflow.PacketInReasonMC), "MulticastGroupDiscovery", snooper)
	return snooper
}
