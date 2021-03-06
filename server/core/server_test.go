/*
 * Copyright 2019-2020 The NATS Authors
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package core

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats-kafka/server/conf"
	gnatsserver "github.com/nats-io/nats-server/v2/server"
	gnatsd "github.com/nats-io/nats-server/v2/test"
	nss "github.com/nats-io/nats-streaming-server/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"
	"github.com/nats-io/stan.go"
	"github.com/segmentio/kafka-go"
)

const (
	serverCert = "../../resources/certs/server-cert.pem"
	serverKey  = "../../resources/certs/server-key.pem"
	clientCert = "../../resources/certs/client-cert.pem"
	clientKey  = "../../resources/certs/client-key.pem"
	caFile     = "../../resources/certs/truststore.pem"
)

// TestEnv encapsulate a bridge test environment
type TestEnv struct {
	Config        *conf.NATSKafkaBridgeConfig
	Gnatsd        *gnatsserver.Server
	Stan          *nss.StanServer
	KafkaHostPort string

	NC *nats.Conn // for bypassing the bridge
	SC stan.Conn  // for bypassing the bridge

	natsPort       int
	natsURL        string
	clusterName    string
	clientID       string // we keep this so we stay the same on reconnect
	bridgeClientID string

	Bridge *NATSKafkaBridge

	useTLS bool
}

func collectTopics(connections []conf.ConnectorConfig) []string {
	topicSet := map[string]string{}
	topics := []string{}

	for _, c := range connections {
		if c.Topic != "" {
			topicSet[c.Topic] = c.Topic
		}
	}

	for t := range topicSet {
		topics = append(topics, t)
	}
	return topics
}

// StartTestEnvironment calls StartTestEnvironmentInfrastructure
// followed by StartBridge
func StartTestEnvironment(connections []conf.ConnectorConfig) (*TestEnv, error) {
	tbs, err := StartTestEnvironmentInfrastructure(false, collectTopics(connections))
	if err != nil {
		return nil, err
	}
	err = tbs.StartBridge(connections)
	if err != nil {
		tbs.Close()
		return nil, err
	}
	return tbs, err
}

// StartTLSTestEnvironment calls StartTestEnvironmentInfrastructure
// followed by StartBridge, with TLS enabled
func StartTLSTestEnvironment(connections []conf.ConnectorConfig) (*TestEnv, error) {
	tbs, err := StartTestEnvironmentInfrastructure(true, collectTopics(connections))
	if err != nil {
		return nil, err
	}
	err = tbs.StartBridge(connections)
	if err != nil {
		tbs.Close()
		return nil, err
	}
	return tbs, err
}

// StartTestEnvironmentInfrastructure creates the kafka server, Nats and streaming
// but does not start a bridge, you can use StartBridge to start a bridge afterward
func StartTestEnvironmentInfrastructure(useTLS bool, topics []string) (*TestEnv, error) {
	tbs := &TestEnv{}
	tbs.useTLS = useTLS

	tbs.KafkaHostPort = "localhost:9092"

	if tbs.useTLS {
		tbs.KafkaHostPort = "localhost:9093"
	}

	err := tbs.CheckKafka(5000)

	if err != nil {
		tbs.Close()
		return nil, err
	}

	for _, t := range topics {
		err := tbs.CreateTopic(t, 5000)
		if err != nil {
			tbs.Close()
			return nil, err
		}
	}

	err = tbs.StartNATSandStan(-1, nuid.Next(), nuid.Next(), nuid.Next())
	if err != nil {
		tbs.Close()
		return nil, err
	}

	return tbs, nil
}

// StartBridge is the second half of StartTestEnvironment
// it is provided separately so that environment can be created before the bridge runs
func (tbs *TestEnv) StartBridge(connections []conf.ConnectorConfig) error {
	config := conf.DefaultBridgeConfig()
	config.Logging.Debug = true
	config.Logging.Trace = true
	config.Logging.Colors = false
	config.Monitoring = conf.HTTPConfig{
		HTTPPort: -1,
	}
	config.NATS = conf.NATSConfig{
		Servers:        []string{tbs.natsURL},
		ConnectTimeout: 2000,
		ReconnectWait:  2000,
		MaxReconnects:  5,
	}
	config.STAN = conf.NATSStreamingConfig{
		ClusterID:          tbs.clusterName,
		ClientID:           tbs.bridgeClientID,
		PubAckWait:         5000,
		DiscoverPrefix:     stan.DefaultDiscoverPrefix,
		MaxPubAcksInflight: stan.DefaultMaxPubAcksInflight,
		ConnectWait:        2000,
	}

	if tbs.useTLS {
		config.Monitoring.HTTPPort = 0
		config.Monitoring.HTTPSPort = -1

		config.Monitoring.TLS = conf.TLSConf{
			Cert: serverCert,
			Key:  serverKey,
		}

		config.NATS.TLS = conf.TLSConf{
			Root: caFile,
		}
	}

	for i, c := range connections {
		c.Brokers = []string{tbs.KafkaHostPort}

		if tbs.useTLS {
			c.TLS = conf.TLSConf{
				Cert: clientCert,
				Key:  clientKey,
				Root: caFile,
			}
		}

		connections[i] = c
	}

	config.Connect = connections

	tbs.Config = &config
	tbs.Bridge = NewNATSKafkaBridge()
	err := tbs.Bridge.InitializeFromConfig(config)
	if err != nil {
		tbs.Close()
		return err
	}
	err = tbs.Bridge.Start()
	if err != nil {
		tbs.Close()
		return err
	}

	return nil
}

// StartNATSandStan starts up the nats and stan servers
func (tbs *TestEnv) StartNATSandStan(port int, clusterID string, clientID string, bridgeClientID string) error {
	var err error
	opts := gnatsd.DefaultTestOptions
	opts.Port = port

	if tbs.useTLS {
		opts.TLSCert = serverCert
		opts.TLSKey = serverKey
		opts.TLSTimeout = 5

		tc := gnatsserver.TLSConfigOpts{}
		tc.CertFile = opts.TLSCert
		tc.KeyFile = opts.TLSKey

		opts.TLSConfig, err = gnatsserver.GenTLSConfig(&tc)

		if err != nil {
			return err
		}
	}
	tbs.Gnatsd = gnatsd.RunServer(&opts)

	if tbs.useTLS {
		tbs.natsURL = fmt.Sprintf("tls://localhost:%d", opts.Port)
	} else {
		tbs.natsURL = fmt.Sprintf("nats://localhost:%d", opts.Port)
	}

	tbs.natsPort = opts.Port
	tbs.clusterName = clusterID
	sOpts := nss.GetDefaultOptions()
	sOpts.ID = tbs.clusterName
	sOpts.NATSServerURL = tbs.natsURL

	if tbs.useTLS {
		sOpts.ClientCA = caFile
	}

	nOpts := nss.DefaultNatsServerOptions
	nOpts.Port = -1

	s, err := nss.RunServerWithOpts(sOpts, &nOpts)
	if err != nil {
		return err
	}

	tbs.Stan = s
	tbs.clientID = clientID
	tbs.bridgeClientID = bridgeClientID

	var nc *nats.Conn

	if tbs.useTLS {
		nc, err = nats.Connect(tbs.natsURL, nats.RootCAs(caFile))
	} else {
		nc, err = nats.Connect(tbs.natsURL)
	}

	if err != nil {
		return err
	}

	tbs.NC = nc

	sc, err := stan.Connect(tbs.clusterName, tbs.clientID, stan.NatsConn(tbs.NC))
	if err != nil {
		return err
	}
	tbs.SC = sc

	return nil
}

// StopBridge stops the bridge
func (tbs *TestEnv) StopBridge() {
	if tbs.Bridge != nil {
		tbs.Bridge.Stop()
		tbs.Bridge = nil
	}
}

// StopNATS shuts down the NATS and Stan servers
func (tbs *TestEnv) StopNATS() error {
	if tbs.SC != nil {
		tbs.SC.Close()
	}

	if tbs.NC != nil {
		tbs.NC.Close()
	}

	if tbs.Stan != nil {
		tbs.Stan.Shutdown()
	}

	if tbs.Gnatsd != nil {
		tbs.Gnatsd.Shutdown()
	}

	return nil
}

// RestartNATS shuts down the NATS and stan server and then starts it again
func (tbs *TestEnv) RestartNATS() error {
	if tbs.SC != nil {
		tbs.SC.Close()
	}

	if tbs.NC != nil {
		tbs.NC.Close()
	}

	if tbs.Stan != nil {
		tbs.Stan.Shutdown()
	}

	if tbs.Gnatsd != nil {
		tbs.Gnatsd.Shutdown()
	}

	err := tbs.StartNATSandStan(tbs.natsPort, tbs.clusterName, tbs.clientID, tbs.bridgeClientID)
	if err != nil {
		return err
	}

	return nil
}

// Close the bridge server and clean up the test environment
func (tbs *TestEnv) Close() {
	// Stop the bridge first!
	if tbs.Bridge != nil {
		tbs.Bridge.Stop()
	}

	if tbs.SC != nil {
		tbs.SC.Close()
	}

	if tbs.NC != nil {
		tbs.NC.Close()
	}

	if tbs.Stan != nil {
		tbs.Stan.Shutdown()
	}

	if tbs.Gnatsd != nil {
		tbs.Gnatsd.Shutdown()
	}
}

func (tbs *TestEnv) createDialer(waitMillis int32) (*kafka.Dialer, error) {
	dialer := &kafka.Dialer{
		Timeout:   time.Duration(waitMillis) * time.Millisecond,
		DualStack: true,
	}

	if tbs.useTLS {
		tlsC := &conf.TLSConf{
			Cert: clientCert,
			Key:  clientKey,
			Root: caFile,
		}
		tlsCC, err := tlsC.MakeTLSConfig()

		if err != nil {
			return nil, err
		}

		dialer.TLS = tlsCC
	}

	return dialer, nil
}

// SendMessageToKafka puts a message on the kafka topic, bypassing the bridge
func (tbs *TestEnv) SendMessageToKafka(topic string, data []byte, waitMillis int32) error {
	dialer, err := tbs.createDialer(waitMillis)

	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(waitMillis)*time.Millisecond)
	partition := 0
	conn, err := dialer.DialLeader(ctx, "tcp", tbs.KafkaHostPort, topic, partition)
	cancel()

	if err != nil {
		return err
	}

	defer conn.Close()

	err = conn.SetWriteDeadline(time.Now().Add(time.Duration(waitMillis) * time.Millisecond))

	if err != nil {
		return err
	}

	_, err = conn.WriteMessages(
		kafka.Message{Value: []byte(data)},
	)

	return err
}

// CreateReader creates a new reader
func (tbs *TestEnv) CreateReader(topic string, waitMillis int32) *kafka.Reader {
	dialer, err := tbs.createDialer(waitMillis)

	if err != nil {
		return nil
	}

	return kafka.NewReader(kafka.ReaderConfig{
		Brokers:  []string{tbs.KafkaHostPort},
		Topic:    topic,
		GroupID:  "kbt-" + topic,
		MinBytes: 1,
		MaxBytes: 10e3, // 10KB
		Dialer:   dialer,
	})
}

// GetMessageFromKafka uses an extra connection to talk to kafka, bypassing the bridge
func (tbs *TestEnv) GetMessageFromKafka(reader *kafka.Reader, waitMillis int32) ([]byte, []byte, error) {
	context, cancel := context.WithTimeout(context.Background(), time.Duration(waitMillis)*time.Millisecond)
	m, err := reader.ReadMessage(context)
	cancel()

	if err != nil || m.Value == nil {
		return nil, nil, err
	}

	return m.Key, m.Value, nil
}

func (tbs *TestEnv) CreateTopic(topic string, waitMillis int32) error {
	dialer, err := tbs.createDialer(waitMillis)

	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(waitMillis)*time.Millisecond)
	connection, err := dialer.DialContext(ctx, "tcp", tbs.KafkaHostPort)
	cancel() // clean up the context

	if connection == nil || err != nil {
		return fmt.Errorf("unable to connect to kafka server")
	}

	connection.SetDeadline(time.Now().Add(15 * time.Second))
	err = connection.CreateTopics(kafka.TopicConfig{
		Topic:             topic,
		NumPartitions:     1,
		ReplicationFactor: 1,
	})
	connection.Close() // ignore the error

	return err
}

func (tbs *TestEnv) CheckKafka(waitMillis int32) error {
	dialer, err := tbs.createDialer(waitMillis)

	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(waitMillis)*time.Millisecond)
	connection, err := dialer.DialContext(ctx, "tcp", tbs.KafkaHostPort)
	cancel() // clean up the context

	if connection == nil || err != nil {
		return fmt.Errorf("unable to connect to kafka server, %s", err.Error())
	}

	_, err = connection.Controller()

	connection.Close() // ignore the error

	return err
}

func (tbs *TestEnv) WaitForIt(requestCount int64, done chan string) string {
	timeout := time.Duration(5000) * time.Millisecond // 5 second timeout for tests
	stop := time.Now().Add(timeout)
	timer := time.NewTimer(timeout)
	requestsOk := make(chan bool)

	// Timeout the done channel
	go func() {
		<-timer.C
		done <- ""
	}()

	ticker := time.NewTicker(50 * time.Millisecond)
	go func() {
		for t := range ticker.C {
			if t.After(stop) {
				requestsOk <- false
				break
			}

			if tbs.Bridge.SafeStats().RequestCount >= requestCount {
				requestsOk <- true
				break
			}
		}
		ticker.Stop()
	}()

	received := <-done
	ok := <-requestsOk

	if !ok {
		received = ""
	}

	return received
}

func (tbs *TestEnv) WaitForRequests(requestCount int64) {
	timeout := time.Duration(5000) * time.Millisecond // 5 second timeout for tests
	stop := time.Now().Add(timeout)
	requestsOk := make(chan bool)

	ticker := time.NewTicker(50 * time.Millisecond)
	go func() {
		for t := range ticker.C {
			if t.After(stop) {
				requestsOk <- false
				break
			}

			if tbs.Bridge.SafeStats().RequestCount >= requestCount {
				requestsOk <- true
				break
			}
		}
		ticker.Stop()
	}()

	<-requestsOk
}
