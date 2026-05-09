// Go Avro producer + consumer using confluent-kafka-go (CGO/librdkafka) and
// the schemaregistry/serde/avrov2 packages. Schema Registry endpoint is the
// LOCAL CACHING PROXY — schema registration, schema-id lookups, and subject
// queries all flow through the proxy.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/confluentinc/confluent-kafka-go/v2/schemaregistry"
	"github.com/confluentinc/confluent-kafka-go/v2/schemaregistry/serde"
	"github.com/confluentinc/confluent-kafka-go/v2/schemaregistry/serde/avrov2"
)

const clientTag = "go"

type TestEvent struct {
	ID     int32  `avro:"id"`
	Msg    string `avro:"msg"`
	Client string `avro:"client"`
}

func main() {
	loadDotenv("../.env")

	bootstrap := mustEnv("BOOTSTRAP_SERVERS")
	apiKey := mustEnv("KAFKA_API_KEY")
	apiSecret := mustEnv("KAFKA_API_SECRET")
	srURL := mustEnv("SR_URL")
	topic := mustEnv("TOPIC")

	schemaPath, _ := filepath.Abs("../schema.avsc")
	schemaBytes, err := os.ReadFile(schemaPath)
	if err != nil {
		log.Fatalf("read schema: %v", err)
	}
	schemaStr := string(schemaBytes)

	// SR client points at local proxy; no auth — proxy injects upstream creds.
	srClient, err := schemaregistry.NewClient(schemaregistry.NewConfig(srURL))
	if err != nil {
		log.Fatalf("sr client: %v", err)
	}

	serCfg := avrov2.NewSerializerConfig()
	serCfg.AutoRegisterSchemas = true
	ser, err := avrov2.NewSerializer(srClient, serde.ValueSerde, serCfg)
	if err != nil {
		log.Fatalf("avro ser: %v", err)
	}
	defer ser.Close()

	deCfg := avrov2.NewDeserializerConfig()
	de, err := avrov2.NewDeserializer(srClient, serde.ValueSerde, deCfg)
	if err != nil {
		log.Fatalf("avro de: %v", err)
	}
	defer de.Close()

	runID := fmt.Sprintf("%d", time.Now().UnixNano()%1_0000_0000)
	if err := produce(bootstrap, apiKey, apiSecret, topic, ser, schemaStr, runID); err != nil {
		log.Fatalf("produce: %v", err)
	}
	if err := consume(bootstrap, apiKey, apiSecret, topic, de, runID); err != nil {
		log.Fatalf("consume: %v", err)
	}
	fmt.Println("[go] OK — Avro SerDes through the proxy succeeded")
}

func produce(bootstrap, key, secret, topic string, ser *avrov2.Serializer, _schemaStr, runID string) error {
	p, err := kafka.NewProducer(&kafka.ConfigMap{
		"bootstrap.servers": bootstrap,
		"security.protocol": "SASL_SSL",
		"sasl.mechanisms":   "PLAIN",
		"sasl.username":     key,
		"sasl.password":     secret,
		"acks":              "all",
	})
	if err != nil {
		return fmt.Errorf("new producer: %w", err)
	}
	defer p.Close()

	deliveryChan := make(chan kafka.Event, 8)

	for i := 0; i < 3; i++ {
		evt := TestEvent{
			ID:     int32(i),
			Msg:    "hello from go (" + runID + ")",
			Client: clientTag,
		}
		val, err := ser.Serialize(topic, &evt)
		if err != nil {
			return fmt.Errorf("serialize: %w", err)
		}
		if err := p.Produce(&kafka.Message{
			TopicPartition: kafka.TopicPartition{Topic: &topic, Partition: kafka.PartitionAny},
			Key:            []byte(fmt.Sprintf("%s-%s-%d", clientTag, runID, i)),
			Value:          val,
		}, deliveryChan); err != nil {
			return fmt.Errorf("produce: %w", err)
		}
	}

	delivered := 0
	for delivered < 3 {
		ev := <-deliveryChan
		msg := ev.(*kafka.Message)
		if msg.TopicPartition.Error != nil {
			return fmt.Errorf("delivery error: %w", msg.TopicPartition.Error)
		}
		delivered++
	}
	close(deliveryChan)
	fmt.Printf("[go] produced 3 messages\n")
	return nil
}

func consume(bootstrap, key, secret, topic string, de *avrov2.Deserializer, runID string) error {
	c, err := kafka.NewConsumer(&kafka.ConfigMap{
		"bootstrap.servers":  bootstrap,
		"security.protocol":  "SASL_SSL",
		"sasl.mechanisms":    "PLAIN",
		"sasl.username":      key,
		"sasl.password":      secret,
		"group.id":           "sr-cache-test-go-" + runID,
		"auto.offset.reset":  "earliest",
		"enable.auto.commit": false,
	})
	if err != nil {
		return fmt.Errorf("new consumer: %w", err)
	}
	defer c.Close()

	if err := c.Subscribe(topic, nil); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	seen := 0
	for time.Now().Before(deadline) && seen < 3 {
		msg, err := c.ReadMessage(2 * time.Second)
		if err != nil {
			var kerr kafka.Error
			if errors.As(err, &kerr) && kerr.IsTimeout() {
				continue
			}
			return fmt.Errorf("read: %w", err)
		}
		var evt TestEvent
		if err := de.DeserializeInto(topic, msg.Value, &evt); err != nil {
			return fmt.Errorf("deserialize: %w", err)
		}
		if evt.Client != clientTag || !strings.Contains(evt.Msg, runID) {
			continue
		}
		fmt.Printf("  key=%s value=%+v\n", string(msg.Key), evt)
		seen++
	}
	if seen < 3 {
		return fmt.Errorf("only saw %d/3 of our run", seen)
	}
	fmt.Printf("[go] consumed %d messages, all decoded correctly\n", seen)
	return nil
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("env var %s is required", k)
	}
	return v
}

func loadDotenv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.TrimSpace(line[eq+1:])
		v = strings.Trim(v, `"'`)
		if _, ok := os.LookupEnv(k); !ok {
			_ = os.Setenv(k, v)
		}
	}
}
