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

package clickhouseclient

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go"
	"github.com/gammazero/deque"
	ipfixentities "github.com/vmware/go-ipfix/pkg/entities"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

const (
	maxQueueSize = 1 << 19 // 524288. ~500MB assuming 1KB per record
	insertQuery  = `INSERT INTO flows (
                   flowStartSeconds,
                   flowEndSeconds,
                   flowEndSecondsFromSourceNode,
                   flowEndSecondsFromDestinationNode,
                   flowEndReason,
                   sourceIP,
                   destinationIP,
                   sourceTransportPort,
                   destinationTransportPort,
                   protocolIdentifier,
                   packetTotalCount,
                   octetTotalCount,
                   packetDeltaCount,
                   octetDeltaCount,
                   reversePacketTotalCount,
                   reverseOctetTotalCount,
                   reversePacketDeltaCount,
                   reverseOctetDeltaCount,
                   sourcePodName,
                   sourcePodNamespace,
                   sourceNodeName,
                   destinationPodName,
                   destinationPodNamespace,
                   destinationNodeName,
                   destinationClusterIP,
                   destinationServicePort,
                   destinationServicePortName,
                   ingressNetworkPolicyName,
                   ingressNetworkPolicyNamespace,
                   ingressNetworkPolicyRuleName,
                   ingressNetworkPolicyRuleAction,
                   ingressNetworkPolicyType,
                   egressNetworkPolicyName,
                   egressNetworkPolicyNamespace,
                   egressNetworkPolicyRuleName,
                   egressNetworkPolicyRuleAction,
                   egressNetworkPolicyType,
                   tcpState,
                   flowType,
                   sourcePodLabels,
                   destinationPodLabels,
                   throughput,
                   reverseThroughput,
                   throughputFromSourceNode,
                   throughputFromDestinationNode,
                   reverseThroughputFromSourceNode,
                   reverseThroughputFromDestinationNode) 
                   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 
                           ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
)

type ClickHouseExportProcess struct {
	// db holds sql connection struct to clickhouse db.
	db *sql.DB
	// dsn is data source name used for connection to clickhouse db.
	dsn string
	// deque buffers flows records between batch commits.
	deque *deque.Deque
	// mutex is for concurrency between adding and removing records from deque.
	mutex sync.RWMutex
	// queueSize is the max size of deque
	queueSize int
	// commitInterval is the interval between batch commits
	commitInterval time.Duration
	// stopCh is the channel to receive stop message
	stopCh chan bool
	// commitTicker is a ticker, containing a channel used to trigger batchCommitAll() for every commitInterval period
	commitTicker *time.Ticker
}

type ClickHouseInput struct {
	Username       string
	Password       string
	Database       string
	DatabaseURL    string
	Debug          bool
	Compress       *bool
	CommitInterval time.Duration
}

func (ci *ClickHouseInput) getDataSourceName() (string, error) {
	if len(ci.DatabaseURL) == 0 || len(ci.Username) == 0 || len(ci.Password) == 0 {
		return "", fmt.Errorf("URL, Username or Password missing for clickhouse DSN")
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s?username=%s&password=%s", ci.DatabaseURL, ci.Username, ci.Password))

	if len(ci.Database) > 0 {
		sb.WriteString("&database=")
		sb.WriteString(ci.Database)
	}
	if ci.Debug {
		sb.WriteString("&debug=true")
	} else {
		sb.WriteString("&debug=false")
	}
	if *ci.Compress {
		sb.WriteString("&compress=true")
	} else {
		sb.WriteString("&compress=false")
	}

	return sb.String(), nil
}

type ClickHouseFlowRow struct {
	flowStartSeconds                     time.Time
	flowEndSeconds                       time.Time
	flowEndSecondsFromSourceNode         time.Time
	flowEndSecondsFromDestinationNode    time.Time
	flowEndReason                        uint8
	sourceIP                             string
	destinationIP                        string
	sourceTransportPort                  uint16
	destinationTransportPort             uint16
	protocolIdentifier                   uint8
	packetTotalCount                     uint64
	octetTotalCount                      uint64
	packetDeltaCount                     uint64
	octetDeltaCount                      uint64
	reversePacketTotalCount              uint64
	reverseOctetTotalCount               uint64
	reversePacketDeltaCount              uint64
	reverseOctetDeltaCount               uint64
	sourcePodName                        string
	sourcePodNamespace                   string
	sourceNodeName                       string
	destinationPodName                   string
	destinationPodNamespace              string
	destinationNodeName                  string
	destinationClusterIP                 string
	destinationServicePort               uint16
	destinationServicePortName           string
	ingressNetworkPolicyName             string
	ingressNetworkPolicyNamespace        string
	ingressNetworkPolicyRuleName         string
	ingressNetworkPolicyRuleAction       uint8
	ingressNetworkPolicyType             uint8
	egressNetworkPolicyName              string
	egressNetworkPolicyNamespace         string
	egressNetworkPolicyRuleName          string
	egressNetworkPolicyRuleAction        uint8
	egressNetworkPolicyType              uint8
	tcpState                             string
	flowType                             uint8
	sourcePodLabels                      string
	destinationPodLabels                 string
	throughput                           uint64
	reverseThroughput                    uint64
	throughputFromSourceNode             uint64
	throughputFromDestinationNode        uint64
	reverseThroughputFromSourceNode      uint64
	reverseThroughputFromDestinationNode uint64
}

func NewClickHouseClient(input ClickHouseInput) (*ClickHouseExportProcess, error) {

	dsn, connect, err := PrepareConnection(input)
	if err != nil {
		return nil, err
	}

	chClient := &ClickHouseExportProcess{
		db:             connect,
		dsn:            dsn,
		deque:          deque.New(),
		mutex:          sync.RWMutex{},
		queueSize:      maxQueueSize,
		commitInterval: input.CommitInterval,
		stopCh:         make(chan bool),
		commitTicker:   time.NewTicker(input.CommitInterval),
	}
	return chClient, nil
}

func (ch *ClickHouseExportProcess) CacheSet(set ipfixentities.Set) {
	record := set.GetRecords()[0]
	chRow := ch.getClickHouseFlowRow(record)

	ch.mutex.Lock()
	defer ch.mutex.Unlock()
	for ch.deque.Len() >= ch.queueSize {
		ch.deque.PopFront()
	}
	ch.deque.PushBack(chRow)
}

func (ch *ClickHouseExportProcess) Start() {
	go ch.flowRecordPeriodicCommit()
	<-ch.stopCh
}

func (ch *ClickHouseExportProcess) Stop() {
	close(ch.stopCh)
}

func (ch *ClickHouseExportProcess) getClickHouseFlowRow(record ipfixentities.Record) *ClickHouseFlowRow {
	chFlowRow := ClickHouseFlowRow{}
	if flowStartSeconds, _, ok := record.GetInfoElementWithValue("flowStartSeconds"); ok {
		chFlowRow.flowStartSeconds = time.Unix(int64(flowStartSeconds.GetUnsigned32Value()), 0)
	}
	if flowEndSeconds, _, ok := record.GetInfoElementWithValue("flowEndSeconds"); ok {
		chFlowRow.flowEndSeconds = time.Unix(int64(flowEndSeconds.GetUnsigned32Value()), 0)
	}
	if flowEndSecFromSrcNode, _, ok := record.GetInfoElementWithValue("flowEndSecondsFromSourceNode"); ok {
		chFlowRow.flowEndSecondsFromSourceNode = time.Unix(int64(flowEndSecFromSrcNode.GetUnsigned32Value()), 0)
	}
	if flowEndSecFromDstNode, _, ok := record.GetInfoElementWithValue("flowEndSecondsFromDestinationNode"); ok {
		chFlowRow.flowEndSecondsFromDestinationNode = time.Unix(int64(flowEndSecFromDstNode.GetUnsigned32Value()), 0)
	}
	if flowEndReason, _, ok := record.GetInfoElementWithValue("flowEndReason"); ok {
		chFlowRow.flowEndReason = flowEndReason.GetUnsigned8Value()
	}
	if sourceIPv4, _, ok := record.GetInfoElementWithValue("sourceIPv4Address"); ok {
		chFlowRow.sourceIP = sourceIPv4.GetIPAddressValue().String()
	} else if sourceIPv6, _, ok := record.GetInfoElementWithValue("sourceIPv6Address"); ok {
		chFlowRow.sourceIP = sourceIPv6.GetIPAddressValue().String()
	}
	if destinationIPv4, _, ok := record.GetInfoElementWithValue("destinationIPv4Address"); ok {
		chFlowRow.destinationIP = destinationIPv4.GetIPAddressValue().String()
	} else if destinationIPv6, _, ok := record.GetInfoElementWithValue("destinationIPv6Address"); ok {
		chFlowRow.destinationIP = destinationIPv6.GetIPAddressValue().String()
	}
	if sourcePort, _, ok := record.GetInfoElementWithValue("sourceTransportPort"); ok {
		chFlowRow.sourceTransportPort = sourcePort.GetUnsigned16Value()
	}
	if destinationPort, _, ok := record.GetInfoElementWithValue("destinationTransportPort"); ok {
		chFlowRow.destinationTransportPort = destinationPort.GetUnsigned16Value()
	}
	if protocolIdentifier, _, ok := record.GetInfoElementWithValue("protocolIdentifier"); ok {
		chFlowRow.protocolIdentifier = protocolIdentifier.GetUnsigned8Value()
	}
	if packetTotalCount, _, ok := record.GetInfoElementWithValue("packetTotalCount"); ok {
		chFlowRow.packetTotalCount = packetTotalCount.GetUnsigned64Value()
	}
	if octetTotalCount, _, ok := record.GetInfoElementWithValue("octetTotalCount"); ok {
		chFlowRow.octetTotalCount = octetTotalCount.GetUnsigned64Value()
	}
	if packetDeltaCount, _, ok := record.GetInfoElementWithValue("packetDeltaCount"); ok {
		chFlowRow.packetDeltaCount = packetDeltaCount.GetUnsigned64Value()
	}
	if octetDeltaCount, _, ok := record.GetInfoElementWithValue("octetDeltaCount"); ok {
		chFlowRow.octetDeltaCount = octetDeltaCount.GetUnsigned64Value()
	}
	if reversePacketTotalCount, _, ok := record.GetInfoElementWithValue("reversePacketTotalCount"); ok {
		chFlowRow.reversePacketTotalCount = reversePacketTotalCount.GetUnsigned64Value()
	}
	if reverseOctetTotalCount, _, ok := record.GetInfoElementWithValue("reverseOctetTotalCount"); ok {
		chFlowRow.reverseOctetTotalCount = reverseOctetTotalCount.GetUnsigned64Value()
	}
	if reversePacketDeltaCount, _, ok := record.GetInfoElementWithValue("reversePacketDeltaCount"); ok {
		chFlowRow.reversePacketDeltaCount = reversePacketDeltaCount.GetUnsigned64Value()
	}
	if reverseOctetDeltaCount, _, ok := record.GetInfoElementWithValue("reverseOctetDeltaCount"); ok {
		chFlowRow.reverseOctetDeltaCount = reverseOctetDeltaCount.GetUnsigned64Value()
	}
	if sourcePodName, _, ok := record.GetInfoElementWithValue("sourcePodName"); ok {
		chFlowRow.sourcePodName = sourcePodName.GetStringValue()
	}
	if sourcePodNamespace, _, ok := record.GetInfoElementWithValue("sourcePodNamespace"); ok {
		chFlowRow.sourcePodNamespace = sourcePodNamespace.GetStringValue()
	}
	if sourceNodeName, _, ok := record.GetInfoElementWithValue("sourceNodeName"); ok {
		chFlowRow.sourceNodeName = sourceNodeName.GetStringValue()
	}
	if destinationPodName, _, ok := record.GetInfoElementWithValue("destinationPodName"); ok {
		chFlowRow.destinationPodName = destinationPodName.GetStringValue()
	}
	if destinationPodNamespace, _, ok := record.GetInfoElementWithValue("destinationPodNamespace"); ok {
		chFlowRow.destinationPodNamespace = destinationPodNamespace.GetStringValue()
	}
	if destinationNodeName, _, ok := record.GetInfoElementWithValue("destinationNodeName"); ok {
		chFlowRow.destinationNodeName = destinationNodeName.GetStringValue()
	}
	if destinationClusterIPv4, _, ok := record.GetInfoElementWithValue("destinationClusterIPv4"); ok {
		chFlowRow.destinationClusterIP = destinationClusterIPv4.GetIPAddressValue().String()
	} else if destinationClusterIPv6, _, ok := record.GetInfoElementWithValue("destinationClusterIPv6"); ok {
		chFlowRow.destinationClusterIP = destinationClusterIPv6.GetIPAddressValue().String()
	}
	if destinationServicePort, _, ok := record.GetInfoElementWithValue("destinationServicePort"); ok {
		chFlowRow.destinationServicePort = destinationServicePort.GetUnsigned16Value()
	}
	if destinationServicePortName, _, ok := record.GetInfoElementWithValue("destinationServicePortName"); ok {
		chFlowRow.destinationServicePortName = destinationServicePortName.GetStringValue()
	}
	if ingressNPName, _, ok := record.GetInfoElementWithValue("ingressNetworkPolicyName"); ok {
		chFlowRow.ingressNetworkPolicyName = ingressNPName.GetStringValue()
	}
	if ingressNPNamespace, _, ok := record.GetInfoElementWithValue("ingressNetworkPolicyNamespace"); ok {
		chFlowRow.ingressNetworkPolicyNamespace = ingressNPNamespace.GetStringValue()
	}
	if ingressNPRuleName, _, ok := record.GetInfoElementWithValue("ingressNetworkPolicyRuleName"); ok {
		chFlowRow.ingressNetworkPolicyRuleName = ingressNPRuleName.GetStringValue()
	}
	if ingressNPType, _, ok := record.GetInfoElementWithValue("ingressNetworkPolicyType"); ok {
		chFlowRow.ingressNetworkPolicyType = ingressNPType.GetUnsigned8Value()
	}
	if ingressNPRuleAction, _, ok := record.GetInfoElementWithValue("ingressNetworkPolicyRuleAction"); ok {
		chFlowRow.ingressNetworkPolicyRuleAction = ingressNPRuleAction.GetUnsigned8Value()
	}
	if egressNPName, _, ok := record.GetInfoElementWithValue("egressNetworkPolicyName"); ok {
		chFlowRow.egressNetworkPolicyName = egressNPName.GetStringValue()
	}
	if egressNPNamespace, _, ok := record.GetInfoElementWithValue("egressNetworkPolicyNamespace"); ok {
		chFlowRow.egressNetworkPolicyNamespace = egressNPNamespace.GetStringValue()
	}
	if egressNPRuleName, _, ok := record.GetInfoElementWithValue("egressNetworkPolicyRuleName"); ok {
		chFlowRow.egressNetworkPolicyRuleName = egressNPRuleName.GetStringValue()
	}
	if egressNPType, _, ok := record.GetInfoElementWithValue("egressNetworkPolicyType"); ok {
		chFlowRow.egressNetworkPolicyType = egressNPType.GetUnsigned8Value()
	}
	if egressNPRuleAction, _, ok := record.GetInfoElementWithValue("egressNetworkPolicyRuleAction"); ok {
		chFlowRow.egressNetworkPolicyRuleAction = egressNPRuleAction.GetUnsigned8Value()
	}
	if tcpState, _, ok := record.GetInfoElementWithValue("tcpState"); ok {
		chFlowRow.tcpState = tcpState.GetStringValue()
	}
	if flowType, _, ok := record.GetInfoElementWithValue("flowType"); ok {
		chFlowRow.flowType = flowType.GetUnsigned8Value()
	}
	if sourcePodLabels, _, ok := record.GetInfoElementWithValue("sourcePodLabels"); ok {
		chFlowRow.sourcePodLabels = sourcePodLabels.GetStringValue()
	}
	if destinationPodLabels, _, ok := record.GetInfoElementWithValue("destinationPodLabels"); ok {
		chFlowRow.destinationPodLabels = destinationPodLabels.GetStringValue()
	}
	if throughput, _, ok := record.GetInfoElementWithValue("throughput"); ok {
		chFlowRow.throughput = throughput.GetUnsigned64Value()
	}
	if reverseThroughput, _, ok := record.GetInfoElementWithValue("reverseThroughput"); ok {
		chFlowRow.reverseThroughput = reverseThroughput.GetUnsigned64Value()
	}
	if throughputFromSrcNode, _, ok := record.GetInfoElementWithValue("throughputFromSourceNode"); ok {
		chFlowRow.throughputFromSourceNode = throughputFromSrcNode.GetUnsigned64Value()
	}
	if throughputFromDstNode, _, ok := record.GetInfoElementWithValue("throughputFromDestinationNode"); ok {
		chFlowRow.throughputFromDestinationNode = throughputFromDstNode.GetUnsigned64Value()
	}
	if revTputFromSrcNode, _, ok := record.GetInfoElementWithValue("reverseThroughputFromSourceNode"); ok {
		chFlowRow.reverseThroughputFromSourceNode = revTputFromSrcNode.GetUnsigned64Value()
	}
	if revTputFromDstNode, _, ok := record.GetInfoElementWithValue("reverseThroughputFromDestinationNode"); ok {
		chFlowRow.reverseThroughputFromDestinationNode = revTputFromDstNode.GetUnsigned64Value()
	}
	return &chFlowRow
}

func (ch *ClickHouseExportProcess) flowRecordPeriodicCommit() {
	logTicker := time.NewTicker(time.Minute)
	committedRec := 0
	for {
		select {
		case <-ch.stopCh:
			klog.InfoS("Stopping ClickHouse exporting process")
			committed, err := ch.batchCommitAll()
			if err != nil {
				klog.ErrorS(err, "Error when doing last batchCommitAll")
			} else {
				committedRec += committed
				klog.V(4).InfoS("Total number of records committed to DB", "count", committedRec)
			}
			ch.commitTicker.Stop()
			logTicker.Stop()
			return
		case <-ch.commitTicker.C:
			committed, err := ch.batchCommitAll()
			if err == nil {
				committedRec += committed
			}
		case <-logTicker.C:
			klog.V(4).InfoS("Total number of records committed to DB", "count", committedRec)
			committedRec = 0
		}
	}
}

// batchCommitAll commits all flow records cached in local deque in one INSERT query.
// Returns the number of records successfully committed, and error if encountered.
// Cached records will be removed only after successful commit.
func (ch *ClickHouseExportProcess) batchCommitAll() (int, error) {
	currSize := ch.deque.Len()
	if currSize == 0 {
		return 0, nil
	}

	var stmt *sql.Stmt

	// start new connection
	tx, err := ch.db.Begin()
	if err == nil {
		stmt, err = tx.Prepare(insertQuery)
	}
	if err != nil {
		klog.ErrorS(err, "Error when preparing insert statement")
		_ = tx.Rollback()
		return 0, err
	}

	// populate items from deque
	for i := 0; i < currSize; i++ {
		record, ok := ch.deque.At(i).(*ClickHouseFlowRow)
		if !ok {
			continue
		}
		_, err := stmt.Exec(
			record.flowStartSeconds,
			record.flowEndSeconds,
			record.flowEndSecondsFromSourceNode,
			record.flowEndSecondsFromDestinationNode,
			record.flowEndReason,
			record.sourceIP,
			record.destinationIP,
			record.sourceTransportPort,
			record.destinationTransportPort,
			record.protocolIdentifier,
			record.packetTotalCount,
			record.octetTotalCount,
			record.packetDeltaCount,
			record.octetDeltaCount,
			record.reversePacketTotalCount,
			record.reverseOctetTotalCount,
			record.reversePacketDeltaCount,
			record.reverseOctetDeltaCount,
			record.sourcePodName,
			record.sourcePodNamespace,
			record.sourceNodeName,
			record.destinationPodName,
			record.destinationPodNamespace,
			record.destinationNodeName,
			record.destinationClusterIP,
			record.destinationServicePort,
			record.destinationServicePortName,
			record.ingressNetworkPolicyName,
			record.ingressNetworkPolicyNamespace,
			record.ingressNetworkPolicyRuleName,
			record.ingressNetworkPolicyRuleAction,
			record.ingressNetworkPolicyType,
			record.egressNetworkPolicyName,
			record.egressNetworkPolicyNamespace,
			record.egressNetworkPolicyRuleName,
			record.egressNetworkPolicyRuleAction,
			record.egressNetworkPolicyType,
			record.tcpState,
			record.flowType,
			record.sourcePodLabels,
			record.destinationPodLabels,
			record.throughput,
			record.reverseThroughput,
			record.throughputFromSourceNode,
			record.throughputFromDestinationNode,
			record.reverseThroughputFromSourceNode,
			record.reverseThroughputFromDestinationNode)

		if err != nil {
			klog.ErrorS(err, "Error when adding record")
			_ = tx.Rollback()
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		klog.ErrorS(err, "Error when committing record")
		return 0, err
	}

	// remove committed record from deque
	ch.mutex.Lock()
	defer ch.mutex.Unlock()
	for i := 0; i < currSize; i++ {
		ch.deque.PopFront()
	}

	return currSize, nil
}

func PrepareConnection(input ClickHouseInput) (string, *sql.DB, error) {
	dsn, err := input.getDataSourceName()
	if err != nil {
		return "", nil, fmt.Errorf("error when parsing ClickHouse DSN: %v", err)
	}
	connect, err := ConnectClickHouse(dsn)
	if err != nil {
		return "", nil, err
	}
	// Test open Transaction
	tx, err := connect.Begin()
	if err == nil {
		_, err = tx.Prepare(insertQuery)
	}
	if err != nil {
		return "", nil, fmt.Errorf("error when preparing insert statement, %v", err)
	}
	_ = tx.Commit()
	return dsn, connect, err
}

func (ch *ClickHouseExportProcess) GetDsnMap() map[string]string {
	parseURL := strings.Split(ch.dsn, "?")
	m := make(map[string]string)
	m["databaseURL"] = parseURL[0]
	for _, v := range strings.Split(parseURL[1], "&") {
		pair := strings.Split(v, "=")
		m[pair[0]] = pair[1]
	}
	return m
}

func (ch *ClickHouseExportProcess) UpdateCH(input *ClickHouseExportProcess, dsn string, connect *sql.DB) {
	input.mutex.Lock()
	defer input.mutex.Unlock()
	input.dsn = dsn
	input.db = connect
}

func (ch *ClickHouseExportProcess) GetCommitInterval() time.Duration {
	return ch.commitInterval
}

func (ch *ClickHouseExportProcess) SetCommitInterval(commitInterval time.Duration) {
	ch.commitInterval = commitInterval
	ch.commitTicker.Reset(ch.commitInterval)
}

func (ch *ClickHouseExportProcess) GetDsn() string {
	return ch.dsn
}

func ConnectClickHouse(url string) (*sql.DB, error) {
	var connect *sql.DB
	var connErr error
	connRetryInterval := 1 * time.Second
	connTimeout := 10 * time.Second

	// Connect to ClickHouse in a loop
	if err := wait.PollImmediate(connRetryInterval, connTimeout, func() (bool, error) {
		// Open the database and ping it
		var err error
		connect, err = sql.Open("clickhouse", url)
		if err != nil {
			connErr = fmt.Errorf("error when opening DB connection: %v", err)
			return false, nil
		}
		if err := connect.Ping(); err != nil {
			if exception, ok := err.(*clickhouse.Exception); ok {
				connErr = fmt.Errorf("failed to ping ClickHouse: %v", exception.Message)
			} else {
				connErr = fmt.Errorf("failed to ping ClickHouse: %v", err)
			}
			return false, nil
		} else {
			return true, nil
		}
	}); err != nil {
		return nil, fmt.Errorf("failed to connect to ClickHouse after %s: %v", connTimeout, connErr)
	}
	return connect, nil
}
