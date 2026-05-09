package com.example.srcache.test;

import io.confluent.kafka.serializers.KafkaAvroDeserializer;
import io.confluent.kafka.serializers.KafkaAvroDeserializerConfig;
import io.confluent.kafka.serializers.KafkaAvroSerializer;
import io.confluent.kafka.serializers.KafkaAvroSerializerConfig;
import org.apache.avro.Schema;
import org.apache.avro.generic.GenericData;
import org.apache.avro.generic.GenericRecord;
import org.apache.kafka.clients.consumer.ConsumerConfig;
import org.apache.kafka.clients.consumer.ConsumerRecord;
import org.apache.kafka.clients.consumer.ConsumerRecords;
import org.apache.kafka.clients.consumer.KafkaConsumer;
import org.apache.kafka.clients.producer.KafkaProducer;
import org.apache.kafka.clients.producer.ProducerConfig;
import org.apache.kafka.clients.producer.ProducerRecord;
import org.apache.kafka.common.errors.WakeupException;
import org.apache.kafka.common.serialization.StringDeserializer;
import org.apache.kafka.common.serialization.StringSerializer;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.time.Duration;
import java.util.Collections;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.Properties;
import java.util.UUID;

/**
 * Java Avro producer + consumer using kafka-clients and kafka-avro-serializer.
 * Schema Registry endpoint is the LOCAL CACHING PROXY — schema registration,
 * schema-id lookups, and subject queries all flow through the proxy.
 */
public class App {

    static final String CLIENT_TAG = "java";

    public static void main(String[] args) throws Exception {
        Path clientsDir = findClientsDir();
        Map<String, String> env = loadDotenv(clientsDir.resolve(".env"));

        String bootstrap = mustGet(env, "BOOTSTRAP_SERVERS");
        String apiKey = mustGet(env, "KAFKA_API_KEY");
        String apiSecret = mustGet(env, "KAFKA_API_SECRET");
        String srUrl = mustGet(env, "SR_URL");
        String topic = mustGet(env, "TOPIC");

        String schemaStr = Files.readString(clientsDir.resolve("schema.avsc"));
        Schema schema = new Schema.Parser().parse(schemaStr);

        String runId = UUID.randomUUID().toString().substring(0, 8);
        produce(bootstrap, apiKey, apiSecret, srUrl, topic, schema, runId);
        consume(bootstrap, apiKey, apiSecret, srUrl, topic, schema, runId);
        System.out.println("[java] OK — Avro SerDes through the proxy succeeded");
    }

    static void produce(String bootstrap, String apiKey, String apiSecret, String srUrl,
                        String topic, Schema schema, String runId) throws Exception {
        Properties props = baseSaslProps(bootstrap, apiKey, apiSecret);
        props.put(ProducerConfig.KEY_SERIALIZER_CLASS_CONFIG, StringSerializer.class.getName());
        props.put(ProducerConfig.VALUE_SERIALIZER_CLASS_CONFIG, KafkaAvroSerializer.class.getName());
        props.put(KafkaAvroSerializerConfig.SCHEMA_REGISTRY_URL_CONFIG, srUrl);
        props.put(KafkaAvroSerializerConfig.AUTO_REGISTER_SCHEMAS, true);
        props.put(ProducerConfig.ACKS_CONFIG, "all");

        try (KafkaProducer<String, GenericRecord> p = new KafkaProducer<>(props)) {
            for (int i = 0; i < 3; i++) {
                GenericRecord rec = new GenericData.Record(schema);
                rec.put("id", i);
                rec.put("msg", "hello from java (" + runId + ")");
                rec.put("client", CLIENT_TAG);
                String key = CLIENT_TAG + "-" + runId + "-" + i;
                ProducerRecord<String, GenericRecord> pr = new ProducerRecord<>(topic, key, rec);
                p.send(pr).get();
            }
        }
        System.out.println("[java] produced 3 messages");
    }

    static void consume(String bootstrap, String apiKey, String apiSecret, String srUrl,
                        String topic, Schema schema, String runId) {
        Properties props = baseSaslProps(bootstrap, apiKey, apiSecret);
        props.put(ConsumerConfig.GROUP_ID_CONFIG, "sr-cache-test-java-" + UUID.randomUUID());
        props.put(ConsumerConfig.AUTO_OFFSET_RESET_CONFIG, "earliest");
        props.put(ConsumerConfig.ENABLE_AUTO_COMMIT_CONFIG, "false");
        props.put(ConsumerConfig.KEY_DESERIALIZER_CLASS_CONFIG, StringDeserializer.class.getName());
        props.put(ConsumerConfig.VALUE_DESERIALIZER_CLASS_CONFIG, KafkaAvroDeserializer.class.getName());
        props.put(KafkaAvroDeserializerConfig.SCHEMA_REGISTRY_URL_CONFIG, srUrl);
        props.put(KafkaAvroDeserializerConfig.SPECIFIC_AVRO_READER_CONFIG, false);

        long deadline = System.currentTimeMillis() + 30_000;
        int seen = 0;
        try (KafkaConsumer<String, GenericRecord> c = new KafkaConsumer<>(props)) {
            c.subscribe(Collections.singletonList(topic));
            while (System.currentTimeMillis() < deadline && seen < 3) {
                ConsumerRecords<String, GenericRecord> recs = c.poll(Duration.ofSeconds(2));
                for (ConsumerRecord<String, GenericRecord> r : recs) {
                    GenericRecord v = r.value();
                    String client = String.valueOf(v.get("client"));
                    String msg = String.valueOf(v.get("msg"));
                    if (!CLIENT_TAG.equals(client) || !msg.contains(runId)) continue;
                    System.out.println("  key=" + r.key() + " value={id=" + v.get("id")
                            + ", msg=\"" + msg + "\", client=" + client + "}");
                    seen++;
                    if (seen >= 3) break;
                }
            }
        } catch (WakeupException ignored) { }

        if (seen < 3) {
            System.err.println("[java] only saw " + seen + "/3 of our run");
            System.exit(3);
        }
        System.out.println("[java] consumed " + seen + " messages, all decoded correctly");
    }

    static Properties baseSaslProps(String bootstrap, String apiKey, String apiSecret) {
        Properties p = new Properties();
        p.put("bootstrap.servers", bootstrap);
        p.put("security.protocol", "SASL_SSL");
        p.put("sasl.mechanism", "PLAIN");
        p.put("sasl.jaas.config",
                "org.apache.kafka.common.security.plain.PlainLoginModule required "
                        + "username=\"" + apiKey + "\" password=\"" + apiSecret + "\";");
        return p;
    }

    static Path findClientsDir() {
        Path cwd = Paths.get(System.getProperty("user.dir"));
        // Walk up looking for a sibling 'clients/.env' or this dir's '../.env'
        Path probe = cwd;
        for (int i = 0; i < 5 && probe != null; i++) {
            Path env = probe.resolve("clients").resolve(".env");
            if (Files.exists(env)) return probe.resolve("clients");
            env = probe.resolve(".env");
            if (Files.exists(env) && probe.getFileName() != null
                    && probe.getFileName().toString().equals("clients")) {
                return probe;
            }
            probe = probe.getParent();
        }
        // fallbacks
        if (Files.exists(cwd.resolve("../.env"))) return cwd.getParent();
        return cwd;
    }

    static Map<String, String> loadDotenv(Path path) throws IOException {
        Map<String, String> out = new HashMap<>();
        if (!Files.exists(path)) return out;
        List<String> lines = Files.readAllLines(path);
        for (String raw : lines) {
            String line = raw.trim();
            if (line.isEmpty() || line.startsWith("#")) continue;
            int eq = line.indexOf('=');
            if (eq <= 0) continue;
            String k = line.substring(0, eq).trim();
            String v = line.substring(eq + 1).trim();
            if (v.length() >= 2 && (v.startsWith("\"") || v.startsWith("'"))) {
                v = v.substring(1, v.length() - 1);
            }
            // Real env var wins
            String real = System.getenv(k);
            out.put(k, real != null && !real.isEmpty() ? real : v);
        }
        return out;
    }

    static String mustGet(Map<String, String> env, String k) {
        String v = env.get(k);
        if (v == null || v.isEmpty()) {
            throw new RuntimeException("env var " + k + " is required");
        }
        return v;
    }
}
