"""Consume the most recent records from the topic and confirm we can decode
records produced by all three client languages. Demonstrates that the
caching proxy correctly serves schema-id lookups for cross-language consumers.
"""
import os
import sys
import time
from collections import Counter
from pathlib import Path

from confluent_kafka import Consumer, KafkaException, TopicPartition
from confluent_kafka.serialization import (
    SerializationContext,
    MessageField,
    StringDeserializer,
)
from confluent_kafka.schema_registry import SchemaRegistryClient
from confluent_kafka.schema_registry.avro import AvroDeserializer


def load_env(path):
    for line in Path(path).read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        k, _, v = line.partition("=")
        os.environ.setdefault(k.strip(), v.strip())


CLIENTS_DIR = Path(__file__).resolve().parent.parent
load_env(CLIENTS_DIR / ".env")

SCHEMA_STR = (CLIENTS_DIR / "schema.avsc").read_text()
TOPIC = os.environ["TOPIC"]


def kafka_conf():
    return {
        "bootstrap.servers": os.environ["BOOTSTRAP_SERVERS"],
        "security.protocol": "SASL_SSL",
        "sasl.mechanisms": "PLAIN",
        "sasl.username": os.environ["KAFKA_API_KEY"],
        "sasl.password": os.environ["KAFKA_API_SECRET"],
    }


def main():
    sr = SchemaRegistryClient({"url": os.environ["SR_URL"]})
    avro_de = AvroDeserializer(sr, SCHEMA_STR)
    key_de = StringDeserializer("utf_8")

    conf = kafka_conf()
    conf.update({"group.id": f"sr-cache-cross-consume-{time.time_ns()}",
                 "auto.offset.reset": "earliest",
                 "enable.auto.commit": False})
    c = Consumer(conf)
    c.subscribe([TOPIC])

    seen = Counter()
    records = []
    deadline = time.time() + 20
    while time.time() < deadline:
        msg = c.poll(2.0)
        if msg is None:
            if seen and time.time() > deadline - 12:
                break
            continue
        if msg.error():
            raise KafkaException(msg.error())
        key = key_de(msg.key(), SerializationContext(TOPIC, MessageField.KEY))
        val = avro_de(msg.value(), SerializationContext(TOPIC, MessageField.VALUE))
        seen[val.get("client", "?")] += 1
        records.append((val.get("client"), key, val))
    c.close()

    print(f"decoded {sum(seen.values())} total records, breakdown by client:")
    for client, n in sorted(seen.items()):
        print(f"  {client}: {n}")

    if not all(seen.get(lang, 0) > 0 for lang in ("python", "go", "java")):
        print(f"missing one or more languages: {dict(seen)}", file=sys.stderr)
        sys.exit(2)

    print()
    print("sample records (one per client):")
    shown = set()
    for client, key, val in records:
        if client in shown:
            continue
        shown.add(client)
        print(f"  [{client}] key={key!r} value={val}")
        if len(shown) == 3:
            break

    print()
    print("OK — cross-language Avro SerDes via the caching proxy verified")


if __name__ == "__main__":
    main()
