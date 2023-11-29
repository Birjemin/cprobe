package exporter

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"github.com/cprobe/cprobe/lib/logger"
	"github.com/krallistic/kazoo-go"
	"github.com/rcrowley/go-metrics"
	"io/ioutil"
	"k8s.io/klog/v2"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	kingpin "github.com/alecthomas/kingpin/v2"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/version"
)

const (
	namespace = "kafka"
	clientID  = "kafka_exporter"
)

const (
	INFO  = 0
	DEBUG = 1
	TRACE = 2
)

var (
	clusterBrokers                     *prometheus.Desc
	clusterBrokerInfo                  *prometheus.Desc
	topicPartitions                    *prometheus.Desc
	topicCurrentOffset                 *prometheus.Desc
	topicOldestOffset                  *prometheus.Desc
	topicPartitionLeader               *prometheus.Desc
	topicPartitionReplicas             *prometheus.Desc
	topicPartitionInSyncReplicas       *prometheus.Desc
	topicPartitionUsesPreferredReplica *prometheus.Desc
	topicUnderReplicatedPartition      *prometheus.Desc
	consumergroupCurrentOffset         *prometheus.Desc
	consumergroupCurrentOffsetSum      *prometheus.Desc
	consumergroupLag                   *prometheus.Desc
	consumergroupLagSum                *prometheus.Desc
	consumergroupLagZookeeper          *prometheus.Desc
	consumergroupMembers               *prometheus.Desc
)

// Exporter collects Kafka stats from the given server and exports them using
// the prometheus metrics package.
type Exporter struct {
	client                  sarama.Client
	topicFilter             *regexp.Regexp
	topicExclude            *regexp.Regexp
	groupFilter             *regexp.Regexp
	groupExclude            *regexp.Regexp
	mu                      sync.Mutex
	useZooKeeperLag         bool
	zookeeperClient         *kazoo.Kazoo
	nextMetadataRefresh     time.Time
	metadataRefreshInterval time.Duration
	offsetShowAll           bool
	topicWorkers            int
	allowConcurrent         bool
	sgMutex                 sync.Mutex
	sgWaitCh                chan struct{}
	sgChans                 []chan<- prometheus.Metric
	consumerGroupFetchAll   bool
}

type KafkaOpts struct {
	Uri                      []string
	UseSASL                  bool
	UseSASLHandshake         bool
	SaslUsername             string
	SaslPassword             string
	SaslMechanism            string
	SaslDisablePAFXFast      bool
	UseTLS                   bool
	TlsServerName            string
	TlsCAFile                string
	TlsCertFile              string
	TlsKeyFile               string
	ServerUseTLS             bool
	ServerMutualAuthEnabled  bool
	ServerTlsCAFile          string
	ServerTlsCertFile        string
	ServerTlsKeyFile         string
	TlsInsecureSkipTLSVerify bool
	KafkaVersion             string
	UseZooKeeperLag          bool
	UriZookeeper             []string
	Labels                   string
	MetadataRefreshInterval  string
	ServiceName              string
	KerberosConfigPath       string
	Realm                    string
	KeyTabPath               string
	KerberosAuthType         string
	OffsetShowAll            bool
	TopicWorkers             int
	AllowConcurrent          bool
	AllowAutoTopicCreation   bool
	VerbosityLogLevel        int
}

// CanReadCertAndKey returns true if the certificate and key files already exists,
// otherwise returns false. If lost one of cert and key, returns error.
func CanReadCertAndKey(certPath, keyPath string) (bool, error) {
	certReadable := canReadFile(certPath)
	keyReadable := canReadFile(keyPath)

	if certReadable == false && keyReadable == false {
		return false, nil
	}

	if certReadable == false {
		return false, fmt.Errorf("error reading %s, certificate and key must be supplied as a pair", certPath)
	}

	if keyReadable == false {
		return false, fmt.Errorf("error reading %s, certificate and key must be supplied as a pair", keyPath)
	}

	return true, nil
}

// If the file represented by path exists and
// readable, returns true otherwise returns false.
func canReadFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}

	defer f.Close()

	return true
}

// NewExporter returns an initialized Exporter.
func NewExporter(opts KafkaOpts, topicFilter string, topicExclude string, groupFilter string, groupExclude string) (*Exporter, error) {
	var zookeeperClient *kazoo.Kazoo
	config := sarama.NewConfig()
	config.ClientID = clientID
	kafkaVersion, err := sarama.ParseKafkaVersion(opts.KafkaVersion)
	if err != nil {
		return nil, err
	}
	config.Version = kafkaVersion

	if opts.UseSASL {
		// Convert to lowercase so that SHA512 and SHA256 is still valid
		opts.SaslMechanism = strings.ToLower(opts.SaslMechanism)
		switch opts.SaslMechanism {
		case "scram-sha512":
			config.Net.SASL.SCRAMClientGeneratorFunc = func() sarama.SCRAMClient { return &XDGSCRAMClient{HashGeneratorFcn: SHA512} }
			config.Net.SASL.Mechanism = sarama.SASLMechanism(sarama.SASLTypeSCRAMSHA512)
		case "scram-sha256":
			config.Net.SASL.SCRAMClientGeneratorFunc = func() sarama.SCRAMClient { return &XDGSCRAMClient{HashGeneratorFcn: SHA256} }
			config.Net.SASL.Mechanism = sarama.SASLMechanism(sarama.SASLTypeSCRAMSHA256)
		case "gssapi":
			config.Net.SASL.Mechanism = sarama.SASLMechanism(sarama.SASLTypeGSSAPI)
			config.Net.SASL.GSSAPI.ServiceName = opts.ServiceName
			config.Net.SASL.GSSAPI.KerberosConfigPath = opts.KerberosConfigPath
			config.Net.SASL.GSSAPI.Realm = opts.Realm
			config.Net.SASL.GSSAPI.Username = opts.SaslUsername
			if opts.KerberosAuthType == "keytabAuth" {
				config.Net.SASL.GSSAPI.AuthType = sarama.KRB5_KEYTAB_AUTH
				config.Net.SASL.GSSAPI.KeyTabPath = opts.KeyTabPath
			} else {
				config.Net.SASL.GSSAPI.AuthType = sarama.KRB5_USER_AUTH
				config.Net.SASL.GSSAPI.Password = opts.SaslPassword
			}
			if opts.SaslDisablePAFXFast {
				config.Net.SASL.GSSAPI.DisablePAFXFAST = true
			}
		case "plain":
		default:
			return nil, fmt.Errorf(
				`invalid sasl mechanism "%s": can only be "scram-sha256", "scram-sha512", "gssapi" or "plain"`,
				opts.SaslMechanism,
			)
		}

		config.Net.SASL.Enable = true
		config.Net.SASL.Handshake = opts.UseSASLHandshake

		if opts.SaslUsername != "" {
			config.Net.SASL.User = opts.SaslUsername
		}

		if opts.SaslPassword != "" {
			config.Net.SASL.Password = opts.SaslPassword
		}
	}

	if opts.UseTLS {
		config.Net.TLS.Enable = true

		config.Net.TLS.Config = &tls.Config{
			ServerName:         opts.TlsServerName,
			InsecureSkipVerify: opts.TlsInsecureSkipTLSVerify,
		}

		if opts.TlsCAFile != "" {
			if ca, err := ioutil.ReadFile(opts.TlsCAFile); err == nil {
				config.Net.TLS.Config.RootCAs = x509.NewCertPool()
				config.Net.TLS.Config.RootCAs.AppendCertsFromPEM(ca)
			} else {
				return nil, err
			}
		}

		canReadCertAndKey, err := CanReadCertAndKey(opts.TlsCertFile, opts.TlsKeyFile)
		if err != nil {
			return nil, errors.Wrap(err, "error reading cert and key")
		}
		if canReadCertAndKey {
			cert, err := tls.LoadX509KeyPair(opts.TlsCertFile, opts.TlsKeyFile)
			if err == nil {
				config.Net.TLS.Config.Certificates = []tls.Certificate{cert}
			} else {
				return nil, err
			}
		}
	}

	if opts.UseZooKeeperLag {
		klog.V(DEBUG).Infoln("Using zookeeper lag, so connecting to zookeeper")
		zookeeperClient, err = kazoo.NewKazoo(opts.UriZookeeper, nil)
		if err != nil {
			return nil, errors.Wrap(err, "error connecting to zookeeper")
		}
	}

	interval, err := time.ParseDuration(opts.MetadataRefreshInterval)
	if err != nil {
		return nil, errors.Wrap(err, "Cannot parse metadata refresh interval")
	}

	config.Metadata.RefreshFrequency = interval

	config.Metadata.AllowAutoTopicCreation = opts.AllowAutoTopicCreation

	client, err := sarama.NewClient(opts.Uri, config)

	if err != nil {
		return nil, errors.Wrap(err, "Error Init Kafka Client")
	}

	logger.Infof("Done Init Clients")
	// Init our exporter.
	return &Exporter{
		client:                  client,
		topicFilter:             regexp.MustCompile(topicFilter),
		topicExclude:            regexp.MustCompile(topicExclude),
		groupFilter:             regexp.MustCompile(groupFilter),
		groupExclude:            regexp.MustCompile(groupExclude),
		useZooKeeperLag:         opts.UseZooKeeperLag,
		zookeeperClient:         zookeeperClient,
		nextMetadataRefresh:     time.Now(),
		metadataRefreshInterval: interval,
		offsetShowAll:           opts.OffsetShowAll,
		topicWorkers:            opts.TopicWorkers,
		allowConcurrent:         opts.AllowConcurrent,
		sgMutex:                 sync.Mutex{},
		sgWaitCh:                nil,
		sgChans:                 []chan<- prometheus.Metric{},
		consumerGroupFetchAll:   config.Version.IsAtLeast(sarama.V2_0_0_0),
	}, nil
}

//func (e *Exporter) fetchOffsetVersion() int16 {
//	version := e.client.Config().Version
//	if e.client.Config().Version.IsAtLeast(sarama.V2_0_0_0) {
//		return 4
//	} else if version.IsAtLeast(sarama.V0_10_2_0) {
//		return 2
//	} else if version.IsAtLeast(sarama.V0_8_2_2) {
//		return 1
//	}
//	return 0
//}

// Describe describes all the metrics ever exported by the Kafka exporter. It
// implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- clusterBrokers
	ch <- topicCurrentOffset
	ch <- topicOldestOffset
	ch <- topicPartitions
	ch <- topicPartitionLeader
	ch <- topicPartitionReplicas
	ch <- topicPartitionInSyncReplicas
	ch <- topicPartitionUsesPreferredReplica
	ch <- topicUnderReplicatedPartition
	ch <- consumergroupCurrentOffset
	ch <- consumergroupCurrentOffsetSum
	ch <- consumergroupLag
	ch <- consumergroupLagZookeeper
	ch <- consumergroupLagSum
}

// Collect fetches the stats from configured Kafka location and delivers them
// as Prometheus metrics. It implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	if e.allowConcurrent {
		e.collect(ch)
		return
	}
	// Locking to avoid race add
	e.sgMutex.Lock()
	e.sgChans = append(e.sgChans, ch)
	// Safe to compare length since we own the Lock
	if len(e.sgChans) == 1 {
		e.sgWaitCh = make(chan struct{})
		go e.collectChans(e.sgWaitCh)
	} else {
		logger.Infof("concurrent calls detected, waiting for first to finish")
	}
	// Put in another variable to ensure not overwriting it in another Collect once we wait
	waiter := e.sgWaitCh
	e.sgMutex.Unlock()
	// Released lock, we have insurance that our chan will be part of the collectChan slice
	<-waiter
	// collectChan finished
}

func (e *Exporter) collectChans(quit chan struct{}) {
	original := make(chan prometheus.Metric)
	container := make([]prometheus.Metric, 0, 100)
	go func() {
		for metric := range original {
			container = append(container, metric)
		}
	}()
	e.collect(original)
	close(original)
	// Lock to avoid modification on the channel slice
	e.sgMutex.Lock()
	for _, ch := range e.sgChans {
		for _, metric := range container {
			ch <- metric
		}
	}
	// Reset the slice
	e.sgChans = e.sgChans[:0]
	// Notify remaining waiting Collect they can return
	close(quit)
	// Release the lock so Collect can append to the slice again
	e.sgMutex.Unlock()
}

func (e *Exporter) collect(ch chan<- prometheus.Metric) {
	var wg = sync.WaitGroup{}
	ch <- prometheus.MustNewConstMetric(
		clusterBrokers, prometheus.GaugeValue, float64(len(e.client.Brokers())),
	)
	for _, b := range e.client.Brokers() {
		ch <- prometheus.MustNewConstMetric(
			clusterBrokerInfo, prometheus.GaugeValue, 1, strconv.Itoa(int(b.ID())), b.Addr(),
		)
	}

	offset := make(map[string]map[int32]int64)

	now := time.Now()

	if now.After(e.nextMetadataRefresh) {
		klog.V(DEBUG).Info("Refreshing client metadata")

		if err := e.client.RefreshMetadata(); err != nil {
			klog.Errorf("Cannot refresh topics, using cached data: %v", err)
		}

		e.nextMetadataRefresh = now.Add(e.metadataRefreshInterval)
	}

	topics, err := e.client.Topics()
	logger.Infof("kafka topics: ", topics)
	if err != nil {
		klog.Errorf("Cannot get topics: %v", err)
		return
	}

	topicChannel := make(chan string)

	getTopicMetrics := func(topic string) {
		defer wg.Done()
		logger.Infof("开始获取topic指标", topic)

		if !e.topicFilter.MatchString(topic) || e.topicExclude.MatchString(topic) {
			return
		}

		partitions, err := e.client.Partitions(topic)
		logger.Infof("the partition of topics", partitions, topics)
		if err != nil {
			klog.Errorf("Cannot get partitions of topic %s: %v", topic, err)
			return
		}
		ch <- prometheus.MustNewConstMetric(
			topicPartitions, prometheus.GaugeValue, float64(len(partitions)), topic,
		)
		e.mu.Lock()
		offset[topic] = make(map[int32]int64, len(partitions))
		e.mu.Unlock()
		for _, partition := range partitions {
			broker, err := e.client.Leader(topic, partition)
			if err != nil {
				klog.Errorf("Cannot get leader of topic %s partition %d: %v", topic, partition, err)
			} else {
				ch <- prometheus.MustNewConstMetric(
					topicPartitionLeader, prometheus.GaugeValue, float64(broker.ID()), topic, strconv.FormatInt(int64(partition), 10),
				)
			}

			currentOffset, err := e.client.GetOffset(topic, partition, sarama.OffsetNewest)
			if err != nil {
				klog.Errorf("Cannot get current offset of topic %s partition %d: %v", topic, partition, err)
			} else {
				e.mu.Lock()
				offset[topic][partition] = currentOffset
				e.mu.Unlock()
				ch <- prometheus.MustNewConstMetric(
					topicCurrentOffset, prometheus.GaugeValue, float64(currentOffset), topic, strconv.FormatInt(int64(partition), 10),
				)
			}

			oldestOffset, err := e.client.GetOffset(topic, partition, sarama.OffsetOldest)
			if err != nil {
				klog.Errorf("Cannot get oldest offset of topic %s partition %d: %v", topic, partition, err)
			} else {
				ch <- prometheus.MustNewConstMetric(
					topicOldestOffset, prometheus.GaugeValue, float64(oldestOffset), topic, strconv.FormatInt(int64(partition), 10),
				)
			}

			replicas, err := e.client.Replicas(topic, partition)
			if err != nil {
				klog.Errorf("Cannot get replicas of topic %s partition %d: %v", topic, partition, err)
			} else {
				ch <- prometheus.MustNewConstMetric(
					topicPartitionReplicas, prometheus.GaugeValue, float64(len(replicas)), topic, strconv.FormatInt(int64(partition), 10),
				)
			}

			inSyncReplicas, err := e.client.InSyncReplicas(topic, partition)
			if err != nil {
				klog.Errorf("Cannot get in-sync replicas of topic %s partition %d: %v", topic, partition, err)
			} else {
				ch <- prometheus.MustNewConstMetric(
					topicPartitionInSyncReplicas, prometheus.GaugeValue, float64(len(inSyncReplicas)), topic, strconv.FormatInt(int64(partition), 10),
				)
			}

			if broker != nil && replicas != nil && len(replicas) > 0 && broker.ID() == replicas[0] {
				ch <- prometheus.MustNewConstMetric(
					topicPartitionUsesPreferredReplica, prometheus.GaugeValue, float64(1), topic, strconv.FormatInt(int64(partition), 10),
				)
			} else {
				ch <- prometheus.MustNewConstMetric(
					topicPartitionUsesPreferredReplica, prometheus.GaugeValue, float64(0), topic, strconv.FormatInt(int64(partition), 10),
				)
			}

			if replicas != nil && inSyncReplicas != nil && len(inSyncReplicas) < len(replicas) {
				ch <- prometheus.MustNewConstMetric(
					topicUnderReplicatedPartition, prometheus.GaugeValue, float64(1), topic, strconv.FormatInt(int64(partition), 10),
				)
			} else {
				ch <- prometheus.MustNewConstMetric(
					topicUnderReplicatedPartition, prometheus.GaugeValue, float64(0), topic, strconv.FormatInt(int64(partition), 10),
				)
			}

			if e.useZooKeeperLag {
				ConsumerGroups, err := e.zookeeperClient.Consumergroups()

				if err != nil {
					klog.Errorf("Cannot get consumer group %v", err)
				}

				for _, group := range ConsumerGroups {
					offset, _ := group.FetchOffset(topic, partition)
					if offset > 0 {

						consumerGroupLag := currentOffset - offset
						ch <- prometheus.MustNewConstMetric(
							consumergroupLagZookeeper, prometheus.GaugeValue, float64(consumerGroupLag), group.Name, topic, strconv.FormatInt(int64(partition), 10),
						)
					}
				}
			}
		}
	}

	loopTopics := func() {
		ok := true
		for ok {
			topic, open := <-topicChannel
			logger.Warnf("open", open)
			ok = open
			if open {
				getTopicMetrics(topic)
			}
		}
	}

	minx := func(x int, y int) int {
		if x < y {
			return x
		} else {
			return y
		}
	}

	N := len(topics)
	if N > 1 {
		N = minx(N/2, e.topicWorkers)
	}

	for w := 1; w <= N; w++ {
		logger.Infof("准备获取topic指标")
		go loopTopics()
	}

	for _, topic := range topics {
		if e.topicFilter.MatchString(topic) && !e.topicExclude.MatchString(topic) {
			wg.Add(1)
			topicChannel <- topic
		}
	}
	close(topicChannel)

	wg.Wait()

	getConsumerGroupMetrics := func(broker *sarama.Broker) {
		defer wg.Done()
		if err := broker.Open(e.client.Config()); err != nil && err != sarama.ErrAlreadyConnected {
			klog.Errorf("Cannot connect to broker %d: %v", broker.ID(), err)
			return
		}
		defer broker.Close()

		groups, err := broker.ListGroups(&sarama.ListGroupsRequest{})
		if err != nil {
			klog.Errorf("Cannot get consumer group: %v", err)
			return
		}
		groupIds := make([]string, 0)
		for groupId := range groups.Groups {
			if e.groupFilter.MatchString(groupId) && !e.groupExclude.MatchString(groupId) {
				groupIds = append(groupIds, groupId)
			}
		}

		describeGroups, err := broker.DescribeGroups(&sarama.DescribeGroupsRequest{Groups: groupIds})
		if err != nil {
			klog.Errorf("Cannot get describe groups: %v", err)
			return
		}
		for _, group := range describeGroups.Groups {
			offsetFetchRequest := sarama.OffsetFetchRequest{ConsumerGroup: group.GroupId, Version: 1}
			if e.offsetShowAll {
				for topic, partitions := range offset {
					for partition := range partitions {
						offsetFetchRequest.AddPartition(topic, partition)
					}
				}
			} else {
				for _, member := range group.Members {
					assignment, err := member.GetMemberAssignment()
					if err != nil {
						klog.Errorf("Cannot get GetMemberAssignment of group member %v : %v", member, err)
						return
					}
					for topic, partions := range assignment.Topics {
						for _, partition := range partions {
							offsetFetchRequest.AddPartition(topic, partition)
						}
					}
				}
			}
			ch <- prometheus.MustNewConstMetric(
				consumergroupMembers, prometheus.GaugeValue, float64(len(group.Members)), group.GroupId,
			)
			offsetFetchResponse, err := broker.FetchOffset(&offsetFetchRequest)
			if err != nil {
				klog.Errorf("Cannot get offset of group %s: %v", group.GroupId, err)
				continue
			}

			for topic, partitions := range offsetFetchResponse.Blocks {
				// If the topic is not consumed by that consumer group, skip it
				topicConsumed := false
				for _, offsetFetchResponseBlock := range partitions {
					// Kafka will return -1 if there is no offset associated with a topic-partition under that consumer group
					if offsetFetchResponseBlock.Offset != -1 {
						topicConsumed = true
						break
					}
				}
				if !topicConsumed {
					continue
				}

				var currentOffsetSum int64
				var lagSum int64
				for partition, offsetFetchResponseBlock := range partitions {
					err := offsetFetchResponseBlock.Err
					if err != sarama.ErrNoError {
						klog.Errorf("Error for  partition %d :%v", partition, err.Error())
						continue
					}
					currentOffset := offsetFetchResponseBlock.Offset
					currentOffsetSum += currentOffset
					ch <- prometheus.MustNewConstMetric(
						consumergroupCurrentOffset, prometheus.GaugeValue, float64(currentOffset), group.GroupId, topic, strconv.FormatInt(int64(partition), 10),
					)
					e.mu.Lock()
					if offset, ok := offset[topic][partition]; ok {
						// If the topic is consumed by that consumer group, but no offset associated with the partition
						// forcing lag to -1 to be able to alert on that
						var lag int64
						if offsetFetchResponseBlock.Offset == -1 {
							lag = -1
						} else {
							lag = offset - offsetFetchResponseBlock.Offset
							lagSum += lag
						}
						ch <- prometheus.MustNewConstMetric(
							consumergroupLag, prometheus.GaugeValue, float64(lag), group.GroupId, topic, strconv.FormatInt(int64(partition), 10),
						)
					} else {
						klog.Errorf("No offset of topic %s partition %d, cannot get consumer group lag", topic, partition)
					}
					e.mu.Unlock()
				}
				ch <- prometheus.MustNewConstMetric(
					consumergroupCurrentOffsetSum, prometheus.GaugeValue, float64(currentOffsetSum), group.GroupId, topic,
				)
				ch <- prometheus.MustNewConstMetric(
					consumergroupLagSum, prometheus.GaugeValue, float64(lagSum), group.GroupId, topic,
				)
			}
		}
	}

	klog.V(DEBUG).Info("Fetching consumer group metrics")
	if len(e.client.Brokers()) > 0 {
		for _, broker := range e.client.Brokers() {
			wg.Add(1)
			go getConsumerGroupMetrics(broker)
		}
		wg.Wait()
	} else {
		klog.Errorln("No valid broker, cannot get consumer group metrics")
	}
}

func init() {
	metrics.UseNilMetrics = true
	prometheus.MustRegister(version.NewCollector("kafka_exporter"))
}

//func toFlag(name string, help string) *kingpin.FlagClause {
//	flag.CommandLine.String(name, "", help) // hack around flag.Parse and klog.init flags
//	return kingpin.Flag(name, help)
//}

// hack around flag.Parse and klog.init flags
func toFlagString(name string, help string, value string) *string {
	flag.CommandLine.String(name, value, help) // hack around flag.Parse and klog.init flags
	return kingpin.Flag(name, help).Default(value).String()
}

func toFlagBool(name string, help string, value bool, valueString string) *bool {
	flag.CommandLine.Bool(name, value, help) // hack around flag.Parse and klog.init flags
	return kingpin.Flag(name, help).Default(valueString).Bool()
}

func toFlagStringsVar(name string, help string, value string, target *[]string) {
	flag.CommandLine.String(name, value, help) // hack around flag.Parse and klog.init flags
	kingpin.Flag(name, help).Default(value).StringsVar(target)
}

func toFlagStringVar(name string, help string, value string, target *string) {
	flag.CommandLine.String(name, value, help) // hack around flag.Parse and klog.init flags
	kingpin.Flag(name, help).Default(value).StringVar(target)
}

func toFlagBoolVar(name string, help string, value bool, valueString string, target *bool) {
	flag.CommandLine.Bool(name, value, help) // hack around flag.Parse and klog.init flags
	kingpin.Flag(name, help).Default(valueString).BoolVar(target)
}

func toFlagIntVar(name string, help string, value int, valueString string, target *int) {
	flag.CommandLine.Int(name, value, help) // hack around flag.Parse and klog.init flags
	kingpin.Flag(name, help).Default(valueString).IntVar(target)
}

func Setup(topicFilter string, topicExclude string, groupFilter string, groupExclude string, opts KafkaOpts, labels map[string]string) (*Exporter, error) {

	logger.Infof("Starting kafka_exporter", version.Info())
	logger.Infof("Build context", version.BuildContext())
	clusterBrokers = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "brokers"),
		"Number of Brokers in the Kafka Cluster.",
		nil, labels,
	)
	clusterBrokerInfo = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "broker_info"),
		"Information about the Kafka Broker.",
		[]string{"id", "address"}, labels,
	)
	topicPartitions = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "topic", "partitions"),
		"Number of partitions for this Topic",
		[]string{"topic"}, labels,
	)
	topicCurrentOffset = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "topic", "partition_current_offset"),
		"Current Offset of a Broker at Topic/Partition",
		[]string{"topic", "partition"}, labels,
	)
	topicOldestOffset = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "topic", "partition_oldest_offset"),
		"Oldest Offset of a Broker at Topic/Partition",
		[]string{"topic", "partition"}, labels,
	)

	topicPartitionLeader = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "topic", "partition_leader"),
		"Leader Broker ID of this Topic/Partition",
		[]string{"topic", "partition"}, labels,
	)

	topicPartitionReplicas = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "topic", "partition_replicas"),
		"Number of Replicas for this Topic/Partition",
		[]string{"topic", "partition"}, labels,
	)

	topicPartitionInSyncReplicas = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "topic", "partition_in_sync_replica"),
		"Number of In-Sync Replicas for this Topic/Partition",
		[]string{"topic", "partition"}, labels,
	)

	topicPartitionUsesPreferredReplica = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "topic", "partition_leader_is_preferred"),
		"1 if Topic/Partition is using the Preferred Broker",
		[]string{"topic", "partition"}, labels,
	)

	topicUnderReplicatedPartition = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "topic", "partition_under_replicated_partition"),
		"1 if Topic/Partition is under Replicated",
		[]string{"topic", "partition"}, labels,
	)

	consumergroupCurrentOffset = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "consumergroup", "current_offset"),
		"Current Offset of a ConsumerGroup at Topic/Partition",
		[]string{"consumergroup", "topic", "partition"}, labels,
	)

	consumergroupCurrentOffsetSum = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "consumergroup", "current_offset_sum"),
		"Current Offset of a ConsumerGroup at Topic for all partitions",
		[]string{"consumergroup", "topic"}, labels,
	)

	consumergroupLag = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "consumergroup", "lag"),
		"Current Approximate Lag of a ConsumerGroup at Topic/Partition",
		[]string{"consumergroup", "topic", "partition"}, labels,
	)

	consumergroupLagZookeeper = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "consumergroupzookeeper", "lag_zookeeper"),
		"Current Approximate Lag(zookeeper) of a ConsumerGroup at Topic/Partition",
		[]string{"consumergroup", "topic", "partition"}, nil,
	)

	consumergroupLagSum = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "consumergroup", "lag_sum"),
		"Current Approximate Lag of a ConsumerGroup at Topic for all partitions",
		[]string{"consumergroup", "topic"}, labels,
	)

	consumergroupMembers = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "consumergroup", "members"),
		"Amount of members in a consumer group",
		[]string{"consumergroup"}, labels,
	)
	exp, err := NewExporter(opts, topicFilter, topicExclude, groupFilter, groupExclude)
	//if err != nil {
	//	logger.Errorf("Get Exporter error: %s", err.Error())
	//}

	//prometheus.MustRegister(exp)
	return exp, err
}
